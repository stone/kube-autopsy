// Package controller implements the kube-autopsy controller components:
// the reconciler, garbage collector, webhook sender, and Prometheus metrics.
package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
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

	// CaptureLatencySeconds observes the latency (in seconds) from event
	// detection to CRD creation.
	CaptureLatencySeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "kube_autopsy_capture_latency_seconds",
			Help:    "Time from event detection to PodCrashReport creation in seconds.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		},
	)

	// LogCaptureFailuresTotal counts the number of failed log tail attempts,
	// partitioned by namespace.
	LogCaptureFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kube_autopsy_log_capture_failures_total",
			Help: "Total number of failed log tail attempts.",
		},
		[]string{"namespace"},
	)

	// VictimAnonRSSBytes observes the Anonymous RSS footprint of the victim.
	VictimAnonRSSBytes = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "kube_autopsy_victim_anon_rss_bytes",
			Help: "Anonymous RSS footprint of the OOM victim in bytes.",
			Buckets: []float64{1024*1024*10, 1024*1024*50, 1024*1024*100, 1024*1024*500, 1024*1024*1024, 1024*1024*1024*5}, // 10M, 50M, 100M, 500M, 1G, 5G
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
)

// RegisterMetrics registers all kube-autopsy Prometheus metrics with the
// default registerer. It should be called once during controller startup.
func RegisterMetrics() {
	crmetrics.Registry.MustRegister(
		ReportsCreatedTotal,
		OOMEventsTotal,
		ReportAgeSeconds,
		CaptureLatencySeconds,
		LogCaptureFailuresTotal,
		VictimAnonRSSBytes,
		TriggerProcessesTotal,
	)
}
