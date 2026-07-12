package analyzer

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/abhi1693/k8s-recommendation-engine/internal/config"
	"gopkg.in/yaml.v3"
)

func ObserveConvergence(ctx context.Context, worktree, baseBranch string, profile *config.ApplicationProfile, report *Report) *ObservationReport {
	observation := &ObservationReport{
		Application: report.Application,
		Namespace:   report.Namespace,
		GeneratedAt: report.GeneratedAt,
		Git: GitObservation{
			Worktree:   worktree,
			BaseBranch: baseBranch,
		},
	}
	if baseBranch == "" {
		observation.Git.BaseBranch = profile.Spec.Git.Branch
	}
	if observation.Git.BaseBranch == "" {
		observation.Git.BaseBranch = "master"
	}
	attachGitObservation(ctx, observation)

	workloadsByName := make(map[string]config.WorkloadSpec, len(profile.Spec.Workloads))
	for _, workload := range profile.Spec.Workloads {
		workloadsByName[workload.Name] = workload
	}
	for _, workloadReport := range report.Workloads {
		workload, ok := workloadsByName[workloadReport.Name]
		if !ok {
			observation.Errors = append(observation.Errors, fmt.Sprintf("workload %s is missing from profile", workloadReport.Name))
			continue
		}
		item := observeWorkload(worktree, profile, workload, workloadReport, observation.Git)
		observation.Workloads = append(observation.Workloads, item)
	}
	observation.Summary.WorkloadsTotal = len(observation.Workloads)
	for _, workload := range observation.Workloads {
		switch workload.Status {
		case "applied":
			observation.Summary.Applied++
		case "pending":
			observation.Summary.Pending++
		case "drifted":
			observation.Summary.Drifted++
		case "failed":
			observation.Summary.Failed++
		default:
			observation.Summary.Unknown++
		}
		switch workload.Outcome {
		case "improved":
			observation.Summary.Improved++
		case "neutral":
			observation.Summary.Neutral++
		case "regressed":
			observation.Summary.Regressed++
		case "unsafe":
			observation.Summary.Unsafe++
		}
	}
	return observation
}

func attachGitObservation(ctx context.Context, observation *ObservationReport) {
	if observation.Git.Worktree == "" {
		observation.Errors = append(observation.Errors, "--git-worktree is required for observation")
		return
	}
	if branch, err := gitCurrentBranch(ctx, observation.Git.Worktree); err == nil {
		observation.Git.Branch = branch
	} else {
		observation.Errors = append(observation.Errors, "read current branch: "+err.Error())
	}
	if upstream, err := gitOutput(ctx, observation.Git.Worktree, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); err == nil {
		observation.Git.Upstream = strings.TrimSpace(upstream)
		observation.Git.HasUpstream = observation.Git.Upstream != ""
	}
	if status, err := gitOutput(ctx, observation.Git.Worktree, "status", "--porcelain"); err == nil {
		observation.Git.DirtyLines = blockingGitStatusLines(status)
	} else {
		observation.Errors = append(observation.Errors, "read git status: "+err.Error())
	}
	if commit, subject, err := latestProposalCommit(ctx, observation.Git.Worktree, false); err == nil {
		observation.Git.LatestProposalCommit = commit
		observation.Git.LatestProposalSubject = subject
	} else if !strings.Contains(err.Error(), errNoProposalCommit.Error()) {
		observation.Errors = append(observation.Errors, "read latest proposal commit: "+err.Error())
	}
	if differs, err := branchDiffersFromBase(ctx, observation.Git.Worktree, observation.Git.BaseBranch); err == nil {
		observation.Git.BranchDiffersFromBase = differs
	} else {
		observation.Errors = append(observation.Errors, "compare branch to base: "+err.Error())
	}
}

func observeWorkload(worktree string, profile *config.ApplicationProfile, workload config.WorkloadSpec, live WorkloadReport, git GitObservation) WorkloadObservation {
	item := WorkloadObservation{
		Name:             live.Name,
		Namespace:        live.Namespace,
		Deployment:       live.Deployment,
		Resource:         fmt.Sprintf("%s/%s/%s", workload.TargetRef.Kind, profile.Spec.Namespace, workload.TargetRef.Name),
		MetricsCondition: live.MetricsCondition,
		Status:           "unknown",
		Outcome:          "unknown",
		Live: ObservedResources{
			Replicas: strconv.FormatInt(int64(live.Replicas), 10),
		},
	}
	if len(live.Containers) == 1 {
		item.Live.CPURequest = live.Containers[0].CPURequest
		item.Live.MemoryRequest = live.Containers[0].MemoryRequest
	}
	desired, sourceFile, err := desiredResourcesFromGit(worktree, profile, workload, live)
	item.SourceFile = sourceFile
	if err != nil {
		item.Errors = append(item.Errors, err.Error())
		item.Status = "failed"
		item.Outcome = "unknown"
		return item
	}
	item.Desired = desired
	item.Fields = appendConvergenceFields(item.Fields, workload, item.Desired, item.Live)
	item.Status = convergenceStatus(item, live, git)
	item.Outcome = observationOutcome(item, live)
	item.Reasons = observationReasons(item, live, git)
	return item
}

