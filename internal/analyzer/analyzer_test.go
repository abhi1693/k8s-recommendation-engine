package analyzer

import (
	"strings"
	"testing"
	"time"

	"github.com/abhi1693/k8s-recommendation-engine/internal/config"
	"github.com/abhi1693/k8s-recommendation-engine/internal/prom"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestMetricCondition(t *testing.T) {
	tests := []struct {
		name    string
		signals []SignalReport
		want    string
	}{
		{
			name: "required healthy",
			signals: []SignalReport{
				{Name: "request_rate", Required: true, Healthy: true},
			},
			want: "healthy",
		},
		{
			name: "required missing optional healthy",
			signals: []SignalReport{
				{Name: "request_rate", Required: true},
				{Name: "latency", Healthy: true},
			},
			want: "degraded",
		},
		{
			name: "all missing",
			signals: []SignalReport{
				{Name: "request_rate", Required: true},
			},
			want: "unhealthy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := metricCondition(tt.signals); got != tt.want {
				t.Fatalf("metricCondition() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestContainerReport(t *testing.T) {
	requests := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("100m"),
		corev1.ResourceMemory: resource.MustParse("256Mi"),
	}
	got := containerReport("web", requests, nil)
	if got.CPURequest != "100m" {
		t.Fatalf("CPURequest = %q", got.CPURequest)
	}
	if got.MemoryRequest != "256Mi" {
		t.Fatalf("MemoryRequest = %q", got.MemoryRequest)
	}
	if got.CPURequestCores != 0.1 {
		t.Fatalf("CPURequestCores = %f", got.CPURequestCores)
	}
	if got.MemoryRequestBytes != 268435456 {
		t.Fatalf("MemoryRequestBytes = %f", got.MemoryRequestBytes)
	}
}

func TestBuildRecommendationHoldsScaleDownWithoutHistory(t *testing.T) {
	report := WorkloadReport{
		Replicas: 2,
		Containers: []ContainerReport{
			{
				Name:               "web",
				CPURequest:         "700m",
				CPURequestCores:    0.7,
				MemoryRequest:      "5Gi",
				MemoryRequestBytes: 5 * 1024 * 1024 * 1024,
			},
		},
		MetricsCondition: "healthy",
		MetricSignals: []SignalReport{
			sampleSignal("cpu_usage", 0.2),
			sampleSignal("memory_working_set", 1024*1024*1024),
		},
	}
	workload := config.WorkloadSpec{
		Scaling: config.ScalingSpec{Replicas: true, CPU: true, Memory: true},
		Bounds: config.BoundsSpec{
			Replicas: config.ReplicaBounds{Min: 2, Max: 4},
		},
	}

	got := buildRecommendation(workload, report, nil)
	if got.RecommendedReplicas != 2 {
		t.Fatalf("RecommendedReplicas = %d, want 2", got.RecommendedReplicas)
	}
	if got.RecommendedCPURequest != "700m" {
		t.Fatalf("RecommendedCPURequest = %q", got.RecommendedCPURequest)
	}
	if got.RecommendedMemoryRequest != "5Gi" {
		t.Fatalf("RecommendedMemoryRequest = %q", got.RecommendedMemoryRequest)
	}
}

func TestBuildRecommendationIncreasesReplicasOnSaturation(t *testing.T) {
	report := WorkloadReport{
		Replicas: 2,
		Containers: []ContainerReport{
			{
				Name:               "web",
				CPURequest:         "500m",
				CPURequestCores:    0.5,
				MemoryRequest:      "512Mi",
				MemoryRequestBytes: 512 * 1024 * 1024,
			},
		},
		MetricsCondition: "healthy",
		MetricSignals: []SignalReport{
			sampleSignal("cpu_usage", 1.8),
			sampleSignal("memory_working_set", 800*1024*1024),
		},
	}
	workload := config.WorkloadSpec{
		Scaling: config.ScalingSpec{Replicas: true, CPU: true, Memory: true},
		Bounds: config.BoundsSpec{
			Replicas: config.ReplicaBounds{Min: 2, Max: 4},
			CPU:      config.ChangeBounds{MaxIncreasePercent: 50},
		},
	}

	got := buildRecommendation(workload, report, nil)
	if got.RecommendedReplicas != 4 {
		t.Fatalf("RecommendedReplicas = %d, want 4", got.RecommendedReplicas)
	}
	if got.RecommendedCPURequest != "740m" {
		t.Fatalf("RecommendedCPURequest = %q, want 740m", got.RecommendedCPURequest)
	}
}

func TestBuildRecommendationIncreasesReplicasOnTrafficOutsideLearnedEnvelope(t *testing.T) {
	report := WorkloadReport{
		Replicas: 2,
		Containers: []ContainerReport{
			{
				Name:               "web",
				CPURequest:         "700m",
				CPURequestCores:    0.7,
				MemoryRequest:      "5Gi",
				MemoryRequestBytes: 5 * 1024 * 1024 * 1024,
			},
		},
		MetricsCondition: "healthy",
		MetricSignals: []SignalReport{
			sampleSignalWithHistory("request_rate", 101, SignalHistory{Points: 24, P50: 40, P95: 80, Max: 100}),
			sampleSignal("cpu_usage", 0.2),
			sampleSignal("memory_working_set", 1024*1024*1024),
		},
	}
	workload := config.WorkloadSpec{
		Scaling: config.ScalingSpec{Replicas: true, CPU: true, Memory: true},
		Bounds: config.BoundsSpec{
			Replicas: config.ReplicaBounds{Min: 2, Max: 6},
		},
	}

	got := buildRecommendation(workload, report, nil)
	if got.RecommendedReplicas != 3 {
		t.Fatalf("RecommendedReplicas = %d, want 3", got.RecommendedReplicas)
	}
	if got.RecommendedCPURequest != "700m" {
		t.Fatalf("RecommendedCPURequest = %q, want 700m", got.RecommendedCPURequest)
	}
	if got.RecommendedMemoryRequest != "5Gi" {
		t.Fatalf("RecommendedMemoryRequest = %q, want 5Gi", got.RecommendedMemoryRequest)
	}
}

func TestBuildRecommendationDoesNotScaleUpForLowTrafficLatencySpike(t *testing.T) {
	latency := sampleSignalWithHistory("latency_p95", 2.061, SignalHistory{Points: 24, P50: 0.24, P95: 0.528, Max: 3.44})
	latency.Anomaly = AnomalyStatus{State: "warning", Reason: "current latency exceeds p95"}
	report := WorkloadReport{
		Replicas: 2,
		Containers: []ContainerReport{
			{
				Name:               "web",
				CPURequest:         "700m",
				CPURequestCores:    0.7,
				MemoryRequest:      "5Gi",
				MemoryRequestBytes: 5 * 1024 * 1024 * 1024,
			},
		},
		MetricsCondition: "healthy",
		MetricSignals: []SignalReport{
			sampleSignalWithHistory("request_rate", 0.9833, SignalHistory{Points: 73, P50: 1.349, P95: 2.771, Max: 3.5}),
			latency,
			sampleSignalWithHistory("cpu_usage", 0.095, SignalHistory{Points: 73, P50: 0.139, P95: 0.222, Max: 0.339}),
			sampleSignalWithHistory("memory_working_set", 4*1024*1024*1024, SignalHistory{Points: 73, P50: 2 * 1024 * 1024 * 1024, P95: 7.3 * 1024 * 1024 * 1024, Max: 7.6 * 1024 * 1024 * 1024}),
			sampleSignalWithHistory("available_replicas", 2, SignalHistory{Points: 73, P50: 2, P95: 2, Max: 2}),
		},
	}
	workload := config.WorkloadSpec{
		Scaling: config.ScalingSpec{Replicas: true, CPU: false, Memory: false},
		Bounds: config.BoundsSpec{
			Replicas: config.ReplicaBounds{Min: 1, Max: 4},
		},
	}

	got := buildRecommendation(workload, report, nil)
	if got.RecommendedReplicas != 2 {
		t.Fatalf("RecommendedReplicas = %d, want 2", got.RecommendedReplicas)
	}
	if len(got.Learning.Signals) == 0 {
		t.Fatal("Learning.Signals is empty")
	}
	if len(got.Learning.Decisions) == 0 {
		t.Fatal("Learning.Decisions is empty")
	}
}

func TestBuildRecommendationUsesMultiSignalReplicaBasis(t *testing.T) {
	latency := sampleSignalWithHistory("latency_p95", 2, SignalHistory{Points: 24, P50: 0.2, P95: 0.5, Max: 1})
	latency.Anomaly = AnomalyStatus{State: "critical", Reason: "latency outside learned max"}
	report := WorkloadReport{
		Replicas: 2,
		Containers: []ContainerReport{
			{
				Name:               "web",
				CPURequest:         "500m",
				CPURequestCores:    0.5,
				MemoryRequest:      "1Gi",
				MemoryRequestBytes: 1024 * 1024 * 1024,
			},
		},
		MetricsCondition: "healthy",
		MetricSignals: []SignalReport{
			sampleSignalWithHistory("request_rate", 10, SignalHistory{Points: 24, P50: 4, P95: 8, Max: 9}),
			latency,
			sampleSignalWithHistory("cpu_usage", 0.2, SignalHistory{Points: 24, P50: 0.1, P95: 0.2, Max: 0.3}),
			sampleSignalWithHistory("memory_working_set", 256*1024*1024, SignalHistory{Points: 24, P50: 128 * 1024 * 1024, P95: 256 * 1024 * 1024, Max: 512 * 1024 * 1024}),
		},
		Availability: AvailabilityReport{ReplicaFloor: 2},
	}
	workload := config.WorkloadSpec{
		Scaling: config.ScalingSpec{Replicas: true, CPU: true, Memory: true},
		Bounds: config.BoundsSpec{
			Replicas: config.ReplicaBounds{Min: 1, Max: 6},
		},
	}

	got := buildRecommendation(workload, report, nil)
	if got.ReplicaDecision == nil {
		t.Fatal("ReplicaDecision is nil")
	}
	if got.RecommendedReplicas <= got.CurrentReplicas {
		t.Fatalf("RecommendedReplicas = %d, want scale-up; reasons=%#v", got.RecommendedReplicas, got.ReasonCodes)
	}
	if !containsSubstring(got.ReplicaDecision.Basis, "traffic_forecast") || !containsSubstring(got.ReplicaDecision.Basis, "latency") || !containsSubstring(got.ReplicaDecision.Basis, "availability_floor") {
		t.Fatalf("ReplicaDecision.Basis = %q, want traffic+latency and availability floor", got.ReplicaDecision.Basis)
	}
	if !hasLearnedDecision(got.Learning.Decisions, "replicas.multi_signal") {
		t.Fatalf("missing replicas.multi_signal decision: %#v", got.Learning.Decisions)
	}
}

func TestBuildRecommendationDecreasesOverReservedRequestsWithHistory(t *testing.T) {
	report := WorkloadReport{
		Replicas: 2,
		Containers: []ContainerReport{
			{
				Name:               "imgproxy",
				CPURequest:         "500m",
				CPURequestCores:    0.5,
				MemoryRequest:      "512Mi",
				MemoryRequestBytes: 512 * 1024 * 1024,
			},
		},
		MetricsCondition: "healthy",
		MetricSignals: []SignalReport{
			sampleSignalWithHistory("request_rate", 5, SignalHistory{Points: 24, P50: 4, P95: 6, Max: 8}),
			sampleSignalWithHistory("cpu_usage", 0.1, SignalHistory{Points: 24, P50: 0.08, P95: 0.12, Max: 0.15}),
			sampleSignalWithHistory("memory_working_set", 128*1024*1024, SignalHistory{Points: 24, P50: 120 * 1024 * 1024, P95: 160 * 1024 * 1024, Max: 180 * 1024 * 1024}),
		},
	}
	workload := config.WorkloadSpec{
		Scaling: config.ScalingSpec{Replicas: true, CPU: true, Memory: true},
		Bounds: config.BoundsSpec{
			Replicas: config.ReplicaBounds{Min: 2, Max: 6},
			CPU:      config.ChangeBounds{MaxDecreasePercent: 20},
			Memory:   config.ChangeBounds{MaxDecreasePercent: 15},
		},
	}

	got := buildRecommendation(workload, report, nil)
	if got.RecommendedCPURequest != "400m" {
		t.Fatalf("RecommendedCPURequest = %q, want 400m", got.RecommendedCPURequest)
	}
	if got.RecommendedMemoryRequest != "436Mi" {
		t.Fatalf("RecommendedMemoryRequest = %q, want 436Mi", got.RecommendedMemoryRequest)
	}
}

func TestApplyMinimumResourceChangeThresholdSuppressesNoise(t *testing.T) {
	workload := config.WorkloadSpec{
		Scaling: config.ScalingSpec{CPU: true, Memory: true},
		Bounds: config.BoundsSpec{
			CPU:    config.ChangeBounds{MinChangePercent: 5},
			Memory: config.ChangeBounds{MinChangePercent: 5},
		},
	}
	recommendation := Recommendation{
		CurrentCPURequest:        "40m",
		RecommendedCPURequest:    "30m",
		CurrentMemoryRequest:     "76Mi",
		RecommendedMemoryRequest: "77Mi",
	}

	applyMinimumResourceChangeThreshold(workload, &recommendation)

	if recommendation.RecommendedCPURequest != "30m" {
		t.Fatalf("RecommendedCPURequest = %q, want meaningful CPU change retained", recommendation.RecommendedCPURequest)
	}
	if recommendation.RecommendedMemoryRequest != "76Mi" {
		t.Fatalf("RecommendedMemoryRequest = %q, want current request held by threshold", recommendation.RecommendedMemoryRequest)
	}
	if !hasReasonPrefix(recommendation, "memory_request_change_below_min_percent:") {
		t.Fatalf("ReasonCodes missing min-change threshold: %#v", recommendation.ReasonCodes)
	}
}

func TestBuildWasteScoreCalculatesResourceHourReduction(t *testing.T) {
	score := buildWasteScore(Recommendation{
		CurrentReplicas:          2,
		RecommendedReplicas:      1,
		CurrentCPURequest:        "500m",
		RecommendedCPURequest:    "400m",
		CurrentMemoryRequest:     "1Gi",
		RecommendedMemoryRequest: "768Mi",
	})

	if score.HourlyReduction.CPUCoreHours != 0.6 {
		t.Fatalf("CPU hourly reduction = %.3f, want 0.6", score.HourlyReduction.CPUCoreHours)
	}
	if score.HourlyReduction.MemoryGiBHours != 1.25 {
		t.Fatalf("memory hourly reduction = %.3f, want 1.25", score.HourlyReduction.MemoryGiBHours)
	}
	if score.HourlyReduction.ReplicaHours != 1 {
		t.Fatalf("replica hourly reduction = %.3f, want 1", score.HourlyReduction.ReplicaHours)
	}
	if score.MonthlyReduction.CPUCoreHours != 438 {
		t.Fatalf("monthly CPU reduction = %.3f, want 438", score.MonthlyReduction.CPUCoreHours)
	}
	if score.MonthlyReduction.MemoryGiBHours != 912.5 {
		t.Fatalf("monthly memory reduction = %.3f, want 912.5", score.MonthlyReduction.MemoryGiBHours)
	}
	if !strings.Contains(score.Summary, "reduction") {
		t.Fatalf("summary = %q, want reduction", score.Summary)
	}
}

func TestBuildWasteScoreShowsCapacityIncrease(t *testing.T) {
	score := buildWasteScore(Recommendation{
		CurrentReplicas:          1,
		RecommendedReplicas:      2,
		CurrentCPURequest:        "500m",
		RecommendedCPURequest:    "600m",
		CurrentMemoryRequest:     "1Gi",
		RecommendedMemoryRequest: "1536Mi",
	})

	if score.HourlyReduction.CPUCoreHours >= 0 {
		t.Fatalf("CPU hourly reduction = %.3f, want negative capacity increase", score.HourlyReduction.CPUCoreHours)
	}
	if score.HourlyReduction.MemoryGiBHours >= 0 {
		t.Fatalf("memory hourly reduction = %.3f, want negative capacity increase", score.HourlyReduction.MemoryGiBHours)
	}
	if !strings.Contains(score.Summary, "capacity_increase") {
		t.Fatalf("summary = %q, want capacity_increase", score.Summary)
	}
}

func TestLearnedResourcePolicyAdaptsToWorkloadVolatility(t *testing.T) {
	stable := []SignalReport{
		sampleSignalWithHistory("cpu_usage", 0.10, SignalHistory{Points: 24, P50: 0.09, P95: 0.10, Max: 0.11}),
	}
	bursty := []SignalReport{
		sampleSignalWithHistory("cpu_usage", 0.10, SignalHistory{Points: 24, P50: 0.03, P95: 0.10, Max: 0.30}),
	}

	stablePolicy := learnedResourcePolicy(stable, "cpu_usage", cpuPolicyProfile(), config.ChangeBounds{})
	burstyPolicy := learnedResourcePolicy(bursty, "cpu_usage", cpuPolicyProfile(), config.ChangeBounds{})

	if stablePolicy.MaxDecreasePercent <= burstyPolicy.MaxDecreasePercent {
		t.Fatalf("stable max down = %d, bursty max down = %d; want stable larger", stablePolicy.MaxDecreasePercent, burstyPolicy.MaxDecreasePercent)
	}
	if stablePolicy.MaxIncreasePercent >= burstyPolicy.MaxIncreasePercent {
		t.Fatalf("stable max up = %d, bursty max up = %d; want bursty larger", stablePolicy.MaxIncreasePercent, burstyPolicy.MaxIncreasePercent)
	}
	if stablePolicy.TargetUtilization <= burstyPolicy.TargetUtilization {
		t.Fatalf("stable target = %.2f, bursty target = %.2f; want stable higher", stablePolicy.TargetUtilization, burstyPolicy.TargetUtilization)
	}
}

func TestBuildRecommendationScalesDownWhenDemandAndPolicyAllow(t *testing.T) {
	report := scaleDownCandidateReport()
	workload := config.WorkloadSpec{
		Scaling: config.ScalingSpec{Replicas: true, CPU: true, Memory: true},
		Bounds: config.BoundsSpec{
			Replicas: config.ReplicaBounds{Min: 1, Max: 4},
		},
	}

	got := buildRecommendation(workload, report, nil)
	if got.RecommendedReplicas != 1 {
		t.Fatalf("RecommendedReplicas = %d, want 1", got.RecommendedReplicas)
	}
}

func TestBuildRecommendationBlocksChangeWhenPrometheusHistoryIsSparse(t *testing.T) {
	report := scaleDownCandidateReport()
	report.MetricSignals = []SignalReport{
		sparseSignal("available_replicas", 2, 2, 2, 2),
		sparseSignal("request_rate", 0.25, 0.2, 0.5, 2),
		sparseSignal("cpu_usage", 1.5, 1.1, 1.5, 1.6),
		sparseSignal("memory_working_set", 128*1024*1024, 96*1024*1024, 128*1024*1024, 160*1024*1024),
	}
	workload := config.WorkloadSpec{
		Scaling: config.ScalingSpec{Replicas: true, CPU: true, Memory: true},
		Bounds: config.BoundsSpec{
			Replicas: config.ReplicaBounds{Min: 1, Max: 4},
		},
	}

	got := buildRecommendation(workload, report, nil)
	if got.Confidence >= got.ConfidenceAssessment.MinAutoCommit {
		t.Fatalf("confidence = %.2f, want below %.2f", got.Confidence, got.ConfidenceAssessment.MinAutoCommit)
	}
	if got.RecommendedCPURequest == got.CurrentCPURequest {
		t.Fatal("CPU request did not change; test needs an actionable recommendation")
	}
	if !got.Blocked {
		t.Fatal("Blocked = false, want true for sparse Prometheus data")
	}
	if !hasReasonPrefix(got, "confidence_below_min_auto_commit") {
		t.Fatalf("missing confidence block reason: %#v", got.ReasonCodes)
	}
	if len(got.ConfidenceAssessment.Signals) == 0 {
		t.Fatal("confidence signals missing")
	}
}

func TestBuildRecommendationHonorsConfiguredMinAutoCommitConfidence(t *testing.T) {
	report := scaleDownCandidateReport()
	workload := config.WorkloadSpec{
		Scaling: config.ScalingSpec{Replicas: true, CPU: true, Memory: true},
		Bounds: config.BoundsSpec{
			Replicas: config.ReplicaBounds{Min: 1, Max: 4},
		},
		Policy: config.PolicySpec{
			Confidence: config.ConfidencePolicySpec{MinAutoCommit: 0.96},
		},
	}

	got := buildRecommendation(workload, report, nil)
	if got.ConfidenceAssessment.MinAutoCommit != 0.96 {
		t.Fatalf("MinAutoCommit = %.2f, want 0.96", got.ConfidenceAssessment.MinAutoCommit)
	}
	if !got.Blocked {
		t.Fatal("Blocked = false, want true when configured confidence threshold is higher than adjusted confidence")
	}
}

func TestBuildRecommendationHonorsAvailabilityFloor(t *testing.T) {
	report := scaleDownCandidateReport()
	report.Availability = AvailabilityReport{
		ReplicaFloor:                 2,
		Public:                       true,
		Services:                     []string{"web"},
		ReadyEndpoints:               2,
		ReadyNodes:                   2,
		RollingUpdateZeroUnavailable: true,
		Reasons:                      []string{"public_service", "multi_node_ready_endpoints", "zero_unavailable_rollout"},
	}
	workload := config.WorkloadSpec{
		Scaling: config.ScalingSpec{Replicas: true, CPU: true, Memory: true},
		Bounds: config.BoundsSpec{
			Replicas: config.ReplicaBounds{Min: 1, Max: 4},
		},
	}

	got := buildRecommendation(workload, report, nil)
	if got.RecommendedReplicas != 2 {
		t.Fatalf("RecommendedReplicas = %d, want 2", got.RecommendedReplicas)
	}
	if !hasReasonPrefix(got, "availability_replica_floor:2") {
		t.Fatalf("ReasonCodes missing availability floor: %#v", got.ReasonCodes)
	}
	if !hasLearnedDecision(got.Learning.Decisions, "replicas.availability") {
		t.Fatal("Learning.Decisions missing replicas.availability")
	}
}

func TestBuildRecommendationHonorsPDBReplicaFloor(t *testing.T) {
	report := scaleDownCandidateReport()
	report.PDBs = []PDBReport{
		{Name: "web", MinAvailable: "2", MinimumReplicaFloor: 2, ScaleDownFloorEnforced: true},
	}
	workload := config.WorkloadSpec{
		Scaling: config.ScalingSpec{Replicas: true, CPU: true, Memory: true},
		Bounds: config.BoundsSpec{
			Replicas: config.ReplicaBounds{Min: 1, Max: 4},
		},
	}

	got := buildRecommendation(workload, report, nil)
	if got.RecommendedReplicas != 2 {
		t.Fatalf("RecommendedReplicas = %d, want 2", got.RecommendedReplicas)
	}
}

func TestBuildRecommendationPrefersFewerLargerPodsWhenTotalRequestIsLower(t *testing.T) {
	report := WorkloadReport{
		Replicas:      3,
		ReadyReplicas: 3,
		Containers: []ContainerReport{
			{
				Name:               "web",
				CPURequest:         "240m",
				CPURequestCores:    0.24,
				MemoryRequest:      "4938Mi",
				MemoryRequestBytes: 4938 * 1024 * 1024,
			},
		},
		Availability: AvailabilityReport{
			ReplicaFloor:                 2,
			Public:                       true,
			ReadyEndpoints:               3,
			ReadyNodes:                   3,
			RollingUpdateZeroUnavailable: true,
		},
		MetricsCondition: "healthy",
		MetricSignals: []SignalReport{
			sampleSignalWithHistory("available_replicas", 3, SignalHistory{Points: 73, P50: 2, P95: 3, Max: 3}),
			sampleSignalWithHistory("request_rate", 1.84, SignalHistory{Points: 73, P50: 1.4, P95: 1.91, Max: 2.28}),
			sampleSignalWithHistory("latency_p95", 0.48, SignalHistory{Points: 73, P50: 0.24, P95: 0.61, Max: 3.02}),
			sampleSignalWithHistory("cpu_usage", 0.22, SignalHistory{Points: 73, P50: 0.14, P95: 0.25, Max: 0.70}),
			sampleSignalWithHistory("memory_working_set", 1.72*1024*1024*1024, SignalHistory{Points: 73, P50: 3.71 * 1024 * 1024 * 1024, P95: 7.69 * 1024 * 1024 * 1024, Max: 8.48 * 1024 * 1024 * 1024}),
		},
	}
	workload := config.WorkloadSpec{
		Scaling: config.ScalingSpec{Replicas: true, CPU: true, Memory: true},
		Bounds: config.BoundsSpec{
			Replicas: config.ReplicaBounds{Min: 1, Max: 4},
		},
	}

	got := buildRecommendation(workload, report, nil)
	if got.RecommendedReplicas != 2 {
		t.Fatalf("RecommendedReplicas = %d, want 2; reasons=%#v", got.RecommendedReplicas, got.ReasonCodes)
	}
	if got.RecommendedMemoryRequest == "" || got.RecommendedMemoryRequest == "4938Mi" {
		t.Fatalf("RecommendedMemoryRequest = %q, want per-pod memory adjustment", got.RecommendedMemoryRequest)
	}
	if !hasReasonPrefix(got, "replica_joint_optimizer_selected:replicas=2") {
		t.Fatalf("ReasonCodes missing joint optimizer selection: %#v", got.ReasonCodes)
	}
}

func TestBuildRecommendationLearningRespectsDisabledReplicaManagement(t *testing.T) {
	report := scaleDownCandidateReport()
	workload := config.WorkloadSpec{
		Scaling: config.ScalingSpec{Replicas: false, CPU: true, Memory: true},
	}

	got := buildRecommendation(workload, report, nil)
	if hasLearnedDecision(got.Learning.Decisions, "replicas.memory") {
		t.Fatal("Learning.Decisions includes replicas.memory with replica management disabled")
	}
	if !hasLearnedDecision(got.Learning.Decisions, "replicas.management") {
		t.Fatal("Learning.Decisions missing replicas.management")
	}
	if !hasLearnedDecision(got.Learning.Decisions, "resources.cpu_request") {
		t.Fatal("Learning.Decisions missing resources.cpu_request")
	}
	if !hasLearnedDecision(got.Learning.Decisions, "resources.memory_request") {
		t.Fatal("Learning.Decisions missing resources.memory_request")
	}
}

func hasLearnedDecision(decisions []LearnedDecision, subject string) bool {
	for _, decision := range decisions {
		if decision.Subject == subject {
			return true
		}
	}
	return false
}

func containsSubstring(value, want string) bool {
	return strings.Contains(value, want)
}

func TestClassifyAnomaly(t *testing.T) {
	sample := 12.2
	signal := SignalReport{
		Sample: &sample,
		History: &SignalHistory{
			Points: 10,
			P95:    10,
			Max:    12,
		},
	}
	got := classifyAnomaly(signal)
	if got.State != "warning" {
		t.Fatalf("State = %q, want warning for max breach", got.State)
	}

	sample = 12.6
	got = classifyAnomaly(signal)
	if got.State != "warning" {
		t.Fatalf("State = %q, want warning for p95 breach", got.State)
	}

	sample = 14.0
	got = classifyAnomaly(signal)
	if got.State != "critical" {
		t.Fatalf("State = %q, want critical", got.State)
	}

	sample = 11.0
	got = classifyAnomaly(signal)
	if got.State != "normal" {
		t.Fatalf("State = %q, want normal", got.State)
	}
}

func TestBuildSignalForecastCreatesHorizonBands(t *testing.T) {
	samples := []prom.Sample{
		{Timestamp: 0, Value: 1},
		{Timestamp: 3600, Value: 2},
		{Timestamp: 7200, Value: 3},
	}
	current := 3.0
	forecast := buildSignalForecast(samples, &current, SignalHistory{Points: 3, P50: 2, P95: 3, Max: 3})
	if forecast == nil || len(forecast.Horizons) != 3 {
		t.Fatalf("forecast = %#v, want three horizons", forecast)
	}
	if forecast.TrendSlopePerHour < 0.99 || forecast.TrendSlopePerHour > 1.01 {
		t.Fatalf("TrendSlopePerHour = %.4f, want about 1", forecast.TrendSlopePerHour)
	}
	var oneHour ForecastHorizon
	for _, horizon := range forecast.Horizons {
		if horizon.Horizon == "next_1h" {
			oneHour = horizon
		}
	}
	if oneHour.Forecast < 3.99 || oneHour.Forecast > 4.01 {
		t.Fatalf("next_1h forecast = %.4f, want about 4", oneHour.Forecast)
	}
	if oneHour.P95BandLow > oneHour.Forecast || oneHour.P95BandHigh < oneHour.Forecast {
		t.Fatalf("band does not contain forecast: %#v", oneHour)
	}
}

func TestTrafficDecisionUsesThirtyMinuteForecastBand(t *testing.T) {
	request := 2.2
	report := WorkloadReport{
		Replicas: 2,
		MetricSignals: []SignalReport{
			{
				Name:    "request_rate",
				Healthy: true,
				Sample:  &request,
				History: &SignalHistory{Points: 24, P50: 1, P95: 2, Max: 10},
				Forecast: &SignalForecast{Horizons: []ForecastHorizon{
					{Horizon: "next_30m", Forecast: 7, P95BandLow: 6, P95BandHigh: 8, Confidence: 0.82},
				}},
			},
			{
				Name:    "latency_p95",
				Healthy: true,
				Sample:  &request,
				Anomaly: AnomalyStatus{State: "warning", Reason: "test"},
			},
		},
	}

	decision := trafficReplicaDecision(report, nil, 2)
	if decision.ForecastBasis != "next_30m_p95_band_high" {
		t.Fatalf("ForecastBasis = %q, want next_30m_p95_band_high", decision.ForecastBasis)
	}
	if decision.Replicas != 3 {
		t.Fatalf("Replicas = %d, want 3; decision=%#v", decision.Replicas, decision)
	}
}

func TestFormatDuration(t *testing.T) {
	tests := map[string]string{
		"6h0m0s": "6h",
		"5m0s":   "5m",
	}
	for input, want := range tests {
		duration, err := time.ParseDuration(input)
		if err != nil {
			t.Fatal(err)
		}
		if got := formatDuration(duration); got != want {
			t.Fatalf("formatDuration(%s) = %q, want %q", input, got, want)
		}
	}
}

func sampleSignal(name string, value float64) SignalReport {
	return SignalReport{Name: name, Healthy: true, Sample: &value}
}

func sampleSignalWithHistory(name string, value float64, history SignalHistory) SignalReport {
	return SignalReport{Name: name, Healthy: true, Sample: &value, History: &history}
}

func sparseSignal(name string, value, p50, p95, max float64) SignalReport {
	signal := sampleSignalWithHistory(name, value, SignalHistory{
		Window:         "6h",
		Step:           "5m",
		Points:         3,
		ExpectedPoints: 72,
		Coverage:       0.04,
		P50:            p50,
		P95:            p95,
		Max:            max,
	})
	signal.Required = true
	return signal
}

func scaleDownCandidateReport() WorkloadReport {
	return WorkloadReport{
		Replicas:      2,
		ReadyReplicas: 2,
		Containers: []ContainerReport{
			{
				Name:               "web",
				CPURequest:         "500m",
				CPURequestCores:    0.5,
				MemoryRequest:      "512Mi",
				MemoryRequestBytes: 512 * 1024 * 1024,
			},
		},
		MetricsCondition: "healthy",
		MetricSignals: []SignalReport{
			sampleSignalWithHistory("available_replicas", 2, SignalHistory{Points: 24, P50: 2, P95: 2, Max: 2}),
			sampleSignalWithHistory("request_rate", 0.25, SignalHistory{Points: 24, P50: 0.2, P95: 0.5, Max: 2}),
			sampleSignalWithHistory("cpu_usage", 0.05, SignalHistory{Points: 24, P50: 0.04, P95: 0.08, Max: 0.1}),
			sampleSignalWithHistory("memory_working_set", 128*1024*1024, SignalHistory{Points: 24, P50: 96 * 1024 * 1024, P95: 128 * 1024 * 1024, Max: 160 * 1024 * 1024}),
		},
	}
}
