package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// podLogBasePath is the host path where kubelet writes container logs.
	podLogBasePath = "/var/log/pods"

	// logCaptureRetries is the number of retry attempts for log capture.
	logCaptureRetries = 3

	// logCaptureBaseDelay is the initial delay between retries. Retries use
	// exponential back-off: 100ms, 200ms, 200ms ≈ 500ms total.
	logCaptureBaseDelay = 100 * time.Millisecond

	// maxLogLineBytes caps a single captured line. The CRI runtime splits log
	// lines at roughly 16KiB, so without a cap 50 lines could approach etcd's
	// 1.5MiB per-object limit and make the status write fail outright.
	maxLogLineBytes = 2048

	// maxLogTotalBytes caps the captured lines in aggregate, for the same reason.
	maxLogTotalBytes = 64 * 1024

	// truncationMarker is appended to any line that was shortened.
	truncationMarker = "…[truncated]"
)

// LogCapturer reads the final log lines from container log files on the host.
// It handles the race condition where container runtimes may temporarily lock
// or rotate log files during container teardown.
type LogCapturer struct {
	tailLines int
}

// NewLogCapturer creates a new LogCapturer that captures the specified number
// of tail lines from container log files.
func NewLogCapturer(tailLines int) *LogCapturer {
	return &LogCapturer{
		tailLines: tailLines,
	}
}

// CaptureLogTail reads the last N lines from the container's log file under
// /var/log/pods/<namespace>_<podName>_<podUID>/<containerName>/. It retries
// with exponential back-off to handle container runtime teardown races.
func (lc *LogCapturer) CaptureLogTail(podUID, namespace, podName, containerName string) ([]string, error) {
	logDir := filepath.Join(podLogBasePath, fmt.Sprintf("%s_%s_%s", namespace, podName, podUID), containerName)

	var lastErr error
	delay := logCaptureBaseDelay

	for attempt := 0; attempt < logCaptureRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(delay)
			delay *= 2
		}

		logFile, err := findLatestLogFile(logDir)
		if err != nil {
			lastErr = fmt.Errorf("attempt %d: find log file: %w", attempt+1, err)
			continue
		}

		lines, err := tailFile(logFile, lc.tailLines)
		if err != nil {
			lastErr = fmt.Errorf("attempt %d: tail file: %w", attempt+1, err)
			continue
		}

		return truncateLines(parseLogLines(lines)), nil
	}

	return nil, fmt.Errorf("log capture failed after %d retries: %w", logCaptureRetries, lastErr)
}

// dockerLogEntry is the docker json-file log format, still produced by the
// dockershim-era runtimes and by some managed distributions.
type dockerLogEntry struct {
	Log    string `json:"log"`
	Stream string `json:"stream"`
}

// parseLogLines strips container-runtime framing from raw log file lines and
// reassembles lines the runtime split.
//
// Kubelet writes the CRI format, "<RFC3339Nano> <stream> <F|P> <message>",
// where a P tag means the runtime split an over-long line and the message
// continues on the next one. Storing those raw meant every captured line
// carried a timestamp, a stream name and a flag, and long lines appeared cut
// into fragments.
//
// Lines in an unrecognised format are passed through unchanged rather than
// dropped: an unfamiliar runtime should degrade to raw output, not to nothing.
func parseLogLines(raw []string) []string {
	out := make([]string, 0, len(raw))
	var partial strings.Builder

	flush := func(message string) {
		if partial.Len() > 0 {
			partial.WriteString(message)
			out = append(out, partial.String())
			partial.Reset()
			return
		}
		out = append(out, message)
	}

	for _, line := range raw {
		if entry, ok := parseDockerLogLine(line); ok {
			flush(entry)
			continue
		}

		message, isPartial, ok := parseCRILogLine(line)
		if !ok {
			// Unknown format: emit as-is, after closing any open partial.
			flush(line)
			continue
		}

		if isPartial {
			partial.WriteString(message)
			continue
		}
		flush(message)
	}

	// A container killed mid-line leaves a partial with no terminating full
	// line. That fragment is often the most interesting thing in the file.
	if partial.Len() > 0 {
		out = append(out, partial.String())
	}

	return out
}

// parseCRILogLine splits a CRI-format log line into its message, reporting
// whether the runtime marked it as a partial line.
func parseCRILogLine(line string) (message string, isPartial bool, ok bool) {
	// <timestamp> <stream> <tag> <message>; the message itself may contain
	// spaces, so only the first three fields are split off.
	timestamp, rest, found := strings.Cut(line, " ")
	if !found {
		return "", false, false
	}
	stream, rest, found := strings.Cut(rest, " ")
	if !found {
		return "", false, false
	}
	tag, message, found := strings.Cut(rest, " ")
	if !found {
		// A line with an empty message still has a trailing tag and no space.
		tag, message = rest, ""
	}

	if stream != "stdout" && stream != "stderr" {
		return "", false, false
	}
	// The tag field is "F" or "P", optionally followed by ":"-separated flags.
	baseTag, _, _ := strings.Cut(tag, ":")
	if baseTag != "F" && baseTag != "P" {
		return "", false, false
	}
	// Guard against a plain log line that happens to have this shape.
	if _, err := time.Parse(time.RFC3339Nano, timestamp); err != nil {
		return "", false, false
	}

	return message, baseTag == "P", true
}

