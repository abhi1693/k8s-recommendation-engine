package analyzer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type ProposalOptions struct {
	Kind                   string
	PatchDir               string
	BranchName             string
	DefaultBranch          string
	Remote                 string
	Push                   bool
	AllowDefaultBranchPush bool
	GeneratedAt            time.Time
}

func CreateProposal(ctx context.Context, worktree string, report *Report, options ProposalOptions) *ProposalReport {
	proposal := BuildProposal(worktree, report)
	proposal.Mode = "propose"
	if options.Kind == "" {
		options.Kind = "patch"
	}
	proposal.Kind = options.Kind
	if proposal.Blocked {
		return proposal
	}
	if !proposal.Needed {
		if options.Kind == "commit" && options.Push {
			publishExistingProposalCommit(ctx, worktree, proposal, options)
		}
		return proposal
	}

	switch options.Kind {
	case "patch":
		writeProposalPatch(worktree, report, proposal, options)
	case "commit":
		writeProposalCommit(ctx, worktree, report, proposal, options)
	default:
		proposal.Blocked = true
		proposal.Errors = append(proposal.Errors, fmt.Sprintf("unsupported proposal kind %q", options.Kind))
	}
	return proposal
}

func BuildProposal(worktree string, report *Report) *ProposalReport {
	proposal := &ProposalReport{
		Mode: "dry-run",
		Kind: "patch",
	}
	if worktree == "" {
		proposal.Blocked = true
		proposal.BlockReasons = append(proposal.BlockReasons, "--git-worktree is required for proposal generation")
		return proposal
	}

	plansByFile := map[string][]*PatchPlan{}
	for index := range report.Workloads {
		plan := report.Workloads[index].Recommendation.PatchPlan
		if plan == nil {
			continue
		}
		if plan.Blocked {
			proposal.BlockReasons = append(proposal.BlockReasons, scopedPlanReasons(report.Workloads[index], plan.BlockReasons)...)
		}
		if len(plan.Errors) > 0 {
			proposal.Errors = append(proposal.Errors, scopedPlanReasons(report.Workloads[index], plan.Errors)...)
		}
		if patchPlanApplyable(plan) {
			plansByFile[plan.SourceFile] = append(plansByFile[plan.SourceFile], plan)
		}
	}
	if len(plansByFile) == 0 {
		proposal.Blocked = len(proposal.Errors) > 0 || len(proposal.BlockReasons) > 0
		return proposal
	}

	for sourceFile, plans := range plansByFile {
		fileProposal, err := buildProposalFile(worktree, sourceFile, plans)
		if err != nil {
			proposal.Errors = append(proposal.Errors, err.Error())
			continue
		}
		if fileProposal.Diff == "" {
			continue
		}
		proposal.Files = append(proposal.Files, fileProposal)
	}
	proposal.Needed = len(proposal.Files) > 0
	proposal.Blocked = len(proposal.Errors) > 0
	return proposal
}

func buildProposalFile(worktree, sourceFile string, plans []*PatchPlan) (ProposalFile, error) {
	cleaned, err := cleanGitPath(sourceFile, "patch plan sourceFile")
	if err != nil {
		return ProposalFile{}, err
	}
	path := filepath.Join(worktree, cleaned)
	data, err := os.ReadFile(path)
	if err != nil {
		return ProposalFile{}, fmt.Errorf("read proposal source %s: %w", sourceFile, err)
	}
	documents, err := decodeYAMLDocuments(data)
	if err != nil {
		return ProposalFile{}, fmt.Errorf("parse proposal source %s: %w", sourceFile, err)
	}

	file := ProposalFile{SourceFile: filepath.ToSlash(cleaned)}
	for _, plan := range plans {
		kind, namespace, name := splitPlanResource(plan.Resource)
		document := findDocumentByIdentity(documents, kind, namespace, name)
		if document == nil {
			return ProposalFile{}, fmt.Errorf("resource %s not found while building proposal for %s", plan.Resource, sourceFile)
		}
		for _, change := range plan.Changes {
			if applyPatchChange(document, change) {
				file.Changes = append(file.Changes, change)
			}
		}
	}
	if len(file.Changes) == 0 {
		return file, nil
	}
	rendered, err := encodeYAMLDocuments(documents)
	if err != nil {
		return ProposalFile{}, fmt.Errorf("render proposal source %s: %w", sourceFile, err)
	}
	file.ProposedContent = string(rendered)
	file.Diff = unifiedDiff(file.SourceFile, string(data), file.ProposedContent)
	return file, nil
}

