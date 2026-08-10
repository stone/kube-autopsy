package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kube-autopsy/kube-autopsy/api/v1alpha1"
	"github.com/kube-autopsy/kube-autopsy/internal/config"
)

func TestReconcileEmitsEvent(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add to scheme: %v", err)
	}

	report := &v1alpha1.PodCrashReport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-crash-report",
			Namespace: "default",
		},
		Spec: v1alpha1.PodCrashReportSpec{
			PodName:           "test-pod",
			Namespace:         "default",
			ContainerName:     "test-container",
			TerminationReason: "OOMKilled",
			ExitCode:          137,
		},
		Status: v1alpha1.PodCrashReportStatus{
			Phase: "Pending",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(report).
		WithStatusSubresource(report).
		Build()

	fakeRecorder := record.NewFakeRecorder(10)
	cfg := config.NewConfig()

	reconciler := &PodCrashReportReconciler{
		Client:   fakeClient,
		Scheme:   scheme,
		Config:   cfg,
		Recorder: fakeRecorder,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-crash-report",
			Namespace: "default",
		},
	}

	_, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	select {
	case event := <-fakeRecorder.Events:
		expectedSubstr := "Warning CrashDetected Processed crash report for pod default/test-pod"
		if len(event) < len(expectedSubstr) || event[:len(expectedSubstr)] != expectedSubstr {
			t.Errorf("expected event starting with %q, got %q", expectedSubstr, event)
		}
	default:
		t.Errorf("expected an event to be emitted on the fake recorder, but none was received")
	}
}

// newReportForReconcile builds a report and a reconciler wired to a fake client.
func newReportForReconcile(t *testing.T, report *v1alpha1.PodCrashReport) (*PodCrashReportReconciler, *record.FakeRecorder) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add to scheme: %v", err)
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(report).
		WithStatusSubresource(report).
		Build()

	recorder := record.NewFakeRecorder(10)
	return &PodCrashReportReconciler{
		Client:   fakeClient,
		Scheme:   scheme,
		Config:   config.NewConfig(),
		Recorder: recorder,
	}, recorder
}

// The agent creates a report and attaches diagnostics in a second write. If the
// controller processes the report in that window it notifies with no memory
// figures and no logs, and then processes the report a second time once the
// agent's write lands.
func TestReconcileWaitsForAgentDiagnostics(t *testing.T) {
	report := &v1alpha1.PodCrashReport{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "freshly-created-report",
			Namespace:         "default",
			CreationTimestamp: metav1.Now(),
		},
		Spec: v1alpha1.PodCrashReportSpec{
			PodName:           "test-pod",
			Namespace:         "default",
			ContainerName:     "test-container",
			TerminationReason: "OOMKilled",
			ExitCode:          137,
		},
		// No status: the agent has not written diagnostics yet.
	}

	reconciler, recorder := newReportForReconcile(t, report)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "freshly-created-report", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	if result.RequeueAfter <= 0 {
		t.Errorf("expected the report to be requeued, got %+v", result)
	}

	select {
	case event := <-recorder.Events:
		t.Errorf("expected no event before diagnostics are attached, got %q", event)
	default:
	}

	var stored v1alpha1.PodCrashReport
	if err := reconciler.Get(context.Background(), types.NamespacedName{
		Name: "freshly-created-report", Namespace: "default",
	}, &stored); err != nil {
		t.Fatalf("failed to read back report: %v", err)
	}
	if stored.Status.Phase != "" {
		t.Errorf("expected phase to remain unset, got %q", stored.Status.Phase)
	}
}

