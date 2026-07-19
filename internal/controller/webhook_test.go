package controller

import (
	"testing"
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
	ws := NewWebhookSender("")
	if ws != nil {
		t.Errorf("NewWebhookSender(\"\") = %v, want nil", ws)
	}

	// Test valid URL
	url := "https://hooks.slack.com/test"
	ws = NewWebhookSender(url)
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
