// Package main is the entry point for the kube-autopsy binary.
// Usage: kube-autopsy <agent|controller> [flags]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	autopsyv1alpha1 "github.com/kube-autopsy/kube-autopsy/api/v1alpha1"
	"github.com/kube-autopsy/kube-autopsy/internal/agent"
	"github.com/kube-autopsy/kube-autopsy/internal/config"
	"github.com/kube-autopsy/kube-autopsy/internal/controller"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(autopsyv1alpha1.AddToScheme(scheme))
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: kube-autopsy <agent|controller> [flags]\n")
		os.Exit(1)
	}

	subcommand := os.Args[1]
	// Remove the subcommand from os.Args so flag parsing works correctly.
	os.Args = append(os.Args[:1], os.Args[2:]...)

	switch subcommand {
	case "agent":
		runAgent()
	case "controller":
		runController()
	default:
		fmt.Fprintf(os.Stderr, "Unknown subcommand %q. Usage: kube-autopsy <agent|controller> [flags]\n", subcommand)
		os.Exit(1)
	}
}

func runAgent() {
	cfg := config.NewConfig()

	fs := flag.CommandLine
	cfg.BindFlags(fs)

	opts := zap.Options{Development: false}
	opts.BindFlags(fs)
	flag.Parse()

	cfg.LoadFromEnv()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		setupLog.Error(fmt.Errorf("NODE_NAME environment variable is required"), "unable to start agent")
		os.Exit(1)
	}

	setupLog.Info("starting kube-autopsy agent", "node", nodeName)

	// Create a minimal manager for the Kubernetes client.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		// Disable metrics and health probes for the agent; the controller handles those.
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// Index pods by nodeName so the agent can efficiently list its own pods.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, "spec.nodeName", func(rawObj client.Object) []string {
		pod := rawObj.(*corev1.Pod)
		return []string{pod.Spec.NodeName}
	}); err != nil {
		setupLog.Error(err, "unable to index pods by nodeName")
		os.Exit(1)
	}

	// Set up signal-aware context for graceful shutdown.
	ctx, cancel := signal.NotifyContext(ctrl.SetupSignalHandler(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	a := agent.NewAgent(mgr.GetClient(), cfg, nodeName)

	// Start the manager in a goroutine so the cache is available for the agent.
	go func() {
		if err := mgr.Start(ctx); err != nil {
			setupLog.Error(err, "manager exited with error")
			os.Exit(1)
		}
	}()

	// Wait for the cache to sync before running the agent.
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		setupLog.Error(fmt.Errorf("cache sync failed"), "unable to sync cache")
		os.Exit(1)
	}

	if err := a.Run(ctx); err != nil {
		setupLog.Error(err, "agent exited with error")
		os.Exit(1)
	}

	setupLog.Info("agent shut down gracefully")
}

func runController() {
	cfg := config.NewConfig()

	fs := flag.CommandLine
	cfg.BindFlags(fs)

	opts := zap.Options{Development: false}
	opts.BindFlags(fs)
	flag.Parse()

	cfg.LoadFromEnv()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	setupLog.Info("starting kube-autopsy controller")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		LeaderElection:         cfg.LeaderElect,
		LeaderElectionID:       "kube-autopsy-leader",
		Metrics:                metricsserver.Options{BindAddress: cfg.MetricsBindAddr},
		HealthProbeBindAddress: cfg.HealthProbeBindAddr,
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// Register Prometheus metrics.
	controller.RegisterMetrics()

	// Set up the webhook sender if configured.
	var webhookSender *controller.WebhookSender
	if cfg.WebhookURL != "" {
		webhookSender = controller.NewWebhookSender(cfg.WebhookURL)
		setupLog.Info("webhook notifications enabled", "url", cfg.WebhookURL)
	}

	// Set up the PodCrashReport reconciler.
	reconciler := &controller.PodCrashReportReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		Config:        cfg,
		WebhookSender: webhookSender,
		Recorder:      mgr.GetEventRecorderFor("kube-autopsy-controller"),
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up PodCrashReport reconciler")
		os.Exit(1)
	}

	// Register health checks.
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up readiness check")
		os.Exit(1)
	}

	// Register the garbage collector as a Runnable with the manager.
	gc := controller.NewGarbageCollector(mgr.GetClient(), time.Duration(cfg.TTLHours)*time.Hour)
	if err := mgr.Add(gc); err != nil {
		setupLog.Error(err, "unable to set up garbage collector")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(ctrl.SetupSignalHandler(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}

	setupLog.Info("controller shut down gracefully")
}
