package analyzer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abhi1693/k8s-recommendation-engine/internal/config"
)

func TestCreateProposalWritesPatchArtifact(t *testing.T) {
	worktree := t.TempDir()
	sourcePath := filepath.Join(worktree, "apps", "deployment.yaml")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte(`
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
`), 0o644); err != nil {
		t.Fatal(err)
	}
	report := &Report{
		Application: "shipyard",
		GeneratedAt: time.Date(2026, 7, 8, 20, 0, 0, 0, time.UTC),
		Workloads: []WorkloadReport{
			{
				Namespace:  "shipyardhq",
				Deployment: "shipyardhq",
				Recommendation: Recommendation{
					PatchPlan: &PatchPlan{
						SourceFile: "apps/deployment.yaml",
						Resource:   "Deployment/shipyardhq/shipyardhq",
						Needed:     true,
						Changes: []PatchChange{
							{Field: "spec.template.spec.containers[name=web].resources.requests.cpu", Operation: "replace", Current: "700m", Recommended: "490m"},
						},
					},
				},
			},
		},
	}

	proposal := CreateProposal(context.Background(), worktree, report, ProposalOptions{
		Kind:        "patch",
		PatchDir:    ".k8s-recommendation-engine/proposals",
		GeneratedAt: report.GeneratedAt,
	})
	if proposal.Blocked {
		t.Fatalf("proposal blocked: %#v", proposal)
	}
	if proposal.PatchFile == "" {
		t.Fatalf("PatchFile is empty: %#v", proposal)
	}
	patch, err := os.ReadFile(filepath.Join(worktree, filepath.FromSlash(proposal.PatchFile)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(patch), "+              cpu: 490m") {
		t.Fatalf("patch missing cpu change:\n%s", string(patch))
	}
	manifest, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(manifest), "cpu: 490m") {
		t.Fatalf("patch proposal should not modify source manifest:\n%s", string(manifest))
	}
}

func TestCreateProposalCommitAllowsExplicitDefaultBranch(t *testing.T) {
	worktree := initProposalGitRepo(t)
	writeRepoFile(t, worktree, "app.yaml", `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: shipyardhq
  namespace: shipyardhq
spec: {}
`)
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "initial")
	report := &Report{
		Application: "shipyard",
		GeneratedAt: time.Date(2026, 7, 8, 20, 0, 0, 0, time.UTC),
		Workloads: []WorkloadReport{
			{
				Namespace:  "shipyardhq",
				Deployment: "shipyardhq",
				Recommendation: Recommendation{
					PatchPlan: &PatchPlan{
						SourceFile: "app.yaml",
						Resource:   "Deployment/shipyardhq/shipyardhq",
						Needed:     true,
						Changes: []PatchChange{
							{Field: "spec.replicas", Operation: "add", Current: missingValue, Recommended: "2"},
						},
					},
				},
			},
		},
	}

	proposal := CreateProposal(context.Background(), worktree, report, ProposalOptions{
		Kind:          "commit",
		BranchName:    "master",
		DefaultBranch: "master",
	})
	if proposal.Blocked || len(proposal.Errors) > 0 {
		t.Fatalf("proposal should allow explicit existing branch: %#v", proposal)
	}
	if proposal.Branch != "master" || proposal.Commit == "" {
		t.Fatalf("proposal branch/commit = %q/%q", proposal.Branch, proposal.Commit)
	}
}

func TestCreateProposalCommitBlocksDefaultBranchPushWithoutAllow(t *testing.T) {
	worktree := initProposalGitRepo(t)
	writeRepoFile(t, worktree, "app.yaml", `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: shipyardhq
  namespace: shipyardhq
spec: {}
`)
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "initial")
	report := proposalReplicasReport()

	proposal := CreateProposal(context.Background(), worktree, report, ProposalOptions{
		Kind:          "commit",
		BranchName:    "master",
		DefaultBranch: "master",
		Remote:        "origin",
		Push:          true,
	})
	if !proposal.Blocked {
		t.Fatalf("proposal should block default branch push without allow flag: %#v", proposal)
	}
	if len(proposal.BlockReasons) != 1 || !strings.Contains(proposal.BlockReasons[0], "--allow-default-branch-push") {
		t.Fatalf("BlockReasons = %#v, want default branch push block", proposal.BlockReasons)
	}
	if proposal.Commit != "" || proposal.Pushed {
		t.Fatalf("blocked push should not commit or push: %#v", proposal)
	}
	content, err := os.ReadFile(filepath.Join(worktree, "app.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "replicas: 2") {
		t.Fatalf("blocked default branch push wrote manifest:\n%s", string(content))
	}
}