func applyPatchChange(document *yaml.Node, change PatchChange) bool {
	if change.Current == change.Recommended {
		return false
	}
	if change.Field == "spec.replicas" {
		return setScalarAt(document, change.Recommended, "spec", "replicas")
	}
	const prefix = "spec.template.spec.containers[name="
	const suffixCPU = "].resources.requests.cpu"
	const suffixMemory = "].resources.requests.memory"
	switch {
	case strings.HasPrefix(change.Field, prefix) && strings.HasSuffix(change.Field, suffixCPU):
		container := strings.TrimSuffix(strings.TrimPrefix(change.Field, prefix), suffixCPU)
		return setContainerRequestScalar(document, container, "cpu", change.Recommended)
	case strings.HasPrefix(change.Field, prefix) && strings.HasSuffix(change.Field, suffixMemory):
		container := strings.TrimSuffix(strings.TrimPrefix(change.Field, prefix), suffixMemory)
		return setContainerRequestScalar(document, container, "memory", change.Recommended)
	default:
		return false
	}
}

func writeProposalPatch(worktree string, report *Report, proposal *ProposalReport, options ProposalOptions) {
	patch := proposalPatchContent(proposal)
	if strings.TrimSpace(patch) == "" {
		proposal.Needed = false
		return
	}
	patchDir := options.PatchDir
	if patchDir == "" {
		patchDir = ".k8s-recommendation-engine/proposals"
	}
	cleanDir, err := cleanGitPath(patchDir, "proposal patch dir")
	if err != nil {
		proposal.Errors = append(proposal.Errors, err.Error())
		proposal.Blocked = true
		return
	}
	if err := os.MkdirAll(filepath.Join(worktree, cleanDir), 0o755); err != nil {
		proposal.Errors = append(proposal.Errors, fmt.Sprintf("create proposal patch dir: %v", err))
		proposal.Blocked = true
		return
	}
	timestamp := options.GeneratedAt
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}
	filename := fmt.Sprintf("%s-%s.patch", safeName(report.Application), timestamp.Format("20060102T150405Z"))
	patchPath := filepath.Join(cleanDir, filename)
	fullPath := filepath.Join(worktree, patchPath)
	if err := os.WriteFile(fullPath, []byte(patch), 0o644); err != nil {
		proposal.Errors = append(proposal.Errors, fmt.Sprintf("write proposal patch: %v", err))
		proposal.Blocked = true
		return
	}
	proposal.PatchFile = filepath.ToSlash(patchPath)
	proposal.Message = "proposal patch file written; no manifests were changed"
}

