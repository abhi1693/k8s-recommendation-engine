package state

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abhi1693/k8s-recommendation-engine/internal/analyzer"
	"github.com/abhi1693/k8s-recommendation-engine/internal/config"
)

func TestAttachAndRecordPersistsPriorLearning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first := testReport()
	if err := AttachAndRecord(context.Background(), path, first); err != nil {
		t.Fatal(err)
	}
	if first.Workloads[0].Recommendation.Learning.Persistent == nil {
		t.Fatal("first persistent summary is nil")
	}
	if first.Workloads[0].Recommendation.Learning.Mode != "prometheus-history+sqlite-state" {
		t.Fatalf("first learning mode = %q", first.Workloads[0].Recommendation.Learning.Mode)
	}
	if first.Workloads[0].Recommendation.Learning.Persistent.PriorRecommendationRuns != 0 {
		t.Fatalf("first prior runs = %d, want 0", first.Workloads[0].Recommendation.Learning.Persistent.PriorRecommendationRuns)
	}

	second := testReport()
	if err := AttachAndRecord(context.Background(), path, second); err != nil {
		t.Fatal(err)
	}
	persistent := second.Workloads[0].Recommendation.Learning.Persistent
	if persistent == nil {
		t.Fatal("second persistent summary is nil")
	}
	if persistent.PriorRecommendationRuns != 1 {
		t.Fatalf("second prior runs = %d, want 1", persistent.PriorRecommendationRuns)
	}
	if persistent.PriorSignalObservations != 1 {
		t.Fatalf("second prior signals = %d, want 1", persistent.PriorSignalObservations)
	}
	if persistent.LastRecommendedReplicas != 2 {
		t.Fatalf("LastRecommendedReplicas = %d, want 2", persistent.LastRecommendedReplicas)
	}
	if persistent.LastOutcome == nil {
		t.Fatal("second last outcome is nil")
	}
	if persistent.LastOutcome.Status != "no_action_taken" {
		t.Fatalf("LastOutcome.Status = %q, want no_action_taken", persistent.LastOutcome.Status)
	}
}

func TestAttachAndRecordClassifiesProposeNotApplied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first := testReport()
	first.Workloads[0].Recommendation.Mode = "propose"
	first.Workloads[0].Recommendation.RecommendedCPURequest = "800m"
	if err := AttachAndRecord(context.Background(), path, first); err != nil {
		t.Fatal(err)
	}

	second := testReport()
	second.Workloads[0].Recommendation.Mode = "propose"
	second.Workloads[0].Recommendation.RecommendedCPURequest = "800m"
	if err := AttachAndRecord(context.Background(), path, second); err != nil {
		t.Fatal(err)
	}

	persistent := second.Workloads[0].Recommendation.Learning.Persistent
	if persistent == nil || persistent.LastOutcome == nil {
		t.Fatal("second last outcome is nil")
	}
	if persistent.LastOutcome.Status != "not_applied" {
		t.Fatalf("LastOutcome.Status = %q, want not_applied", persistent.LastOutcome.Status)
	}
}

func TestAttachAndRecordDoesNotBlockProposeAfterDryRunHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first := testReport()
	first.Workloads[0].Recommendation.Mode = "dry-run"
	if err := AttachAndRecord(context.Background(), path, first); err != nil {
		t.Fatal(err)
	}

	second := testReport()
	second.Workloads[0].Recommendation.Mode = "propose"
	if err := AttachAndRecord(context.Background(), path, second); err != nil {
		t.Fatal(err)
	}

	stability := second.Workloads[0].Recommendation.Stability
	if stability == nil {
		t.Fatal("stability is nil")
	}
	if stability.CPU.Status == "blocked" {
		t.Fatalf("CPU gate should not be blocked by dry-run history after switching to propose: %#v", stability.CPU)
	}
	if second.Workloads[0].Recommendation.Learning.Persistent.LastOutcome == nil ||
		second.Workloads[0].Recommendation.Learning.Persistent.LastOutcome.Status != "no_action_taken" {
		t.Fatalf("last outcome = %#v, want no_action_taken", second.Workloads[0].Recommendation.Learning.Persistent.LastOutcome)
	}
}

func TestAttachAndRecordSanitizesNonFiniteLearnedSignals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	report := testReport()
	report.Workloads[0].Recommendation.Learning.Signals = []analyzer.LearnedSignal{
		{Name: "latency_p95", Window: "6h", Step: "5m", Points: 12, Current: math.NaN(), P50: math.Inf(1), P95: 1, Max: 2},
	}

	if err := AttachAndRecord(context.Background(), path, report); err != nil {
		t.Fatal(err)
	}
}

