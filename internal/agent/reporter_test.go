package agent

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	autopsy "github.com/kube-autopsy/kube-autopsy/api/v1alpha1"
)

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "lowercase only",
			input:    "Hogger",
			expected: "hogger",
		},
		{
			name:     "replace invalid characters",
			input:    "oom@victim!test",
			expected: "oom-victim-test",
		},
		{
			name:     "trim leading and trailing dashes",
			input:    "-oom-victim-",
			expected: "oom-victim",
		},
		{
			name:     "trim leading and trailing dots",
			input:    ".oom-victim.",
			expected: "oom-victim",
		},
		{
			name:     "valid complex name",
			input:    "My-Super_App.v1",
			expected: "my-super-app.v1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeName(tt.input)
			if result != tt.expected {
				t.Errorf("sanitizeName(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestFindContainerByID(t *testing.T) {
	pod := corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", ContainerID: "containerd://12345"},
			},
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "init-app", ContainerID: "containerd://67890"},
			},
			EphemeralContainerStatuses: []corev1.ContainerStatus{
				{Name: "debug-app", ContainerID: "containerd://abcde"},
			},
		},
	}

	tests := []struct {
		name         string
		containerID  string
		expectedName string
	}{
		{
			name:         "match container",
			containerID:  "12345",
			expectedName: "app",
		},
		{
			name:         "match init container",
			containerID:  "67890",
			expectedName: "init-app",
		},
		{
			name:         "match ephemeral container",
			containerID:  "abcde",
			expectedName: "debug-app",
		},
		{
			name:         "no match",
			containerID:  "99999",
			expectedName: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, _ := findContainerByID(pod, tt.containerID)
			if result != tt.expectedName {
				t.Errorf("findContainerByID(%q) = %q, want %q", tt.containerID, result, tt.expectedName)
			}
		})
	}
}

func TestReportNamePrefix(t *testing.T) {
	tests := []struct {
		name          string
		podName       string
		containerName string
		expected      string
	}{
		{
			name:          "typical pod and container",
			podName:       "oom-victim",
			containerName: "hogger",
			expected:      "oom-victim-hogger-",
		},
		{
			name:          "invalid characters are sanitized",
			podName:       "My_Pod",
			containerName: "App!",
			expected:      "my-pod-app-",
		},
		{
			name:          "empty input falls back to a valid prefix",
			podName:       "",
			containerName: "",
			expected:      "podcrashreport-",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reportNamePrefix(tt.podName, tt.containerName); got != tt.expected {
				t.Errorf("reportNamePrefix(%q, %q) = %q, want %q",
					tt.podName, tt.containerName, got, tt.expected)
			}
		})
	}
}

// A generated name is the prefix plus a 5-character suffix and must remain a
// valid DNS-1123 label, so the prefix has to leave room and must not end in a
// character that is illegal mid-name after truncation.
func TestReportNamePrefixStaysWithinGeneratedNameBudget(t *testing.T) {
	longPod := strings.Repeat("a", 200)
	longContainer := strings.Repeat("b", 200)

	prefix := reportNamePrefix(longPod, longContainer)

	if len(prefix) > generatedNameBaseLimit {
		t.Errorf("prefix length = %d, want <= %d", len(prefix), generatedNameBaseLimit)
	}
	if !strings.HasSuffix(prefix, "-") {
		t.Errorf("prefix %q does not end with a separator", prefix)
	}
	if strings.HasSuffix(strings.TrimSuffix(prefix, "-"), "-") {
		t.Errorf("prefix %q ends with a doubled separator", prefix)
	}
}

func TestSetOwnerReference(t *testing.T) {
	t.Run("sets a non-controller owner reference to the pod", func(t *testing.T) {
		report := &autopsy.PodCrashReport{}
		setOwnerReference(report, PodMeta{
			PodName:   "oom-victim",
			PodUID:    "1234-5678",
			Namespace: "default",
		})

		if len(report.OwnerReferences) != 1 {
			t.Fatalf("expected 1 owner reference, got %d", len(report.OwnerReferences))
		}
		ref := report.OwnerReferences[0]
		if ref.Kind != "Pod" || ref.Name != "oom-victim" || string(ref.UID) != "1234-5678" {
			t.Errorf("unexpected owner reference: %+v", ref)
		}
		if ref.Controller == nil || *ref.Controller {
			t.Error("expected Controller to be false")
		}
	})

	t.Run("skips when the pod could not be identified", func(t *testing.T) {
		report := &autopsy.PodCrashReport{}
		setOwnerReference(report, PodMeta{PodName: "oom-victim"})

		if len(report.OwnerReferences) != 0 {
			t.Errorf("expected no owner reference without a UID, got %+v", report.OwnerReferences)
		}
	})
}

func TestLabelValue(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"ordinary name", "oom-victim", "oom-victim"},
		{"invalid characters replaced", "my/pod:1", "my-pod-1"},
		{"leading and trailing separators trimmed", "-name.", "name"},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := labelValue(tt.input); got != tt.expected {
				t.Errorf("labelValue(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

// An over-long or malformed label makes the API server reject the whole report,
// losing the diagnostic entirely.
func TestLabelValueIsAlwaysAcceptable(t *testing.T) {
	inputs := []string{
		strings.Repeat("a", 300),
		strings.Repeat("-", 80) + "x",
		"pod." + strings.Repeat("b", 100),
	}

	for _, in := range inputs {
		got := labelValue(in)
		if len(got) > maxLabelValueLength {
			t.Errorf("labelValue(%.20q...) length = %d, want <= %d", in, len(got), maxLabelValueLength)
		}
		if got == "" {
			continue
		}
		first, last := got[0], got[len(got)-1]
		for _, c := range []byte{first, last} {
			isAlnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
			if !isAlnum {
				t.Errorf("labelValue(%.20q...) = %q must start and end alphanumeric", in, got)
			}
		}
	}
}

// A substring match could attribute a crash to the wrong container whenever one
// container ID happens to contain another.
func TestContainerIDsMatch(t *testing.T) {
	tests := []struct {
		name     string
		statusID string
		cgroupID string
		expected bool
	}{
		{"exact match after stripping the scheme", "containerd://abc123", "abc123", true},
		{"cri-o scheme", "cri-o://abc123", "abc123", true},
		{"different container", "containerd://abc123", "abc999", false},
		{"cgroup id is only a prefix", "containerd://abc123456", "abc123", false},
		{"cgroup id is only a suffix", "containerd://xxabc123", "abc123", false},
		{"scheme must not be matched", "containerd://abc123", "containerd", false},
		{"empty status id", "", "abc123", false},
		{"empty cgroup id", "containerd://abc123", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containerIDsMatch(tt.statusID, tt.cgroupID); got != tt.expected {
				t.Errorf("containerIDsMatch(%q, %q) = %v, want %v",
					tt.statusID, tt.cgroupID, got, tt.expected)
			}
		})
	}
}
