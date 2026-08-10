package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kube-autopsy/kube-autopsy/api/v1alpha1"
	"github.com/kube-autopsy/kube-autopsy/internal/config"
)

const (
	// diagnosticsGracePeriod is how long the controller waits for the agent to
	// attach diagnostics to a freshly created report before processing it
	// anyway. It only matters when the agent dies between creating the report
	// and writing its status.
	diagnosticsGracePeriod = 2 * time.Minute

	// diagnosticsRequeueAfter is the poll interval used while waiting out
	// diagnosticsGracePeriod. It is a safety net only: the agent's status write
	// triggers a watch event that reconciles the report immediately.
	diagnosticsRequeueAfter = 15 * time.Second

	// webhookRetryWindow bounds how long delivery is retried before the report
	// is marked Processed regardless. Without it, an endpoint that is down for
	// good would keep every report in Pending and requeue indefinitely.
	webhookRetryWindow = 10 * time.Minute
)

// PodCrashReportReconciler reconciles PodCrashReport resources. It transitions
// reports from Pending to Processed, optionally sends webhook notifications,
// and records Kubernetes Events for observability.
type PodCrashReportReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Config        *config.Config
	WebhookSender *WebhookSender
	Recorder      record.EventRecorder
}

// Reconcile handles a single PodCrashReport reconciliation cycle. Reports that
// are already in "Processed" phase are skipped (idempotent). New or "Pending"
// reports are transitioned to "Processed", a Kubernetes Event is recorded, and
// an optional webhook notification is sent.
//
// +kubebuilder:rbac:groups=autopsy.tty.se,resources=podcrashreports,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=autopsy.tty.se,resources=podcrashreports/status,verbs=update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
func (r *PodCrashReportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the PodCrashReport instance.
	var report v1alpha1.PodCrashReport
	if err := r.Get(ctx, req.NamespacedName, &report); err != nil {
		if errors.IsNotFound(err) {
			// Report was deleted before we could reconcile — nothing to do.
			logger.V(1).Info("PodCrashReport not found, likely deleted", "name", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get PodCrashReport: %w", err)
	}

	// Idempotency: skip reports that are already processed.
	if report.Status.Phase == v1alpha1.PhaseProcessed {
		logger.V(1).Info("PodCrashReport already processed, skipping", "name", report.Name)
		return ctrl.Result{}, nil
	}

	// The agent creates the report and attaches diagnostics in a second write,
	// because status is a subresource. Processing the report in that window
	// would emit an Event and a webhook with no memory figures and no logs, and
	// would then be overwritten by the agent's write and processed all over
	// again. An empty phase means the agent has not written its status yet.
	if report.Status.Phase == "" {
		if age := time.Since(report.CreationTimestamp.Time); age < diagnosticsGracePeriod {
			logger.V(1).Info("waiting for agent to attach diagnostics",
				"name", report.Name, "age", age.String())
			return ctrl.Result{RequeueAfter: diagnosticsRequeueAfter}, nil
		}
		logger.Info("processing report without diagnostics; the agent likely failed before writing them",
			"name", report.Name)
	}

	// The webhook is attempted before the phase transition. Marking the report
	// Processed first made delivery at-most-once: the report was never
	// reconciled again, so a failed notification was lost permanently.
	//
	// Sending first makes it at-least-once instead — a delivery that succeeds
	// but whose status write then fails will be retried, and the receiver may
	// see a duplicate. For crash alerting a duplicate is far preferable to a
	// silently dropped alert.
	notifiedAt := report.Status.NotifiedAt
	if r.WebhookSender != nil && notifiedAt == nil {
		if err := r.WebhookSender.Send(ctx, &report); err != nil {
			// Give up once the report is old enough that retrying is pointless,
			// so a permanently broken endpoint cannot pin every report in
			// Pending forever.
			if age := time.Since(report.CreationTimestamp.Time); age < webhookRetryWindow {
				logger.Error(err, "webhook delivery failed, will retry",
					"name", report.Name, "age", age.String())
				return ctrl.Result{}, err
			}
			logger.Error(err, "webhook delivery failed and the retry window has expired, giving up",
				"name", report.Name, "retryWindow", webhookRetryWindow.String())
		} else {
			now := metav1.Now()
			notifiedAt = &now
		}
	}

	// Transition phase from empty/"Pending" to "Processed".
	patch := client.MergeFrom(report.DeepCopy())
	report.Status.Phase = v1alpha1.PhaseProcessed
	report.Status.NotifiedAt = notifiedAt
	if err := r.Status().Patch(ctx, &report, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update PodCrashReport status: %w", err)
	}

	logger.Info("PodCrashReport processed",
		"name", report.Name,
		"pod", report.Spec.PodName,
		"container", report.Spec.ContainerName,
		"reason", report.Spec.TerminationReason,
		"exitCode", report.Spec.ExitCode,
	)

	// Record a Kubernetes Event on the PodCrashReport resource.
	r.Recorder.Eventf(&report, "Warning", "CrashDetected",
		"Processed crash report for pod %s/%s (container: %s, reason: %s, exit code: %d)",
		report.Spec.Namespace, report.Spec.PodName,
		report.Spec.ContainerName, report.Spec.TerminationReason, report.Spec.ExitCode,
	)

	// Update Prometheus metrics.
	ReportsCreatedTotal.WithLabelValues(
		report.Spec.Namespace,
		report.Spec.NodeName,
		report.Spec.TerminationReason,
	).Inc()

	if report.Spec.TerminationReason == v1alpha1.TerminationOOMKilled {
		OOMEventsTotal.WithLabelValues(
			report.Spec.Namespace,
			report.Spec.NodeName,
		).Inc()

		if report.Status.Diagnostics.RSSDissection != nil {
			VictimAnonRSSBytes.WithLabelValues(
				report.Spec.Namespace,
				containerNameLimiter.bound(report.Spec.ContainerName),
			).Observe(float64(report.Status.Diagnostics.RSSDissection.AnonRSSBytes))
		}

		if report.Status.Diagnostics.TriggerComm != "" {
			TriggerProcessesTotal.WithLabelValues(
				triggerCommLimiter.bound(report.Status.Diagnostics.TriggerComm),
			).Inc()
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager registers the PodCrashReportReconciler with the given
// controller manager, watching for PodCrashReport resources.
func (r *PodCrashReportReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.PodCrashReport{}).
		Complete(r)
}
