package analyzer

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

func WriteTextReport(w io.Writer, report *Report) error {
	if _, err := fmt.Fprintf(w, "Application: %s\nNamespace:   %s\nGenerated:   %s\n\n", report.Application, report.Namespace, report.GeneratedAt.Format("2006-01-02T15:04:05Z")); err != nil {
		return err
	}
	if err := writeTextCapabilities(w, report.ClusterCapabilities); err != nil {
		return err
	}

	if len(report.SharedSignals) > 0 {
		if _, err := fmt.Fprintln(w, "Shared Signals:"); err != nil {
			return err
		}
		for _, signal := range report.SharedSignals {
			if _, err := fmt.Fprintf(w, "  - %-32s %s\n", signal.Name, signalStatus(signal)); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintln(w, "Workloads:"); err != nil {
		return err
	}
	for _, workload := range report.Workloads {
		if _, err := fmt.Fprintf(w, "  - %s/%s (%s)\n", workload.Namespace, workload.Deployment, workload.Name); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "      replicas: %d ready: %d fleet: %t helm: %s\n", workload.Replicas, workload.ReadyReplicas, workload.FleetManaged, emptyDash(workload.HelmRelease)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "      scaling: replicas=%t cpu=%t memory=%t commitBlocked=%t\n", workload.Scaling.Replicas, workload.Scaling.CPU, workload.Scaling.Memory, workload.CommitBlocked); err != nil {
			return err
		}
		if len(workload.BlockReasons) > 0 {
			if _, err := fmt.Fprintf(w, "      blockReasons: %s\n", strings.Join(workload.BlockReasons, "; ")); err != nil {
				return err
			}
		}
		if len(workload.Autoscalers) > 0 {
			var autoscalers []string
			for _, autoscaler := range workload.Autoscalers {
				autoscalers = append(autoscalers, fmt.Sprintf("%s/%s", autoscaler.Kind, autoscaler.Name))
			}
			if _, err := fmt.Fprintf(w, "      autoscalers: %s\n", strings.Join(autoscalers, ", ")); err != nil {
				return err
			}
		}
		if len(workload.PDBs) > 0 {
			var pdbs []string
			for _, pdb := range workload.PDBs {
				parts := []string{pdb.Name}
				if pdb.MinAvailable != "" {
					parts = append(parts, fmt.Sprintf("minAvailable=%s", pdb.MinAvailable))
				}
				if pdb.MaxUnavailable != "" {
					parts = append(parts, fmt.Sprintf("maxUnavailable=%s", pdb.MaxUnavailable))
				}
				if pdb.MinimumReplicaFloor > 0 {
					parts = append(parts, fmt.Sprintf("replicaFloor=%d", pdb.MinimumReplicaFloor))
				}
				parts = append(parts, fmt.Sprintf("disruptionsAllowed=%d", pdb.DisruptionsAllowed))
				pdbs = append(pdbs, strings.Join(parts, " "))
			}
			if _, err := fmt.Fprintf(w, "      pdbs: %s\n", strings.Join(pdbs, ", ")); err != nil {
				return err
			}
		}
		if workload.Availability.ReplicaFloor > 0 || workload.Availability.Public {
			if _, err := fmt.Fprintf(w, "      availability: floor=%d public=%t readyEndpoints=%d readyNodes=%d zeroUnavailable=%t reasons=%s\n",
				workload.Availability.ReplicaFloor,
				workload.Availability.Public,
				workload.Availability.ReadyEndpoints,
				workload.Availability.ReadyNodes,
				workload.Availability.RollingUpdateZeroUnavailable,
				emptyDash(strings.Join(workload.Availability.Reasons, ",")),
			); err != nil {
				return err
			}
		}
		if len(workload.Containers) > 0 {
			if _, err := fmt.Fprintln(w, "      containers:"); err != nil {
				return err
			}
			for _, container := range workload.Containers {
				if _, err := fmt.Fprintf(w, "        - %s cpu=%s memory=%s\n", container.Name, emptyDash(container.CPURequest), emptyDash(container.MemoryRequest)); err != nil {
					return err
				}
			}
		}
		if _, err := fmt.Fprintf(w, "      metrics: %s (%s)\n", workload.MetricsCondition, workload.MetricProfile); err != nil {
			return err
		}
		for _, signal := range workload.MetricSignals {
			if _, err := fmt.Fprintf(w, "        - %-32s %s\n", signal.Name, signalStatus(signal)); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w, "      recommendation:"); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "        mode: %s confidence: %.2f blocked: %t\n", workload.Recommendation.Mode, workload.Recommendation.Confidence, workload.Recommendation.Blocked); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "        replicas: %d -> %d\n", workload.Recommendation.CurrentReplicas, workload.Recommendation.RecommendedReplicas); err != nil {
			return err
		}
		if workload.Recommendation.CurrentCPURequest != "" || workload.Recommendation.RecommendedCPURequest != "" {
			if _, err := fmt.Fprintf(w, "        cpu request: %s -> %s\n", emptyDash(workload.Recommendation.CurrentCPURequest), emptyDash(workload.Recommendation.RecommendedCPURequest)); err != nil {
				return err
			}
		}
		if workload.Recommendation.CurrentMemoryRequest != "" || workload.Recommendation.RecommendedMemoryRequest != "" {
			if _, err := fmt.Fprintf(w, "        memory request: %s -> %s\n", emptyDash(workload.Recommendation.CurrentMemoryRequest), emptyDash(workload.Recommendation.RecommendedMemoryRequest)); err != nil {
				return err
			}
		}
		if len(workload.Recommendation.ReasonCodes) > 0 {
			if _, err := fmt.Fprintf(w, "        reasons: %s\n", strings.Join(workload.Recommendation.ReasonCodes, "; ")); err != nil {
				return err
			}
		}
		if err := writeTextLearning(w, workload.Recommendation.Learning); err != nil {
			return err
		}
		if workload.Recommendation.PatchPlan != nil {
			if err := writePatchPlan(w, workload.Recommendation.PatchPlan); err != nil {
				return err
			}
		}
	}

	_, err := fmt.Fprintf(w, "\nSummary: workloads=%d commitBlocked=%d metricsHealthy=%d metricsDegraded=%d metricsUnhealthy=%d\n",
		report.Summary.WorkloadsTotal,
		report.Summary.CommitBlocked,
		report.Summary.MetricsHealthy,
		report.Summary.MetricsDegraded,
		report.Summary.MetricsUnhealthy,
	)
	return err
}

