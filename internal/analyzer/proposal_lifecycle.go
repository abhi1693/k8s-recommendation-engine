package analyzer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type RollbackOptions struct {
	Branch                 string
	DefaultBranch          string
	Remote                 string
	Push                   bool
	AllowDefaultBranchPush bool
}

func ProposalStatus(ctx context.Context, worktree, baseBranch string) (*ProposalStatusReport, error) {
	if worktree == "" {
		return nil, errors.New("--git-worktree is required")
	}
	if baseBranch == "" {
		baseBranch = "master"
	}
	report := &ProposalStatusReport{
		Worktree:   worktree,
		BaseBranch: baseBranch,
	}

	branch, err := gitOutput(ctx, worktree, "branch", "--show-current")
	if err != nil {
		return nil, fmt.Errorf("read current branch: %w", err)
	}
	report.CurrentBranch = strings.TrimSpace(branch)
	branches, err := proposalBranches(ctx, worktree)
	if err != nil {
		report.Errors = append(report.Errors, "read proposal branches: "+err.Error())
	} else {
		report.ProposalBranches = branches
	}

	if upstream, err := gitOutput(ctx, worktree, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); err == nil {
		report.Upstream = strings.TrimSpace(upstream)
		report.HasUpstream = report.Upstream != ""
	}

	if status, err := gitOutput(ctx, worktree, "status", "--porcelain"); err == nil {
		report.DirtyLines = blockingGitStatusLines(status)
	} else {
		report.Errors = append(report.Errors, "read git status: "+err.Error())
	}

	commit, subject, err := latestProposalCommit(ctx, worktree, true)
	if err == nil {
		report.LatestProposalCommit = commit
		report.LatestProposalSubject = subject
		files, fileErr := gitOutput(ctx, worktree, "show", "--name-only", "--format=", "--no-renames", commit)
		if fileErr != nil {
			report.Errors = append(report.Errors, "read proposal files: "+fileErr.Error())
		} else {
			report.LatestProposalFiles = nonEmptyLines(files)
		}
	} else if !errors.Is(err, errNoProposalCommit) {
		report.Errors = append(report.Errors, "read latest proposal commit: "+err.Error())
	}

	artifacts, err := proposalPatchArtifacts(worktree)
	if err != nil {
		report.Errors = append(report.Errors, "read proposal artifacts: "+err.Error())
	} else {
		report.PatchArtifacts = artifacts
	}

	differs, err := branchDiffersFromBase(ctx, worktree, baseBranch)
	if err != nil {
		report.Errors = append(report.Errors, "compare branch to base: "+err.Error())
	} else {
		report.BranchDiffersFromBase = differs
	}

	return report, nil
}

func ProposalDiff(ctx context.Context, worktree, baseBranch, branch string) (string, error) {
	if worktree == "" {
		return "", errors.New("--git-worktree is required")
	}
	if baseBranch == "" {
		baseBranch = "master"
	}
	target, err := proposalDiffTarget(ctx, worktree, branch)
	if err != nil {
		return "", err
	}
	diff, err := gitOutput(ctx, worktree, "diff", "--stat", "--patch", baseBranch+"..."+target)
	if err != nil {
		return "", fmt.Errorf("read proposal diff: %w", err)
	}
	if strings.TrimSpace(diff) == "" {
		return fmt.Sprintf("No proposal diff for %s against %s.\n", target, baseBranch), nil
	}
	return diff, nil
}

func ProposalRevert(ctx context.Context, worktree string) (string, error) {
	if worktree == "" {
		return "", errors.New("--git-worktree is required")
	}
	branch, err := gitOutput(ctx, worktree, "branch", "--show-current")
	if err != nil {
		return "", fmt.Errorf("read current branch: %w", err)
	}
	branch = strings.TrimSpace(branch)
	if !strings.HasPrefix(branch, "k8s-recommendation-engine/") {
		return "", fmt.Errorf("refusing to revert on branch %q; current branch must start with k8s-recommendation-engine/", branch)
	}
	subject, err := gitOutput(ctx, worktree, "log", "-1", "--format=%s")
	if err != nil {
		return "", fmt.Errorf("read latest commit subject: %w", err)
	}
	subject = strings.TrimSpace(subject)
	if !strings.HasPrefix(subject, "k8s-recommendation-engine:") {
		return "", fmt.Errorf("refusing to revert latest commit %q; subject must start with k8s-recommendation-engine:", subject)
	}
	status, err := gitOutput(ctx, worktree, "status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("read git status: %w", err)
	}
	if dirty := blockingGitStatusLines(status); len(dirty) > 0 {
		return "", fmt.Errorf("refusing to revert with uncommitted changes: %s", strings.Join(dirty, "; "))
	}
	output, err := gitOutput(ctx, worktree, "revert", "--no-edit", "HEAD")
	if err != nil {
		return output, fmt.Errorf("revert latest proposal commit: %w", err)
	}
	commit, commitErr := gitOutput(ctx, worktree, "rev-parse", "--short", "HEAD")
	if commitErr != nil {
		return output, fmt.Errorf("read revert commit: %w", commitErr)
	}
	return strings.TrimSpace(output) + "\nrevert_commit=" + strings.TrimSpace(commit), nil
}

