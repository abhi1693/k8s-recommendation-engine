package config

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"
)

type ApplicationProfile struct {
	APIVersion     string                   `yaml:"apiVersion" json:"apiVersion"`
	Kind           string                   `yaml:"kind" json:"kind"`
	Metadata       Metadata                 `yaml:"metadata" json:"metadata"`
	Spec           ApplicationSpec          `yaml:"spec" json:"spec"`
	MetricProfiles map[string]MetricProfile `yaml:"metricProfiles" json:"metricProfiles"`
}

type Metadata struct {
	Name string `yaml:"name" json:"name"`
}

type ApplicationSpec struct {
	Namespace     string         `yaml:"namespace" json:"namespace"`
	Git           GitSpec        `yaml:"git" json:"git"`
	SharedSignals []SharedSignal `yaml:"sharedSignals" json:"sharedSignals"`
	Workloads     []WorkloadSpec `yaml:"workloads" json:"workloads"`
}

type GitSpec struct {
	Provider   string `yaml:"provider" json:"provider"`
	Mode       string `yaml:"mode" json:"mode"`
	Repository string `yaml:"repository" json:"repository"`
	Branch     string `yaml:"branch" json:"branch"`
	BasePath   string `yaml:"basePath" json:"basePath"`
}

type SharedSignal struct {
	Name             string            `yaml:"name" json:"name"`
	MetricProfileRef string            `yaml:"metricProfileRef" json:"metricProfileRef"`
	Required         bool              `yaml:"required" json:"required"`
	Vars             map[string]string `yaml:"vars" json:"vars"`
}

type WorkloadSpec struct {
	Name             string            `yaml:"name" json:"name"`
	TargetRef        TargetRef         `yaml:"targetRef" json:"targetRef"`
	SourceFile       string            `yaml:"sourceFile" json:"sourceFile"`
	HelmValues       *HelmValuesSpec   `yaml:"helmValues,omitempty" json:"helmValues,omitempty"`
	MetricProfileRef string            `yaml:"metricProfileRef" json:"metricProfileRef"`
	Scaling          ScalingSpec       `yaml:"scaling" json:"scaling"`
	Bounds           BoundsSpec        `yaml:"bounds" json:"bounds"`
	Policy           PolicySpec        `yaml:"policy" json:"policy"`
	Vars             map[string]string `yaml:"vars" json:"vars"`
}

type HelmValuesSpec struct {
	Paths HelmValuePaths `yaml:"paths" json:"paths"`
}

type HelmValuePaths struct {
	Replicas      []string `yaml:"replicas,omitempty" json:"replicas,omitempty"`
	CPURequest    []string `yaml:"cpuRequest,omitempty" json:"cpuRequest,omitempty"`
	MemoryRequest []string `yaml:"memoryRequest,omitempty" json:"memoryRequest,omitempty"`
}

type TargetRef struct {
	APIVersion string `yaml:"apiVersion" json:"apiVersion"`
	Kind       string `yaml:"kind" json:"kind"`
	Name       string `yaml:"name" json:"name"`
}

type ScalingSpec struct {
	Replicas bool `yaml:"replicas" json:"replicas"`
	CPU      bool `yaml:"cpu" json:"cpu"`
	Memory   bool `yaml:"memory" json:"memory"`
}

type BoundsSpec struct {
	Replicas ReplicaBounds `yaml:"replicas" json:"replicas"`
	CPU      ChangeBounds  `yaml:"cpu" json:"cpu"`
	Memory   ChangeBounds  `yaml:"memory" json:"memory"`
}

type ReplicaBounds struct {
	Min                int `yaml:"min" json:"min"`
	Max                int `yaml:"max" json:"max"`
	MaxIncreasePercent int `yaml:"maxIncreasePercent" json:"maxIncreasePercent"`
	MaxDecreasePercent int `yaml:"maxDecreasePercent" json:"maxDecreasePercent"`
}

type ChangeBounds struct {
	MaxIncreasePercent int     `yaml:"maxIncreasePercent" json:"maxIncreasePercent"`
	MaxDecreasePercent int     `yaml:"maxDecreasePercent" json:"maxDecreasePercent"`
	MinChangePercent   float64 `yaml:"minChangePercent" json:"minChangePercent"`
}

type PolicySpec struct {
	MaxProposalsPerHour  int                            `yaml:"maxProposalsPerHour" json:"maxProposalsPerHour"`
	MaxProposalsPerDay   int                            `yaml:"maxProposalsPerDay" json:"maxProposalsPerDay"`
	Safety               SafetyPolicySpec               `yaml:"safety" json:"safety"`
	Confidence           ConfidencePolicySpec           `yaml:"confidence" json:"confidence"`
	AvailabilityRecovery AvailabilityRecoveryPolicySpec `yaml:"availabilityRecovery" json:"availabilityRecovery"`
}

