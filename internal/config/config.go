// Package config provides centralized configuration for kube-autopsy.
// All flags can also be set via environment variables with the KUBE_AUTOPSY_ prefix.
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all configuration for the kube-autopsy binary.
type Config struct {
	// TTLHours is the number of hours before a PodCrashReport is garbage-collected.
	TTLHours int
	// LogTailLines is the number of log tail lines to capture per container.
	LogTailLines int
	// PodOwnerReference makes each report owned by the pod it describes, so the
	// control plane deletes it with the pod. Off by default: a Deployment
	// rollout or a `kubectl delete pod` would otherwise destroy the record of
	// why the pod died, usually before anyone has read it. Retention is handled
	// by TTLHours instead.
	PodOwnerReference bool
	// MaxConcurrentReports bounds how many crash events are turned into reports
	// at once, so a burst of OOM kills cannot flood the API server.
	MaxConcurrentReports int
	// ReportCooldownSeconds suppresses repeat reports for the same container
	// within this window. A container in a tight OOM loop would otherwise
	// generate reports indefinitely, which is an etcd pressure vector available
	// to any unprivileged workload.
	ReportCooldownSeconds int
	// CaptureLogs enables copying the container's final log lines into the
	// PodCrashReport. It defaults to false: log content routinely contains
	// credentials and PII, and reading it via a PodCrashReport requires only
	// "get podcrashreports" rather than the "get pods/log" it would normally
	// be gated behind.
	CaptureLogs bool
	// MetricsBindAddr is the address for the Prometheus metrics endpoint.
	MetricsBindAddr string
	// MetricsSecure serves metrics over TLS and requires the scraper to
	// authenticate and pass a SubjectAccessReview.
	MetricsSecure bool
	// HealthProbeBindAddr is the address for health/readiness probes.
	HealthProbeBindAddr string
	// WebhookURL is the optional webhook URL for crash notifications. Prefer
	// the KUBE_AUTOPSY_WEBHOOK_URL environment variable sourced from a Secret:
	// webhook URLs are credentials, and a flag is visible in the pod spec to
	// anyone who can read pods.
	WebhookURL string
	// WebhookAuthHeader is an optional Authorization header value sent with
	// webhook requests. It can only be set from the environment.
	WebhookAuthHeader string
	// WebhookIncludeLogs allows captured log lines to leave the cluster in the
	// webhook payload. It defaults to false for the same reason as CaptureLogs.
	WebhookIncludeLogs bool
	// LeaderElect enables leader election for the controller.
	LeaderElect bool
}

// NewConfig returns a Config with sensible defaults.
func NewConfig() *Config {
	return &Config{
		TTLHours:              24,
		LogTailLines:          50,
		PodOwnerReference:     false,
		MaxConcurrentReports:  8,
		ReportCooldownSeconds: 30,
		CaptureLogs:           false,
		MetricsBindAddr:       ":8443",
		MetricsSecure:         true,
		HealthProbeBindAddr:   ":8081",
		WebhookURL:            "",
		WebhookIncludeLogs:    false,
		LeaderElect:           true,
	}
}

// BindFlags registers all configuration flags on the given FlagSet.
func (c *Config) BindFlags(fs *flag.FlagSet) {
	fs.IntVar(&c.TTLHours, "ttl-hours", c.TTLHours,
		"Hours before a PodCrashReport is garbage-collected")
	fs.IntVar(&c.LogTailLines, "log-tail-lines", c.LogTailLines,
		"Number of log tail lines to capture per container")
	fs.BoolVar(&c.PodOwnerReference, "pod-owner-reference", c.PodOwnerReference,
		"Own each report by its pod, so Kubernetes deletes the report when the pod "+
			"is deleted. Off by default: a rollout would otherwise destroy the "+
			"post-mortem before anyone reads it")
	fs.IntVar(&c.MaxConcurrentReports, "max-concurrent-reports", c.MaxConcurrentReports,
		"Maximum number of crash reports created concurrently")
	fs.IntVar(&c.ReportCooldownSeconds, "report-cooldown-seconds", c.ReportCooldownSeconds,
		"Suppress repeat reports for the same container within this many seconds")
	fs.BoolVar(&c.CaptureLogs, "capture-logs", c.CaptureLogs,
		"Capture the container's final log lines into the PodCrashReport. "+
			"Off by default: it exposes log content to anyone who can read "+
			"podcrashreports, which is a weaker permission than pods/log")
	fs.StringVar(&c.MetricsBindAddr, "metrics-bind-addr", c.MetricsBindAddr,
		"Address for the Prometheus metrics endpoint")
	fs.BoolVar(&c.MetricsSecure, "metrics-secure", c.MetricsSecure,
		"Serve metrics over TLS and require the scraper to be authenticated and authorized")
	fs.StringVar(&c.HealthProbeBindAddr, "health-probe-bind-addr", c.HealthProbeBindAddr,
		"Address for health/readiness probes")
	fs.StringVar(&c.WebhookURL, "webhook-url", c.WebhookURL,
		"Optional webhook URL for crash notifications (Slack, PagerDuty). "+
			"DEPRECATED: prefer KUBE_AUTOPSY_WEBHOOK_URL from a Secret, since a "+
			"flag value is readable by anyone who can get the pod spec")
	fs.BoolVar(&c.WebhookIncludeLogs, "webhook-include-logs", c.WebhookIncludeLogs,
		"Include captured log lines in the webhook payload, sending them outside the cluster")
	fs.BoolVar(&c.LeaderElect, "leader-elect", c.LeaderElect,
		"Enable leader election for the controller")
}

