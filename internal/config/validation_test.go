package config

import (
	"flag"
	"os"
	"strings"
	"testing"

	autopsy "github.com/kube-autopsy/kube-autopsy/api/v1alpha1"
)

// TTLDuration multiplies by time.Hour, which wraps int64 past ~2.5M hours. A
// negative TTL makes the collector treat every report as expired and delete the
// lot on the pass it runs at startup — so an operator asking for very long
// retention on a forensics tool got none at all.
func TestValidateRejectsOverflowingTTL(t *testing.T) {
	tests := []struct {
		name    string
		ttl     int
		wantErr bool
	}{
		{name: "default", ttl: 24, wantErr: false},
		{name: "ten years", ttl: maxTTLHours, wantErr: false},
		{name: "just over the cap", ttl: maxTTLHours + 1, wantErr: true},
		{name: "overflows int64", ttl: 8760000, wantErr: true},
		{name: "zero", ttl: 0, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewConfig()
			cfg.TTLHours = tt.ttl

			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			// Anything accepted must produce a usable, positive duration.
			if cfg.TTLDuration() <= 0 {
				t.Errorf("accepted ttl-hours=%d yields a non-positive duration %v",
					tt.ttl, cfg.TTLDuration())
			}
		})
	}
}

// The CRD caps lastLogLines at MaxLogLines. Since every diagnostic is written in
// one status patch, exceeding it fails the whole write — costing the memory
// figures and OOM scores, not just the logs.
func TestValidateRejectsLogTailLinesBeyondTheSchemaCap(t *testing.T) {
	tests := []struct {
		lines   int
		wantErr bool
	}{
		{lines: 1, wantErr: false},
		{lines: 50, wantErr: false},
		{lines: autopsy.MaxLogLines, wantErr: false},
		{lines: autopsy.MaxLogLines + 1, wantErr: true},
		{lines: 0, wantErr: true},
	}

	for _, tt := range tests {
		cfg := NewConfig()
		cfg.LogTailLines = tt.lines

		if err := cfg.Validate(); (err != nil) != tt.wantErr {
			t.Errorf("Validate() with log-tail-lines=%d error = %v, wantErr %v",
				tt.lines, err, tt.wantErr)
		}
	}
}

func TestValidateChecksConcurrencyAndCaps(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{name: "defaults", mutate: func(*Config) {}, wantErr: false},
		{name: "zero concurrent reports", mutate: func(c *Config) { c.MaxConcurrentReports = 0 }, wantErr: true},
		{name: "zero concurrent reconciles", mutate: func(c *Config) { c.MaxConcurrentReconciles = 0 }, wantErr: true},
		{name: "negative cooldown", mutate: func(c *Config) { c.ReportCooldownSeconds = -1 }, wantErr: true},
		{name: "zero cooldown disables suppression", mutate: func(c *Config) { c.ReportCooldownSeconds = 0 }, wantErr: false},
		{name: "negative report cap", mutate: func(c *Config) { c.MaxReports = -1 }, wantErr: true},
		{name: "zero report cap disables the cap", mutate: func(c *Config) { c.MaxReports = 0 }, wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewConfig()
			tt.mutate(cfg)
			if err := cfg.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// A webhook URL that cannot be used fails once per crash report, forever, and
// each failure is another chance to spill the credential into a log. It is
// cheaper to refuse at startup — and the error must never quote the URL.
func TestValidateWebhookURL(t *testing.T) {
	const secret = "T012B034SECRETPATH"

	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "https", url: "https://hooks.slack.com/services/" + secret, wantErr: false},
		{name: "http", url: "http://internal.example/hook", wantErr: false},
		{name: "empty is allowed", url: "", wantErr: false},
		{name: "trailing newline", url: "https://hooks.slack.com/services/" + secret + "\n", wantErr: true},
		{name: "leading space", url: " https://hooks.slack.com/x", wantErr: true},
		{name: "no scheme", url: "hooks.slack.com/services/x", wantErr: true},
		{name: "wrong scheme", url: "file:///etc/passwd", wantErr: true},
		{name: "no host", url: "https:///nohost", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewConfig()
			cfg.WebhookURL = tt.url

			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), secret) {
				t.Errorf("validation error quotes the credential: %v", err)
			}
		})
	}
}

