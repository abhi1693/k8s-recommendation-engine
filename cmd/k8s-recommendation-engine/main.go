package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/abhi1693/k8s-recommendation-engine/internal/analyzer"
	"github.com/abhi1693/k8s-recommendation-engine/internal/backtest"
	"github.com/abhi1693/k8s-recommendation-engine/internal/config"
	"github.com/abhi1693/k8s-recommendation-engine/internal/kube"
	"github.com/abhi1693/k8s-recommendation-engine/internal/prom"
	"github.com/abhi1693/k8s-recommendation-engine/internal/state"
)

func main() {
	if err := run(); err != nil {
		slog.Error("command failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return usage(errors.New("missing command"))
	}

	switch os.Args[1] {
	case "analyze":
		return runAnalyze(os.Args[2:])
	case "run":
		return runContinuous(os.Args[2:])
	case "backtest":
		return runBacktest(os.Args[2:])
	case "proposal":
		return runProposal(os.Args[2:])
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return usage(fmt.Errorf("unknown command %q", os.Args[1]))
	}
}

type proposalOptions struct {
	gitWorktree            string
	baseBranch             string
	branch                 string
	remote                 string
	push                   bool
	allowDefaultBranchPush bool
	timeout                time.Duration
}

func runProposal(args []string) error {
	if len(args) < 1 {
		return usage(errors.New("missing proposal subcommand"))
	}
	switch args[0] {
	case "status":
		return runProposalStatus(args[1:])
	case "diff":
		return runProposalDiff(args[1:])
	case "revert":
		return runProposalRevert(args[1:])
	case "rollback":
		return runProposalRollback(args[1:])
	case "observe":
		return runProposalObserve(args[1:])
	default:
		return usage(fmt.Errorf("unknown proposal subcommand %q", args[0]))
	}
}

func addProposalFlags(fs *flag.FlagSet) *proposalOptions {
	options := &proposalOptions{}
	fs.StringVar(&options.gitWorktree, "git-worktree", "", "local Fleet Git worktree")
	fs.StringVar(&options.baseBranch, "base", "master", "base branch used for proposal comparison")
	fs.StringVar(&options.branch, "branch", "", "proposal branch used for diff; defaults to current proposal branch or the only local k8s-recommendation-engine/* branch")
	fs.StringVar(&options.remote, "remote", "origin", "Git remote used when --push is set")
	fs.BoolVar(&options.push, "push", false, "push the resulting proposal lifecycle commit")
	fs.BoolVar(&options.allowDefaultBranchPush, "allow-default-branch-push", false, "allow --push when the target branch is the configured/default branch")
	fs.DurationVar(&options.timeout, "timeout", 30*time.Second, "Git operation timeout")
	return options
}

func runProposalStatus(args []string) error {
	fs := flag.NewFlagSet("proposal status", flag.ExitOnError)
	options := addProposalFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), options.timeout)
	defer cancel()
	status, err := analyzer.ProposalStatus(ctx, options.gitWorktree, options.baseBranch)
	if err != nil {
		return err
	}
	return analyzer.WriteProposalStatus(os.Stdout, status)
}

func runProposalDiff(args []string) error {
	fs := flag.NewFlagSet("proposal diff", flag.ExitOnError)
	options := addProposalFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), options.timeout)
	defer cancel()
	diff, err := analyzer.ProposalDiff(ctx, options.gitWorktree, options.baseBranch, options.branch)
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(os.Stdout, diff)
	return err
}

func runProposalRevert(args []string) error {
	fs := flag.NewFlagSet("proposal revert", flag.ExitOnError)
	options := addProposalFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), options.timeout)
	defer cancel()
	output, err := analyzer.ProposalRevert(ctx, options.gitWorktree)
	if output != "" {
		if _, writeErr := fmt.Fprintln(os.Stdout, strings.TrimSpace(output)); writeErr != nil {
			return writeErr
		}
	}
	return err
}

func runProposalRollback(args []string) error {
	fs := flag.NewFlagSet("proposal rollback", flag.ExitOnError)
	options := addProposalFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), options.timeout)
	defer cancel()
	report := analyzer.ProposalRollback(ctx, options.gitWorktree, analyzer.RollbackOptions{
		Branch:                 options.branch,
		DefaultBranch:          options.baseBranch,
		Remote:                 options.remote,
		Push:                   options.push,
		AllowDefaultBranchPush: options.allowDefaultBranchPush,
	})
	return analyzer.WriteProposalResult(os.Stdout, report)
}

