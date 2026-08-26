// Package config provides centralized configuration for kube-autopsy.
// All flags can also be set via environment variables with the KUBE_AUTOPSY_ prefix.
package config

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	autopsy "github.com/kube-autopsy/kube-autopsy/api/v1alpha1"
)

// maxTTLHours bounds --ttl-hours. TTLDuration multiplies by time.Hour, which
// overflows int64 past roughly 2.5 million hours and wraps to a negative
// duration — under which the garbage collector considers every report expired
// and deletes the lot on its first pass, which runs immediately at startup.
// Ten years is far beyond any real retention policy and nowhere near the wrap.
const maxTTLHours = 24 * 365 * 10

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
	// MaxConcurrentReconciles bounds how many reports the controller processes
	// at once. Webhook delivery happens inline and can block for the client's
	// full timeout, so with a single worker one unresponsive endpoint stalls
	// every other report behind it.
	MaxConcurrentReconciles int
	// MaxReports caps how many PodCrashReports are kept cluster-wide, oldest
	// deleted first. Retention was time-based only, which bounds how long a
	// report lives but not how many exist at once — so a cluster-wide crash loop
	// could grow the collection until the controller could no longer hold it.
	// Zero disables the cap.
	MaxReports int
	// WebhookURLWasFlag records that the URL came from the command line rather
	// than the environment, so the caller can warn about it. It is resolved
	// during ResolveSecrets, since that is where precedence is decided.
	WebhookURLWasFlag bool

	// webhookURLFromEnv holds KUBE_AUTOPSY_WEBHOOK_URL until flags have been
	// parsed. It is deliberately kept out of WebhookURL until then: BindFlags
	// registers the current value as each flag's default, and flag.PrintDefaults
	// prints every default. Since flag.CommandLine is ExitOnError, one mistyped
	// flag dumps the usage — and with it a credential — into the container log.
	webhookURLFromEnv string
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

		MaxConcurrentReconciles: 4,
		MaxReports:              10000,
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
	// Bound with an empty default rather than the current value: anything passed
	// to StringVar as a default is printed verbatim by flag.PrintDefaults, so
	// seeding it from the environment would publish the Secret. ResolveSecrets
	// puts the environment value back once parsing has decided precedence.
	fs.StringVar(&c.WebhookURL, "webhook-url", "",
		"Optional webhook URL for crash notifications (Slack, PagerDuty). "+
			"DEPRECATED: prefer KUBE_AUTOPSY_WEBHOOK_URL from a Secret, since a "+
			"flag value is readable by anyone who can get the pod spec")
	fs.BoolVar(&c.WebhookIncludeLogs, "webhook-include-logs", c.WebhookIncludeLogs,
		"Include captured log lines in the webhook payload, sending them outside the cluster")
	fs.BoolVar(&c.LeaderElect, "leader-elect", c.LeaderElect,
		"Enable leader election for the controller")
	fs.IntVar(&c.MaxConcurrentReconciles, "max-concurrent-reconciles", c.MaxConcurrentReconciles,
		"Reports the controller processes at once. Webhook delivery blocks the "+
			"worker that runs it, so a single worker lets one slow endpoint stall "+
			"every other report")
	fs.IntVar(&c.MaxReports, "max-reports", c.MaxReports,
		"Cap on how many PodCrashReports are kept cluster-wide, oldest deleted "+
			"first, so a cluster-wide crash loop cannot grow the collection past "+
			"what the controller can hold. 0 disables the cap")
}

