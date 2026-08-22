package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	autopsy "github.com/kube-autopsy/kube-autopsy/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PodListError reports that the pods on this node could not be read at all,
// as opposed to being read successfully and containing no match. The two used
// to be indistinguishable to the caller, which meant a broken informer looked
// exactly like a quiet node.
type PodListError struct{ Err error }

func (e *PodListError) Error() string { return "failed to list pods: " + e.Err.Error() }
func (e *PodListError) Unwrap() error { return e.Err }

// StatusWriteError reports that the report was created but its diagnostics
// could not be attached, leaving an object with an empty status that nothing
// will ever fill in.
type StatusWriteError struct{ Err error }

func (e *StatusWriteError) Error() string { return "failed to write report status: " + e.Err.Error() }
func (e *StatusWriteError) Unwrap() error { return e.Err }

// resolveRetryBackoff bounds how long ResolvePodMeta waits for the kubelet to
// publish a container ID. A container with a tight memory limit can be killed a
// few hundred milliseconds after it starts, before its containerID reaches
// status — and a single lookup then discards the crash permanently, which is
// exactly the crash the operator most wants to see.
// The budget is deliberately small. Each attempt holds one of the
// MaxConcurrentReports slots, and the reader goroutine blocks on that semaphore,
// so time spent here is time the ring buffer is not being drained. The pending
// -container-ID guard below narrows *when* this runs but is node-wide, so it
// cannot be relied on to skip every victim that belongs to no pod — the budget
// has to be affordable even when it does not.
var resolveRetryBackoff = wait.Backoff{
	Duration: 100 * time.Millisecond,
	Factor:   2,
	Steps:    3, // one immediate attempt, then ~100ms and ~200ms
}

// statusWriteBackoff bounds retries of the diagnostics write. A dropped
// connection during an OOM storm would otherwise strand the report with no
// diagnostics at all — not just no logs, but no memory figures either.
var statusWriteBackoff = wait.Backoff{
	Duration: 100 * time.Millisecond,
	Factor:   2,
	Jitter:   0.1,
	Steps:    4,
}

// PodMeta contains the Kubernetes metadata for a pod, resolved from its UID.
type PodMeta struct {
	// PodName is the name of the pod.
	PodName string
	// Namespace is the namespace the pod belongs to.
	Namespace string
	// ContainerName is the name of the container that crashed.
	ContainerName string
	// PodUID is the UID of the pod.
	PodUID string
	// LogFileIndex identifies which of the container's log files belongs to the
	// incarnation that was killed. The kubelet names them by restart count —
	// 0.log, 1.log — and all incarnations share one directory, so after a
	// restart the newest file is the replacement's, not the victim's. Reading
	// that one would attach a live process's startup output to a post-mortem.
	// Negative means unknown, in which case the newest file is used.
	LogFileIndex int
}

// Reporter creates PodCrashReport CRD instances in the Kubernetes API.
type Reporter struct {
	client client.Client
	// nodeName is the node this agent runs on.
	nodeName string
	// podOwnerReference makes each report owned by the pod it describes, so
	// Kubernetes deletes it along with the pod. Off by default: a post-mortem
	// that disappears when the pod is recycled is usually gone before anyone
	// reads it. Retention is otherwise handled by the controller's TTL.
	podOwnerReference bool
}

// NewReporter creates a new Reporter that uses the given Kubernetes client
// and node name for creating crash reports.
func NewReporter(client client.Client, nodeName string, podOwnerReference bool) *Reporter {
	return &Reporter{
		client:            client,
		nodeName:          nodeName,
		podOwnerReference: podOwnerReference,
	}
}

