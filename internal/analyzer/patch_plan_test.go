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

func TestAttachPatchPlansAllowsRestorativeAvailabilityRecoveryDuringUnsettledRollout(t *testing.T) {
	worktree := t.TempDir()
	basePath := "shipyard"
	sourceDir := filepath.Join(worktree, basePath)
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "deployment.yaml"), []byte(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: shipyardhq
  namespace: shipyardhq
spec:
  replicas: 3
  template:
    spec:
      containers:
        - name: web
          resources:
            requests:
              memory: 3110Mi
`), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := &config.ApplicationProfile{Spec: config.ApplicationSpec{
		Namespace: "shipyardhq",
		Git:       config.GitSpec{BasePath: basePath},
		Workloads: []config.WorkloadSpec{
			{
				Name:       "web",
				SourceFile: "deployment.yaml",
				TargetRef:  config.TargetRef{Kind: "Deployment", Name: "shipyardhq"},
				Scaling:    config.ScalingSpec{Replicas: true, Memory: true},
				Policy: config.PolicySpec{
					AvailabilityRecovery: config.AvailabilityRecoveryPolicySpec{Enabled: true},
				},
			},
		},
	}}
	report := &Report{Workloads: []WorkloadReport{
		{
			Name:       "web",
			Containers: []ContainerReport{{Name: "web"}},
			Rollout: RolloutReport{
				Evaluated: true,
				Settled:   false,
				Reasons:   []string{"ready_replicas_pending:0/3"},
			},
			Recommendation: Recommendation{
				AvailabilityRecovery:     true,
				CurrentReplicas:          3,
				RecommendedReplicas:      4,
				CurrentMemoryRequest:     "3110Mi",
				RecommendedMemoryRequest: "4Gi",
				Stability: &RecommendationStability{
					Replicas: StabilityGate{Status: "blocked"},
					Memory:   StabilityGate{Status: "blocked"},
				},
			},
		},
	}}

	AttachPatchPlans(worktree, profile, report)
	plan := report.Workloads[0].Recommendation.PatchPlan
	if plan == nil || plan.Blocked || !plan.Needed {
		t.Fatalf("PatchPlan = %#v, want applyable availability recovery", plan)
	}
	if len(plan.Changes) != 2 {
		t.Fatalf("Changes = %#v, want replica and memory increases", plan.Changes)
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

func TestAttachPatchPlansHelmValuesUsesMappedScalarPaths(t *testing.T) {
	worktree := t.TempDir()
	valuesPath := filepath.Join(worktree, "zitadel", "values.yaml")
	if err := os.MkdirAll(filepath.Dir(valuesPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(valuesPath, []byte(`
# chart settings stay intact
login:
  replicaCount: 2 # replica baseline

  resources:
    requests:
      cpu: "0.05" # CPU baseline
      memory: 128Mi
`), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := helmPatchProfile("zitadel-login", config.HelmValuePaths{
		Replicas:      []string{"login", "replicaCount"},
		CPURequest:    []string{"login", "resources", "requests", "cpu"},
		MemoryRequest: []string{"login", "resources", "requests", "memory"},
	})
	report := helmPatchReport(2, "50m", "128Mi")
	report.Workloads[0].Recommendation.RecommendedReplicas = 3
	report.Workloads[0].Recommendation.RecommendedCPURequest = "40m"
	report.Workloads[0].Recommendation.RecommendedMemoryRequest = "96Mi"

	AttachPatchPlans(worktree, profile, report)
	plan := report.Workloads[0].Recommendation.PatchPlan
	if plan == nil || plan.SourceFormat != patchSourceHelmValues || !plan.Needed || len(plan.Errors) != 0 {
		t.Fatalf("PatchPlan = %#v, want applyable Helm values plan", plan)
	}
	if len(plan.Changes) != 3 {
		t.Fatalf("Changes = %#v, want replicas, CPU, and memory", plan.Changes)
	}
	wantPaths := map[string][]string{
		"spec.replicas": []string{"login", "replicaCount"},
		"spec.template.spec.containers[name=zitadel-login].resources.requests.cpu":    []string{"login", "resources", "requests", "cpu"},
		"spec.template.spec.containers[name=zitadel-login].resources.requests.memory": []string{"login", "resources", "requests", "memory"},
	}
	for _, change := range plan.Changes {
		want, ok := wantPaths[change.Field]
		if !ok || compareHelmPaths(change.SourcePath, want) != 0 {
			t.Fatalf("change = %#v, want semantic field with mapped source path", change)
		}
		if change.Operation != "replace" {
			t.Fatalf("change operation = %q, want replace", change.Operation)
		}
	}
	for _, want := range []string{"# chart settings stay intact", "# replica baseline", "# CPU baseline", "replicaCount: 3", `cpu: "40m"`, "memory: 96Mi"} {
		if !strings.Contains(plan.Diff, want) {
			t.Fatalf("PatchPlan.Diff missing %q:\n%s", want, plan.Diff)
		}
	}
	proposal := BuildProposal(worktree, report)
	if proposal.Blocked || len(proposal.Files) != 1 || proposal.Files[0].Diff != plan.Diff {
		t.Fatalf("proposal diff does not match the displayed Helm plan diff:\nplan:\n%s\nproposal: %#v", plan.Diff, proposal)
	}
}

func TestAttachPatchPlansHelmValuesTreatsEquivalentQuantityAsUnchanged(t *testing.T) {
	worktree := t.TempDir()
	valuesDir := filepath.Join(worktree, "zitadel")
	if err := os.MkdirAll(valuesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(valuesDir, "values.yaml"), []byte("resources:\n  requests:\n    cpu: 0.05\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := helmPatchProfile("zitadel", config.HelmValuePaths{CPURequest: []string{"resources", "requests", "cpu"}})
	profile.Spec.Workloads[0].Scaling = config.ScalingSpec{CPU: true}
	report := helmPatchReport(2, "50m", "128Mi")
	report.Workloads[0].Deployment = "zitadel"
	report.Workloads[0].Containers[0].Name = "zitadel"
	report.Workloads[0].Recommendation.RecommendedCPURequest = "50m"

	AttachPatchPlans(worktree, profile, report)
	plan := report.Workloads[0].Recommendation.PatchPlan
	if plan == nil || plan.Needed || len(plan.Changes) != 0 || len(plan.Errors) != 0 {
		t.Fatalf("PatchPlan = %#v, want no semantic quantity change", plan)
	}
}

func TestAttachPatchPlansHelmValuesRejectsUnsafeSources(t *testing.T) {
	tests := []struct {
		name   string
		values string
		live   string
		want   string
	}{
		{name: "missing path", values: "resources: {}\n", live: "50m", want: "does not exist"},
		{name: "non scalar", values: "resources:\n  requests:\n    cpu: {}\n", live: "50m", want: "existing non-null scalar"},
		{name: "baseline mismatch", values: "resources:\n  requests:\n    cpu: 100m\n", live: "50m", want: "mapping may be wrong or Fleet is not converged"},
		{name: "live request absent", values: "resources:\n  requests:\n    cpu: 50m\n", live: "", want: "does not have a cpu request"},
		{name: "multiple documents", values: "resources:\n  requests:\n    cpu: 50m\n---\nother: value\n", live: "50m", want: "exactly one non-empty YAML document"},
		{name: "merge key", values: "defaults: &defaults\n  cpu: 50m\nresources:\n  requests:\n    <<: *defaults\n", live: "50m", want: "YAML merge key"},
		{name: "anchor", values: "resources:\n  requests:\n    cpu: &shared-cpu 50m\n", live: "50m", want: "YAML anchor"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			worktree := t.TempDir()
			valuesDir := filepath.Join(worktree, "zitadel")
			if err := os.MkdirAll(valuesDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(valuesDir, "values.yaml"), []byte(test.values), 0o644); err != nil {
				t.Fatal(err)
			}
			profile := helmPatchProfile("zitadel", config.HelmValuePaths{CPURequest: []string{"resources", "requests", "cpu"}})
			profile.Spec.Workloads[0].Scaling = config.ScalingSpec{CPU: true}
			report := helmPatchReport(2, test.live, "128Mi")
			report.Workloads[0].Deployment = "zitadel"
			report.Workloads[0].Containers[0].Name = "zitadel"

			AttachPatchPlans(worktree, profile, report)
			plan := report.Workloads[0].Recommendation.PatchPlan
			if plan == nil || !strings.Contains(strings.Join(plan.Errors, "\n"), test.want) {
				t.Fatalf("PatchPlan errors = %#v, want substring %q", plan, test.want)
			}
		})
	}
}

func TestAttachPatchPlansHelmValuesRejectsMultipleLiveContainers(t *testing.T) {
	worktree := t.TempDir()
	valuesDir := filepath.Join(worktree, "zitadel")
	if err := os.MkdirAll(valuesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(valuesDir, "values.yaml"), []byte("resources:\n  requests:\n    cpu: 50m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := helmPatchProfile("zitadel", config.HelmValuePaths{CPURequest: []string{"resources", "requests", "cpu"}})
	profile.Spec.Workloads[0].Scaling = config.ScalingSpec{CPU: true}
	report := helmPatchReport(2, "50m", "128Mi")
	report.Workloads[0].Containers = []ContainerReport{{Name: "server"}, {Name: "sidecar"}}

	AttachPatchPlans(worktree, profile, report)
	plan := report.Workloads[0].Recommendation.PatchPlan
	if plan == nil || !strings.Contains(strings.Join(plan.Errors, "\n"), "require exactly one regular container") {
		t.Fatalf("PatchPlan = %#v, want multi-container mapping error", plan)
	}
}

func TestAttachPatchPlansHelmValuesUsesConfiguredContainerSelector(t *testing.T) {
	worktree := t.TempDir()
	valuesDir := filepath.Join(worktree, "valkey")
	if err := os.MkdirAll(valuesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(valuesDir, "values.yaml"), []byte("replica:\n  resources:\n    requests:\n      cpu: 50m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := helmPatchProfile("valkey-node", config.HelmValuePaths{CPURequest: []string{"replica", "resources", "requests", "cpu"}})
	profile.Spec.Namespace = "valkey"
	profile.Spec.Git.BasePath = "valkey"
	profile.Spec.Workloads[0].Vars = map[string]string{"container": "valkey"}
	profile.Spec.Workloads[0].Scaling = config.ScalingSpec{CPU: true}
	report := helmPatchReport(3, "50m", "112Mi")
	report.Workloads[0].Namespace = "valkey"
	report.Workloads[0].Deployment = "valkey-node"
	report.Workloads[0].Containers = []ContainerReport{
		{Name: "valkey", CPURequest: "50m", MemoryRequest: "112Mi"},
		{Name: "sentinel", CPURequest: "35m", MemoryRequest: "32Mi"},
	}
	report.Workloads[0].Recommendation.RecommendedCPURequest = "60m"

	AttachPatchPlans(worktree, profile, report)
	plan := report.Workloads[0].Recommendation.PatchPlan
	if plan == nil || len(plan.Errors) != 0 || !plan.Needed || len(plan.Changes) != 1 {
		t.Fatalf("PatchPlan = %#v, want one CPU change without multi-container error", plan)
	}
	change := plan.Changes[0]
	if change.Field != "spec.template.spec.containers[name=valkey].resources.requests.cpu" || change.Current != "50m" || change.Recommended != "60m" {
		t.Fatalf("change = %#v, want selected valkey CPU change", change)
	}
}

func TestAttachPatchPlansRejectsSourceSymlinkOutsideWorktree(t *testing.T) {
	worktree := t.TempDir()
	outside := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(outside, []byte("replicaCount: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(worktree, "zitadel"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(worktree, "zitadel", "values.yaml")); err != nil {
		t.Fatal(err)
	}
	profile := helmPatchProfile("zitadel", config.HelmValuePaths{Replicas: []string{"replicaCount"}})
	profile.Spec.Workloads[0].Scaling = config.ScalingSpec{Replicas: true}
	report := helmPatchReport(2, "50m", "128Mi")

	AttachPatchPlans(worktree, profile, report)
	plan := report.Workloads[0].Recommendation.PatchPlan
	if plan == nil || !strings.Contains(strings.Join(plan.Errors, "\n"), "resolves outside the Git worktree") {
		t.Fatalf("PatchPlan = %#v, want symlink containment error", plan)
	}
}

func TestAttachPatchPlansRejectsSourceSymlinkInsideWorktree(t *testing.T) {
	worktree := t.TempDir()
	valuesDir := filepath.Join(worktree, "zitadel")
	if err := os.MkdirAll(valuesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realValues := filepath.Join(valuesDir, "shared-values.yaml")
	if err := os.WriteFile(realValues, []byte("replicaCount: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("shared-values.yaml", filepath.Join(valuesDir, "values.yaml")); err != nil {
		t.Fatal(err)
	}
	profile := helmPatchProfile("zitadel", config.HelmValuePaths{Replicas: []string{"replicaCount"}})
	profile.Spec.Workloads[0].Scaling = config.ScalingSpec{Replicas: true}
	report := helmPatchReport(2, "50m", "128Mi")

	AttachPatchPlans(worktree, profile, report)
	plan := report.Workloads[0].Recommendation.PatchPlan
	if plan == nil || !strings.Contains(strings.Join(plan.Errors, "\n"), "must not resolve through a symlink") {
		t.Fatalf("PatchPlan = %#v, want in-worktree symlink rejection", plan)
	}
}

func helmPatchProfile(target string, paths config.HelmValuePaths) *config.ApplicationProfile {
	return &config.ApplicationProfile{Spec: config.ApplicationSpec{
		Namespace: "zitadel",
		Git:       config.GitSpec{BasePath: "zitadel"},
		Workloads: []config.WorkloadSpec{
			{
				Name:       "server",
				SourceFile: "values.yaml",
				TargetRef:  config.TargetRef{Kind: "Deployment", Name: target},
				HelmValues: &config.HelmValuesSpec{Paths: paths},
				Scaling:    config.ScalingSpec{Replicas: true, CPU: true, Memory: true},
			},
		},
	}}
}

func helmPatchReport(replicas int32, cpu, memory string) *Report {
	return &Report{Workloads: []WorkloadReport{
		{
			Name:       "server",
			Namespace:  "zitadel",
			Deployment: "zitadel-login",
			Replicas:   replicas,
			Containers: []ContainerReport{{Name: "zitadel-login", CPURequest: cpu, MemoryRequest: memory}},
			Recommendation: Recommendation{
				CurrentReplicas:          replicas,
				RecommendedReplicas:      replicas,
				CurrentCPURequest:        cpu,
				RecommendedCPURequest:    cpu,
				CurrentMemoryRequest:     memory,
				RecommendedMemoryRequest: memory,
				Stability: &RecommendationStability{
					Replicas: StabilityGate{Status: "stable", Observed: 3, Required: 3},
					CPU:      StabilityGate{Status: "stable", Observed: 3, Required: 3},
					Memory:   StabilityGate{Status: "stable", Observed: 3, Required: 3},
				},
			},
		},
	}}
}

func containsString(value, want string) bool {
	return strings.Contains(value, want)
}
