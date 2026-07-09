package analyzer

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/abhi1693/k8s-recommendation-engine/internal/config"
	"gopkg.in/yaml.v3"
)

const missingValue = "<missing>"

func AttachPatchPlans(worktree string, profile *config.ApplicationProfile, report *Report) {
	if worktree == "" {
		return
	}

	workloads := make(map[string]config.WorkloadSpec, len(profile.Spec.Workloads))
	for _, workload := range profile.Spec.Workloads {
		workloads[workload.Name] = workload
	}

	for index := range report.Workloads {
		workloadReport := &report.Workloads[index]
		workload, ok := workloads[workloadReport.Name]
		if !ok {
			continue
		}
		workloadReport.Recommendation.PatchPlan = buildPatchPlan(worktree, profile, workload, workloadReport)
	}
}

func buildPatchPlan(worktree string, profile *config.ApplicationProfile, workload config.WorkloadSpec, report *WorkloadReport) *PatchPlan {
	plan := &PatchPlan{
		Resource: fmt.Sprintf("%s/%s/%s", workload.TargetRef.Kind, profile.Spec.Namespace, workload.TargetRef.Name),
		Blocked:  report.Recommendation.Blocked,
	}
	if plan.Blocked {
		plan.BlockReasons = append([]string(nil), report.Recommendation.BlockReasons...)
		return plan
	}
	if profile.Spec.Git.BasePath == "" {
		plan.Errors = append(plan.Errors, "spec.git.basePath is required for patch planning")
		return plan
	}
	if workload.SourceFile == "" {
		plan.Errors = append(plan.Errors, "workload sourceFile is required for patch planning")
		return plan
	}
	basePath, err := cleanGitPath(profile.Spec.Git.BasePath, "spec.git.basePath")
	if err != nil {
		plan.Errors = append(plan.Errors, err.Error())
		return plan
	}
	sourceFile, err := cleanGitPath(workload.SourceFile, "workload sourceFile")
	if err != nil {
		plan.Errors = append(plan.Errors, err.Error())
		return plan
	}

	plan.SourceFile = filepath.ToSlash(filepath.Join(basePath, sourceFile))
	sourcePath := filepath.Join(worktree, basePath, sourceFile)
	sourceData, err := os.ReadFile(sourcePath)
	if err != nil {
		plan.Errors = append(plan.Errors, fmt.Sprintf("read source manifest %s: %v", sourcePath, err))
		return plan
	}
	documents, err := decodeYAMLDocuments(sourceData)
	if err != nil {
		plan.Errors = append(plan.Errors, fmt.Sprintf("parse source manifest %s: %v", sourcePath, err))
		return plan
	}
	modifiedDocuments, err := decodeYAMLDocuments(sourceData)
	if err != nil {
		plan.Errors = append(plan.Errors, fmt.Sprintf("parse source manifest %s for dry-run diff: %v", sourcePath, err))
		return plan
	}
	document := findWorkloadDocument(documents, profile.Spec.Namespace, workload)
	if document == nil {
		plan.Errors = append(plan.Errors, fmt.Sprintf("resource %s/%s not found in %s", workload.TargetRef.Kind, workload.TargetRef.Name, plan.SourceFile))
		return plan
	}
	modifiedDocument := findWorkloadDocument(modifiedDocuments, profile.Spec.Namespace, workload)
	if modifiedDocument == nil {
		plan.Errors = append(plan.Errors, fmt.Sprintf("resource %s/%s not found in dry-run document %s", workload.TargetRef.Kind, workload.TargetRef.Name, plan.SourceFile))
		return plan
	}

	if report.Recommendation.Stability == nil {
		plan.Blocked = true
		plan.BlockReasons = append(plan.BlockReasons, "stability state is unavailable; run with --state-db before planning Git changes")
		return plan
	}
	if report.Rollout.Evaluated && !report.Rollout.Settled {
		plan.Blocked = true
		plan.BlockReasons = append(plan.BlockReasons, "workload rollout is not settled: "+strings.Join(report.Rollout.Reasons, ", "))
		return plan
	}

	if workload.Scaling.Replicas {
		current, ok := scalarAt(document, "spec", "replicas")
		operation := "replace"
		if !ok {
			current = missingValue
			operation = "add"
		}
		if stabilityGateAllowsPatch(report.Recommendation.Stability.Replicas, !ok) {
			recommended := strconv.FormatInt(int64(report.Recommendation.RecommendedReplicas), 10)
			if appendPatchChange(plan, "spec.replicas", operation, current, recommended) {
				setScalarAt(modifiedDocument, recommended, "spec", "replicas")
			}
		} else if report.Recommendation.RecommendedReplicas != report.Recommendation.CurrentReplicas {
			plan.BlockReasons = append(plan.BlockReasons, "replicas blocked by stability gate: "+formatGate(report.Recommendation.Stability.Replicas))
		}
	}

	if len(report.Containers) == 1 {
		containerName := report.Containers[0].Name
		if workload.Scaling.CPU && report.Recommendation.RecommendedCPURequest != "" {
			field := fmt.Sprintf("spec.template.spec.containers[name=%s].resources.requests.cpu", containerName)
			current, ok := containerRequestScalar(document, containerName, "cpu")
			operation := "replace"
			if !ok {
				current = missingValue
				operation = "add"
			}
			if stabilityGateAllowsPatch(report.Recommendation.Stability.CPU, !ok) {
				if appendPatchChange(plan, field, operation, current, report.Recommendation.RecommendedCPURequest) {
					setContainerRequestScalar(modifiedDocument, containerName, "cpu", report.Recommendation.RecommendedCPURequest)
				}
			} else if report.Recommendation.RecommendedCPURequest != report.Recommendation.CurrentCPURequest {
				plan.BlockReasons = append(plan.BlockReasons, "cpu request blocked by stability gate: "+formatGate(report.Recommendation.Stability.CPU))
			}
		}
		if workload.Scaling.Memory && report.Recommendation.RecommendedMemoryRequest != "" {
			field := fmt.Sprintf("spec.template.spec.containers[name=%s].resources.requests.memory", containerName)
			current, ok := containerRequestScalar(document, containerName, "memory")
			operation := "replace"
			if !ok {
				current = missingValue
				operation = "add"
			}
			if stabilityGateAllowsPatch(report.Recommendation.Stability.Memory, !ok) {
				if appendPatchChange(plan, field, operation, current, report.Recommendation.RecommendedMemoryRequest) {
					setContainerRequestScalar(modifiedDocument, containerName, "memory", report.Recommendation.RecommendedMemoryRequest)
				}
			} else if report.Recommendation.RecommendedMemoryRequest != report.Recommendation.CurrentMemoryRequest {
				plan.BlockReasons = append(plan.BlockReasons, "memory request blocked by stability gate: "+formatGate(report.Recommendation.Stability.Memory))
			}
		}
	}

	plan.Needed = len(plan.Changes) > 0
	if !plan.Needed && len(plan.BlockReasons) > 0 {
		plan.Blocked = true
	}
	if plan.Needed {
		originalRendered, err := encodeYAMLDocuments(documents)
		if err != nil {
			plan.Errors = append(plan.Errors, "render original source for diff: "+err.Error())
			return plan
		}
		modifiedRendered, err := encodeYAMLDocuments(modifiedDocuments)
		if err != nil {
			plan.Errors = append(plan.Errors, "render modified source for diff: "+err.Error())
			return plan
		}
		plan.Diff = unifiedDiff(plan.SourceFile, string(originalRendered), string(modifiedRendered))
	}
	return plan
}

