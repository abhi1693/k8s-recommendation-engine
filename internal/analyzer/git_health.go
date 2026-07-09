package analyzer

import (
	"context"
	"errors"
	"strings"
)

type GitHealthOptions struct {
	Branch      string
	Remote      string
	PushEnabled bool
}

func InspectGitHealth(ctx context.Context, worktree string, options GitHealthOptions) *GitHealthReport {
	report := &GitHealthReport{
		Worktree:    worktree,
		Remote:      strings.TrimSpace(options.Remote),
		PushEnabled: options.PushEnabled,
		Status:      "unknown",
	}
	if worktree == "" {
		report.Errors = append(report.Errors, "--git-worktree is not configured")
		return report
	}
	if report.Remote == "" {
		report.Remote = "origin"
	}

	currentBranch, err := gitCurrentBranch(ctx, worktree)
	if err != nil {
		report.Errors = append(report.Errors, "read current branch: "+err.Error())
		return report
	}
	report.Branch = currentBranch
	report.TargetBranch = strings.TrimSpace(options.Branch)
	if report.TargetBranch == "" {
		report.TargetBranch = currentBranch
	}

	if upstream, err := gitOutput(ctx, worktree, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); err == nil {
		report.Upstream = strings.TrimSpace(upstream)
	}
	if status, err := gitOutput(ctx, worktree, "status", "--porcelain"); err == nil {
		report.DirtyLines = blockingGitStatusLines(status)
		report.Dirty = len(report.DirtyLines) > 0
	} else {
		report.Errors = append(report.Errors, "read git status: "+err.Error())
	}

	if commit, err := gitOutput(ctx, worktree, "rev-parse", "--short", "refs/heads/"+report.TargetBranch); err == nil {
		report.LocalCommit = strings.TrimSpace(commit)
	} else {
		report.Errors = append(report.Errors, "read local commit: "+err.Error())
	}

	refspec := "+refs/heads/" + report.TargetBranch + ":refs/remotes/" + report.Remote + "/" + report.TargetBranch
	if _, err := gitOutput(ctx, worktree, "fetch", "--prune", report.Remote, refspec); err != nil {
		if !remoteBranchMissing(err) {
			report.Errors = append(report.Errors, "fetch remote branch: "+err.Error())
		}
	}
	if commit, err := gitOutput(ctx, worktree, "rev-parse", "--short", "refs/remotes/"+report.Remote+"/"+report.TargetBranch); err == nil {
		report.RemoteCommit = strings.TrimSpace(commit)
	} else if len(report.Errors) == 0 || !remoteBranchMissing(err) {
		report.Errors = append(report.Errors, "read remote commit: "+err.Error())
	}

	if report.LocalCommit != "" && report.RemoteCommit != "" {
		ahead, behind, err := gitAheadBehind(ctx, worktree, report.TargetBranch, report.Remote)
		if err != nil {
			report.Errors = append(report.Errors, "read ahead/behind state: "+err.Error())
		} else {
			report.Ahead = ahead
			report.Behind = behind
			report.Diverged = ahead > 0 && behind > 0
		}
	}

	if commit, subject, err := latestProposalCommit(ctx, worktree, false); err == nil {
		report.LatestProposalCommit = shortCommit(commit)
		report.LatestProposalSubject = subject
	} else if !errors.Is(err, errNoProposalCommit) {
		report.Errors = append(report.Errors, "read latest proposal commit: "+err.Error())
	}

	report.Status = gitHealthStatus(report)
	return report
}

func gitHealthStatus(report *GitHealthReport) string {
	if report == nil {
		return "unknown"
	}
	if len(report.Errors) > 0 && report.LocalCommit == "" {
		return "unknown"
	}
	if report.Dirty {
		return "dirty"
	}
	if report.Diverged {
		return "diverged"
	}
	if report.Behind > 0 {
		return "behind"
	}
	if report.Ahead > 0 {
		return "ahead"
	}
	if len(report.Errors) > 0 {
		return "unknown"
	}
	return "clean"
}

func shortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
