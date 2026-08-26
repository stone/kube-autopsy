// Package controller implements the kube-autopsy controller components:
// the reconciler, garbage collector, webhook sender, and Prometheus metrics.
package controller

import (
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	// maxLabelCardinality bounds how many distinct values a tenant-influenced
	// label may take before further values collapse into overflowLabel. A
	// process name comes straight from the victim's comm, which any workload
	// can set to anything it likes via prctl(PR_SET_NAME) and change at will —
	// without a bound, a pod that OOMs in a loop under rotating names creates
	// unbounded time series and can exhaust Prometheus's memory.
	maxLabelCardinality = 100

	// overflowLabel replaces a value once maxLabelCardinality is reached.
	overflowLabel = "other"

	// unknownLabel replaces an empty value, since an empty label is
	// indistinguishable from an absent one in most queries.
	unknownLabel = "unknown"

	// maxLabelValueLength caps a single label value. comm is 16 bytes, but
	// nothing guarantees that for other sources.
	maxLabelValueLength = 64
)

// labelLimiter bounds the set of distinct values a metric label may take.
type labelLimiter struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

func newLabelLimiter() *labelLimiter {
	return &labelLimiter{seen: make(map[string]struct{})}
}

// bound sanitizes value and collapses it to overflowLabel once the limiter has
// already admitted maxLabelCardinality distinct values.
func (l *labelLimiter) bound(value string) string {
	value = sanitizeLabelValue(value)
	if value == unknownLabel {
		return value
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if _, ok := l.seen[value]; ok {
		return value
	}
	if len(l.seen) >= maxLabelCardinality {
		return overflowLabel
	}
	l.seen[value] = struct{}{}
	return value
}

// sanitizeLabelValue makes a kernel-supplied string safe to use as a Prometheus
// label value. comm is raw bytes from the kernel, so it is not necessarily
// valid UTF-8, and invalid UTF-8 corrupts the entire /metrics exposition rather
// than just this one series.
func sanitizeLabelValue(value string) string {
	if value == "" {
		return unknownLabel
	}

	if len(value) > maxLabelValueLength {
		value = value[:maxLabelValueLength]
	}

	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		switch {
		case r == utf8.RuneError:
			b.WriteByte('_')
		case unicode.IsPrint(r):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}

	if out := b.String(); out != "" {
		return out
	}
	return unknownLabel
}

var (
	// triggerCommLimiter bounds the tenant-controlled process-name label.
	triggerCommLimiter = newLabelLimiter()
	// containerNameLimiter bounds the container-name label, which is chosen by
	// whoever can create pods.
	containerNameLimiter = newLabelLimiter()
)

var (
	// ReportsCreatedTotal counts the total number of PodCrashReports created,
	// partitioned by namespace, node, and termination reason.
	ReportsCreatedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kube_autopsy_reports_created_total",
			Help: "Total number of PodCrashReports created.",
		},
		[]string{"namespace", "node", "reason"},
	)

	// OOMEventsTotal counts the total number of OOM kill events detected,
	// partitioned by namespace and node.
	OOMEventsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kube_autopsy_oom_events_total",
			Help: "Total number of OOM kill events detected.",
		},
		[]string{"namespace", "node"},
	)

	// ReportAgeSeconds observes the age (in seconds) of PodCrashReports at
	// the time they are garbage-collected.
	ReportAgeSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "kube_autopsy_report_age_seconds",
			Help:    "Age of PodCrashReports in seconds at GC deletion time.",
			Buckets: []float64{3600, 7200, 14400, 28800, 57600, 86400},
		},
	)

	// VictimAnonRSSBytes observes the Anonymous RSS footprint of the victim.
	VictimAnonRSSBytes = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "kube_autopsy_victim_anon_rss_bytes",
			Help:    "Anonymous RSS footprint of the OOM victim in bytes.",
			Buckets: []float64{1024 * 1024 * 10, 1024 * 1024 * 50, 1024 * 1024 * 100, 1024 * 1024 * 500, 1024 * 1024 * 1024, 1024 * 1024 * 1024 * 5}, // 10M, 50M, 100M, 500M, 1G, 5G
		},
		[]string{"namespace", "container"},
	)

	// TriggerProcessesTotal tracks the triggering process names.
	TriggerProcessesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kube_autopsy_trigger_processes_total",
			Help: "Total number of times a specific command triggered an OOM.",
		},
		[]string{"comm"},
	)

	// WebhookDeliveriesTotal counts webhook delivery outcomes. Without it an
	// endpoint that has been failing for a week is invisible: the give-up path
	// leaves only a log line, and a report whose alert was dropped is otherwise
	// indistinguishable from one that was delivered.
	//
	// result is "success", "retry" (failed, will be retried) or "dropped" (the
	// retry window expired and the alert was lost).
	WebhookDeliveriesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kube_autopsy_webhook_deliveries_total",
			Help: "Webhook delivery attempts by outcome.",
		},
		[]string{"result"},
	)

	// WebhookDurationSeconds measures how long delivery takes. Delivery blocks a
	// reconcile worker, so its latency is also the controller's throughput.
	WebhookDurationSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "kube_autopsy_webhook_duration_seconds",
			Help:    "Time spent delivering a webhook notification, in seconds.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		},
	)

	// ReportsTrimmedTotal counts reports deleted to honour --max-reports rather
	// than because they aged out, so trimming is never silent.
	ReportsTrimmedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "kube_autopsy_reports_trimmed_total",
			Help: "Reports deleted because the cluster-wide report cap was reached.",
		},
	)

	// GCErrorsTotal counts reports the collector failed to delete. A report that
	// cannot be deleted is retried every pass forever, so a non-zero rate here
	// means retention is not actually being enforced.
	GCErrorsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "kube_autopsy_gc_errors_total",
			Help: "Failed PodCrashReport deletions during garbage collection.",
		},
	)
)

// RegisterMetrics registers all kube-autopsy Prometheus metrics with the
// default registerer. It should be called once during controller startup.
func RegisterMetrics() {
	crmetrics.Registry.MustRegister(
		ReportsCreatedTotal,
		OOMEventsTotal,
		ReportAgeSeconds,
		VictimAnonRSSBytes,
		TriggerProcessesTotal,
		WebhookDeliveriesTotal,
		WebhookDurationSeconds,
		ReportsTrimmedTotal,
		GCErrorsTotal,
	)
}
