package analyzer

import (
	"fmt"
	"math"
	"strings"

	"github.com/abhi1693/k8s-recommendation-engine/internal/config"
)

const (
	SafetyLowRisk    = "low_risk"
	SafetyMediumRisk = "medium_risk"
	SafetyHighRisk   = "high_risk"
)

func AttachSafetyAssessments(report *Report) {
	AttachSafetyAssessmentsWithPolicy(report, nil)
}

func AttachSafetyAssessmentsWithPolicy(report *Report, profile *config.ApplicationProfile) {
	if report == nil {
		return
	}
	policies := workloadPoliciesForSafety(profile)
	for index := range report.Workloads {
		workload := &report.Workloads[index]
		workload.Recommendation.Safety = classifyRecommendationSafety(*workload, report.SharedSignals)
		policy := policies[workload.Name]
		applySafetyPolicy(workload, policy)
		if !workload.Recommendation.Safety.AutoCommitAllowed {
			reason := safetyBlockReason(workload.Recommendation.Safety)
			workload.Recommendation.Blocked = true
			if !stringSliceContains(workload.Recommendation.BlockReasons, reason) {
				workload.Recommendation.BlockReasons = append(workload.Recommendation.BlockReasons, reason)
			}
			code := "safety_auto_commit_blocked"
			if workload.Recommendation.Safety.Classification == SafetyHighRisk {
				code = "safety_high_risk_auto_commit_blocked"
			}
			if !stringSliceContains(workload.Recommendation.ReasonCodes, code) {
				workload.Recommendation.ReasonCodes = append(workload.Recommendation.ReasonCodes, code)
			}
		}
	}
}

func classifyRecommendationSafety(workload WorkloadReport, sharedSignals []SignalReport) SafetyAssessment {
	factors := []SafetyFactor{
		resourceDecreaseSafety(workload),
		forecastAccuracySafety(workload.Recommendation.Learning.Persistent),
		workloadHealthSafety(workload),
		rolloutSafety(workload),
		memoryHeadroomSafety(workload),
		trafficAnomalySafety(workload.MetricSignals, sharedSignals),
	}
	classification := SafetyLowRisk
	var reasons []string
	for _, factor := range factors {
		switch factor.Classification {
		case SafetyHighRisk:
			classification = SafetyHighRisk
		case SafetyMediumRisk:
			if classification != SafetyHighRisk {
				classification = SafetyMediumRisk
			}
		}
		if factor.Classification != SafetyLowRisk {
			reasons = append(reasons, factor.Name+": "+factor.Reason)
		}
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "all safety factors are low risk")
	}
	return SafetyAssessment{
		Classification:    classification,
		AutoCommitAllowed: classification != SafetyHighRisk,
		Reasons:           reasons,
		Factors:           factors,
	}
}

func workloadPoliciesForSafety(profile *config.ApplicationProfile) map[string]config.PolicySpec {
	policies := map[string]config.PolicySpec{}
	if profile == nil {
		return policies
	}
	for _, workload := range profile.Spec.Workloads {
		policies[workload.Name] = workload.Policy
	}
	return policies
}

func applySafetyPolicy(workload *WorkloadReport, policy config.PolicySpec) {
	if workload == nil {
		return
	}
	safety := &workload.Recommendation.Safety
	allowed := policy.Safety.AllowAutoCommit
	if len(allowed) == 0 {
		allowed = []string{SafetyLowRisk, SafetyMediumRisk}
	}
	safety.AutoCommitAllowed = riskListContains(allowed, safety.Classification)
	if !safety.AutoCommitAllowed {
		safety.Reasons = appendSafetyReason(safety.Reasons, fmt.Sprintf("policy allowAutoCommit excludes %s", safety.Classification))
	}

	maxDecreaseRisk := policy.Safety.MaxDecreaseRisk
	if maxDecreaseRisk == "" {
		maxDecreaseRisk = SafetyHighRisk
	}
	if factor, ok := safetyFactorByName(safety.Factors, "resource_decrease_size"); ok && riskRank(factor.Classification) > riskRank(maxDecreaseRisk) {
		safety.AutoCommitAllowed = false
		safety.Reasons = appendSafetyReason(safety.Reasons, fmt.Sprintf("resource decrease risk %s exceeds policy maxDecreaseRisk %s", factor.Classification, maxDecreaseRisk))
	}
	if policy.AvailabilityRecovery.Enabled && availabilityRecoveryChange(workload.Recommendation) {
		safety.AutoCommitAllowed = true
		safety.Reasons = appendSafetyReason(safety.Reasons, "availability recovery increase is allowed during an outage")
		if !stringSliceContains(workload.Recommendation.ReasonCodes, "availability_recovery_safety_bypass") {
			workload.Recommendation.ReasonCodes = append(workload.Recommendation.ReasonCodes, "availability_recovery_safety_bypass")
		}
	}
}

