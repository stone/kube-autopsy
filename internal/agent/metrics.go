package agent

import (
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// These are the agent's own signals: how long it takes to turn a kernel event
// into a report, and how often log capture loses the race with the container
// runtime tearing the log directory down. They previously lived in the
// controller package, where nothing could ever populate them.
var (
	// CaptureLatencySeconds measures detection through to the report being
	// written, which is the tool's core latency budget.
	CaptureLatencySeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "kube_autopsy_capture_latency_seconds",
			Help:    "Time from OOM detection to PodCrashReport creation in seconds.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		},
	)

	// LogCaptureFailuresTotal counts crashes where the log tail could not be
	// read. A rising rate means the runtime is reclaiming log files before the
	// agent reaches them.
	LogCaptureFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kube_autopsy_log_capture_failures_total",
			Help: "Total number of failed log tail attempts.",
		},
		[]string{"namespace"},
	)

	// ReportsSuppressedTotal counts crash events dropped by the per-container
	// cooldown, so a quiet report count can be distinguished from suppression.
	ReportsSuppressedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "kube_autopsy_reports_suppressed_total",
			Help: "Crash events not reported because the container was within its cooldown window.",
		},
	)

	// ReportErrorsTotal counts crash events that could not be turned into a
	// report, partitioned by the stage that failed.
	ReportErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kube_autopsy_report_errors_total",
			Help: "Crash events that could not be recorded, by failing stage.",
		},
		[]string{"stage"},
	)
)

// RegisterMetrics registers the agent's metrics with the controller-runtime
// registry. It should be called once during agent startup.
func RegisterMetrics() {
	crmetrics.Registry.MustRegister(
		CaptureLatencySeconds,
		LogCaptureFailuresTotal,
		ReportsSuppressedTotal,
		ReportErrorsTotal,
	)
}
