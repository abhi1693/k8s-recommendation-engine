package analyzer

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestWritePrettyReport(t *testing.T) {
	sample := 2.5
	memorySample := 128.0 * 1024 * 1024
	report := &Report{
		Application: "shipyard",
		Namespace:   "shipyardhq",
		GeneratedAt: time.Date(2026, 7, 8, 18, 55, 34, 0, time.UTC),
		Summary: Summary{
			WorkloadsTotal: 1,
			MetricsHealthy: 1,
		},
		SharedSignals: []SignalReport{
			{
				Name:    "request_rate",
				Healthy: true,
				Sample:  &sample,
				History: &SignalHistory{Points: 12, P50: 1, P95: 2, Max: 3},
				Anomaly: AnomalyStatus{State: "normal"},
			},
		},
		Workloads: []WorkloadReport{
			{
				Name:             "web",
				Namespace:        "shipyardhq",
				Deployment:       "shipyardhq",
				Replicas:         2,
				ReadyReplicas:    2,
				FleetManaged:     true,
				HelmRelease:      "shipyardhq",
				MetricsCondition: "healthy",
				MetricProfile:    "opentelemetry-http-deployment",
				Scaling:          ScalingReport{Replicas: true, CPU: true, Memory: true},
				PDBs: []PDBReport{
					{Name: "shipyardhq", MinAvailable: "2", MinimumReplicaFloor: 2},
				},
				Containers: []ContainerReport{
					{Name: "web", CPURequest: "700m", MemoryRequest: "5Gi"},
				},
				MetricSignals: []SignalReport{
					{
						Name:     "request_rate",
						Required: true,
						Healthy:  true,
						Sample:   &sample,
						History:  &SignalHistory{Points: 12, P50: 1, P95: 2, Max: 3},
						Anomaly:  AnomalyStatus{State: "normal"},
					},
					{
						Name:     "memory_working_set",
						Required: true,
						Healthy:  true,
						Sample:   &memorySample,
						History:  &SignalHistory{Points: 12, P50: 64 * 1024 * 1024, P95: 128 * 1024 * 1024, Max: 256 * 1024 * 1024},
						Anomaly:  AnomalyStatus{State: "normal"},
					},
				},
				Recommendation: Recommendation{
					Mode:                     "dry-run",
					CurrentReplicas:          2,
					RecommendedReplicas:      3,
					CurrentCPURequest:        "700m",
					RecommendedCPURequest:    "560m",
					CurrentMemoryRequest:     "5Gi",
					RecommendedMemoryRequest: "5Gi",
					Confidence:               0.95,
					Learning: LearningEvidence{
						Mode:        "prometheus-history",
						Description: "learned from test history",
						Signals: []LearnedSignal{
							{Name: "request_rate", Window: "6h", Points: 12, Current: 2.5, P50: 1, P95: 2, Max: 3, Classification: "near_or_above_learned_p95"},
							{Name: "memory_working_set", Window: "6h", Points: 12, Current: 128 * 1024 * 1024, P50: 64 * 1024 * 1024, P95: 128 * 1024 * 1024, Max: 256 * 1024 * 1024, Classification: "inside_learned_envelope"},
						},
						Decisions: []LearnedDecision{
							{Subject: "replicas.traffic", Learned: "peak_per_replica=1.5", Observed: "forecast=2.5", Conclusion: "traffic needs 3 replica(s)"},
						},
					},
					ReasonCodes: []string{
						"traffic_replicas:3",
						"cpu_request_decrease_recommended",
					},
				},
			},
		},
	}

	var output bytes.Buffer
	if err := WritePrettyReport(&output, report); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{
		"Decision Summary",
		"shipyardhq/shipyardhq",
		"replicas: 2 -> 3",
		"- traffic_replicas:3",
		"learning: prometheus-history",
		"learned signals:",
		"learned decisions:",
		"Shared Signals",
		"Metrics (opentelemetry-http-deployment)",
		"128Mi",
		"256Mi",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("pretty report missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "1.34218e+08") || strings.Contains(got, "2.68435e+08") {
		t.Fatalf("pretty report should humanize memory values:\n%s", got)
	}
}

func TestWriteSummaryReport(t *testing.T) {
	report := &Report{
		Application: "shipyard",
		Namespace:   "shipyardhq",
		GeneratedAt: time.Date(2026, 7, 8, 18, 55, 34, 0, time.UTC),
		Summary: Summary{
			WorkloadsTotal: 1,
			MetricsHealthy: 1,
		},
		Workloads: []WorkloadReport{
			{
				Namespace:  "shipyardhq",
				Deployment: "shipyardhq",
				Recommendation: Recommendation{
					CurrentReplicas:          2,
					RecommendedReplicas:      2,
					CurrentCPURequest:        "700m",
					RecommendedCPURequest:    "480m",
					CurrentMemoryRequest:     "5Gi",
					RecommendedMemoryRequest: "4250Mi",
				},
			},
		},
	}

	var output bytes.Buffer
	if err := WriteSummaryReport(&output, report); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{
		"K8s Recommendation Engine Summary",
		"Decision Summary",
		"shipyardhq/shipyardhq",
		"status:",
		"decision:",
		"replicas:",
		"cpu:",
		"memory:",
		"700m -> 480m",
		"Summary: workloads=1 commitBlocked=0 metricsHealthy=1 metricsDegraded=0 metricsUnhealthy=0",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary report missing %q:\n%s", want, got)
		}
	}
	for _, notWant := range []string{
		"WORKLOAD                         METRICS",
		"WORKLOAD                         STATUS",
		"Shared Signals",
		"Metrics (",
		"learning:",
	} {
		if strings.Contains(got, notWant) {
			t.Fatalf("summary report should not include %q:\n%s", notWant, got)
		}
	}
}

