// Package agent implements the DaemonSet node agent for kube-autopsy.
// It detects OOM kills by attaching an eBPF kprobe to the kernel's
// oom_kill_process, rather than by polling cgroup memory.events, which reports
// that a kill happened but not who triggered it or what the victim was using.
// The diagnostic data (memory stats, log tails) is captured before the container
// runtime cleans up the cgroup and log directories.
package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cilium/ebpf/ringbuf"
	"github.com/kube-autopsy/kube-autopsy/internal/config"
	"golang.org/x/sys/unix"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// cgroupRoot is the unified cgroup hierarchy mount point.
const cgroupRoot = "/sys/fs/cgroup"

// detectCgroupVersion reports the cgroup hierarchy in use, refusing to run on
// anything but the unified one.
//
// This is a hard requirement rather than a preference: the eBPF program reads
// the victim's cgroup through task->cgroups->dfl_cgrp, which is the unified
// hierarchy's node. On a v1 host that name is not the container scope, so every
// kill would be received and then silently fail to resolve to a pod — a failure
// mode that looks exactly like a quiet node.
func detectCgroupVersion() (string, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(cgroupRoot, &st); err != nil {
		return "", fmt.Errorf(
			"cannot inspect %s (%w); kube-autopsy requires a cgroups v2 (unified) host",
			cgroupRoot, err)
	}
	if st.Type != unix.CGROUP2_SUPER_MAGIC {
		return "", fmt.Errorf(
			"%s is not a cgroups v2 (unified) hierarchy; kube-autopsy cannot resolve "+
				"an OOM victim to its pod on cgroups v1, so every kill on this node "+
				"would go unreported. Boot the node with systemd.unified_cgroup_hierarchy=1 "+
				"or use a distribution that defaults to cgroups v2",
			cgroupRoot)
	}
	return "v2", nil
}

var log = logf.Log.WithName("agent")

// shutdownDrainTimeout bounds how long Run waits for in-flight crash reports to
// reach the API server after the context is cancelled. It must stay comfortably
// below the DaemonSet's terminationGracePeriodSeconds.
const shutdownDrainTimeout = 15 * time.Second

// pageSize converts the kernel's page counts into bytes. The agent shares a
// kernel with the workloads it observes, so the process page size is the right
// one to use.
var pageSize = int64(os.Getpagesize())

// reportCooldown suppresses repeat reports for the same container within a
// window. A container that OOMs in a tight loop would otherwise produce a
// report per kill for as long as it runs — an etcd and API-server pressure
// vector open to any unprivileged workload.
type reportCooldown struct {
	window time.Duration
	mu     sync.Mutex
	last   map[string]time.Time
}

func newReportCooldown(window time.Duration) *reportCooldown {
	return &reportCooldown{
		window: window,
		last:   make(map[string]time.Time),
	}
}

