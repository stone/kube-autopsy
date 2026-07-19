package agent

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
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
		name          string
		containerID   string
		expectedName  string
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
			result := findContainerByID(pod, tt.containerID)
			if result != tt.expectedName {
				t.Errorf("findContainerByID(%q) = %q, want %q", tt.containerID, result, tt.expectedName)
			}
		})
	}
}
