package agent

import "testing"

func TestContainerIDFromCgroup(t *testing.T) {
	const id = "8d2f1a3c4b5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8"

	tests := []struct {
		name     string
		cgroup   string
		expected string
	}{
		{
			name:     "containerd systemd driver",
			cgroup:   "cri-containerd-" + id + ".scope",
			expected: id,
		},
		{
			name:     "cri-o systemd driver",
			cgroup:   "crio-" + id + ".scope",
			expected: id,
		},
		{
			name:     "docker systemd driver",
			cgroup:   "docker-" + id + ".scope",
			expected: id,
		},
		{
			name:     "cgroupfs driver uses the bare container ID",
			cgroup:   id,
			expected: id,
		},
		{
			name:     "unrecognised name is passed through",
			cgroup:   "system.slice",
			expected: "system.slice",
		},
		{
			name:     "empty",
			cgroup:   "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containerIDFromCgroup(tt.cgroup); got != tt.expected {
				t.Errorf("containerIDFromCgroup(%q) = %q, want %q", tt.cgroup, got, tt.expected)
			}
		})
	}
}
