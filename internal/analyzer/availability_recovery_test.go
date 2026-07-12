package analyzer

import "testing"

func TestAvailabilityRecoveryChangeRequiresRestorativeValues(t *testing.T) {
	tests := []struct {
		name           string
		recommendation Recommendation
		want           bool
	}{
		{
			name: "replica increase",
			recommendation: Recommendation{
				AvailabilityRecovery:     true,
				CurrentReplicas:          3,
				RecommendedReplicas:      4,
				CurrentCPURequest:        "200m",
				RecommendedCPURequest:    "200m",
				CurrentMemoryRequest:     "3110Mi",
				RecommendedMemoryRequest: "3892Mi",
			},
			want: true,
		},
		{
			name: "memory decrease",
			recommendation: Recommendation{
				AvailabilityRecovery:     true,
				CurrentReplicas:          3,
				RecommendedReplicas:      4,
				CurrentMemoryRequest:     "3110Mi",
				RecommendedMemoryRequest: "3Gi",
			},
		},
		{
			name: "invalid resource",
			recommendation: Recommendation{
				AvailabilityRecovery:     true,
				CurrentReplicas:          3,
				RecommendedReplicas:      4,
				CurrentMemoryRequest:     "3110Mi",
				RecommendedMemoryRequest: "invalid",
			},
		},
		{
			name: "no change",
			recommendation: Recommendation{
				AvailabilityRecovery:     true,
				CurrentReplicas:          3,
				RecommendedReplicas:      3,
				CurrentMemoryRequest:     "3110Mi",
				RecommendedMemoryRequest: "3110Mi",
			},
		},
		{
			name: "policy candidate absent",
			recommendation: Recommendation{
				CurrentReplicas:     3,
				RecommendedReplicas: 4,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := availabilityRecoveryChange(test.recommendation); got != test.want {
				t.Fatalf("availabilityRecoveryChange() = %t, want %t", got, test.want)
			}
		})
	}
}
