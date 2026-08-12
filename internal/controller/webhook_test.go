package controller

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kube-autopsy/kube-autopsy/api/v1alpha1"
)

func TestIsSlackURL(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		expected bool
	}{
		{
			name:     "official slack webhook",
			url:      "https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX",
			expected: true,
		},
		{
			name:     "custom slack proxy",
			url:      "http://internal-slack-proxy.local/alert",
			expected: true,
		},
		{
			name:     "pagerduty",
			url:      "https://events.pagerduty.com/v2/enqueue",
			expected: false,
		},
		{
			name:     "generic endpoint",
			url:      "https://webhook.site/12345",
			expected: false,
		},
		{
			name:     "empty string",
			url:      "",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isSlackURL(tt.url)
			if result != tt.expected {
				t.Errorf("isSlackURL(%q) = %v, want %v", tt.url, result, tt.expected)
			}
		})
	}
}

func TestNewWebhookSender(t *testing.T) {
	// Test empty URL
	ws := NewWebhookSender("", "", false)
	if ws != nil {
		t.Errorf("NewWebhookSender(\"\") = %v, want nil", ws)
	}

	// Test valid URL
	url := "https://hooks.slack.com/test"
	ws = NewWebhookSender(url, "", false)
	if ws == nil {
		t.Fatal("NewWebhookSender returned nil for valid URL")
	}
	if ws.url != url {
		t.Errorf("WebhookSender.url = %q, want %q", ws.url, url)
	}
	if ws.client == nil {
		t.Error("WebhookSender.client is nil")
	}
}

// A URL with "slack" in its path is not a Slack endpoint; matching anywhere in
// the URL meant unrelated endpoints silently received Slack-shaped payloads.
func TestIsSlackURLMatchesHostOnly(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		expected bool
	}{
		{"slack in the path", "https://example.com/slack-notify", false},
		{"slack in the query", "https://example.com/hook?channel=slack", false},
		{"slack in the host", "https://hooks.slack.com/services/AAA", true},
		{"slack proxy host", "http://internal-slack-proxy.local/alert", true},
		{"not a url", "://nonsense", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSlackURL(tt.url); got != tt.expected {
				t.Errorf("isSlackURL(%q) = %v, want %v", tt.url, got, tt.expected)
			}
		})
	}
}

// Slack and PagerDuty put the shared secret in the URL path, so the full URL
// must never reach the logs.
func TestRedactedURLHidesTheSecretPath(t *testing.T) {
	const secret = "T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX"
	ws := NewWebhookSender("https://hooks.slack.com/services/"+secret, "", false)

	got := ws.redactedURL()
	if strings.Contains(got, secret) || strings.Contains(got, "XXXXXXXX") {
		t.Errorf("redactedURL() leaked the credential: %q", got)
	}
	if !strings.Contains(got, "hooks.slack.com") {
		t.Errorf("redactedURL() = %q, want the host retained for diagnosis", got)
	}
}

func TestSendOmitsLogsUnlessEnabled(t *testing.T) {
	report := &v1alpha1.PodCrashReport{
		Spec: v1alpha1.PodCrashReportSpec{PodName: "p", Namespace: "n", ContainerName: "c"},
		Status: v1alpha1.PodCrashReportStatus{
			Diagnostics: v1alpha1.DiagnosticData{
				LastLogLines: []string{"password=hunter2"},
			},
		},
	}

	for _, tt := range []struct {
		name        string
		includeLogs bool
		wantLeak    bool
	}{
		{"logs withheld by default", false, false},
		{"logs sent when explicitly enabled", true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var body []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ = io.ReadAll(r.Body)
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			ws := NewWebhookSender(srv.URL, "", tt.includeLogs)
			if err := ws.Send(context.Background(), report); err != nil {
				t.Fatalf("Send returned error: %v", err)
			}

			leaked := strings.Contains(string(body), "hunter2")
			if leaked != tt.wantLeak {
				t.Errorf("payload contains log content = %v, want %v (payload: %s)", leaked, tt.wantLeak, body)
			}
		})
	}
}

func TestSendSetsAuthorizationHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ws := NewWebhookSender(srv.URL, "Bearer s3cret", false)
	if err := ws.Send(context.Background(), &v1alpha1.PodCrashReport{}); err != nil {
		t.Fatalf("Send returned error: %v", err)
	}

	if gotAuth != "Bearer s3cret" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer s3cret")
	}
}