// WebhookURLFromFlag reports whether the webhook URL came from the command line
// rather than the environment, so the caller can warn about it.
func (c *Config) WebhookURLFromFlag(fs *flag.FlagSet) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "webhook-url" {
			set = true
		}
	})
	return set
}

// LoadFromEnv overrides config values from environment variables.
// Env vars follow the pattern KUBE_AUTOPSY_<FLAG_NAME> with dashes replaced by underscores.
func (c *Config) LoadFromEnv() {
	if v := os.Getenv("KUBE_AUTOPSY_TTL_HOURS"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.TTLHours = i
		}
	}
	if v := os.Getenv("KUBE_AUTOPSY_LOG_TAIL_LINES"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.LogTailLines = i
		}
	}
	if v := os.Getenv("KUBE_AUTOPSY_POD_OWNER_REFERENCE"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.PodOwnerReference = b
		}
	}
	if v := os.Getenv("KUBE_AUTOPSY_MAX_CONCURRENT_REPORTS"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.MaxConcurrentReports = i
		}
	}
	if v := os.Getenv("KUBE_AUTOPSY_REPORT_COOLDOWN_SECONDS"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.ReportCooldownSeconds = i
		}
	}
	if v := os.Getenv("KUBE_AUTOPSY_CAPTURE_LOGS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.CaptureLogs = b
		}
	}
	if v := os.Getenv("KUBE_AUTOPSY_METRICS_BIND_ADDR"); v != "" {
		c.MetricsBindAddr = v
	}
	if v := os.Getenv("KUBE_AUTOPSY_METRICS_SECURE"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.MetricsSecure = b
		}
	}
	if v := os.Getenv("KUBE_AUTOPSY_HEALTH_PROBE_BIND_ADDR"); v != "" {
		c.HealthProbeBindAddr = v
	}
	if v := os.Getenv("KUBE_AUTOPSY_WEBHOOK_URL"); v != "" {
		c.WebhookURL = v
	}
	// Deliberately environment-only: a credential must not be settable from a
	// flag, where it would be readable in the pod spec and in process listings.
	if v := os.Getenv("KUBE_AUTOPSY_WEBHOOK_AUTH_HEADER"); v != "" {
		c.WebhookAuthHeader = v
	}
	if v := os.Getenv("KUBE_AUTOPSY_WEBHOOK_INCLUDE_LOGS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.WebhookIncludeLogs = b
		}
	}
	if v := os.Getenv("KUBE_AUTOPSY_LEADER_ELECT"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.LeaderElect = b
		}
	}
}

// Validate reports whether the configuration is usable. It is called after
// flags and environment variables have been merged, and rejects values that
// would otherwise cause data loss or a crash at runtime: a non-positive TTL
// makes the garbage collector delete every report on its first pass, and a
// non-positive tail-line count is not a meaningful log capture request.
func (c *Config) Validate() error {
	var errs []error

	if c.TTLHours < 1 {
		errs = append(errs, fmt.Errorf("ttl-hours must be at least 1, got %d", c.TTLHours))
	}
	if c.LogTailLines < 1 {
		errs = append(errs, fmt.Errorf("log-tail-lines must be at least 1, got %d", c.LogTailLines))
	}
	if c.MaxConcurrentReports < 1 {
		errs = append(errs, fmt.Errorf("max-concurrent-reports must be at least 1, got %d", c.MaxConcurrentReports))
	}
	if c.ReportCooldownSeconds < 0 {
		errs = append(errs, fmt.Errorf("report-cooldown-seconds cannot be negative, got %d", c.ReportCooldownSeconds))
	}

	return errors.Join(errs...)
}

// TTLDuration returns the TTL as a time.Duration.
func (c *Config) TTLDuration() time.Duration {
	return time.Duration(c.TTLHours) * time.Hour
}

// ReportCooldown returns the per-container report suppression window.
func (c *Config) ReportCooldown() time.Duration {
	return time.Duration(c.ReportCooldownSeconds) * time.Second
}