// allow reports whether a crash for key should produce a report now, recording
// the decision. It also evicts entries that have aged out, so the map cannot
// grow without bound on a node with lots of churn.
func (c *reportCooldown) allow(key string, now time.Time) bool {
	if c.window <= 0 {
		return true
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if seen, ok := c.last[key]; ok && now.Sub(seen) < c.window {
		return false
	}

	for k, seen := range c.last {
		if now.Sub(seen) >= c.window {
			delete(c.last, k)
		}
	}

	c.last[key] = now
	return true
}

// CrashEvent contains diagnostic data captured at the moment an OOM kill is
// detected via eBPF.
type CrashEvent struct {
	ContainerID string
	// OOMScopeLimitBytes is the capacity of the OOM scope (the container's
	// memory limit, or the node's RAM for a global OOM), not the victim's usage.
	OOMScopeLimitBytes int64
	// VictimRSSBytes is the victim's resident memory: anonymous plus file-backed
	// plus shared, matching the kernel's get_mm_rss(). Only meaningful when
	// RSSValid is true.
	VictimRSSBytes  int64
	OOMVictimPID    int32
	OOMVictimComm   string
	TriggerPID      int32
	TriggerComm     string
	OOMScore        int64
	OOMScoreAdj     int32
	AnonRSSBytes    int64
	FileRSSBytes    int64
	ShmemRSSBytes   int64
	SwapBytes       int64
	PageTablesBytes int64
	// RSSValid reports whether the kernel's memory layout was recognised. When
	// false the RSS figures are unknown and must not be published.
	RSSValid    bool
	IsGlobalOOM bool
	DetectedAt  time.Time
}

// Agent is the main DaemonSet agent that runs on each node. It watches for
// kernel OOM kill events, resolves each one to the pod owning the victim's
// cgroup v2 scope, and creates PodCrashReport CRDs with captured diagnostic
// data.
type Agent struct {
	client   client.Client
	cfg      *config.Config
	nodeName string
	// tracing reports whether the kprobe is attached and the ring buffer is
	// being read. It backs the readiness probe: a process that started but never
	// attached is otherwise indistinguishable from a working one, and an empty
	// report list reads as "nothing crashed" rather than "not watching".
	tracing atomic.Bool
}

// NewAgent creates a new Agent instance bound to the given Kubernetes client,
// configuration, and node name.
func NewAgent(client client.Client, cfg *config.Config, nodeName string) *Agent {
	return &Agent{
		client:   client,
		cfg:      cfg,
		nodeName: nodeName,
	}
}

// TracingReadyCheck is a healthz.Checker reporting whether this node is
// actually being traced.
func (a *Agent) TracingReadyCheck(*http.Request) error {
	if !a.tracing.Load() {
		return errors.New("eBPF tracer is not attached")
	}
	return nil
}

// Run starts the agent. It attaches the eBPF tracer and blocks until ctx is
// cancelled. Reports already in flight when cancellation arrives are given up
// to shutdownDrainTimeout to reach the API server.
func (a *Agent) Run(ctx context.Context) error {
	// Checked rather than assumed. Pod resolution reads the victim's unified
	// cgroup name, which only exists on cgroups v2, so on a v1 node the agent
	// would load, attach, receive every kill and silently resolve none of them —
	// looking perfectly healthy while reporting nothing.
	cgroupVersion, err := detectCgroupVersion()
	if err != nil {
		return err
	}

	log.Info("agent starting",
		"nodeName", a.nodeName,
		"cgroupVersion", cgroupVersion,
		"logTailLines", a.cfg.LogTailLines,
		"captureLogs", a.cfg.CaptureLogs,
		"uid", os.Getuid(),
	)

	reporter := NewReporter(a.client, a.nodeName, a.cfg.PodOwnerReference)
	capturer := NewLogCapturer(a.cfg.LogTailLines)
	cooldown := newReportCooldown(a.cfg.ReportCooldown())

	// Bound how many reports are built at once, so a burst of OOM kills — a
	// node running out of memory can produce many in quick succession — cannot
	// spawn unbounded goroutines all calling the API server.
	slots := make(chan struct{}, a.cfg.MaxConcurrentReports)

	// Track in-flight report creation for graceful shutdown.
	var wg sync.WaitGroup

	tracer, err := NewOOMTracer()
	if err != nil {
		return fmt.Errorf("failed to initialize eBPF tracer: %w", err)
	}
	defer tracer.Close()

	// From here the node is genuinely being watched, so the pod can go Ready.
	a.tracing.Store(true)
	defer a.tracing.Store(false)

	// Report creation runs on a context detached from ctx: when SIGTERM cancels
	// ctx we still need the API calls for already-captured events to succeed.
	// It is cancelled explicitly once the drain completes or times out.
	reportCtx, cancelReports := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelReports()

	// Start reading eBPF events in a goroutine.
	watchErrCh := make(chan error, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			event, err := tracer.ReadEvent(ctx)
			if err != nil {
				// A cancelled context or a closed ring buffer is the normal
				// shutdown path, not a failure — leave watchErrCh empty so the
				// select below deterministically takes the ctx.Done() branch.
				if !errors.Is(err, context.Canceled) && !errors.Is(err, ringbuf.ErrClosed) {
					watchErrCh <- err
				}
				return
			}

			// The kernel reports memory in pages; the page size is a property of
			// the running kernel, not a constant (arm64 commonly uses 64KiB).
			anonRSS := int64(event.AnonRssPages) * pageSize
			fileRSS := int64(event.FileRssPages) * pageSize
			shmemRSS := int64(event.ShmemRssPages) * pageSize

			crash := CrashEvent{
				ContainerID:        containerIDFromCgroup(parseComm(event.CgroupName[:])),
				OOMScopeLimitBytes: int64(event.ScopeTotalPages) * pageSize,
				// Matches the kernel's get_mm_rss(), so this reconciles with
				// the OOMScore reported next to it.
				VictimRSSBytes:  anonRSS + fileRSS + shmemRSS,
				OOMVictimPID:    int32(event.Tpid),
				OOMVictimComm:   parseComm(event.Tcomm[:]),
				TriggerPID:      int32(event.Fpid),
				TriggerComm:     parseComm(event.Fcomm[:]),
				OOMScore:        event.OomScore,
				OOMScoreAdj:     int32(event.OomScoreAdj),
				AnonRSSBytes:    anonRSS,
				FileRSSBytes:    fileRSS,
				ShmemRSSBytes:   shmemRSS,
				SwapBytes:       int64(event.SwapPages) * pageSize,
				PageTablesBytes: int64(event.PgtablesBytes),
				RSSValid:        event.RssValid,
				IsGlobalOOM:     event.IsGlobalOom,
				DetectedAt:      time.Now(),
			}

			// Counted before any filtering, so "the agent saw nothing" can be
			// told apart from "the agent saw plenty and recorded none of it".
			EventsReceivedTotal.Inc()

			// Suppression is keyed on the container, so a crash loop reports
			// once per window while unrelated containers are unaffected.
			if !cooldown.allow(crash.ContainerID, crash.DetectedAt) {
				log.V(1).Info("suppressing repeat crash report within the cooldown window",
					"containerID", crash.ContainerID)
				ReportsSuppressedTotal.Inc()
				continue
			}

			// The slot is taken here rather than inside the goroutine. Acquiring
			// it after the `go` bounded only the concurrent API calls, not the
			// goroutines queued to make them — so a node-wide OOM storm, which is
			// the case the limit exists for, could still pile up thousands of them
			// inside the DaemonSet's memory limit.
			//
			// The cancellation arm watches ctx, not reportCtx: reportCtx outlives
			// the drain by design, so waiting on it would let this goroutine sit
			// here through the whole shutdown window and then call wg.Add after
			// wg.Wait had already started on a zero counter — the race the drain
			// below is careful to avoid. On SIGTERM the reader stops taking new
			// work immediately; events already handed to a worker still complete
			// on reportCtx.
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}

			wg.Add(1)
			go func(ce CrashEvent) {
				defer wg.Done()
				defer func() { <-slots }()

				a.handleCrashEvent(reportCtx, ce, reporter, capturer)
			}(crash)
		}
	}()

	select {
	case err := <-watchErrCh:
		return fmt.Errorf("oom tracer failed: %w", err)
	case <-ctx.Done():
		log.Info("shutdown signal received, completing in-flight reports")
	}

	// Give in-flight report creations a bounded window to finish. The deadline
	// is a context rather than a timer so both waits below can observe it.
	drainCtx, cancelDrain := context.WithTimeout(context.WithoutCancel(ctx), shutdownDrainTimeout)
	defer cancelDrain()

	// The reader goroutine is the only caller of wg.Add, so it has to exit
	// before wg.Wait runs — otherwise an event read moments before shutdown
	// could be added to the group after the wait began and be abandoned.
	select {
	case <-readerDone:
	case <-drainCtx.Done():
		log.Info("timed out waiting for the event reader to stop")
	}

	drained := make(chan struct{})
	go func() {
		wg.Wait()
		close(drained)
	}()

	select {
	case <-drained:
	case <-drainCtx.Done():
		log.Info("timed out draining in-flight reports", "timeout", shutdownDrainTimeout.String())
	}

	log.Info("agent shutdown complete")
	return nil
}

