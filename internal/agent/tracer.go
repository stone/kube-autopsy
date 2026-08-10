package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/cilium/ebpf/btf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
	"github.com/kube-autopsy/kube-autopsy/internal/agent/bpf"
)

// oomKillSymbol is the kernel function the tracer attaches to. It is a static
// function in mm/oom_kill.c, so some kernel builds expose it only under a
// compiler-generated name such as "oom_kill_process.isra.0", or inline it away
// entirely.
const oomKillSymbol = "oom_kill_process"

// kallsymsPath lists the kernel symbols available for kprobe attachment.
const kallsymsPath = "/proc/kallsyms"

// OOMTracer handles the loading and lifecycle of the eBPF kprobe.
type OOMTracer struct {
	objs   *bpf.BpfObjects
	kprobe link.Link
	reader *ringbuf.Reader
}

// NewOOMTracer loads the eBPF objects into the kernel and attaches the kprobe.
func NewOOMTracer() (*OOMTracer, error) {
	// Kernels before 5.11 charge BPF map memory against RLIMIT_MEMLOCK, whose
	// default is far too small for a 16MB ring buffer. Without this, map
	// creation fails with a bare EPERM that gives no hint as to the cause.
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("removing memlock rlimit: %w", err)
	}

	// CO-RE needs the running kernel's BTF. Checking up front turns an opaque
	// relocation failure into an actionable message.
	if _, err := btf.LoadKernelSpec(); err != nil {
		return nil, fmt.Errorf(
			"kernel BTF is required for CO-RE but could not be loaded (%w). "+
				"Ensure the kernel is built with CONFIG_DEBUG_INFO_BTF=y, or that "+
				"/sys/kernel/btf/vmlinux is present and readable in the agent container", err)
	}

	objs := &bpf.BpfObjects{}
	if err := bpf.LoadBpfObjects(objs, nil); err != nil {
		return nil, fmt.Errorf("loading eBPF objects: %w", err)
	}

	kp, err := link.Kprobe(oomKillSymbol, objs.KprobeOomKillProcess, nil)
	if err != nil {
		objs.Close()
		return nil, fmt.Errorf("attaching kprobe to %s: %w%s", oomKillSymbol, err, kprobeHint())
	}

	rd, err := ringbuf.NewReader(objs.Events)
	if err != nil {
		kp.Close()
		objs.Close()
		return nil, fmt.Errorf("creating ringbuf reader: %w", err)
	}

	return &OOMTracer{
		objs:   objs,
		kprobe: kp,
		reader: rd,
	}, nil
}

// kprobeHint inspects /proc/kallsyms to explain why attachment may have failed.
// It returns a trailing hint string, or "" when it has nothing useful to add.
func kprobeHint() string {
	data, err := os.ReadFile(kallsymsPath)
	if err != nil {
		return ""
	}

	var variants []string
	for _, line := range strings.Split(string(data), "\n") {
		// Lines look like: "ffffffff81234560 t oom_kill_process.isra.0".
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if name := fields[2]; name == oomKillSymbol || strings.HasPrefix(name, oomKillSymbol+".") {
			variants = append(variants, name)
		}
	}

	switch {
	case len(variants) == 0:
		return fmt.Sprintf(
			". %s is not present in %s: this kernel appears to have inlined it, "+
				"so kube-autopsy cannot trace OOM kills on this node",
			oomKillSymbol, kallsymsPath)
	case len(variants) == 1 && variants[0] == oomKillSymbol:
		return ""
	default:
		return fmt.Sprintf(
			". %s exists only under compiler-generated name(s) %s on this kernel",
			oomKillSymbol, strings.Join(variants, ", "))
	}
}

// Close removes the eBPF programs from the kernel and frees resources.
func (t *OOMTracer) Close() {
	if t.reader != nil {
		t.reader.Close()
	}
	if t.kprobe != nil {
		t.kprobe.Close()
	}
	t.objs.Close()
}

// ReadEvent blocks until an OOM event is received from the kernel, or until ctx
// is cancelled. Cancellation closes the ring buffer reader, which unblocks this
// and every subsequent read with ringbuf.ErrClosed.
func (t *OOMTracer) ReadEvent(ctx context.Context) (*bpf.BpfOomEvent, error) {
	stop := context.AfterFunc(ctx, func() { t.reader.Close() })
	defer stop()

	record, err := t.reader.Read()
	if err != nil {
		if errors.Is(err, ringbuf.ErrClosed) && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}

	var event bpf.BpfOomEvent
	if err := binary.Read(bytes.NewReader(record.RawSample), binary.LittleEndian, &event); err != nil {
		return nil, fmt.Errorf("failed to parse ringbuf event: %w", err)
	}
	return &event, nil
}

// parseComm converts a C-style null-terminated byte array to a Go string.
func parseComm(comm []int8) string {
	b := make([]byte, 0, len(comm))
	for _, v := range comm {
		if v == 0 {
			break
		}
		b = append(b, byte(v))
	}
	return string(b)
}
