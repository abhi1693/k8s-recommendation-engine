package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/abhi1693/k8s-recommendation-engine/internal/analyzer"
	"github.com/abhi1693/k8s-recommendation-engine/internal/config"
	"k8s.io/apimachinery/pkg/api/resource"
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

func ApplyProposalBatch(ctx context.Context, path string, report *analyzer.Report, proposalKind string, batchWindow time.Duration) error {
	if report == nil || proposalKind != "commit" || batchWindow <= 0 {
		return nil
	}
	if path == "" {
		if !reportHasNoApplyablePlans(report) {
			blockApplyablePlans(report, "proposal batch window requires --state-db for persisted grouping")
		}
		return nil
	}
	store, err := Open(path)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.ApplyProposalBatch(ctx, report, batchWindow)
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

func (s *Store) ApplyProposalBatch(ctx context.Context, report *analyzer.Report, batchWindow time.Duration) error {
	if report == nil || batchWindow <= 0 {
		return nil
	}
	generatedAt := report.GeneratedAt
	if generatedAt.IsZero() {
		generatedAt = time.Now().UTC()
	}
	for index := range report.Workloads {
		workload := &report.Workloads[index]
		plan := workload.Recommendation.PatchPlan
		if proposalEventPlan(plan) {
			if urgentBatchBypass(report.SharedSignals, workload, plan) {
				plan.BlockReasons = append(plan.BlockReasons, "proposal batch bypassed for urgent traffic anomaly")
				continue
			}
			firstSeen, err := s.upsertProposalBatchItem(ctx, report.Application, workload, plan, generatedAt)
			if err != nil {
				return err
			}
			readyAt := firstSeen.Add(batchWindow)
			if generatedAt.Before(readyAt) {
				plan.Blocked = true
				plan.Needed = false
				plan.BlockReasons = append(plan.BlockReasons, fmt.Sprintf("proposal batch window open: first_seen=%s ready_at=%s window=%s", firstSeen.Format(time.RFC3339), readyAt.Format(time.RFC3339), batchWindow))
			}
			continue
		}
		if err := s.deleteProposalBatchItem(ctx, report.Application, workload.Namespace, workload.Name); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) upsertProposalBatchItem(ctx context.Context, application string, workload *analyzer.WorkloadReport, plan *analyzer.PatchPlan, generatedAt time.Time) (time.Time, error) {
	var firstSeenRaw, existingPlanJSON string
	err := s.db.QueryRowContext(ctx, `
		SELECT first_seen_at, patch_plan_json
		FROM proposal_batch_items
		WHERE application = ?
			AND namespace = ?
			AND workload_name = ?
	`, application, workload.Namespace, workload.Name).Scan(&firstSeenRaw, &existingPlanJSON)
	switch {
	case err == nil:
		encoded, marshalErr := json.Marshal(plan)
		if marshalErr != nil {
			return time.Time{}, fmt.Errorf("encode proposal batch item: %w", marshalErr)
		}
		firstSeen, parseErr := time.Parse(time.RFC3339Nano, firstSeenRaw)
		if parseErr != nil {
			return time.Time{}, fmt.Errorf("parse proposal batch first_seen_at: %w", parseErr)
		}
		if existingPlanJSON != string(encoded) {
			firstSeen = generatedAt
		}
		if _, updateErr := s.db.ExecContext(ctx, `
			UPDATE proposal_batch_items
			SET deployment = ?,
				source_file = ?,
				resource = ?,
				patch_plan_json = ?,
				first_seen_at = ?,
				last_seen_at = ?
			WHERE application = ?
				AND namespace = ?
				AND workload_name = ?
		`,
			workload.Deployment,
			plan.SourceFile,
			plan.Resource,
			string(encoded),
			firstSeen.Format(time.RFC3339Nano),
			generatedAt.Format(time.RFC3339Nano),
			application,
			workload.Namespace,
			workload.Name,
		); updateErr != nil {
			return time.Time{}, fmt.Errorf("update proposal batch item: %w", updateErr)
		}
		return firstSeen, nil
	case err != nil && err != sql.ErrNoRows:
		return time.Time{}, fmt.Errorf("lookup proposal batch item: %w", err)
	}

	encoded, err := json.Marshal(plan)
	if err != nil {
		return time.Time{}, fmt.Errorf("encode proposal batch item: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO proposal_batch_items (
			application,
			namespace,
			workload_name,
			deployment,
			source_file,
			resource,
			patch_plan_json,
			first_seen_at,
			last_seen_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		application,
		workload.Namespace,
		workload.Name,
		workload.Deployment,
		plan.SourceFile,
		plan.Resource,
		string(encoded),
		generatedAt.Format(time.RFC3339Nano),
		generatedAt.Format(time.RFC3339Nano),
	); err != nil {
		return time.Time{}, fmt.Errorf("insert proposal batch item: %w", err)
	}
	return generatedAt, nil
}

func (s *Store) deleteProposalBatchItem(ctx context.Context, application, namespace, workload string) error {
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM proposal_batch_items
		WHERE application = ?
			AND namespace = ?
			AND workload_name = ?
	`, application, namespace, workload); err != nil {
		return fmt.Errorf("delete proposal batch item: %w", err)
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
		if err := s.deleteProposalBatchItem(ctx, report.Application, workload.Namespace, workload.Name); err != nil {
			return err
		}
	}
	return nil
}

func proposalEventPlan(plan *analyzer.PatchPlan) bool {
	return plan != nil && plan.Needed && !plan.Blocked && len(plan.Errors) == 0 && len(plan.Changes) > 0
}

func reportHasNoApplyablePlans(report *analyzer.Report) bool {
	for _, workload := range report.Workloads {
		if proposalEventPlan(workload.Recommendation.PatchPlan) {
			return false
		}
	}
	return true
}

func blockApplyablePlans(report *analyzer.Report, reason string) {
	for index := range report.Workloads {
		plan := report.Workloads[index].Recommendation.PatchPlan
		if !proposalEventPlan(plan) {
			continue
		}
		plan.Blocked = true
		plan.Needed = false
		plan.BlockReasons = append(plan.BlockReasons, reason)
	}
}

func urgentBatchBypass(sharedSignals []analyzer.SignalReport, workload *analyzer.WorkloadReport, plan *analyzer.PatchPlan) bool {
	if workload == nil || plan == nil || !hasTrafficAnomaly(workload.MetricSignals, sharedSignals) {
		return false
	}
	for _, change := range plan.Changes {
		if change.Current == change.Recommended {
			continue
		}
		if change.Field == "spec.replicas" && numericIncrease(change.Current, change.Recommended) {
			return true
		}
		if strings.HasSuffix(change.Field, ".resources.requests.cpu") && quantityIncrease(change.Current, change.Recommended) {
			return true
		}
		if strings.HasSuffix(change.Field, ".resources.requests.memory") && quantityIncrease(change.Current, change.Recommended) {
			return true
		}
	}
	return false
}

func numericIncrease(current, recommended string) bool {
	currentValue, err := strconv.ParseFloat(current, 64)
	if err != nil {
		return false
	}
	recommendedValue, err := strconv.ParseFloat(recommended, 64)
	if err != nil {
		return false
	}
	return recommendedValue > currentValue
}

func quantityIncrease(current, recommended string) bool {
	currentQuantity, err := resource.ParseQuantity(current)
	if err != nil {
		return false
	}
	recommendedQuantity, err := resource.ParseQuantity(recommended)
	if err != nil {
		return false
	}
	return recommendedQuantity.AsApproximateFloat64() > currentQuantity.AsApproximateFloat64()
}

func hasTrafficAnomaly(workloadSignals, sharedSignals []analyzer.SignalReport) bool {
	for _, signal := range append(append([]analyzer.SignalReport(nil), workloadSignals...), sharedSignals...) {
		if !trafficBatchSignal(signal.Name) {
			continue
		}
		if signal.Anomaly.State == "warning" || signal.Anomaly.State == "critical" {
			return true
		}
	}
	return false
}

func trafficBatchSignal(name string) bool {
	switch name {
	case "request_rate", "latency_p95", "error_rate", "concurrent_requests":
		return true
	default:
		return false
	}
}