func desiredResourcesFromGit(worktree string, profile *config.ApplicationProfile, workload config.WorkloadSpec, live WorkloadReport) (ObservedResources, string, error) {
	var desired ObservedResources
	basePath, err := cleanGitPath(profile.Spec.Git.BasePath, "spec.git.basePath")
	if err != nil {
		return desired, "", err
	}
	sourceFile, err := cleanGitPath(workload.SourceFile, "workload sourceFile")
	if err != nil {
		return desired, "", err
	}
	relativeSource := filepath.ToSlash(filepath.Join(basePath, sourceFile))
	sourcePath, err := resolveExistingGitPath(worktree, filepath.Join(basePath, sourceFile), "workload sourceFile")
	if err != nil {
		return desired, relativeSource, err
	}
	documents, err := readYAMLDocuments(sourcePath)
	if err != nil {
		return desired, relativeSource, err
	}
	if workload.HelmValues != nil {
		desired, err = desiredResourcesFromHelmValues(documents, workload)
		return desired, relativeSource, err
	}
	document := findWorkloadDocument(documents, profile.Spec.Namespace, workload)
	if document == nil {
		return desired, relativeSource, fmt.Errorf("resource %s/%s not found in %s", workload.TargetRef.Kind, workload.TargetRef.Name, relativeSource)
	}
	if replicas, ok := scalarAt(document, "spec", "replicas"); ok {
		desired.Replicas = replicas
	}
	if len(live.Containers) == 1 {
		container := live.Containers[0].Name
		if cpu, ok := containerRequestScalar(document, container, "cpu"); ok {
			desired.CPURequest = cpu
		}
		if memory, ok := containerRequestScalar(document, container, "memory"); ok {
			desired.MemoryRequest = memory
		}
	}
	return desired, relativeSource, nil
}

func desiredResourcesFromHelmValues(documents []*yaml.Node, workload config.WorkloadSpec) (ObservedResources, error) {
	var desired ObservedResources
	if len(documents) != 1 || documents[0].Kind != yaml.MappingNode {
		return desired, fmt.Errorf("Helm values source must contain exactly one non-empty YAML mapping document")
	}
	paths := workload.HelmValues.Paths
	values := []struct {
		name   string
		path   []string
		assign func(string)
	}{
		{name: "replicas", path: paths.Replicas, assign: func(value string) { desired.Replicas = value }},
		{name: "cpuRequest", path: paths.CPURequest, assign: func(value string) { desired.CPURequest = value }},
		{name: "memoryRequest", path: paths.MemoryRequest, assign: func(value string) { desired.MemoryRequest = value }},
	}
	for _, mapped := range values {
		if len(mapped.path) == 0 {
			continue
		}
		value, ok, err := helmScalarAt(documents[0], mapped.path)
		if err != nil {
			return desired, err
		}
		if !ok {
			return desired, fmt.Errorf("configured Helm value %s for %s does not exist", formatHelmSourcePath(mapped.path), mapped.name)
		}
		mapped.assign(value)
	}
	return desired, nil
}

func appendConvergenceFields(fields []FieldObservation, workload config.WorkloadSpec, desired, live ObservedResources) []FieldObservation {
	fields = append(fields, FieldObservation{
		Field:   "spec.replicas",
		Desired: desired.Replicas,
		Live:    live.Replicas,
		Managed: workload.Scaling.Replicas,
		Match:   helmReplicaValuesEqual(desired.Replicas, live.Replicas),
	})
	fields = append(fields, FieldObservation{
		Field:   "resources.requests.cpu",
		Desired: desired.CPURequest,
		Live:    live.CPURequest,
		Managed: workload.Scaling.CPU,
		Match:   helmResourceValuesEqual(desired.CPURequest, live.CPURequest),
	})
	fields = append(fields, FieldObservation{
		Field:   "resources.requests.memory",
		Desired: desired.MemoryRequest,
		Live:    live.MemoryRequest,
		Managed: workload.Scaling.Memory,
		Match:   helmResourceValuesEqual(desired.MemoryRequest, live.MemoryRequest),
	})
	return fields
}

func convergenceStatus(item WorkloadObservation, live WorkloadReport, git GitObservation) string {
	if len(item.Errors) > 0 {
		return "failed"
	}
	if managedFieldUnspecified(item.Fields) {
		return "drifted"
	}
	if !managedFieldsMatch(item.Fields) {
		if live.MetricsCondition != "healthy" {
			return "failed"
		}
		if git.LatestProposalCommit != "" && git.HasUpstream && len(git.DirtyLines) == 0 {
			return "pending"
		}
		return "drifted"
	}
	return "applied"
}