func writeProposalCommit(ctx context.Context, worktree string, report *Report, proposal *ProposalReport, options ProposalOptions) {
	if status, err := gitOutput(ctx, worktree, "status", "--porcelain"); err != nil {
		proposal.Errors = append(proposal.Errors, "read git status: "+err.Error())
		proposal.Blocked = true
		return
	} else if dirty := blockingGitStatusLines(status); len(dirty) > 0 {
		proposal.Blocked = true
		proposal.BlockReasons = append(proposal.BlockReasons, "git worktree is not clean; refusing to create proposal commit: "+strings.Join(dirty, "; "))
		return
	}
	branch := options.BranchName
	if branch == "" {
		timestamp := options.GeneratedAt
		if timestamp.IsZero() {
			timestamp = time.Now().UTC()
		}
		branch = fmt.Sprintf("k8s-recommendation-engine/%s-%s", safeName(report.Application), timestamp.Format("20060102T150405Z"))
	}
	currentBranch, err := gitCurrentBranch(ctx, worktree)
	if err != nil {
		proposal.Errors = append(proposal.Errors, "read current branch: "+err.Error())
		proposal.Blocked = true
		return
	}
	branchExists := gitBranchExists(ctx, worktree, branch)
	defaultBranch := strings.TrimSpace(options.DefaultBranch)
	if defaultBranch == "" {
		defaultBranch = gitDefaultBranch(ctx, worktree)
	}
	if reason := proposalBranchBlockReason(branch, currentBranch, branchExists, defaultBranch); reason != "" {
		proposal.Blocked = true
		proposal.BlockReasons = append(proposal.BlockReasons, reason)
		return
	}
	if reason := proposalPushBlockReason(branch, defaultBranch, options); reason != "" {
		proposal.Blocked = true
		proposal.BlockReasons = append(proposal.BlockReasons, reason)
		return
	}
	if branchExists && currentBranch != branch {
		if _, err := gitOutput(ctx, worktree, "switch", branch); err != nil {
			proposal.Blocked = true
			proposal.BlockReasons = append(proposal.BlockReasons, "could not switch to proposal branch "+branch+": "+err.Error())
			return
		}
		currentBranch = branch
	}
	syncBranch := branch
	if !branchExists {
		syncBranch = currentBranch
	}
	if !ensureProposalRemoteFresh(ctx, worktree, proposal, options, syncBranch, false) {
		return
	}
	if !refreshProposalAfterGitSync(worktree, report, proposal) {
		return
	}
	if currentBranch != branch {
		if _, err := gitOutput(ctx, worktree, "switch", "-c", branch); err != nil {
			proposal.Blocked = true
			proposal.BlockReasons = append(proposal.BlockReasons, "could not create proposal branch "+branch+": "+err.Error())
			return
		}
	}
	proposal.Branch = branch

	for _, file := range proposal.Files {
		path := filepath.Join(worktree, filepath.FromSlash(file.SourceFile))
		if err := os.WriteFile(path, []byte(file.ProposedContent), 0o644); err != nil {
			proposal.Errors = append(proposal.Errors, fmt.Sprintf("write proposed manifest %s: %v", file.SourceFile, err))
			proposal.Blocked = true
			return
		}
		if _, err := gitOutput(ctx, worktree, "add", "--", file.SourceFile); err != nil {
			proposal.Errors = append(proposal.Errors, "stage proposed manifest: "+err.Error())
			proposal.Blocked = true
			return
		}
	}
	message := proposalCommitMessage(report, proposal)
	if _, err := gitOutput(ctx, worktree, "commit", "-m", message); err != nil {
		proposal.Errors = append(proposal.Errors, "create proposal commit: "+err.Error())
		proposal.Blocked = true
		return
	}
	commit, err := gitOutput(ctx, worktree, "rev-parse", "--short", "HEAD")
	if err != nil {
		proposal.Errors = append(proposal.Errors, "read proposal commit: "+err.Error())
		proposal.Blocked = true
		return
	}
	proposal.Commit = strings.TrimSpace(commit)
	if options.Push {
		pushProposalCommit(ctx, worktree, proposal, options)
		return
	}
	proposal.Message = "proposal branch and local commit created; nothing was pushed"
}

func refreshProposalAfterGitSync(worktree string, report *Report, proposal *ProposalReport) bool {
	refreshed := BuildProposal(worktree, report)
	if refreshed.Blocked {
		proposal.Blocked = true
		proposal.BlockReasons = append(proposal.BlockReasons, refreshed.BlockReasons...)
		proposal.Errors = append(proposal.Errors, refreshed.Errors...)
		return false
	}
	proposal.Needed = refreshed.Needed
	proposal.Files = refreshed.Files
	if !proposal.Needed {
		proposal.Message = "proposal no longer needed after git sync"
		return false
	}
	return true
}

