package bpf

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/btf"
)

// btfFixtureDir holds kernel BTF blobs used to check that the program's CO-RE
// relocations resolve against real kernels. The blobs are several megabytes
// each and are therefore not committed; see the Makefile's btf-fixtures target.
const btfFixtureDir = "testdata/btf"

// Linux 6.2 replaced mm_struct's mm_rss_stat with an array of percpu_counters.
// The program carries a branch for each layout, guarded by a CO-RE existence
// check, and exactly one of them must be live on any given kernel. If both go
// dead the RSS figures are silently absent — which is the bug this guards.
const (
	preLayoutGuard  = `type_exists, Struct:"mm_rss_stat___pre62"`
	postLayoutGuard = `field_exists, Struct:"mm_struct"`
)

// relocate applies the program's CO-RE relocations against the given kernel BTF
// and returns the resolved value of each existence guard, keyed by guard name.
func relocate(t *testing.T, targetPath string) map[string]int64 {
	t.Helper()

	spec, err := LoadBpf()
	if err != nil {
		t.Fatalf("loading collection spec: %v", err)
	}

	target, err := btf.LoadSpec(targetPath)
	if err != nil {
		t.Fatalf("loading target BTF %s: %v", targetPath, err)
	}

	prog, ok := spec.Programs[BpfProgKprobeOomKillProcess]
	if !ok {
		t.Fatalf("program %s not found in collection", BpfProgKprobeOomKillProcess)
	}

	var relos []*btf.CORERelocation
	var insns []*asm.Instruction
	iter := prog.Instructions.Iterate()
	for iter.Next() {
		if relo := btf.CORERelocationMetadata(iter.Ins); relo != nil {
			relos = append(relos, relo)
			insns = append(insns, iter.Ins)
		}
	}
	if len(relos) == 0 {
		t.Fatal("program carries no CO-RE relocations; the object may be stale")
	}

	builder, err := btf.NewBuilder(nil, nil)
	if err != nil {
		t.Fatalf("creating BTF builder: %v", err)
	}

	fixups, err := btf.CORERelocate(relos, []*btf.Spec{target}, binary.LittleEndian, builder.Add)
	if err != nil {
		t.Fatalf("CO-RE relocation against %s failed: %v", filepath.Base(targetPath), err)
	}

	guards := make(map[string]int64)
	for i, fixup := range fixups {
		desc := relos[i].String()
		if err := fixup.Apply(insns[i]); err != nil {
			// Poisoned fixups are expected for the layout branch that is dead on
			// this kernel; the verifier removes them as unreachable code.
			continue
		}
		switch {
		case strings.Contains(desc, preLayoutGuard):
			guards[preLayoutGuard] = insns[i].Constant
		case strings.Contains(desc, postLayoutGuard) && strings.Contains(desc, "field_exists"):
			guards[postLayoutGuard] = insns[i].Constant
		}
	}
	return guards
}

// TestRSSLayoutGuardsSelectExactlyOneBranch checks the program against whichever
// kernel BTF fixtures are available. On every one of them, exactly one of the
// two mm_struct RSS layouts must be selected.
func TestRSSLayoutGuardsSelectExactlyOneBranch(t *testing.T) {
	entries, err := os.ReadDir(btfFixtureDir)
	if err != nil {
		t.Skipf("no BTF fixtures in %s (run 'make btf-fixtures'): %v", btfFixtureDir, err)
	}

	var fixtures []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".btf") {
			fixtures = append(fixtures, filepath.Join(btfFixtureDir, e.Name()))
		}
	}
	if len(fixtures) == 0 {
		t.Skipf("no .btf files in %s (run 'make btf-fixtures')", btfFixtureDir)
	}

	for _, fixture := range fixtures {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			guards := relocate(t, fixture)

			pre, hasPre := guards[preLayoutGuard]
			post, hasPost := guards[postLayoutGuard]
			if !hasPre || !hasPost {
				t.Fatalf("expected both layout guards to be relocated, got %v", guards)
			}

			live := 0
			if pre != 0 {
				live++
			}
			if post != 0 {
				live++
			}

			if live != 1 {
				t.Errorf("expected exactly one RSS layout branch to be live, got %d "+
					"(pre-6.2 guard=%d, post-6.2 guard=%d). Neither being live means "+
					"anonRssBytes/fileRssBytes are silently never populated on this kernel.",
					live, pre, post)
			}
		})
	}
}