func TestCreateProposalCommitPushesDefaultBranchWithAllow(t *testing.T) {
	worktree := initProposalGitRepo(t)
	remote := initBareGitRepo(t)
	gitTest(t, worktree, "remote", "add", "origin", remote)
	writeRepoFile(t, worktree, "app.yaml", `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: shipyardhq
  namespace: shipyardhq
spec: {}
`)
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "initial")
	gitTest(t, worktree, "push", "-u", "origin", "master")
	report := proposalReplicasReport()

	proposal := CreateProposal(context.Background(), worktree, report, ProposalOptions{
		Kind:                   "commit",
		BranchName:             "master",
		DefaultBranch:          "master",
		Remote:                 "origin",
		Push:                   true,
		AllowDefaultBranchPush: true,
	})
	if proposal.Blocked || len(proposal.Errors) > 0 {
		t.Fatalf("proposal should push default branch with allow flag: %#v", proposal)
	}
	if !proposal.Pushed || proposal.Remote != "origin" || proposal.PushRef != "origin/master" {
		t.Fatalf("push fields = pushed:%t remote:%q ref:%q", proposal.Pushed, proposal.Remote, proposal.PushRef)
	}
	remoteHead := strings.TrimSpace(gitBareTest(t, remote, "rev-parse", "--short", "master"))
	if remoteHead == "" || remoteHead != proposal.Commit {
		t.Fatalf("remote master = %q, proposal commit = %q", remoteHead, proposal.Commit)
	}
}

func TestCreateProposalCommitMarksPushFailureBlocked(t *testing.T) {
	worktree := initProposalGitRepo(t)
	remote := initBareGitRepo(t)
	gitTest(t, worktree, "remote", "add", "origin", remote)
	writeRepoFile(t, worktree, "app.yaml", `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: shipyardhq
  namespace: shipyardhq
spec: {}
`)
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "initial")
	gitTest(t, worktree, "push", "-u", "origin", "master")
	hook := filepath.Join(remote, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho rejected by test hook >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	proposal := CreateProposal(context.Background(), worktree, proposalReplicasReport(), ProposalOptions{
		Kind:                   "commit",
		BranchName:             "master",
		DefaultBranch:          "master",
		Remote:                 "origin",
		Push:                   true,
		AllowDefaultBranchPush: true,
	})
	if !proposal.Blocked {
		t.Fatalf("proposal should be blocked after push failure: %#v", proposal)
	}
	if proposal.Commit == "" {
		t.Fatalf("proposal commit should be recorded after local commit: %#v", proposal)
	}
	if proposal.Pushed {
		t.Fatalf("proposal.Pushed = true, want false: %#v", proposal)
	}
	if proposal.Remote != "origin" {
		t.Fatalf("proposal.Remote = %q, want origin", proposal.Remote)
	}
	if len(proposal.BlockReasons) != 1 || !strings.Contains(proposal.BlockReasons[0], "push to origin/master failed") {
		t.Fatalf("BlockReasons = %#v, want push failure block", proposal.BlockReasons)
	}
	if len(proposal.Errors) != 1 || !strings.Contains(proposal.Errors[0], "push proposal commit") || !strings.Contains(proposal.Errors[0], "rejected by test hook") {
		t.Fatalf("Errors = %#v, want push error", proposal.Errors)
	}
	if proposal.Message != "proposal commit created locally, but push failed" {
		t.Fatalf("Message = %q", proposal.Message)
	}
	remoteHead := strings.TrimSpace(gitBareTest(t, remote, "rev-parse", "--short", "master"))
	if remoteHead == proposal.Commit {
		t.Fatalf("remote should not advance to failed proposal commit %s", proposal.Commit)
	}
}

func TestCreateProposalCommitPullsRebaseWhenRemoteAdvanced(t *testing.T) {
	worktree := initProposalGitRepo(t)
	remote := initBareGitRepo(t)
	gitTest(t, worktree, "remote", "add", "origin", remote)
	writeRepoFile(t, worktree, "app.yaml", `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: shipyardhq
  namespace: shipyardhq
spec: {}
`)
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "initial")
	gitTest(t, worktree, "push", "-u", "origin", "master")

	other := cloneGitRepo(t, remote)
	gitTest(t, other, "config", "user.name", "K8s Recommendation Engine Test")
	gitTest(t, other, "config", "user.email", "k8s-recommendation-engine@example.invalid")
	writeRepoFile(t, other, "app.yaml", `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: shipyardhq
  namespace: shipyardhq
  labels:
    remote: advanced
spec: {}
`)
	gitTest(t, other, "add", ".")
	gitTest(t, other, "commit", "-m", "remote update")
	gitTest(t, other, "push")

	proposal := CreateProposal(context.Background(), worktree, proposalReplicasReport(), ProposalOptions{
		Kind:                   "commit",
		BranchName:             "master",
		DefaultBranch:          "master",
		Remote:                 "origin",
		Push:                   true,
		AllowDefaultBranchPush: true,
	})
	if proposal.Blocked {
		t.Fatalf("proposal should pull --rebase stale local branch: %#v", proposal)
	}
	if proposal.Commit == "" || !proposal.Pushed {
		t.Fatalf("proposal should commit and push after pull --rebase: %#v", proposal)
	}
	remoteHead := strings.TrimSpace(gitBareTest(t, remote, "rev-parse", "--short", "master"))
	if remoteHead != proposal.Commit {
		t.Fatalf("remote master = %q, proposal commit = %q", remoteHead, proposal.Commit)
	}
	content, err := os.ReadFile(filepath.Join(worktree, "app.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "remote: advanced") || !strings.Contains(string(content), "replicas: 2") {
		t.Fatalf("proposal did not preserve remote update and apply recommendation:\n%s", string(content))
	}
}

