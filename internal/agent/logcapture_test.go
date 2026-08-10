package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func writeTempLog(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "0.log")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write temp log: %v", err)
	}
	return path
}

func TestTailFile(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		lines    int
		expected []string
	}{
		{
			name:     "fewer lines available than requested",
			content:  "one\ntwo\n",
			lines:    10,
			expected: []string{"one", "two"},
		},
		{
			name:     "returns only the last N lines",
			content:  "one\ntwo\nthree\nfour\n",
			lines:    2,
			expected: []string{"three", "four"},
		},
		{
			name:     "file without a trailing newline",
			content:  "one\ntwo",
			lines:    2,
			expected: []string{"one", "two"},
		},
		{
			name:     "empty file",
			content:  "",
			lines:    5,
			expected: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tailFile(writeTempLog(t, tt.content), tt.lines)
			if err != nil {
				t.Fatalf("tailFile returned error: %v", err)
			}
			if len(got) != len(tt.expected) {
				t.Fatalf("tailFile returned %d lines (%q), want %d (%q)",
					len(got), got, len(tt.expected), tt.expected)
			}
			for i := range got {
				if got[i] != tt.expected[i] {
					t.Errorf("line %d = %q, want %q", i, got[i], tt.expected[i])
				}
			}
		})
	}
}

// A non-positive line count used to index past the end of the slice and panic,
// taking down the agent on every node.
func TestTailFileRejectsNonPositiveLineCount(t *testing.T) {
	path := writeTempLog(t, "one\ntwo\n")

	for _, lines := range []int{0, -1, -50} {
		if _, err := tailFile(path, lines); err == nil {
			t.Errorf("tailFile(path, %d) = nil error, want an error", lines)
		}
	}
}

// The chunked reverse read must stitch chunk boundaries back together.
func TestTailFileSpansMultipleChunks(t *testing.T) {
	var b strings.Builder
	const total = 5000
	for i := range total {
		b.WriteString(strings.Repeat("x", 20))
		b.WriteString("\n")
		_ = i
	}

	got, err := tailFile(writeTempLog(t, b.String()), 3)
	if err != nil {
		t.Fatalf("tailFile returned error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(got))
	}
	for i, line := range got {
		if line != strings.Repeat("x", 20) {
			t.Errorf("line %d = %q, want 20 x's", i, line)
		}
	}
}

func TestExtractLogNumber(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected int
	}{
		{name: "zero", input: "0.log", expected: 0},
		{name: "rotated", input: "3.log", expected: 3},
		{name: "unparseable", input: "current.log", expected: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractLogNumber(tt.input); got != tt.expected {
				t.Errorf("extractLogNumber(%q) = %d, want %d", tt.input, got, tt.expected)
			}
		})
	}
}

// A report whose status exceeds etcd's per-object limit cannot be written at
// all, which would lose every diagnostic, not just the logs.
func TestTruncateLines(t *testing.T) {
	t.Run("short lines pass through unchanged", func(t *testing.T) {
		in := []string{"one", "two", "three"}
		got := truncateLines(in)
		if len(got) != 3 || got[0] != "one" || got[2] != "three" {
			t.Errorf("truncateLines(%q) = %q, want unchanged", in, got)
		}
	})

	t.Run("an over-long line is cut and marked", func(t *testing.T) {
		got := truncateLines([]string{strings.Repeat("x", maxLogLineBytes*2)})
		if len(got) != 1 {
			t.Fatalf("expected 1 line, got %d", len(got))
		}
		if !strings.HasSuffix(got[0], truncationMarker) {
			t.Errorf("truncated line is not marked: %q", got[0][:40])
		}
		if len(got[0]) > maxLogLineBytes+len(truncationMarker) {
			t.Errorf("line length = %d, want <= %d", len(got[0]), maxLogLineBytes+len(truncationMarker))
		}
	})

	t.Run("aggregate size is bounded", func(t *testing.T) {
		var in []string
		for range 500 {
			in = append(in, strings.Repeat("y", maxLogLineBytes))
		}

		got := truncateLines(in)

		total := 0
		for _, line := range got {
			total += len(line)
		}
		if total > maxLogTotalBytes+len(truncationMarker) {
			t.Errorf("total captured bytes = %d, want <= %d", total, maxLogTotalBytes)
		}
		if got[len(got)-1] != truncationMarker {
			t.Errorf("expected a truncation marker as the final entry, got %q", got[len(got)-1])
		}
	})

	t.Run("multi-byte runes are never split", func(t *testing.T) {
		// "€" is 3 bytes, so the cut lands mid-rune without care.
		got := truncateLines([]string{strings.Repeat("€", maxLogLineBytes)})
		body := strings.TrimSuffix(got[0], truncationMarker)
		if !utf8.ValidString(body) {
			t.Error("truncation produced invalid UTF-8, which the API server rejects")
		}
	})
}

// Kubelet writes CRI-framed lines; storing them raw meant every captured line
// carried a timestamp, stream name and flag, contradicting the documented
// output, and long lines appeared as fragments.
func TestParseLogLines(t *testing.T) {
	tests := []struct {
		name     string
		raw      []string
		expected []string
	}{
		{
			name: "CRI framing is stripped",
			raw: []string{
				"2026-07-18T13:24:28.123456789Z stdout F Allocated block 41",
				"2026-07-18T13:24:28.223456789Z stderr F out of memory",
			},
			expected: []string{"Allocated block 41", "out of memory"},
		},
		{
			name: "partial lines are reassembled",
			raw: []string{
				"2026-07-18T13:24:28.123456789Z stdout P this is a very ",
				"2026-07-18T13:24:28.123456790Z stdout P long line that was ",
				"2026-07-18T13:24:28.123456791Z stdout F split by the runtime",
			},
			expected: []string{"this is a very long line that was split by the runtime"},
		},
		{
			name: "a trailing partial is still emitted",
			raw: []string{
				"2026-07-18T13:24:28.123456789Z stdout P killed mid-sentence",
			},
			expected: []string{"killed mid-sentence"},
		},
		{
			name: "messages containing spaces survive intact",
			raw:  []string{"2026-07-18T13:24:28.123456789Z stdout F a b c  d"},
			expected: []string{
				"a b c  d",
			},
		},
		{
			name:     "empty messages are preserved",
			raw:      []string{"2026-07-18T13:24:28.123456789Z stdout F"},
			expected: []string{""},
		},
		{
			name:     "docker json-file format",
			raw:      []string{`{"log":"hello world\n","stream":"stdout","time":"2026-07-18T13:24:28.1Z"}`},
			expected: []string{"hello world"},
		},
		{
			name:     "unrecognised lines pass through unchanged",
			raw:      []string{"just a plain log line"},
			expected: []string{"just a plain log line"},
		},
		{
			name:     "a plain line shaped like CRI is not misparsed",
			raw:      []string{"not-a-timestamp stdout F something"},
			expected: []string{"not-a-timestamp stdout F something"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseLogLines(tt.raw)
			if len(got) != len(tt.expected) {
				t.Fatalf("parseLogLines(%q) = %q, want %q", tt.raw, got, tt.expected)
			}
			for i := range got {
				if got[i] != tt.expected[i] {
					t.Errorf("line %d = %q, want %q", i, got[i], tt.expected[i])
				}
			}
		})
	}
}
