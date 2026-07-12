package analyzer

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abhi1693/k8s-recommendation-engine/internal/config"
)

func TestObserveConvergenceApplied(t *testing.T) {
	worktree, profile := initObservationRepo(t, "490m", "3892Mi")
	report := observationReport("490m", "3892Mi")

	observation := ObserveConvergence(context.Background(), worktree, "master", profile, report)

	if len(observation.Workloads) != 1 {
		t.Fatalf("workloads = %d, want 1", len(observation.Workloads))
	}
	workload := observation.Workloads[0]
	if workload.Status != "applied" {
		t.Fatalf("Status = %q, want applied: %#v", workload.Status, workload)
	}
	if workload.Outcome != "neutral" {
		t.Fatalf("Outcome = %q, want neutral", workload.Outcome)
	}
	if observation.Summary.Applied != 1 || observation.Summary.Neutral != 1 {
		t.Fatalf("Summary = %#v", observation.Summary)
	}
}

func TestObserveConvergencePendingAfterPushedProposal(t *testing.T) {
	worktree, profile := initObservationRepo(t, "700m", "5Gi")
	remote := initBareGitRepo(t)
	gitTest(t, worktree, "remote", "add", "origin", remote)
	gitTest(t, worktree, "push", "-u", "origin", "master")
	writeObservationManifest(t, worktree, "490m", "3892Mi")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "k8s-recommendation-engine: propose shipyard resource changes")
	gitTest(t, worktree, "push")
	report := observationReport("700m", "5Gi")

	observation := ObserveConvergence(context.Background(), worktree, "master", profile, report)

	workload := observation.Workloads[0]
	if workload.Status != "pending" {
		t.Fatalf("Status = %q, want pending: %#v", workload.Status, workload)
	}
	if workload.Outcome != "pending" {
		t.Fatalf("Outcome = %q, want pending", workload.Outcome)
	}
	if observation.Git.LatestProposalCommit == "" {
		t.Fatalf("LatestProposalCommit is empty: %#v", observation.Git)
	}
	if !containsReasonPrefix(workload.Reasons, "resources.requests.cpu:pending") {
		t.Fatalf("missing cpu pending reason: %#v", workload.Reasons)
	}
}

func TestObserveConvergenceReportsMissingOwnedReplicaFieldAsDrift(t *testing.T) {
	worktree, profile := initObservationRepo(t, "490m", "3892Mi")
	writeObservationManifestWithoutReplicas(t, worktree, "490m", "3892Mi")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "k8s-recommendation-engine: remove explicit replicas")
	report := observationReport("490m", "3892Mi")

	observation := ObserveConvergence(context.Background(), worktree, "master", profile, report)

	workload := observation.Workloads[0]
	if workload.Status != "drifted" {
		t.Fatalf("Status = %q, want drifted: %#v", workload.Status, workload)
	}
	if workload.Desired.Replicas != "" {
		t.Fatalf("Desired.Replicas = %q, want unspecified", workload.Desired.Replicas)
	}
	if !containsReasonPrefix(workload.Reasons, "spec.replicas:not_specified_in_git") {
		t.Fatalf("missing not_specified_in_git reason: %#v", workload.Reasons)
	}
}

func TestWriteObservationReport(t *testing.T) {
	worktree, profile := initObservationRepo(t, "490m", "3892Mi")
	report := observationReport("490m", "3892Mi")
	observation := ObserveConvergence(context.Background(), worktree, "master", profile, report)
	var output strings.Builder

	if err := WriteObservationReport(&output, observation); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{
		"K8s Recommendation Engine Observation",
		"shipyardhq/shipyardhq",
		"status: applied",
		"desired: replicas=2 cpu=490m memory=3892Mi",
		"Summary: workloads=1 applied=1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("observation report missing %q:\n%s", want, got)
		}
	}
}

