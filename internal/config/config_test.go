package config

import (
	"strings"
	"testing"
)

func TestRenderQuery(t *testing.T) {
	got, err := RenderQuery(`up{namespace="{{ .namespace }}",deployment="{{ .deployment }}"}`, map[string]string{
		"namespace":  "shipyardhq",
		"deployment": "shipyardhq",
	})
	if err != nil {
		t.Fatalf("RenderQuery() error = %v", err)
	}
	want := `up{namespace="shipyardhq",deployment="shipyardhq"}`
	if got != want {
		t.Fatalf("RenderQuery() = %q, want %q", got, want)
	}
}

func TestRenderQueryMissingKey(t *testing.T) {
	if _, err := RenderQuery(`up{namespace="{{ .namespace }}"}`, map[string]string{}); err == nil {
		t.Fatalf("RenderQuery() expected missing key error")
	}
}

func TestValidateRejectsUnsupportedSafetyRisk(t *testing.T) {
	profile := validTestProfile()
	profile.Spec.Workloads[0].Policy.Safety.AllowAutoCommit = []string{"low_risk", "unknown"}
	if err := profile.Validate(); err == nil {
		t.Fatal("Validate() expected unsupported allowAutoCommit risk error")
	}
}

func TestValidateRejectsUnsupportedMaxDecreaseRisk(t *testing.T) {
	profile := validTestProfile()
	profile.Spec.Workloads[0].Policy.Safety.MaxDecreaseRisk = "unknown"
	if err := profile.Validate(); err == nil {
		t.Fatal("Validate() expected unsupported maxDecreaseRisk error")
	}
}

func TestValidateRejectsInvalidMinAutoCommitConfidence(t *testing.T) {
	profile := validTestProfile()
	profile.Spec.Workloads[0].Policy.Confidence.MinAutoCommit = 1.2
	if err := profile.Validate(); err == nil {
		t.Fatal("Validate() expected invalid minAutoCommit error")
	}
}

func TestValidateRejectsInvalidAvailabilityRecoveryPolicy(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*AvailabilityRecoveryPolicySpec)
	}{
		{
			name: "failure grace period",
			mutate: func(policy *AvailabilityRecoveryPolicySpec) {
				policy.FailureGracePeriod = "later"
			},
		},
		{
			name: "cooldown",
			mutate: func(policy *AvailabilityRecoveryPolicySpec) {
				policy.Cooldown = "-1m"
			},
		},
		{
			name: "attempt limit",
			mutate: func(policy *AvailabilityRecoveryPolicySpec) {
				policy.MaxAttemptsPerHour = -1
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile := validTestProfile()
			test.mutate(&profile.Spec.Workloads[0].Policy.AvailabilityRecovery)
			if err := profile.Validate(); err == nil {
				t.Fatal("Validate() expected availability recovery policy error")
			}
		})
	}
}

func TestValidateAcceptsHelmValuePaths(t *testing.T) {
	profile := validTestProfile()
	workload := &profile.Spec.Workloads[0]
	workload.SourceFile = "values.yaml"
	workload.Scaling = ScalingSpec{Replicas: true, CPU: true, Memory: true}
	workload.HelmValues = &HelmValuesSpec{Paths: HelmValuePaths{
		Replicas:      []string{"replicaCount"},
		CPURequest:    []string{"server", "resources", "requests", "cpu"},
		MemoryRequest: []string{"server", "resources", "requests", "memory"},
	}}
	if err := profile.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateAcceptsStatefulSetTarget(t *testing.T) {
	profile := validTestProfile()
	profile.Spec.Workloads[0].TargetRef.Kind = "StatefulSet"
	profile.Spec.Workloads[0].TargetRef.Name = "valkey-node"
	if err := profile.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateRejectsInvalidHelmValueMappings(t *testing.T) {
	tests := []struct {
		name string
		want string
		edit func(*ApplicationProfile)
	}{
		{
			name: "missing source file",
			want: "sourceFile is required",
			edit: func(profile *ApplicationProfile) {
				profile.Spec.Workloads[0].HelmValues = &HelmValuesSpec{Paths: HelmValuePaths{Replicas: []string{"replicaCount"}}}
			},
		},
		{
			name: "missing enabled mapping",
			want: "helmValues.paths.cpuRequest is required",
			edit: func(profile *ApplicationProfile) {
				workload := &profile.Spec.Workloads[0]
				workload.SourceFile = "values.yaml"
				workload.Scaling.CPU = true
				workload.HelmValues = &HelmValuesSpec{Paths: HelmValuePaths{Replicas: []string{"replicaCount"}}}
			},
		},
		{
			name: "empty segment",
			want: "must not be empty",
			edit: func(profile *ApplicationProfile) {
				workload := &profile.Spec.Workloads[0]
				workload.SourceFile = "values.yaml"
				workload.HelmValues = &HelmValuesSpec{Paths: HelmValuePaths{CPURequest: []string{"resources", "", "cpu"}}}
			},
		},
		{
			name: "empty paths",
			want: "must configure at least one value path",
			edit: func(profile *ApplicationProfile) {
				workload := &profile.Spec.Workloads[0]
				workload.SourceFile = "values.yaml"
				workload.HelmValues = &HelmValuesSpec{}
			},
		},
		{
			name: "overlapping paths",
			want: "overlaps workload web",
			edit: func(profile *ApplicationProfile) {
				workload := &profile.Spec.Workloads[0]
				workload.SourceFile = "values.yaml"
				workload.HelmValues = &HelmValuesSpec{Paths: HelmValuePaths{
					CPURequest:    []string{"resources"},
					MemoryRequest: []string{"resources", "requests", "memory"},
				}}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile := validTestProfile()
			test.edit(profile)
			err := profile.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestValidateRejectsSharedHelmPathOwnershipAndMixedFormats(t *testing.T) {
	tests := []struct {
		name       string
		secondHelm bool
		secondPath []string
		want       string
	}{
		{name: "duplicate ownership", secondHelm: true, secondPath: []string{"resources", "requests", "cpu"}, want: "overlaps workload web"},
		{name: "prefix ownership", secondHelm: true, secondPath: []string{"resources", "requests"}, want: "overlaps workload web"},
		{name: "mixed formats", secondHelm: false, want: "must not mix"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile := validTestProfile()
			first := &profile.Spec.Workloads[0]
			first.SourceFile = "charts/../values.yaml"
			first.HelmValues = &HelmValuesSpec{Paths: HelmValuePaths{CPURequest: []string{"resources", "requests", "cpu"}}}
			second := *first
			second.Name = "worker"
			second.TargetRef.Name = "worker"
			second.SourceFile = "values.yaml"
			if test.secondHelm {
				second.HelmValues = &HelmValuesSpec{Paths: HelmValuePaths{CPURequest: test.secondPath}}
			} else {
				second.HelmValues = nil
			}
			profile.Spec.Workloads = append(profile.Spec.Workloads, second)
			err := profile.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func validTestProfile() *ApplicationProfile {
	return &ApplicationProfile{
		Metadata: Metadata{Name: "shipyard"},
		Spec: ApplicationSpec{
			Namespace: "shipyardhq",
			Workloads: []WorkloadSpec{
				{
					Name:             "web",
					TargetRef:        TargetRef{Kind: "Deployment", Name: "shipyardhq"},
					MetricProfileRef: "http",
				},
			},
		},
		MetricProfiles: map[string]MetricProfile{
			"http": {},
		},
	}
}
