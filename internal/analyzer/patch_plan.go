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
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	missingValue                  = "<missing>"
	patchSourceKubernetesManifest = "kubernetesManifest"
	patchSourceHelmValues         = "helmValues"
)

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
		SourceFormat: patchSourceFormat(workload),
		Resource:     fmt.Sprintf("%s/%s/%s", workload.TargetRef.Kind, profile.Spec.Namespace, workload.TargetRef.Name),
		Blocked:      report.Recommendation.Blocked,
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
	if plan.Blocked {
		plan.BlockReasons = append([]string(nil), report.Recommendation.BlockReasons...)
		return plan
	}
	sourcePath, err := resolveExistingGitPath(worktree, filepath.Join(basePath, sourceFile), "workload sourceFile")
	if err != nil {
		plan.Errors = append(plan.Errors, err.Error())
		return plan
	}
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
	document, modifiedDocument, err := patchSourceDocuments(documents, modifiedDocuments, profile.Spec.Namespace, workload, plan)
	if err != nil {
		plan.Errors = append(plan.Errors, err.Error())
		return plan
	}

	if report.Recommendation.Stability == nil {
		plan.Blocked = true
		plan.BlockReasons = append(plan.BlockReasons, "stability state is unavailable; run with --state-db before planning Git changes")
		return plan
	}
	recoveryChange := workload.Policy.AvailabilityRecovery.Enabled && availabilityRecoveryChange(report.Recommendation)
	if report.Rollout.Evaluated && !report.Rollout.Settled && !recoveryChange {
		plan.Blocked = true
		plan.BlockReasons = append(plan.BlockReasons, "workload rollout is not settled: "+strings.Join(report.Rollout.Reasons, ", "))
		return plan
	}

	if workload.Scaling.Replicas {
		planReplicaPatch(plan, document, modifiedDocument, workload, report, recoveryChange)
	}

	if len(report.Containers) == 1 {
		containerName := report.Containers[0].Name
		currentCPU := report.Containers[0].CPURequest
		if currentCPU == "" {
			currentCPU = report.Recommendation.CurrentCPURequest
		}
		currentMemory := report.Containers[0].MemoryRequest
		if currentMemory == "" {
			currentMemory = report.Recommendation.CurrentMemoryRequest
		}
		if workload.Scaling.CPU {
			planContainerRequestPatch(plan, document, modifiedDocument, workload, report, containerName, "cpu", currentCPU, report.Recommendation.RecommendedCPURequest, report.Recommendation.Stability.CPU, recoveryChange)
		}
		if workload.Scaling.Memory {
			planContainerRequestPatch(plan, document, modifiedDocument, workload, report, containerName, "memory", currentMemory, report.Recommendation.RecommendedMemoryRequest, report.Recommendation.Stability.Memory, recoveryChange)
		}
	} else if plan.SourceFormat == patchSourceHelmValues && (workload.Scaling.CPU || workload.Scaling.Memory) {
		plan.Errors = append(plan.Errors, fmt.Sprintf("Helm values resource mappings require exactly one regular container in the live %s, found %d", workload.TargetRef.Kind, len(report.Containers)))
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
		originalForDiff := string(originalRendered)
		if plan.SourceFormat == patchSourceHelmValues {
			originalForDiff = string(sourceData)
		}
		plan.Diff = unifiedDiff(plan.SourceFile, originalForDiff, string(modifiedRendered))
	}
	return plan
}

func patchSourceFormat(workload config.WorkloadSpec) string {
	if workload.HelmValues != nil {
		return patchSourceHelmValues
	}
	return ""
}

func patchSourceDocuments(documents, modifiedDocuments []*yaml.Node, namespace string, workload config.WorkloadSpec, plan *PatchPlan) (*yaml.Node, *yaml.Node, error) {
	if plan.SourceFormat == patchSourceHelmValues {
		if len(documents) != 1 || len(modifiedDocuments) != 1 {
			return nil, nil, fmt.Errorf("Helm values source %s must contain exactly one non-empty YAML document", plan.SourceFile)
		}
		if documents[0].Kind != yaml.MappingNode || modifiedDocuments[0].Kind != yaml.MappingNode {
			return nil, nil, fmt.Errorf("Helm values source %s must have a mapping at its document root", plan.SourceFile)
		}
		return documents[0], modifiedDocuments[0], nil
	}

	document := findWorkloadDocument(documents, namespace, workload)
	if document == nil {
		return nil, nil, fmt.Errorf("resource %s/%s not found in %s", workload.TargetRef.Kind, workload.TargetRef.Name, plan.SourceFile)
	}
	modifiedDocument := findWorkloadDocument(modifiedDocuments, namespace, workload)
	if modifiedDocument == nil {
		return nil, nil, fmt.Errorf("resource %s/%s not found in dry-run document %s", workload.TargetRef.Kind, workload.TargetRef.Name, plan.SourceFile)
	}
	return document, modifiedDocument, nil
}