// Silently keeping the default meant KUBE_AUTOPSY_CAPTURE_LOGS=yes left capture
// off and KUBE_AUTOPSY_METRICS_SECURE=no left metrics authenticated, with no
// way for the operator to tell the setting had not taken.
func TestLoadFromEnvRejectsUnparseableValues(t *testing.T) {
	tests := []struct {
		key   string
		value string
	}{
		{key: "KUBE_AUTOPSY_TTL_HOURS", value: "not-a-number"},
		{key: "KUBE_AUTOPSY_LOG_TAIL_LINES", value: "12x"},
		{key: "KUBE_AUTOPSY_CAPTURE_LOGS", value: "yes"},
		{key: "KUBE_AUTOPSY_METRICS_SECURE", value: "no"},
		{key: "KUBE_AUTOPSY_LEADER_ELECT", value: "maybe"},
	}

	for _, tt := range tests {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			t.Setenv(tt.key, tt.value)

			err := NewConfig().LoadFromEnv()
			if err == nil {
				t.Fatalf("expected %s=%q to be rejected", tt.key, tt.value)
			}
			if !strings.Contains(err.Error(), tt.key) {
				t.Errorf("error does not name the offending variable: %v", err)
			}
		})
	}
}

// The Secret is commonly created with --from-file or $(cat …), both of which
// keep a trailing newline that makes the URL unparseable.
func TestLoadFromEnvTrimsCredentialWhitespace(t *testing.T) {
	t.Setenv("KUBE_AUTOPSY_WEBHOOK_URL", "https://hooks.slack.com/services/x\n")
	t.Setenv("KUBE_AUTOPSY_WEBHOOK_AUTH_HEADER", "Bearer token\n")

	cfg := NewConfig()
	if err := cfg.LoadFromEnv(); err != nil {
		t.Fatalf("LoadFromEnv: %v", err)
	}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg.BindFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg.ResolveSecrets(fs)

	if cfg.WebhookURL != "https://hooks.slack.com/services/x" {
		t.Errorf("webhook URL was not trimmed: %q", cfg.WebhookURL)
	}
	if cfg.WebhookAuthHeader != "Bearer token" {
		t.Errorf("auth header was not trimmed: %q", cfg.WebhookAuthHeader)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("a trimmed URL should validate: %v", err)
	}
}

// flag.PrintDefaults writes every registered default, and flag.CommandLine is
// ExitOnError — so one mistyped flag dumps the usage, and with it anything bound
// as a default, into the container log. The webhook URL is a credential and must
// never be one.
func TestWebhookSecretIsNeverAFlagDefault(t *testing.T) {
	const secret = "https://hooks.slack.com/services/T00/B00/SUPERSECRET"
	t.Setenv("KUBE_AUTOPSY_WEBHOOK_URL", secret)

	cfg := NewConfig()
	if err := cfg.LoadFromEnv(); err != nil {
		t.Fatalf("LoadFromEnv: %v", err)
	}

	fs := flag.NewFlagSet("controller", flag.ContinueOnError)
	var usage strings.Builder
	fs.SetOutput(&usage)
	cfg.BindFlags(fs)

	fs.VisitAll(func(f *flag.Flag) {
		if strings.Contains(f.DefValue, "SUPERSECRET") {
			t.Errorf("flag -%s carries the credential as its default: %q", f.Name, f.DefValue)
		}
	})

	fs.PrintDefaults()
	if strings.Contains(usage.String(), "SUPERSECRET") {
		t.Errorf("the usage output contains the credential:\n%s", usage.String())
	}

	// Precedence must still hold: with no flag passed, the environment wins.
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg.ResolveSecrets(fs)
	if cfg.WebhookURL != secret {
		t.Errorf("environment value was not applied: %q", cfg.WebhookURL)
	}
	if cfg.WebhookURLWasFlag {
		t.Error("WebhookURLWasFlag should be false when the value came from the environment")
	}
}

// An explicitly passed flag still beats the environment.
func TestFlagBeatsEnvironmentForWebhookURL(t *testing.T) {
	t.Setenv("KUBE_AUTOPSY_WEBHOOK_URL", "https://from-env.example/hook")

	cfg := NewConfig()
	if err := cfg.LoadFromEnv(); err != nil {
		t.Fatalf("LoadFromEnv: %v", err)
	}

	fs := flag.NewFlagSet("controller", flag.ContinueOnError)
	cfg.BindFlags(fs)
	if err := fs.Parse([]string{"--webhook-url=https://from-flag.example/hook"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg.ResolveSecrets(fs)

	if cfg.WebhookURL != "https://from-flag.example/hook" {
		t.Errorf("flag did not win over the environment: %q", cfg.WebhookURL)
	}
	if !cfg.WebhookURLWasFlag {
		t.Error("WebhookURLWasFlag should be true when the flag was passed")
	}
}

func TestMain(m *testing.M) {
	// These tests set KUBE_AUTOPSY_* variables; make sure none leak in from the
	// surrounding environment and make the assertions meaningless.
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "KUBE_AUTOPSY_") {
			key, _, _ := strings.Cut(kv, "=")
			_ = os.Unsetenv(key)
		}
	}
	os.Exit(m.Run())
}