func WritePrettyReport(w io.Writer, report *Report) error {
	if _, err := fmt.Fprintf(w, "K8s Recommendation Engine Report\n"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Application: %s   Namespace: %s   Generated: %s\n\n", report.Application, report.Namespace, report.GeneratedAt.Format("2006-01-02T15:04:05Z")); err != nil {
		return err
	}
	if err := writePrettyCapabilities(w, report.ClusterCapabilities); err != nil {
		return err
	}

	if err := writeCompactDecisionSummary(w, report); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}

	if len(report.SharedSignals) > 0 {
		if _, err := fmt.Fprintln(w, "Shared Signals"); err != nil {
			return err
		}
		if err := writePrettySignals(w, report.SharedSignals); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}

	for _, workload := range report.Workloads {
		if err := writePrettyWorkload(w, workload); err != nil {
			return err
		}
	}

	_, err := fmt.Fprintf(w, "Summary: workloads=%d commitBlocked=%d metricsHealthy=%d metricsDegraded=%d metricsUnhealthy=%d\n",
		report.Summary.WorkloadsTotal,
		report.Summary.CommitBlocked,
		report.Summary.MetricsHealthy,
		report.Summary.MetricsDegraded,
		report.Summary.MetricsUnhealthy,
	)
	return err
}

func WriteSummaryReport(w io.Writer, report *Report) error {
	if _, err := fmt.Fprintf(w, "K8s Recommendation Engine Summary\n"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Application: %s   Namespace: %s   Generated: %s\n\n", report.Application, report.Namespace, report.GeneratedAt.Format("2006-01-02T15:04:05Z")); err != nil {
		return err
	}
	if err := writeCompactDecisionSummary(w, report); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "\nSummary: workloads=%d commitBlocked=%d metricsHealthy=%d metricsDegraded=%d metricsUnhealthy=%d\n",
		report.Summary.WorkloadsTotal,
		report.Summary.CommitBlocked,
		report.Summary.MetricsHealthy,
		report.Summary.MetricsDegraded,
		report.Summary.MetricsUnhealthy,
	)
	return err
}