func planReplicaPatch(plan *PatchPlan, document, modifiedDocument *yaml.Node, workload config.WorkloadSpec, report *WorkloadReport, recoveryChange bool) {
	field := "spec.replicas"
	operation := "replace"
	var sourcePath []string
	var current string
	var ok bool

	if plan.SourceFormat == patchSourceHelmValues {
		sourcePath = workload.HelmValues.Paths.Replicas
		if len(sourcePath) == 0 {
			plan.Errors = append(plan.Errors, "helmValues.paths.replicas is required when replica scaling is enabled")
			return
		}
		var err error
		current, ok, err = helmScalarAt(document, sourcePath)
		if err != nil {
			plan.Errors = append(plan.Errors, err.Error())
			return
		}
		if !ok {
			plan.Errors = append(plan.Errors, fmt.Sprintf("configured Helm value %s does not exist in %s", formatHelmSourcePath(sourcePath), plan.SourceFile))
			return
		}
		if err := validateHelmReplicaBaseline(sourcePath, current, report.Recommendation.CurrentReplicas); err != nil {
			plan.Errors = append(plan.Errors, err.Error())
			return
		}
	} else {
		current, ok = scalarAt(document, "spec", "replicas")
		if !ok {
			current = missingValue
			operation = "add"
		}
	}

	if recoveryChange || stabilityGateAllowsPatch(report.Recommendation.Stability.Replicas, !ok) {
		recommended := strconv.FormatInt(int64(report.Recommendation.RecommendedReplicas), 10)
		if appendSourcePatchChange(plan, field, sourcePath, operation, current, recommended, helmReplicaValuesEqual(current, recommended)) {
			if plan.SourceFormat == patchSourceHelmValues {
				if err := setHelmScalarAt(modifiedDocument, sourcePath, recommended, false); err != nil {
					plan.Errors = append(plan.Errors, err.Error())
				}
			} else {
				setScalarAt(modifiedDocument, recommended, "spec", "replicas")
			}
		}
	} else if report.Recommendation.RecommendedReplicas != report.Recommendation.CurrentReplicas {
		plan.BlockReasons = append(plan.BlockReasons, "replicas blocked by stability gate: "+formatGate(report.Recommendation.Stability.Replicas))
	}
}

func planContainerRequestPatch(plan *PatchPlan, document, modifiedDocument *yaml.Node, workload config.WorkloadSpec, report *WorkloadReport, containerName, resourceName, liveCurrent, recommended string, gate StabilityGate, recoveryChange bool) {
	if plan.SourceFormat != patchSourceHelmValues && recommended == "" {
		return
	}
	field := fmt.Sprintf("spec.template.spec.containers[name=%s].resources.requests.%s", containerName, resourceName)
	operation := "replace"
	var sourcePath []string
	var current string
	var ok bool

	if plan.SourceFormat == patchSourceHelmValues {
		switch resourceName {
		case "cpu":
			sourcePath = workload.HelmValues.Paths.CPURequest
		case "memory":
			sourcePath = workload.HelmValues.Paths.MemoryRequest
		}
		if len(sourcePath) == 0 {
			plan.Errors = append(plan.Errors, fmt.Sprintf("helmValues.paths.%sRequest is required when %s scaling is enabled", resourceName, resourceName))
			return
		}
		var err error
		current, ok, err = helmScalarAt(document, sourcePath)
		if err != nil {
			plan.Errors = append(plan.Errors, err.Error())
			return
		}
		if !ok {
			plan.Errors = append(plan.Errors, fmt.Sprintf("configured Helm value %s does not exist in %s", formatHelmSourcePath(sourcePath), plan.SourceFile))
			return
		}
		if err := validateHelmResourceBaseline(sourcePath, current, liveCurrent, resourceName); err != nil {
			plan.Errors = append(plan.Errors, err.Error())
			return
		}
	} else {
		current, ok = containerRequestScalar(document, containerName, resourceName)
		if !ok {
			current = missingValue
			operation = "add"
		}
	}
	if recommended == "" {
		return
	}

	if recoveryChange || stabilityGateAllowsPatch(gate, !ok) {
		if appendSourcePatchChange(plan, field, sourcePath, operation, current, recommended, helmResourceValuesEqual(current, recommended)) {
			if plan.SourceFormat == patchSourceHelmValues {
				if err := setHelmScalarAt(modifiedDocument, sourcePath, recommended, true); err != nil {
					plan.Errors = append(plan.Errors, err.Error())
				}
			} else {
				setContainerRequestScalar(modifiedDocument, containerName, resourceName, recommended)
			}
		}
	} else if recommended != liveCurrent {
		plan.BlockReasons = append(plan.BlockReasons, fmt.Sprintf("%s request blocked by stability gate: %s", resourceName, formatGate(gate)))
	}
}

