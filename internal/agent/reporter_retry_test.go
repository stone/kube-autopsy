package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	autopsy "github.com/kube-autopsy/kube-autopsy/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func agentScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := autopsy.AddToScheme(scheme); err != nil {
		t.Fatalf("adding autopsy scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding core scheme: %v", err)
	}
	return scheme
}

func podOnNode(name, node, containerName, containerID string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID("uid-" + name)},
		Spec:       corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: containerName, ContainerID: containerID},
			},
		},
	}
}

// nodeNameIndex mirrors the field index main.go registers, so the fake client
// can serve the same MatchingFields query the reporter issues.
func withNodeIndex(b *fake.ClientBuilder) *fake.ClientBuilder {
	return b.WithIndex(&corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
		return []string{o.(*corev1.Pod).Spec.NodeName}
	})
}

// A container with a tight limit can be killed a few hundred milliseconds after
// it starts, before the kubelet has published its containerID. A single lookup
// discarded that crash forever — which is exactly the crash the operator most
// wants to see.
func TestResolvePodMetaRetriesWhileTheContainerIDIsUnpublished(t *testing.T) {
	pod := podOnNode("late-pod", "node-1", "app", "")

	calls := 0
	fakeClient := withNodeIndex(fake.NewClientBuilder().WithScheme(agentScheme(t))).
		WithObjects(pod).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				calls++
				if err := c.List(ctx, list, opts...); err != nil {
					return err
				}
				// The kubelet publishes the ID between the second and third call.
				if calls >= 3 {
					pods := list.(*corev1.PodList)
					for i := range pods.Items {
						pods.Items[i].Status.ContainerStatuses[0].ContainerID = "containerd://abc123"
					}
				}
				return nil
			},
		}).
		Build()

	r := NewReporter(fakeClient, "node-1", false)

	meta, err := r.ResolvePodMeta(context.Background(), CrashEvent{ContainerID: "abc123"})
	if err != nil {
		t.Fatalf("ResolvePodMeta: %v", err)
	}
	if meta.PodName != "late-pod" || meta.ContainerName != "app" {
		t.Errorf("resolved to %+v, want late-pod/app", meta)
	}
	if calls < 3 {
		t.Errorf("resolution succeeded after %d attempts; the retry did not happen", calls)
	}
}

// After a restart the killed container's ID has moved to lastState, so matching
// only the live container loses exactly the reports a crash-looping container
// should produce.
func TestResolvePodMetaMatchesThePreviousIncarnation(t *testing.T) {
	pod := podOnNode("restarted", "node-1", "app", "containerd://new999")
	pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ContainerID: "containerd://old111"},
	}

	fakeClient := withNodeIndex(fake.NewClientBuilder().WithScheme(agentScheme(t))).
		WithObjects(pod).
		Build()

	r := NewReporter(fakeClient, "node-1", false)

	meta, err := r.ResolvePodMeta(context.Background(), CrashEvent{ContainerID: "old111"})
	if err != nil {
		t.Fatalf("ResolvePodMeta: %v", err)
	}
	if meta.ContainerName != "app" {
		t.Errorf("resolved to %q, want the container whose previous incarnation was killed", meta.ContainerName)
	}
}

// A listing failure is an agent problem and must reach the caller as itself.
// Sharing a return with "no pod owns this cgroup" meant a broken informer looked
// exactly like a quiet node.
func TestResolvePodMetaDistinguishesAListFailure(t *testing.T) {
	fakeClient := withNodeIndex(fake.NewClientBuilder().WithScheme(agentScheme(t))).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				return errors.New("informer is not synced")
			},
		}).
		Build()

	r := NewReporter(fakeClient, "node-1", false)

	_, err := r.ResolvePodMeta(context.Background(), CrashEvent{ContainerID: "abc123"})
	if err == nil {
		t.Fatal("expected an error")
	}

	var listErr *PodListError
	if !errors.As(err, &listErr) {
		t.Errorf("error is %T, want a *PodListError so the caller can tell it apart from a non-pod victim", err)
	}
}

// A genuine non-pod victim — a global OOM that took sshd — must NOT look like a
// list failure.
func TestResolvePodMetaReportsNoMatchPlainly(t *testing.T) {
	fakeClient := withNodeIndex(fake.NewClientBuilder().WithScheme(agentScheme(t))).
		WithObjects(podOnNode("unrelated", "node-1", "app", "containerd://zzz")).
		Build()

	r := NewReporter(fakeClient, "node-1", false)

	// Shorten the schedule so the test does not sit through the full backoff.
	original := resolveRetryBackoff
	resolveRetryBackoff.Duration = time.Millisecond
	defer func() { resolveRetryBackoff = original }()

	_, err := r.ResolvePodMeta(context.Background(), CrashEvent{ContainerID: "abc123"})
	if err == nil {
		t.Fatal("expected an error")
	}

	var listErr *PodListError
	if errors.As(err, &listErr) {
		t.Error("an unmatched container was reported as a list failure")
	}
}