func WriteActionsReport(w io.Writer, report *Report, gitWorktreeEnabled bool) error {
	if _, err := fmt.Fprintf(w, "K8s Recommendation Engine Actions\n"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Application: %s   Namespace: %s   Generated: %s\n", report.Application, report.Namespace, report.GeneratedAt.Format("2006-01-02T15:04:05Z")); err != nil {
		return err
	}
	if !gitWorktreeEnabled {
		if _, err := fmt.Fprintln(w, "Git diff: unavailable (pass --git-worktree to render dry-run manifest diffs)"); err != nil {
			return err
		}
	}
	if report.Proposal != nil {
		if err := writeProposalSummary(w, report.Proposal); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}

	for _, workload := range report.Workloads {
		rec := workload.Recommendation
		plan := rec.PatchPlan
		eligible := patchPlanApplyable(plan)
		applyable := actionApplyable(report.Proposal, eligible)
		showChanges := actionShowChanges(report.Proposal, eligible)
		if _, err := fmt.Fprintf(w, "%s/%s\n", workload.Namespace, workload.Deployment); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "  apply: %s\n", yesNo(applyable)); err != nil {
			return err
		}
		if !applyable && eligible {
			if _, err := fmt.Fprintln(w, "  eligible: yes (proposal is blocked; no changes were written)"); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "  decision: %s confidence=%.2f outcome=%s\n", primaryDecision(rec), rec.Confidence, lastOutcome(rec)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "  gates: replicas=%s cpu=%s memory=%s actionable=%s\n", replicaStabilitySummary(rec), cpuStabilitySummary(rec), memoryStabilitySummary(rec), actionableSummary(rec)); err != nil {
			return err
		}
		if plan == nil {
			if _, err := fmt.Fprintln(w, "  manifest: unavailable"); err != nil {
				return err
			}
			if err := writeActionRecommendationPreview(w, rec); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(w, "  manifest: %s resource=%s\n", emptyDash(plan.SourceFile), emptyDash(plan.Resource)); err != nil {
			return err
		}
		for _, reason := range plan.BlockReasons {
			if _, err := fmt.Fprintf(w, "  blocked: %s\n", reason); err != nil {
				return err
			}
		}
		for _, planError := range plan.Errors {
			if _, err := fmt.Fprintf(w, "  error: %s\n", planError); err != nil {
				return err
			}
		}
		if !showChanges {
			message := "none"
			if actionBlocked(report.Proposal) {
				message = "hidden because proposal is blocked"
			}
			if _, err := fmt.Fprintf(w, "  changes: %s\n", message); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
			continue
		}
		if len(plan.Changes) == 0 {
			if _, err := fmt.Fprintln(w, "  changes: none"); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprintln(w, "  changes:"); err != nil {
				return err
			}
			for _, change := range plan.Changes {
				if _, err := fmt.Fprintf(w, "    - %s %s: %s -> %s\n", change.Operation, change.Field, change.Current, change.Recommended); err != nil {
					return err
				}
			}
		}
		if plan.Diff != "" {
			if _, err := fmt.Fprintln(w, "  diff:"); err != nil {
				return err
			}
			if _, err := fmt.Fprint(w, indentBlock(plan.Diff, "    ")); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	return nil
}

func actionApplyable(proposal *ProposalReport, eligible bool) bool {
	if !eligible {
		return false
	}
	if proposal == nil {
		return true
	}
	return proposal.Needed && !proposal.Blocked && len(proposal.Errors) == 0
}

func actionShowChanges(proposal *ProposalReport, eligible bool) bool {
	if !eligible {
		return false
	}
	if proposal == nil {
		return true
	}
	return proposal.Needed && !proposal.Blocked && len(proposal.Errors) == 0
}

func actionBlocked(proposal *ProposalReport) bool {
	return proposal != nil && (proposal.Blocked || len(proposal.Errors) > 0)
}

func writeProposalSummary(w io.Writer, proposal *ProposalReport) error {
	if _, err := fmt.Fprintf(w, "Proposal: mode=%s kind=%s needed=%t blocked=%t\n", proposal.Mode, proposal.Kind, proposal.Needed, proposal.Blocked); err != nil {
		return err
	}
	if proposal.PatchFile != "" {
		if _, err := fmt.Fprintf(w, "Proposal patch: %s\n", proposal.PatchFile); err != nil {
			return err
		}
	}
	if proposal.Branch != "" || proposal.Commit != "" {
		if _, err := fmt.Fprintf(w, "Proposal git: branch=%s commit=%s\n", emptyDash(proposal.Branch), emptyDash(proposal.Commit)); err != nil {
			return err
		}
	}
	if proposal.Remote != "" || proposal.PushRef != "" || proposal.Pushed {
		if _, err := fmt.Fprintf(w, "Proposal push: pushed=%t remote=%s ref=%s\n", proposal.Pushed, emptyDash(proposal.Remote), emptyDash(proposal.PushRef)); err != nil {
			return err
		}
	}
	if proposal.Message != "" {
		if _, err := fmt.Fprintf(w, "Proposal note: %s\n", proposal.Message); err != nil {
			return err
		}
	}
	for _, reason := range proposal.BlockReasons {
		if _, err := fmt.Fprintf(w, "Proposal blocked: %s\n", reason); err != nil {
			return err
		}
	}
	for _, proposalError := range proposal.Errors {
		if _, err := fmt.Fprintf(w, "Proposal error: %s\n", proposalError); err != nil {
			return err
		}
	}
	return nil
}

func WriteProposalResult(w io.Writer, proposal *ProposalReport) error {
	return writeProposalSummary(w, proposal)
}

func writeActionRecommendationPreview(w io.Writer, rec Recommendation) error {
	if rec.RecommendedReplicas != rec.CurrentReplicas {
		if _, err := fmt.Fprintf(w, "  recommendation: replicas %s stability=%s\n", replicaDelta(rec), replicaStabilitySummary(rec)); err != nil {
			return err
		}
	}
	if rec.CurrentCPURequest != "" && rec.RecommendedCPURequest != "" && rec.CurrentCPURequest != rec.RecommendedCPURequest {
		if _, err := fmt.Fprintf(w, "  recommendation: cpu %s stability=%s\n", resourceDelta(rec.CurrentCPURequest, rec.RecommendedCPURequest), cpuStabilitySummary(rec)); err != nil {
			return err
		}
	}
	if rec.CurrentMemoryRequest != "" && rec.RecommendedMemoryRequest != "" && rec.CurrentMemoryRequest != rec.RecommendedMemoryRequest {
		if _, err := fmt.Fprintf(w, "  recommendation: memory %s stability=%s\n", resourceDelta(rec.CurrentMemoryRequest, rec.RecommendedMemoryRequest), memoryStabilitySummary(rec)); err != nil {
			return err
		}
	}
	return nil
}

func writeCompactDecisionSummary(w io.Writer, report *Report) error {
	if _, err := fmt.Fprintln(w, "Decision Summary"); err != nil {
		return err
	}
	for _, workload := range report.Workloads {
		rec := workload.Recommendation
		if _, err := fmt.Fprintf(w, "\n%s\n", workload.Namespace+"/"+workload.Deployment); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "  status:   metrics=%s blocked=%t outcome=%s\n", workload.MetricsCondition, rec.Blocked, lastOutcome(rec)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "  decision: %s confidence=%.2f actionable=%s\n", primaryDecision(rec), rec.Confidence, actionableSummary(rec)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "  replicas: %s basis=%s stability=%s\n", replicaDelta(rec), replicaBasis(rec), replicaStabilitySummary(rec)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "  cpu:      %s basis=%s stability=%s\n", resourceDelta(rec.CurrentCPURequest, rec.RecommendedCPURequest), resourceBasis(rec), cpuStabilitySummary(rec)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "  memory:   %s basis=%s stability=%s\n", resourceDelta(rec.CurrentMemoryRequest, rec.RecommendedMemoryRequest), resourceBasis(rec), memoryStabilitySummary(rec)); err != nil {
			return err
		}
	}
	return nil
}

func writeDecisionSummary(w io.Writer, report *Report) error {
	return writeCompactDecisionSummary(w, report)
}

func writeTextCapabilities(w io.Writer, capabilities ClusterCapabilities) error {
	resize := capabilities.InPlacePodResize
	if _, err := fmt.Fprintln(w, "Cluster Capabilities:"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  - in_place_pod_resize supported=%t normalMode=%s subresource=%s verbs=%s", resize.Supported, emptyDash(resize.NormalMode), emptyDash(resize.Subresource), strings.Join(resize.Verbs, ",")); err != nil {
		return err
	}
	if resize.Reason != "" {
		if _, err := fmt.Fprintf(w, " reason=%q", resize.Reason); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w)
	return err
}

func writePrettyCapabilities(w io.Writer, capabilities ClusterCapabilities) error {
	resize := capabilities.InPlacePodResize
	if _, err := fmt.Fprintln(w, "Cluster Capabilities"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  in-place pod resize: supported=%t normalMode=%s subresource=%s verbs=%s\n", resize.Supported, emptyDash(resize.NormalMode), emptyDash(resize.Subresource), strings.Join(resize.Verbs, ",")); err != nil {
		return err
	}
	if resize.Reason != "" {
		if _, err := fmt.Fprintf(w, "  reason: %s\n", resize.Reason); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

func writePrettyWorkload(w io.Writer, workload WorkloadReport) error {
	if _, err := fmt.Fprintf(w, "%s/%s (%s)\n", workload.Namespace, workload.Deployment, workload.Name); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  Status: replicas=%d ready=%d metrics=%s fleet=%t helm=%s\n", workload.Replicas, workload.ReadyReplicas, workload.MetricsCondition, workload.FleetManaged, emptyDash(workload.HelmRelease)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  Scaling: replicas=%t cpu=%t memory=%t commitBlocked=%t\n", workload.Scaling.Replicas, workload.Scaling.CPU, workload.Scaling.Memory, workload.CommitBlocked); err != nil {
		return err
	}
	if len(workload.PDBs) > 0 {
		if _, err := fmt.Fprintln(w, "  PDBs:"); err != nil {
			return err
		}
		for _, pdb := range workload.PDBs {
			if _, err := fmt.Fprintf(w, "    - %s minAvailable=%s maxUnavailable=%s replicaFloor=%s disruptionsAllowed=%d\n", pdb.Name, emptyDash(pdb.MinAvailable), emptyDash(pdb.MaxUnavailable), int32Dash(pdb.MinimumReplicaFloor), pdb.DisruptionsAllowed); err != nil {
				return err
			}
		}
	}
	if workload.Availability.ReplicaFloor > 0 || workload.Availability.Public {
		if _, err := fmt.Fprintf(w, "  Availability: floor=%d public=%t readyEndpoints=%d readyNodes=%d zeroUnavailable=%t\n", workload.Availability.ReplicaFloor, workload.Availability.Public, workload.Availability.ReadyEndpoints, workload.Availability.ReadyNodes, workload.Availability.RollingUpdateZeroUnavailable); err != nil {
			return err
		}
		if len(workload.Availability.Services) > 0 {
			if _, err := fmt.Fprintf(w, "    services: %s\n", strings.Join(workload.Availability.Services, ", ")); err != nil {
				return err
			}
		}
		if len(workload.Availability.Reasons) > 0 {
			if _, err := fmt.Fprintf(w, "    reasons: %s\n", strings.Join(workload.Availability.Reasons, ", ")); err != nil {
				return err
			}
		}
	}
	if len(workload.Containers) > 0 {
		if _, err := fmt.Fprintln(w, "  Containers:"); err != nil {
			return err
		}
		for _, container := range workload.Containers {
			if _, err := fmt.Fprintf(w, "    - %s cpu=%s memory=%s\n", container.Name, emptyDash(container.CPURequest), emptyDash(container.MemoryRequest)); err != nil {
				return err
			}
		}
	}

	rec := workload.Recommendation
	if _, err := fmt.Fprintln(w, "  Recommendation:"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "    mode=%s confidence=%.2f blocked=%t\n", rec.Mode, rec.Confidence, rec.Blocked); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "    replicas: %s\n", replicaDelta(rec)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "    cpu request: %s\n", resourceDelta(rec.CurrentCPURequest, rec.RecommendedCPURequest)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "    memory request: %s\n", resourceDelta(rec.CurrentMemoryRequest, rec.RecommendedMemoryRequest)); err != nil {
		return err
	}
	if len(rec.BlockReasons) > 0 {
		if _, err := fmt.Fprintf(w, "    block reasons: %s\n", strings.Join(rec.BlockReasons, "; ")); err != nil {
			return err
		}
	}
	if rec.Stability != nil {
		if _, err := fmt.Fprintf(w, "    stability: actionable=%t replicas=%s cpu=%s memory=%s\n", rec.Stability.Actionable, formatGate(rec.Stability.Replicas), formatGate(rec.Stability.CPU), formatGate(rec.Stability.Memory)); err != nil {
			return err
		}
	}
	if len(rec.ReasonCodes) > 0 {
		if _, err := fmt.Fprintln(w, "    reasons:"); err != nil {
			return err
		}
		for _, reason := range rec.ReasonCodes {
			if _, err := fmt.Fprintf(w, "      - %s\n", reason); err != nil {
				return err
			}
		}
	}
	if err := writePrettyLearning(w, rec.Learning); err != nil {
		return err
	}
	if rec.PatchPlan != nil {
		if err := writePrettyPatchPlan(w, rec.PatchPlan); err != nil {
			return err
		}
	}

	if len(workload.MetricSignals) > 0 {
		if _, err := fmt.Fprintf(w, "  Metrics (%s):\n", workload.MetricProfile); err != nil {
			return err
		}
		if err := writePrettySignals(w, workload.MetricSignals); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

func writeTextLearning(w io.Writer, learning LearningEvidence) error {
	if learning.Mode == "" {
		return nil
	}
	if _, err := fmt.Fprintf(w, "        learning: mode=%s signals=%d decisions=%d\n", learning.Mode, len(learning.Signals), len(learning.Decisions)); err != nil {
		return err
	}
	if learning.Persistent != nil {
		if _, err := fmt.Fprintf(w, "          persistent: enabled=%t priorRuns=%d priorSignals=%d message=%q\n", learning.Persistent.Enabled, learning.Persistent.PriorRecommendationRuns, learning.Persistent.PriorSignalObservations, learning.Persistent.Message); err != nil {
			return err
		}
		if learning.Persistent.ForecastAccuracy != nil {
			if _, err := fmt.Fprintf(w, "          forecastAccuracy: samples=%d confidenceAdjustment=%+.3f wasteBias=%s message=%q\n", learning.Persistent.ForecastAccuracy.Samples, learning.Persistent.ForecastAccuracy.ConfidenceAdjustment, emptyDash(learning.Persistent.ForecastAccuracy.WasteReductionBias), learning.Persistent.ForecastAccuracy.Message); err != nil {
				return err
			}
		}
		if learning.Persistent.LastOutcome != nil {
			if _, err := fmt.Fprintf(w, "          lastOutcome: status=%s details=%s\n", learning.Persistent.LastOutcome.Status, emptyDash(strings.Join(learning.Persistent.LastOutcome.Details, ","))); err != nil {
				return err
			}
		}
	}
	for _, decision := range learning.Decisions {
		if _, err := fmt.Fprintf(w, "          - %s: %s; %s; %s\n", decision.Subject, decision.Learned, decision.Observed, decision.Conclusion); err != nil {
			return err
		}
	}
	return nil
}

func writePrettyLearning(w io.Writer, learning LearningEvidence) error {
	if learning.Mode == "" {
		return nil
	}
	if _, err := fmt.Fprintf(w, "    learning: %s\n", learning.Mode); err != nil {
		return err
	}
	if learning.Description != "" {
		if _, err := fmt.Fprintf(w, "      %s\n", learning.Description); err != nil {
			return err
		}
	}
	if learning.Persistent != nil {
		if _, err := fmt.Fprintf(w, "      persistent memory: priorRuns=%d priorSignals=%d\n", learning.Persistent.PriorRecommendationRuns, learning.Persistent.PriorSignalObservations); err != nil {
			return err
		}
		if learning.Persistent.LastObservedAt != nil {
			if _, err := fmt.Fprintf(w, "        last observed: %s\n", learning.Persistent.LastObservedAt.Format("2006-01-02T15:04:05Z")); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "        last recommendation: replicas=%d cpu=%s memory=%s\n", learning.Persistent.LastRecommendedReplicas, emptyDash(learning.Persistent.LastRecommendedCPURequest), emptyDash(learning.Persistent.LastRecommendedMemoryRequest)); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "        %s\n", learning.Persistent.Message); err != nil {
			return err
		}
		if learning.Persistent.ForecastAccuracy != nil {
			if _, err := fmt.Fprintf(w, "        forecast accuracy: samples=%d confidenceAdjustment=%+.3f wasteBias=%s lastScored=%d\n",
				learning.Persistent.ForecastAccuracy.Samples,
				learning.Persistent.ForecastAccuracy.ConfidenceAdjustment,
				emptyDash(learning.Persistent.ForecastAccuracy.WasteReductionBias),
				learning.Persistent.ForecastAccuracy.LastScoredRecommendationCount,
			); err != nil {
				return err
			}
			for _, score := range learning.Persistent.ForecastAccuracy.Signals {
				if _, err := fmt.Fprintf(w, "          - %s mape=%.1f%% bias=%+.1f%% samples=%d class=%s\n", score.Signal, score.MeanAbsolutePercentError*100, score.MeanBiasPercent*100, score.Samples, score.Classification); err != nil {
					return err
				}
			}
		}
		if learning.Persistent.LastOutcome != nil {
			if _, err := fmt.Fprintf(w, "        last outcome: %s\n", learning.Persistent.LastOutcome.Status); err != nil {
				return err
			}
			if len(learning.Persistent.LastOutcome.Details) > 0 {
				if _, err := fmt.Fprintf(w, "          details: %s\n", strings.Join(learning.Persistent.LastOutcome.Details, ", ")); err != nil {
					return err
				}
			}
		}
	}
	if len(learning.Signals) > 0 {
		if _, err := fmt.Fprintln(w, "      learned signals:"); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, "        SIGNAL                    WINDOW   POINTS   CURRENT      P50          P95          MAX          CLASSIFICATION"); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, "        ------------------------------------------------------------------------------------------------"); err != nil {
			return err
		}
		for _, signal := range learning.Signals {
			if _, err := fmt.Fprintf(w, "        %-25s %-8s %-8d %-12s %-12s %-12s %-12s %s\n",
				signal.Name,
				emptyDash(signal.Window),
				signal.Points,
				formatSignalValue(signal.Name, signal.Current),
				formatSignalValue(signal.Name, signal.P50),
				formatSignalValue(signal.Name, signal.P95),
				formatSignalValue(signal.Name, signal.Max),
				signal.Classification,
			); err != nil {
				return err
			}
		}
	}
	if len(learning.Decisions) > 0 {
		if _, err := fmt.Fprintln(w, "      learned decisions:"); err != nil {
			return err
		}
		for _, decision := range learning.Decisions {
			if _, err := fmt.Fprintf(w, "        - %s\n", decision.Subject); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "          learned: %s\n", decision.Learned); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "          observed: %s\n", decision.Observed); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "          conclusion: %s\n", decision.Conclusion); err != nil {
				return err
			}
		}
	}
	return nil
}