func appendSourcePatchChange(plan *PatchPlan, field string, sourcePath []string, operation, current, recommended string, semanticallyEqual bool) bool {
	if current == recommended || plan.SourceFormat == patchSourceHelmValues && semanticallyEqual {
		return false
	}
	plan.Changes = append(plan.Changes, PatchChange{
		Field:       field,
		SourcePath:  append([]string(nil), sourcePath...),
		Operation:   operation,
		Current:     current,
		Recommended: recommended,
	})
	return true
}

func validateHelmReplicaBaseline(path []string, current string, live int32) error {
	value, err := strconv.ParseInt(current, 10, 32)
	if err != nil || value < 0 {
		return fmt.Errorf("configured Helm value %s must be a non-negative integer replica count, got %q", formatHelmSourcePath(path), current)
	}
	if int32(value) != live {
		return fmt.Errorf("configured Helm value %s is %q but the live workload has %d replicas; the mapping may be wrong or Fleet is not converged", formatHelmSourcePath(path), current, live)
	}
	return nil
}

func validateHelmResourceBaseline(path []string, current, live, resourceName string) error {
	currentQuantity, err := resource.ParseQuantity(current)
	if err != nil {
		return fmt.Errorf("configured Helm value %s has invalid %s quantity %q: %v", formatHelmSourcePath(path), resourceName, current, err)
	}
	if strings.TrimSpace(live) == "" {
		return fmt.Errorf("live workload does not have a %s request; the Helm mapping cannot be verified", resourceName)
	}
	liveQuantity, err := resource.ParseQuantity(live)
	if err != nil {
		return fmt.Errorf("live workload has invalid %s request %q: %v", resourceName, live, err)
	}
	if currentQuantity.Cmp(liveQuantity) != 0 {
		return fmt.Errorf("configured Helm value %s is %q but the live workload %s request is %q; the mapping may be wrong or Fleet is not converged", formatHelmSourcePath(path), current, resourceName, live)
	}
	return nil
}

func helmReplicaValuesEqual(left, right string) bool {
	leftValue, leftErr := strconv.ParseInt(left, 10, 32)
	rightValue, rightErr := strconv.ParseInt(right, 10, 32)
	return leftErr == nil && rightErr == nil && leftValue == rightValue
}

func helmResourceValuesEqual(left, right string) bool {
	leftValue, leftErr := resource.ParseQuantity(left)
	rightValue, rightErr := resource.ParseQuantity(right)
	return leftErr == nil && rightErr == nil && leftValue.Cmp(rightValue) == 0
}

func helmScalarAt(document *yaml.Node, path []string) (string, bool, error) {
	node, ok, err := helmScalarNodeAt(document, path)
	if err != nil || !ok {
		return "", ok, err
	}
	return node.Value, true, nil
}

