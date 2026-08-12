package controller

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// A process name comes from the kernel's comm field, which a workload controls
// and can change at will. Left unbounded it becomes an unbounded set of
// Prometheus time series.
func TestLabelLimiterBoundsCardinality(t *testing.T) {
	limiter := newLabelLimiter()

	for i := range maxLabelCardinality {
		value := fmt.Sprintf("proc-%d", i)
		if got := limiter.bound(value); got != value {
			t.Fatalf("value %d within the limit was collapsed to %q", i, got)
		}
	}

	// Anything new beyond the limit collapses.
	if got := limiter.bound("one-too-many"); got != overflowLabel {
		t.Errorf("bound(%q) = %q, want %q", "one-too-many", got, overflowLabel)
	}

	// Values already admitted keep reporting under their own name.
	if got := limiter.bound("proc-0"); got != "proc-0" {
		t.Errorf("previously admitted value was collapsed to %q", got)
	}
}

func TestSanitizeLabelValue(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "ordinary process name",
			input:    "java",
			expected: "java",
		},
		{
			name:     "empty becomes unknown",
			input:    "",
			expected: unknownLabel,
		},
		{
			name:     "control characters are replaced",
			input:    "ja\x00va\x1b[31m",
			expected: "ja_va_[31m",
		},
		{
			name:     "newlines cannot break the exposition format",
			input:    "evil\nkube_autopsy_fake_metric 1",
			expected: "evil_kube_autopsy_fake_metric 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeLabelValue(tt.input); got != tt.expected {
				t.Errorf("sanitizeLabelValue(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

// comm is raw kernel bytes with no UTF-8 guarantee, and invalid UTF-8 in a
// label value corrupts the whole /metrics response rather than one series.
func TestSanitizeLabelValueAlwaysReturnsValidUTF8(t *testing.T) {
	inputs := []string{
		"\xff\xfe\xfd",
		"ok\xc3\x28bad",
		strings.Repeat("\xff", 40),
	}

	for _, in := range inputs {
		got := sanitizeLabelValue(in)
		if !utf8.ValidString(got) {
			t.Errorf("sanitizeLabelValue(%q) returned invalid UTF-8: %q", in, got)
		}
	}
}

func TestSanitizeLabelValueCapsLength(t *testing.T) {
	got := sanitizeLabelValue(strings.Repeat("a", maxLabelValueLength*3))
	if len(got) > maxLabelValueLength {
		t.Errorf("value length = %d, want <= %d", len(got), maxLabelValueLength)
	}
}