// CreateCrashReport creates a PodCrashReport custom resource in Kubernetes.
// The name is server-generated from the prefix <podName>-<containerName>-, so
// a container that is OOM-killed repeatedly produces one report per kill rather
// than colliding on a deterministic name. Owner references are set to the
// originating pod when possible.
func (r *Reporter) CreateCrashReport(ctx context.Context, event CrashEvent, podMeta PodMeta, logLines []string) error {
	// Calculate OOMContext
	oomContext := autopsy.OOMContextContainerLimit
	if event.IsGlobalOOM {
		oomContext = autopsy.OOMContextNodeExhaustion
	}

	report := &autopsy.PodCrashReport{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: reportNamePrefix(podMeta.PodName, podMeta.ContainerName),
			Namespace:    podMeta.Namespace,
			// Names are server-generated and therefore opaque, so these labels
			// are how a report is found: they allow server-side selection with
			// `kubectl get pcr -l ...` rather than filtering every object
			// client-side.
			Labels: map[string]string{
				autopsy.LabelPod:       labelValue(podMeta.PodName),
				autopsy.LabelContainer: labelValue(podMeta.ContainerName),
				autopsy.LabelNode:      labelValue(r.nodeName),
				autopsy.LabelReason:    autopsy.TerminationOOMKilled,
			},
		},
		Spec: autopsy.PodCrashReportSpec{
			PodName:           podMeta.PodName,
			PodUID:            podMeta.PodUID,
			Namespace:         podMeta.Namespace,
			ContainerName:     podMeta.ContainerName,
			NodeName:          r.nodeName,
			TerminationReason: autopsy.TerminationOOMKilled,
			ExitCode:          137, // Standard OOM kill exit code (128 + SIGKILL=9).
			Timestamp:         metav1.NewTime(event.DetectedAt.UTC()),
		},
	}

	if r.podOwnerReference {
		setOwnerReference(report, podMeta)
	}

	if err := r.client.Create(ctx, report); err != nil {
		return fmt.Errorf("failed to create PodCrashReport in %s: %w", podMeta.Namespace, err)
	}

	// Because 'status' is a subresource, Create() ignores the Status field, so
	// the diagnostics need a second write. The patch carries an optimistic-lock
	// precondition on the resourceVersion returned by Create: if anything else
	// has written to the report in the meantime, fail loudly rather than
	// silently clobbering it.
	patch := client.MergeFromWithOptions(report.DeepCopy(), client.MergeFromWithOptimisticLock{})

	diagnostics := autopsy.DiagnosticData{
		OOMScopeLimitBytes: event.OOMScopeLimitBytes,
		OOMVictimPID:       event.OOMVictimPID,
		OOMVictimComm:      event.OOMVictimComm,
		TriggerPID:         event.TriggerPID,
		TriggerComm:        event.TriggerComm,
		OOMScore:           event.OOMScore,
		OOMScoreAdj:        event.OOMScoreAdj,
		OOMContext:         oomContext,
		LastLogLines:       logLines,
	}

	// Only publish memory figures the kernel actually gave us. Reporting zeroes
	// would be indistinguishable from a container that used no memory.
	if event.RSSValid {
		diagnostics.VictimRSSBytes = event.VictimRSSBytes
		diagnostics.RSSDissection = &autopsy.RSSDissection{
			AnonRSSBytes:    event.AnonRSSBytes,
			FileRSSBytes:    event.FileRSSBytes,
			ShmemRSSBytes:   event.ShmemRSSBytes,
			SwapBytes:       event.SwapBytes,
			PageTablesBytes: event.PageTablesBytes,
		}
	}

	report.Status = autopsy.PodCrashReportStatus{
		Diagnostics: diagnostics,
		Phase:       autopsy.PhasePending,
	}

	// Retried, because this write is the one that carries every diagnostic. If it
	// is lost the report still exists, so the controller will happily process it
	// and notify with zeroes, and there is no path that ever fills it in.
	var lastErr error
	err := retryOnBackoff(ctx, statusWriteBackoff, func() (bool, error) {
		if err := r.client.Status().Patch(ctx, report, patch); err != nil {
			if !isRetryableStatusError(err) {
				return false, &StatusWriteError{Err: fmt.Errorf(
					"%s/%s: %w", podMeta.Namespace, report.Name, err)}
			}
			lastErr = err
			return false, nil
		}
		// Cleared, so a success after one or more retryable failures is not
		// reported as a failure.
		lastErr = nil
		return true, nil
	})
	if err != nil {
		return err
	}
	if lastErr != nil {
		return &StatusWriteError{Err: fmt.Errorf(
			"%s/%s: %w", podMeta.Namespace, report.Name, lastErr)}
	}

	return nil
}

// isRetryableStatusError reports whether re-issuing the status write could
// plausibly succeed. A schema violation or a revoked permission fails
// identically every time, so retrying it only delays the error.
//
// Conflict is deliberately absent. The patch carries an optimistic-lock
// precondition on the resourceVersion returned by Create, and the agent is not
// granted "get" on podcrashreports, so it cannot re-read the object to refresh
// that base — replaying the same patch could only conflict again. A conflict
// here also means something else has written to a report the agent created
// moments ago, which is worth surfacing rather than papering over.
func isRetryableStatusError(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		// No API status at all means the request never reached a verdict — a
		// dropped connection, a DNS blip — which is worth another go.
		return true
	}

	return apierrors.IsServerTimeout(err) ||
		apierrors.IsTimeout(err) ||
		apierrors.IsTooManyRequests(err) ||
		apierrors.IsInternalError(err) ||
		apierrors.IsServiceUnavailable(err)
}