func helmScalarNodeAt(document *yaml.Node, path []string) (*yaml.Node, bool, error) {
	if len(path) == 0 {
		return nil, false, fmt.Errorf("Helm value path must not be empty")
	}
	current := document
	for index, key := range path {
		if current.Anchor != "" {
			return nil, false, fmt.Errorf("configured Helm value %s traverses YAML anchor %q", formatHelmSourcePath(path), current.Anchor)
		}
		if current.Kind == yaml.AliasNode {
			return nil, false, fmt.Errorf("configured Helm value %s traverses a YAML alias before key %q", formatHelmSourcePath(path), key)
		}
		if current.Kind != yaml.MappingNode {
			return nil, false, fmt.Errorf("configured Helm value %s traverses a non-mapping value before key %q", formatHelmSourcePath(path), key)
		}
		next, found, err := uniqueHelmMappingValue(current, key)
		if err != nil {
			return nil, false, fmt.Errorf("configured Helm value %s: %w", formatHelmSourcePath(path), err)
		}
		if !found {
			return nil, false, nil
		}
		current = next
		if current.Anchor != "" {
			return nil, false, fmt.Errorf("configured Helm value %s selects YAML anchor %q at key %q", formatHelmSourcePath(path), current.Anchor, key)
		}
		if index < len(path)-1 && current.Kind == yaml.AliasNode {
			return nil, false, fmt.Errorf("configured Helm value %s traverses a YAML alias at key %q", formatHelmSourcePath(path), key)
		}
	}
	if current.Kind != yaml.ScalarNode || current.Tag == "!!null" {
		return nil, false, fmt.Errorf("configured Helm value %s must point to an existing non-null scalar", formatHelmSourcePath(path))
	}
	return current, true, nil
}

func uniqueHelmMappingValue(mapping *yaml.Node, key string) (*yaml.Node, bool, error) {
	if len(mapping.Content)%2 != 0 {
		return nil, false, fmt.Errorf("contains a malformed YAML mapping")
	}
	var result *yaml.Node
	for index := 0; index < len(mapping.Content); index += 2 {
		keyNode := mapping.Content[index]
		if keyNode.Kind != yaml.ScalarNode {
			return nil, false, fmt.Errorf("contains a non-scalar mapping key")
		}
		if keyNode.Value == "<<" || keyNode.Tag == "!!merge" {
			return nil, false, fmt.Errorf("uses a YAML merge key on the selected path")
		}
		if keyNode.Value != key {
			continue
		}
		if result != nil {
			return nil, false, fmt.Errorf("contains duplicate key %q", key)
		}
		result = mapping.Content[index+1]
	}
	return result, result != nil, nil
}

func setHelmScalarAt(document *yaml.Node, path []string, value string, stringValue bool) error {
	node, ok, err := helmScalarNodeAt(document, path)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("configured Helm value %s does not exist", formatHelmSourcePath(path))
	}
	replacement := scalarNode(value)
	if stringValue || node.Tag == "!!str" {
		replacement.Tag = "!!str"
	}
	node.Kind = replacement.Kind
	node.Tag = replacement.Tag
	node.Value = replacement.Value
	node.Content = nil
	node.Alias = nil
	if replacement.Tag != "!!str" {
		node.Style = 0
	}
	return nil
}

func formatHelmSourcePath(path []string) string {
	var result strings.Builder
	result.WriteString("helmValues")
	for _, segment := range path {
		fmt.Fprintf(&result, "[%q]", segment)
	}
	return result.String()
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

func resolveExistingGitPath(worktree, relativePath, field string) (string, error) {
	root, err := filepath.Abs(worktree)
	if err != nil {
		return "", fmt.Errorf("resolve Git worktree: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve Git worktree: %w", err)
	}
	unresolved := filepath.Clean(filepath.Join(root, relativePath))
	candidate, err := filepath.EvalSymlinks(unresolved)
	if err != nil {
		return "", fmt.Errorf("resolve %s %s: %w", field, filepath.ToSlash(relativePath), err)
	}
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return "", fmt.Errorf("verify %s containment: %w", field, err)
	}
	if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s %s resolves outside the Git worktree", field, filepath.ToSlash(relativePath))
	}
	if candidate != unresolved {
		return "", fmt.Errorf("%s %s must not resolve through a symlink", field, filepath.ToSlash(relativePath))
	}
	info, err := os.Stat(candidate)
	if err != nil {
		return "", fmt.Errorf("stat %s %s: %w", field, filepath.ToSlash(relativePath), err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s %s must resolve to a regular file", field, filepath.ToSlash(relativePath))
	}
	return candidate, nil
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