func safetyBlockReason(safety SafetyAssessment) string {
	if safety.Classification == SafetyHighRisk {
		return "safety classification high_risk blocks auto-commit"
	}
	if len(safety.Reasons) > 0 {
		for _, reason := range safety.Reasons {
			if strings.Contains(reason, "policy allowAutoCommit") || strings.Contains(reason, "maxDecreaseRisk") {
				return "safety policy blocks auto-commit: " + reason
			}
		}
	}
	return "safety policy blocks auto-commit"
}

func riskListContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func safetyFactorByName(factors []SafetyFactor, name string) (SafetyFactor, bool) {
	for _, factor := range factors {
		if factor.Name == name {
			return factor, true
		}
	}
	return SafetyFactor{}, false
}

func riskRank(risk string) int {
	switch risk {
	case SafetyLowRisk:
		return 1
	case SafetyMediumRisk:
		return 2
	case SafetyHighRisk:
		return 3
	default:
		return 3
	}
}

func appendSafetyReason(reasons []string, reason string) []string {
	if !stringSliceContains(reasons, reason) {
		reasons = append(reasons, reason)
	}
	return reasons
}

func resourceDecreaseSafety(workload WorkloadReport) SafetyFactor {
	rec := workload.Recommendation
	maxDecrease := 0.0
	var parts []string
	if rec.CurrentReplicas > 0 && rec.RecommendedReplicas < rec.CurrentReplicas {
		decrease := float64(rec.CurrentReplicas-rec.RecommendedReplicas) / float64(rec.CurrentReplicas) * 100
		maxDecrease = math.Max(maxDecrease, decrease)
		parts = append(parts, fmt.Sprintf("replicas %.1f%%", decrease))
	}
	if decrease, ok := resourceDecreasePercent(rec.CurrentCPURequest, rec.RecommendedCPURequest); ok {
		maxDecrease = math.Max(maxDecrease, decrease)
		parts = append(parts, fmt.Sprintf("cpu %.1f%%", decrease))
	}
	if decrease, ok := resourceDecreasePercent(rec.CurrentMemoryRequest, rec.RecommendedMemoryRequest); ok {
		maxDecrease = math.Max(maxDecrease, decrease)
		parts = append(parts, fmt.Sprintf("memory %.1f%%", decrease))
	}
	switch {
	case maxDecrease >= 35:
		return SafetyFactor{Name: "resource_decrease_size", Classification: SafetyHighRisk, Reason: "large decrease: " + strings.Join(parts, ", ")}
	case maxDecrease >= 15:
		return SafetyFactor{Name: "resource_decrease_size", Classification: SafetyMediumRisk, Reason: "moderate decrease: " + strings.Join(parts, ", ")}
	default:
		reason := "no material resource decrease"
		if len(parts) > 0 {
			reason = "small decrease: " + strings.Join(parts, ", ")
		}
		return SafetyFactor{Name: "resource_decrease_size", Classification: SafetyLowRisk, Reason: reason}
	}
}