func publishExistingProposalCommit(ctx context.Context, worktree string, proposal *ProposalReport, options ProposalOptions) {
	if status, err := gitOutput(ctx, worktree, "status", "--porcelain"); err != nil {
		proposal.Errors = append(proposal.Errors, "read git status: "+err.Error())
		proposal.Blocked = true
		return
	} else if dirty := blockingGitStatusLines(status); len(dirty) > 0 {
		proposal.Blocked = true
		proposal.BlockReasons = append(proposal.BlockReasons, "git worktree is not clean; refusing to push proposal commit: "+strings.Join(dirty, "; "))
		return
	}
	currentBranch, err := gitCurrentBranch(ctx, worktree)
	if err != nil {
		proposal.Errors = append(proposal.Errors, "read current branch: "+err.Error())
		proposal.Blocked = true
		return
	}
	branch := strings.TrimSpace(options.BranchName)
	if branch == "" {
		branch = currentBranch
	}
	branchExists := gitBranchExists(ctx, worktree, branch)
	defaultBranch := strings.TrimSpace(options.DefaultBranch)
	if defaultBranch == "" {
		defaultBranch = gitDefaultBranch(ctx, worktree)
	}
	if reason := proposalBranchBlockReason(branch, currentBranch, branchExists, defaultBranch); reason != "" {
		proposal.Blocked = true
		proposal.BlockReasons = append(proposal.BlockReasons, reason)
		return
	}
	if reason := proposalPushBlockReason(branch, defaultBranch, options); reason != "" {
		proposal.Blocked = true
		proposal.BlockReasons = append(proposal.BlockReasons, reason)
		return
	}
	if !ensureProposalRemoteFresh(ctx, worktree, proposal, options, branch, true) {
		return
	}
	if currentBranch != branch {
		if !branchExists {
			proposal.Blocked = true
			proposal.BlockReasons = append(proposal.BlockReasons, fmt.Sprintf("no proposal changes and branch %q does not exist; nothing to push", branch))
			return
		}
		if _, err := gitOutput(ctx, worktree, "switch", branch); err != nil {
			proposal.Blocked = true
			proposal.BlockReasons = append(proposal.BlockReasons, "could not switch to proposal branch "+branch+": "+err.Error())
			return
		}
	}
	subject, err := gitOutput(ctx, worktree, "log", "-1", "--format=%s")
	if err != nil {
		proposal.Errors = append(proposal.Errors, "read latest commit subject: "+err.Error())
		proposal.Blocked = true
		return
	}
	if !strings.HasPrefix(strings.TrimSpace(subject), "k8s-recommendation-engine:") {
		proposal.Blocked = true
		proposal.BlockReasons = append(proposal.BlockReasons, "latest commit is not an k8s-recommendation-engine proposal; refusing to push existing branch")
		return
	}
	commit, err := gitOutput(ctx, worktree, "rev-parse", "--short", "HEAD")
	if err != nil {
		proposal.Errors = append(proposal.Errors, "read proposal commit: "+err.Error())
		proposal.Blocked = true
		return
	}
	proposal.Branch = branch
	proposal.Commit = strings.TrimSpace(commit)
	pushProposalCommit(ctx, worktree, proposal, options)
}

func proposalPatchContent(proposal *ProposalReport) string {
	var output strings.Builder
	for _, file := range proposal.Files {
		if file.Diff == "" {
			continue
		}
		output.WriteString(file.Diff)
		if !strings.HasSuffix(file.Diff, "\n") {
			output.WriteString("\n")
		}
	}
	return output.String()
}

func proposalCommitMessage(report *Report, proposal *ProposalReport) string {
	return fmt.Sprintf("k8s-recommendation-engine: propose %s resource changes\n\nApplication: %s\nFiles: %d\nGenerated: %s",
		report.Application,
		report.Application,
		len(proposal.Files),
		report.GeneratedAt.Format(time.RFC3339),
	)
}

