package v1alpha1

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// crdPath is the generated manifest that clusters actually enforce.
const crdPath = "../../deploy/base/crd.yaml"

// MaxLogLines is what the agent trims to and what Config.Validate accepts, but
// the API server enforces the MaxItems marker in the CRD. If the two drift, the
// agent happily builds a list the cluster rejects — and since every diagnostic
// travels in one status patch, that rejection costs the memory figures and OOM
// scores as well as the logs.
func TestMaxLogLinesMatchesTheGeneratedCRD(t *testing.T) {
	manifest, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("reading %s: %v", crdPath, err)
	}

	// The lastLogLines property is the only array in the schema with a maxItems.
	re := regexp.MustCompile(`(?s)lastLogLines:.*?maxItems:\s*(\d+)`)
	match := re.FindSubmatch(manifest)
	if match == nil {
		t.Fatalf("no maxItems found for lastLogLines in %s; the schema bound has gone missing", crdPath)
	}

	maxItems, err := strconv.Atoi(string(match[1]))
	if err != nil {
		t.Fatalf("parsing maxItems %q: %v", match[1], err)
	}

	if maxItems != MaxLogLines {
		t.Errorf("CRD maxItems for lastLogLines is %d but MaxLogLines is %d; "+
			"run `make generate` after changing the marker, and keep the constant in step",
			maxItems, MaxLogLines)
	}
}

// The per-line cap the agent applies must fit inside what the schema accepts,
// for the same reason.
func TestLogLineLengthCapFitsTheGeneratedCRD(t *testing.T) {
	manifest, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("reading %s: %v", crdPath, err)
	}

	re := regexp.MustCompile(`(?s)lastLogLines:.*?items:.*?maxLength:\s*(\d+)`)
	match := re.FindSubmatch(manifest)
	if match == nil {
		t.Fatalf("no per-item maxLength found for lastLogLines in %s", crdPath)
	}

	maxLength, err := strconv.Atoi(string(match[1]))
	if err != nil {
		t.Fatalf("parsing maxLength %q: %v", match[1], err)
	}

	// internal/agent trims to 2048 bytes plus a truncation marker; the schema
	// has to leave room for that.
	const agentLineCap = 2048 + len("…[truncated]")
	if maxLength < agentLineCap {
		t.Errorf("CRD allows %d bytes per log line but the agent can emit %d",
			maxLength, agentLineCap)
	}
}
