package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/abhi1693/k8s-recommendation-engine/internal/analyzer"
	"github.com/abhi1693/k8s-recommendation-engine/internal/config"
	"github.com/abhi1693/k8s-recommendation-engine/internal/kube"
	"github.com/abhi1693/k8s-recommendation-engine/internal/prom"
	"github.com/abhi1693/k8s-recommendation-engine/internal/recovery"
	"github.com/abhi1693/k8s-recommendation-engine/internal/state"
)

type ProfileProcessor interface {
	Process(context.Context, *config.ApplicationProfile) (*analyzer.Report, error)
}

type AnalyzerProcessor struct {
	Kube                   *kube.Client
	Prometheus             *prom.Client
	StateDB                string
	HistoryWindow          time.Duration
	HistoryStep            time.Duration
	AvailabilityRecovery   bool
	Mode                   string
	GitWorktree            string
	ProposalKind           string
	ProposalDir            string
	ProposalBranch         string
	ProposalRemote         string
	ProposalPush           bool
	ProposalBatchWindow    time.Duration
	AllowDefaultBranchPush bool
}

func (p *AnalyzerProcessor) Process(ctx context.Context, profile *config.ApplicationProfile) (*analyzer.Report, error) {
	if p == nil || p.Kube == nil || p.Prometheus == nil {
		return nil, fmt.Errorf("analyzer processor dependencies are not configured")
	}
	report, err := analyzer.New(p.Kube, p.Prometheus, analyzer.Options{
		HistoryWindow: p.HistoryWindow,
		HistoryStep:   p.HistoryStep,
	}).Analyze(ctx, profile)
	if err != nil {
		return nil, err
	}
	for index := range report.Workloads {
		report.Workloads[index].Recommendation.Mode = p.mode()
	}
	if err := state.AttachAndRecord(ctx, p.StateDB, report); err != nil {
		return nil, err
	}
	analyzer.AttachSafetyAssessmentsWithPolicy(report, profile)
	if err := recovery.Apply(ctx, p.Kube, profile, report, recovery.Options{
		Enabled: p.AvailabilityRecovery,
		StateDB: p.StateDB,
	}); err != nil {
		return nil, err
	}
	analyzer.AttachPatchPlans(p.GitWorktree, profile, report)
	if err := state.ApplyProposalBudgets(ctx, p.StateDB, report, profile); err != nil {
		return nil, err
	}
	if err := state.ApplyProposalBatch(ctx, p.StateDB, report, profile, p.ProposalKind, p.ProposalBatchWindow); err != nil {
		return nil, err
	}
	switch p.mode() {
	case "dry-run":
	case "propose":
		report.Proposal = analyzer.CreateProposal(ctx, p.GitWorktree, report, analyzer.ProposalOptions{
			Kind:                   p.ProposalKind,
			PatchDir:               p.ProposalDir,
			BranchName:             p.ProposalBranch,
			DefaultBranch:          profile.Spec.Git.Branch,
			Remote:                 p.ProposalRemote,
			Push:                   p.ProposalPush,
			AllowDefaultBranchPush: p.AllowDefaultBranchPush,
			GeneratedAt:            report.GeneratedAt,
		})
	default:
		return nil, fmt.Errorf("unsupported controller mode %q", p.Mode)
	}
	if p.GitWorktree != "" {
		branch := strings.TrimSpace(p.ProposalBranch)
		if branch == "" {
			branch = strings.TrimSpace(profile.Spec.Git.Branch)
		}
		report.GitHealth = analyzer.InspectGitHealth(ctx, p.GitWorktree, analyzer.GitHealthOptions{
			Branch:      branch,
			Remote:      p.ProposalRemote,
			PushEnabled: p.ProposalPush,
		})
	}
	if err := state.RecordProposalEvents(ctx, p.StateDB, report); err != nil {
		return nil, err
	}
	return report, nil
}

func (p *AnalyzerProcessor) mode() string {
	if p.Mode == "" {
		return "dry-run"
	}
	return p.Mode
}