// The status write carries every diagnostic. Losing it leaves a report the
// controller will happily process and notify with zeroes, and nothing ever
// fills it in — so a transient failure has to be retried.
func TestCreateCrashReportRetriesTheStatusWrite(t *testing.T) {
	attempts := 0
	fakeClient := fake.NewClientBuilder().
		WithScheme(agentScheme(t)).
		WithStatusSubresource(&autopsy.PodCrashReport{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string,
				obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				attempts++
				if attempts < 3 {
					return apierrors.NewServiceUnavailable("simulated API outage")
				}
				return c.Status().Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	r := NewReporter(fakeClient, "node-1", false)

	original := statusWriteBackoff
	statusWriteBackoff.Duration = time.Millisecond
	defer func() { statusWriteBackoff = original }()

	err := r.CreateCrashReport(context.Background(),
		CrashEvent{RSSValid: true, VictimRSSBytes: 4096, DetectedAt: time.Now()},
		PodMeta{PodName: "p", Namespace: "default", ContainerName: "c", PodUID: "u"},
		nil)
	if err != nil {
		t.Fatalf("CreateCrashReport: %v", err)
	}
	if attempts != 3 {
		t.Errorf("status write took %d attempts, want 3", attempts)
	}
}

// A permanent rejection — a schema violation, a revoked permission — must not be
// retried, and must surface as a StatusWriteError so the caller can count the
// orphan it leaves behind.
func TestCreateCrashReportDoesNotRetryPermanentStatusFailures(t *testing.T) {
	attempts := 0
	fakeClient := fake.NewClientBuilder().
		WithScheme(agentScheme(t)).
		WithStatusSubresource(&autopsy.PodCrashReport{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string,
				obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				attempts++
				return apierrors.NewInvalid(
					schema.GroupKind{Group: "autopsy.tty.se", Kind: "PodCrashReport"},
					obj.GetName(), nil)
			},
		}).
		Build()

	r := NewReporter(fakeClient, "node-1", false)

	err := r.CreateCrashReport(context.Background(),
		CrashEvent{DetectedAt: time.Now()},
		PodMeta{PodName: "p", Namespace: "default", ContainerName: "c", PodUID: "u"},
		nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if attempts != 1 {
		t.Errorf("a permanent rejection was retried %d times", attempts)
	}

	var statusErr *StatusWriteError
	if !errors.As(err, &statusErr) {
		t.Errorf("error is %T, want a *StatusWriteError", err)
	}
}

func TestIsRetryableStatusError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "transport failure", err: errors.New("connection reset by peer"), want: true},
		{name: "service unavailable", err: apierrors.NewServiceUnavailable("x"), want: true},
		{name: "too many requests", err: apierrors.NewTooManyRequestsError("x"), want: true},
		{name: "internal error", err: apierrors.NewInternalError(errors.New("x")), want: true},
		{
			name: "invalid",
			err:  apierrors.NewInvalid(schema.GroupKind{Kind: "PodCrashReport"}, "n", nil),
			want: false,
		},
		{name: "forbidden", err: apierrors.NewForbidden(schema.GroupResource{}, "n", errors.New("x")), want: false},
		{
			// The patch base cannot be refreshed without "get", which the agent is
			// not granted, so replaying it could only conflict again.
			name: "conflict is not retried",
			err:  apierrors.NewConflict(schema.GroupResource{}, "n", errors.New("x")),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableStatusError(tt.err); got != tt.want {
				t.Errorf("isRetryableStatusError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// Retrying must not outlast a shutdown.
func TestRetryOnBackoffHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	err := retryOnBackoff(ctx, resolveRetryBackoff, func() (bool, error) {
		calls++
		return false, nil
	})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	// The first attempt runs before any sleep; the second must not.
	if calls != 1 {
		t.Errorf("fn called %d times after cancellation, want 1", calls)
	}
}

// A node-level OOM kills processes that belong to no pod at all — a systemd
// unit, sshd, the kubelet — and those are the common case exactly when the node
// is busiest. Waiting out the full retry schedule for each would hold one of the
// eight concurrency slots for seconds and throttle the reports that do matter.
func TestResolvePodMetaDoesNotRetryWhenEveryContainerIDIsKnown(t *testing.T) {
	calls := 0
	fakeClient := withNodeIndex(fake.NewClientBuilder().WithScheme(agentScheme(t))).
		WithObjects(podOnNode("settled", "node-1", "app", "containerd://known")).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				calls++
				return c.List(ctx, list, opts...)
			},
		}).
		Build()

	r := NewReporter(fakeClient, "node-1", false)

	start := time.Now()
	_, err := r.ResolvePodMeta(context.Background(), CrashEvent{ContainerID: "sshd-cgroup"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error for a victim that belongs to no pod")
	}
	var listErr *PodListError
	if errors.As(err, &listErr) {
		t.Error("a non-pod victim was reported as a list failure")
	}
	if calls != 1 {
		t.Errorf("listed %d times; a settled node should be answered on the first look", calls)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("took %v; the retry schedule should not have run", elapsed)
	}
}

func TestAnyContainerIDPending(t *testing.T) {
	running := podOnNode("running", "node-1", "app", "containerd://abc")
	starting := podOnNode("starting", "node-1", "app", "")
	// A container that never started is not a container still being registered.
	neverStarted := podOnNode("never", "node-1", "app", "")
	neverStarted.Status.ContainerStatuses[0].State.Terminated =
		&corev1.ContainerStateTerminated{Reason: "CreateContainerError"}

	tests := []struct {
		name string
		pods []corev1.Pod
		want bool
	}{
		{name: "all IDs known", pods: []corev1.Pod{*running}, want: false},
		{name: "one still starting", pods: []corev1.Pod{*running, *starting}, want: true},
		{name: "terminated without an ID does not count", pods: []corev1.Pod{*running, *neverStarted}, want: false},
		{name: "no pods at all", pods: nil, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := anyContainerIDPending(tt.pods); got != tt.want {
				t.Errorf("anyContainerIDPending() = %v, want %v", got, tt.want)
			}
		})
	}
}
