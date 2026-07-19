package config

import (
	"flag"
	"os"
	"testing"
	"time"
)

func TestNewConfig(t *testing.T) {
	cfg := NewConfig()

	if cfg.TTLHours != 24 {
		t.Errorf("expected default TTLHours to be 24, got %d", cfg.TTLHours)
	}
	if cfg.LogTailLines != 50 {
		t.Errorf("expected default LogTailLines to be 50, got %d", cfg.LogTailLines)
	}
	if cfg.MetricsBindAddr != ":8080" {
		t.Errorf("expected default MetricsBindAddr to be :8080, got %s", cfg.MetricsBindAddr)
	}
	if !cfg.LeaderElect {
		t.Error("expected default LeaderElect to be true")
	}
}

func TestBindFlags(t *testing.T) {
	cfg := NewConfig()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg.BindFlags(fs)

	// Test flag overrides
	err := fs.Parse([]string{
		"--ttl-hours=48",
		"--log-tail-lines=100",
		"--metrics-bind-addr=:9090",
		"--leader-elect=false",
	})
	if err != nil {
		t.Fatalf("failed to parse flags: %v", err)
	}

	if cfg.TTLHours != 48 {
		t.Errorf("expected TTLHours to be 48, got %d", cfg.TTLHours)
	}
	if cfg.LogTailLines != 100 {
		t.Errorf("expected LogTailLines to be 100, got %d", cfg.LogTailLines)
	}
	if cfg.MetricsBindAddr != ":9090" {
		t.Errorf("expected MetricsBindAddr to be :9090, got %s", cfg.MetricsBindAddr)
	}
	if cfg.LeaderElect {
		t.Error("expected LeaderElect to be false")
	}
}

func TestLoadFromEnv(t *testing.T) {
	cfg := NewConfig()

	// Set environment variables
	os.Setenv("KUBE_AUTOPSY_TTL_HOURS", "72")
	os.Setenv("KUBE_AUTOPSY_LOG_TAIL_LINES", "200")
	os.Setenv("KUBE_AUTOPSY_METRICS_BIND_ADDR", ":9091")
	os.Setenv("KUBE_AUTOPSY_WEBHOOK_URL", "https://hooks.slack.com/test")
	os.Setenv("KUBE_AUTOPSY_LEADER_ELECT", "false")

	// Cleanup environment variables after test
	defer func() {
		os.Unsetenv("KUBE_AUTOPSY_TTL_HOURS")
		os.Unsetenv("KUBE_AUTOPSY_LOG_TAIL_LINES")
		os.Unsetenv("KUBE_AUTOPSY_METRICS_BIND_ADDR")
		os.Unsetenv("KUBE_AUTOPSY_WEBHOOK_URL")
		os.Unsetenv("KUBE_AUTOPSY_LEADER_ELECT")
	}()

	cfg.LoadFromEnv()

	if cfg.TTLHours != 72 {
		t.Errorf("expected TTLHours to be 72, got %d", cfg.TTLHours)
	}
	if cfg.LogTailLines != 200 {
		t.Errorf("expected LogTailLines to be 200, got %d", cfg.LogTailLines)
	}
	if cfg.MetricsBindAddr != ":9091" {
		t.Errorf("expected MetricsBindAddr to be :9091, got %s", cfg.MetricsBindAddr)
	}
	if cfg.WebhookURL != "https://hooks.slack.com/test" {
		t.Errorf("expected WebhookURL to be https://hooks.slack.com/test, got %s", cfg.WebhookURL)
	}
	if cfg.LeaderElect {
		t.Error("expected LeaderElect to be false")
	}
}

func TestTTLDuration(t *testing.T) {
	cfg := NewConfig()
	cfg.TTLHours = 2

	expected := 2 * time.Hour
	if got := cfg.TTLDuration(); got != expected {
		t.Errorf("expected TTLDuration %v, got %v", expected, got)
	}
}
