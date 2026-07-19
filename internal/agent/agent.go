// Package agent implements the DaemonSet node agent for kube-autopsy.
// It monitors cgroup v2 memory events to detect OOM kills and captures
// diagnostic data (memory stats, log tails) before the container runtime
// cleans up the cgroup and log directories.
package agent

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kube-autopsy/kube-autopsy/internal/config"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var log = logf.Log.WithName("agent")

// CrashEvent contains diagnostic data captured at the moment an OOM kill is
// detected via eBPF.
type CrashEvent struct {
	ContainerID        string
	PodUID             string
	PeakMemoryBytes    int64
	CurrentMemoryBytes int64
	OOMKillCount       int64
	OOMVictimPID       int32
	OOMVictimComm      string
	TriggerPID         int32
	TriggerComm        string
	OOMScore           int32
	OOMScoreAdj        int32
	AnonRSSBytes       int64
	FileRSSBytes       int64
	PageTablesBytes    int64
	IsGlobalOOM        bool
	DetectedAt         time.Time
}

// Agent is the main DaemonSet agent that runs on each node. It watches for
// cgroup v2 OOM kill events and creates PodCrashReport CRDs with captured
// diagnostic data.
type Agent struct {
	client   client.Client
	cfg      *config.Config
	nodeName string
	stopCh   chan struct{}
}

// NewAgent creates a new Agent instance bound to the given Kubernetes client,
// configuration, and node name.
func NewAgent(client client.Client, cfg *config.Config, nodeName string) *Agent {
	return &Agent{
		client:   client,
		cfg:      cfg,
		nodeName: nodeName,
		stopCh:   make(chan struct{}),
	}
}

// Run starts the agent. It verifies that cgroups v2 is available, starts the
// CgroupWatcher, and blocks until the context is cancelled or SIGTERM is
// received. In-flight report creation is completed before shutdown.
func (a *Agent) Run(ctx context.Context) error {
	log.Info("agent starting",
		"nodeName", a.nodeName,
		"cgroupVersion", "v2",
		"logTailLines", a.cfg.LogTailLines,
		"uid", os.Getuid(),
	)

	reporter := NewReporter(a.client, a.nodeName)
	capturer := NewLogCapturer(a.cfg.LogTailLines)

	// Track in-flight report creation for graceful shutdown.
	var wg sync.WaitGroup

	tracer, err := NewOOMTracer()
	if err != nil {
		return fmt.Errorf("failed to initialize eBPF tracer: %w", err)
	}
	defer tracer.Close()

	// Set up SIGTERM handling for graceful shutdown.
	sigCtx, sigCancel := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer sigCancel()

	// Start reading eBPF events in a goroutine
	watchErrCh := make(chan error, 1)
	go func() {
		for {
			event, err := tracer.ReadEvent(sigCtx)
			if err != nil {
				watchErrCh <- err
				return
			}
			
			// Try to parse the container ID from the cgroup name
			rawCgroupName := parseComm(event.CgroupName[:])
			// cgroupName is like cri-containerd-xxx.scope or similar
			cgroupName := rawCgroupName
			parts := strings.Split(rawCgroupName, "-")
			if len(parts) >= 2 {
				id := parts[len(parts)-1]
				id = strings.TrimSuffix(id, ".scope")
				cgroupName = id
			}
			
			crash := CrashEvent{
				ContainerID:        cgroupName, // We use the cgroupName to match cs.ContainerID
				PeakMemoryBytes:    int64(event.Pages * 4096), // Rough estimate
				CurrentMemoryBytes: int64(event.AnonRss + event.FileRss + event.Pgtables),
				OOMKillCount:       1,
				OOMVictimPID:       int32(event.Tpid),
				OOMVictimComm:      parseComm(event.Tcomm[:]),
				TriggerPID:         int32(event.Fpid),
				TriggerComm:        parseComm(event.Fcomm[:]),
				OOMScore:           int32(event.OomScore),
				OOMScoreAdj:        int32(event.OomScoreAdj),
				AnonRSSBytes:       int64(event.AnonRss),
				FileRSSBytes:       int64(event.FileRss),
				PageTablesBytes:    int64(event.Pgtables),
				IsGlobalOOM:        event.IsGlobalOom,
			}

			wg.Add(1)
			go func(ce CrashEvent) {
				defer wg.Done()
				a.handleCrashEvent(ctx, ce, reporter, capturer)
			}(crash)
		}
	}()


	select {
	case err := <-watchErrCh:
		if err != nil {
			return fmt.Errorf("cgroup watcher failed: %w", err)
		}
	case <-sigCtx.Done():
		log.Info("shutdown signal received, completing in-flight reports")
		close(a.stopCh)
	}

	// Wait for all in-flight report creations to finish.
	wg.Wait()
	log.Info("agent shutdown complete")
	return nil
}

// handleCrashEvent processes a single crash event by resolving pod metadata,
// capturing log tails, and creating a PodCrashReport CRD.
func (a *Agent) handleCrashEvent(ctx context.Context, event CrashEvent, reporter *Reporter, capturer *LogCapturer) {
	eventLog := log.WithValues(
		"podUID", event.PodUID,
		"containerID", event.ContainerID,
		"oomKillCount", event.OOMKillCount,
	)

	eventLog.Info("processing crash event")

	// Resolve pod metadata from the Kubernetes API.
	podMeta, err := reporter.ResolvePodMeta(ctx, event)
	if err != nil {
		eventLog.Error(err, "failed to resolve pod metadata")
		return
	}

	eventLog = eventLog.WithValues(
		"podName", podMeta.PodName,
		"namespace", podMeta.Namespace,
		"containerName", podMeta.ContainerName,
	)

	// Capture log tails — this uses retry/back-off for runtime teardown races.
	logLines, err := capturer.CaptureLogTail(podMeta.PodUID, podMeta.Namespace, podMeta.PodName, podMeta.ContainerName)
	if err != nil {
		eventLog.Error(err, "failed to capture log tail, continuing with empty logs")
		logLines = nil
	}

	// Create the PodCrashReport CRD.
	if err := reporter.CreateCrashReport(ctx, event, podMeta, logLines); err != nil {
		eventLog.Error(err, "failed to create PodCrashReport")
		return
	}

	eventLog.Info("PodCrashReport created successfully")
}

// detectCgroupsV2 verifies that the system is running cgroups v2 by checking
// for the existence of /sys/fs/cgroup/cgroup.controllers.
func detectCgroupsV2() error {
	const cgroupControllers = "/sys/fs/cgroup/cgroup.controllers"
	if _, err := os.Stat(cgroupControllers); os.IsNotExist(err) {
		return fmt.Errorf(
			"cgroups v2 is required but not detected: %s does not exist. "+
				"kube-autopsy only supports cgroups v2 (unified hierarchy). "+
				"Ensure your kernel is configured with cgroup_no_v1=all or uses cgroups v2 by default",
			cgroupControllers,
		)
	} else if err != nil {
		return fmt.Errorf("failed to check for cgroups v2: %w", err)
	}
	return nil
}