func writePrettyPatchPlan(w io.Writer, plan *PatchPlan) error {
	if _, err := fmt.Fprintf(w, "    patch plan: source=%s needed=%t blocked=%t\n", emptyDash(plan.SourceFile), plan.Needed, plan.Blocked); err != nil {
		return err
	}
	if len(plan.Errors) > 0 {
		if _, err := fmt.Fprintf(w, "      errors: %s\n", strings.Join(plan.Errors, "; ")); err != nil {
			return err
		}
	}
	for _, change := range plan.Changes {
		if _, err := fmt.Fprintf(w, "      - %s %s: %s -> %s\n", change.Operation, change.Field, change.Current, change.Recommended); err != nil {
			return err
		}
	}
	return nil
}

func writePrettySignals(w io.Writer, signals []SignalReport) error {
	ordered := append([]SignalReport(nil), signals...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Name < ordered[j].Name
	})
	if _, err := fmt.Fprintln(w, "  SIGNAL                         STATE      SAMPLE       P50          P95          MAX          POINTS   ANOMALY"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "  ------------------------------------------------------------------------------------------------------"); err != nil {
		return err
	}
	for _, signal := range ordered {
		if _, err := fmt.Fprintf(w, "  %-30s %-10s %-12s %-12s %-12s %-12s %-8s %s\n",
			signal.Name,
			prettySignalState(signal),
			floatPtrDash(signal.Name, signal.Sample),
			historyValue(signal.Name, signal.History, "p50"),
			historyValue(signal.Name, signal.History, "p95"),
			historyValue(signal.Name, signal.History, "max"),
			historyPoints(signal.History),
			prettyAnomaly(signal),
		); err != nil {
			return err
		}
	}
	return nil
}