func runProposalObserve(args []string) error {
	fs := flag.NewFlagSet("proposal observe", flag.ExitOnError)
	options := addCommonFlags(fs)
	baseBranch := fs.String("base", "", "base branch used for Git comparison; defaults to spec.git.branch")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), options.timeout)
	defer cancel()

	profile, err := config.LoadFile(options.configPath)
	if err != nil {
		return err
	}
	kubeClient, err := kube.NewClient(options.kubeconfig, options.contextName)
	if err != nil {
		return err
	}
	promClient := prom.NewClient(options.promURL, nil)
	report, err := analyzer.New(kubeClient, promClient, analyzer.Options{
		HistoryWindow: options.historyWindow,
		HistoryStep:   options.historyStep,
	}).Analyze(ctx, profile)
	if err != nil {
		return err
	}
	setReportRecommendationMode(report, options.mode)
	if err := state.AttachAndRecord(ctx, options.stateDB, report); err != nil {
		return err
	}
	observation := analyzer.ObserveConvergence(ctx, options.gitWorktree, *baseBranch, profile, report)
	if err := state.RecordObservation(ctx, options.stateDB, observation); err != nil {
		return err
	}
	if options.output == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(observation)
	}
	return analyzer.WriteObservationReport(os.Stdout, observation)
}

type commandOptions struct {
	configPath             string
	promURL                string
	kubeconfig             string
	contextName            string
	output                 string
	timeout                time.Duration
	historyWindow          time.Duration
	historyStep            time.Duration
	gitWorktree            string
	stateDB                string
	mode                   string
	proposalKind           string
	proposalDir            string
	proposalBranch         string
	proposalRemote         string
	proposalPush           bool
	allowDefaultBranchPush bool
}

func runAnalyze(args []string) error {
	fs := flag.NewFlagSet("analyze", flag.ExitOnError)
	options := addCommonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), options.timeout)
	defer cancel()
	return executeAnalyze(ctx, options, os.Stdout)
}

func runBacktest(args []string) error {
	fs := flag.NewFlagSet("backtest", flag.ExitOnError)
	options := &commandOptions{}
	fs.StringVar(&options.configPath, "config", "configs/shipyard-profile.yaml", "application profile YAML")
	fs.StringVar(&options.promURL, "prometheus-url", "http://127.0.0.1:9090", "Prometheus base URL")
	fs.StringVar(&options.output, "output", "text", "output format: text, summary, pretty, or json")
	fs.DurationVar(&options.timeout, "timeout", 30*time.Second, "backtest timeout")
	window := durationFlag{value: 7 * 24 * time.Hour}
	step := durationFlag{value: 5 * time.Minute}
	forecastHorizon := durationFlag{value: 30 * time.Minute}
	fs.Var(&window, "window", "Prometheus replay window; accepts Go durations plus d, for example 7d")
	fs.Var(&step, "step", "Prometheus query_range step for replay; accepts Go durations plus d")
	fs.Var(&forecastHorizon, "forecast-horizon", "lookahead horizon used by predictive replay; accepts Go durations plus d")
	stabilityRuns := fs.Int("stability-runs", 3, "stable replay points required before counting a Git proposal")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if window.value <= 0 {
		return fmt.Errorf("window must be greater than zero")
	}
	if step.value <= 0 {
		return fmt.Errorf("step must be greater than zero")
	}
	if forecastHorizon.value <= 0 {
		return fmt.Errorf("forecast-horizon must be greater than zero")
	}
	if *stabilityRuns <= 0 {
		return fmt.Errorf("stability-runs must be greater than zero")
	}

	ctx, cancel := context.WithTimeout(context.Background(), options.timeout)
	defer cancel()

	profile, err := config.LoadFile(options.configPath)
	if err != nil {
		return err
	}
	promClient := prom.NewClient(options.promURL, nil)
	report, err := backtest.Run(ctx, promClient, profile, backtest.Options{
		Window:          window.value,
		Step:            step.value,
		ForecastHorizon: forecastHorizon.value,
		StabilityRuns:   *stabilityRuns,
	})
	if err != nil {
		return err
	}

	switch options.output {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	case "text", "pretty", "summary":
		return backtest.WriteTextReport(os.Stdout, report)
	default:
		return fmt.Errorf("unsupported output format %q", options.output)
	}
}

type durationFlag struct {
	value time.Duration
}

func (d *durationFlag) Set(value string) error {
	parsed, err := parseDurationWithDays(value)
	if err != nil {
		return err
	}
	d.value = parsed
	return nil
}

func (d durationFlag) String() string {
	return d.value.String()
}

func parseDurationWithDays(value string) (time.Duration, error) {
	if strings.HasSuffix(value, "d") {
		days, err := time.ParseDuration(strings.TrimSuffix(value, "d") + "h")
		if err != nil {
			return 0, err
		}
		return days * 24, nil
	}
	return time.ParseDuration(value)
}

