package state

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abhi1693/k8s-recommendation-engine/internal/analyzer"
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
	if persistent.LastOutcome.Status != "dry_run_not_applied" {
		t.Fatalf("LastOutcome.Status = %q, want dry_run_not_applied", persistent.LastOutcome.Status)
	}
}

func TestAttachAndRecordClassifiesProposeNotApplied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first := testReport()
	first.Workloads[0].Recommendation.Mode = "propose"
	if err := AttachAndRecord(context.Background(), path, first); err != nil {
		t.Fatal(err)
	}

	second := testReport()
	second.Workloads[0].Recommendation.Mode = "propose"
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
		second.Workloads[0].Recommendation.Learning.Persistent.LastOutcome.Status != "dry_run_not_applied" {
		t.Fatalf("last outcome = %#v, want dry_run_not_applied", second.Workloads[0].Recommendation.Learning.Persistent.LastOutcome)
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