func ProposalRollback(ctx context.Context, worktree string, options RollbackOptions) *ProposalReport {
	proposal := &ProposalReport{
		Mode: "rollback",
		Kind: "revert",
	}
	if worktree == "" {
		proposal.Blocked = true
		proposal.BlockReasons = append(proposal.BlockReasons, "--git-worktree is required")
		return proposal
	}
	status, err := gitOutput(ctx, worktree, "status", "--porcelain")
	if err != nil {
		proposal.Errors = append(proposal.Errors, "read git status: "+err.Error())
		proposal.Blocked = true
		return proposal
	}
	if dirty := blockingGitStatusLines(status); len(dirty) > 0 {
		proposal.Blocked = true
		proposal.BlockReasons = append(proposal.BlockReasons, "refusing rollback with uncommitted changes: "+strings.Join(dirty, "; "))
		return proposal
	}
	currentBranch, err := gitCurrentBranch(ctx, worktree)
	if err != nil {
		proposal.Errors = append(proposal.Errors, "read current branch: "+err.Error())
		proposal.Blocked = true
		return proposal
	}
	branch := strings.TrimSpace(options.Branch)
	if branch == "" {
		branch = currentBranch
	}
	defaultBranch := strings.TrimSpace(options.DefaultBranch)
	if defaultBranch == "" {
		defaultBranch = gitDefaultBranch(ctx, worktree)
	}
	branchExists := gitBranchExists(ctx, worktree, branch)
	if reason := proposalBranchBlockReason(branch, currentBranch, branchExists, defaultBranch); reason != "" {
		proposal.Blocked = true
		proposal.BlockReasons = append(proposal.BlockReasons, reason)
		return proposal
	}
	if reason := proposalPushBlockReason(branch, defaultBranch, ProposalOptions{
		Remote:                 options.Remote,
		Push:                   options.Push,
		AllowDefaultBranchPush: options.AllowDefaultBranchPush,
	}); reason != "" {
		proposal.Blocked = true
		proposal.BlockReasons = append(proposal.BlockReasons, reason)
		return proposal
	}
	if currentBranch != branch {
		if !branchExists {
			proposal.Blocked = true
			proposal.BlockReasons = append(proposal.BlockReasons, "rollback branch does not exist: "+branch)
			return proposal
		}
		if _, err := gitOutput(ctx, worktree, "switch", branch); err != nil {
			proposal.Blocked = true
			proposal.BlockReasons = append(proposal.BlockReasons, "could not switch to rollback branch "+branch+": "+err.Error())
			return proposal
		}
	}
	subject, err := gitOutput(ctx, worktree, "log", "-1", "--format=%s")
	if err != nil {
		proposal.Errors = append(proposal.Errors, "read latest commit subject: "+err.Error())
		proposal.Blocked = true
		return proposal
	}
	subject = strings.TrimSpace(subject)
	if !strings.HasPrefix(subject, "k8s-recommendation-engine:") {
		proposal.Blocked = true
		proposal.BlockReasons = append(proposal.BlockReasons, fmt.Sprintf("latest commit %q is not an k8s-recommendation-engine proposal", subject))
		return proposal
	}
	if output, err := gitOutput(ctx, worktree, "revert", "--no-edit", "HEAD"); err != nil {
		proposal.Errors = append(proposal.Errors, "rollback latest proposal commit: "+err.Error())
		if strings.TrimSpace(output) != "" {
			proposal.Errors = append(proposal.Errors, strings.TrimSpace(output))
		}
		proposal.Blocked = true
		return proposal
	}
	commit, err := gitOutput(ctx, worktree, "rev-parse", "--short", "HEAD")
	if err != nil {
		proposal.Errors = append(proposal.Errors, "read rollback commit: "+err.Error())
		proposal.Blocked = true
		return proposal
	}
	proposal.Needed = true
	proposal.Branch = branch
	proposal.Commit = strings.TrimSpace(commit)
	if options.Push {
		pushProposalCommit(ctx, worktree, proposal, ProposalOptions{
			Remote: options.Remote,
			Push:   true,
		})
		if proposal.Pushed {
			proposal.Message = "rollback commit pushed; Fleet can reconcile after Git updates"
		}
		return proposal
	}
	proposal.Message = "rollback commit created locally; nothing was pushed"
	return proposal
}