func TestWriteSummaryReportIncludesProposalFailure(t *testing.T) {
	report := &Report{
		Application: "shipyard",
		Namespace:   "shipyardhq",
		GeneratedAt: time.Date(2026, 7, 8, 18, 55, 34, 0, time.UTC),
		Summary: Summary{
			WorkloadsTotal: 1,
			MetricsHealthy: 1,
		},
		Proposal: &ProposalReport{
			Mode:         "propose",
			Kind:         "commit",
			Needed:       true,
			Blocked:      true,
			Branch:       "master",
			Commit:       "abc123",
			Remote:       "origin",
			Message:      "proposal commit created locally, but push failed",
			BlockReasons: []string{"push to origin/master failed; proposal commit exists only in the local worktree"},
			Errors:       []string{"push proposal commit: rejected"},
		},
		Workloads: []WorkloadReport{
			{
				Namespace:  "shipyardhq",
				Deployment: "shipyardhq",
				Recommendation: Recommendation{
					CurrentReplicas:          2,
					RecommendedReplicas:      2,
					CurrentCPURequest:        "700m",
					RecommendedCPURequest:    "700m",
					CurrentMemoryRequest:     "5Gi",
					RecommendedMemoryRequest: "5Gi",
				},
			},
		},
	}

	var output bytes.Buffer
	if err := WriteSummaryReport(&output, report); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{
		"Proposal: mode=propose kind=commit needed=true blocked=true",
		"Proposal git: branch=master commit=abc123",
		"Proposal push: pushed=false remote=origin ref=-",
		"Proposal note: proposal commit created locally, but push failed",
		"Proposal blocked: push to origin/master failed",
		"Proposal error: push proposal commit: rejected",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary report missing %q:\n%s", want, got)
		}
	}
}

func TestWriteSummaryReportIncludesGitHealth(t *testing.T) {
	report := &Report{
		Application: "shipyard",
		Namespace:   "shipyardhq",
		GeneratedAt: time.Date(2026, 7, 8, 18, 55, 34, 0, time.UTC),
		Summary: Summary{
			WorkloadsTotal: 1,
			MetricsHealthy: 1,
		},
		GitHealth: &GitHealthReport{
			Worktree:              "/git/home-lab",
			Branch:                "master",
			TargetBranch:          "master",
			Remote:                "origin",
			Upstream:              "origin/master",
			LocalCommit:           "abc123",
			RemoteCommit:          "def456",
			Ahead:                 1,
			Behind:                2,
			Diverged:              true,
			Dirty:                 true,
			DirtyLines:            []string{"M app.yaml"},
			LatestProposalCommit:  "abc123",
			LatestProposalSubject: "k8s-recommendation-engine: propose resource changes",
			Status:                "dirty",
			PushEnabled:           true,
		},
		Workloads: []WorkloadReport{
			{
				Namespace:  "shipyardhq",
				Deployment: "shipyardhq",
				Recommendation: Recommendation{
					CurrentReplicas:          2,
					RecommendedReplicas:      2,
					CurrentCPURequest:        "700m",
					RecommendedCPURequest:    "700m",
					CurrentMemoryRequest:     "5Gi",
					RecommendedMemoryRequest: "5Gi",
				},
			},
		},
	}

	var output bytes.Buffer
	if err := WriteSummaryReport(&output, report); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{
		"Git Worktree",
		"status=dirty branch=master target=master upstream=origin/master remote=origin push=true",
		"local=abc123 remoteHead=def456 ahead=1 behind=2 diverged=true dirty=true",
		"latestProposal=abc123 k8s-recommendation-engine: propose resource changes",
		"dirty: M app.yaml",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary report missing %q:\n%s", want, got)
		}
	}
}

