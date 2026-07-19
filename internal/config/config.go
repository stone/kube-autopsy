// Package config provides centralized configuration for kube-autopsy.
// All flags can also be set via environment variables with the KUBE_AUTOPSY_ prefix.
package config

import (
	"flag"
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
	// MetricsBindAddr is the address for the Prometheus metrics endpoint.
	MetricsBindAddr string
	// HealthProbeBindAddr is the address for health/readiness probes.
	HealthProbeBindAddr string
	// WebhookURL is the optional webhook URL for crash notifications.
	WebhookURL string
	// LeaderElect enables leader election for the controller.
	LeaderElect bool
}

// NewConfig returns a Config with sensible defaults.
func NewConfig() *Config {
	return &Config{
		TTLHours:            24,
		LogTailLines:        50,
		MetricsBindAddr:     ":8080",
		HealthProbeBindAddr: ":8081",
		WebhookURL:          "",
		LeaderElect:         true,
	}
}

// BindFlags registers all configuration flags on the given FlagSet.
func (c *Config) BindFlags(fs *flag.FlagSet) {
	fs.IntVar(&c.TTLHours, "ttl-hours", c.TTLHours,
		"Hours before a PodCrashReport is garbage-collected")
	fs.IntVar(&c.LogTailLines, "log-tail-lines", c.LogTailLines,
		"Number of log tail lines to capture per container")
	fs.StringVar(&c.MetricsBindAddr, "metrics-bind-addr", c.MetricsBindAddr,
		"Address for the Prometheus metrics endpoint")
	fs.StringVar(&c.HealthProbeBindAddr, "health-probe-bind-addr", c.HealthProbeBindAddr,
		"Address for health/readiness probes")
	fs.StringVar(&c.WebhookURL, "webhook-url", c.WebhookURL,
		"Optional webhook URL for crash notifications (Slack, PagerDuty)")
	fs.BoolVar(&c.LeaderElect, "leader-elect", c.LeaderElect,
		"Enable leader election for the controller")
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
	if v := os.Getenv("KUBE_AUTOPSY_METRICS_BIND_ADDR"); v != "" {
		c.MetricsBindAddr = v
	}
	if v := os.Getenv("KUBE_AUTOPSY_HEALTH_PROBE_BIND_ADDR"); v != "" {
		c.HealthProbeBindAddr = v
	}
	if v := os.Getenv("KUBE_AUTOPSY_WEBHOOK_URL"); v != "" {
		c.WebhookURL = v
	}
	if v := os.Getenv("KUBE_AUTOPSY_LEADER_ELECT"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.LeaderElect = b
		}
	}
}

// TTLDuration returns the TTL as a time.Duration.
func (c *Config) TTLDuration() time.Duration {
	return time.Duration(c.TTLHours) * time.Hour
}