type SafetyPolicySpec struct {
	AllowAutoCommit     []string `yaml:"allowAutoCommit" json:"allowAutoCommit"`
	MaxDecreaseRisk     string   `yaml:"maxDecreaseRisk" json:"maxDecreaseRisk"`
	UrgentBypassAllowed *bool    `yaml:"urgentBypassAllowed" json:"urgentBypassAllowed,omitempty"`
}

type ConfidencePolicySpec struct {
	MinAutoCommit float64 `yaml:"minAutoCommit" json:"minAutoCommit"`
}

type AvailabilityRecoveryPolicySpec struct {
	Enabled            bool   `yaml:"enabled" json:"enabled"`
	FailureGracePeriod string `yaml:"failureGracePeriod" json:"failureGracePeriod,omitempty"`
	Cooldown           string `yaml:"cooldown" json:"cooldown,omitempty"`
	MaxAttemptsPerHour int    `yaml:"maxAttemptsPerHour" json:"maxAttemptsPerHour,omitempty"`
}

type MetricProfile struct {
	Description string            `yaml:"description" json:"description"`
	Signals     map[string]Signal `yaml:"signals" json:"signals"`
}

type Signal struct {
	Query    string `yaml:"query" json:"query"`
	Required bool   `yaml:"required" json:"required"`
}

func LoadFile(path string) (*ApplicationProfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var profile ApplicationProfile
	if err := yaml.Unmarshal(data, &profile); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	return &profile, nil
}

func (p *ApplicationProfile) Validate() error {
	if p.Metadata.Name == "" {
		return fmt.Errorf("metadata.name is required")
	}
	if p.Spec.Namespace == "" {
		return fmt.Errorf("spec.namespace is required")
	}
	if len(p.Spec.Workloads) == 0 {
		return fmt.Errorf("spec.workloads must not be empty")
	}
	if len(p.MetricProfiles) == 0 {
		return fmt.Errorf("metricProfiles must not be empty")
	}
	for _, workload := range p.Spec.Workloads {
		if workload.Name == "" {
			return fmt.Errorf("workload name is required")
		}
		if !supportedTargetKind(workload.TargetRef.Kind) {
			return fmt.Errorf("workload %s target kind %q is unsupported; supported kinds are Deployment and StatefulSet", workload.Name, workload.TargetRef.Kind)
		}
		if workload.TargetRef.Name == "" {
			return fmt.Errorf("workload %s targetRef.name is required", workload.Name)
		}
		if err := validateHelmValues(workload); err != nil {
			return err
		}
		if _, ok := p.MetricProfiles[workload.MetricProfileRef]; !ok {
			return fmt.Errorf("workload %s references missing metric profile %q", workload.Name, workload.MetricProfileRef)
		}
		if workload.Bounds.CPU.MinChangePercent < 0 {
			return fmt.Errorf("workload %s bounds.cpu.minChangePercent must be non-negative", workload.Name)
		}
		if workload.Bounds.Memory.MinChangePercent < 0 {
			return fmt.Errorf("workload %s bounds.memory.minChangePercent must be non-negative", workload.Name)
		}
		if workload.Policy.MaxProposalsPerHour < 0 {
			return fmt.Errorf("workload %s policy.maxProposalsPerHour must be non-negative", workload.Name)
		}
		if workload.Policy.MaxProposalsPerDay < 0 {
			return fmt.Errorf("workload %s policy.maxProposalsPerDay must be non-negative", workload.Name)
		}
		for _, risk := range workload.Policy.Safety.AllowAutoCommit {
			if !validSafetyRisk(risk) {
				return fmt.Errorf("workload %s policy.safety.allowAutoCommit contains unsupported risk %q", workload.Name, risk)
			}
		}
		if workload.Policy.Safety.MaxDecreaseRisk != "" && !validSafetyRisk(workload.Policy.Safety.MaxDecreaseRisk) {
			return fmt.Errorf("workload %s policy.safety.maxDecreaseRisk %q is unsupported", workload.Name, workload.Policy.Safety.MaxDecreaseRisk)
		}
		if workload.Policy.Confidence.MinAutoCommit < 0 || workload.Policy.Confidence.MinAutoCommit > 1 {
			return fmt.Errorf("workload %s policy.confidence.minAutoCommit must be between 0 and 1", workload.Name)
		}
		if value := workload.Policy.AvailabilityRecovery.FailureGracePeriod; value != "" {
			if duration, err := time.ParseDuration(value); err != nil || duration < 0 {
				return fmt.Errorf("workload %s policy.availabilityRecovery.failureGracePeriod must be a non-negative duration", workload.Name)
			}
		}
		if value := workload.Policy.AvailabilityRecovery.Cooldown; value != "" {
			if duration, err := time.ParseDuration(value); err != nil || duration < 0 {
				return fmt.Errorf("workload %s policy.availabilityRecovery.cooldown must be a non-negative duration", workload.Name)
			}
		}
		if workload.Policy.AvailabilityRecovery.MaxAttemptsPerHour < 0 {
			return fmt.Errorf("workload %s policy.availabilityRecovery.maxAttemptsPerHour must be non-negative", workload.Name)
		}
	}
	if err := validateHelmValueOwnership(p.Spec.Workloads); err != nil {
		return err
	}
	for _, signal := range p.Spec.SharedSignals {
		if signal.Name == "" {
			return fmt.Errorf("shared signal name is required")
		}
		if _, ok := p.MetricProfiles[signal.MetricProfileRef]; !ok {
			return fmt.Errorf("shared signal %s references missing metric profile %q", signal.Name, signal.MetricProfileRef)
		}
	}
	return nil
}

