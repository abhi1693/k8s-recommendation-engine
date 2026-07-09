package analyzer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestInspectGitHealthReportsCleanBranch(t *testing.T) {
	worktree := initProposalGitRepo(t)
	remote := initBareGitRepo(t)
	gitTest(t, worktree, "remote", "add", "origin", remote)
	writeRepoFile(t, worktree, "app.yaml", "cpu: 700m\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "initial")
	gitTest(t, worktree, "push", "-u", "origin", "master")

	health := InspectGitHealth(context.Background(), worktree, GitHealthOptions{
		Branch:      "master",
		Remote:      "origin",
		PushEnabled: true,
	})
	if health.Status != "clean" {
		t.Fatalf("Status = %q, want clean: %#v", health.Status, health)
	}
	if health.Branch != "master" || health.TargetBranch != "master" || health.Upstream != "origin/master" {
		t.Fatalf("branch fields = %#v", health)
	}
	if health.LocalCommit == "" || health.RemoteCommit == "" || health.LocalCommit != health.RemoteCommit {
		t.Fatalf("commit fields = local:%q remote:%q", health.LocalCommit, health.RemoteCommit)
	}
	if health.Ahead != 0 || health.Behind != 0 || health.Diverged || health.Dirty {
		t.Fatalf("ahead/behind/dirty = %#v", health)
	}
}

func TestInspectGitHealthReportsAheadAndLatestProposal(t *testing.T) {
	worktree := initProposalGitRepo(t)
	remote := initBareGitRepo(t)
	gitTest(t, worktree, "remote", "add", "origin", remote)
	writeRepoFile(t, worktree, "app.yaml", "cpu: 700m\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "initial")
	gitTest(t, worktree, "push", "-u", "origin", "master")
	writeRepoFile(t, worktree, "app.yaml", "cpu: 490m\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "k8s-recommendation-engine: propose resource changes")

	health := InspectGitHealth(context.Background(), worktree, GitHealthOptions{
		Branch:      "master",
		Remote:      "origin",
		PushEnabled: true,
	})
	if health.Status != "ahead" {
		t.Fatalf("Status = %q, want ahead: %#v", health.Status, health)
	}
	if health.Ahead != 1 || health.Behind != 0 {
		t.Fatalf("ahead/behind = %d/%d, want 1/0", health.Ahead, health.Behind)
	}
	if health.LatestProposalCommit == "" || health.LatestProposalSubject != "k8s-recommendation-engine: propose resource changes" {
		t.Fatalf("latest proposal = %q %q", health.LatestProposalCommit, health.LatestProposalSubject)
	}
}

func TestInspectGitHealthReportsBehindAndDirty(t *testing.T) {
	worktree := initProposalGitRepo(t)
	remote := initBareGitRepo(t)
	gitTest(t, worktree, "remote", "add", "origin", remote)
	writeRepoFile(t, worktree, "app.yaml", "cpu: 700m\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "initial")
	gitTest(t, worktree, "push", "-u", "origin", "master")

	other := cloneGitRepo(t, remote)
	gitTest(t, other, "config", "user.name", "K8s Recommendation Engine Test")
	gitTest(t, other, "config", "user.email", "k8s-recommendation-engine@example.invalid")
	writeRepoFile(t, other, "remote.yaml", "advanced: true\n")
	gitTest(t, other, "add", ".")
	gitTest(t, other, "commit", "-m", "remote update")
	gitTest(t, other, "push")
	if err := os.WriteFile(filepath.Join(worktree, "manual.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	health := InspectGitHealth(context.Background(), worktree, GitHealthOptions{
		Branch:      "master",
		Remote:      "origin",
		PushEnabled: true,
	})
	if health.Status != "dirty" {
		t.Fatalf("Status = %q, want dirty: %#v", health.Status, health)
	}
	if health.Behind != 1 || health.Ahead != 0 {
		t.Fatalf("ahead/behind = %d/%d, want 0/1", health.Ahead, health.Behind)
	}
	if !health.Dirty || len(health.DirtyLines) != 1 {
		t.Fatalf("dirty fields = %#v", health)
	}
}
