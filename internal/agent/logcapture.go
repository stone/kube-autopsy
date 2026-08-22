package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	autopsy "github.com/kube-autopsy/kube-autopsy/api/v1alpha1"
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

	// tailChunkSize is how much of the file is read per backwards step.
	tailChunkSize = 8192
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

// CaptureLogTail reads the last N lines from the log file of the container
// incarnation identified by meta, under
// /var/log/pods/<namespace>_<podName>_<podUID>/<containerName>/. It retries
// with exponential back-off to handle container runtime teardown races.
func (lc *LogCapturer) CaptureLogTail(meta PodMeta) ([]string, error) {
	logDir := filepath.Join(podLogBasePath,
		fmt.Sprintf("%s_%s_%s", meta.Namespace, meta.PodName, meta.PodUID), meta.ContainerName)

	var lastErr error
	delay := logCaptureBaseDelay

	for attempt := 0; attempt < logCaptureRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(delay)
			delay *= 2
		}

		logFile, err := findLogFile(logDir, meta.LogFileIndex)
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
	// A pointer so an absent "log" key can be told from an empty one; only a
	// genuine docker entry has the key at all.
	Log    *string `json:"log"`
	Stream string  `json:"stream"`
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
// Partials are tracked per stream. stdout and stderr are interleaved in one
// file, so a single buffer let an stderr line land in the middle of a split
// stdout line and spliced the two together — corrupting exactly the last-gasp
// output this exists to capture, since a dying process usually writes to stderr
// while the application logs to stdout.
//
// Lines in an unrecognised format are passed through unchanged rather than
// dropped: an unfamiliar runtime should degrade to raw output, not to nothing.
func parseLogLines(raw []string) []string {
	out := make([]string, 0, len(raw))
	// Keyed by stream name; kubelet only ever writes stdout and stderr.
	partials := make(map[string]*strings.Builder, 2)

	// flush closes any partial open on stream and emits the completed line.
	flush := func(stream, message string) {
		if b, ok := partials[stream]; ok {
			b.WriteString(message)
			out = append(out, b.String())
			delete(partials, stream)
			return
		}
		out = append(out, message)
	}

	// unframed is the stream key for lines with no recognisable framing. They
	// cannot be attributed to a stream, so they get their own bucket rather than
	// disturbing an in-flight stdout or stderr partial.
	const unframed = ""

	for _, line := range raw {
		if entry, ok := parseDockerLogLine(line); ok {
			flush(unframed, entry)
			continue
		}

		message, stream, isPartial, ok := parseCRILogLine(line)
		if !ok {
			// Unknown format: emit as-is.
			flush(unframed, line)
			continue
		}

		if isPartial {
			b, ok := partials[stream]
			if !ok {
				b = &strings.Builder{}
				partials[stream] = b
			}
			b.WriteString(message)
			continue
		}
		flush(stream, message)
	}

	// A container killed mid-line leaves a partial with no terminating full
	// line. That fragment is often the most interesting thing in the file, so it
	// is emitted rather than discarded. Sorted so the output is deterministic
	// when both streams were mid-line.
	openStreams := make([]string, 0, len(partials))
	for stream := range partials {
		openStreams = append(openStreams, stream)
	}
	sort.Strings(openStreams)
	for _, stream := range openStreams {
		out = append(out, partials[stream].String())
	}

	return out
}

// parseCRILogLine splits a CRI-format log line into its message and stream,
// reporting whether the runtime marked it as a partial line.
func parseCRILogLine(line string) (message, stream string, isPartial, ok bool) {
	// <timestamp> <stream> <tag> <message>; the message itself may contain
	// spaces, so only the first three fields are split off.
	timestamp, rest, found := strings.Cut(line, " ")
	if !found {
		return "", "", false, false
	}
	stream, rest, found = strings.Cut(rest, " ")
	if !found {
		return "", "", false, false
	}
	tag, message, found := strings.Cut(rest, " ")
	if !found {
		// A line with an empty message still has a trailing tag and no space.
		tag, message = rest, ""
	}

	if stream != "stdout" && stream != "stderr" {
		return "", "", false, false
	}
	// The tag field is "F" or "P", optionally followed by ":"-separated flags.
	baseTag, _, _ := strings.Cut(tag, ":")
	if baseTag != "F" && baseTag != "P" {
		return "", "", false, false
	}
	// Guard against a plain log line that happens to have this shape.
	if _, err := time.Parse(time.RFC3339Nano, timestamp); err != nil {
		return "", "", false, false
	}

	return message, stream, baseTag == "P", true
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
	// Only stdout and stderr, matching parseCRILogLine. Accepting any non-empty
	// value meant a structured application log that happened to carry its own
	// "stream" field was claimed as docker framing and replaced by its (absent)
	// "log" key — turning a fatal message into an empty line.
	if entry.Stream != "stdout" && entry.Stream != "stderr" {
		return "", false
	}
	if entry.Log == nil {
		return "", false
	}
	return strings.TrimSuffix(*entry.Log, "\n"), true
}

