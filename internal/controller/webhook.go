package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kube-autopsy/kube-autopsy/api/v1alpha1"
)

// WebhookPayload is the JSON payload sent to webhook endpoints when a
// PodCrashReport is processed.
type WebhookPayload struct {
	PodName       string `json:"podName"`
	Namespace     string `json:"namespace"`
	ContainerName string `json:"containerName"`
	NodeName      string `json:"nodeName"`
	Reason        string `json:"reason"`
	ExitCode      int32  `json:"exitCode"`
	Timestamp     string `json:"timestamp"`
	// VictimRSSMB is what the killed process was actually using. Zero when the
	// kernel's memory layout was not recognised.
	VictimRSSMB int64 `json:"victimRssMB"`
	// OOMScopeLimitMB is the container memory limit, or the node's RAM for a
	// node-level OOM.
	OOMScopeLimitMB int64    `json:"oomScopeLimitMB"`
	LastLogLines    []string `json:"lastLogLines,omitempty"`
}

// SlackPayload wraps a text message for Slack-compatible webhook endpoints.
type SlackPayload struct {
	Text string `json:"text"`
}

// WebhookSender sends crash report summaries to a configured webhook URL.
type WebhookSender struct {
	url         string
	authHeader  string
	includeLogs bool
	client      *http.Client
}

// NewWebhookSender creates a new WebhookSender for the given URL. If url is
// empty, nil is returned (webhook sending is disabled). authHeader, when set,
// is sent as the Authorization header. includeLogs controls whether captured
// log lines are allowed to leave the cluster.
func NewWebhookSender(url, authHeader string, includeLogs bool) *WebhookSender {
	if url == "" {
		return nil
	}
	return &WebhookSender{
		url:         url,
		authHeader:  authHeader,
		includeLogs: includeLogs,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Send posts a JSON summary of the given PodCrashReport to the configured
// webhook URL. For Slack-compatible endpoints (URLs containing "slack"),
// the payload is wrapped in a Slack-style message with a "text" field.
// Errors are logged but not returned to avoid failing reconciliation.
func (ws *WebhookSender) Send(ctx context.Context, report *v1alpha1.PodCrashReport) error {
	logger := log.FromContext(ctx)

	payload := WebhookPayload{
		PodName:         report.Spec.PodName,
		Namespace:       report.Spec.Namespace,
		ContainerName:   report.Spec.ContainerName,
		NodeName:        report.Spec.NodeName,
		Reason:          report.Spec.TerminationReason,
		ExitCode:        report.Spec.ExitCode,
		Timestamp:       report.Spec.Timestamp.UTC().Format(time.RFC3339),
		VictimRSSMB:     report.Status.Diagnostics.VictimRSSBytes / (1024 * 1024),
		OOMScopeLimitMB: report.Status.Diagnostics.OOMScopeLimitBytes / (1024 * 1024),
	}

	// Log content leaves the cluster only when explicitly allowed.
	if ws.includeLogs {
		payload.LastLogLines = report.Status.Diagnostics.LastLogLines
	}

	var body []byte
	var err error

	if isSlackURL(ws.url) {
		text := fmt.Sprintf(
			":rotating_light: *Pod Crash Detected*\n"+
				"*Pod:* %s/%s (container: %s)\n"+
				"*Node:* %s\n"+
				"*Reason:* %s (exit code: %d)\n"+
				"*OOM Context:* %s\n"+
				"*Trigger Process:* %s (PID: %d)\n"+
				"*Victim Process:* %s (PID: %d)\n"+
				"*Victim RSS:* %d MB (scope limit: %d MB)\n"+
				"*Time:* %s",
			payload.Namespace, payload.PodName, payload.ContainerName,
			payload.NodeName,
			payload.Reason, payload.ExitCode,
			report.Status.Diagnostics.OOMContext,
			report.Status.Diagnostics.TriggerComm, report.Status.Diagnostics.TriggerPID,
			report.Status.Diagnostics.OOMVictimComm, report.Status.Diagnostics.OOMVictimPID,
			payload.VictimRSSMB, payload.OOMScopeLimitMB,
			payload.Timestamp,
		)
		body, err = json.Marshal(SlackPayload{Text: text})
	} else {
		body, err = json.Marshal(payload)
	}
	if err != nil {
		logger.Error(err, "Failed to marshal webhook payload")
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ws.url, bytes.NewReader(body))
	if err != nil {
		logger.Error(err, "Failed to create webhook request")
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if ws.authHeader != "" {
		req.Header.Set("Authorization", ws.authHeader)
	}

	resp, err := ws.client.Do(req)
	if err != nil {
		logger.Error(err, "Failed to send webhook", "url", ws.redactedURL())
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain the body to allow connection reuse.
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logger.Info("Webhook returned non-2xx status", "status", resp.StatusCode, "url", ws.redactedURL())
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}

	logger.V(1).Info("Webhook sent successfully", "url", ws.redactedURL(), "report", report.Name)
	return nil
}

// redactedURL returns the webhook endpoint with its path and query removed.
// Slack and PagerDuty webhook URLs carry their secret in the path, so logging
// the full URL would write a credential into the controller's logs.
func (ws *WebhookSender) redactedURL() string {
	u, err := url.Parse(ws.url)
	if err != nil || u.Host == "" {
		return "[redacted]"
	}
	return u.Scheme + "://" + u.Host + "/[redacted]"
}

// isSlackURL reports whether the URL looks like a Slack-compatible endpoint,
// and so should receive the Slack message format. Only the host is considered:
// matching anywhere in the URL meant an unrelated endpoint with "slack" in its
// path or query string was silently sent Slack-shaped payloads.
func isSlackURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(u.Hostname()), "slack")
}