func observationOutcome(item WorkloadObservation, live WorkloadReport) string {
	if len(item.Errors) > 0 {
		return "unknown"
	}
	if item.Status != "applied" {
		return "pending"
	}
	if observationWorkloadUnhealthy(live) {
		return "unsafe"
	}
	if observationSignalAnomaly(live, "error_rate") || observationSignalAnomaly(live, "latency_p95") {
		return "regressed"
	}
	if live.Recommendation.Learning.Persistent != nil && live.Recommendation.Learning.Persistent.LastOutcome != nil {
		switch live.Recommendation.Learning.Persistent.LastOutcome.Status {
		case "applied_successful":
			return "improved"
		case "too_aggressive":
			return "regressed"
		}
	}
	return "neutral"
}

func observationReasons(item WorkloadObservation, live WorkloadReport, git GitObservation) []string {
	var reasons []string
	for _, field := range item.Fields {
		if !field.Managed {
			reasons = append(reasons, field.Field+":unmanaged")
			continue
		}
		if field.Desired == "" {
			reasons = append(reasons, field.Field+":not_specified_in_git")
			continue
		}
		if field.Match {
			reasons = append(reasons, field.Field+":matched")
		} else {
			reasons = append(reasons, fmt.Sprintf("%s:pending desired=%s live=%s", field.Field, emptyDash(field.Desired), emptyDash(field.Live)))
		}
	}
	if git.LatestProposalCommit != "" {
		reasons = append(reasons, "latest_proposal_commit:"+git.LatestProposalCommit)
	}
	if live.MetricsCondition != "" {
		reasons = append(reasons, "metrics:"+live.MetricsCondition)
	}
	return reasons
}

func managedFieldsMatch(fields []FieldObservation) bool {
	for _, field := range fields {
		if field.Managed && !field.Match {
			return false
		}
	}
	return true
}

func managedFieldUnspecified(fields []FieldObservation) bool {
	for _, field := range fields {
		if field.Managed && field.Desired == "" {
			return true
		}
	}
	return false
}

func observationWorkloadUnhealthy(workload WorkloadReport) bool {
	if workload.MetricsCondition != "healthy" {
		return true
	}
	if workload.ReadyReplicas < workload.Replicas {
		return true
	}
	for _, signal := range workload.MetricSignals {
		if signal.Anomaly.State == "critical" {
			return true
		}
	}
	return false
}

func observationSignalAnomaly(workload WorkloadReport, name string) bool {
	for _, signal := range workload.MetricSignals {
		if signal.Name != name {
			continue
		}
		if signal.Anomaly.State == "warning" || signal.Anomaly.State == "critical" {
			return true
		}
	}
	return false
}

func WriteObservationReport(w io.Writer, report *ObservationReport) error {
	if _, err := fmt.Fprintln(w, "K8s Recommendation Engine Observation"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Application: %s   Namespace: %s   Generated: %s\n", report.Application, report.Namespace, report.GeneratedAt.Format("2006-01-02T15:04:05Z")); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Git: branch=%s upstream=%s latest=%s dirty=%d\n", emptyDash(report.Git.Branch), emptyDash(report.Git.Upstream), emptyDash(report.Git.LatestProposalCommit), len(report.Git.DirtyLines)); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	for _, workload := range report.Workloads {
		if _, err := fmt.Fprintf(w, "%s/%s\n", workload.Namespace, workload.Deployment); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "  status: %s outcome=%s metrics=%s\n", workload.Status, workload.Outcome, emptyDash(workload.MetricsCondition)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "  manifest: %s resource=%s\n", emptyDash(workload.SourceFile), workload.Resource); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "  desired: replicas=%s cpu=%s memory=%s\n", emptyDash(workload.Desired.Replicas), emptyDash(workload.Desired.CPURequest), emptyDash(workload.Desired.MemoryRequest)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "  live:    replicas=%s cpu=%s memory=%s\n", emptyDash(workload.Live.Replicas), emptyDash(workload.Live.CPURequest), emptyDash(workload.Live.MemoryRequest)); err != nil {
			return err
		}
		for _, field := range workload.Fields {
			if _, err := fmt.Fprintf(w, "  field: %s match=%t managed=%t desired=%s live=%s\n", field.Field, field.Match, field.Managed, emptyDash(field.Desired), emptyDash(field.Live)); err != nil {
				return err
			}
		}
		for _, reason := range workload.Reasons {
			if _, err := fmt.Fprintf(w, "  reason: %s\n", reason); err != nil {
				return err
			}
		}
		for _, observeError := range workload.Errors {
			if _, err := fmt.Fprintf(w, "  error: %s\n", observeError); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "Summary: workloads=%d applied=%d pending=%d drifted=%d failed=%d unknown=%d improved=%d neutral=%d regressed=%d unsafe=%d\n",
		report.Summary.WorkloadsTotal,
		report.Summary.Applied,
		report.Summary.Pending,
		report.Summary.Drifted,
		report.Summary.Failed,
		report.Summary.Unknown,
		report.Summary.Improved,
		report.Summary.Neutral,
		report.Summary.Regressed,
		report.Summary.Unsafe,
	); err != nil {
		return err
	}
	for _, observeError := range report.Errors {
		if _, err := fmt.Fprintf(w, "Error: %s\n", observeError); err != nil {
			return err
		}
	}
	return nil
}