func gitOutput(ctx context.Context, worktree string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", worktree}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func gitCurrentBranch(ctx context.Context, worktree string) (string, error) {
	branch, err := gitOutput(ctx, worktree, "branch", "--show-current")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(branch), nil
}

func gitDefaultBranch(ctx context.Context, worktree string) string {
	if output, err := gitOutput(ctx, worktree, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		branch := strings.TrimSpace(output)
		if strings.HasPrefix(branch, "origin/") {
			return strings.TrimPrefix(branch, "origin/")
		}
		if branch != "" {
			return branch
		}
	}
	if gitBranchExists(ctx, worktree, "master") {
		return "master"
	}
	if gitBranchExists(ctx, worktree, "main") {
		return "main"
	}
	current, err := gitCurrentBranch(ctx, worktree)
	if err == nil && current != "" {
		return current
	}
	return "master"
}

func proposalBranchBlockReason(branch, currentBranch string, branchExists bool, defaultBranch string) string {
	if currentBranch == branch {
		return ""
	}
	if branch == defaultBranch {
		return ""
	}
	if strings.HasPrefix(branch, "k8s-recommendation-engine/") {
		return ""
	}
	if branchExists {
		return fmt.Sprintf("proposal branch %q is not allowed; choose default branch %q or an k8s-recommendation-engine/* branch", branch, defaultBranch)
	}
	return fmt.Sprintf("new proposal branch %q does not exist; create it first as the default branch or use an k8s-recommendation-engine/* branch", branch)
}

func proposalPushBlockReason(branch, defaultBranch string, options ProposalOptions) string {
	if !options.Push {
		return ""
	}
	if branch == defaultBranch && !options.AllowDefaultBranchPush {
		return fmt.Sprintf("pushing configured default branch %q requires --allow-default-branch-push", defaultBranch)
	}
	return ""
}

func ensureProposalRemoteFresh(ctx context.Context, worktree string, proposal *ProposalReport, options ProposalOptions, branch string, allowAhead bool) bool {
	if !options.Push {
		return true
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return true
	}
	remote := strings.TrimSpace(options.Remote)
	if remote == "" {
		remote = "origin"
	}
	refspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", branch, remote, branch)
	if _, err := gitOutput(ctx, worktree, "fetch", "--prune", remote, refspec); err != nil {
		if remoteBranchMissing(err) {
			return true
		}
		proposal.Blocked = true
		proposal.BlockReasons = append(proposal.BlockReasons, fmt.Sprintf("could not verify %s/%s freshness before proposal commit: %v", remote, branch, err))
		return false
	}
	ahead, behind, err := gitAheadBehind(ctx, worktree, branch, remote)
	if err != nil {
		proposal.Errors = append(proposal.Errors, "read git ahead/behind state: "+err.Error())
		proposal.Blocked = true
		return false
	}
	remoteRef := remote + "/" + branch
	switch {
	case ahead > 0 && behind > 0:
		proposal.Blocked = true
		proposal.BlockReasons = append(proposal.BlockReasons, fmt.Sprintf("git branch %q has diverged from %s after fetch (ahead %d, behind %d); refusing to create another proposal commit on a stale base", branch, remoteRef, ahead, behind))
		return false
	case behind > 0:
		if _, err := gitOutput(ctx, worktree, "pull", "--rebase", remote, branch); err != nil {
			proposal.Blocked = true
			proposal.BlockReasons = append(proposal.BlockReasons, fmt.Sprintf("git branch %q is behind %s by %d commit(s), and pull --rebase failed: %v", branch, remoteRef, behind, err))
			return false
		}
		ahead, behind, err = gitAheadBehind(ctx, worktree, branch, remote)
		if err != nil {
			proposal.Errors = append(proposal.Errors, "read git ahead/behind state after pull --rebase: "+err.Error())
			proposal.Blocked = true
			return false
		}
		if behind > 0 || ahead > 0 && !allowAhead {
			proposal.Blocked = true
			proposal.BlockReasons = append(proposal.BlockReasons, fmt.Sprintf("git branch %q remains out of sync with %s after pull --rebase (ahead %d, behind %d)", branch, remoteRef, ahead, behind))
			return false
		}
		return true
	case ahead > 0 && !allowAhead:
		proposal.Blocked = true
		proposal.BlockReasons = append(proposal.BlockReasons, fmt.Sprintf("git branch %q already has %d unpushed commit(s) relative to %s; refusing to create another proposal commit", branch, ahead, remoteRef))
		return false
	default:
		return true
	}
}