func writePatchPlan(w io.Writer, plan *PatchPlan) error {
	if _, err := fmt.Fprintf(w, "        patch plan: source=%s needed=%t blocked=%t\n", emptyDash(plan.SourceFile), plan.Needed, plan.Blocked); err != nil {
		return err
	}
	if len(plan.BlockReasons) > 0 {
		if _, err := fmt.Fprintf(w, "          blockReasons: %s\n", strings.Join(plan.BlockReasons, "; ")); err != nil {
			return err
		}
	}
	if len(plan.Errors) > 0 {
		if _, err := fmt.Fprintf(w, "          errors: %s\n", strings.Join(plan.Errors, "; ")); err != nil {
			return err
		}
	}
	for _, change := range plan.Changes {
		if _, err := fmt.Fprintf(w, "          - %s %s: %s -> %s\n", change.Operation, change.Field, change.Current, change.Recommended); err != nil {
			return err
		}
	}
	return nil
}

func signalStatus(signal SignalReport) string {
	required := "optional"
	if signal.Required {
		required = "required"
	}
	if signal.Error != "" {
		return fmt.Sprintf("error %s: %s", required, signal.Error)
	}
	if signal.Healthy {
		history := ""
		if signal.History != nil {
			history = fmt.Sprintf(" history[p50=%s p95=%s max=%s points=%d]",
				formatSignalValue(signal.Name, signal.History.P50),
				formatSignalValue(signal.Name, signal.History.P95),
				formatSignalValue(signal.Name, signal.History.Max),
				signal.History.Points,
			)
		} else if signal.HistoryError != "" {
			history = fmt.Sprintf(" history_error=%q", signal.HistoryError)
		}
		anomaly := ""
		if signal.Anomaly.State != "" {
			anomaly = fmt.Sprintf(" anomaly=%s", signal.Anomaly.State)
			if signal.Anomaly.Reason != "" && signal.Anomaly.State != "normal" {
				anomaly += fmt.Sprintf("(%s)", signal.Anomaly.Reason)
			}
		}
		if signal.Sample != nil {
			return fmt.Sprintf("ok %s series=%d sample=%s%s%s", required, signal.Series, formatSignalValue(signal.Name, *signal.Sample), history, anomaly)
		}
		return fmt.Sprintf("ok %s series=%d%s%s", required, signal.Series, history, anomaly)
	}
	return fmt.Sprintf("missing %s series=%d", required, signal.Series)
}