func cleanGitPath(pathValue, field string) (string, error) {
	pathValue = filepath.FromSlash(strings.TrimSpace(pathValue))
	if pathValue == "" {
		return "", fmt.Errorf("%s is required for patch planning", field)
	}
	if filepath.IsAbs(pathValue) {
		return "", fmt.Errorf("%s must be a relative Git path", field)
	}
	cleaned := filepath.Clean(pathValue)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s must stay inside the Git worktree", field)
	}
	return cleaned, nil
}

func appendPatchChange(plan *PatchPlan, field, operation, current, recommended string) bool {
	if current == recommended {
		return false
	}
	plan.Changes = append(plan.Changes, PatchChange{
		Field:       field,
		Operation:   operation,
		Current:     current,
		Recommended: recommended,
	})
	return true
}

func readYAMLDocuments(path string) ([]*yaml.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read source manifest %s: %w", path, err)
	}
	documents, err := decodeYAMLDocuments(data)
	if err != nil {
		return nil, fmt.Errorf("parse source manifest %s: %w", path, err)
	}
	return documents, nil
}

func decodeYAMLDocuments(data []byte) ([]*yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var documents []*yaml.Node
	for {
		var document yaml.Node
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		root := documentRoot(&document)
		if root == nil {
			continue
		}
		documents = append(documents, root)
	}
	return documents, nil
}

