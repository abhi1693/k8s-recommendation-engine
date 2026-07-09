package analyzer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/abhi1693/k8s-recommendation-engine/internal/config"
)

func TestAttachSafetyAssessmentsBlocksHighRiskMemoryDecrease(t *testing.T) {
	report := &Report{
		Workloads: []WorkloadReport{
			{
				Name:             "web",
				Namespace:        "shipyardhq",
				Deployment:       "shipyardhq",
				Replicas:         2,
				ReadyReplicas:    2,
				MetricsCondition: "healthy",
				Rollout:          RolloutReport{Evaluated: true, Settled: true},
				MetricSignals: []SignalReport{
					{
						Name:    "memory_working_set",
						History: &SignalHistory{P95: 950 * 1024 * 1024},
					},
				},
				Recommendation: Recommendation{
					CurrentReplicas:          2,
					RecommendedReplicas:      1,
					CurrentCPURequest:        "700m",
					RecommendedCPURequest:    "700m",
					CurrentMemoryRequest:     "2Gi",
					RecommendedMemoryRequest: "1Gi",
					Learning:                 LearningEvidence{Persistent: &PersistentLearning{}},
				},
			},
		},
	}

	AttachSafetyAssessments(report)
	rec := report.Workloads[0].Recommendation
	if rec.Safety.Classification != SafetyHighRisk {
		t.Fatalf("safety = %s, want high_risk: %#v", rec.Safety.Classification, rec.Safety)
	}
	if rec.Safety.AutoCommitAllowed {
		t.Fatal("AutoCommitAllowed = true, want false")
	}
	if !rec.Blocked {
		t.Fatal("Recommendation.Blocked = false, want true")
	}
}

func TestMediumRiskSafetyDoesNotBlockPatchPlan(t *testing.T) {
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
  replicas: 2
  template:
    spec:
      containers:
        - name: web
          resources:
            requests:
              cpu: 700m
              memory: 5Gi
`), 0o644); err != nil {
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
					Scaling:    config.ScalingSpec{CPU: true, Memory: true},
				},
			},
		},
	}
	report := &Report{
		Workloads: []WorkloadReport{
			{
				Name:             "web",
				Namespace:        "shipyardhq",
				Deployment:       "shipyardhq",
				Replicas:         2,
				ReadyReplicas:    2,
				MetricsCondition: "healthy",
				Rollout:          RolloutReport{Evaluated: true, Settled: true},
				Containers:       []ContainerReport{{Name: "web"}},
				Recommendation: Recommendation{
					CurrentReplicas:          2,
					RecommendedReplicas:      2,
					CurrentCPURequest:        "700m",
					RecommendedCPURequest:    "560m",
					CurrentMemoryRequest:     "5Gi",
					RecommendedMemoryRequest: "5Gi",
					Stability:                &RecommendationStability{CPU: StabilityGate{Status: "stable"}, Memory: StabilityGate{Status: "hold"}, Replicas: StabilityGate{Status: "hold"}},
					Learning:                 LearningEvidence{Persistent: &PersistentLearning{}},
				},
			},
		},
	}

	AttachSafetyAssessments(report)
	if got := report.Workloads[0].Recommendation.Safety.Classification; got != SafetyMediumRisk {
		t.Fatalf("safety = %s, want medium_risk", got)
	}
	AttachPatchPlans(worktree, profile, report)
	plan := report.Workloads[0].Recommendation.PatchPlan
	if plan == nil {
		t.Fatal("PatchPlan is nil")
	}
	if plan.Blocked {
		t.Fatalf("PatchPlan.Blocked = true, reasons=%v", plan.BlockReasons)
	}
	if !patchPlanApplyable(plan) {
		t.Fatalf("patch plan should be applyable: %#v", plan)
	}
}