func primaryDecision(recommendation Recommendation) string {
	switch {
	case recommendation.Blocked:
		return "blocked"
	case recommendation.RecommendedReplicas > recommendation.CurrentReplicas:
		return "scale replicas up"
	case recommendation.RecommendedReplicas < recommendation.CurrentReplicas:
		return "scale replicas down"
	case recommendation.CurrentCPURequest != "" && recommendation.RecommendedCPURequest != "" && recommendation.CurrentCPURequest != recommendation.RecommendedCPURequest:
		return "adjust cpu request"
	case recommendation.CurrentMemoryRequest != "" && recommendation.RecommendedMemoryRequest != "" && recommendation.CurrentMemoryRequest != recommendation.RecommendedMemoryRequest:
		return "adjust memory request"
	default:
		return "hold"
	}
}

func replicaBasis(recommendation Recommendation) string {
	if hasReasonPrefix(recommendation, "replica_management_disabled") {
		return "disabled"
	}
	if hasReasonPrefix(recommendation, "replica_joint_optimizer_selected:") {
		return "cost optimizer"
	}
	if recommendation.RecommendedReplicas > recommendation.CurrentReplicas {
		switch {
		case reasonInt(recommendation, "traffic_replica_floor:") >= recommendation.RecommendedReplicas:
			return "traffic"
		case reasonInt(recommendation, "cpu_replicas:") >= recommendation.RecommendedReplicas && reasonInt(recommendation, "memory_replicas:") >= recommendation.RecommendedReplicas:
			return "learned cpu+memory pressure"
		case reasonInt(recommendation, "cpu_replicas:") >= recommendation.RecommendedReplicas:
			return "learned cpu pressure"
		case reasonInt(recommendation, "memory_replicas:") >= recommendation.RecommendedReplicas:
			return "learned memory pressure"
		case reasonInt(recommendation, "pdb_replica_floor:") >= recommendation.RecommendedReplicas:
			return "pdb floor"
		case reasonInt(recommendation, "availability_replica_floor:") >= recommendation.RecommendedReplicas:
			return "availability floor"
		default:
			return "capacity"
		}
	}
	switch {
	case reasonInt(recommendation, "traffic_replica_floor:") >= recommendation.RecommendedReplicas:
		return "traffic"
	case recommendation.RecommendedReplicas == reasonInt(recommendation, "pdb_replica_floor:"):
		return "pdb floor"
	case recommendation.RecommendedReplicas == reasonInt(recommendation, "availability_replica_floor:"):
		return "availability floor"
	default:
		return "capacity"
	}
}

