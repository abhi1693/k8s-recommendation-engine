package main

import (
	"flag"
	"fmt"
	"time"

	recommendationv1alpha1 "github.com/abhi1693/k8s-recommendation-engine/api/v1alpha1"
	profilecontroller "github.com/abhi1693/k8s-recommendation-engine/internal/controller"
	"github.com/abhi1693/k8s-recommendation-engine/internal/kube"
	"github.com/abhi1693/k8s-recommendation-engine/internal/prom"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func runController(args []string) error {
	fs := flag.NewFlagSet("controller", flag.ContinueOnError)
	var metricsAddress string
	var healthAddress string
	var leaderElect bool
	var leaderElectionLeaseDuration time.Duration
	var leaderElectionRenewDeadline time.Duration
	var leaderElectionRetryPeriod time.Duration
	var watchNamespace string
	var prometheusURL string
	var stateDB string
	var stateRetention time.Duration
	var historyWindow time.Duration
	var historyStep time.Duration
	var reconcileInterval time.Duration
	var reconcileTimeout time.Duration
	var availabilityRecovery bool
	var maxConcurrentReconciles int
	var mode string
	var gitWorktree string
	var proposalKind string
	var proposalDir string
	var proposalBranch string
	var proposalRemote string
	var proposalPush bool
	var proposalBatchWindow time.Duration
	var allowDefaultBranchPush bool
	fs.StringVar(&metricsAddress, "metrics-bind-address", ":8080", "address for controller-runtime metrics; set 0 to disable")
	fs.StringVar(&healthAddress, "health-probe-bind-address", ":8081", "address for liveness and readiness probes")
	fs.BoolVar(&leaderElect, "leader-elect", false, "use a Lease so only one controller instance reconciles profiles")
	fs.DurationVar(&leaderElectionLeaseDuration, "leader-election-lease-duration", 30*time.Second, "leader-election lease duration")
	fs.DurationVar(&leaderElectionRenewDeadline, "leader-election-renew-deadline", 20*time.Second, "leader-election lease renewal deadline")
	fs.DurationVar(&leaderElectionRetryPeriod, "leader-election-retry-period", 5*time.Second, "leader-election retry period")
	fs.StringVar(&watchNamespace, "watch-namespace", "", "namespace containing ApplicationProfile resources; empty watches all namespaces")
	fs.StringVar(&prometheusURL, "prometheus-url", "http://127.0.0.1:9090", "Prometheus base URL shared by profiles")
	fs.StringVar(&stateDB, "state-db", "", "SQLite state database shared by profiles")
	fs.DurationVar(&stateRetention, "state-retention", 14*24*time.Hour, "retention for persisted learning and operational history")
	fs.DurationVar(&historyWindow, "history-window", 24*time.Hour, "Prometheus history window used by profile analysis")
	fs.DurationVar(&historyStep, "history-step", 5*time.Minute, "Prometheus query_range step used by profile analysis")
	fs.DurationVar(&reconcileInterval, "reconcile-interval", 5*time.Minute, "default interval between reconciliations of each profile")
	fs.DurationVar(&reconcileTimeout, "reconcile-timeout", 5*time.Minute, "maximum analysis time for one profile reconciliation")
	fs.BoolVar(&availabilityRecovery, "availability-recovery", false, "allow policy-enabled failed Pods to be recreated directly")
	fs.IntVar(&maxConcurrentReconciles, "max-concurrent-reconciles", 1, "maximum profiles reconciled concurrently")
	fs.StringVar(&mode, "mode", "dry-run", "operation mode shared by profiles: dry-run or propose")
	fs.StringVar(&gitWorktree, "git-worktree", "", "shared local Fleet Git worktree")
	fs.StringVar(&proposalKind, "proposal-kind", "patch", "proposal artifact type: patch or commit")
	fs.StringVar(&proposalDir, "proposal-dir", ".k8s-recommendation-engine/proposals", "relative Git worktree directory for patch artifacts")
	fs.StringVar(&proposalBranch, "proposal-branch", "", "commit target branch; defaults to each profile's Git branch")
	fs.StringVar(&proposalRemote, "proposal-remote", "origin", "Git remote used for proposal pushes")
	fs.BoolVar(&proposalPush, "proposal-push", false, "push proposal commits")
	fs.DurationVar(&proposalBatchWindow, "proposal-batch-window", 15*time.Minute, "stable recommendation batch window before commits")
	fs.BoolVar(&allowDefaultBranchPush, "allow-default-branch-push", false, "allow proposal pushes to a profile's default branch")
	zapOptions := zap.Options{Development: false}
	zapOptions.BindFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if historyWindow <= 0 || historyStep <= 0 || reconcileInterval <= 0 || reconcileTimeout <= 0 || stateRetention <= 0 {
		return fmt.Errorf("history-window, history-step, reconcile-interval, reconcile-timeout, and state-retention must be greater than zero")
	}
	if maxConcurrentReconciles <= 0 {
		return fmt.Errorf("max-concurrent-reconciles must be greater than zero")
	}
	if mode != "dry-run" && mode != "propose" {
		return fmt.Errorf("mode must be dry-run or propose")
	}
	if mode == "propose" && gitWorktree == "" {
		return fmt.Errorf("propose mode requires --git-worktree")
	}
	if proposalBatchWindow < 0 {
		return fmt.Errorf("proposal-batch-window must be non-negative")
	}
	if gitWorktree != "" && maxConcurrentReconciles != 1 {
		return fmt.Errorf("a shared git-worktree requires max-concurrent-reconciles=1")
	}
	if availabilityRecovery && stateDB == "" {
		return fmt.Errorf("availability-recovery requires --state-db")
	}
	if leaderElectionLeaseDuration <= leaderElectionRenewDeadline || leaderElectionRenewDeadline <= leaderElectionRetryPeriod || leaderElectionRetryPeriod <= 0 {
		return fmt.Errorf("leader-election durations must satisfy lease-duration > renew-deadline > retry-period > 0")
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOptions)))
	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load Kubernetes REST config: %w", err)
	}
	kubeClient, err := kube.NewClientForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(recommendationv1alpha1.AddToScheme(scheme))
	cacheOptions := cache.Options{}
	if watchNamespace != "" {
		cacheOptions.DefaultNamespaces = map[string]cache.Config{watchNamespace: {}}
	}
	manager, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                        scheme,
		Cache:                         cacheOptions,
		Metrics:                       metricsserver.Options{BindAddress: metricsAddress, SecureServing: false},
		HealthProbeBindAddress:        healthAddress,
		LeaderElection:                leaderElect,
		LeaderElectionID:              "k8s-recommendation-engine-controller",
		LeaderElectionReleaseOnCancel: true,
		LeaseDuration:                 &leaderElectionLeaseDuration,
		RenewDeadline:                 &leaderElectionRenewDeadline,
		RetryPeriod:                   &leaderElectionRetryPeriod,
	})
	if err != nil {
		return fmt.Errorf("create controller manager: %w", err)
	}
	processor := &profilecontroller.AnalyzerProcessor{
		Kube:                   kubeClient,
		Prometheus:             prom.NewClient(prometheusURL, nil),
		StateDB:                stateDB,
		StateRetention:         stateRetention,
		HistoryWindow:          historyWindow,
		HistoryStep:            historyStep,
		AvailabilityRecovery:   availabilityRecovery,
		Mode:                   mode,
		GitWorktree:            gitWorktree,
		ProposalKind:           proposalKind,
		ProposalDir:            proposalDir,
		ProposalBranch:         proposalBranch,
		ProposalRemote:         proposalRemote,
		ProposalPush:           proposalPush,
		ProposalBatchWindow:    proposalBatchWindow,
		AllowDefaultBranchPush: allowDefaultBranchPush,
	}
	reconciler := &profilecontroller.ApplicationProfileReconciler{
		Client:                   manager.GetClient(),
		Scheme:                   manager.GetScheme(),
		Recorder:                 manager.GetEventRecorderFor("k8s-recommendation-engine"),
		Processor:                processor,
		DefaultReconcileInterval: reconcileInterval,
		ReconcileTimeout:         reconcileTimeout,
		MaxConcurrentReconciles:  maxConcurrentReconciles,
	}
	if err := reconciler.SetupWithManager(manager); err != nil {
		return fmt.Errorf("register ApplicationProfile controller: %w", err)
	}
	if err := manager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("register health check: %w", err)
	}
	if err := manager.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("register readiness check: %w", err)
	}
	ctrl.Log.WithName("setup").Info("starting ApplicationProfile controller",
		"watchNamespace", watchNamespace,
		"reconcileInterval", reconcileInterval,
		"reconcileTimeout", reconcileTimeout,
		"stateRetention", stateRetention,
		"leaderElection", leaderElect,
		"maxConcurrentReconciles", maxConcurrentReconciles,
	)
	if err := manager.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("run controller manager: %w", err)
	}
	return nil
}
