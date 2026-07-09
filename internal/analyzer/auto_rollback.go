package analyzer

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/abhi1693/k8s-recommendation-engine/internal/config"
)

func AutoRollback(ctx context.Context, worktree string, profile *config.ApplicationProfile, analysis *Report, observation *ObservationReport, options RollbackOptions) *AutoRollbackReport {
	report := &AutoRollbackReport{
		Observation: observation,
	}
	if analysis != nil {
		report.Application = analysis.Application
		report.Namespace = analysis.Namespace
		report.GeneratedAt = analysis.GeneratedAt
	}
	if observation != nil {
		if report.Application == "" {
			report.Application = observation.Application
		}
		if report.Namespace == "" {
			report.Namespace = observation.Namespace
		}
		if report.GeneratedAt.IsZero() {
			report.GeneratedAt = observation.GeneratedAt
		}
	}
	if observation == nil {
		report.Blocked = true
		report.Reasons = append(report.Reasons, "observation is required before auto-rollback")
		return report
	}
	if profile == nil {
		report.Blocked = true
		report.Reasons = append(report.Reasons, "profile is required before auto-rollback")
		return report
	}

	reasons := AutoRollbackReasons(observation)
	report.Reasons = append(report.Reasons, reasons...)
	if len(reasons) == 0 {
		report.Reasons = append(report.Reasons, "no applied regressed or unsafe workload was observed")
		return report
	}
	report.Needed = true

	if observation.Git.LatestProposalCommit == "" {
		report.Blocked = true
		report.Reasons = append(report.Reasons, "latest k8s-recommendation-engine proposal commit was not found")
		return report
	}
	if len(observation.Git.DirtyLines) > 0 {
		report.Blocked = true
		report.Reasons = append(report.Reasons, "git worktree is dirty; refusing automatic rollback: "+strings.Join(observation.Git.DirtyLines, "; "))
		return report
	}
	if len(observation.Errors) > 0 {
		report.Blocked = true
		report.Errors = append(report.Errors, observation.Errors...)
		return report
	}

	if options.DefaultBranch == "" {
		options.DefaultBranch = profile.Spec.Git.Branch
	}
	if options.Branch == "" {
		options.Branch = profile.Spec.Git.Branch
	}
	rollback := ProposalRollback(ctx, worktree, options)
	report.Rollback = rollback
	if rollback.Blocked || len(rollback.Errors) > 0 {
		report.Blocked = true
		report.Reasons = append(report.Reasons, rollback.BlockReasons...)
		report.Errors = append(report.Errors, rollback.Errors...)
		return report
	}
	return report
}

func AutoRollbackReasons(observation *ObservationReport) []string {
	if observation == nil {
		return nil
	}
	var reasons []string
	for _, workload := range observation.Workloads {
		if workload.Status != "applied" {
			continue
		}
		switch workload.Outcome {
		case "unsafe", "regressed":
			reason := fmt.Sprintf("%s/%s applied outcome=%s metrics=%s", workload.Namespace, workload.Deployment, workload.Outcome, emptyDash(workload.MetricsCondition))
			if len(workload.Reasons) > 0 {
				reason += " reasons=" + strings.Join(workload.Reasons, ",")
			}
			reasons = append(reasons, reason)
		}
	}
	return reasons
}

func WriteAutoRollbackReport(w io.Writer, report *AutoRollbackReport) error {
	if report == nil {
		return fmt.Errorf("auto rollback report is required")
	}
	if _, err := fmt.Fprintln(w, "K8s Recommendation Engine Auto Rollback"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Application: %s   Namespace: %s   Generated: %s\n", report.Application, report.Namespace, report.GeneratedAt.Format("2006-01-02T15:04:05Z")); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Decision: needed=%t blocked=%t\n", report.Needed, report.Blocked); err != nil {
		return err
	}
	for _, reason := range report.Reasons {
		if _, err := fmt.Fprintf(w, "Reason: %s\n", reason); err != nil {
			return err
		}
	}
	for _, rollbackError := range report.Errors {
		if _, err := fmt.Fprintf(w, "Error: %s\n", rollbackError); err != nil {
			return err
		}
	}
	if report.Rollback != nil {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		if err := WriteProposalResult(w, report.Rollback); err != nil {
			return err
		}
	}
	if report.Observation != nil {
		if _, err := fmt.Fprintf(w, "\nObservation: applied=%d pending=%d drifted=%d failed=%d regressed=%d unsafe=%d latest=%s\n",
			report.Observation.Summary.Applied,
			report.Observation.Summary.Pending,
			report.Observation.Summary.Drifted,
			report.Observation.Summary.Failed,
			report.Observation.Summary.Regressed,
			report.Observation.Summary.Unsafe,
			emptyDash(report.Observation.Git.LatestProposalCommit),
		); err != nil {
			return err
		}
	}
	return nil
}
