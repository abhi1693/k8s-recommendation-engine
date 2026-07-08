package config

import (
	"bytes"
	"fmt"
	"os"
	"text/template"

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
	MetricProfileRef string            `yaml:"metricProfileRef" json:"metricProfileRef"`
	Scaling          ScalingSpec       `yaml:"scaling" json:"scaling"`
	Bounds           BoundsSpec        `yaml:"bounds" json:"bounds"`
	Vars             map[string]string `yaml:"vars" json:"vars"`
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
	MaxIncreasePercent int `yaml:"maxIncreasePercent" json:"maxIncreasePercent"`
	MaxDecreasePercent int `yaml:"maxDecreasePercent" json:"maxDecreasePercent"`
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
		if workload.TargetRef.Kind != "Deployment" {
			return fmt.Errorf("workload %s target kind %q is unsupported; only Deployment is implemented", workload.Name, workload.TargetRef.Kind)
		}
		if workload.TargetRef.Name == "" {
			return fmt.Errorf("workload %s targetRef.name is required", workload.Name)
		}
		if _, ok := p.MetricProfiles[workload.MetricProfileRef]; !ok {
			return fmt.Errorf("workload %s references missing metric profile %q", workload.Name, workload.MetricProfileRef)
		}
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