// truncateLines bounds captured log output so a report can always be written.
// Individual lines are shortened to maxLogLineBytes, the set is cut off once it
// reaches maxLogTotalBytes, and the number of entries is capped at
// autopsy.MaxLogLines. Cuts are made on rune boundaries so the result stays
// valid UTF-8, which the API server requires of a string field.
//
// The item cap matters as much as the byte cap: the CRD rejects a longer list
// outright, and because every diagnostic is written in one status patch, that
// rejection costs the memory figures and OOM scores as well as the logs.
// Config.Validate keeps --log-tail-lines within the same bound, so this is the
// second of two independent guards on the same limit.
func truncateLines(lines []string) []string {
	if len(lines) == 0 {
		return lines
	}

	// Keep the tail: the lines closest to the kill are the interesting ones.
	// The marker prepended below occupies one of the permitted entries and some
	// of the byte budget, so room is reserved for it here rather than discovered
	// afterwards — otherwise adding it would itself breach the cap.
	dropped := false
	budget := maxLogTotalBytes
	if len(lines) > autopsy.MaxLogLines {
		lines = lines[len(lines)-(autopsy.MaxLogLines-1):]
		dropped = true
		budget -= len(truncationMarker)
	}

	out := make([]string, 0, len(lines))
	total := 0

	for _, line := range lines {
		if len(line) > maxLogLineBytes {
			line = trimToValidUTF8(line, maxLogLineBytes) + truncationMarker
		}
		if total+len(line) > budget {
			out = append(out, truncationMarker)
			break
		}
		out = append(out, line)
		total += len(line)
	}

	if dropped {
		out = append([]string{truncationMarker}, out...)
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

// findLogFile locates the log file for one container incarnation. The kubelet
// names them by restart count — 0.log, 1.log — and every incarnation of a
// container shares the directory, so asking for the newest file returns the
// running container's log once the victim has been replaced. index selects the
// incarnation; a negative index means it could not be determined, and the newest
// file is used.
//
// A named index that is absent is an error rather than a fallback: the victim's
// log has been rotated away, and the replacement's output is not a post-mortem
// of the process that died.
func findLogFile(logDir string, index int) (string, error) {
	if index < 0 {
		return findLatestLogFile(logDir)
	}

	path := filepath.Join(logDir, strconv.Itoa(index)+".log")
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("log file for incarnation %d of %s: %w", index, logDir, err)
	}
	// Symlinks are refused here for the same reason as in findLatestLogFile.
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is a symlink, refusing to follow it", path)
	}
	return path, nil
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
	symlinks := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		// A symlink here is not something a current kubelet creates. Following
		// one would let anything that can write into /var/log — a log shipper
		// with the conventional read-write hostPath is the realistic case —
		// point the agent at a file of its choosing and have the contents
		// published in a PodCrashReport, which is readable with only
		// "get podcrashreports".
		if entry.Type()&os.ModeSymlink != 0 {
			symlinks++
			continue
		}
		logFiles = append(logFiles, entry)
	}

	if len(logFiles) == 0 {
		if symlinks > 0 {
			// The dockershim-era layout symlinked these into
			// /var/lib/docker/containers, which the agent does not mount and so
			// could never have read anyway. Saying so beats "no log files found",
			// which would send someone looking for a file that is right there.
			return "", fmt.Errorf(
				"the %d log file(s) in %s are symlinks, which are not followed; "+
					"this layout stores the real files outside /var/log/pods, where "+
					"the agent has no access", symlinks, logDir)
		}
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

// tailFile reads the last N lines from a file by walking backwards in chunks,
// so an enormous log costs no more than the tail that is actually wanted.
//
// Two bounds stop it degenerating. Chunks are collected in reverse and joined
// once at the end: the previous version re-appended the whole accumulation on
// every iteration, which is quadratic — a 64MiB file with no newlines took 45
// seconds and allocated 262GB. And maxReadBytes caps the scan outright, because
// the newline count alone is not a bound: a file with no newlines is read in
// full, and the agent has a 128Mi memory limit. In practice CRI framing splits
// lines at roughly 16KiB so neither is reached, but nothing in the file format
// guarantees that.
func tailFile(path string, lines int) ([]string, error) {
	// Defence in depth: Config.Validate rejects this, but a non-positive count
	// would otherwise index past the end of the slice below.
	if lines < 1 {
		return nil, fmt.Errorf("tail line count must be at least 1, got %d", lines)
	}

	// Read-only and never following a symlink: see findLatestLogFile.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", path, err)
	}
	// Read-only handle, so there is no deferred write for Close to report.
	defer func() { _ = f.Close() }()

	stat, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat %s: %w", path, err)
	}
	// A directory, device or fifo in place of the log file is not something the
	// kubelet produces, and reading one could block indefinitely.
	if !stat.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}

	fileSize := stat.Size()
	if fileSize == 0 {
		return []string{}, nil
	}

	// Cap the scan at what the requested lines could plausibly occupy, with
	// generous headroom for framing, plus one chunk so a short file is never
	// truncated by the cap alone.
	maxReadBytes := int64(lines)*maxLogLineBytes*2 + tailChunkSize

	chunks := make([][]byte, 0, 8)
	remaining := fileSize
	read := int64(0)
	lineCount := 0

	for remaining > 0 && lineCount <= lines && read < maxReadBytes {
		readSize := int64(tailChunkSize)
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

		lineCount += bytes.Count(chunk, []byte{'\n'})

		// Prepended once at the end rather than on every iteration.
		chunks = append(chunks, chunk)
		remaining = offset
		read += int64(n)
	}

	// chunks holds the file's tail in reverse order.
	total := 0
	for _, c := range chunks {
		total += len(c)
	}
	collected := make([]byte, 0, total)
	for i := len(chunks) - 1; i >= 0; i-- {
		collected = append(collected, chunks[i]...)
	}

	// Split into lines and return the last N.
	allLines := strings.Split(string(collected), "\n")

	// Remove trailing empty line from final newline.
	if len(allLines) > 0 && allLines[len(allLines)-1] == "" {
		allLines = allLines[:len(allLines)-1]
	}

	// The scan may have stopped mid-line, either at the read cap or at the start
	// of the file; drop that leading fragment unless it is all there is.
	if remaining > 0 && len(allLines) > 1 {
		allLines = allLines[1:]
	}

	if len(allLines) <= lines {
		return allLines, nil
	}

	return allLines[len(allLines)-lines:], nil
}
