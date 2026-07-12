package state

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abhi1693/k8s-recommendation-engine/internal/analyzer"
)

func TestAvailabilityRecoveryStabilityGateAllowsOnlyIncreases(t *testing.T) {
	workload := &analyzer.WorkloadReport{Recommendation: analyzer.Recommendation{
		AvailabilityRecovery:     true,
		CurrentReplicas:          3,
		RecommendedReplicas:      4,
		CurrentCPURequest:        "200m",
		RecommendedCPURequest:    "250m",
		CurrentMemoryRequest:     "3110Mi",
		RecommendedMemoryRequest: "3892Mi",
		Stability: &analyzer.RecommendationStability{
			Replicas: analyzer.StabilityGate{Status: "blocked"},
			CPU:      analyzer.StabilityGate{Status: "blocked"},
			Memory:   analyzer.StabilityGate{Status: "blocked"},
		},
	}}

	applyAvailabilityRecoveryStabilityGate(workload)
	stability := workload.Recommendation.Stability
	if !stability.Actionable || stability.Replicas.Status != "stable" || stability.CPU.Status != "stable" || stability.Memory.Status != "stable" {
		t.Fatalf("Stability = %#v, want all recovery increases actionable", stability)
	}

	workload.Recommendation.RecommendedMemoryRequest = "3Gi"
	workload.Recommendation.Stability.Memory = analyzer.StabilityGate{Status: "blocked"}
	applyAvailabilityRecoveryStabilityGate(workload)
	if workload.Recommendation.Stability.Actionable || workload.Recommendation.Stability.Memory.Status != "blocked" {
		t.Fatalf("Stability = %#v, want memory decrease blocked", workload.Recommendation.Stability)
	}
}

func TestAvailabilityRecoveryReservationEnforcesCooldownAndHourlyLimit(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	now := time.Date(2026, 7, 12, 14, 30, 0, 0, time.UTC)
	key := AvailabilityRecoveryKey{
		Application: "shipyard",
		Namespace:   "shipyardhq",
		Workload:    "web",
		Deployment:  "shipyardhq",
		Pod:         "shipyardhq-1",
		PodUID:      "uid-1",
	}

	first, err := ReserveAvailabilityRecovery(ctx, path, key, now, 5*time.Minute, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Allowed || first.ID == 0 {
		t.Fatalf("first reservation = %#v, want allowed", first)
	}
	if err := CompleteAvailabilityRecovery(ctx, path, first.ID, "succeeded", "deleted"); err != nil {
		t.Fatal(err)
	}

	duringCooldown, err := ReserveAvailabilityRecovery(ctx, path, key, now.Add(time.Minute), 5*time.Minute, 2)
	if err != nil {
		t.Fatal(err)
	}
	if duringCooldown.Allowed || !strings.Contains(duringCooldown.Reason, "cooldown") {
		t.Fatalf("cooldown reservation = %#v, want cooldown block", duringCooldown)
	}

	second, err := ReserveAvailabilityRecovery(ctx, path, key, now.Add(6*time.Minute), 5*time.Minute, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Allowed || second.ID == 0 {
		t.Fatalf("second reservation = %#v, want allowed", second)
	}

	atLimit, err := ReserveAvailabilityRecovery(ctx, path, key, now.Add(12*time.Minute), 5*time.Minute, 2)
	if err != nil {
		t.Fatal(err)
	}
	if atLimit.Allowed || !strings.Contains(atLimit.Reason, "hourly attempt limit") {
		t.Fatalf("hourly-limit reservation = %#v, want limit block", atLimit)
	}
}

func TestAvailabilityRecoveryReservationRequiresStateDB(t *testing.T) {
	reservation, err := ReserveAvailabilityRecovery(context.Background(), "", AvailabilityRecoveryKey{}, time.Now(), time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if reservation.Allowed || reservation.Reason != "availability recovery requires --state-db" {
		t.Fatalf("reservation = %#v, want state DB requirement", reservation)
	}
}