func TestRecordObservationPersistsConvergence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	report := &analyzer.ObservationReport{
		Application: "shipyard",
		Namespace:   "shipyardhq",
		GeneratedAt: time.Date(2026, 7, 8, 21, 30, 0, 0, time.UTC),
		Git: analyzer.GitObservation{
			Branch:               "master",
			Upstream:             "origin/master",
			LatestProposalCommit: "abc123",
		},
		Workloads: []analyzer.WorkloadObservation{
			{
				Name:       "web",
				Namespace:  "shipyardhq",
				Deployment: "shipyardhq",
				Status:     "applied",
				Outcome:    "neutral",
				Desired:    analyzer.ObservedResources{Replicas: "2", CPURequest: "490m", MemoryRequest: "3892Mi"},
				Live:       analyzer.ObservedResources{Replicas: "2", CPURequest: "490m", MemoryRequest: "3892Mi"},
				Reasons:    []string{"spec.replicas:matched"},
			},
		},
	}

	if err := RecordObservation(context.Background(), path, report); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var count int
	var status string
	var outcome string
	err = store.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*), MAX(status), MAX(outcome)
		FROM convergence_observations
		WHERE application = 'shipyard' AND workload_name = 'web'
	`).Scan(&count, &status, &outcome)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || status != "applied" || outcome != "neutral" {
		t.Fatalf("stored observation count/status/outcome = %d/%s/%s", count, status, outcome)
	}
}

func TestClassifyOutcomeAppliedSuccessful(t *testing.T) {
	report := testReport()
	workload := &report.Workloads[0]
	workload.Recommendation.CurrentCPURequest = "560m"
	workload.Recommendation.RecommendedCPURequest = "560m"
	workload.ReadyReplicas = 2
	workload.Replicas = 2
	workload.MetricsCondition = "healthy"

	outcome := classifyOutcome(priorRun{
		Actionable:               true,
		CPUActionable:            true,
		CurrentReplicas:          2,
		RecommendedReplicas:      2,
		CurrentCPURequest:        "700m",
		RecommendedCPURequest:    "560m",
		CurrentMemoryRequest:     "5Gi",
		RecommendedMemoryRequest: "5Gi",
	}, workload)

	if outcome.Status != "applied_successful" {
		t.Fatalf("Status = %q, want applied_successful; details=%v", outcome.Status, outcome.Details)
	}
}

func TestClassifyOutcomeTooAggressive(t *testing.T) {
	report := testReport()
	workload := &report.Workloads[0]
	workload.Recommendation.CurrentCPURequest = "560m"
	workload.Recommendation.RecommendedCPURequest = "560m"
	workload.ReadyReplicas = 1
	workload.Replicas = 2
	workload.MetricsCondition = "healthy"

	outcome := classifyOutcome(priorRun{
		Actionable:               true,
		CPUActionable:            true,
		CurrentReplicas:          2,
		RecommendedReplicas:      2,
		CurrentCPURequest:        "700m",
		RecommendedCPURequest:    "560m",
		CurrentMemoryRequest:     "5Gi",
		RecommendedMemoryRequest: "5Gi",
	}, workload)

	if outcome.Status != "too_aggressive" {
		t.Fatalf("Status = %q, want too_aggressive; details=%v", outcome.Status, outcome.Details)
	}
}

func TestClassifyOutcomeIgnoresBlockedFieldsButTracksActionableResources(t *testing.T) {
	report := testReport()
	workload := &report.Workloads[0]
	workload.Recommendation.CurrentReplicas = 3
	workload.Recommendation.CurrentCPURequest = "200m"
	workload.Recommendation.CurrentMemoryRequest = "5092Mi"
	workload.ReadyReplicas = 3
	workload.Replicas = 3
	workload.MetricsCondition = "healthy"

	outcome := classifyOutcome(priorRun{
		Actionable:               false,
		ReplicasActionable:       false,
		CPUActionable:            true,
		MemoryActionable:         true,
		CurrentReplicas:          3,
		RecommendedReplicas:      2,
		CurrentCPURequest:        "220m",
		RecommendedCPURequest:    "200m",
		CurrentMemoryRequest:     "5092Mi",
		RecommendedMemoryRequest: "3884Mi",
	}, workload)

	if outcome.Status != "partially_applied" {
		t.Fatalf("Status = %q, want partially_applied; details=%v", outcome.Status, outcome.Details)
	}
	if !contains(outcome.Details, "cpu_request_applied") {
		t.Fatalf("details missing cpu_request_applied: %v", outcome.Details)
	}
	if contains(outcome.Details, "replicas_not_applied") {
		t.Fatalf("blocked replica field should not be tracked as expected: %v", outcome.Details)
	}
}

func TestStabilizeRecommendationKeepsPriorCloseCPUAndMemoryTargets(t *testing.T) {
	report := testReport()
	workload := &report.Workloads[0]
	workload.Recommendation.CurrentCPURequest = "700m"
	workload.Recommendation.RecommendedCPURequest = "480m"
	workload.Recommendation.CurrentMemoryRequest = "5Gi"
	workload.Recommendation.RecommendedMemoryRequest = "3928Mi"

	stabilizeRecommendation(workload, &analyzer.PersistentLearning{
		PriorRecommendationRuns:      1,
		LastRecommendedCPURequest:    "490m",
		LastRecommendedMemoryRequest: "3943Mi",
	})

	if workload.Recommendation.RecommendedCPURequest != "490m" {
		t.Fatalf("RecommendedCPURequest = %q, want 490m", workload.Recommendation.RecommendedCPURequest)
	}
	if workload.Recommendation.RecommendedMemoryRequest != "3943Mi" {
		t.Fatalf("RecommendedMemoryRequest = %q, want 3943Mi", workload.Recommendation.RecommendedMemoryRequest)
	}
	if !contains(workload.Recommendation.ReasonCodes, "cpu_request_stabilized_to_prior:490m") {
		t.Fatalf("missing cpu stabilization reason: %#v", workload.Recommendation.ReasonCodes)
	}
	if !contains(workload.Recommendation.ReasonCodes, "memory_request_stabilized_to_prior:3943Mi") {
		t.Fatalf("missing memory stabilization reason: %#v", workload.Recommendation.ReasonCodes)
	}
}

func TestStabilizeRecommendationAllowsMeaningfulChange(t *testing.T) {
	report := testReport()
	workload := &report.Workloads[0]
	workload.Recommendation.CurrentCPURequest = "700m"
	workload.Recommendation.RecommendedCPURequest = "350m"

	stabilizeRecommendation(workload, &analyzer.PersistentLearning{
		PriorRecommendationRuns:   1,
		LastRecommendedCPURequest: "490m",
	})

	if workload.Recommendation.RecommendedCPURequest != "350m" {
		t.Fatalf("RecommendedCPURequest = %q, want 350m", workload.Recommendation.RecommendedCPURequest)
	}
}

func TestEvaluateStabilityRequiresThreeConsecutiveResourceDecreaseRuns(t *testing.T) {
	report := testReport()
	workload := &report.Workloads[0]
	workload.Recommendation.CurrentCPURequest = "700m"
	workload.Recommendation.RecommendedCPURequest = "560m"
	workload.Recommendation.CurrentMemoryRequest = "5Gi"
	workload.Recommendation.RecommendedMemoryRequest = "4Gi"

	pending := evaluateStability(workload, []priorRun{
		{CurrentCPURequest: "700m", RecommendedCPURequest: "560m", CurrentMemoryRequest: "5Gi", RecommendedMemoryRequest: "4Gi"},
	})
	if pending.CPU.Status != "pending_stability" || pending.CPU.Observed != 2 || pending.CPU.Required != 3 {
		t.Fatalf("pending CPU gate = %#v, want pending 2/3", pending.CPU)
	}
	if pending.Actionable {
		t.Fatal("pending stability should not be actionable")
	}

	stable := evaluateStability(workload, []priorRun{
		{CurrentCPURequest: "700m", RecommendedCPURequest: "560m", CurrentMemoryRequest: "5Gi", RecommendedMemoryRequest: "4Gi"},
		{CurrentCPURequest: "700m", RecommendedCPURequest: "570m", CurrentMemoryRequest: "5Gi", RecommendedMemoryRequest: "4100Mi"},
	})
	if stable.CPU.Status != "stable" || stable.CPU.Observed != 3 || stable.CPU.Required != 3 {
		t.Fatalf("stable CPU gate = %#v, want stable 3/3", stable.CPU)
	}
	if stable.Memory.Status != "stable" || stable.Memory.Observed != 3 || stable.Memory.Required != 3 {
		t.Fatalf("stable memory gate = %#v, want stable 3/3", stable.Memory)
	}
	if !stable.Actionable {
		t.Fatal("stable gates should be actionable")
	}
}

func TestEvaluateStabilityBlocksMemoryDecreaseOnAnomaly(t *testing.T) {
	report := testReport()
	workload := &report.Workloads[0]
	workload.Recommendation.CurrentMemoryRequest = "5Gi"
	workload.Recommendation.RecommendedMemoryRequest = "4Gi"
	workload.MetricSignals = []analyzer.SignalReport{
		{Name: "memory_working_set", Anomaly: analyzer.AnomalyStatus{State: "warning", Reason: "test"}},
	}

	stability := evaluateStability(workload, []priorRun{
		{CurrentMemoryRequest: "5Gi", RecommendedMemoryRequest: "4Gi"},
		{CurrentMemoryRequest: "5Gi", RecommendedMemoryRequest: "4Gi"},
	})
	if stability.Memory.Status != "blocked" {
		t.Fatalf("memory gate = %#v, want blocked", stability.Memory)
	}
	if stability.Actionable {
		t.Fatal("blocked memory gate should not be actionable")
	}
}

func TestOutcomeSafetyGateBlocksUnsettledPriorRecommendation(t *testing.T) {
	stability := &analyzer.RecommendationStability{
		Actionable: true,
		Replicas:   analyzer.StabilityGate{Status: "hold"},
		CPU:        analyzer.StabilityGate{Status: "stable", Observed: 3, Required: 3},
		Memory:     analyzer.StabilityGate{Status: "hold"},
	}

	applyOutcomeSafetyGate(stability, &analyzer.RecommendationOutcome{Status: "partially_applied"}, "propose")

	if stability.Actionable {
		t.Fatal("partially applied prior recommendation should block action")
	}
	if stability.CPU.Status != "blocked" || !strings.Contains(stability.CPU.Reason, "partially applied") {
		t.Fatalf("CPU gate = %#v, want blocked by partially applied outcome", stability.CPU)
	}
}

func TestOutcomeSafetyGateBlocksDryRunRecommendationWithExplicitReason(t *testing.T) {
	stability := &analyzer.RecommendationStability{
		Actionable: true,
		Replicas:   analyzer.StabilityGate{Status: "stable", Observed: 3, Required: 3},
		CPU:        analyzer.StabilityGate{Status: "stable", Observed: 3, Required: 3},
		Memory:     analyzer.StabilityGate{Status: "hold"},
	}

	applyOutcomeSafetyGate(stability, &analyzer.RecommendationOutcome{Status: "dry_run_not_applied"}, "dry-run")

	if stability.Actionable {
		t.Fatal("dry-run prior recommendation should block action")
	}
	if stability.Replicas.Status != "blocked" || stability.Replicas.Reason != "previous dry-run recommendation not applied" {
		t.Fatalf("replica gate = %#v, want blocked by dry-run outcome", stability.Replicas)
	}
}

func TestOutcomeSafetyGateAllowsProposeAfterDryRunRecommendation(t *testing.T) {
	stability := &analyzer.RecommendationStability{
		Actionable: true,
		Replicas:   analyzer.StabilityGate{Status: "stable", Observed: 3, Required: 3},
		CPU:        analyzer.StabilityGate{Status: "stable", Observed: 3, Required: 3},
		Memory:     analyzer.StabilityGate{Status: "hold"},
	}

	applyOutcomeSafetyGate(stability, &analyzer.RecommendationOutcome{Status: "dry_run_not_applied"}, "propose")

	if !stability.Actionable {
		t.Fatal("dry-run prior recommendation should not block propose mode")
	}
	if stability.Replicas.Status == "blocked" {
		t.Fatalf("replica gate = %#v, should not be blocked", stability.Replicas)
	}
}

func TestOutcomeSafetyGateBlocksTooConservativePriorRecommendation(t *testing.T) {
	stability := &analyzer.RecommendationStability{
		Actionable: true,
		Replicas:   analyzer.StabilityGate{Status: "hold"},
		CPU:        analyzer.StabilityGate{Status: "stable", Observed: 3, Required: 3},
		Memory:     analyzer.StabilityGate{Status: "hold"},
	}

	applyOutcomeSafetyGate(stability, &analyzer.RecommendationOutcome{Status: "too_conservative"}, "propose")

	if stability.Actionable {
		t.Fatal("too conservative prior recommendation should block action")
	}
	if stability.CPU.Status != "blocked" || !strings.Contains(stability.CPU.Reason, "post-apply observation") {
		t.Fatalf("CPU gate = %#v, want blocked by post-apply observation", stability.CPU)
	}
}

func TestRecentOutcomeCooldownBlocksAfterNoActionObservation(t *testing.T) {
	stability := &analyzer.RecommendationStability{
		Actionable: true,
		Replicas:   analyzer.StabilityGate{Status: "hold"},
		CPU:        analyzer.StabilityGate{Status: "stable", Observed: 3, Required: 3},
		Memory:     analyzer.StabilityGate{Status: "hold"},
	}

	applyRecentOutcomeCooldownGate(stability, []priorRun{
		{OutcomeStatus: "no_action_taken"},
		{OutcomeStatus: "too_conservative"},
	}, "propose")

	if stability.Actionable {
		t.Fatal("recent too conservative outcome should keep recommendation under observation")
	}
	if stability.CPU.Status != "blocked" || !strings.Contains(stability.CPU.Reason, "observation cooldown 2/3") {
		t.Fatalf("CPU gate = %#v, want observation cooldown block", stability.CPU)
	}
}

func TestOutcomeSafetyGateAllowsSuccessfulPriorRecommendation(t *testing.T) {
	stability := &analyzer.RecommendationStability{
		Actionable: true,
		Replicas:   analyzer.StabilityGate{Status: "hold"},
		CPU:        analyzer.StabilityGate{Status: "stable", Observed: 3, Required: 3},
		Memory:     analyzer.StabilityGate{Status: "hold"},
	}

	applyOutcomeSafetyGate(stability, &analyzer.RecommendationOutcome{Status: "applied_successful"}, "propose")

	if !stability.Actionable {
		t.Fatal("successful prior recommendation should not block action")
	}
	if stability.CPU.Status != "stable" {
		t.Fatalf("CPU gate = %#v, want unchanged stable gate", stability.CPU)
	}
}

func TestScoreForecastAccuracyUsesPriorForecastAndCurrentSignals(t *testing.T) {
	report := testReport()
	workload := &report.Workloads[0]
	workload.ReadyReplicas = 2
	requestRate := 1.0
	cpuUsage := 0.25
	memoryUsage := float64(2 * 1024 * 1024 * 1024)
	availableReplicas := 2.0
	workload.MetricSignals = []analyzer.SignalReport{
		{Name: "request_rate", Sample: &requestRate},
		{Name: "cpu_usage", Sample: &cpuUsage},
		{Name: "memory_working_set", Sample: &memoryUsage},
		{Name: "available_replicas", Sample: &availableReplicas},
	}

	scores := scoreForecastAccuracy(priorRun{
		RecommendedReplicas: 2,
		ReasonCodes: []string{
			"traffic_forecast:2",
			"cpu_usage_p95_6h:0.5",
			"memory_working_set_p95_6h:3Gi",
		},
	}, workload)

	if len(scores) != 4 {
		t.Fatalf("scores = %d, want 4: %#v", len(scores), scores)
	}
	if scores[0].Signal != "request_rate" || scores[0].Classification != "overestimated" {
		t.Fatalf("request score = %#v, want overestimated request_rate", scores[0])
	}
}

func TestForecastAccuracyFeedbackAdjustsConfidenceAndResourceDecrease(t *testing.T) {
	report := testReport()
	workload := &report.Workloads[0]
	workload.Recommendation.Confidence = 0.80
	workload.Recommendation.CurrentCPURequest = "700m"
	workload.Recommendation.RecommendedCPURequest = "560m"
	workload.Recommendation.CurrentMemoryRequest = "5Gi"
	workload.Recommendation.RecommendedMemoryRequest = "4Gi"

	applyForecastAccuracyFeedback(workload, &analyzer.ForecastAccuracy{
		Enabled:              true,
		Samples:              4,
		ConfidenceAdjustment: -0.05,
		WasteReductionBias:   "favor_waste_reduction",
		Signals: []analyzer.ForecastAccuracyScore{
			{Signal: "cpu_usage", Samples: 3, MeanBiasPercent: 0.30, Classification: "overestimated"},
			{Signal: "memory_working_set", Samples: 3, MeanBiasPercent: -0.30, Classification: "underestimated"},
		},
	})

	if workload.Recommendation.Confidence != 0.75 {
		t.Fatalf("Confidence = %.2f, want 0.75", workload.Recommendation.Confidence)
	}
	if workload.Recommendation.RecommendedCPURequest == "560m" {
		t.Fatal("CPU recommendation was not adjusted by overestimate bias")
	}
	if workload.Recommendation.RecommendedMemoryRequest == "4Gi" {
		t.Fatal("memory recommendation was not adjusted by underestimate bias")
	}
	if !containsPrefix(workload.Recommendation.ReasonCodes, "forecast_waste_reduction_bias:") {
		t.Fatalf("missing forecast waste bias reason: %#v", workload.Recommendation.ReasonCodes)
	}
}

func TestProposalBudgetBlocksWhenHourlyLimitReached(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	report := testProposalReport(time.Date(2026, 7, 9, 18, 0, 0, 0, time.UTC))
	if err := RecordProposalEvents(context.Background(), path, report); err != nil {
		t.Fatal(err)
	}

	next := testProposalReport(time.Date(2026, 7, 9, 18, 30, 0, 0, time.UTC))
	next.Proposal = nil
	profile := &config.ApplicationProfile{
		Spec: config.ApplicationSpec{
			Workloads: []config.WorkloadSpec{
				{
					Name:   "web",
					Policy: config.PolicySpec{MaxProposalsPerHour: 1},
				},
			},
		},
	}
	if err := ApplyProposalBudgets(context.Background(), path, next, profile); err != nil {
		t.Fatal(err)
	}
	plan := next.Workloads[0].Recommendation.PatchPlan
	if plan == nil || !plan.Blocked {
		t.Fatalf("PatchPlan = %#v, want budget blocked", plan)
	}
	if plan.Needed {
		t.Fatal("PatchPlan.Needed = true, want false after budget block")
	}
	if !containsPrefix(plan.BlockReasons, "proposal budget exhausted: 1/1 in last 1h") {
		t.Fatalf("missing budget block reason: %#v", plan.BlockReasons)
	}
	if next.Workloads[0].Recommendation.RecommendedCPURequest != "490m" {
		t.Fatalf("recommendation was changed by budget gate: %#v", next.Workloads[0].Recommendation)
	}
}

func TestProposalBudgetIsUnlimitedWhenUnset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	report := testProposalReport(time.Date(2026, 7, 9, 18, 0, 0, 0, time.UTC))
	if err := RecordProposalEvents(context.Background(), path, report); err != nil {
		t.Fatal(err)
	}

	next := testProposalReport(time.Date(2026, 7, 9, 18, 1, 0, 0, time.UTC))
	next.Proposal = nil
	profile := &config.ApplicationProfile{
		Spec: config.ApplicationSpec{
			Workloads: []config.WorkloadSpec{{Name: "web"}},
		},
	}
	if err := ApplyProposalBudgets(context.Background(), path, next, profile); err != nil {
		t.Fatal(err)
	}
	plan := next.Workloads[0].Recommendation.PatchPlan
	if plan == nil || plan.Blocked {
		t.Fatalf("PatchPlan = %#v, want unblocked with unset budget", plan)
	}
}

func TestProposalBatchBlocksUntilWindowElapses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first := testProposalReport(time.Date(2026, 7, 9, 18, 0, 0, 0, time.UTC))
	first.Proposal = nil
	if err := ApplyProposalBatch(context.Background(), path, first, nil, "commit", 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	firstPlan := first.Workloads[0].Recommendation.PatchPlan
	if firstPlan == nil || !firstPlan.Blocked {
		t.Fatalf("first PatchPlan = %#v, want batch blocked", firstPlan)
	}
	if !containsPrefix(firstPlan.BlockReasons, "proposal batch window open:") {
		t.Fatalf("missing batch window reason: %#v", firstPlan.BlockReasons)
	}

	second := testProposalReport(time.Date(2026, 7, 9, 18, 16, 0, 0, time.UTC))
	second.Proposal = nil
	if err := ApplyProposalBatch(context.Background(), path, second, nil, "commit", 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	secondPlan := second.Workloads[0].Recommendation.PatchPlan
	if secondPlan == nil || secondPlan.Blocked {
		t.Fatalf("second PatchPlan = %#v, want released after batch window", secondPlan)
	}
	if !secondPlan.Needed {
		t.Fatal("second PatchPlan.Needed = false, want true")
	}
}

func TestProposalBatchRequiresStateDB(t *testing.T) {
	report := testProposalReport(time.Date(2026, 7, 9, 18, 0, 0, 0, time.UTC))
	report.Proposal = nil
	if err := ApplyProposalBatch(context.Background(), "", report, nil, "commit", 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	plan := report.Workloads[0].Recommendation.PatchPlan
	if plan == nil || !plan.Blocked {
		t.Fatalf("PatchPlan = %#v, want blocked without state db", plan)
	}
	if !contains(plan.BlockReasons, "proposal batch window requires --state-db for persisted grouping") {
		t.Fatalf("missing state db block reason: %#v", plan.BlockReasons)
	}
}

func TestRecordProposalEventsClearsProposalBatchItem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first := testProposalReport(time.Date(2026, 7, 9, 18, 0, 0, 0, time.UTC))
	first.Proposal = nil
	if err := ApplyProposalBatch(context.Background(), path, first, nil, "commit", 15*time.Minute); err != nil {
		t.Fatal(err)
	}

	ready := testProposalReport(time.Date(2026, 7, 9, 18, 16, 0, 0, time.UTC))
	if err := ApplyProposalBatch(context.Background(), path, ready, nil, "commit", 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := RecordProposalEvents(context.Background(), path, ready); err != nil {
		t.Fatal(err)
	}

	next := testProposalReport(time.Date(2026, 7, 9, 18, 17, 0, 0, time.UTC))
	next.Proposal = nil
	if err := ApplyProposalBatch(context.Background(), path, next, nil, "commit", 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	plan := next.Workloads[0].Recommendation.PatchPlan
	if plan == nil || !plan.Blocked {
		t.Fatalf("PatchPlan = %#v, want new batch window after cleanup", plan)
	}
}

func TestProposalBatchResetsWindowWhenRecommendationChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first := testProposalReport(time.Date(2026, 7, 9, 18, 0, 0, 0, time.UTC))
	first.Proposal = nil
	if err := ApplyProposalBatch(context.Background(), path, first, nil, "commit", 15*time.Minute); err != nil {
		t.Fatal(err)
	}

	changed := testProposalReport(time.Date(2026, 7, 9, 18, 16, 0, 0, time.UTC))
	changed.Proposal = nil
	changed.Workloads[0].Recommendation.RecommendedCPURequest = "480m"
	changed.Workloads[0].Recommendation.PatchPlan.Changes[0].Recommended = "480m"
	if err := ApplyProposalBatch(context.Background(), path, changed, nil, "commit", 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	plan := changed.Workloads[0].Recommendation.PatchPlan
	if plan == nil || !plan.Blocked {
		t.Fatalf("PatchPlan = %#v, want reset batch window after recommendation changed", plan)
	}
	if !containsPrefix(plan.BlockReasons, "proposal batch window open:") {
		t.Fatalf("missing batch window reason after changed recommendation: %#v", plan.BlockReasons)
	}
}

func TestProposalBatchBypassesWindowForUrgentTrafficScaleUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	report := testProposalReport(time.Date(2026, 7, 9, 18, 0, 0, 0, time.UTC))
	report.Proposal = nil
	workload := &report.Workloads[0]
	workload.Recommendation.CurrentReplicas = 2
	workload.Recommendation.RecommendedReplicas = 3
	workload.Recommendation.PatchPlan.Changes = []analyzer.PatchChange{
		{Field: "spec.replicas", Operation: "replace", Current: "2", Recommended: "3"},
	}
	workload.MetricSignals = []analyzer.SignalReport{
		{Name: "request_rate", Anomaly: analyzer.AnomalyStatus{State: "critical", Reason: "spike"}},
	}

	if err := ApplyProposalBatch(context.Background(), path, report, nil, "commit", 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	plan := workload.Recommendation.PatchPlan
	if plan == nil || plan.Blocked {
		t.Fatalf("PatchPlan = %#v, want urgent bypass unblocked", plan)
	}
	if !contains(plan.BlockReasons, "proposal batch bypassed for urgent traffic anomaly") {
		t.Fatalf("missing urgent bypass reason: %#v", plan.BlockReasons)
	}
}

func TestProposalBatchHonorsUrgentBypassPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	report := testProposalReport(time.Date(2026, 7, 9, 18, 0, 0, 0, time.UTC))
	report.Proposal = nil
	workload := &report.Workloads[0]
	workload.Recommendation.CurrentReplicas = 2
	workload.Recommendation.RecommendedReplicas = 3
	workload.Recommendation.PatchPlan.Changes = []analyzer.PatchChange{
		{Field: "spec.replicas", Operation: "replace", Current: "2", Recommended: "3"},
	}
	workload.MetricSignals = []analyzer.SignalReport{
		{Name: "request_rate", Anomaly: analyzer.AnomalyStatus{State: "critical", Reason: "spike"}},
	}
	urgentBypassAllowed := false
	profile := &config.ApplicationProfile{
		Spec: config.ApplicationSpec{
			Workloads: []config.WorkloadSpec{
				{
					Name: "web",
					Policy: config.PolicySpec{
						Safety: config.SafetyPolicySpec{UrgentBypassAllowed: &urgentBypassAllowed},
					},
				},
			},
		},
	}

	if err := ApplyProposalBatch(context.Background(), path, report, profile, "commit", 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	plan := workload.Recommendation.PatchPlan
	if plan == nil || !plan.Blocked {
		t.Fatalf("PatchPlan = %#v, want batch window block when urgent bypass is disabled", plan)
	}
	if contains(plan.BlockReasons, "proposal batch bypassed for urgent traffic anomaly") {
		t.Fatalf("unexpected urgent bypass reason: %#v", plan.BlockReasons)
	}
	if !containsPrefix(plan.BlockReasons, "proposal batch window open:") {
		t.Fatalf("missing batch window reason: %#v", plan.BlockReasons)
	}
}

func TestProposalBatchDoesNotBypassForAnomalousDecrease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	report := testProposalReport(time.Date(2026, 7, 9, 18, 0, 0, 0, time.UTC))
	report.Proposal = nil
	workload := &report.Workloads[0]
	workload.MetricSignals = []analyzer.SignalReport{
		{Name: "request_rate", Anomaly: analyzer.AnomalyStatus{State: "critical", Reason: "spike"}},
	}

	if err := ApplyProposalBatch(context.Background(), path, report, nil, "commit", 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	plan := workload.Recommendation.PatchPlan
	if plan == nil || !plan.Blocked {
		t.Fatalf("PatchPlan = %#v, want anomalous decrease to remain batched", plan)
	}
	if contains(plan.BlockReasons, "proposal batch bypassed for urgent traffic anomaly") {
		t.Fatalf("unexpected urgent bypass for decrease: %#v", plan.BlockReasons)
	}
}

func TestSeasonalityPersistsHourlyBuckets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	base := time.Date(2026, 7, 7, 14, 0, 0, 0, time.UTC)
	for index := 0; index < 3; index++ {
		report := seasonalTestReport(base.Add(time.Duration(index)*5*time.Minute), 10+float64(index), 0.5+float64(index)/10, 2*1024*1024*1024, 0.2)
		if err := AttachAndRecord(context.Background(), path, report); err != nil {
			t.Fatal(err)
		}
	}

	current := seasonalTestReport(base.Add(7*24*time.Hour), 1, 0.1, 512*1024*1024, 0.1)
	if err := AttachAndRecord(context.Background(), path, current); err != nil {
		t.Fatal(err)
	}
	seasonality := current.Workloads[0].Recommendation.Learning.Persistent.Seasonality
	if seasonality == nil {
		t.Fatal("seasonality is nil")
	}
	if seasonality.ObservationCount == 0 {
		t.Fatal("ObservationCount = 0, want prior observations")
	}
	if !containsSeasonalSignal(seasonality.Signals, "request_rate", "same_day_type_hour") {
		t.Fatalf("missing request_rate same_day_type_hour bucket: %#v", seasonality.Signals)
	}
	if len(seasonality.LatencyByTrafficBand) == 0 {
		t.Fatalf("missing latency traffic-band buckets: %#v", seasonality)
	}
}

func TestSeasonalityFeedbackBlocksReductionsForHotHour(t *testing.T) {
	report := seasonalTestReport(time.Date(2026, 7, 7, 14, 0, 0, 0, time.UTC), 1, 0.1, 512*1024*1024, 0.1)
	workload := &report.Workloads[0]
	workload.Recommendation.CurrentReplicas = 2
	workload.Recommendation.RecommendedReplicas = 1
	workload.Recommendation.CurrentCPURequest = "700m"
	workload.Recommendation.RecommendedCPURequest = "400m"
	workload.Recommendation.CurrentMemoryRequest = "1Gi"
	workload.Recommendation.RecommendedMemoryRequest = "512Mi"

	applySeasonalityFeedback(workload, &analyzer.SeasonalityLearning{
		Enabled:          true,
		ObservationCount: 9,
		CurrentHour:      14,
		CurrentDayType:   "weekday",
		Signals: []analyzer.SeasonalSignal{
			{Signal: "request_rate", Bucket: "same_day_type_hour", Hour: 14, DayType: "weekday", Points: 3, P95: 10, Max: 12},
			{Signal: "cpu_usage", Bucket: "same_day_type_hour", Hour: 14, DayType: "weekday", Points: 3, P95: 0.8, Max: 1},
			{Signal: "memory_working_set", Bucket: "same_day_type_hour", Hour: 14, DayType: "weekday", Points: 3, P95: 2 * 1024 * 1024 * 1024, Max: 3 * 1024 * 1024 * 1024},
		},
	})

	rec := workload.Recommendation
	if rec.RecommendedReplicas != rec.CurrentReplicas {
		t.Fatalf("RecommendedReplicas = %d, want held at %d", rec.RecommendedReplicas, rec.CurrentReplicas)
	}
	if rec.RecommendedCPURequest != rec.CurrentCPURequest {
		t.Fatalf("RecommendedCPURequest = %q, want held at %q", rec.RecommendedCPURequest, rec.CurrentCPURequest)
	}
	if rec.RecommendedMemoryRequest != rec.CurrentMemoryRequest {
		t.Fatalf("RecommendedMemoryRequest = %q, want held at %q", rec.RecommendedMemoryRequest, rec.CurrentMemoryRequest)
	}
	if !containsPrefix(rec.ReasonCodes, "replica_scale_down_blocked_by_seasonality:") {
		t.Fatalf("missing seasonality replica block reason: %#v", rec.ReasonCodes)
	}
	if !hasDecision(rec.Learning.Decisions, "seasonality.cpu") {
		t.Fatalf("missing seasonality CPU decision: %#v", rec.Learning.Decisions)
	}
}

func TestReplicaOutcomeFeedbackAddsReplicaComponent(t *testing.T) {
	report := testReport()
	workload := &report.Workloads[0]
	workload.Recommendation.ReplicaDecision = &analyzer.ReplicaDecision{
		RecommendedReplicas: 3,
		Score:               0.30,
		Basis:               "traffic_forecast",
	}

	applyReplicaOutcomeFeedback(workload, &analyzer.RecommendationOutcome{
		Status:  "too_conservative",
		Details: []string{"replicas_applied"},
	})

	decision := workload.Recommendation.ReplicaDecision
	if decision == nil || decision.Score <= 0.30 {
		t.Fatalf("ReplicaDecision = %#v, want score increased by prior outcome", decision)
	}
	if !containsPrefix(workload.Recommendation.ReasonCodes, "replica_signal_score_after_outcome:") {
		t.Fatalf("missing outcome score reason: %#v", workload.Recommendation.ReasonCodes)
	}
	if len(decision.Components) != 1 || decision.Components[0].Name != "prior_replica_outcome" {
		t.Fatalf("components = %#v, want prior_replica_outcome", decision.Components)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if len(value) >= len(prefix) && value[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

func containsSeasonalSignal(signals []analyzer.SeasonalSignal, signal, bucket string) bool {
	for _, item := range signals {
		if item.Signal == signal && item.Bucket == bucket {
			return true
		}
	}
	return false
}

func hasDecision(decisions []analyzer.LearnedDecision, subject string) bool {
	for _, decision := range decisions {
		if decision.Subject == subject {
			return true
		}
	}
	return false
}

func testReport() *analyzer.Report {
	return &analyzer.Report{
		Application: "shipyard",
		GeneratedAt: time.Date(2026, 7, 8, 19, 30, 0, 0, time.UTC),
		Workloads: []analyzer.WorkloadReport{
			{
				Name:       "web",
				Namespace:  "shipyardhq",
				Deployment: "shipyardhq",
				Recommendation: analyzer.Recommendation{
					Mode:                     "dry-run",
					CurrentReplicas:          2,
					RecommendedReplicas:      2,
					CurrentCPURequest:        "700m",
					RecommendedCPURequest:    "560m",
					CurrentMemoryRequest:     "5Gi",
					RecommendedMemoryRequest: "5Gi",
					Confidence:               0.95,
					ReasonCodes:              []string{"cpu_request_decrease_recommended"},
					Learning: analyzer.LearningEvidence{
						Mode: "prometheus-history",
						Signals: []analyzer.LearnedSignal{
							{Name: "request_rate", Window: "6h", Step: "5m", Points: 73, Current: 1, P50: 1.2, P95: 2, Max: 3, Classification: "below_learned_median"},
						},
						Decisions: []analyzer.LearnedDecision{
							{Subject: "replicas.traffic", Learned: "peak_per_replica=1.5", Observed: "forecast=2", Conclusion: "hold"},
						},
					},
				},
			},
		},
	}
}

func seasonalTestReport(generatedAt time.Time, requestRate, cpuUsage, memoryWorkingSet, latencyP95 float64) *analyzer.Report {
	report := testReport()
	report.GeneratedAt = generatedAt
	workload := &report.Workloads[0]
	workload.Replicas = 2
	workload.ReadyReplicas = 2
	workload.MetricsCondition = "healthy"
	workload.MetricSignals = []analyzer.SignalReport{
		{Name: "request_rate", Healthy: true, Sample: &requestRate, History: &analyzer.SignalHistory{Points: 12, P50: 1, P95: 2, Max: 3}},
		{Name: "cpu_usage", Healthy: true, Sample: &cpuUsage, History: &analyzer.SignalHistory{Points: 12, P50: 0.1, P95: 0.2, Max: 0.3}},
		{Name: "memory_working_set", Healthy: true, Sample: &memoryWorkingSet, History: &analyzer.SignalHistory{Points: 12, P50: 512 * 1024 * 1024, P95: 1024 * 1024 * 1024, Max: 1536 * 1024 * 1024}},
		{Name: "latency_p95", Healthy: true, Sample: &latencyP95, History: &analyzer.SignalHistory{Points: 12, P50: 0.1, P95: 0.2, Max: 0.4}},
	}
	workload.Recommendation.Learning.Signals = nil
	return report
}

func testProposalReport(generatedAt time.Time) *analyzer.Report {
	report := testReport()
	report.GeneratedAt = generatedAt
	report.Workloads[0].Recommendation.Mode = "propose"
	report.Workloads[0].Recommendation.CurrentCPURequest = "700m"
	report.Workloads[0].Recommendation.RecommendedCPURequest = "490m"
	report.Workloads[0].Recommendation.PatchPlan = &analyzer.PatchPlan{
		SourceFile: "shipyard/deployment.yaml",
		Resource:   "Deployment/shipyardhq/shipyardhq",
		Needed:     true,
		Changes: []analyzer.PatchChange{
			{Field: "spec.template.spec.containers[name=web].resources.requests.cpu", Operation: "replace", Current: "700m", Recommended: "490m"},
		},
	}
	report.Proposal = &analyzer.ProposalReport{
		Mode:   "propose",
		Kind:   "commit",
		Needed: true,
		Commit: "abc123",
	}
	return report
}
