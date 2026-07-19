package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kube-autopsy/kube-autopsy/api/v1alpha1"
	"github.com/kube-autopsy/kube-autopsy/internal/config"
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
// +kubebuilder:rbac:groups=autopsy.io,resources=podcrashreports,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=autopsy.io,resources=podcrashreports/status,verbs=update;patch
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
	if report.Status.Phase == "Processed" {
		logger.V(1).Info("PodCrashReport already processed, skipping", "name", report.Name)
		return ctrl.Result{}, nil
	}

	// Transition phase from empty/"Pending" to "Processed".
	patch := client.MergeFrom(report.DeepCopy())
	report.Status.Phase = "Processed"
	if err := r.Status().Patch(ctx, &report, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update PodCrashReport status: %w", err)
	}

	logger.Info("PodCrashReport processed",
		"name", report.Name,
		"pod", report.Spec.PodName,
		"container", report.Spec.ContainerName,
		"reason", report.Spec.Termination,
		"exitCode", report.Spec.ExitCode,
	)

	// Record a Kubernetes Event on the PodCrashReport resource.
	r.Recorder.Eventf(&report, "Normal", "Processed",
		"Processed crash report for pod %s/%s (container: %s, reason: %s, exit code: %d)",
		report.Spec.Namespace, report.Spec.PodName,
		report.Spec.ContainerName, report.Spec.Termination, report.Spec.ExitCode,
	)

	// Update Prometheus metrics.
	ReportsCreatedTotal.WithLabelValues(
		report.Spec.Namespace,
		report.Spec.NodeName,
		report.Spec.Termination,
	).Inc()

	if report.Spec.Termination == "OOMKilled" {
		OOMEventsTotal.WithLabelValues(
			report.Spec.Namespace,
			report.Spec.NodeName,
		).Inc()
		
		if report.Status.Diagnostics.RSSDissection != nil {
			VictimAnonRSSBytes.WithLabelValues(
				report.Spec.Namespace,
				report.Spec.ContainerName,
			).Observe(float64(report.Status.Diagnostics.RSSDissection.AnonRSSBytes))
		}
		
		if report.Status.Diagnostics.TriggerComm != "" {
			TriggerProcessesTotal.WithLabelValues(
				report.Status.Diagnostics.TriggerComm,
			).Inc()
		}
	}

	// Send webhook notification if configured.
	if r.WebhookSender != nil {
		if err := r.WebhookSender.Send(ctx, &report); err != nil {
			// Log the error but don't fail reconciliation — the report is
			// already marked as Processed.
			logger.Error(err, "Failed to send webhook notification", "name", report.Name)
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
