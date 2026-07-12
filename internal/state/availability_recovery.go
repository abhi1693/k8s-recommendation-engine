package state

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const availabilityRecoveryTimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

type AvailabilityRecoveryKey struct {
	Application string
	Namespace   string
	Workload    string
	Deployment  string
	Pod         string
	PodUID      string
}

type AvailabilityRecoveryReservation struct {
	ID      int64
	Allowed bool
	Reason  string
}

func ReserveAvailabilityRecovery(ctx context.Context, path string, key AvailabilityRecoveryKey, now time.Time, cooldown time.Duration, maxAttemptsPerHour int) (AvailabilityRecoveryReservation, error) {
	if path == "" {
		return AvailabilityRecoveryReservation{Reason: "availability recovery requires --state-db"}, nil
	}
	store, err := Open(path)
	if err != nil {
		return AvailabilityRecoveryReservation{}, err
	}
	defer store.Close()
	return store.reserveAvailabilityRecovery(ctx, key, now, cooldown, maxAttemptsPerHour)
}

func CompleteAvailabilityRecovery(ctx context.Context, path string, id int64, status, message string) error {
	if path == "" || id == 0 {
		return nil
	}
	store, err := Open(path)
	if err != nil {
		return err
	}
	defer store.Close()
	if _, err := store.db.ExecContext(ctx, `
		UPDATE availability_recovery_events
		SET status = ?, message = ?
		WHERE id = ?
	`, status, message, id); err != nil {
		return fmt.Errorf("complete availability recovery event: %w", err)
	}
	return nil
}

func (s *Store) reserveAvailabilityRecovery(ctx context.Context, key AvailabilityRecoveryKey, now time.Time, cooldown time.Duration, maxAttemptsPerHour int) (AvailabilityRecoveryReservation, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	if maxAttemptsPerHour <= 0 {
		maxAttemptsPerHour = 6
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AvailabilityRecoveryReservation{}, fmt.Errorf("begin availability recovery reservation: %w", err)
	}
	defer tx.Rollback()

	var latestRaw string
	err = tx.QueryRowContext(ctx, `
		SELECT attempted_at
		FROM availability_recovery_events
		WHERE application = ? AND namespace = ? AND workload_name = ?
		ORDER BY attempted_at DESC
		LIMIT 1
	`, key.Application, key.Namespace, key.Workload).Scan(&latestRaw)
	if err != nil && err != sql.ErrNoRows {
		return AvailabilityRecoveryReservation{}, fmt.Errorf("query latest availability recovery: %w", err)
	}
	if err == nil && cooldown > 0 {
		latest, parseErr := time.Parse(time.RFC3339Nano, latestRaw)
		if parseErr != nil {
			return AvailabilityRecoveryReservation{}, fmt.Errorf("parse latest availability recovery time: %w", parseErr)
		}
		readyAt := latest.Add(cooldown)
		if now.Before(readyAt) {
			return AvailabilityRecoveryReservation{Reason: fmt.Sprintf("availability recovery cooldown active until %s", readyAt.Format(time.RFC3339))}, nil
		}
	}

	var attempts int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM availability_recovery_events
		WHERE application = ? AND namespace = ? AND workload_name = ? AND attempted_at >= ?
	`, key.Application, key.Namespace, key.Workload, now.Add(-time.Hour).Format(availabilityRecoveryTimeFormat)).Scan(&attempts); err != nil {
		return AvailabilityRecoveryReservation{}, fmt.Errorf("count availability recovery attempts: %w", err)
	}
	if attempts >= maxAttemptsPerHour {
		return AvailabilityRecoveryReservation{Reason: fmt.Sprintf("availability recovery hourly attempt limit reached: %d/%d", attempts, maxAttemptsPerHour)}, nil
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO availability_recovery_events (
			application, namespace, workload_name, deployment, pod_name, pod_uid,
			attempted_at, action, status, message
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'recreate_failed_pod', 'reserved', '')
	`, key.Application, key.Namespace, key.Workload, key.Deployment, key.Pod, key.PodUID, now.Format(availabilityRecoveryTimeFormat))
	if err != nil {
		return AvailabilityRecoveryReservation{}, fmt.Errorf("reserve availability recovery: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return AvailabilityRecoveryReservation{}, fmt.Errorf("read availability recovery reservation id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return AvailabilityRecoveryReservation{}, fmt.Errorf("commit availability recovery reservation: %w", err)
	}
	return AvailabilityRecoveryReservation{ID: id, Allowed: true}, nil
}