func WriteProposalStatus(w io.Writer, report *ProposalStatusReport) error {
	if _, err := fmt.Fprintln(w, "K8s Recommendation Engine Proposal Status"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Worktree: %s\n", report.Worktree); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Branch:   %s\n", emptyDash(report.CurrentBranch)); err != nil {
		return err
	}
	if len(report.ProposalBranches) > 0 {
		if _, err := fmt.Fprintln(w, "Proposal branches:"); err != nil {
			return err
		}
		for _, branch := range report.ProposalBranches {
			if _, err := fmt.Fprintf(w, "  - %s\n", branch); err != nil {
				return err
			}
		}
	}
	if _, err := fmt.Fprintf(w, "Base:     %s differs=%t\n", report.BaseBranch, report.BranchDiffersFromBase); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Upstream: %s pushed=%t\n", emptyDash(report.Upstream), report.HasUpstream); err != nil {
		return err
	}
	if report.LatestProposalCommit != "" {
		if _, err := fmt.Fprintf(w, "Latest:   %s %s\n", report.LatestProposalCommit, report.LatestProposalSubject); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintln(w, "Latest:   -"); err != nil {
		return err
	}
	if len(report.LatestProposalFiles) > 0 {
		if _, err := fmt.Fprintln(w, "Files:"); err != nil {
			return err
		}
		for _, file := range report.LatestProposalFiles {
			if _, err := fmt.Fprintf(w, "  - %s\n", file); err != nil {
				return err
			}
		}
	}
	if len(report.PatchArtifacts) > 0 {
		if _, err := fmt.Fprintf(w, "Patch artifacts: %d\n", len(report.PatchArtifacts)); err != nil {
			return err
		}
		for _, artifact := range report.PatchArtifacts {
			if _, err := fmt.Fprintf(w, "  - %s\n", artifact); err != nil {
				return err
			}
		}
	} else if _, err := fmt.Fprintln(w, "Patch artifacts: 0"); err != nil {
		return err
	}
	if len(report.DirtyLines) > 0 {
		if _, err := fmt.Fprintln(w, "Dirty:"); err != nil {
			return err
		}
		for _, line := range report.DirtyLines {
			if _, err := fmt.Fprintf(w, "  - %s\n", line); err != nil {
				return err
			}
		}
	} else if _, err := fmt.Fprintln(w, "Dirty: clean"); err != nil {
		return err
	}
	for _, reportError := range report.Errors {
		if _, err := fmt.Fprintf(w, "Error: %s\n", reportError); err != nil {
			return err
		}
	}
	return nil
}

var errNoProposalCommit = errors.New("no proposal commit")

func latestProposalCommit(ctx context.Context, worktree string, allBranches bool) (commit, subject string, err error) {
	args := []string{"log", "--grep=^k8s-recommendation-engine:", "-n", "1", "--format=%H%x00%s"}
	if allBranches {
		args = append([]string{"log", "--all"}, args[1:]...)
	}
	output, err := gitOutput(ctx, worktree, args...)
	if err != nil {
		return "", "", err
	}
	output = strings.TrimSpace(output)
	if output == "" {
		return "", "", errNoProposalCommit
	}
	parts := strings.SplitN(output, "\x00", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("unexpected proposal commit output %q", output)
	}
	return parts[0], parts[1], nil
}

func proposalBranches(ctx context.Context, worktree string) ([]string, error) {
	output, err := gitOutput(ctx, worktree, "branch", "--list", "k8s-recommendation-engine/*", "--format=%(refname:short)")
	if err != nil {
		return nil, err
	}
	return nonEmptyLines(output), nil
}

func proposalDiffTarget(ctx context.Context, worktree, branch string) (string, error) {
	if branch != "" {
		return branch, nil
	}
	current, err := gitOutput(ctx, worktree, "branch", "--show-current")
	if err != nil {
		return "", fmt.Errorf("read current branch: %w", err)
	}
	current = strings.TrimSpace(current)
	if strings.HasPrefix(current, "k8s-recommendation-engine/") {
		return "HEAD", nil
	}
	branches, err := proposalBranches(ctx, worktree)
	if err != nil {
		return "", fmt.Errorf("read proposal branches: %w", err)
	}
	switch len(branches) {
	case 0:
		return "HEAD", nil
	case 1:
		return branches[0], nil
	default:
		return "", fmt.Errorf("multiple proposal branches exist; pass --branch: %s", strings.Join(branches, ", "))
	}
}

func proposalPatchArtifacts(worktree string) ([]string, error) {
	root := filepath.Join(worktree, ".k8s-recommendation-engine", "proposals")
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var artifacts []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".patch") {
			continue
		}
		artifacts = append(artifacts, filepath.ToSlash(filepath.Join(".k8s-recommendation-engine", "proposals", entry.Name())))
	}
	return artifacts, nil
}

func branchDiffersFromBase(ctx context.Context, worktree, baseBranch string) (bool, error) {
	command := exec.CommandContext(ctx, "git", "-C", worktree, "diff", "--quiet", baseBranch+"...HEAD")
	if err := command.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

func nonEmptyLines(value string) []string {
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