// ResolveSecrets applies environment-sourced credentials that were deliberately
// withheld from the flag defaults, preserving flag > environment > default. It
// must be called after fs.Parse and before Validate.
func (c *Config) ResolveSecrets(fs *flag.FlagSet) {
	c.WebhookURLWasFlag = c.WebhookURLFromFlag(fs)
	if !c.WebhookURLWasFlag {
		c.WebhookURL = c.webhookURLFromEnv
	}
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
//
// A value that does not parse is an error rather than a silent fallback to the
// default. Discarding it quietly turns a typo in a ConfigMap into a security
// posture nobody chose — KUBE_AUTOPSY_CAPTURE_LOGS=yes leaves capture off,
// KUBE_AUTOPSY_METRICS_SECURE=no leaves metrics authenticated — and the operator
// has no way to tell the setting did not take.
func (c *Config) LoadFromEnv() error {
	var errs []error

	envInt := func(key string, target *int) {
		v := os.Getenv(key)
		if v == "" {
			return
		}
		i, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %q is not an integer", key, v))
			return
		}
		*target = i
	}

	envBool := func(key string, target *bool) {
		v := os.Getenv(key)
		if v == "" {
			return
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"%s: %q is not a boolean (use true or false)", key, v))
			return
		}
		*target = b
	}

	envString := func(key string, target *string) {
		if v := os.Getenv(key); v != "" {
			*target = v
		}
	}

	envInt("KUBE_AUTOPSY_TTL_HOURS", &c.TTLHours)
	envInt("KUBE_AUTOPSY_LOG_TAIL_LINES", &c.LogTailLines)
	envInt("KUBE_AUTOPSY_MAX_CONCURRENT_REPORTS", &c.MaxConcurrentReports)
	envInt("KUBE_AUTOPSY_MAX_CONCURRENT_RECONCILES", &c.MaxConcurrentReconciles)
	envInt("KUBE_AUTOPSY_REPORT_COOLDOWN_SECONDS", &c.ReportCooldownSeconds)

	envBool("KUBE_AUTOPSY_POD_OWNER_REFERENCE", &c.PodOwnerReference)
	envBool("KUBE_AUTOPSY_CAPTURE_LOGS", &c.CaptureLogs)
	envBool("KUBE_AUTOPSY_METRICS_SECURE", &c.MetricsSecure)
	envBool("KUBE_AUTOPSY_WEBHOOK_INCLUDE_LOGS", &c.WebhookIncludeLogs)
	envBool("KUBE_AUTOPSY_LEADER_ELECT", &c.LeaderElect)

	envString("KUBE_AUTOPSY_METRICS_BIND_ADDR", &c.MetricsBindAddr)
	envString("KUBE_AUTOPSY_HEALTH_PROBE_BIND_ADDR", &c.HealthProbeBindAddr)

	// Held aside rather than assigned to WebhookURL, so it never becomes a flag
	// default; ResolveSecrets applies it after parsing. Surrounding whitespace is
	// trimmed because the common `--from-file` and `$(cat …)` recipes for the
	// Secret leave a trailing newline, which makes the URL unparseable.
	if v := os.Getenv("KUBE_AUTOPSY_WEBHOOK_URL"); v != "" {
		c.webhookURLFromEnv = strings.TrimSpace(v)
	}
	// Deliberately environment-only: a credential must not be settable from a
	// flag, where it would be readable in the pod spec and in process listings.
	if v := os.Getenv("KUBE_AUTOPSY_WEBHOOK_AUTH_HEADER"); v != "" {
		c.WebhookAuthHeader = strings.TrimSpace(v)
	}

	return errors.Join(errs...)
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
	if c.TTLHours > maxTTLHours {
		errs = append(errs, fmt.Errorf(
			"ttl-hours must be at most %d (about ten years), got %d: a larger value "+
				"overflows the retention duration and makes the collector delete every report",
			maxTTLHours, c.TTLHours))
	}
	if c.LogTailLines < 1 {
		errs = append(errs, fmt.Errorf("log-tail-lines must be at least 1, got %d", c.LogTailLines))
	}
	if c.LogTailLines > autopsy.MaxLogLines {
		errs = append(errs, fmt.Errorf(
			"log-tail-lines must be at most %d, got %d: the PodCrashReport schema "+
				"rejects a longer list, which would fail the status write and lose "+
				"every other diagnostic along with the logs",
			autopsy.MaxLogLines, c.LogTailLines))
	}
	if c.MaxConcurrentReports < 1 {
		errs = append(errs, fmt.Errorf("max-concurrent-reports must be at least 1, got %d", c.MaxConcurrentReports))
	}
	if c.MaxConcurrentReconciles < 1 {
		errs = append(errs, fmt.Errorf("max-concurrent-reconciles must be at least 1, got %d", c.MaxConcurrentReconciles))
	}
	if c.MaxReports < 0 {
		errs = append(errs, fmt.Errorf("max-reports cannot be negative, got %d", c.MaxReports))
	}
	if c.ReportCooldownSeconds < 0 {
		errs = append(errs, fmt.Errorf("report-cooldown-seconds cannot be negative, got %d", c.ReportCooldownSeconds))
	}
	// Checked at startup rather than at the first crash: an unusable URL
	// otherwise fails once per report, forever, and each failure is a chance to
	// spill the credential into a log.
	if c.WebhookURL != "" {
		if err := validateWebhookURL(c.WebhookURL); err != nil {
			// The URL is a credential, so the offending value is never quoted back.
			errs = append(errs, fmt.Errorf("webhook URL is not usable: %w", err))
		}
	}

	return errors.Join(errs...)
}

// validateWebhookURL rejects a URL the HTTP client could not use, without
// including the URL itself in the error.
func validateWebhookURL(raw string) error {
	if raw != strings.TrimSpace(raw) {
		return errors.New("it has leading or trailing whitespace")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("it is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("its scheme is %q, want http or https", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("it has no host")
	}
	return nil
}

// TTLDuration returns the TTL as a time.Duration.
func (c *Config) TTLDuration() time.Duration {
	return time.Duration(c.TTLHours) * time.Hour
}

// ReportCooldown returns the per-container report suppression window.
func (c *Config) ReportCooldown() time.Duration {
	return time.Duration(c.ReportCooldownSeconds) * time.Second
}
