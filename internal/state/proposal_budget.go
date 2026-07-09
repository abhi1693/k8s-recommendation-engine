package state

import (
	"context"
	"fmt"
	"time"

	"github.com/abhi1693/k8s-recommendation-engine/internal/analyzer"
	"github.com/abhi1693/k8s-recommendation-engine/internal/config"
)

func ApplyProposalBudgets(ctx context.Context, path string, report *analyzer.Report, profile *config.ApplicationProfile) error {
	if path == "" || report == nil || profile == nil {
		return nil
	}
	store, err := Open(path)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.ApplyProposalBudgets(ctx, report, profile)
}

func RecordProposalEvents(ctx context.Context, path string, report *analyzer.Report) error {
	if path == "" || report == nil || report.Proposal == nil {
		return nil
	}
	store, err := Open(path)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.RecordProposalEvents(ctx, report)
}

func (s *Store) ApplyProposalBudgets(ctx context.Context, report *analyzer.Report, profile *config.ApplicationProfile) error {
	workloads := make(map[string]config.WorkloadSpec, len(profile.Spec.Workloads))
	for _, workload := range profile.Spec.Workloads {
		workloads[workload.Name] = workload
	}
	for index := range report.Workloads {
		workload := &report.Workloads[index]
		if workload.Recommendation.Mode != "propose" {
			continue
		}
		plan := workload.Recommendation.PatchPlan
		if plan == nil || plan.Blocked || len(plan.Errors) > 0 || len(plan.Changes) == 0 {
			continue
		}
		spec, ok := workloads[workload.Name]
		if !ok {
			continue
		}
		reasons, err := s.proposalBudgetBlockReasons(ctx, report.Application, workload.Namespace, workload.Name, report.GeneratedAt, spec.Policy)
		if err != nil {
			return err
		}
		if len(reasons) == 0 {
			continue
		}
		plan.Blocked = true
		plan.Needed = false
		plan.BlockReasons = append(plan.BlockReasons, reasons...)
	}
	return nil
}

func (s *Store) proposalBudgetBlockReasons(ctx context.Context, application, namespace, workload string, generatedAt time.Time, policy config.PolicySpec) ([]string, error) {
	var reasons []string
	if policy.MaxProposalsPerHour > 0 {
		count, err := s.countProposalEventsSince(ctx, application, namespace, workload, generatedAt.Add(-time.Hour))
		if err != nil {
			return nil, err
		}
		if count >= policy.MaxProposalsPerHour {
			reasons = append(reasons, fmt.Sprintf("proposal budget exhausted: %d/%d in last 1h", count, policy.MaxProposalsPerHour))
		}
	}
	if policy.MaxProposalsPerDay > 0 {
		count, err := s.countProposalEventsSince(ctx, application, namespace, workload, generatedAt.Add(-24*time.Hour))
		if err != nil {
			return nil, err
		}
		if count >= policy.MaxProposalsPerDay {
			reasons = append(reasons, fmt.Sprintf("proposal budget exhausted: %d/%d in last 24h", count, policy.MaxProposalsPerDay))
		}
	}
	return reasons, nil
}

func (s *Store) countProposalEventsSince(ctx context.Context, application, namespace, workload string, since time.Time) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM proposal_events
		WHERE application = ?
			AND namespace = ?
			AND workload_name = ?
			AND generated_at >= ?
	`, application, namespace, workload, since.Format(time.RFC3339Nano)).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count proposal events: %w", err)
	}
	return count, nil
}

func (s *Store) RecordProposalEvents(ctx context.Context, report *analyzer.Report) error {
	if report.Proposal == nil || !report.Proposal.Needed || report.Proposal.Blocked || len(report.Proposal.Errors) > 0 {
		return nil
	}
	for _, workload := range report.Workloads {
		plan := workload.Recommendation.PatchPlan
		if !proposalEventPlan(plan) {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO proposal_events (
				application,
				namespace,
				workload_name,
				deployment,
				generated_at,
				proposal_kind,
				proposal_commit,
				proposal_patch_file,
				changes_count
			)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			report.Application,
			workload.Namespace,
			workload.Name,
			workload.Deployment,
			report.GeneratedAt.Format(time.RFC3339Nano),
			report.Proposal.Kind,
			report.Proposal.Commit,
			report.Proposal.PatchFile,
			len(plan.Changes),
		); err != nil {
			return fmt.Errorf("record proposal event: %w", err)
		}
	}
	return nil
}

func proposalEventPlan(plan *analyzer.PatchPlan) bool {
	return plan != nil && plan.Needed && !plan.Blocked && len(plan.Errors) == 0 && len(plan.Changes) > 0
}