func encodeYAMLDocuments(documents []*yaml.Node) ([]byte, error) {
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	for _, document := range documents {
		if err := encoder.Encode(document); err != nil {
			encoder.Close()
			return nil, err
		}
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func findWorkloadDocument(documents []*yaml.Node, namespace string, workload config.WorkloadSpec) *yaml.Node {
	for _, document := range documents {
		if scalarValue(mappingValue(document, "kind")) != workload.TargetRef.Kind {
			continue
		}
		metadata := mappingValue(document, "metadata")
		if scalarValue(mappingValue(metadata, "name")) != workload.TargetRef.Name {
			continue
		}
		documentNamespace := scalarValue(mappingValue(metadata, "namespace"))
		if documentNamespace != "" && documentNamespace != namespace {
			continue
		}
		return document
	}
	return nil
}

func containerRequestScalar(document *yaml.Node, containerName, resourceName string) (string, bool) {
	containers := mappingPath(document, "spec", "template", "spec", "containers")
	if containers == nil || containers.Kind != yaml.SequenceNode {
		return "", false
	}
	for _, container := range containers.Content {
		if scalarValue(mappingValue(container, "name")) != containerName {
			continue
		}
		requests := mappingPath(container, "resources", "requests")
		if requests == nil {
			return "", false
		}
		node := mappingValue(requests, resourceName)
		if node == nil {
			return "", false
		}
		return scalarValue(node), true
	}
	return "", false
}

func setContainerRequestScalar(document *yaml.Node, containerName, resourceName, value string) bool {
	containers := mappingPath(document, "spec", "template", "spec", "containers")
	if containers == nil || containers.Kind != yaml.SequenceNode {
		return false
	}
	for _, container := range containers.Content {
		if scalarValue(mappingValue(container, "name")) != containerName {
			continue
		}
		requests := ensureMappingPath(container, "resources", "requests")
		setMappingScalar(requests, resourceName, value)
		return true
	}
	return false
}

func scalarAt(document *yaml.Node, path ...string) (string, bool) {
	node := mappingPath(document, path...)
	if node == nil {
		return "", false
	}
	return scalarValue(node), true
}

func setScalarAt(document *yaml.Node, value string, path ...string) bool {
	if len(path) == 0 {
		return false
	}
	parent := ensureMappingPath(document, path[:len(path)-1]...)
	key := path[len(path)-1]
	if len(path) == 2 && path[0] == "spec" && key == "replicas" {
		setMappingScalarBeforeAny(parent, key, value, "progressDeadlineSeconds", "selector", "strategy", "template")
		return true
	}
	setMappingScalar(parent, key, value)
	return true
}

func mappingPath(node *yaml.Node, path ...string) *yaml.Node {
	current := node
	for _, key := range path {
		current = mappingValue(current, key)
		if current == nil {
			return nil
		}
	}
	return current
}

func ensureMappingPath(node *yaml.Node, path ...string) *yaml.Node {
	current := node
	for _, key := range path {
		next := mappingValue(current, key)
		if next == nil {
			next = &yaml.Node{Kind: yaml.MappingNode}
			current.Content = append(current.Content, scalarNode(key), next)
		}
		if next.Kind != yaml.MappingNode {
			next.Kind = yaml.MappingNode
			next.Tag = "!!map"
			next.Value = ""
			next.Content = nil
		}
		current = next
	}
	return current
}

func setMappingScalar(mapping *yaml.Node, key, value string) {
	if mapping == nil {
		return
	}
	for index := 0; index < len(mapping.Content)-1; index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content[index+1] = scalarNode(value)
			return
		}
	}
	mapping.Content = append(mapping.Content, scalarNode(key), scalarNode(value))
}

func setMappingScalarBeforeAny(mapping *yaml.Node, key, value string, beforeKeys ...string) {
	if mapping == nil {
		return
	}
	for index := 0; index < len(mapping.Content)-1; index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content[index+1] = scalarNode(value)
			return
		}
	}
	insertAt := len(mapping.Content)
	for index := 0; index < len(mapping.Content)-1; index += 2 {
		for _, beforeKey := range beforeKeys {
			if mapping.Content[index].Value == beforeKey {
				insertAt = index
				break
			}
		}
		if insertAt != len(mapping.Content) {
			break
		}
	}
	mapping.Content = append(mapping.Content, nil, nil)
	copy(mapping.Content[insertAt+2:], mapping.Content[insertAt:])
	mapping.Content[insertAt] = scalarNode(key)
	mapping.Content[insertAt+1] = scalarNode(value)
}

