package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

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
	// forceSlackFormat selects the Slack message shape regardless of the URL's
	// host. isSlackURL keys on the hostname, which a test server cannot supply,
	// so this exists to make that branch reachable in tests.
	forceSlackFormat bool
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
	start := time.Now()
	defer func() { WebhookDurationSeconds.Observe(time.Since(start).Seconds()) }()

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

	if ws.forceSlackFormat || isSlackURL(ws.url) {
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
			// Process names come from the kernel's comm, which any workload sets
			// to anything it likes via prctl(PR_SET_NAME). Every other field here
			// is a Kubernetes name and cannot contain the characters Slack's
			// mrkdwn treats as markup; comm can, and "<!channel>" fits inside the
			// 15 usable bytes, so an unescaped one would let a tenant page a whole
			// channel on every OOM.
			escapeSlackText(report.Status.Diagnostics.TriggerComm), report.Status.Diagnostics.TriggerPID,
			escapeSlackText(report.Status.Diagnostics.OOMVictimComm), report.Status.Diagnostics.OOMVictimPID,
			payload.VictimRSSMB, payload.OOMScopeLimitMB,
			payload.Timestamp,
		)
		// The JSON payload carries lastLogLines when enabled, so the Slack
		// message has to as well — otherwise --webhook-include-logs is a silent
		// no-op for the one endpoint type most people point this at.
		//
		// Escaped before it is measured: escaping expands (& becomes &amp;), so
		// trimming first and escaping afterwards would let a log tail full of
		// ampersands grow back past the limit and have Slack reject the whole
		// message. The budget is what is left of the limit after the fields
		// above, and the fence itself.
		if len(payload.LastLogLines) > 0 {
			const fence = "\n*Last log lines:*\n```\n" + "\n```"
			budget := slackMaxTextBytes - len(text) - len(fence)
			if block := slackLogBlock(escapeSlackText(strings.Join(payload.LastLogLines, "\n")), budget); block != "" {
				text += "\n*Last log lines:*\n```\n" + block + "\n```"
			}
		}
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
		// A URL that will not parse — a Secret created from a file keeps its
		// trailing newline, which is the usual cause — fails here on every
		// single report, so an unscrubbed error would repeat the credential in
		// the log indefinitely.
		err = ws.scrubURLError(err)
		logger.Error(err, "Failed to create webhook request", "url", ws.redactedURL())
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if ws.authHeader != "" {
		req.Header.Set("Authorization", ws.authHeader)
	}

	resp, err := ws.client.Do(req)
	if err != nil {
		err = ws.scrubURLError(err)
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

// slackMaxTextBytes keeps a message inside Slack's 40,000-character limit for
// the text field, with room for the surrounding template.
const slackMaxTextBytes = 30000

// slackTruncationMarker introduces a log block whose start was cut off.
const slackTruncationMarker = "…[earlier lines truncated]\n"

// escapeSlackText escapes the three characters Slack's mrkdwn parser treats as
// markup, per Slack's own formatting guidance.
func escapeSlackText(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// slackLogBlock bounds an already-escaped log body to budget bytes, keeping the
// end — the lines closest to the kill are the interesting ones. Slack rejects an
// over-long message outright, which would turn a captured log tail into no
// notification at all.
//
// The input must already be escaped: escaping expands, so measuring before it
// would not bound the result. Cuts land on a rune boundary, and then on the next
// line break where one is close by, so the block never opens mid-character or
// mid-line.
func slackLogBlock(escaped string, budget int) string {
	if budget <= len(slackTruncationMarker) {
		return ""
	}
	if len(escaped) <= budget {
		return escaped
	}

	keep := budget - len(slackTruncationMarker)
	cut := len(escaped) - keep
	// Escaping never produces a partial rune, but the cut offset can still land
	// inside one, so walk forward to the next boundary.
	for cut < len(escaped) && !utf8.RuneStart(escaped[cut]) {
		cut++
	}
	trimmed := escaped[cut:]
	if i := strings.IndexByte(trimmed, '\n'); i >= 0 && i < len(trimmed)-1 {
		trimmed = trimmed[i+1:]
	}
	return slackTruncationMarker + trimmed
}

// IncludesLogs reports whether this sender forwards captured log lines, so the
// caller knows whether it must supply a report that still has them.
func (ws *WebhookSender) IncludesLogs() bool {
	return ws != nil && ws.includeLogs
}

// scrubURLError strips the webhook URL out of an error before it is logged or
// returned. net/http and net/url both fail with a *url.Error, whose Error()
// embeds the full request URL — including the path that Slack and PagerDuty
// endpoints carry their credential in. Logging that error, or handing it to the
// reconciler (which logs it again, as does controller-runtime), would defeat
// redactedURL entirely and write the credential to stdout on every failure.
//
// url.Error.Err is the underlying cause and carries no URL, so it can be
// re-wrapped. Any other error shape is scrubbed by substitution instead, so a
// transport that formats its own message cannot reintroduce the leak; that path
// necessarily drops the error chain, which is the right trade when the
// alternative is publishing the credential.
func (ws *WebhookSender) scrubURLError(err error) error {
	if err == nil {
		return nil
	}

	var uerr *url.Error
	if errors.As(err, &uerr) {
		return fmt.Errorf("%s %s: %w", uerr.Op, ws.redactedURL(), uerr.Err)
	}

	if ws.url != "" && strings.Contains(err.Error(), ws.url) {
		return errors.New(strings.ReplaceAll(err.Error(), ws.url, ws.redactedURL()))
	}

	return err
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