func resourceBasis(recommendation Recommendation) string {
	switch {
	case recommendation.Blocked:
		return "blocked"
	case hasReasonPrefix(recommendation, "cpu_request_increase_recommended") || hasReasonPrefix(recommendation, "memory_request_increase_recommended"):
		return "learned pressure"
	case hasReasonPrefix(recommendation, "cpu_request_decrease_recommended") || hasReasonPrefix(recommendation, "memory_request_decrease_recommended"):
		return "learned waste"
	case hasReasonPrefix(recommendation, "forecast_waste_reduction_bias:favor_waste_reduction"):
		return "learned waste"
	case hasReasonPrefix(recommendation, "forecast_waste_reduction_bias:preserve_headroom"):
		return "learned pressure"
	default:
		return "hold"
	}
}

func lastOutcome(recommendation Recommendation) string {
	if recommendation.Learning.Persistent == nil || recommendation.Learning.Persistent.LastOutcome == nil {
		return "-"
	}
	return recommendation.Learning.Persistent.LastOutcome.Status
}

func stabilitySummary(recommendation Recommendation) string {
	if recommendation.Stability == nil {
		return "-"
	}
	return fmt.Sprintf(
		"act=%t cpu=%s mem=%s rep=%s",
		recommendation.Stability.Actionable,
		gateProgress(recommendation.Stability.CPU),
		gateProgress(recommendation.Stability.Memory),
		gateProgress(recommendation.Stability.Replicas),
	)
}

func actionableSummary(recommendation Recommendation) string {
	if recommendation.Stability == nil {
		return "-"
	}
	return fmt.Sprintf("%t", recommendation.Stability.Actionable)
}

func replicaStabilitySummary(recommendation Recommendation) string {
	if recommendation.Stability == nil {
		return "-"
	}
	return gateSummary(recommendation.Stability.Replicas)
}