func forecastAccuracySafety(persistent *PersistentLearning) SafetyFactor {
	if persistent == nil || persistent.ForecastAccuracy == nil || persistent.ForecastAccuracy.Samples == 0 {
		return SafetyFactor{Name: "prior_forecast_accuracy", Classification: SafetyMediumRisk, Reason: "no prior forecast accuracy samples"}
	}
	accuracy := persistent.ForecastAccuracy
	classification := SafetyLowRisk
	var reasons []string
	for _, score := range accuracy.Signals {
		if score.Samples < 3 {
			continue
		}
		switch {
		case score.MeanAbsolutePercentError > 0.50:
			classification = SafetyHighRisk
			reasons = append(reasons, fmt.Sprintf("%s mape %.1f%%", score.Signal, score.MeanAbsolutePercentError*100))
		case score.MeanAbsolutePercentError > 0.30 && classification != SafetyHighRisk:
			classification = SafetyMediumRisk
			reasons = append(reasons, fmt.Sprintf("%s mape %.1f%%", score.Signal, score.MeanAbsolutePercentError*100))
		}
		if score.MeanBiasPercent < -0.20 {
			classification = SafetyHighRisk
			reasons = append(reasons, fmt.Sprintf("%s underestimated by %.1f%%", score.Signal, math.Abs(score.MeanBiasPercent)*100))
		} else if score.MeanBiasPercent < -0.10 && classification != SafetyHighRisk {
			classification = SafetyMediumRisk
			reasons = append(reasons, fmt.Sprintf("%s underestimated by %.1f%%", score.Signal, math.Abs(score.MeanBiasPercent)*100))
		}
		if score.Classification == "noisy" && classification != SafetyHighRisk {
			classification = SafetyMediumRisk
			reasons = append(reasons, score.Signal+" forecast is noisy")
		}
	}
	if len(reasons) == 0 {
		reasons = append(reasons, fmt.Sprintf("samples=%d confidenceAdjustment=%+.3f", accuracy.Samples, accuracy.ConfidenceAdjustment))
	}
	return SafetyFactor{Name: "prior_forecast_accuracy", Classification: classification, Reason: strings.Join(reasons, "; ")}
}

func workloadHealthSafety(workload WorkloadReport) SafetyFactor {
	if workload.Replicas > 0 && workload.ReadyReplicas == 0 {
		return SafetyFactor{Name: "workload_health", Classification: SafetyHighRisk, Reason: "no ready replicas"}
	}
	switch workload.MetricsCondition {
	case "unhealthy":
		return SafetyFactor{Name: "workload_health", Classification: SafetyHighRisk, Reason: "metrics condition is unhealthy"}
	case "degraded":
		return SafetyFactor{Name: "workload_health", Classification: SafetyMediumRisk, Reason: "metrics condition is degraded"}
	}
	if workload.ReadyReplicas < workload.Replicas {
		return SafetyFactor{Name: "workload_health", Classification: SafetyMediumRisk, Reason: fmt.Sprintf("ready replicas %d/%d", workload.ReadyReplicas, workload.Replicas)}
	}
	return SafetyFactor{Name: "workload_health", Classification: SafetyLowRisk, Reason: "workload is healthy"}
}

func rolloutSafety(workload WorkloadReport) SafetyFactor {
	if workload.Rollout.Evaluated && !workload.Rollout.Settled {
		reason := strings.Join(workload.Rollout.Reasons, ", ")
		if reason == "" {
			reason = "rollout is not settled"
		}
		return SafetyFactor{Name: "rollout_history", Classification: SafetyHighRisk, Reason: reason}
	}
	if persistent := workload.Recommendation.Learning.Persistent; persistent != nil && persistent.LastOutcome != nil {
		switch persistent.LastOutcome.Status {
		case "unsafe", "regressed", "too_aggressive":
			return SafetyFactor{Name: "rollout_history", Classification: SafetyHighRisk, Reason: "previous outcome was " + persistent.LastOutcome.Status}
		case "partially_applied", "not_applied":
			return SafetyFactor{Name: "rollout_history", Classification: SafetyMediumRisk, Reason: "previous outcome was " + persistent.LastOutcome.Status}
		}
	}
	return SafetyFactor{Name: "rollout_history", Classification: SafetyLowRisk, Reason: "rollout settled and no unsafe prior outcome"}
}