// parseDockerLogLine extracts the message from a docker json-file log line.
func parseDockerLogLine(line string) (string, bool) {
	if !strings.HasPrefix(line, "{") {
		return "", false
	}

	var entry dockerLogEntry
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		return "", false
	}
	if entry.Stream == "" {
		return "", false
	}
	return strings.TrimSuffix(entry.Log, "\n"), true
}

// truncateLines bounds captured log output so a report can always be written.
// Individual lines are shortened to maxLogLineBytes and the set is cut off once
// it reaches maxLogTotalBytes, keeping the earliest lines of the tail. Cuts are
// made on rune boundaries so the result stays valid UTF-8, which the API server
// requires of a string field.
func truncateLines(lines []string) []string {
	if len(lines) == 0 {
		return lines
	}

	out := make([]string, 0, len(lines))
	total := 0

	for _, line := range lines {
		if len(line) > maxLogLineBytes {
			line = trimToValidUTF8(line, maxLogLineBytes) + truncationMarker
		}
		if total+len(line) > maxLogTotalBytes {
			out = append(out, truncationMarker)
			break
		}
		out = append(out, line)
		total += len(line)
	}

	return out
}

// trimToValidUTF8 cuts s to at most limit bytes without splitting a rune.
func trimToValidUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}

// findLatestLogFile finds the most recent log file in the given directory.
// Container runtimes name log files with numeric suffixes (e.g., 0.log, 1.log)
// where higher numbers are more recent.
func findLatestLogFile(logDir string) (string, error) {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return "", fmt.Errorf("failed to read log directory %s: %w", logDir, err)
	}

	var logFiles []os.DirEntry
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".log") {
			logFiles = append(logFiles, entry)
		}
	}

	if len(logFiles) == 0 {
		return "", fmt.Errorf("no log files found in %s", logDir)
	}

	// Sort by the numeric prefix — highest number is the latest log file.
	sort.Slice(logFiles, func(i, j int) bool {
		numI := extractLogNumber(logFiles[i].Name())
		numJ := extractLogNumber(logFiles[j].Name())
		return numI > numJ
	})

	return filepath.Join(logDir, logFiles[0].Name()), nil
}

// extractLogNumber extracts the numeric portion from a log file name like "3.log".
// Returns -1 if the name cannot be parsed.
func extractLogNumber(name string) int {
	base := strings.TrimSuffix(name, ".log")
	n, err := strconv.Atoi(base)
	if err != nil {
		return -1
	}
	return n
}

// tailFile reads the last N lines from a file using an efficient reverse-read
// strategy. It reads from the end of the file in chunks to avoid loading the
// entire file into memory.
func tailFile(path string, lines int) ([]string, error) {
	// Defence in depth: Config.Validate rejects this, but a non-positive count
	// would otherwise index past the end of the slice below.
	if lines < 1 {
		return nil, fmt.Errorf("tail line count must be at least 1, got %d", lines)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", path, err)
	}
	// Read-only handle, so there is no deferred write for Close to report.
	defer func() { _ = f.Close() }()

	stat, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat %s: %w", path, err)
	}

	fileSize := stat.Size()
	if fileSize == 0 {
		return []string{}, nil
	}

	// Read the file from the end in chunks.
	const chunkSize = 8192
	var collected []byte
	remaining := fileSize
	lineCount := 0

	for remaining > 0 && lineCount <= lines {
		readSize := int64(chunkSize)
		if readSize > remaining {
			readSize = remaining
		}

		offset := remaining - readSize
		chunk := make([]byte, readSize)
		n, err := f.ReadAt(chunk, offset)
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("failed to read chunk at offset %d: %w", offset, err)
		}
		chunk = chunk[:n]

		// Count newlines in this chunk.
		for _, b := range chunk {
			if b == '\n' {
				lineCount++
			}
		}

		collected = append(chunk, collected...)
		remaining = offset
	}

	// Split into lines and return the last N.
	allLines := strings.Split(string(collected), "\n")

	// Remove trailing empty line from final newline.
	if len(allLines) > 0 && allLines[len(allLines)-1] == "" {
		allLines = allLines[:len(allLines)-1]
	}

	if len(allLines) <= lines {
		return allLines, nil
	}

	return allLines[len(allLines)-lines:], nil
}
