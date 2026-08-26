package config

import (
	"flag"
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
	if cfg.MetricsBindAddr != ":8443" {
		t.Errorf("expected default MetricsBindAddr to be :8443, got %s", cfg.MetricsBindAddr)
	}
	if !cfg.MetricsSecure {
		t.Error("expected metrics to be served securely by default")
	}
	// Log content is sensitive, so capture must be opt-in and must not leave
	// the cluster unless explicitly allowed.
	if cfg.CaptureLogs {
		t.Error("expected CaptureLogs to default to false")
	}
	if cfg.WebhookIncludeLogs {
		t.Error("expected WebhookIncludeLogs to default to false")
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

	// t.Setenv restores the previous values when the test finishes.
	t.Setenv("KUBE_AUTOPSY_TTL_HOURS", "72")
	t.Setenv("KUBE_AUTOPSY_LOG_TAIL_LINES", "200")
	t.Setenv("KUBE_AUTOPSY_METRICS_BIND_ADDR", ":9091")
	t.Setenv("KUBE_AUTOPSY_WEBHOOK_URL", "https://hooks.slack.com/test")
	t.Setenv("KUBE_AUTOPSY_LEADER_ELECT", "false")

	if err := cfg.LoadFromEnv(); err != nil {
		t.Fatalf("LoadFromEnv returned an error: %v", err)
	}

	if cfg.TTLHours != 72 {
		t.Errorf("expected TTLHours to be 72, got %d", cfg.TTLHours)
	}
	if cfg.LogTailLines != 200 {
		t.Errorf("expected LogTailLines to be 200, got %d", cfg.LogTailLines)
	}
	if cfg.MetricsBindAddr != ":9091" {
		t.Errorf("expected MetricsBindAddr to be :9091, got %s", cfg.MetricsBindAddr)
	}
	// The webhook URL is deliberately withheld from WebhookURL until
	// ResolveSecrets runs: binding it before flags are registered would make it
	// a flag default, and flag.PrintDefaults publishes every default.
	if cfg.WebhookURL != "" {
		t.Errorf("expected WebhookURL to stay empty until ResolveSecrets, got %s", cfg.WebhookURL)
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg.BindFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("failed to parse flags: %v", err)
	}
	cfg.ResolveSecrets(fs)
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

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{
			name:    "defaults are valid",
			mutate:  func(*Config) {},
			wantErr: false,
		},
		{
			name:    "zero ttl-hours would GC every report immediately",
			mutate:  func(c *Config) { c.TTLHours = 0 },
			wantErr: true,
		},
		{
			name:    "negative ttl-hours",
			mutate:  func(c *Config) { c.TTLHours = -1 },
			wantErr: true,
		},
		{
			name:    "zero log-tail-lines",
			mutate:  func(c *Config) { c.LogTailLines = 0 },
			wantErr: true,
		},
		{
			name:    "negative log-tail-lines used to panic the agent",
			mutate:  func(c *Config) { c.LogTailLines = -1 },
			wantErr: true,
		},
		{
			name: "reports every invalid field at once",
			mutate: func(c *Config) {
				c.TTLHours = 0
				c.LogTailLines = 0
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewConfig()
			tt.mutate(cfg)

			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Errorf("Validate() = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}
