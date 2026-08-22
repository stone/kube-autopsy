package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kube-autopsy/kube-autopsy/api/v1alpha1"
)

// The window used to run from the report's creation alone. A controller that had
// been down longer than the window — a node drain, a rollout, a slow image pull
// — came back to a backlog that was already expired, so each report got exactly
// one delivery attempt and any transient failure (a Slack 429 against a backlog
// is the obvious one) discarded all of them at once.
func TestWebhookDeadlineIsAnchoredToProcessStart(t *testing.T) {
	startedAt := time.Now()
	r := &PodCrashReportReconciler{startedAt: startedAt}

	tests := []struct {
		name     string
		created  time.Time
		wantFrom time.Time
	}{
		{
			name:     "a report created after startup uses its own creation time",
			created:  startedAt.Add(2 * time.Minute),
			wantFrom: startedAt.Add(2 * time.Minute),
		},
		{
			name:     "a backlogged report gets a full window from startup",
			created:  startedAt.Add(-3 * time.Hour),
			wantFrom: startedAt,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := &v1alpha1.PodCrashReport{}
			report.CreationTimestamp = metav1.NewTime(tt.created)

			got := r.webhookDeadline(report)
			want := tt.wantFrom.Add(webhookRetryWindow)
			if !got.Equal(want) {
				t.Errorf("webhookDeadline() = %v, want %v", got, want)
			}
			if !got.After(time.Now()) {
				t.Error("a report reconciled now should still have retries left")
			}
		})
	}
}

// A backlogged report must be retried rather than dropped on its first attempt.
func TestReconcileRetriesABackloggedReport(t *testing.T) {
	report := reportWithDiagnostics("backlogged", time.Now().Add(-3*time.Hour))
	reconciler, recorder := newReportForReconcile(t, report)
	reconciler.WebhookSender = newFailingWebhookServer(t)
	reconciler.startedAt = time.Now()

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "backlogged", Namespace: "default"},
	})
	if err == nil {
		t.Fatal("expected an error so the report is requeued and delivery retried")
	}

	var stored v1alpha1.PodCrashReport
	if err := reconciler.Get(context.Background(), types.NamespacedName{
		Name: "backlogged", Namespace: "default",
	}, &stored); err != nil {
		t.Fatalf("failed to read back report: %v", err)
	}
	if stored.Status.Phase == v1alpha1.PhaseProcessed {
		t.Error("a backlogged report was given up on before it was ever retried")
	}

	select {
	case event := <-recorder.Events:
		t.Errorf("did not expect a dropped-notification event yet, got %q", event)
	default:
	}
}

// Giving up used to leave only a log line, so an endpoint that had been quietly
// failing for a week was invisible on a dashboard.
func TestReconcileRecordsAnEventWhenAnAlertIsDropped(t *testing.T) {
	// Created long enough ago that the window has expired even measured from
	// this process's start.
	report := reportWithDiagnostics("expired", time.Now().Add(-3*time.Hour))
	reconciler, recorder := newReportForReconcile(t, report)
	reconciler.WebhookSender = newFailingWebhookServer(t)
	reconciler.startedAt = time.Now().Add(-3 * time.Hour)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "expired", Namespace: "default"},
	}); err != nil {
		t.Fatalf("expected the report to be processed after the window expired: %v", err)
	}

	var sawDrop bool
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "NotificationDropped") {
				sawDrop = true
			}
			continue
		default:
		}
		break
	}
	if !sawDrop {
		t.Error("expected a NotificationDropped event when the alert was abandoned")
	}

	var stored v1alpha1.PodCrashReport
	if err := reconciler.Get(context.Background(), types.NamespacedName{
		Name: "expired", Namespace: "default",
	}, &stored); err != nil {
		t.Fatalf("failed to read back report: %v", err)
	}
	if stored.Status.Phase != v1alpha1.PhaseProcessed {
		t.Errorf("expected the report to be marked Processed, got %q", stored.Status.Phase)
	}
	if stored.Status.NotifiedAt != nil {
		t.Error("NotifiedAt must stay nil when nothing was delivered")
	}
}

// The controller watches every report in the cluster, and with capture on each
// carries up to 64KiB of logs — which would make its memory a function of how
// badly the cluster is crashing, precisely when it must stay up.
func TestStripCachedLogLines(t *testing.T) {
	report := &v1alpha1.PodCrashReport{}
	report.Name = "with-logs"
	report.Status.Diagnostics.LastLogLines = []string{"secret log line"}
	report.Status.Diagnostics.VictimRSSBytes = 1234

	out, err := StripCachedLogLines(report)
	if err != nil {
		t.Fatalf("StripCachedLogLines: %v", err)
	}

	stripped, ok := out.(*v1alpha1.PodCrashReport)
	if !ok {
		t.Fatalf("expected a *PodCrashReport, got %T", out)
	}
	if stripped.Status.Diagnostics.LastLogLines != nil {
		t.Error("log lines survived into the cache")
	}
	// Everything else must be intact — the reconciler reads these.
	if stripped.Status.Diagnostics.VictimRSSBytes != 1234 {
		t.Error("stripping removed more than the log lines")
	}
	if stripped.Name != "with-logs" {
		t.Error("stripping lost the object's identity")
	}

	// The informer hands the same pointer to every reader, so the original must
	// not be mutated in place.
	if report.Status.Diagnostics.LastLogLines == nil {
		t.Error("the source object was mutated instead of copied")
	}
}

func TestStripCachedLogLinesPassesOtherTypesThrough(t *testing.T) {
	type other struct{ name string }
	in := &other{name: "x"}

	out, err := StripCachedLogLines(in)
	if err != nil {
		t.Fatalf("StripCachedLogLines: %v", err)
	}
	if out != in {
		t.Error("a non-report object should be returned unchanged")
	}
}