func supportedTargetKind(kind string) bool {
	switch kind {
	case "Deployment", "StatefulSet":
		return true
	default:
		return false
	}
}

func validateHelmValues(workload WorkloadSpec) error {
	if workload.HelmValues == nil {
		return nil
	}
	if strings.TrimSpace(workload.SourceFile) == "" {
		return fmt.Errorf("workload %s sourceFile is required when helmValues is configured", workload.Name)
	}

	paths := []struct {
		name    string
		value   []string
		enabled bool
	}{
		{name: "replicas", value: workload.HelmValues.Paths.Replicas, enabled: workload.Scaling.Replicas},
		{name: "cpuRequest", value: workload.HelmValues.Paths.CPURequest, enabled: workload.Scaling.CPU},
		{name: "memoryRequest", value: workload.HelmValues.Paths.MemoryRequest, enabled: workload.Scaling.Memory},
	}
	configured := 0
	for _, path := range paths {
		if path.enabled && len(path.value) == 0 {
			return fmt.Errorf("workload %s helmValues.paths.%s is required when the corresponding scaling field is enabled", workload.Name, path.name)
		}
		if len(path.value) == 0 {
			continue
		}
		configured++
		for index, segment := range path.value {
			if strings.TrimSpace(segment) == "" {
				return fmt.Errorf("workload %s helmValues.paths.%s[%d] must not be empty", workload.Name, path.name, index)
			}
		}
	}
	if configured == 0 {
		return fmt.Errorf("workload %s helmValues must configure at least one value path", workload.Name)
	}
	return nil
}

func validateHelmValueOwnership(workloads []WorkloadSpec) error {
	type owner struct {
		workload string
		field    string
		path     []string
	}
	formats := map[string]bool{}
	owners := map[string][]owner{}
	for _, workload := range workloads {
		if strings.TrimSpace(workload.SourceFile) == "" {
			continue
		}
		sourceFile := path.Clean(strings.ReplaceAll(strings.TrimSpace(workload.SourceFile), "\\", "/"))
		helm := workload.HelmValues != nil
		if existing, ok := formats[sourceFile]; ok && existing != helm {
			return fmt.Errorf("workloads sharing sourceFile %q must not mix Kubernetes manifest and Helm values mappings", sourceFile)
		}
		formats[sourceFile] = helm
		if !helm {
			continue
		}
		paths := []struct {
			field string
			value []string
		}{
			{field: "replicas", value: workload.HelmValues.Paths.Replicas},
			{field: "cpuRequest", value: workload.HelmValues.Paths.CPURequest},
			{field: "memoryRequest", value: workload.HelmValues.Paths.MemoryRequest},
		}
		for _, candidate := range paths {
			if len(candidate.value) == 0 {
				continue
			}
			for _, existing := range owners[sourceFile] {
				if helmPathsOverlap(existing.path, candidate.value) {
					return fmt.Errorf("workload %s helmValues.paths.%s overlaps workload %s helmValues.paths.%s in sourceFile %q", workload.Name, candidate.field, existing.workload, existing.field, sourceFile)
				}
			}
			owners[sourceFile] = append(owners[sourceFile], owner{
				workload: workload.Name,
				field:    candidate.field,
				path:     candidate.value,
			})
		}
	}
	return nil
}

func helmPathsOverlap(left, right []string) bool {
	shorter := len(left)
	if len(right) < shorter {
		shorter = len(right)
	}
	for index := 0; index < shorter; index++ {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validSafetyRisk(risk string) bool {
	switch risk {
	case "low_risk", "medium_risk", "high_risk":
		return true
	default:
		return false
	}
}

func (w WorkloadSpec) VarsWithDefaults(namespace string) map[string]string {
	vars := map[string]string{
		"namespace":  namespace,
		"deployment": w.TargetRef.Name,
		"workload":   w.Name,
	}
	for key, value := range w.Vars {
		vars[key] = value
	}
	return vars
}

func (s SharedSignal) VarsWithDefaults(namespace string) map[string]string {
	vars := map[string]string{
		"namespace": namespace,
		"signal":    s.Name,
	}
	for key, value := range s.Vars {
		vars[key] = value
	}
	return vars
}

func RenderQuery(query string, vars map[string]string) (string, error) {
	tmpl, err := template.New("promql").Option("missingkey=error").Parse(query)
	if err != nil {
		return "", fmt.Errorf("parse query template: %w", err)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, vars); err != nil {
		return "", fmt.Errorf("render query template: %w", err)
	}
	return rendered.String(), nil
}
