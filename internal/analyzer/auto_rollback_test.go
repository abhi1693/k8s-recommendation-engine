package analyzer

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abhi1693/k8s-recommendation-engine/internal/config"
)

func TestAutoRollbackIgnoresPendingRegression(t *testing.T) {
	observation := &ObservationReport{
		Application: "shipyard",
		Namespace:   "shipyardhq",
		GeneratedAt: time.Date(2026, 7, 9, 20, 0, 0, 0, time.UTC),
		Git:         GitObservation{LatestProposalCommit: "abc123"},
		Workloads: []WorkloadObservation{
			{Namespace: "shipyardhq", Deployment: "shipyardhq", Status: "pending", Outcome: "regressed"},
		},
	}

	report := AutoRollback(context.Background(), "", observationProfile(), observationReport("490m", "3892Mi"), observation, RollbackOptions{})
	if report.Needed {
		t.Fatalf("Needed = true, want false: %#v", report)
	}
	if report.Rollback != nil {
		t.Fatalf("Rollback should not be created: %#v", report.Rollback)
	}
}

func TestAutoRollbackRevertsAppliedUnsafeProposal(t *testing.T) {
	worktree := initProposalGitRepo(t)
	writeRepoFile(t, worktree, "apps/deployment.yaml", "cpu: 700m\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "initial")
	writeRepoFile(t, worktree, "apps/deployment.yaml", "cpu: 490m\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "k8s-recommendation-engine: propose shipyard resource changes")
	latest := strings.TrimSpace(gitTest(t, worktree, "rev-parse", "--short", "HEAD"))
	observation := &ObservationReport{
		Application: "shipyard",
		Namespace:   "shipyardhq",
		GeneratedAt: time.Date(2026, 7, 9, 20, 0, 0, 0, time.UTC),
		Git: GitObservation{
			Worktree:             worktree,
			Branch:               "master",
			BaseBranch:           "master",
			LatestProposalCommit: latest,
		},
		Workloads: []WorkloadObservation{
			{Namespace: "shipyardhq", Deployment: "shipyardhq", Status: "applied", Outcome: "unsafe", MetricsCondition: "unhealthy"},
		},
		Summary: ObservationSummary{Applied: 1, Unsafe: 1, WorkloadsTotal: 1},
	}
	profile := &config.ApplicationProfile{
		Metadata: config.Metadata{Name: "shipyard"},
		Spec: config.ApplicationSpec{
			Namespace: "shipyardhq",
			Git:       config.GitSpec{Branch: "master", BasePath: "apps"},
		},
	}
	analysis := &Report{Application: "shipyard", Namespace: "shipyardhq", GeneratedAt: observation.GeneratedAt}

	report := AutoRollback(context.Background(), worktree, profile, analysis, observation, RollbackOptions{
		Branch:        "master",
		DefaultBranch: "master",
	})
	if report.Blocked || !report.Needed {
		t.Fatalf("auto rollback = %#v, want needed and unblocked", report)
	}
	if report.Rollback == nil || report.Rollback.Commit == "" {
		t.Fatalf("rollback proposal missing: %#v", report.Rollback)
	}
	content, err := os.ReadFile(filepath.Join(worktree, "apps", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "cpu: 700m\n" {
		t.Fatalf("rollback content = %q", string(content))
	}
}

func TestWriteAutoRollbackReport(t *testing.T) {
	report := &AutoRollbackReport{
		Application: "shipyard",
		Namespace:   "shipyardhq",
		GeneratedAt: time.Date(2026, 7, 9, 20, 0, 0, 0, time.UTC),
		Needed:      true,
		Reasons:     []string{"shipyardhq/shipyardhq applied outcome=unsafe metrics=unhealthy"},
		Observation: &ObservationReport{Summary: ObservationSummary{Applied: 1, Unsafe: 1}, Git: GitObservation{LatestProposalCommit: "abc123"}},
	}
	var output bytes.Buffer
	if err := WriteAutoRollbackReport(&output, report); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{"Auto Rollback", "needed=true", "outcome=unsafe", "Observation: applied=1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("report missing %q:\n%s", want, got)
		}
	}
}
