package agent

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// podLogBasePath is the host path where kubelet writes container logs.
	podLogBasePath = "/var/log/pods"

	// logCaptureRetries is the number of retry attempts for log capture.
	logCaptureRetries = 3

	// logCaptureBaseDelay is the initial delay between retries. Retries use
	// exponential back-off: 100ms, 200ms, 200ms ≈ 500ms total.
	logCaptureBaseDelay = 100 * time.Millisecond
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

		return lines, nil
	}

	return nil, fmt.Errorf("log capture failed after %d retries: %w", logCaptureRetries, lastErr)
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
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", path, err)
	}
	defer f.Close()

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
