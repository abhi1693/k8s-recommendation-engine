package backtest

import (
	"fmt"
	"io"
	"math"
	"strings"
)

func WriteTextReport(w io.Writer, report *Report) error {
	if report == nil {
		return fmt.Errorf("report is required")
	}
	if _, err := fmt.Fprintln(w, "K8s Recommendation Engine Backtest"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Application: %s   Namespace: %s   Generated: %s\n", report.Application, report.Namespace, report.GeneratedAt.Format("2006-01-02T15:04:05Z")); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Window: %s   Step: %s   Range: %s -> %s\n", report.Window, report.Step, report.Start.Format("2006-01-02T15:04:05Z"), report.End.Format("2006-01-02T15:04:05Z")); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Method: %s\n\n", report.Method); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(w, "Proof Summary"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  workloads=%d points=%d insufficientData=%d\n", report.Summary.WorkloadsTotal, report.Summary.Points, report.Summary.WorkloadsWithInsufficientData); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  spikes=%d proactiveScaleBeforeSpikes=%d coveredByExistingCapacity=%d missedSpikes=%d\n",
		report.Summary.Spikes,
		report.Summary.ProactiveScaleBeforeSpikes,
		report.Summary.CoveredByExistingCapacity,
		report.Summary.MissedSpikes,
	); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  overProvisionedPoints=%d underProvisionedPoints=%d\n", report.Summary.OverProvisionedPoints, report.Summary.UnderProvisionedPoints); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  computeSaved=%s replica-hours observed=%s reactiveNeed=%s predictive=%s\n",
		formatFloat(report.Summary.ComputeSavedReplicaHours),
		formatFloat(report.Summary.ObservedReplicaHours),
		formatFloat(report.Summary.ReactiveReplicaHours),
		formatFloat(report.Summary.PredictiveReplicaHours),
	); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  estimatedGitCommits=%d reactiveChangeEvents=%d commitReductionVsReactive=%d\n\n",
		report.Summary.EstimatedGitCommits,
		report.Summary.ReactiveChangeEvents,
		report.Summary.CommitReductionVsReactive,
	); err != nil {
		return err
	}

	for _, workload := range report.Workloads {
		if err := writeWorkload(w, workload); err != nil {
			return err
		}
	}
	return nil
}

func writeWorkload(w io.Writer, workload WorkloadReport) error {
	if _, err := fmt.Fprintf(w, "%s/%s (%s)\n", workload.Namespace, workload.Deployment, workload.Name); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  points=%d metricProfile=%s replicaScaling=%t\n", workload.Points, workload.MetricProfile, workload.ReplicaScalingEnabled); err != nil {
		return err
	}
	if workload.InsufficientData {
		if _, err := fmt.Fprintf(w, "  data: insufficient (%s)\n", strings.Join(workload.InsufficientDataReasons, "; ")); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "  spike proof: spikes=%d proactive=%d existingCapacity=%d missed=%d\n",
		workload.Spikes,
		workload.ProactiveScaleBeforeSpikes,
		workload.CoveredByExistingCapacity,
		workload.MissedSpikes,
	); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  provisioning: over=%d under=%d computeSaved=%s replica-hours observed=%s predictive=%s\n",
		workload.OverProvisionedPoints,
		workload.UnderProvisionedPoints,
		formatFloat(workload.ComputeSavedReplicaHours),
		formatFloat(workload.ObservedReplicaHours),
		formatFloat(workload.PredictiveReplicaHours),
	); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  git estimate: commits=%d reactiveChanges=%d reduction=%d\n",
		workload.EstimatedGitCommits,
		workload.ReactiveChangeEvents,
		workload.CommitReductionVsReactive,
	); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  learned capacity: traffic/replica=%s cpu/replica=%s memory/replica=%s\n",
		formatSignalValue("request_rate", workload.TrafficCapacityPerReplica),
		formatSignalValue("cpu_usage", workload.CPUCapacityPerReplica),
		formatSignalValue("memory_working_set", workload.MemoryCapacityPerReplica),
	); err != nil {
		return err
	}
	if len(workload.Signals) > 0 {
		if _, err := fmt.Fprintln(w, "  signals:"); err != nil {
			return err
		}
		for _, signal := range workload.Signals {
			state := "ok"
			if signal.Error != "" {
				state = "error"
			}
			if _, err := fmt.Fprintf(w, "    - %-22s %-5s points=%-4d p50=%-10s p95=%-10s max=%s\n",
				signal.Name,
				state,
				signal.Points,
				formatSignalValue(signal.Name, signal.P50),
				formatSignalValue(signal.Name, signal.P95),
				formatSignalValue(signal.Name, signal.Max),
			); err != nil {
				return err
			}
			if signal.Error != "" {
				if _, err := fmt.Fprintf(w, "      error: %s\n", signal.Error); err != nil {
					return err
				}
			}
		}
	}
	if len(workload.Events) > 0 {
		if _, err := fmt.Fprintln(w, "  replay events:"); err != nil {
			return err
		}
		for _, event := range workload.Events {
			if _, err := fmt.Fprintf(w, "    - %s %-22s %s\n", event.Time.Format("2006-01-02T15:04:05Z"), event.Type, event.Detail); err != nil {
				return err
			}
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

func formatSignalValue(name string, value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "-"
	}
	if name == "memory_working_set" {
		return formatBytes(value)
	}
	if name == "latency_p95" {
		if value >= 1 {
			return fmt.Sprintf("%.2fs", value)
		}
		return fmt.Sprintf("%.0fms", value*1000)
	}
	if math.Abs(value) >= 100 {
		return fmt.Sprintf("%.0f", value)
	}
	if math.Abs(value) >= 10 {
		return fmt.Sprintf("%.1f", value)
	}
	if math.Abs(value) >= 1 {
		return fmt.Sprintf("%.2f", value)
	}
	return fmt.Sprintf("%.4f", value)
}

func formatBytes(value float64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%.0fB", value)
	}
	divisor := float64(unit)
	units := []string{"Ki", "Mi", "Gi", "Ti"}
	for _, suffix := range units {
		next := divisor * unit
		if value < next {
			return fmt.Sprintf("%.0f%s", value/divisor, suffix)
		}
		divisor = next
	}
	return fmt.Sprintf("%.0fPi", value/divisor)
}

func formatFloat(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "-"
	}
	if math.Abs(value) >= 100 {
		return fmt.Sprintf("%.0f", value)
	}
	if math.Abs(value) >= 10 {
		return fmt.Sprintf("%.1f", value)
	}
	return fmt.Sprintf("%.2f", value)
}
