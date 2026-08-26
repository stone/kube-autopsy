package controller

import (
	"context"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kube-autopsy/kube-autopsy/api/v1alpha1"
)

const gcInterval = 5 * time.Minute

// GarbageCollector periodically deletes PodCrashReport resources that have
// exceeded the configured time-to-live (TTL), or that fall outside the
// cluster-wide report cap.
type GarbageCollector struct {
	client client.Client
	ttl    time.Duration
	// maxReports caps how many reports are kept at once, oldest deleted first.
	// The TTL bounds how long a report lives but not how many exist, so a
	// cluster-wide crash loop could otherwise grow the collection until the
	// controller could no longer hold it. Zero disables the cap.
	maxReports int
}

// NewGarbageCollector creates a GarbageCollector that will delete PodCrashReport
// resources older than ttl, and trim the collection to maxReports (0 for no cap).
func NewGarbageCollector(c client.Client, ttl time.Duration, maxReports int) *GarbageCollector {
	return &GarbageCollector{
		client:     c,
		ttl:        ttl,
		maxReports: maxReports,
	}
}

// Start begins the garbage collection loop, which fires every 5 minutes. It
// respects context cancellation for graceful shutdown and runs an initial
// collection immediately on startup.
func (gc *GarbageCollector) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("gc")
	logger.Info("Starting garbage collector",
		"ttl", gc.ttl.String(), "interval", gcInterval.String(), "maxReports", gc.maxReports)

	// Run an initial collection immediately.
	if deleted, err := gc.deleteExpiredReports(ctx); err != nil {
		logger.Error(err, "Initial garbage collection encountered errors", "deleted", deleted)
	} else if deleted > 0 {
		logger.Info("Initial garbage collection completed", "deleted", deleted)
	}

	ticker := time.NewTicker(gcInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("Garbage collector shutting down")
			return nil
		case <-ticker.C:
			deleted, err := gc.deleteExpiredReports(ctx)
			if err != nil {
				logger.Error(err, "Garbage collection encountered errors", "deleted", deleted)
			} else if deleted > 0 {
				logger.Info("Garbage collection completed", "deleted", deleted)
			}
		}
	}
}

// deleteExpiredReports lists all PodCrashReport resources and deletes those
// with a creation timestamp older than the configured TTL. It returns the
// number of successfully deleted reports. Partial failures are handled
// gracefully — the function continues deleting remaining reports even if
// individual deletions fail.
func (gc *GarbageCollector) deleteExpiredReports(ctx context.Context) (int, error) {
	logger := log.FromContext(ctx).WithName("gc")

	var reportList v1alpha1.PodCrashReportList
	if err := gc.client.List(ctx, &reportList); err != nil {
		return 0, err
	}

	now := time.Now()
	deleted := 0
	var lastErr error

	// Oldest first, so the over-cap trim below drops the least useful reports.
	items := make([]*v1alpha1.PodCrashReport, 0, len(reportList.Items))
	for i := range reportList.Items {
		items = append(items, &reportList.Items[i])
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].CreationTimestamp.Time.Before(items[j].CreationTimestamp.Time)
	})

	// Expiry is decided first, and the cap is then applied to what would have
	// survived it. Testing the cap against the whole list instead would label an
	// ordinary expiry as a trim whenever the collection also happened to be over
	// the cap, making kube_autopsy_reports_trimmed_total report pressure that was
	// not there.
	live := 0
	for _, report := range items {
		if now.Sub(report.CreationTimestamp.Time) <= gc.ttl {
			live++
		}
	}

	overCap := 0
	if gc.maxReports > 0 && live > gc.maxReports {
		overCap = live - gc.maxReports
		logger.Info("Report count is over the cap, trimming oldest",
			"live", live, "cap", gc.maxReports, "trimming", overCap)
	}

	// Counts down as unexpired reports are trimmed, oldest first.
	remainingTrim := overCap

	for _, report := range items {
		age := now.Sub(report.CreationTimestamp.Time)
		expired := age > gc.ttl

		trimmed := false
		if !expired && remainingTrim > 0 {
			trimmed = true
			remainingTrim--
		}

		if !expired && !trimmed {
			continue
		}

		logger.Info("Deleting PodCrashReport",
			"name", report.Name,
			"namespace", report.Namespace,
			"age", age.String(),
			"reason", deleteReason(trimmed),
		)

		if err := gc.client.Delete(ctx, report); err != nil {
			// Already gone: the cache was stale, or the pod owning the report was
			// deleted between the list and now. That is the outcome this pass
			// wanted, so it is neither an error nor a deletion this pass made.
			if apierrors.IsNotFound(err) {
				continue
			}
			logger.Error(err, "Failed to delete PodCrashReport",
				"name", report.Name,
				"namespace", report.Namespace,
			)
			GCErrorsTotal.Inc()
			lastErr = err
			continue
		}

		// Observed only on success. Recording it before the delete meant a report
		// that could not be deleted — a stuck finalizer, a revoked RBAC rule —
		// was re-observed with a larger age on every pass, forever, until the
		// histogram described that one report rather than the fleet.
		ReportAgeSeconds.Observe(age.Seconds())
		if trimmed {
			ReportsTrimmedTotal.Inc()
		}
		deleted++
	}

	return deleted, lastErr
}

func deleteReason(trimmed bool) string {
	if trimmed {
		return "over-cap"
	}
	return "expired"
}