func TestReplicaBasisPrefersScaleUpDriverOverAvailabilityFloor(t *testing.T) {
	recommendation := Recommendation{
		CurrentReplicas:     2,
		RecommendedReplicas: 3,
		ReasonCodes: []string{
			"availability_replica_floor:2",
			"memory_replicas:3",
			"replica_scale_up_recommended",
		},
	}

	if got := replicaBasis(recommendation); got != "learned memory pressure" {
		t.Fatalf("replicaBasis = %q, want learned memory pressure", got)
	}
}

func TestReplicaBasisUsesAvailabilityFloorForHoldAtFloor(t *testing.T) {
	recommendation := Recommendation{
		CurrentReplicas:     2,
		RecommendedReplicas: 2,
		ReasonCodes: []string{
			"availability_replica_floor:2",
			"replica_count_hold",
		},
	}

	if got := replicaBasis(recommendation); got != "availability floor" {
		t.Fatalf("replicaBasis = %q, want availability floor", got)
	}
}

func TestGateSummaryBlockedIncludesReason(t *testing.T) {
	gate := StabilityGate{Status: "blocked", Reason: "previous dry-run recommendation not applied"}

	if got := gateSummary(gate); got != "blocked: previous dry-run recommendation not applied" {
		t.Fatalf("gateSummary = %q", got)
	}
	if got := formatGate(gate); got != "blocked: previous dry-run recommendation not applied" {
		t.Fatalf("formatGate = %q", got)
	}
}

func TestWriteActionsReport(t *testing.T) {
	report := &Report{
		Application: "shipyard",
		Namespace:   "shipyardhq",
		GeneratedAt: time.Date(2026, 7, 8, 18, 55, 34, 0, time.UTC),
		Workloads: []WorkloadReport{
			{
				Namespace:  "shipyardhq",
				Deployment: "shipyardhq",
				Recommendation: Recommendation{
					CurrentReplicas:          2,
					RecommendedReplicas:      2,
					CurrentCPURequest:        "700m",
					RecommendedCPURequest:    "490m",
					CurrentMemoryRequest:     "5Gi",
					RecommendedMemoryRequest: "3892Mi",
					Confidence:               0.91,
					Stability: &RecommendationStability{
						Actionable: true,
						Replicas:   StabilityGate{Status: "hold"},
						CPU:        StabilityGate{Status: "stable", Observed: 3, Required: 3},
						Memory:     StabilityGate{Status: "stable", Observed: 3, Required: 3},
					},
					PatchPlan: &PatchPlan{
						SourceFile:   "shipyard/values.yaml",
						SourceFormat: patchSourceHelmValues,
						Resource:     "Deployment/shipyardhq/shipyardhq",
						Needed:       true,
						Changes: []PatchChange{
							{Field: "spec.template.spec.containers[name=web].resources.requests.cpu", SourcePath: []string{"resources", "requests", "cpu"}, Operation: "replace", Current: "700m", Recommended: "490m"},
						},
						Diff: "--- a/shipyard/values.yaml\n+++ b/shipyard/values.yaml\n@@ dry-run @@\n-    cpu: 700m\n+    cpu: 490m\n",
					},
				},
			},
		},
	}

	var output bytes.Buffer
	if err := WriteActionsReport(&output, report, true); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{
		"K8s Recommendation Engine Actions",
		"apply: yes",
		"manifest: shipyard/values.yaml format=helmValues",
		"gates: replicas=hold cpu=stable 3/3 memory=stable 3/3 actionable=true",
		"changes:",
		"cpu: 700m -> 490m",
		`source=helmValues["resources"]["requests"]["cpu"]`,
		"diff:",
		"+++ b/shipyard/values.yaml",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("actions report missing %q:\n%s", want, got)
		}
	}
	for _, notWant := range []string{"Shared Signals", "Metrics (", "learned signals:"} {
		if strings.Contains(got, notWant) {
			t.Fatalf("actions report should not include %q:\n%s", notWant, got)
		}
	}
}

