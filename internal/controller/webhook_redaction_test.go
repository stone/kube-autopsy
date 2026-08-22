package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kube-autopsy/kube-autopsy/api/v1alpha1"
)

// captureSlackText runs one Send against a recording server and returns the
// decoded Slack message. Decoding matters: the assertions are about what Slack
// renders, and JSON transport escapes "&" to \u0026 on the wire.
func captureSlackText(t *testing.T, report *v1alpha1.PodCrashReport, includeLogs bool) string {
	t.Helper()

	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		raw = b
	}))
	defer srv.Close()

	ws := NewWebhookSender(srv.URL, "", includeLogs)
	// isSlackURL keys on the hostname, which httptest cannot supply, so the
	// Slack branch is selected directly.
	ws.forceSlackFormat = true

	if err := ws.Send(context.Background(), report); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	var payload SlackPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decoding Slack payload %q: %v", raw, err)
	}
	return payload.Text
}

// webhookSecret stands in for the credential a Slack or PagerDuty URL carries
// in its path. Every assertion below is that this string never escapes.
const webhookSecret = "T012B034aBcDeFgHiJkLmNoP"

// The URL is a credential and docs/security.md promises it is never logged
// beyond its host. net/http and net/url both fail with a *url.Error, whose
// Error() embeds the full request URL — so returning that error unchanged put
// the credential into the controller log, into the reconciler's log line, and
// into controller-runtime's, on every delivery failure for the whole retry
// window.
func TestSendNeverLeaksURLInError(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{
			// Fails in http.NewRequestWithContext. A Secret made with
			// --from-file, or from $(cat url.txt), keeps its trailing newline,
			// so this fails on every report rather than once.
			name: "unparseable URL",
			url:  "https://hooks.slack.com/services/" + webhookSecret + "\n",
		},
		{
			// Fails in client.Do. Port 1 is closed, so this is a transport error
			// without needing a network.
			name: "transport failure",
			url:  "http://127.0.0.1:1/services/" + webhookSecret,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws := NewWebhookSender(tt.url, "", false)

			err := ws.Send(context.Background(), &v1alpha1.PodCrashReport{})
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), webhookSecret) {
				t.Errorf("error leaks the webhook credential: %v", err)
			}
			if strings.Contains(ws.redactedURL(), webhookSecret) {
				t.Errorf("redactedURL leaks the webhook credential: %s", ws.redactedURL())
			}
		})
	}
}

// scrubURLError has to survive an error that carries the URL without being a
// *url.Error, since a future transport could format its own message.
func TestScrubURLErrorHandlesNonURLErrors(t *testing.T) {
	rawURL := "https://hooks.slack.com/services/" + webhookSecret
	ws := NewWebhookSender(rawURL, "", false)

	scrubbed := ws.scrubURLError(errors.New("dial " + rawURL + ": refused"))
	if strings.Contains(scrubbed.Error(), webhookSecret) {
		t.Errorf("credential survived scrubbing: %v", scrubbed)
	}

	if ws.scrubURLError(nil) != nil {
		t.Error("scrubURLError(nil) should be nil")
	}

	// An unrelated error must pass through untouched, chain intact.
	sentinel := errors.New("something else entirely")
	if got := ws.scrubURLError(sentinel); !errors.Is(got, sentinel) {
		t.Errorf("unrelated error was not passed through: %v", got)
	}
}

// --webhook-include-logs populated the JSON payload but not the Slack message,
// so for the endpoint type most people point this at, the flag did nothing.
func TestSlackPayloadCarriesLogLinesWhenEnabled(t *testing.T) {
	report := &v1alpha1.PodCrashReport{}
	report.Status.Diagnostics.LastLogLines = []string{"heap exhausted", "aborting"}

	for _, tt := range []struct {
		name        string
		includeLogs bool
		wantLogs    bool
	}{
		{name: "enabled", includeLogs: true, wantLogs: true},
		{name: "disabled", includeLogs: false, wantLogs: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			text := captureSlackText(t, report, tt.includeLogs)
			if got := strings.Contains(text, "heap exhausted"); got != tt.wantLogs {
				t.Errorf("Slack message contains log lines = %v, want %v: %s", got, tt.wantLogs, text)
			}
		})
	}
}

// comm comes from the kernel and any workload can set it via prctl(PR_SET_NAME).
// Unescaped, "<!channel>" — 10 characters against a 15-byte budget — makes every
// OOM alert page the whole Slack channel.
func TestSlackPayloadEscapesProcessNames(t *testing.T) {
	report := &v1alpha1.PodCrashReport{}
	report.Status.Diagnostics.TriggerComm = "<!channel>"
	report.Status.Diagnostics.OOMVictimComm = "a&b<c>"

	text := captureSlackText(t, report, false)

	if strings.Contains(text, "<!channel>") {
		t.Errorf("Slack message contains an unescaped broadcast directive: %s", text)
	}
	for _, want := range []string{"&lt;!channel&gt;", "a&amp;b&lt;c&gt;"} {
		if !strings.Contains(text, want) {
			t.Errorf("Slack message missing escaped %q: %s", want, text)
		}
	}
}

// Escaping expands (& becomes &amp;), so a log tail full of ampersands used to
// grow back past the limit after being trimmed — and Slack rejects an over-long
// message outright, turning a captured tail into no notification at all.
func TestSlackMessageStaysWithinTheLimit(t *testing.T) {
	tests := []struct {
		name string
		line string
	}{
		{name: "plain", line: strings.Repeat("x", 200)},
		// Every byte expands fivefold once escaped.
		{name: "all ampersands", line: strings.Repeat("&", 200)},
		{name: "mixed markup", line: strings.Repeat("<a&b>", 40)},
		// Multi-byte runes, so a naive cut would split one.
		{name: "multi-byte", line: strings.Repeat("€", 60)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := make([]string, 0, 500)
			for i := 0; i < 500; i++ {
				lines = append(lines, tt.line)
			}

			report := &v1alpha1.PodCrashReport{}
			report.Status.Diagnostics.LastLogLines = lines

			text := captureSlackText(t, report, true)

			if len(text) > slackMaxTextBytes {
				t.Errorf("Slack message is %d bytes, want <= %d", len(text), slackMaxTextBytes)
			}
			if !utf8.ValidString(text) {
				t.Error("truncation produced invalid UTF-8")
			}
			if !strings.Contains(text, slackTruncationMarker) {
				t.Error("expected a truncation marker on a message that was cut")
			}
			// The tail is what matters, so the newest line must survive whole.
			if !strings.Contains(text, escapeSlackText(tt.line)) {
				t.Error("the most recent log line did not survive intact")
			}
		})
	}
}

func TestSlackLogBlockBudget(t *testing.T) {
	body := escapeSlackText(strings.Repeat("a\n", 5000))

	tests := []struct {
		name   string
		budget int
	}{
		{name: "ample", budget: len(body) + 100},
		{name: "exact", budget: len(body)},
		{name: "tight", budget: 500},
		{name: "only the marker fits", budget: len(slackTruncationMarker)},
		{name: "nothing fits", budget: 0},
		{name: "negative", budget: -50},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := slackLogBlock(body, tt.budget)
			if tt.budget > 0 && len(got) > tt.budget {
				t.Errorf("block is %d bytes, want <= budget %d", len(got), tt.budget)
			}
			if !utf8.ValidString(got) {
				t.Error("block is not valid UTF-8")
			}
		})
	}
}