// ResolvePodMeta uses the Kubernetes API to find the pod matching the given event
// and returns its metadata including the name of the container that matches the
// crash event's container ID.
// It retries briefly: the kubelet publishes a container's ID asynchronously, so
// a container killed moments after starting may not carry one yet, and the
// agent's informer can lag a little behind the API server besides. A single
// lookup discarded those crashes permanently.
func (r *Reporter) ResolvePodMeta(ctx context.Context, event CrashEvent) (PodMeta, error) {
	var meta PodMeta

	err := retryOnBackoff(ctx, resolveRetryBackoff, func() (bool, error) {
		// List all pods on this node to find the matching UID.
		var podList corev1.PodList
		if err := r.client.List(ctx, &podList, client.MatchingFields{
			"spec.nodeName": r.nodeName,
		}); err != nil {
			// Terminal: a listing failure is not something waiting fixes, and it
			// must reach the caller as itself rather than as "no pod found".
			return false, &PodListError{Err: fmt.Errorf("on node %s: %w", r.nodeName, err)}
		}

		for _, pod := range podList.Items {
			containerName, logIndex := findContainerByID(pod, event.ContainerID)
			if containerName != "" {
				// Found the pod containing this container!
				// We populate PodUID so the rest of the flow can use it
				meta = PodMeta{
					PodName:       pod.Name,
					Namespace:     pod.Namespace,
					ContainerName: containerName,
					PodUID:        string(pod.UID),
					LogFileIndex:  logIndex,
				}
				return true, nil
			}
		}

		// No match. Retrying is only worth it while some container on this node
		// has yet to be given an ID — that is the race being waited out. When
		// every ID is published the answer is already final, and this event is a
		// victim that belongs to no pod: a systemd unit, sshd, the kubelet. Those
		// are the common case during a node-level OOM, and waiting out the full
		// schedule for each one would hold a concurrency slot for seconds and
		// throttle the reports that do matter.
		if !anyContainerIDPending(podList.Items) {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return PodMeta{}, err
	}
	if meta.PodName == "" {
		return PodMeta{}, fmt.Errorf("pod with container ID %s not found on node %s", event.ContainerID, r.nodeName)
	}
	return meta, nil
}

// anyContainerIDPending reports whether some container on this node is running
// or starting without a published container ID — the window in which a crash can
// arrive before the kubelet has said which container it belongs to.
func anyContainerIDPending(pods []corev1.Pod) bool {
	for i := range pods {
		for _, group := range [][]corev1.ContainerStatus{
			pods[i].Status.ContainerStatuses,
			pods[i].Status.InitContainerStatuses,
			pods[i].Status.EphemeralContainerStatuses,
		} {
			for _, cs := range group {
				// Terminated with no ID is a container that never started, not one
				// still being registered, so only waiting and running count.
				if cs.ContainerID == "" && cs.State.Terminated == nil {
					return true
				}
			}
		}
	}
	return false
}

// retryOnBackoff runs fn until it reports success, its retries are exhausted, or
// ctx is cancelled. Unlike wait.ExponentialBackoff it does not sleep after the
// final attempt, and it respects cancellation so a shutdown does not have to
// wait out the whole schedule.
func retryOnBackoff(ctx context.Context, backoff wait.Backoff, fn func() (bool, error)) error {
	delay := backoff.Duration
	for attempt := 0; attempt < backoff.Steps; attempt++ {
		if attempt > 0 {
			// wait.Jitter is applied to the sleep but not carried into the next
			// delay, so the schedule stays exponential rather than compounding.
			sleep := delay
			if backoff.Jitter > 0 {
				sleep = wait.Jitter(delay, backoff.Jitter)
			}
			timer := time.NewTimer(sleep)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
			delay = time.Duration(float64(delay) * backoff.Factor)
		}

		done, err := fn()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
	return nil
}

// setOwnerReference sets the pod as the owner of the PodCrashReport, so the
// report is garbage-collected when the pod is deleted. The pod identity comes
// from the metadata already resolved by ResolvePodMeta — re-reading it here
// would cost an extra full-namespace List per crash event.
func setOwnerReference(report *autopsy.PodCrashReport, podMeta PodMeta) {
	if podMeta.PodName == "" || podMeta.PodUID == "" {
		return
	}

	isController := false
	// Deliberately false: blockOwnerDeletion requires "update" on
	// pods/finalizers wherever the OwnerReferencesPermissionEnforcement
	// admission plugin is enabled (OpenShift), which the agent is not granted.
	blockOwnerDeletion := false
	report.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion:         "v1",
			Kind:               "Pod",
			Name:               podMeta.PodName,
			UID:                types.UID(podMeta.PodUID),
			Controller:         &isController,
			BlockOwnerDeletion: &blockOwnerDeletion,
		},
	}
}

// findContainerByID inspects a pod's container statuses to find one that matches
// the given container ID. It returns the container's name and the index of the
// log file belonging to that incarnation, or ("", -1) when nothing matches.
//
// Both the current and the previous incarnation are considered. After a restart
// the ID of the container that was actually killed has moved to
// lastState.terminated.containerID, so matching only the live one loses exactly
// the reports a crash-looping container should produce. The log index tracks
// which one matched, because the incarnations share a log directory and the
// newest file belongs to whichever is running now.
func findContainerByID(pod corev1.Pod, containerID string) (string, int) {
	statuses := make([]corev1.ContainerStatus, 0, len(pod.Status.ContainerStatuses)+len(pod.Status.InitContainerStatuses)+len(pod.Status.EphemeralContainerStatuses))
	statuses = append(statuses, pod.Status.ContainerStatuses...)
	statuses = append(statuses, pod.Status.InitContainerStatuses...)
	statuses = append(statuses, pod.Status.EphemeralContainerStatuses...)

	for _, cs := range statuses {
		// The kubelet writes <restartCount>.log, so the live container's file is
		// numbered by its current restart count.
		if containerIDsMatch(cs.ContainerID, containerID) {
			return cs.Name, int(cs.RestartCount)
		}
		// The previous incarnation ran one restart earlier, so its file is one
		// lower. RestartCount is only 0 here if the status has not been updated
		// yet, in which case the index is unknown rather than negative.
		if t := cs.LastTerminationState.Terminated; t != nil && containerIDsMatch(t.ContainerID, containerID) {
			return cs.Name, int(cs.RestartCount) - 1
		}
	}
	return "", -1
}

// containerIDsMatch compares a ContainerStatus ID such as
// "containerd://<64-hex>" with an ID derived from a cgroup name. The runtime
// scheme is stripped and the remainder compared exactly: a substring match
// could attribute a crash to the wrong container whenever one ID happens to
// contain another, and would also match against the scheme itself.
func containerIDsMatch(statusID, cgroupID string) bool {
	if statusID == "" || cgroupID == "" {
		return false
	}
	if _, id, found := strings.Cut(statusID, "://"); found {
		statusID = id
	}
	return statusID == cgroupID
}

// generatedNameBaseLimit is the longest GenerateName prefix the API server
// preserves: it truncates the base to 63 characters less the 5-character
// random suffix it appends.
const generatedNameBaseLimit = 63 - 5

// reportNamePrefix builds the GenerateName prefix for a crash report, trimmed
// so the API server does not truncate it into something unrecognisable.
func reportNamePrefix(podName, containerName string) string {
	base := sanitizeName(fmt.Sprintf("%s-%s", podName, containerName))
	if base == "" {
		base = "podcrashreport"
	}
	if len(base) > generatedNameBaseLimit-1 {
		base = base[:generatedNameBaseLimit-1]
	}
	return strings.TrimRight(base, "-.") + "-"
}

// maxLabelValueLength is the Kubernetes limit for a label value.
const maxLabelValueLength = 63

// labelValue coerces s into a valid Kubernetes label value: at most 63
// characters of alphanumerics, '-', '_' or '.', beginning and ending with an
// alphanumeric. An invalid label would make the whole report rejected at
// creation, so this never returns something the API server will refuse.
func labelValue(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'),
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}

	out := b.String()
	if len(out) > maxLabelValueLength {
		out = out[:maxLabelValueLength]
	}
	// Leading and trailing non-alphanumerics are not permitted.
	return strings.Trim(out, "-_.")
}

// sanitizeName converts a string into a valid Kubernetes resource name by
// lowercasing and replacing invalid characters with dashes.
func sanitizeName(name string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	result := b.String()
	// Trim leading/trailing dashes.
	result = strings.Trim(result, "-.")
	return result
}