func TestObserveConvergenceReadsHelmValuesMappings(t *testing.T) {
	tests := []struct {
		name   string
		values string
		status string
		want   string
	}{
		{
			name:   "applied with equivalent quantity spelling",
			values: "replicaCount: 2\nresources:\n  requests:\n    cpu: 0.49\n    memory: 3892Mi\n",
			status: "applied",
		},
		{
			name:   "drifted",
			values: "replicaCount: 2\nresources:\n  requests:\n    cpu: 700m\n    memory: 3892Mi\n",
			status: "drifted",
		},
		{
			name:   "missing mapped value",
			values: "replicaCount: 2\nresources:\n  requests:\n    cpu: 490m\n",
			status: "failed",
			want:   "does not exist",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			worktree := initProposalGitRepo(t)
			writeRepoFile(t, worktree, "apps/values.yaml", test.values)
			gitTest(t, worktree, "add", ".")
			gitTest(t, worktree, "commit", "-m", "initial")
			profile := helmObservationProfile()
			report := observationReport("490m", "3892Mi")
			observation := ObserveConvergence(context.Background(), worktree, "master", profile, report)
			workload := observation.Workloads[0]
			if workload.Status != test.status {
				t.Fatalf("Status = %q, want %q: %#v", workload.Status, test.status, workload)
			}
			if test.want != "" && !strings.Contains(strings.Join(workload.Errors, "\n"), test.want) {
				t.Fatalf("Errors = %#v, want substring %q", workload.Errors, test.want)
			}
		})
	}
}

func helmObservationProfile() *config.ApplicationProfile {
	return &config.ApplicationProfile{
		Metadata: config.Metadata{Name: "shipyard"},
		Spec: config.ApplicationSpec{
			Namespace: "shipyardhq",
			Git:       config.GitSpec{Branch: "master", BasePath: "apps"},
			Workloads: []config.WorkloadSpec{
				{
					Name:       "web",
					SourceFile: "values.yaml",
					TargetRef:  config.TargetRef{Kind: "Deployment", Name: "shipyardhq"},
					HelmValues: &config.HelmValuesSpec{Paths: config.HelmValuePaths{
						Replicas:      []string{"replicaCount"},
						CPURequest:    []string{"resources", "requests", "cpu"},
						MemoryRequest: []string{"resources", "requests", "memory"},
					}},
					Scaling: config.ScalingSpec{Replicas: true, CPU: true, Memory: true},
				},
			},
		},
	}
}

func initObservationRepo(t *testing.T, cpu, memory string) (string, *config.ApplicationProfile) {
	t.Helper()
	worktree := initProposalGitRepo(t)
	writeObservationManifest(t, worktree, cpu, memory)
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "initial")
	return worktree, observationProfile()
}

func writeObservationManifest(t *testing.T, worktree, cpu, memory string) {
	t.Helper()
	writeRepoFile(t, worktree, filepath.ToSlash(filepath.Join("apps", "deployment.yaml")), `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: shipyardhq
  namespace: shipyardhq
spec:
  replicas: 2
  template:
    spec:
      containers:
        - name: web
          resources:
            requests:
              cpu: `+cpu+`
              memory: `+memory+`
`)
}

func writeObservationManifestWithoutReplicas(t *testing.T, worktree, cpu, memory string) {
	t.Helper()
	writeRepoFile(t, worktree, filepath.ToSlash(filepath.Join("apps", "deployment.yaml")), `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: shipyardhq
  namespace: shipyardhq
spec:
  template:
    spec:
      containers:
        - name: web
          resources:
            requests:
              cpu: `+cpu+`
              memory: `+memory+`
`)
}

func observationProfile() *config.ApplicationProfile {
	return &config.ApplicationProfile{
		Metadata: config.Metadata{Name: "shipyard"},
		Spec: config.ApplicationSpec{
			Namespace: "shipyardhq",
			Git: config.GitSpec{
				Branch:   "master",
				BasePath: "apps",
			},
			Workloads: []config.WorkloadSpec{
				{
					Name:       "web",
					SourceFile: "deployment.yaml",
					TargetRef:  config.TargetRef{Kind: "Deployment", Name: "shipyardhq"},
					Scaling:    config.ScalingSpec{Replicas: true, CPU: true, Memory: true},
				},
			},
		},
	}
}

func observationReport(cpu, memory string) *Report {
	return &Report{
		Application: "shipyard",
		Namespace:   "shipyardhq",
		GeneratedAt: time.Date(2026, 7, 8, 21, 30, 0, 0, time.UTC),
		Workloads: []WorkloadReport{
			{
				Name:             "web",
				Namespace:        "shipyardhq",
				Deployment:       "shipyardhq",
				Replicas:         2,
				ReadyReplicas:    2,
				FleetManaged:     true,
				MetricsCondition: "healthy",
				Containers: []ContainerReport{
					{Name: "web", CPURequest: cpu, MemoryRequest: memory},
				},
				Recommendation: Recommendation{
					Learning: LearningEvidence{
						Persistent: &PersistentLearning{},
					},
				},
			},
		},
	}
}

func containsReasonPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}