func scalarNode(value string) *yaml.Node {
	tag := "!!str"
	if _, err := strconv.ParseInt(value, 10, 64); err == nil {
		tag = "!!int"
	}
	return &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   tag,
		Value: value,
	}
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index < len(node.Content)-1; index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}

func documentRoot(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		return node.Content[0]
	}
	return node
}

func scalarValue(node *yaml.Node) string {
	if node == nil {
		return ""
	}
	return node.Value
}

func stabilityGateStable(gate StabilityGate) bool {
	return gate.Status == "stable"
}

func stabilityGateAllowsPatch(gate StabilityGate, sourceMissing bool) bool {
	return stabilityGateStable(gate) || (sourceMissing && gate.Status == "hold")
}

func unifiedDiff(path, original, modified string) string {
	if original == modified {
		return ""
	}
	originalLines := splitDiffLines(original)
	modifiedLines := splitDiffLines(modified)
	var output strings.Builder
	output.WriteString("--- a/")
	output.WriteString(path)
	output.WriteString("\n")
	output.WriteString("+++ b/")
	output.WriteString(path)
	output.WriteString("\n")
	for _, hunk := range compactLineDiff(originalLines, modifiedLines) {
		output.WriteString("@@ dry-run @@\n")
		for _, line := range hunk {
			output.WriteString(line)
			output.WriteString("\n")
		}
	}
	return output.String()
}

func splitDiffLines(value string) []string {
	trimmed := strings.TrimSuffix(value, "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func compactLineDiff(original, modified []string) [][]string {
	ops := lineDiffOps(original, modified)
	const contextLines = 3
	var changeIndexes []int
	for index, op := range ops {
		if op.kind != ' ' {
			changeIndexes = append(changeIndexes, index)
		}
	}
	if len(changeIndexes) == 0 {
		return nil
	}

	type hunkRange struct {
		start int
		end   int
	}
	var ranges []hunkRange
	current := hunkRange{
		start: maxInt(0, changeIndexes[0]-contextLines),
		end:   minInt(len(ops)-1, changeIndexes[0]+contextLines),
	}
	for _, changeIndex := range changeIndexes[1:] {
		next := hunkRange{
			start: maxInt(0, changeIndex-contextLines),
			end:   minInt(len(ops)-1, changeIndex+contextLines),
		}
		if next.start <= current.end+1 {
			current.end = maxInt(current.end, next.end)
			continue
		}
		ranges = append(ranges, current)
		current = next
	}
	ranges = append(ranges, current)

	hunks := make([][]string, 0, len(ranges))
	for _, diffRange := range ranges {
		var hunk []string
		for index := diffRange.start; index <= diffRange.end; index++ {
			op := ops[index]
			hunk = append(hunk, string(op.kind)+op.line)
		}
		hunks = append(hunks, hunk)
	}
	return hunks
}

type lineDiffOp struct {
	kind byte
	line string
}

func lineDiffOps(original, modified []string) []lineDiffOp {
	lcs := make([][]int, len(original)+1)
	for index := range lcs {
		lcs[index] = make([]int, len(modified)+1)
	}
	for i := len(original) - 1; i >= 0; i-- {
		for j := len(modified) - 1; j >= 0; j-- {
			if original[i] == modified[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = maxInt(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}

	var ops []lineDiffOp
	i := 0
	j := 0
	for i < len(original) && j < len(modified) {
		switch {
		case original[i] == modified[j]:
			ops = append(ops, lineDiffOp{kind: ' ', line: original[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, lineDiffOp{kind: '-', line: original[i]})
			i++
		default:
			ops = append(ops, lineDiffOp{kind: '+', line: modified[j]})
			j++
		}
	}
	for ; i < len(original); i++ {
		ops = append(ops, lineDiffOp{kind: '-', line: original[i]})
	}
	for ; j < len(modified); j++ {
		ops = append(ops, lineDiffOp{kind: '+', line: modified[j]})
	}
	return ops
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
