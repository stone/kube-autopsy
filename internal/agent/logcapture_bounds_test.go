package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	autopsy "github.com/kube-autopsy/kube-autopsy/api/v1alpha1"
)

// stdout and stderr are interleaved in one log file. A single partial buffer let
// an stderr line land inside a split stdout line and spliced the two together —
// corrupting exactly the output this exists to capture, since a dying process
// usually writes its last words to stderr while the app logs to stdout.
func TestParseLogLinesTracksPartialsPerStream(t *testing.T) {
	tests := []struct {
		name     string
		raw      []string
		expected []string
	}{
		{
			name: "a complete stderr line does not join an open stdout partial",
			raw: []string{
				"2026-08-22T10:00:00.000000000Z stdout P part-of-stdout-line-",
				"2026-08-22T10:00:00.000000001Z stderr F WHOLE STDERR LINE",
				"2026-08-22T10:00:00.000000002Z stdout F continued",
			},
			expected: []string{
				"WHOLE STDERR LINE",
				"part-of-stdout-line-continued",
			},
		},
		{
			name: "both streams can be split at once",
			raw: []string{
				"2026-08-22T10:00:00.000000000Z stdout P out-a-",
				"2026-08-22T10:00:00.000000001Z stderr P err-a-",
				"2026-08-22T10:00:00.000000002Z stdout F out-b",
				"2026-08-22T10:00:00.000000003Z stderr F err-b",
			},
			expected: []string{"out-a-out-b", "err-a-err-b"},
		},
		{
			name: "an unterminated partial is still emitted",
			raw: []string{
				"2026-08-22T10:00:00.000000000Z stderr P killed mid-sentence",
			},
			expected: []string{"killed mid-sentence"},
		},
		{
			name: "an unframed line does not disturb an open partial",
			raw: []string{
				"2026-08-22T10:00:00.000000000Z stdout P first-",
				"a line with no framing at all",
				"2026-08-22T10:00:00.000000001Z stdout F second",
			},
			expected: []string{"a line with no framing at all", "first-second"},
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

// Accepting any non-empty stream meant a structured application log carrying its
// own "stream" field was claimed as docker framing and replaced by its absent
// "log" key — turning a fatal message into an empty line.
func TestParseDockerLogLineRequiresARealStream(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		expected string
	}{
		{
			name:     "genuine docker entry",
			line:     `{"log":"hello\n","stream":"stdout","time":"2026-08-22T10:00:00Z"}`,
			expected: "hello",
		},
		{
			name:     "application JSON with an unrelated stream field",
			line:     `{"level":"fatal","stream":"ingest-1","msg":"heap exhausted, aborting"}`,
			expected: `{"level":"fatal","stream":"ingest-1","msg":"heap exhausted, aborting"}`,
		},
		{
			name:     "docker-shaped but missing the log key",
			line:     `{"stream":"stdout","time":"2026-08-22T10:00:00Z"}`,
			expected: `{"stream":"stdout","time":"2026-08-22T10:00:00Z"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseLogLines([]string{tt.line})
			if len(got) != 1 || got[0] != tt.expected {
				t.Errorf("parseLogLines(%q) = %q, want [%q]", tt.line, got, tt.expected)
			}
		})
	}
}

// The CRD rejects a longer list outright. Because every diagnostic is written in
// one status patch, that rejection costs the memory figures and OOM scores too —
// so the agent must never build one, whatever it was configured with.
func TestTruncateLinesCapsItemCount(t *testing.T) {
	lines := make([]string, 0, autopsy.MaxLogLines*3)
	for i := 0; i < autopsy.MaxLogLines*3; i++ {
		lines = append(lines, "line")
	}

	got := truncateLines(lines)

	if len(got) > autopsy.MaxLogLines {
		t.Errorf("truncateLines returned %d entries, want <= %d", len(got), autopsy.MaxLogLines)
	}
	if got[0] != truncationMarker {
		t.Errorf("expected a truncation marker first, got %q", got[0])
	}
	// The tail is what matters, so the final line must survive.
	if got[len(got)-1] != "line" {
		t.Errorf("expected the newest line last, got %q", got[len(got)-1])
	}
}

func TestTruncateLinesRespectsBothBounds(t *testing.T) {
	// Long enough that the byte cap bites before the item cap.
	lines := make([]string, 0, autopsy.MaxLogLines)
	for i := 0; i < autopsy.MaxLogLines; i++ {
		lines = append(lines, strings.Repeat("y", maxLogLineBytes))
	}

	got := truncateLines(lines)

	total := 0
	for _, line := range got {
		total += len(line)
	}
	if total > maxLogTotalBytes+len(truncationMarker) {
		t.Errorf("total captured bytes = %d, want <= %d", total, maxLogTotalBytes)
	}
	if len(got) > autopsy.MaxLogLines {
		t.Errorf("entry count = %d, want <= %d", len(got), autopsy.MaxLogLines)
	}
}

// The backwards scan used to stop only on newline count or start-of-file, and
// re-appended the whole accumulation each step. A file with few newlines was
// therefore read in full, quadratically: 64MiB took 45s and allocated 262GB,
// inside an agent limited to 128Mi.
func TestTailFileBoundsWorkOnALowNewlineFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "0.log")

	// 8MiB with a single newline near the start.
	var b strings.Builder
	b.WriteString("first line\n")
	b.WriteString(strings.Repeat("x", 8<<20))
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	lines, err := tailFile(path, 50)
	if err != nil {
		t.Fatalf("tailFile: %v", err)
	}

	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc

	// The scan is capped at roughly lines*maxLogLineBytes*2, so reading an 8MiB
	// file must cost a small multiple of that, not the whole file repeatedly.
	const allocationCeiling = 32 << 20
	if allocated > allocationCeiling {
		t.Errorf("tailFile allocated %d bytes, want <= %d", allocated, allocationCeiling)
	}
	if len(lines) == 0 {
		t.Fatal("expected at least one line")
	}

	// The scan is capped, so the returned tail cannot exceed it however few
	// newlines the file has. Individual lines are shortened later, by
	// truncateLines; the contract here is only that the read is bounded.
	returned := 0
	for _, line := range lines {
		returned += len(line)
	}
	if maxRead := 50*maxLogLineBytes*2 + tailChunkSize; returned > maxRead {
		t.Errorf("tailFile returned %d bytes, want <= the %d byte read cap", returned, maxRead)
	}
}

func TestTailFileReadsTheTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "0.log")
	var b strings.Builder
	for i := 0; i < 500; i++ {
		b.WriteString("line-")
		b.WriteString(strings.Repeat("z", 10))
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	lines, err := tailFile(path, 10)
	if err != nil {
		t.Fatalf("tailFile: %v", err)
	}
	if len(lines) != 10 {
		t.Fatalf("got %d lines, want 10", len(lines))
	}
	want := "line-" + strings.Repeat("z", 10)
	for _, line := range lines {
		if line != want {
			t.Errorf("unexpected line %q", line)
		}
	}
}

func TestTailFileHandlesNoTrailingNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "0.log")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma"), 0o600); err != nil {
		t.Fatal(err)
	}

	lines, err := tailFile(path, 10)
	if err != nil {
		t.Fatalf("tailFile: %v", err)
	}
	if len(lines) != 3 || lines[2] != "gamma" {
		t.Errorf("got %q, want the final unterminated line preserved", lines)
	}
}

// A symlink under /var/log/pods is not something the kubelet creates. Following
// one would let anything that can write into /var/log — a log shipper with the
// conventional read-write hostPath is the realistic case — choose a file for the
// agent to read and publish in a report readable with only "get podcrashreports".
func TestLogCaptureRefusesSymlinks(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "token")
	if err := os.WriteFile(secret, []byte("super-secret-token"), 0o600); err != nil {
		t.Fatal(err)
	}

	logDir := filepath.Join(dir, "logs")
	if err := os.Mkdir(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(logDir, "0.log")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}

	// findLatestLogFile must not offer it at all.
	if _, err := findLatestLogFile(logDir); err == nil {
		t.Error("findLatestLogFile returned a symlinked log file")
	}

	// And tailFile must refuse it even if handed the path directly.
	if _, err := tailFile(filepath.Join(logDir, "0.log"), 10); err == nil {
		t.Error("tailFile followed a symlink out of the log directory")
	}
}

func TestTailFileRefusesNonRegularFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := tailFile(dir, 10); err == nil {
		t.Error("tailFile accepted a directory")
	}
}

// Every incarnation of a container shares one log directory, named by restart
// count. Matching a crash through lastState resolves it correctly, but reading
// the newest file would then attach the replacement container's startup output
// to the post-mortem of the process that died.
func TestCaptureReadsTheVictimsIncarnation(t *testing.T) {
	logDir := t.TempDir()
	for i, content := range []string{"victim output", "replacement output"} {
		path := filepath.Join(logDir, strconv.Itoa(i)+".log")
		if err := os.WriteFile(path, []byte(content+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name  string
		index int
		want  string
	}{
		{name: "the killed incarnation", index: 0, want: "0.log"},
		{name: "the live one", index: 1, want: "1.log"},
		{name: "unknown falls back to newest", index: -1, want: "1.log"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := findLogFile(logDir, tt.index)
			if err != nil {
				t.Fatalf("findLogFile: %v", err)
			}
			if filepath.Base(got) != tt.want {
				t.Errorf("findLogFile(%d) = %s, want %s", tt.index, filepath.Base(got), tt.want)
			}
		})
	}
}

// If the victim's own log has been rotated away, its output is gone. Returning
// the replacement's would be worse than returning nothing, because the report
// would present another process's logs as the dying one's.
func TestCaptureRefusesToSubstituteAnotherIncarnation(t *testing.T) {
	logDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(logDir, "3.log"), []byte("newer\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := findLogFile(logDir, 1); err == nil {
		t.Error("findLogFile substituted a different incarnation's log")
	}
}

func TestFindLogFileRefusesASymlinkedIncarnation(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "token")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(dir, "logs")
	if err := os.Mkdir(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(logDir, "0.log")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}

	if _, err := findLogFile(logDir, 0); err == nil {
		t.Error("findLogFile followed a symlink")
	}
}