func TestCreateProposalCommitBlocksWhenBranchAlreadyAhead(t *testing.T) {
	worktree := initProposalGitRepo(t)
	remote := initBareGitRepo(t)
	gitTest(t, worktree, "remote", "add", "origin", remote)
	writeRepoFile(t, worktree, "app.yaml", `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: shipyardhq
  namespace: shipyardhq
spec: {}
`)
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "initial")
	gitTest(t, worktree, "push", "-u", "origin", "master")
	writeRepoFile(t, worktree, "local.yaml", "local: proposal\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "k8s-recommendation-engine: propose existing local change")
	beforeHead := strings.TrimSpace(gitTest(t, worktree, "rev-parse", "HEAD"))

	proposal := CreateProposal(context.Background(), worktree, proposalReplicasReport(), ProposalOptions{
		Kind:                   "commit",
		BranchName:             "master",
		DefaultBranch:          "master",
		Remote:                 "origin",
		Push:                   true,
		AllowDefaultBranchPush: true,
	})
	if !proposal.Blocked {
		t.Fatalf("proposal should block additional local proposal commit: %#v", proposal)
	}
	if len(proposal.BlockReasons) != 1 || !strings.Contains(proposal.BlockReasons[0], "already has 1 unpushed commit") {
		t.Fatalf("BlockReasons = %#v, want unpushed commit block", proposal.BlockReasons)
	}
	afterHead := strings.TrimSpace(gitTest(t, worktree, "rev-parse", "HEAD"))
	if afterHead != beforeHead {
		t.Fatalf("HEAD changed from %s to %s", beforeHead, afterHead)
	}
	content, err := os.ReadFile(filepath.Join(worktree, "app.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "replicas: 2") {
		t.Fatalf("blocked ahead proposal wrote manifest:\n%s", string(content))
	}
}

func TestCreateProposalPushesExistingProposalCommitWhenNoNewChanges(t *testing.T) {
	worktree := initProposalGitRepo(t)
	remote := initBareGitRepo(t)
	gitTest(t, worktree, "remote", "add", "origin", remote)
	writeRepoFile(t, worktree, "app.yaml", "cpu: 700m\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "initial")
	gitTest(t, worktree, "push", "-u", "origin", "master")
	writeRepoFile(t, worktree, "app.yaml", "cpu: 490m\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "k8s-recommendation-engine: propose shipyard resource changes")
	report := &Report{Application: "shipyard", GeneratedAt: time.Date(2026, 7, 8, 20, 0, 0, 0, time.UTC)}

	proposal := CreateProposal(context.Background(), worktree, report, ProposalOptions{
		Kind:                   "commit",
		BranchName:             "master",
		DefaultBranch:          "master",
		Remote:                 "origin",
		Push:                   true,
		AllowDefaultBranchPush: true,
	})
	if proposal.Blocked || len(proposal.Errors) > 0 {
		t.Fatalf("proposal should push existing proposal commit: %#v", proposal)
	}
	if proposal.Needed {
		t.Fatalf("proposal.Needed = true, want false for existing commit publish")
	}
	if !proposal.Pushed || proposal.Commit == "" {
		t.Fatalf("proposal push fields = %#v", proposal)
	}
	remoteHead := strings.TrimSpace(gitBareTest(t, remote, "rev-parse", "--short", "master"))
	if remoteHead != proposal.Commit {
		t.Fatalf("remote master = %q, proposal commit = %q", remoteHead, proposal.Commit)
	}
}

func TestCreateProposalRefusesExistingNonProposalCommitWhenNoNewChanges(t *testing.T) {
	worktree := initProposalGitRepo(t)
	remote := initBareGitRepo(t)
	gitTest(t, worktree, "remote", "add", "origin", remote)
	writeRepoFile(t, worktree, "app.yaml", "cpu: 700m\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "manual change")
	report := &Report{Application: "shipyard", GeneratedAt: time.Date(2026, 7, 8, 20, 0, 0, 0, time.UTC)}

	proposal := CreateProposal(context.Background(), worktree, report, ProposalOptions{
		Kind:                   "commit",
		BranchName:             "master",
		DefaultBranch:          "master",
		Remote:                 "origin",
		Push:                   true,
		AllowDefaultBranchPush: true,
	})
	if !proposal.Blocked {
		t.Fatalf("proposal should block non-proposal latest commit: %#v", proposal)
	}
	if len(proposal.BlockReasons) != 1 || !strings.Contains(proposal.BlockReasons[0], "latest commit is not an k8s-recommendation-engine proposal") {
		t.Fatalf("BlockReasons = %#v, want non-proposal commit block", proposal.BlockReasons)
	}
}

func TestCreateProposalCommitBlocksExistingNonDefaultBranch(t *testing.T) {
	worktree := initProposalGitRepo(t)
	writeRepoFile(t, worktree, "app.yaml", `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: shipyardhq
  namespace: shipyardhq
spec: {}
`)
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "initial")
	gitTest(t, worktree, "switch", "-c", "feature/existing")
	gitTest(t, worktree, "switch", "master")
	report := &Report{
		Application: "shipyard",
		GeneratedAt: time.Date(2026, 7, 8, 20, 0, 0, 0, time.UTC),
		Workloads: []WorkloadReport{
			{
				Namespace:  "shipyardhq",
				Deployment: "shipyardhq",
				Recommendation: Recommendation{
					PatchPlan: &PatchPlan{
						SourceFile: "app.yaml",
						Resource:   "Deployment/shipyardhq/shipyardhq",
						Needed:     true,
						Changes: []PatchChange{
							{Field: "spec.replicas", Operation: "add", Current: missingValue, Recommended: "2"},
						},
					},
				},
			},
		},
	}

	proposal := CreateProposal(context.Background(), worktree, report, ProposalOptions{
		Kind:          "commit",
		BranchName:    "feature/existing",
		DefaultBranch: "master",
	})
	if !proposal.Blocked {
		t.Fatalf("proposal should block unrelated existing branch: %#v", proposal)
	}
	if len(proposal.BlockReasons) != 1 || !strings.Contains(proposal.BlockReasons[0], "not allowed") {
		t.Fatalf("BlockReasons = %#v, want non-default branch block", proposal.BlockReasons)
	}
	if proposal.Branch != "" || proposal.Commit != "" {
		t.Fatalf("proposal should not write branch/commit, got %q/%q", proposal.Branch, proposal.Commit)
	}
	content, err := os.ReadFile(filepath.Join(worktree, "app.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "replicas: 2") {
		t.Fatalf("blocked proposal wrote manifest:\n%s", string(content))
	}
}

func TestBuildProposalMergesChangesForSameFile(t *testing.T) {
	worktree := t.TempDir()
	sourcePath := filepath.Join(worktree, "apps", "deployment.yaml")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte(`
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
  template:
    spec:
      containers:
        - name: worker
          resources:
            requests:
              cpu: 140m
              memory: 608Mi
`), 0o644); err != nil {
		t.Fatal(err)
	}
	report := &Report{
		Application: "shipyard",
		Workloads: []WorkloadReport{
			{
				Namespace:  "shipyardhq",
				Deployment: "shipyardhq",
				Recommendation: Recommendation{PatchPlan: &PatchPlan{
					SourceFile: "apps/deployment.yaml",
					Resource:   "Deployment/shipyardhq/shipyardhq",
					Needed:     true,
					Changes: []PatchChange{
						{Field: "spec.template.spec.containers[name=web].resources.requests.cpu", Operation: "replace", Current: "700m", Recommended: "490m"},
					},
				}},
			},
			{
				Namespace:  "shipyardhq",
				Deployment: "shipyardhq-worker",
				Recommendation: Recommendation{PatchPlan: &PatchPlan{
					SourceFile: "apps/deployment.yaml",
					Resource:   "Deployment/shipyardhq/shipyardhq-worker",
					Needed:     true,
					Changes: []PatchChange{
						{Field: "spec.template.spec.containers[name=worker].resources.requests.cpu", Operation: "replace", Current: "140m", Recommended: "110m"},
					},
				}},
			},
		},
	}

	proposal := BuildProposal(worktree, report)
	if proposal.Blocked {
		t.Fatalf("proposal blocked: %#v", proposal)
	}
	if len(proposal.Files) != 1 {
		t.Fatalf("len(Files) = %d, want 1: %#v", len(proposal.Files), proposal.Files)
	}
	file := proposal.Files[0]
	for _, want := range []string{"cpu: 490m", "cpu: 110m"} {
		if !strings.Contains(file.ProposedContent, want) {
			t.Fatalf("proposed content missing %q:\n%s", want, file.ProposedContent)
		}
	}
	if len(file.Changes) != 2 {
		t.Fatalf("len(Changes) = %d, want 2", len(file.Changes))
	}
}

func TestBuildProposalMarksBlockedWhenOnlyPlansAreBlocked(t *testing.T) {
	report := &Report{
		Application: "shipyard",
		Workloads: []WorkloadReport{
			{
				Namespace:  "shipyardhq",
				Deployment: "shipyardhq",
				Recommendation: Recommendation{PatchPlan: &PatchPlan{
					SourceFile:   "apps/deployment.yaml",
					Resource:     "Deployment/shipyardhq/shipyardhq",
					Needed:       false,
					Blocked:      true,
					BlockReasons: []string{"workload rollout is not settled: incomplete_init_pods:1"},
				}},
			},
		},
	}

	proposal := BuildProposal(t.TempDir(), report)
	if !proposal.Blocked {
		t.Fatalf("proposal.Blocked = false, want true: %#v", proposal)
	}
	if len(proposal.BlockReasons) != 1 {
		t.Fatalf("BlockReasons = %#v, want blocked plan reason", proposal.BlockReasons)
	}
}

func TestBuildProposalMergesHelmValuesChangesForSharedFile(t *testing.T) {
	worktree := t.TempDir()
	writeRepoFile(t, worktree, "zitadel/values.yaml", `
# shared chart values
replicaCount: 2
resources:
  requests:
    cpu: 150m
    memory: 384Mi
login:
  replicaCount: 2
  resources:
    requests:
      cpu: 50m
      memory: 128Mi
`)
	report := &Report{Application: "zitadel", Workloads: []WorkloadReport{
		{
			Namespace: "zitadel", Deployment: "zitadel",
			Recommendation: Recommendation{PatchPlan: &PatchPlan{
				SourceFile: "zitadel/values.yaml", SourceFormat: patchSourceHelmValues,
				Resource: "Deployment/zitadel/zitadel", Needed: true,
				Changes: []PatchChange{
					{Field: "spec.replicas", SourcePath: []string{"replicaCount"}, Operation: "replace", Current: "2", Recommended: "3"},
					{Field: "spec.template.spec.containers[name=zitadel].resources.requests.cpu", SourcePath: []string{"resources", "requests", "cpu"}, Operation: "replace", Current: "150m", Recommended: "125m"},
				},
			}},
		},
		{
			Namespace: "zitadel", Deployment: "zitadel-login",
			Recommendation: Recommendation{PatchPlan: &PatchPlan{
				SourceFile: "zitadel/values.yaml", SourceFormat: patchSourceHelmValues,
				Resource: "Deployment/zitadel/zitadel-login", Needed: true,
				Changes: []PatchChange{
					{Field: "spec.replicas", SourcePath: []string{"login", "replicaCount"}, Operation: "replace", Current: "2", Recommended: "3"},
					{Field: "spec.template.spec.containers[name=zitadel-login].resources.requests.memory", SourcePath: []string{"login", "resources", "requests", "memory"}, Operation: "replace", Current: "128Mi", Recommended: "112Mi"},
				},
			}},
		},
	}}

	proposal := BuildProposal(worktree, report)
	if proposal.Blocked || len(proposal.Files) != 1 {
		t.Fatalf("proposal = %#v, want one applyable shared values file", proposal)
	}
	file := proposal.Files[0]
	if len(file.Changes) != 4 {
		t.Fatalf("Changes = %#v, want four merged changes", file.Changes)
	}
	for _, want := range []string{"# shared chart values", "replicaCount: 3", "cpu: 125m", "memory: 112Mi"} {
		if !strings.Contains(file.ProposedContent, want) {
			t.Fatalf("ProposedContent missing %q:\n%s", want, file.ProposedContent)
		}
	}
	if strings.Count(file.ProposedContent, "replicaCount: 3") != 2 {
		t.Fatalf("ProposedContent did not update both replica paths:\n%s", file.ProposedContent)
	}
}

func TestHelmValuesPatchPlanningFlowsIntoSharedFileProposal(t *testing.T) {
	worktree := t.TempDir()
	writeRepoFile(t, worktree, "charts/zitadel/values.yaml", "replicaCount: 2\nlogin:\n  replicaCount: 2\n")
	profile := &config.ApplicationProfile{Spec: config.ApplicationSpec{
		Namespace: "zitadel",
		Git:       config.GitSpec{BasePath: "charts/zitadel"},
		Workloads: []config.WorkloadSpec{
			{
				Name: "server", SourceFile: "values.yaml",
				TargetRef:  config.TargetRef{Kind: "Deployment", Name: "zitadel"},
				HelmValues: &config.HelmValuesSpec{Paths: config.HelmValuePaths{Replicas: []string{"replicaCount"}}},
				Scaling:    config.ScalingSpec{Replicas: true},
			},
			{
				Name: "login", SourceFile: "values.yaml",
				TargetRef:  config.TargetRef{Kind: "Deployment", Name: "zitadel-login"},
				HelmValues: &config.HelmValuesSpec{Paths: config.HelmValuePaths{Replicas: []string{"login", "replicaCount"}}},
				Scaling:    config.ScalingSpec{Replicas: true},
			},
		},
	}}
	report := &Report{Application: "zitadel", Workloads: []WorkloadReport{
		{
			Name: "server", Namespace: "zitadel", Deployment: "zitadel", Replicas: 2,
			Recommendation: Recommendation{
				CurrentReplicas: 2, RecommendedReplicas: 3,
				Stability: &RecommendationStability{Replicas: StabilityGate{Status: "stable", Observed: 3, Required: 3}},
			},
		},
		{
			Name: "login", Namespace: "zitadel", Deployment: "zitadel-login", Replicas: 2,
			Recommendation: Recommendation{
				CurrentReplicas: 2, RecommendedReplicas: 3,
				Stability: &RecommendationStability{Replicas: StabilityGate{Status: "stable", Observed: 3, Required: 3}},
			},
		},
	}}

	AttachPatchPlans(worktree, profile, report)
	for _, workload := range report.Workloads {
		plan := workload.Recommendation.PatchPlan
		if plan == nil || plan.SourceFormat != patchSourceHelmValues || !plan.Needed || len(plan.Changes) != 1 {
			t.Fatalf("workload %s plan = %#v", workload.Name, plan)
		}
	}
	proposal := BuildProposal(worktree, report)
	if proposal.Blocked || len(proposal.Files) != 1 || strings.Count(proposal.Files[0].ProposedContent, "replicaCount: 3") != 2 {
		t.Fatalf("proposal = %#v, want both Helm workloads merged", proposal)
	}
}

func TestBuildProposalHelmValuesRejectsStaleCurrentValue(t *testing.T) {
	worktree := t.TempDir()
	writeRepoFile(t, worktree, "values.yaml", "replicaCount: 4\n")
	report := helmProposalReport([]PatchChange{
		{Field: "spec.replicas", SourcePath: []string{"replicaCount"}, Operation: "replace", Current: "2", Recommended: "3"},
	})

	proposal := BuildProposal(worktree, report)
	if !proposal.Blocked || !strings.Contains(strings.Join(proposal.Errors, "\n"), "changed after planning") {
		t.Fatalf("proposal = %#v, want stale-value block", proposal)
	}
}

func TestBuildProposalHelmValuesPreservesChartScalarTypes(t *testing.T) {
	worktree := t.TempDir()
	writeRepoFile(t, worktree, "values.yaml", "replicaCount: \"2\"\nresources:\n  requests:\n    cpu: 500m\n")
	report := helmProposalReport([]PatchChange{
		{Field: "spec.replicas", SourcePath: []string{"replicaCount"}, Operation: "replace", Current: "2", Recommended: "3"},
		{Field: "spec.template.spec.containers[name=app].resources.requests.cpu", SourcePath: []string{"resources", "requests", "cpu"}, Operation: "replace", Current: "500m", Recommended: "1"},
	})

	proposal := BuildProposal(worktree, report)
	if proposal.Blocked || len(proposal.Files) != 1 {
		t.Fatalf("proposal = %#v, want applyable Helm values change", proposal)
	}
	documents, err := decodeYAMLDocuments([]byte(proposal.Files[0].ProposedContent))
	if err != nil {
		t.Fatal(err)
	}
	replicas, ok, err := helmScalarNodeAt(documents[0], []string{"replicaCount"})
	if err != nil || !ok || replicas.Tag != "!!str" || replicas.Value != "3" {
		t.Fatalf("replica scalar = %#v ok=%t err=%v, want quoted string 3", replicas, ok, err)
	}
	cpu, ok, err := helmScalarNodeAt(documents[0], []string{"resources", "requests", "cpu"})
	if err != nil || !ok || cpu.Tag != "!!str" || cpu.Value != "1" {
		t.Fatalf("CPU scalar = %#v ok=%t err=%v, want string quantity 1", cpu, ok, err)
	}
}

func TestBuildProposalHelmValuesRejectsOverlappingAndMixedSources(t *testing.T) {
	tests := []struct {
		name   string
		plans  []*PatchPlan
		values string
		want   string
	}{
		{
			name: "conflicting duplicate path",
			plans: []*PatchPlan{
				{SourceFile: "values.yaml", SourceFormat: patchSourceHelmValues, Needed: true, Changes: []PatchChange{{Field: "spec.replicas", SourcePath: []string{"replicaCount"}, Operation: "replace", Current: "2", Recommended: "3"}}},
				{SourceFile: "values.yaml", SourceFormat: patchSourceHelmValues, Needed: true, Changes: []PatchChange{{Field: "spec.replicas", SourcePath: []string{"replicaCount"}, Operation: "replace", Current: "2", Recommended: "4"}}},
			},
			values: "replicaCount: 2\n",
			want:   "overlap",
		},
		{
			name: "prefix path",
			plans: []*PatchPlan{
				{SourceFile: "values.yaml", SourceFormat: patchSourceHelmValues, Needed: true, Changes: []PatchChange{{Field: "spec.replicas", SourcePath: []string{"server"}, Operation: "replace", Current: "2", Recommended: "3"}}},
				{SourceFile: "values.yaml", SourceFormat: patchSourceHelmValues, Needed: true, Changes: []PatchChange{{Field: "spec.replicas", SourcePath: []string{"server", "replicas"}, Operation: "replace", Current: "2", Recommended: "4"}}},
			},
			values: "server:\n  replicas: 2\n",
			want:   "overlap",
		},
		{
			name: "mixed formats",
			plans: []*PatchPlan{
				{SourceFile: "values.yaml", SourceFormat: patchSourceHelmValues, Needed: true, Changes: []PatchChange{{Field: "spec.replicas", SourcePath: []string{"replicaCount"}, Current: "2", Recommended: "3"}}},
				{SourceFile: "values.yaml", Needed: true, Changes: []PatchChange{{Field: "spec.replicas", Current: "2", Recommended: "3"}}},
			},
			values: "replicaCount: 2\n",
			want:   "mixes",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			worktree := t.TempDir()
			writeRepoFile(t, worktree, "values.yaml", test.values)
			report := &Report{Application: "helm", Workloads: make([]WorkloadReport, 0, len(test.plans))}
			for index, plan := range test.plans {
				plan.Resource = fmt.Sprintf("Deployment/default/app-%d", index)
				report.Workloads = append(report.Workloads, WorkloadReport{
					Namespace: "default", Deployment: fmt.Sprintf("app-%d", index),
					Recommendation: Recommendation{PatchPlan: plan},
				})
			}
			proposal := BuildProposal(worktree, report)
			if !proposal.Blocked || !strings.Contains(strings.Join(proposal.Errors, "\n"), test.want) {
				t.Fatalf("proposal = %#v, want error containing %q", proposal, test.want)
			}
		})
	}
}

func helmProposalReport(changes []PatchChange) *Report {
	return &Report{Application: "helm", Workloads: []WorkloadReport{
		{
			Namespace: "default", Deployment: "app",
			Recommendation: Recommendation{PatchPlan: &PatchPlan{
				SourceFile: "values.yaml", SourceFormat: patchSourceHelmValues,
				Resource: "Deployment/default/app", Needed: true, Changes: changes,
			}},
		},
	}}
}

func TestBlockingGitStatusLinesIgnoresProposalArtifacts(t *testing.T) {
	status := "?? .k8s-recommendation-engine/\n M kubernetes/app.yaml\n?? notes.txt\n"
	dirty := blockingGitStatusLines(status)
	if len(dirty) != 2 {
		t.Fatalf("len(dirty) = %d, want 2: %#v", len(dirty), dirty)
	}
	if dirty[0] != "M kubernetes/app.yaml" || dirty[1] != "?? notes.txt" {
		t.Fatalf("dirty = %#v", dirty)
	}
	if got := blockingGitStatusLines("?? .k8s-recommendation-engine/\n"); len(got) != 0 {
		t.Fatalf("proposal artifacts should be ignored, got %#v", got)
	}
}

func TestProposalStatusAndDiff(t *testing.T) {
	worktree := initProposalGitRepo(t)
	writeRepoFile(t, worktree, "app.yaml", "cpu: 700m\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "initial")
	gitTest(t, worktree, "switch", "-c", "k8s-recommendation-engine/test")
	writeRepoFile(t, worktree, "app.yaml", "cpu: 490m\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "k8s-recommendation-engine: propose test changes")
	if err := os.MkdirAll(filepath.Join(worktree, ".k8s-recommendation-engine", "proposals"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeRepoFile(t, worktree, ".k8s-recommendation-engine/proposals/test.patch", "diff\n")

	status, err := ProposalStatus(context.Background(), worktree, "master")
	if err != nil {
		t.Fatal(err)
	}
	if status.CurrentBranch != "k8s-recommendation-engine/test" {
		t.Fatalf("CurrentBranch = %q", status.CurrentBranch)
	}
	if len(status.ProposalBranches) != 1 || status.ProposalBranches[0] != "k8s-recommendation-engine/test" {
		t.Fatalf("ProposalBranches = %#v", status.ProposalBranches)
	}
	if status.LatestProposalCommit == "" || status.LatestProposalSubject != "k8s-recommendation-engine: propose test changes" {
		t.Fatalf("latest proposal = %q %q", status.LatestProposalCommit, status.LatestProposalSubject)
	}
	if !status.BranchDiffersFromBase {
		t.Fatal("BranchDiffersFromBase = false, want true")
	}
	if len(status.PatchArtifacts) != 1 {
		t.Fatalf("PatchArtifacts = %#v, want one artifact", status.PatchArtifacts)
	}
	if len(status.DirtyLines) != 0 {
		t.Fatalf("DirtyLines = %#v, want clean ignoring proposal artifact", status.DirtyLines)
	}

	diff, err := ProposalDiff(context.Background(), worktree, "master", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "-cpu: 700m") || !strings.Contains(diff, "+cpu: 490m") {
		t.Fatalf("diff missing expected content:\n%s", diff)
	}
}

func TestProposalDiffUsesOnlyProposalBranchFromBaseBranch(t *testing.T) {
	worktree := initProposalGitRepo(t)
	writeRepoFile(t, worktree, "app.yaml", "cpu: 700m\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "initial")
	gitTest(t, worktree, "switch", "-c", "k8s-recommendation-engine/test")
	writeRepoFile(t, worktree, "app.yaml", "cpu: 490m\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "k8s-recommendation-engine: propose test changes")
	gitTest(t, worktree, "switch", "master")

	diff, err := ProposalDiff(context.Background(), worktree, "master", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "+cpu: 490m") {
		t.Fatalf("diff should use only proposal branch from master:\n%s", diff)
	}
}

func TestProposalRevertCreatesRollbackCommit(t *testing.T) {
	worktree := initProposalGitRepo(t)
	writeRepoFile(t, worktree, "app.yaml", "cpu: 700m\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "initial")
	gitTest(t, worktree, "switch", "-c", "k8s-recommendation-engine/test")
	writeRepoFile(t, worktree, "app.yaml", "cpu: 490m\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "k8s-recommendation-engine: propose test changes")

	output, err := ProposalRevert(context.Background(), worktree)
	if err != nil {
		t.Fatalf("ProposalRevert error: %v\n%s", err, output)
	}
	subject := strings.TrimSpace(gitTest(t, worktree, "log", "-1", "--format=%s"))
	if !strings.HasPrefix(subject, "Revert \"k8s-recommendation-engine: propose test changes\"") {
		t.Fatalf("latest subject = %q", subject)
	}
	content, err := os.ReadFile(filepath.Join(worktree, "app.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "cpu: 700m\n" {
		t.Fatalf("content = %q, want reverted", string(content))
	}
}

func TestProposalRevertRefusesUnsafeBranch(t *testing.T) {
	worktree := initProposalGitRepo(t)
	writeRepoFile(t, worktree, "app.yaml", "cpu: 700m\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "k8s-recommendation-engine: propose test changes")

	_, err := ProposalRevert(context.Background(), worktree)
	if err == nil || !strings.Contains(err.Error(), "current branch must start with k8s-recommendation-engine/") {
		t.Fatalf("ProposalRevert err = %v, want unsafe branch refusal", err)
	}
}

func TestProposalRollbackRevertsAndPushesDefaultBranch(t *testing.T) {
	worktree := initProposalGitRepo(t)
	remote := initBareGitRepo(t)
	gitTest(t, worktree, "remote", "add", "origin", remote)
	writeRepoFile(t, worktree, "app.yaml", "cpu: 700m\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "initial")
	gitTest(t, worktree, "push", "-u", "origin", "master")
	writeRepoFile(t, worktree, "app.yaml", "cpu: 490m\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "k8s-recommendation-engine: propose shipyard resource changes")
	gitTest(t, worktree, "push")

	proposal := ProposalRollback(context.Background(), worktree, RollbackOptions{
		Branch:                 "master",
		DefaultBranch:          "master",
		Remote:                 "origin",
		Push:                   true,
		AllowDefaultBranchPush: true,
	})
	if proposal.Blocked || len(proposal.Errors) > 0 {
		t.Fatalf("rollback blocked: %#v", proposal)
	}
	if !proposal.Pushed || proposal.Commit == "" {
		t.Fatalf("rollback push fields = %#v", proposal)
	}
	content, err := os.ReadFile(filepath.Join(worktree, "app.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "cpu: 700m\n" {
		t.Fatalf("rollback content = %q", string(content))
	}
	remoteHead := strings.TrimSpace(gitBareTest(t, remote, "rev-parse", "--short", "master"))
	if remoteHead != proposal.Commit {
		t.Fatalf("remote master = %q, rollback commit = %q", remoteHead, proposal.Commit)
	}
}

func TestProposalRollbackRefusesNonProposalHead(t *testing.T) {
	worktree := initProposalGitRepo(t)
	writeRepoFile(t, worktree, "app.yaml", "cpu: 700m\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "manual change")

	proposal := ProposalRollback(context.Background(), worktree, RollbackOptions{
		Branch:        "master",
		DefaultBranch: "master",
	})
	if !proposal.Blocked {
		t.Fatalf("rollback should block non-proposal head: %#v", proposal)
	}
	if len(proposal.BlockReasons) != 1 || !strings.Contains(proposal.BlockReasons[0], "not an k8s-recommendation-engine proposal") {
		t.Fatalf("BlockReasons = %#v", proposal.BlockReasons)
	}
}

func TestProposalBranchBlockReasonAllowsDefaultBranch(t *testing.T) {
	worktree := initProposalGitRepo(t)
	writeRepoFile(t, worktree, "app.yaml", "cpu: 700m\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "initial")

	reason := proposalBranchBlockReason("master", "feature", true, "master")
	if reason != "" {
		t.Fatalf("reason = %q, want default branch allowed", reason)
	}
}

func TestProposalBranchBlockReasonRejectsExistingNonDefaultBranch(t *testing.T) {
	worktree := initProposalGitRepo(t)
	writeRepoFile(t, worktree, "app.yaml", "cpu: 700m\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "initial")

	reason := proposalBranchBlockReason("feature/existing", "master", true, "master")
	if !strings.Contains(reason, "not allowed") {
		t.Fatalf("reason = %q, want non-default branch block", reason)
	}
}

func TestProposalBranchBlockReasonRejectsNewNonProposalBranch(t *testing.T) {
	worktree := initProposalGitRepo(t)
	writeRepoFile(t, worktree, "app.yaml", "cpu: 700m\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "initial")

	reason := proposalBranchBlockReason("feature/new", "master", false, "master")
	if !strings.Contains(reason, "does not exist") {
		t.Fatalf("reason = %q, want missing branch block", reason)
	}
}

func TestProposalBranchBlockReasonAllowsCurrentProposalBranch(t *testing.T) {
	worktree := initProposalGitRepo(t)
	writeRepoFile(t, worktree, "app.yaml", "cpu: 700m\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "initial")
	gitTest(t, worktree, "switch", "-c", "k8s-recommendation-engine/current")

	reason := proposalBranchBlockReason("k8s-recommendation-engine/current", "k8s-recommendation-engine/current", true, "master")
	if reason != "" {
		t.Fatalf("reason = %q, want allowed current branch", reason)
	}
}

func initProposalGitRepo(t *testing.T) string {
	t.Helper()
	worktree := t.TempDir()
	gitTest(t, worktree, "init")
	gitTest(t, worktree, "config", "user.name", "K8s Recommendation Engine Test")
	gitTest(t, worktree, "config", "user.email", "k8s-recommendation-engine@example.invalid")
	return worktree
}

func initBareGitRepo(t *testing.T) string {
	t.Helper()
	remote := t.TempDir()
	command := exec.Command("git", "init", "--bare", remote)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git init --bare failed: %v\n%s", err, string(output))
	}
	return remote
}

func cloneGitRepo(t *testing.T, remote string) string {
	t.Helper()
	worktree := t.TempDir()
	command := exec.Command("git", "clone", remote, worktree)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git clone failed: %v\n%s", err, string(output))
	}
	return worktree
}

func proposalReplicasReport() *Report {
	return &Report{
		Application: "shipyard",
		GeneratedAt: time.Date(2026, 7, 8, 20, 0, 0, 0, time.UTC),
		Workloads: []WorkloadReport{
			{
				Namespace:  "shipyardhq",
				Deployment: "shipyardhq",
				Recommendation: Recommendation{
					PatchPlan: &PatchPlan{
						SourceFile: "app.yaml",
						Resource:   "Deployment/shipyardhq/shipyardhq",
						Needed:     true,
						Changes: []PatchChange{
							{Field: "spec.replicas", Operation: "add", Current: missingValue, Recommended: "2"},
						},
					},
				},
			},
		},
	}
}

func writeRepoFile(t *testing.T, worktree, name, content string) {
	t.Helper()
	path := filepath.Join(worktree, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitTest(t *testing.T, worktree string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", worktree}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(output))
	}
	return string(output)
}

func gitBareTest(t *testing.T, gitDir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"--git-dir", gitDir}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git --git-dir %v failed: %v\n%s", args, err, string(output))
	}
	return string(output)
}
