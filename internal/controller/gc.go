package controller

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kube-autopsy/kube-autopsy/api/v1alpha1"
)

const gcInterval = 5 * time.Minute

// GarbageCollector periodically deletes PodCrashReport resources that have
// exceeded the configured time-to-live (TTL).
type GarbageCollector struct {
	client client.Client
	ttl    time.Duration
}

// NewGarbageCollector creates a GarbageCollector that will delete PodCrashReport
// resources older than ttl.
func NewGarbageCollector(c client.Client, ttl time.Duration) *GarbageCollector {
	return &GarbageCollector{
		client: c,
		ttl:    ttl,
	}
}

// Start begins the garbage collection loop, which fires every 5 minutes. It
// respects context cancellation for graceful shutdown and runs an initial
// collection immediately on startup.
func (gc *GarbageCollector) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("gc")
	logger.Info("Starting garbage collector", "ttl", gc.ttl.String(), "interval", gcInterval.String())

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

	for i := range reportList.Items {
		report := &reportList.Items[i]
		age := now.Sub(report.CreationTimestamp.Time)

		if age <= gc.ttl {
			continue
		}

		// Record the age metric before deletion.
		ReportAgeSeconds.Observe(age.Seconds())

		logger.Info("Deleting expired PodCrashReport",
			"name", report.Name,
			"namespace", report.Namespace,
			"age", age.String(),
		)

		if err := gc.client.Delete(ctx, report); err != nil {
			logger.Error(err, "Failed to delete PodCrashReport",
				"name", report.Name,
				"namespace", report.Namespace,
			)
			lastErr = err
			continue
		}
		deleted++
	}

	return deleted, lastErr
}
