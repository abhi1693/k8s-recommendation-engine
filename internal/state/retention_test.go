package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneExpiredBoundsLearningHistory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	now := time.Date(2026, 7, 16, 15, 0, 0, 0, time.UTC)
	old := testReport()
	old.GeneratedAt = now.Add(-30 * 24 * time.Hour)
	if err := AttachAndRecord(ctx, path, old); err != nil {
		t.Fatal(err)
	}
	recent := testReport()
	recent.GeneratedAt = now.Add(-24 * time.Hour)
	if err := AttachAndRecord(ctx, path, recent); err != nil {
		t.Fatal(err)
	}

	if err := PruneExpired(ctx, path, 14*24*time.Hour, now); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for table, want := range map[string]int{
		"workload_runs":     1,
		"learned_signals":   1,
		"learned_decisions": 1,
	} {
		var got int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s rows = %d, want %d", table, got, want)
		}
	}
}

func TestPruneExpiredRunsAtMostHourly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	now := time.Date(2026, 7, 16, 15, 0, 0, 0, time.UTC)
	if err := PruneExpired(ctx, path, 24*time.Hour, now); err != nil {
		t.Fatal(err)
	}
	old := testReport()
	old.GeneratedAt = now.Add(-48 * time.Hour)
	if err := AttachAndRecord(ctx, path, old); err != nil {
		t.Fatal(err)
	}
	if err := PruneExpired(ctx, path, 24*time.Hour, now.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workload_runs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	store.Close()
	if count != 1 {
		t.Fatalf("workload runs inside prune interval = %d, want 1", count)
	}
	if err := PruneExpired(ctx, path, 24*time.Hour, now.Add(61*time.Minute)); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workload_runs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("workload runs after prune interval = %d, want 0", count)
	}
}
