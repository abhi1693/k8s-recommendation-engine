package config

import "testing"

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