func TestWriteActionsReportHidesChangesWhenProposalBlocked(t *testing.T) {
	report := &Report{
		Application: "shipyard",
		Namespace:   "shipyardhq",
		GeneratedAt: time.Date(2026, 7, 8, 18, 55, 34, 0, time.UTC),
		Proposal: &ProposalReport{
			Mode:         "propose",
			Kind:         "commit",
			Needed:       true,
			Blocked:      true,
			BlockReasons: []string{"git worktree is not clean"},
		},
		Workloads: []WorkloadReport{
			{
				Namespace:  "shipyardhq",
				Deployment: "shipyardhq",
				Recommendation: Recommendation{
					CurrentCPURequest:     "700m",
					RecommendedCPURequest: "490m",
					Confidence:            0.91,
					Stability: &RecommendationStability{
						Actionable: true,
						Replicas:   StabilityGate{Status: "hold"},
						CPU:        StabilityGate{Status: "stable", Observed: 3, Required: 3},
						Memory:     StabilityGate{Status: "hold"},
					},
					PatchPlan: &PatchPlan{
						SourceFile: "shipyard/deployment.yaml",
						Resource:   "Deployment/shipyardhq/shipyardhq",
						Needed:     true,
						Changes: []PatchChange{
							{Field: "spec.template.spec.containers[name=web].resources.requests.cpu", Operation: "replace", Current: "700m", Recommended: "490m"},
						},
						Diff: "--- a/shipyard/deployment.yaml\n+++ b/shipyard/deployment.yaml\n@@ dry-run @@\n-              cpu: 700m\n+              cpu: 490m\n",
					},
				},
			},
		},
	}

	var output bytes.Buffer
	if err := WriteActionsReport(&output, report, true); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{
		"Proposal: mode=propose kind=commit needed=true blocked=true",
		"Proposal blocked: git worktree is not clean",
		"apply: no",
		"eligible: yes (proposal is blocked; no changes were written)",
		"changes: hidden because proposal is blocked",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("blocked actions report missing %q:\n%s", want, got)
		}
	}
	for _, notWant := range []string{"changes:\n    - replace", "diff:", "+++ b/shipyard/deployment.yaml"} {
		if strings.Contains(got, notWant) {
			t.Fatalf("blocked actions report should not include %q:\n%s", notWant, got)
		}
	}
}

func TestFormatSignalValueHumanizesByteSignals(t *testing.T) {
	if got := formatSignalValue("memory_working_set", 529600000); got != "505Mi" {
		t.Fatalf("memory_working_set formatted as %q, want 505Mi", got)
	}
	if got := formatSignalValue("container_network_receive_bytes_total", 1536); got != "1.50Ki" {
		t.Fatalf("bytes metric formatted as %q, want 1.50Ki", got)
	}
	if got := formatSignalValue("request_rate", 2.5); got != "2.5" {
		t.Fatalf("request_rate formatted as %q, want 2.5", got)
	}
}

func TestReportJSONIncludesHumanizedByteDisplays(t *testing.T) {
	sample := 529600000.0
	report := Report{
		Workloads: []WorkloadReport{
			{
				Containers: []ContainerReport{
					{Name: "worker", MemoryRequestBytes: 608 * 1024 * 1024},
				},
				MetricSignals: []SignalReport{
					{
						Name:    "memory_working_set",
						Healthy: true,
						Sample:  &sample,
						History: &SignalHistory{
							Points: 10,
							Min:    450 * 1024 * 1024,
							P50:    456 * 1024 * 1024,
							P95:    501 * 1024 * 1024,
							Max:    506 * 1024 * 1024,
						},
					},
				},
				Recommendation: Recommendation{
					Learning: LearningEvidence{
						Signals: []LearnedSignal{
							{Name: "memory_working_set", Current: 529600000, P50: 456 * 1024 * 1024, P95: 501 * 1024 * 1024, Max: 506 * 1024 * 1024},
						},
					},
				},
			},
		},
	}

	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		`"sampleDisplay":"505Mi"`,
		`"historyDisplay":{"min":"450Mi","p50":"456Mi","p95":"501Mi","max":"506Mi"}`,
		`"currentDisplay":"505Mi"`,
		`"memoryRequestBytesDisplay":"608Mi"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("json report missing %q:\n%s", want, got)
		}
	}
}