func memoryHeadroomSafety(workload WorkloadReport) SafetyFactor {
	rec := workload.Recommendation
	if rec.CurrentMemoryRequest == "" || rec.RecommendedMemoryRequest == "" || rec.CurrentMemoryRequest == rec.RecommendedMemoryRequest {
		return SafetyFactor{Name: "memory_headroom", Classification: SafetyLowRisk, Reason: "memory request is unchanged"}
	}
	if decrease, ok := resourceDecreasePercent(rec.CurrentMemoryRequest, rec.RecommendedMemoryRequest); !ok || decrease <= 0 {
		return SafetyFactor{Name: "memory_headroom", Classification: SafetyLowRisk, Reason: "memory request is not decreasing"}
	}
	recommendedBytes, ok := memoryRequestBytes(rec.RecommendedMemoryRequest)
	if !ok || recommendedBytes <= 0 {
		return SafetyFactor{Name: "memory_headroom", Classification: SafetyMediumRisk, Reason: "recommended memory request cannot be parsed"}
	}
	replicas := float64(maxInt32(rec.RecommendedReplicas, 1))
	totalRecommended := recommendedBytes * replicas
	memoryDemand := 0.0
	if history := signalHistory(workload.MetricSignals, "memory_working_set"); history != nil {
		memoryDemand = math.Max(memoryDemand, history.P95)
	}
	if sample, ok := signalSample(workload.MetricSignals, "memory_working_set"); ok {
		memoryDemand = math.Max(memoryDemand, sample)
	}
	if memoryDemand <= 0 {
		return SafetyFactor{Name: "memory_headroom", Classification: SafetyMediumRisk, Reason: "memory demand is unknown for decrease"}
	}
	headroom := (totalRecommended - memoryDemand) / totalRecommended
	switch {
	case headroom < 0.10:
		return SafetyFactor{Name: "memory_headroom", Classification: SafetyHighRisk, Reason: fmt.Sprintf("only %.1f%% headroom after memory decrease", headroom*100)}
	case headroom < 0.20:
		return SafetyFactor{Name: "memory_headroom", Classification: SafetyMediumRisk, Reason: fmt.Sprintf("%.1f%% headroom after memory decrease", headroom*100)}
	default:
		return SafetyFactor{Name: "memory_headroom", Classification: SafetyLowRisk, Reason: fmt.Sprintf("%.1f%% headroom after memory decrease", headroom*100)}
	}
}

func trafficAnomalySafety(workloadSignals, sharedSignals []SignalReport) SafetyFactor {
	var medium, high []string
	for _, signal := range append(append([]SignalReport(nil), workloadSignals...), sharedSignals...) {
		if !trafficSafetySignal(signal.Name) {
			continue
		}
		switch signal.Anomaly.State {
		case "critical":
			high = append(high, signal.Name+": "+signal.Anomaly.Reason)
		case "warning":
			medium = append(medium, signal.Name+": "+signal.Anomaly.Reason)
		}
	}
	if len(high) > 0 {
		return SafetyFactor{Name: "traffic_anomaly_state", Classification: SafetyHighRisk, Reason: strings.Join(high, "; ")}
	}
	if len(medium) > 0 {
		return SafetyFactor{Name: "traffic_anomaly_state", Classification: SafetyMediumRisk, Reason: strings.Join(medium, "; ")}
	}
	return SafetyFactor{Name: "traffic_anomaly_state", Classification: SafetyLowRisk, Reason: "no active traffic anomaly"}
}

func trafficSafetySignal(name string) bool {
	switch name {
	case "request_rate", "latency_p95", "error_rate", "concurrent_requests":
		return true
	default:
		return false
	}
}

func resourceDecreasePercent(current, recommended string) (float64, bool) {
	if current == "" || recommended == "" || current == recommended {
		return 0, false
	}
	currentValue, currentOK := comparableResourceAny(current)
	recommendedValue, recommendedOK := comparableResourceAny(recommended)
	if !currentOK || !recommendedOK || currentValue <= 0 || recommendedValue >= currentValue {
		return 0, false
	}
	return (currentValue - recommendedValue) / currentValue * 100, true
}

func comparableResourceAny(value string) (float64, bool) {
	if cpu, ok := cpuRequestCores(value); ok {
		return cpu, true
	}
	if memory, ok := memoryRequestBytes(value); ok {
		return memory, true
	}
	return 0, false
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
