package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kube-autopsy/kube-autopsy/api/v1alpha1"
)

// WebhookPayload is the JSON payload sent to webhook endpoints when a
// PodCrashReport is processed.
type WebhookPayload struct {
	PodName       string   `json:"podName"`
	Namespace     string   `json:"namespace"`
	ContainerName string   `json:"containerName"`
	NodeName      string   `json:"nodeName"`
	Reason        string   `json:"reason"`
	ExitCode      int32    `json:"exitCode"`
	Timestamp     string   `json:"timestamp"`
	PeakMemoryMB  int64    `json:"peakMemoryMB"`
	LastLogLines  []string `json:"lastLogLines,omitempty"`
}

// SlackPayload wraps a text message for Slack-compatible webhook endpoints.
type SlackPayload struct {
	Text string `json:"text"`
}

// WebhookSender sends crash report summaries to a configured webhook URL.
type WebhookSender struct {
	url    string
	client *http.Client
}

// NewWebhookSender creates a new WebhookSender for the given URL. If url is
// empty, nil is returned (webhook sending is disabled).
func NewWebhookSender(url string) *WebhookSender {
	if url == "" {
		return nil
	}
	return &WebhookSender{
		url: url,
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
		PodName:       report.Spec.PodName,
		Namespace:     report.Spec.Namespace,
		ContainerName: report.Spec.ContainerName,
		NodeName:      report.Spec.NodeName,
		Reason:        report.Spec.Termination,
		ExitCode:      report.Spec.ExitCode,
		Timestamp:     report.Spec.Timestamp,
		PeakMemoryMB:  report.Status.Diagnostics.PeakMemoryBytes / (1024 * 1024),
		LastLogLines:  report.Status.Diagnostics.LastLogLines,
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
				"*Peak Memory:* %d MB\n"+
				"*Time:* %s",
			payload.Namespace, payload.PodName, payload.ContainerName,
			payload.NodeName,
			payload.Reason, payload.ExitCode,
			report.Status.Diagnostics.OOMContext,
			report.Status.Diagnostics.TriggerComm, report.Status.Diagnostics.TriggerPID,
			report.Status.Diagnostics.OOMVictimComm, report.Status.Diagnostics.OOMVictimPID,
			payload.PeakMemoryMB,
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

	resp, err := ws.client.Do(req)
	if err != nil {
		logger.Error(err, "Failed to send webhook", "url", ws.url)
		return err
	}
	defer resp.Body.Close()
	// Drain the body to allow connection reuse.
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logger.Info("Webhook returned non-2xx status", "status", resp.StatusCode, "url", ws.url)
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}

	logger.V(1).Info("Webhook sent successfully", "url", ws.url, "report", report.Name)
	return nil
}

// isSlackURL returns true if the URL appears to be a Slack webhook endpoint.
func isSlackURL(url string) bool {
	return strings.Contains(url, "hooks.slack.com") || strings.Contains(url, "slack")
}