func cpuStabilitySummary(recommendation Recommendation) string {
	if recommendation.Stability == nil {
		return "-"
	}
	return gateSummary(recommendation.Stability.CPU)
}

func memoryStabilitySummary(recommendation Recommendation) string {
	if recommendation.Stability == nil {
		return "-"
	}
	return gateSummary(recommendation.Stability.Memory)
}

func patchPlanApplyable(plan *PatchPlan) bool {
	return plan != nil && plan.Needed && !plan.Blocked && len(plan.Errors) == 0 && len(plan.Changes) > 0
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func indentBlock(value, prefix string) string {
	if value == "" {
		return ""
	}
	lines := strings.Split(value, "\n")
	var output strings.Builder
	for _, line := range lines {
		if line == "" {
			continue
		}
		output.WriteString(prefix)
		output.WriteString(line)
		output.WriteString("\n")
	}
	return output.String()
}

func gateSummary(gate StabilityGate) string {
	progress := gateProgress(gate)
	switch gate.Status {
	case "":
		return "-"
	case "hold":
		return "hold"
	default:
		return gate.Status + " " + progress
	}
}

func gateProgress(gate StabilityGate) string {
	switch gate.Status {
	case "stable":
		if gate.Required > 0 {
			return fmt.Sprintf("%d/%d", gate.Observed, gate.Required)
		}
		return "stable"
	case "pending_stability":
		return fmt.Sprintf("%d/%d", gate.Observed, gate.Required)
	case "hold":
		return "hold"
	case "blocked":
		return "blocked"
	case "":
		return "-"
	default:
		return gate.Status
	}
}

func formatGate(gate StabilityGate) string {
	progress := gateProgress(gate)
	if gate.Reason == "" {
		return fmt.Sprintf("%s(%s)", gate.Status, progress)
	}
	return fmt.Sprintf("%s(%s: %s)", gate.Status, progress, gate.Reason)
}

func hasReasonPrefix(recommendation Recommendation, prefix string) bool {
	for _, reason := range recommendation.ReasonCodes {
		if strings.HasPrefix(reason, prefix) {
			return true
		}
	}
	return false
}

func reasonInt(recommendation Recommendation, prefix string) int32 {
	for _, reason := range recommendation.ReasonCodes {
		if !strings.HasPrefix(reason, prefix) {
			continue
		}
		value, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(reason, prefix)), 10, 32)
		if err == nil {
			return int32(value)
		}
	}
	return 0
}

func replicaDelta(recommendation Recommendation) string {
	return fmt.Sprintf("%d -> %d", recommendation.CurrentReplicas, recommendation.RecommendedReplicas)
}

func resourceDelta(current, recommended string) string {
	if current == "" && recommended == "" {
		return "-"
	}
	return fmt.Sprintf("%s -> %s", emptyDash(current), emptyDash(recommended))
}

func prettySignalState(signal SignalReport) string {
	switch {
	case signal.Error != "":
		return "error"
	case signal.Healthy:
		if signal.Required {
			return "ok/req"
		}
		return "ok/opt"
	case signal.Required:
		return "missing/req"
	default:
		return "missing/opt"
	}
}

func prettyAnomaly(signal SignalReport) string {
	if signal.Anomaly.State == "" {
		return "-"
	}
	if signal.Anomaly.Reason != "" && signal.Anomaly.State != "normal" {
		return signal.Anomaly.State + ": " + signal.Anomaly.Reason
	}
	return signal.Anomaly.State
}

func floatPtrDash(name string, value *float64) string {
	if value == nil {
		return "-"
	}
	return formatSignalValue(name, *value)
}

func historyValue(name string, history *SignalHistory, field string) string {
	if history == nil {
		return "-"
	}
	switch field {
	case "p50":
		return formatSignalValue(name, history.P50)
	case "p95":
		return formatSignalValue(name, history.P95)
	case "max":
		return formatSignalValue(name, history.Max)
	default:
		return "-"
	}
}

func formatSignalValue(name string, value float64) string {
	if isByteSignal(name) {
		return formatBytes(value)
	}
	return fmt.Sprintf("%.6g", value)
}

func isByteSignal(name string) bool {
	normalized := strings.ToLower(name)
	return strings.Contains(normalized, "memory") || strings.Contains(normalized, "bytes")
}

func formatBytes(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Sprintf("%.6g", value)
	}

	units := []string{"B", "Ki", "Mi", "Gi", "Ti", "Pi"}
	scaled := math.Abs(value)
	unit := units[0]
	for i := 0; i < len(units)-1 && scaled >= 1024; i++ {
		scaled /= 1024
		unit = units[i+1]
	}
	if value < 0 {
		scaled = -scaled
	}
	if unit == "B" {
		return fmt.Sprintf("%.0f%s", scaled, unit)
	}
	switch {
	case math.Abs(scaled) >= 100:
		return fmt.Sprintf("%.0f%s", scaled, unit)
	case math.Abs(scaled) >= 10:
		return fmt.Sprintf("%.1f%s", scaled, unit)
	default:
		return fmt.Sprintf("%.2f%s", scaled, unit)
	}
}

func historyPoints(history *SignalHistory) string {
	if history == nil {
		return "-"
	}
	return fmt.Sprintf("%d", history.Points)
}

func int32Dash(value int32) string {
	if value == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", value)
}

func emptyDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
