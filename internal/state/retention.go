package state

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const statePruneInterval = time.Hour

// PruneExpired removes persisted learning and operational history older than
// retention. It runs at most once per hour per database. SQLite reuses the
// released pages; shrinking an existing database file still requires an
// operator-controlled VACUUM while the controller is stopped.
func PruneExpired(ctx context.Context, path string, retention time.Duration, now time.Time) error {
	if path == "" || retention <= 0 {
		return nil
	}
	store, err := Open(path)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.PruneExpired(ctx, retention, now)
}

func (s *Store) PruneExpired(ctx context.Context, retention time.Duration, now time.Time) error {
	if s == nil || s.db == nil || retention <= 0 {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var lastPrunedRaw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM state_maintenance WHERE key = 'last_pruned_at'`).Scan(&lastPrunedRaw)
	switch {
	case err == nil:
		if lastPruned, parseErr := time.Parse(time.RFC3339Nano, lastPrunedRaw); parseErr == nil && now.Before(lastPruned.Add(statePruneInterval)) {
			return nil
		}
	case err != sql.ErrNoRows:
		return fmt.Errorf("read state prune watermark: %w", err)
	}

	cutoff := now.Add(-retention).Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin state prune: %w", err)
	}
	defer tx.Rollback()
	statements := []string{
		`DELETE FROM learned_signals WHERE run_id IN (SELECT id FROM workload_runs WHERE generated_at < ?)`,
		`DELETE FROM learned_decisions WHERE run_id IN (SELECT id FROM workload_runs WHERE generated_at < ?)`,
		`DELETE FROM forecast_scores WHERE run_id IN (SELECT id FROM workload_runs WHERE generated_at < ?)`,
		`DELETE FROM workload_runs WHERE generated_at < ?`,
		`DELETE FROM seasonal_signal_observations WHERE generated_at < ?`,
		`DELETE FROM convergence_observations WHERE generated_at < ?`,
		`DELETE FROM proposal_events WHERE generated_at < ?`,
		`DELETE FROM availability_recovery_events WHERE attempted_at < ?`,
		`DELETE FROM proposal_batch_items WHERE last_seen_at < ?`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement, cutoff); err != nil {
			return fmt.Errorf("prune expired state: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO state_maintenance(key, value) VALUES ('last_pruned_at', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, now.Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("record state prune watermark: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit state prune: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA optimize;`); err != nil {
		return fmt.Errorf("optimize pruned state: %w", err)
	}
	return nil
}
