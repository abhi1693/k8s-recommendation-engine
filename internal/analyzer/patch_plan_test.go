package analyzer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhi1693/k8s-recommendation-engine/internal/config"
)

func TestAttachPatchPlansFindsDeploymentInMultiDocumentSource(t *testing.T) {
	worktree := t.TempDir()
	basePath := filepath.Join("kubernetes", "projects", "applications", "apps", "shipyardhq")
	sourceDir := filepath.Join(worktree, basePath)
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := []byte(`
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
              cpu: 700m
              memory: 5Gi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: shipyardhq-worker
  namespace: shipyardhq
spec:
  replicas: 1
  template:
    spec:
      containers:
        - name: worker
          resources:
            requests:
              cpu: 140m
              memory: 608Mi
`)
	if err := os.WriteFile(filepath.Join(sourceDir, "deployment.yaml"), source, 0o644); err != nil {
		t.Fatal(err)
	}

	profile := &config.ApplicationProfile{
		Spec: config.ApplicationSpec{
			Namespace: "shipyardhq",
			Git: config.GitSpec{
				BasePath: filepath.ToSlash(basePath),
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
	report := &Report{
		Workloads: []WorkloadReport{
			{
				Name:      "web",
				Namespace: "shipyardhq",
				Containers: []ContainerReport{
					{Name: "web"},
				},
				Recommendation: Recommendation{
					RecommendedReplicas:      2,
					RecommendedCPURequest:    "700m",
					RecommendedMemoryRequest: "5Gi",
					Stability: &RecommendationStability{
						Replicas: StabilityGate{Status: "stable", Observed: 3, Required: 3},
						CPU:      StabilityGate{Status: "hold"},
						Memory:   StabilityGate{Status: "hold"},
					},
				},
			},
		},
	}

	AttachPatchPlans(worktree, profile, report)
	plan := report.Workloads[0].Recommendation.PatchPlan
	if plan == nil {
		t.Fatal("PatchPlan is nil")
	}
	if !plan.Needed {
		t.Fatal("PatchPlan.Needed = false, want true")
	}
	if len(plan.Changes) != 1 {
		t.Fatalf("len(Changes) = %d, want 1: %#v", len(plan.Changes), plan.Changes)
	}
	change := plan.Changes[0]
	if change.Field != "spec.replicas" || change.Operation != "add" || change.Current != missingValue || change.Recommended != "2" {
		t.Fatalf("unexpected change: %#v", change)
	}
	if plan.Diff == "" {
		t.Fatal("PatchPlan.Diff is empty, want dry-run diff")
	}
	if !containsString(plan.Diff, "+  replicas: 2") {
		t.Fatalf("PatchPlan.Diff missing replica addition:\n%s", plan.Diff)
	}
}

func TestAttachPatchPlansReportsNoChangeWhenSourceMatchesRecommendation(t *testing.T) {
	worktree := t.TempDir()
	basePath := "shipyard"
	sourceDir := filepath.Join(worktree, basePath)
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := []byte(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: shipyardhq-imgproxy
  namespace: shipyardhq
spec:
  replicas: 2
  template:
    spec:
      containers:
        - name: imgproxy
          resources:
            requests:
              cpu: 50m
              memory: 128Mi
`)
	if err := os.WriteFile(filepath.Join(sourceDir, "imgproxy-deployment.yaml"), source, 0o644); err != nil {
		t.Fatal(err)
	}

	profile := &config.ApplicationProfile{
		Spec: config.ApplicationSpec{
			Namespace: "shipyardhq",
			Git:       config.GitSpec{BasePath: basePath},
			Workloads: []config.WorkloadSpec{
				{
					Name:       "imgproxy",
					SourceFile: "imgproxy-deployment.yaml",
					TargetRef:  config.TargetRef{Kind: "Deployment", Name: "shipyardhq-imgproxy"},
					Scaling:    config.ScalingSpec{Replicas: true, CPU: true, Memory: true},
				},
			},
		},
	}
	report := &Report{
		Workloads: []WorkloadReport{
			{
				Name: "imgproxy",
				Containers: []ContainerReport{
					{Name: "imgproxy"},
				},
				Recommendation: Recommendation{
					RecommendedReplicas:      2,
					RecommendedCPURequest:    "50m",
					RecommendedMemoryRequest: "128Mi",
					Stability: &RecommendationStability{
						Replicas: StabilityGate{Status: "hold"},
						CPU:      StabilityGate{Status: "hold"},
						Memory:   StabilityGate{Status: "hold"},
					},
				},
			},
		},
	}

	AttachPatchPlans(worktree, profile, report)
	plan := report.Workloads[0].Recommendation.PatchPlan
	if plan == nil {
		t.Fatal("PatchPlan is nil")
	}
	if plan.Needed {
		t.Fatalf("PatchPlan.Needed = true, want false: %#v", plan.Changes)
	}
	if len(plan.Changes) != 0 {
		t.Fatalf("len(Changes) = %d, want 0", len(plan.Changes))
	}
}

func TestAttachPatchPlansAddsMissingReplicasWhenRecommendationHolds(t *testing.T) {
	worktree := t.TempDir()
	basePath := "shipyard"
	sourceDir := filepath.Join(worktree, basePath)
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := []byte(`
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
              cpu: 490m
              memory: 3892Mi
`)
	if err := os.WriteFile(filepath.Join(sourceDir, "deployment.yaml"), source, 0o644); err != nil {
		t.Fatal(err)
	}

	profile := &config.ApplicationProfile{
		Spec: config.ApplicationSpec{
			Namespace: "shipyardhq",
			Git:       config.GitSpec{BasePath: basePath},
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
	report := &Report{
		Workloads: []WorkloadReport{
			{
				Name: "web",
				Containers: []ContainerReport{
					{Name: "web"},
				},
				Recommendation: Recommendation{
					CurrentReplicas:          2,
					RecommendedReplicas:      2,
					CurrentCPURequest:        "490m",
					RecommendedCPURequest:    "490m",
					CurrentMemoryRequest:     "3892Mi",
					RecommendedMemoryRequest: "3892Mi",
					Stability: &RecommendationStability{
						Replicas: StabilityGate{Status: "hold"},
						CPU:      StabilityGate{Status: "hold"},
						Memory:   StabilityGate{Status: "hold"},
					},
				},
			},
		},
	}

	AttachPatchPlans(worktree, profile, report)
	plan := report.Workloads[0].Recommendation.PatchPlan
	if plan == nil || !plan.Needed {
		t.Fatalf("PatchPlan = %#v, want needed replica ownership patch", plan)
	}
	if len(plan.Changes) != 1 {
		t.Fatalf("Changes = %#v, want one replica add", plan.Changes)
	}
	change := plan.Changes[0]
	if change.Field != "spec.replicas" || change.Operation != "add" || change.Current != missingValue || change.Recommended != "2" {
		t.Fatalf("change = %#v, want spec.replicas add 2", change)
	}
}

func TestAttachPatchPlansBlocksPendingStability(t *testing.T) {
	worktree := t.TempDir()
	basePath := "shipyard"
	sourceDir := filepath.Join(worktree, basePath)
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := []byte(`
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
              cpu: 700m
              memory: 5Gi
`)
	if err := os.WriteFile(filepath.Join(sourceDir, "deployment.yaml"), source, 0o644); err != nil {
		t.Fatal(err)
	}

	profile := &config.ApplicationProfile{
		Spec: config.ApplicationSpec{
			Namespace: "shipyardhq",
			Git:       config.GitSpec{BasePath: basePath},
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
	report := &Report{
		Workloads: []WorkloadReport{
			{
				Name: "web",
				Containers: []ContainerReport{
					{Name: "web"},
				},
				Recommendation: Recommendation{
					CurrentReplicas:          2,
					RecommendedReplicas:      2,
					CurrentCPURequest:        "700m",
					RecommendedCPURequest:    "490m",
					CurrentMemoryRequest:     "5Gi",
					RecommendedMemoryRequest: "3892Mi",
					Stability: &RecommendationStability{
						Replicas: StabilityGate{Status: "hold"},
						CPU:      StabilityGate{Status: "pending_stability", Observed: 2, Required: 3},
						Memory:   StabilityGate{Status: "stable", Observed: 3, Required: 3},
					},
				},
			},
		},
	}

	AttachPatchPlans(worktree, profile, report)
	plan := report.Workloads[0].Recommendation.PatchPlan
	if plan == nil {
		t.Fatal("PatchPlan is nil")
	}
	if len(plan.Changes) != 1 {
		t.Fatalf("len(Changes) = %d, want only stable memory change: %#v", len(plan.Changes), plan.Changes)
	}
	if plan.Changes[0].Field != "spec.template.spec.containers[name=web].resources.requests.memory" {
		t.Fatalf("unexpected change: %#v", plan.Changes[0])
	}
	if !containsString(strings.Join(plan.BlockReasons, "\n"), "cpu request blocked by stability gate") {
		t.Fatalf("missing cpu block reason: %#v", plan.BlockReasons)
	}
	if containsString(plan.Diff, "+              cpu: 490m") {
		t.Fatalf("diff should not include pending cpu change:\n%s", plan.Diff)
	}
	if !containsString(plan.Diff, "+              memory: 3892Mi") {
		t.Fatalf("diff should include stable memory change:\n%s", plan.Diff)
	}
}

func TestAttachPatchPlansBlocksUnsettledRollout(t *testing.T) {
	worktree := t.TempDir()
	basePath := "shipyard"
	sourceDir := filepath.Join(worktree, basePath)
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := []byte(`
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
              cpu: 700m
              memory: 5Gi
`)
	if err := os.WriteFile(filepath.Join(sourceDir, "deployment.yaml"), source, 0o644); err != nil {
		t.Fatal(err)
	}

	profile := &config.ApplicationProfile{
		Spec: config.ApplicationSpec{
			Namespace: "shipyardhq",
			Git:       config.GitSpec{BasePath: basePath},
			Workloads: []config.WorkloadSpec{
				{
					Name:       "web",
					SourceFile: "deployment.yaml",
					TargetRef:  config.TargetRef{Kind: "Deployment", Name: "shipyardhq"},
					Scaling:    config.ScalingSpec{CPU: true},
				},
			},
		},
	}
	report := &Report{
		Workloads: []WorkloadReport{
			{
				Name: "web",
				Containers: []ContainerReport{
					{Name: "web"},
				},
				Rollout: RolloutReport{
					Evaluated: true,
					Settled:   false,
					Reasons:   []string{"incomplete_init_pods:1"},
				},
				Recommendation: Recommendation{
					CurrentCPURequest:     "700m",
					RecommendedCPURequest: "490m",
					Stability: &RecommendationStability{
						CPU: StabilityGate{Status: "stable", Observed: 3, Required: 3},
					},
				},
			},
		},
	}

	AttachPatchPlans(worktree, profile, report)
	plan := report.Workloads[0].Recommendation.PatchPlan
	if plan == nil {
		t.Fatal("PatchPlan is nil")
	}
	if !plan.Blocked {
		t.Fatal("PatchPlan.Blocked = false, want true")
	}
	if len(plan.Changes) != 0 {
		t.Fatalf("Changes = %#v, want no changes while rollout is unsettled", plan.Changes)
	}
	if !containsString(strings.Join(plan.BlockReasons, "\n"), "incomplete_init_pods:1") {
		t.Fatalf("missing rollout block reason: %#v", plan.BlockReasons)
	}
}

func TestAttachPatchPlansBlocksMissingStability(t *testing.T) {
	worktree := t.TempDir()
	basePath := "shipyard"
	sourceDir := filepath.Join(worktree, basePath)
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := []byte(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: shipyardhq
  namespace: shipyardhq
spec:
  replicas: 2
`)
	if err := os.WriteFile(filepath.Join(sourceDir, "deployment.yaml"), source, 0o644); err != nil {
		t.Fatal(err)
	}
	profile := &config.ApplicationProfile{
		Spec: config.ApplicationSpec{
			Namespace: "shipyardhq",
			Git:       config.GitSpec{BasePath: basePath},
			Workloads: []config.WorkloadSpec{
				{Name: "web", SourceFile: "deployment.yaml", TargetRef: config.TargetRef{Kind: "Deployment", Name: "shipyardhq"}, Scaling: config.ScalingSpec{Replicas: true}},
			},
		},
	}
	report := &Report{
		Workloads: []WorkloadReport{
			{
				Name: "web",
				Recommendation: Recommendation{
					CurrentReplicas:     2,
					RecommendedReplicas: 1,
				},
			},
		},
	}

	AttachPatchPlans(worktree, profile, report)
	plan := report.Workloads[0].Recommendation.PatchPlan
	if plan == nil {
		t.Fatal("PatchPlan is nil")
	}
	if !plan.Blocked {
		t.Fatal("PatchPlan.Blocked = false, want true")
	}
	if !containsString(strings.Join(plan.BlockReasons, "\n"), "--state-db") {
		t.Fatalf("missing state-db block reason: %#v", plan.BlockReasons)
	}
	if len(plan.Changes) != 0 {
		t.Fatalf("len(Changes) = %d, want 0", len(plan.Changes))
	}
}

func TestAttachPatchPlansRejectsSourceOutsideWorktree(t *testing.T) {
	profile := &config.ApplicationProfile{
		Spec: config.ApplicationSpec{
			Namespace: "shipyardhq",
			Git:       config.GitSpec{BasePath: "../outside"},
			Workloads: []config.WorkloadSpec{
				{
					Name:       "web",
					SourceFile: "deployment.yaml",
					TargetRef:  config.TargetRef{Kind: "Deployment", Name: "shipyardhq"},
					Scaling:    config.ScalingSpec{Replicas: true},
				},
			},
		},
	}
	report := &Report{
		Workloads: []WorkloadReport{
			{
				Name: "web",
				Recommendation: Recommendation{
					RecommendedReplicas: 2,
				},
			},
		},
	}

	AttachPatchPlans(t.TempDir(), profile, report)
	plan := report.Workloads[0].Recommendation.PatchPlan
	if plan == nil {
		t.Fatal("PatchPlan is nil")
	}
	if len(plan.Errors) != 1 {
		t.Fatalf("len(Errors) = %d, want 1", len(plan.Errors))
	}
}

func containsString(value, want string) bool {
	return strings.Contains(value, want)
}