func runContinuous(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	options := addCommonFlags(fs)
	interval := fs.Duration("interval", 5*time.Minute, "reconcile interval")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *interval <= 0 {
		return fmt.Errorf("interval must be greater than zero")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("starting recommendation controller loop", "interval", interval.String(), "mode", options.mode)
	for {
		started := time.Now().UTC()
		if _, err := fmt.Fprintf(os.Stdout, "\n--- reconcile %s ---\n", started.Format("2006-01-02T15:04:05Z")); err != nil {
			return err
		}
		cycleCtx, cancel := context.WithTimeout(ctx, options.timeout)
		err := executeAnalyze(cycleCtx, options, os.Stdout)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Error("reconcile failed", "error", err)
		}

		timer := time.NewTimer(*interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func addCommonFlags(fs *flag.FlagSet) *commandOptions {
	options := &commandOptions{}
	fs.StringVar(&options.configPath, "config", "configs/shipyard-profile.yaml", "application profile YAML")
	fs.StringVar(&options.promURL, "prometheus-url", "http://127.0.0.1:9090", "Prometheus base URL")
	fs.StringVar(&options.kubeconfig, "kubeconfig", defaultKubeconfig(), "path to kubeconfig")
	fs.StringVar(&options.contextName, "context", "", "kubeconfig context override")
	fs.StringVar(&options.output, "output", "text", "output format: text, pretty, summary, actions, or json")
	fs.DurationVar(&options.timeout, "timeout", 30*time.Second, "analysis timeout per reconcile")
	fs.DurationVar(&options.historyWindow, "history-window", 24*time.Hour, "Prometheus history window used for recommendation stats")
	fs.DurationVar(&options.historyStep, "history-step", 5*time.Minute, "Prometheus query_range step used for recommendation stats")
	fs.StringVar(&options.gitWorktree, "git-worktree", "", "local Fleet Git worktree used for dry-run patch planning")
	fs.StringVar(&options.stateDB, "state-db", "", "SQLite state database used for persistent learning")
	fs.StringVar(&options.mode, "mode", "dry-run", "operation mode: dry-run or propose")
	fs.StringVar(&options.proposalKind, "proposal-kind", "patch", "proposal artifact type when --mode propose: patch or commit")
	fs.StringVar(&options.proposalDir, "proposal-dir", ".k8s-recommendation-engine/proposals", "relative Git worktree directory for proposal patch files")
	fs.StringVar(&options.proposalBranch, "proposal-branch", "", "commit target branch: explicit repo default branch or k8s-recommendation-engine/* proposal branch")
	fs.StringVar(&options.proposalRemote, "proposal-remote", "origin", "Git remote used when --proposal-push is set")
	fs.BoolVar(&options.proposalPush, "proposal-push", false, "push proposal commit to the configured remote")
	fs.BoolVar(&options.allowDefaultBranchPush, "allow-default-branch-push", false, "allow --proposal-push when the proposal branch is the configured default branch")
	return options
}

func executeAnalyze(ctx context.Context, options *commandOptions, outputFile *os.File) error {
	profile, err := config.LoadFile(options.configPath)
	if err != nil {
		return err
	}

	kubeClient, err := kube.NewClient(options.kubeconfig, options.contextName)
	if err != nil {
		return err
	}

	promClient := prom.NewClient(options.promURL, nil)
	report, err := analyzer.New(kubeClient, promClient, analyzer.Options{
		HistoryWindow: options.historyWindow,
		HistoryStep:   options.historyStep,
	}).Analyze(ctx, profile)
	if err != nil {
		return err
	}
	setReportRecommendationMode(report, options.mode)
	if err := state.AttachAndRecord(ctx, options.stateDB, report); err != nil {
		return err
	}
	analyzer.AttachPatchPlans(options.gitWorktree, profile, report)
	if err := state.ApplyProposalBudgets(ctx, options.stateDB, report, profile); err != nil {
		return err
	}
	if err := attachProposal(ctx, options, profile, report); err != nil {
		return err
	}
	if err := state.RecordProposalEvents(ctx, options.stateDB, report); err != nil {
		return err
	}

	switch options.output {
	case "json":
		enc := json.NewEncoder(outputFile)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	case "pretty":
		return analyzer.WritePrettyReport(outputFile, report)
	case "summary":
		return analyzer.WriteSummaryReport(outputFile, report)
	case "actions":
		return analyzer.WriteActionsReport(outputFile, report, options.gitWorktree != "")
	case "text":
		return analyzer.WriteTextReport(outputFile, report)
	default:
		return fmt.Errorf("unsupported output format %q", options.output)
	}
}

func setReportRecommendationMode(report *analyzer.Report, mode string) {
	if report == nil {
		return
	}
	if mode == "" {
		mode = "dry-run"
	}
	for index := range report.Workloads {
		report.Workloads[index].Recommendation.Mode = mode
	}
}

func usage(err error) error {
	printUsage()
	return err
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  k8s-recommendation-engine analyze [flags]")
	fmt.Fprintln(os.Stderr, "  k8s-recommendation-engine run [flags]")
	fmt.Fprintln(os.Stderr, "  k8s-recommendation-engine backtest [flags]")
	fmt.Fprintln(os.Stderr, "  k8s-recommendation-engine proposal status [flags]")
	fmt.Fprintln(os.Stderr, "  k8s-recommendation-engine proposal diff [flags]")
	fmt.Fprintln(os.Stderr, "  k8s-recommendation-engine proposal revert [flags]")
	fmt.Fprintln(os.Stderr, "  k8s-recommendation-engine proposal rollback [flags]")
	fmt.Fprintln(os.Stderr, "  k8s-recommendation-engine proposal observe [flags]")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Examples:")
	fmt.Fprintln(os.Stderr, "  k8s-recommendation-engine analyze --config configs/shipyard-profile.yaml --prometheus-url http://127.0.0.1:9090")
	fmt.Fprintln(os.Stderr, "  k8s-recommendation-engine analyze --output pretty")
	fmt.Fprintln(os.Stderr, "  k8s-recommendation-engine analyze --state-db .state/k8s-recommendation-engine.db --output pretty")
	fmt.Fprintln(os.Stderr, "  k8s-recommendation-engine analyze --output json")
	fmt.Fprintln(os.Stderr, "  k8s-recommendation-engine analyze --state-db .state/k8s-recommendation-engine.db --git-worktree /path/to/fleet --output actions")
	fmt.Fprintln(os.Stderr, "  k8s-recommendation-engine analyze --mode propose --proposal-kind patch --state-db .state/k8s-recommendation-engine.db --git-worktree /path/to/fleet --output actions")
	fmt.Fprintln(os.Stderr, "  k8s-recommendation-engine analyze --mode propose --proposal-kind commit --state-db .state/k8s-recommendation-engine.db --git-worktree /path/to/fleet --output actions")
	fmt.Fprintln(os.Stderr, "  k8s-recommendation-engine analyze --mode propose --proposal-kind commit --proposal-branch master --proposal-push --allow-default-branch-push --state-db .state/k8s-recommendation-engine.db --git-worktree /path/to/fleet --output actions")
	fmt.Fprintln(os.Stderr, "  k8s-recommendation-engine backtest --config configs/shipyard-profile.yaml --prometheus-url http://127.0.0.1:9090 --window 7d")
	fmt.Fprintln(os.Stderr, "  k8s-recommendation-engine proposal status --git-worktree /path/to/fleet")
	fmt.Fprintln(os.Stderr, "  k8s-recommendation-engine proposal diff --git-worktree /path/to/fleet")
	fmt.Fprintln(os.Stderr, "  k8s-recommendation-engine proposal revert --git-worktree /path/to/fleet")
	fmt.Fprintln(os.Stderr, "  k8s-recommendation-engine proposal rollback --git-worktree /path/to/fleet --branch master --push --allow-default-branch-push")
	fmt.Fprintln(os.Stderr, "  k8s-recommendation-engine proposal observe --config configs/shipyard-profile.yaml --state-db .state/k8s-recommendation-engine.db --git-worktree /path/to/fleet")
	fmt.Fprintln(os.Stderr, "  k8s-recommendation-engine run --interval 5m --state-db .state/k8s-recommendation-engine.db --output summary")
}

func attachProposal(ctx context.Context, options *commandOptions, profile *config.ApplicationProfile, report *analyzer.Report) error {
	switch options.mode {
	case "", "dry-run":
		return nil
	case "propose":
		report.Proposal = analyzer.CreateProposal(ctx, options.gitWorktree, report, analyzer.ProposalOptions{
			Kind:                   options.proposalKind,
			PatchDir:               options.proposalDir,
			BranchName:             options.proposalBranch,
			DefaultBranch:          profile.Spec.Git.Branch,
			Remote:                 options.proposalRemote,
			Push:                   options.proposalPush,
			AllowDefaultBranchPush: options.allowDefaultBranchPush,
			GeneratedAt:            report.GeneratedAt,
		})
		return nil
	default:
		return fmt.Errorf("unsupported mode %q", options.mode)
	}
}

func defaultKubeconfig() string {
	if value := os.Getenv("KUBECONFIG"); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".kube", "config")
}
