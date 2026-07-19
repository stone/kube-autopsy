package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/kube-autopsy/kube-autopsy/internal/agent/bpf"
)

// OOMTracer handles the loading and lifecycle of the eBPF kprobe.
type OOMTracer struct {
	objs   *bpf.BpfObjects
	kprobe link.Link
	reader *ringbuf.Reader
}

// NewOOMTracer loads the eBPF objects into the kernel and attaches the kprobe.
func NewOOMTracer() (*OOMTracer, error) {
	objs := &bpf.BpfObjects{}
	if err := bpf.LoadBpfObjects(objs, nil); err != nil {
		return nil, fmt.Errorf("loading eBPF objects: %w", err)
	}

	kp, err := link.Kprobe("oom_kill_process", objs.KprobeOomKillProcess, nil)
	if err != nil {
		objs.Close()
		return nil, fmt.Errorf("attaching kprobe to oom_kill_process: %w", err)
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

// ReadEvent blocks until an OOM event is received from the kernel.
func (t *OOMTracer) ReadEvent(ctx context.Context) (*bpf.BpfOomEvent, error) {
	// Ringbuf reading must handle cancellation since it's blocking
	type result struct {
		event *bpf.BpfOomEvent
		err   error
	}
	ch := make(chan result, 1)

	go func() {
		record, err := t.reader.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				ch <- result{err: context.Canceled}
				return
			}
			ch <- result{err: err}
			return
		}

		var event bpf.BpfOomEvent
		if err := binary.Read(bytes.NewReader(record.RawSample), binary.LittleEndian, &event); err != nil {
			ch <- result{err: fmt.Errorf("failed to parse ringbuf event: %w", err)}
			return
		}
		ch <- result{event: &event}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		return res.event, res.err
	}
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