func remoteBranchMissing(err error) bool {
	message := err.Error()
	return strings.Contains(message, "couldn't find remote ref") ||
		strings.Contains(message, "could not find remote ref")
}

func gitAheadBehind(ctx context.Context, worktree, branch, remote string) (int, int, error) {
	localRef := "refs/heads/" + branch
	remoteRef := "refs/remotes/" + remote + "/" + branch
	output, err := gitOutput(ctx, worktree, "rev-list", "--left-right", "--count", localRef+"..."+remoteRef)
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(output)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("unexpected rev-list output %q", strings.TrimSpace(output))
	}
	ahead, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, fmt.Errorf("parse ahead count %q: %w", fields[0], err)
	}
	behind, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, fmt.Errorf("parse behind count %q: %w", fields[1], err)
	}
	return ahead, behind, nil
}

func pushProposalCommit(ctx context.Context, worktree string, proposal *ProposalReport, options ProposalOptions) {
	remote := strings.TrimSpace(options.Remote)
	branch := proposal.Branch
	if remote == "" {
		remote = "origin"
	}
	proposal.Remote = remote
	if _, err := gitOutput(ctx, worktree, "push", "-u", remote, branch); err != nil {
		proposal.Blocked = true
		proposal.BlockReasons = append(proposal.BlockReasons, fmt.Sprintf("push to %s/%s failed; proposal commit exists only in the local worktree", remote, branch))
		proposal.Errors = append(proposal.Errors, "push proposal commit: "+err.Error())
		proposal.Message = "proposal commit created locally, but push failed"
		return
	}
	proposal.Pushed = true
	proposal.PushRef = remote + "/" + branch
	proposal.Message = "proposal commit pushed; Fleet can reconcile after Git updates"
}

func gitBranchExists(ctx context.Context, worktree, branch string) bool {
	_, err := gitOutput(ctx, worktree, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

func blockingGitStatusLines(status string) []string {
	var dirty []string
	for _, line := range strings.Split(status, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "?? .k8s-recommendation-engine/") || line == "?? .k8s-recommendation-engine" {
			continue
		}
		dirty = append(dirty, line)
	}
	return dirty
}

func splitPlanResource(resource string) (kind, namespace, name string) {
	parts := strings.Split(resource, "/")
	if len(parts) != 3 {
		return "", "", ""
	}
	return parts[0], parts[1], parts[2]
}

func findDocumentByIdentity(documents []*yaml.Node, kind, namespace, name string) *yaml.Node {
	for _, document := range documents {
		if scalarValue(mappingValue(document, "kind")) != kind {
			continue
		}
		metadata := mappingValue(document, "metadata")
		if scalarValue(mappingValue(metadata, "name")) != name {
			continue
		}
		documentNamespace := scalarValue(mappingValue(metadata, "namespace"))
		if documentNamespace != "" && namespace != "" && documentNamespace != namespace {
			continue
		}
		return document
	}
	return nil
}

func scopedPlanReasons(workload WorkloadReport, reasons []string) []string {
	scoped := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		scoped = append(scoped, workload.Namespace+"/"+workload.Deployment+": "+reason)
	}
	return scoped
}

func safeName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var output strings.Builder
	lastDash := false
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			output.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			output.WriteByte('-')
			lastDash = true
		}
	}
	result := strings.Trim(output.String(), "-")
	if result == "" {
		return "proposal"
	}
	return result
}