// If the agent dies between creating the report and writing its status, the
// report must not be stranded unprocessed forever.
func TestReconcileProcessesReportWithoutDiagnosticsAfterGracePeriod(t *testing.T) {
	report := &v1alpha1.PodCrashReport{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "stranded-report",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * diagnosticsGracePeriod)),
		},
		Spec: v1alpha1.PodCrashReportSpec{
			PodName:           "test-pod",
			Namespace:         "default",
			ContainerName:     "test-container",
			TerminationReason: "OOMKilled",
			ExitCode:          137,
		},
	}

	reconciler, recorder := newReportForReconcile(t, report)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "stranded-report", Namespace: "default"},
	}); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	select {
	case <-recorder.Events:
	default:
		t.Error("expected the stranded report to be processed and an event emitted")
	}

	var stored v1alpha1.PodCrashReport
	if err := reconciler.Get(context.Background(), types.NamespacedName{
		Name: "stranded-report", Namespace: "default",
	}, &stored); err != nil {
		t.Fatalf("failed to read back report: %v", err)
	}
	if stored.Status.Phase != "Processed" {
		t.Errorf("expected phase Processed, got %q", stored.Status.Phase)
	}
}

// failingSender always fails, standing in for a webhook endpoint that is down.
func newFailingWebhookServer(t *testing.T) *WebhookSender {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return NewWebhookSender(srv.URL, "", false)
}

func reportWithDiagnostics(name string, created time.Time) *v1alpha1.PodCrashReport {
	return &v1alpha1.PodCrashReport{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: v1alpha1.PodCrashReportSpec{
			PodName:           "test-pod",
			Namespace:         "default",
			ContainerName:     "test-container",
			TerminationReason: v1alpha1.TerminationOOMKilled,
			ExitCode:          137,
		},
		Status: v1alpha1.PodCrashReportStatus{Phase: v1alpha1.PhasePending},
	}
}

// Marking the report Processed before notifying made delivery at-most-once: a
// failed webhook was never retried because the report was never revisited.
func TestReconcileRetriesWebhookBeforeMarkingProcessed(t *testing.T) {
	report := reportWithDiagnostics("retryable", time.Now())
	reconciler, _ := newReportForReconcile(t, report)
	reconciler.WebhookSender = newFailingWebhookServer(t)

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "retryable", Namespace: "default"},
	})
	if err == nil {
		t.Error("expected an error so the report is requeued and delivery retried")
	}

	var stored v1alpha1.PodCrashReport
	if err := reconciler.Get(context.Background(), types.NamespacedName{
		Name: "retryable", Namespace: "default",
	}, &stored); err != nil {
		t.Fatalf("failed to read back report: %v", err)
	}
	if stored.Status.Phase == v1alpha1.PhaseProcessed {
		t.Error("report was marked Processed despite the notification failing, so it will never be retried")
	}
}

// A permanently broken endpoint must not pin every report in Pending forever.
func TestReconcileGivesUpOnWebhookAfterRetryWindow(t *testing.T) {
	report := reportWithDiagnostics("expired", time.Now().Add(-2*webhookRetryWindow))
	reconciler, recorder := newReportForReconcile(t, report)
	reconciler.WebhookSender = newFailingWebhookServer(t)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "expired", Namespace: "default"},
	}); err != nil {
		t.Fatalf("expected the report to be processed anyway, got error: %v", err)
	}

	var stored v1alpha1.PodCrashReport
	if err := reconciler.Get(context.Background(), types.NamespacedName{
		Name: "expired", Namespace: "default",
	}, &stored); err != nil {
		t.Fatalf("failed to read back report: %v", err)
	}
	if stored.Status.Phase != v1alpha1.PhaseProcessed {
		t.Errorf("phase = %q, want Processed once the retry window has expired", stored.Status.Phase)
	}
	select {
	case <-recorder.Events:
	default:
		t.Error("expected the crash Event to be recorded even though notification failed")
	}
}

// Once delivered, a notification must not be repeated on a later reconcile.
func TestReconcileDoesNotResendAfterSuccess(t *testing.T) {
	var deliveries int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deliveries++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	report := reportWithDiagnostics("delivered", time.Now())
	reconciler, _ := newReportForReconcile(t, report)
	reconciler.WebhookSender = NewWebhookSender(srv.URL, "", false)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "delivered", Namespace: "default"}}
	for range 3 {
		if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("Reconcile failed: %v", err)
		}
	}

	if deliveries != 1 {
		t.Errorf("webhook delivered %d times, want exactly 1", deliveries)
	}
}