// containerIDFromCgroup extracts a container ID from a cgroup directory name.
// Under the systemd cgroup driver the name looks like "cri-containerd-<id>.scope"
// (or "crio-<id>.scope", "docker-<id>.scope"); under the cgroupfs driver it is
// the bare container ID. Anything unrecognised is returned unchanged.
func containerIDFromCgroup(name string) string {
	parts := strings.Split(name, "-")
	if len(parts) < 2 {
		return name
	}
	return strings.TrimSuffix(parts[len(parts)-1], ".scope")
}

// handleCrashEvent processes a single crash event by resolving pod metadata,
// capturing log tails, and creating a PodCrashReport CRD.
func (a *Agent) handleCrashEvent(ctx context.Context, event CrashEvent, reporter *Reporter, capturer *LogCapturer) {
	eventLog := log.WithValues("containerID", event.ContainerID)

	eventLog.V(1).Info("processing crash event")

	// Measured from kernel detection rather than from the start of this
	// function, so queueing behind the concurrency limit is included.
	defer func() {
		CaptureLatencySeconds.Observe(time.Since(event.DetectedAt).Seconds())
	}()

	if !event.RSSValid {
		UnsupportedKernelEventsTotal.Inc()
	}

	// Resolve pod metadata from the Kubernetes API.
	podMeta, err := reporter.ResolvePodMeta(ctx, event)
	if err != nil {
		var listErr *PodListError
		if errors.As(err, &listErr) {
			// A failure to read pods at all is an agent problem and has to be
			// visible; it used to share a counter and a V(1) log line with the
			// expected no-pod case below, where nobody would ever see it.
			eventLog.Error(err, "could not list pods to resolve the OOM victim")
			ReportErrorsTotal.WithLabelValues(StageListPods).Inc()
			return
		}
		// A global OOM can kill a process that belongs to no pod at all (a
		// systemd unit, sshd, the kubelet). That is not an agent failure, and
		// logging it as an error would be loudest exactly when the node is
		// already unhealthy.
		eventLog.V(1).Info("no pod owns this cgroup, skipping", "reason", err.Error())
		ReportErrorsTotal.WithLabelValues(StageNoPod).Inc()
		return
	}

	eventLog = eventLog.WithValues(
		"podName", podMeta.PodName,
		"namespace", podMeta.Namespace,
		"containerName", podMeta.ContainerName,
		"podUID", podMeta.PodUID,
	)

	// Capture log tails — this uses retry/back-off for runtime teardown races.
	// Off by default: see Config.CaptureLogs for why.
	var logLines []string
	if a.cfg.CaptureLogs {
		logLines, err = capturer.CaptureLogTail(podMeta)
		if err != nil {
			eventLog.Error(err, "failed to capture log tail, continuing with empty logs")
			LogCaptureFailuresTotal.WithLabelValues(podMeta.Namespace).Inc()
			logLines = nil
		}
	}

	// Create the PodCrashReport CRD.
	if err := reporter.CreateCrashReport(ctx, event, podMeta, logLines); err != nil {
		var statusErr *StatusWriteError
		if errors.As(err, &statusErr) {
			// The report exists but carries no diagnostics, and nothing will ever
			// fill them in. It is counted separately because the operator sees a
			// report — it just has zeroes in it — rather than nothing at all.
			eventLog.Error(err, "PodCrashReport was created but its diagnostics could not be attached")
			ReportErrorsTotal.WithLabelValues(StageStatus).Inc()
			return
		}
		eventLog.Error(err, "failed to create PodCrashReport")
		ReportErrorsTotal.WithLabelValues(StageCreate).Inc()
		return
	}

	eventLog.Info("PodCrashReport created successfully")
}
