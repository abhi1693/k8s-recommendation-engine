package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/abhi1693/k8s-recommendation-engine/internal/analyzer"
	"k8s.io/apimachinery/pkg/api/resource"
	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("state db path is required")
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create state db directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open state db: %w", err)
	}
	store := &Store{db: db}
	if err := store.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func AttachAndRecord(ctx context.Context, path string, report *analyzer.Report) error {
	if path == "" {
		return nil
	}
	store, err := Open(path)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.AttachAndRecord(ctx, report)
}

func RecordObservation(ctx context.Context, path string, report *analyzer.ObservationReport) error {
	if path == "" || report == nil {
		return nil
	}
	store, err := Open(path)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.RecordObservation(ctx, report)
}

func (s *Store) AttachAndRecord(ctx context.Context, report *analyzer.Report) error {
	for index := range report.Workloads {
		workload := &report.Workloads[index]
		summary, err := s.summary(ctx, report.Application, report.GeneratedAt, workload)
		if err != nil {
			return err
		}
		workload.Recommendation.Learning.Mode = "prometheus-history+sqlite-state"
		workload.Recommendation.Learning.Description = "learned from the current Prometheus history window and prior persisted recommendation snapshots"
		workload.Recommendation.Learning.Persistent = summary
		stabilizeRecommendation(workload, summary)
		applyForecastAccuracyFeedback(workload, summary.ForecastAccuracy)
		applyReplicaOutcomeFeedback(workload, summary.LastOutcome)
		applySeasonalityFeedback(workload, summary.Seasonality)
		recentRuns, err := s.recentRuns(ctx, report.Application, workload.Namespace, workload.Name, postApplyObservationCooldownRuns)
		if err != nil {
			return err
		}
		var currentForecastScores []forecastScore
		if len(recentRuns) > 0 {
			currentForecastScores = scoreForecastAccuracy(recentRuns[0], workload)
			if summary.ForecastAccuracy != nil {
				summary.ForecastAccuracy.LastScoredRecommendationCount = len(currentForecastScores)
			}
		}
		workload.Recommendation.Stability = evaluateStability(workload, recentRuns)
		applyOutcomeSafetyGate(workload.Recommendation.Stability, summary.LastOutcome, workload.Recommendation.Mode)
		applyRecentOutcomeCooldownGate(workload.Recommendation.Stability, recentRuns, workload.Recommendation.Mode)
		applyAvailabilityRecoveryStabilityGate(workload)
		if err := s.recordWorkload(ctx, report.Application, report.GeneratedAt, workload, currentForecastScores); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) migrate(ctx context.Context) error {
	statements := []string{
		`PRAGMA journal_mode = WAL;`,
		`CREATE TABLE IF NOT EXISTS workload_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			application TEXT NOT NULL,
			namespace TEXT NOT NULL,
			workload_name TEXT NOT NULL,
			deployment TEXT NOT NULL,
			generated_at TEXT NOT NULL,
			current_replicas INTEGER NOT NULL,
			recommended_replicas INTEGER NOT NULL,
			current_cpu_request TEXT NOT NULL,
			recommended_cpu_request TEXT NOT NULL,
			current_memory_request TEXT NOT NULL,
			recommended_memory_request TEXT NOT NULL,
			confidence REAL NOT NULL,
			blocked INTEGER NOT NULL,
			recommendation_mode TEXT NOT NULL DEFAULT '',
			recommendation_actionable INTEGER NOT NULL DEFAULT 0,
			outcome_status TEXT NOT NULL DEFAULT '',
			replicas_actionable INTEGER NOT NULL DEFAULT 0,
			cpu_actionable INTEGER NOT NULL DEFAULT 0,
			memory_actionable INTEGER NOT NULL DEFAULT 0,
			reason_codes_json TEXT NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_workload_runs_lookup
			ON workload_runs(application, namespace, workload_name, generated_at);`,
		`CREATE TABLE IF NOT EXISTS learned_signals (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id INTEGER NOT NULL REFERENCES workload_runs(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			window TEXT NOT NULL,
			step TEXT NOT NULL,
			points INTEGER NOT NULL,
			current REAL NOT NULL,
			p50 REAL NOT NULL,
			p95 REAL NOT NULL,
			max REAL NOT NULL,
			current_vs_p95 REAL NOT NULL,
			current_vs_max REAL NOT NULL,
			classification TEXT NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_learned_signals_run
			ON learned_signals(run_id);`,
		`CREATE TABLE IF NOT EXISTS learned_decisions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id INTEGER NOT NULL REFERENCES workload_runs(id) ON DELETE CASCADE,
			subject TEXT NOT NULL,
			learned TEXT NOT NULL,
			observed TEXT NOT NULL,
			conclusion TEXT NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_learned_decisions_run
			ON learned_decisions(run_id);`,
		`CREATE TABLE IF NOT EXISTS forecast_scores (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id INTEGER NOT NULL REFERENCES workload_runs(id) ON DELETE CASCADE,
			signal TEXT NOT NULL,
			predicted REAL NOT NULL,
			observed REAL NOT NULL,
			absolute_percent_error REAL NOT NULL,
			bias_percent REAL NOT NULL,
			classification TEXT NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_forecast_scores_run
			ON forecast_scores(run_id);`,
		`CREATE INDEX IF NOT EXISTS idx_forecast_scores_lookup
			ON forecast_scores(signal, run_id);`,
		`CREATE TABLE IF NOT EXISTS convergence_observations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			application TEXT NOT NULL,
			namespace TEXT NOT NULL,
			workload_name TEXT NOT NULL,
			deployment TEXT NOT NULL,
			generated_at TEXT NOT NULL,
			git_branch TEXT NOT NULL,
			git_upstream TEXT NOT NULL,
			latest_proposal_commit TEXT NOT NULL,
			status TEXT NOT NULL,
			outcome TEXT NOT NULL,
			desired_replicas TEXT NOT NULL,
			live_replicas TEXT NOT NULL,
			desired_cpu_request TEXT NOT NULL,
			live_cpu_request TEXT NOT NULL,
			desired_memory_request TEXT NOT NULL,
			live_memory_request TEXT NOT NULL,
			reasons_json TEXT NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_convergence_observations_lookup
			ON convergence_observations(application, namespace, workload_name, generated_at);`,
		`CREATE TABLE IF NOT EXISTS proposal_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			application TEXT NOT NULL,
			namespace TEXT NOT NULL,
			workload_name TEXT NOT NULL,
			deployment TEXT NOT NULL,
			generated_at TEXT NOT NULL,
			proposal_kind TEXT NOT NULL,
			proposal_commit TEXT NOT NULL,
			proposal_patch_file TEXT NOT NULL,
			changes_count INTEGER NOT NULL,
			status TEXT NOT NULL DEFAULT 'created',
			message TEXT NOT NULL DEFAULT '',
			pushed INTEGER NOT NULL DEFAULT 0,
			remote TEXT NOT NULL DEFAULT '',
			push_ref TEXT NOT NULL DEFAULT '',
			errors_json TEXT NOT NULL DEFAULT '[]'
		);`,
		`CREATE INDEX IF NOT EXISTS idx_proposal_events_lookup
			ON proposal_events(application, namespace, workload_name, generated_at);`,
		`CREATE TABLE IF NOT EXISTS proposal_batch_items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			application TEXT NOT NULL,
			namespace TEXT NOT NULL,
			workload_name TEXT NOT NULL,
			deployment TEXT NOT NULL,
			source_file TEXT NOT NULL,
			resource TEXT NOT NULL,
			patch_plan_json TEXT NOT NULL,
			first_seen_at TEXT NOT NULL,
			last_seen_at TEXT NOT NULL,
			UNIQUE(application, namespace, workload_name)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_proposal_batch_items_lookup
			ON proposal_batch_items(application, namespace, workload_name, first_seen_at);`,
		`CREATE TABLE IF NOT EXISTS availability_recovery_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			application TEXT NOT NULL,
			namespace TEXT NOT NULL,
			workload_name TEXT NOT NULL,
			deployment TEXT NOT NULL,
			pod_name TEXT NOT NULL,
			pod_uid TEXT NOT NULL,
			attempted_at TEXT NOT NULL,
			action TEXT NOT NULL,
			status TEXT NOT NULL,
			message TEXT NOT NULL DEFAULT ''
		);`,
		`CREATE INDEX IF NOT EXISTS idx_availability_recovery_events_lookup
			ON availability_recovery_events(application, namespace, workload_name, attempted_at);`,
		`CREATE TABLE IF NOT EXISTS seasonal_signal_observations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			application TEXT NOT NULL,
			namespace TEXT NOT NULL,
			workload_name TEXT NOT NULL,
			deployment TEXT NOT NULL,
			generated_at TEXT NOT NULL,
			hour_of_day INTEGER NOT NULL,
			day_of_week INTEGER NOT NULL,
			day_type TEXT NOT NULL,
			signal TEXT NOT NULL,
			value REAL NOT NULL,
			traffic_band TEXT NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_seasonal_signal_lookup
			ON seasonal_signal_observations(application, namespace, workload_name, signal, day_type, hour_of_day);`,
		`CREATE INDEX IF NOT EXISTS idx_seasonal_latency_band_lookup
			ON seasonal_signal_observations(application, namespace, workload_name, signal, traffic_band, day_type, hour_of_day);`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate state db: %w", err)
		}
	}
	if err := s.ensureColumn(ctx, "workload_runs", "recommendation_mode", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "workload_runs", "recommendation_actionable", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "workload_runs", "outcome_status", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "workload_runs", "replicas_actionable", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "workload_runs", "cpu_actionable", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "workload_runs", "memory_actionable", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "proposal_events", "status", "TEXT NOT NULL DEFAULT 'created'"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "proposal_events", "message", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "proposal_events", "pushed", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "proposal_events", "remote", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "proposal_events", "push_ref", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "proposal_events", "errors_json", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
		return err
	}
	return nil
}

func (s *Store) ensureColumn(ctx context.Context, table, column, definition string) error {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return fmt.Errorf("inspect table %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name string
		var columnType string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return fmt.Errorf("scan table %s columns: %w", table, err)
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition)); err != nil {
		return fmt.Errorf("add column %s.%s: %w", table, column, err)
	}
	return nil
}

func (s *Store) RecordObservation(ctx context.Context, report *analyzer.ObservationReport) error {
	for _, workload := range report.Workloads {
		reasons, err := json.Marshal(workload.Reasons)
		if err != nil {
			return fmt.Errorf("encode observation reasons: %w", err)
		}
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO convergence_observations (
				application,
				namespace,
				workload_name,
				deployment,
				generated_at,
				git_branch,
				git_upstream,
				latest_proposal_commit,
				status,
				outcome,
				desired_replicas,
				live_replicas,
				desired_cpu_request,
				live_cpu_request,
				desired_memory_request,
				live_memory_request,
				reasons_json
			)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			report.Application,
			workload.Namespace,
			workload.Name,
			workload.Deployment,
			report.GeneratedAt.Format(time.RFC3339Nano),
			report.Git.Branch,
			report.Git.Upstream,
			report.Git.LatestProposalCommit,
			workload.Status,
			workload.Outcome,
			workload.Desired.Replicas,
			workload.Live.Replicas,
			workload.Desired.CPURequest,
			workload.Live.CPURequest,
			workload.Desired.MemoryRequest,
			workload.Live.MemoryRequest,
			string(reasons),
		); err != nil {
			return fmt.Errorf("record convergence observation: %w", err)
		}
	}
	return nil
}

func (s *Store) summary(ctx context.Context, application string, observedAt time.Time, workload *analyzer.WorkloadReport) (*analyzer.PersistentLearning, error) {
	summary := &analyzer.PersistentLearning{
		Enabled: true,
		Message: "no prior persisted recommendations for this workload",
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM workload_runs
		WHERE application = ? AND namespace = ? AND workload_name = ?
	`, application, workload.Namespace, workload.Name)
	if err := row.Scan(&summary.PriorRecommendationRuns); err != nil {
		return nil, fmt.Errorf("read persisted run count: %w", err)
	}
	row = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM learned_signals ls
		JOIN workload_runs wr ON wr.id = ls.run_id
		WHERE wr.application = ? AND wr.namespace = ? AND wr.workload_name = ?
	`, application, workload.Namespace, workload.Name)
	if err := row.Scan(&summary.PriorSignalObservations); err != nil {
		return nil, fmt.Errorf("read persisted signal count: %w", err)
	}
	forecastAccuracy, err := s.forecastAccuracy(ctx, application, workload.Namespace, workload.Name)
	if err != nil {
		return nil, err
	}
	summary.ForecastAccuracy = forecastAccuracy
	seasonality, err := s.seasonality(ctx, application, observedAt, workload)
	if err != nil {
		return nil, err
	}
	summary.Seasonality = seasonality
	row = s.db.QueryRowContext(ctx, `
		SELECT
			generated_at,
			current_replicas,
			recommended_replicas,
			current_cpu_request,
			recommended_cpu_request,
			current_memory_request,
				recommended_memory_request,
				confidence,
				recommendation_mode,
				recommendation_actionable,
				outcome_status,
				replicas_actionable,
				cpu_actionable,
				memory_actionable,
				reason_codes_json
			FROM workload_runs
		WHERE application = ? AND namespace = ? AND workload_name = ?
		ORDER BY generated_at DESC, id DESC
		LIMIT 1
	`, application, workload.Namespace, workload.Name)
	var generatedAtRaw string
	var reasonCodesJSON string
	previous := priorRun{}
	err = row.Scan(
		&generatedAtRaw,
		&previous.CurrentReplicas,
		&previous.RecommendedReplicas,
		&previous.CurrentCPURequest,
		&previous.RecommendedCPURequest,
		&previous.CurrentMemoryRequest,
		&previous.RecommendedMemoryRequest,
		&previous.Confidence,
		&previous.Mode,
		&previous.Actionable,
		&previous.OutcomeStatus,
		&previous.ReplicasActionable,
		&previous.CPUActionable,
		&previous.MemoryActionable,
		&reasonCodesJSON,
	)
	if err == nil {
		previous.ReasonCodes = decodeReasonCodes(reasonCodesJSON)
		if parsed, parseErr := time.Parse(time.RFC3339Nano, generatedAtRaw); parseErr == nil {
			summary.LastObservedAt = &parsed
			previous.GeneratedAt = &parsed
		}
		summary.LastRecommendedReplicas = previous.RecommendedReplicas
		summary.LastRecommendedCPURequest = previous.RecommendedCPURequest
		summary.LastRecommendedMemoryRequest = previous.RecommendedMemoryRequest
		summary.LastOutcome = classifyOutcome(previous, workload)
		summary.Message = "prior persisted recommendations loaded"
		return summary, nil
	}
	if err == sql.ErrNoRows {
		return summary, nil
	}
	return nil, fmt.Errorf("read persisted latest run: %w", err)
}

func (s *Store) recentRuns(ctx context.Context, application, namespace, workload string, limit int) ([]priorRun, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			generated_at,
			current_replicas,
			recommended_replicas,
			current_cpu_request,
			recommended_cpu_request,
			current_memory_request,
				recommended_memory_request,
				confidence,
				recommendation_mode,
				recommendation_actionable,
				outcome_status,
				replicas_actionable,
				cpu_actionable,
				memory_actionable,
				reason_codes_json
			FROM workload_runs
		WHERE application = ? AND namespace = ? AND workload_name = ?
		ORDER BY generated_at DESC, id DESC
		LIMIT ?
	`, application, namespace, workload, limit)
	if err != nil {
		return nil, fmt.Errorf("read recent persisted runs: %w", err)
	}
	defer rows.Close()
	var runs []priorRun
	for rows.Next() {
		var generatedAt string
		var reasonCodesJSON string
		run := priorRun{}
		if err := rows.Scan(
			&generatedAt,
			&run.CurrentReplicas,
			&run.RecommendedReplicas,
			&run.CurrentCPURequest,
			&run.RecommendedCPURequest,
			&run.CurrentMemoryRequest,
			&run.RecommendedMemoryRequest,
			&run.Confidence,
			&run.Mode,
			&run.Actionable,
			&run.OutcomeStatus,
			&run.ReplicasActionable,
			&run.CPUActionable,
			&run.MemoryActionable,
			&reasonCodesJSON,
		); err != nil {
			return nil, fmt.Errorf("scan recent persisted run: %w", err)
		}
		run.ReasonCodes = decodeReasonCodes(reasonCodesJSON)
		if parsed, parseErr := time.Parse(time.RFC3339Nano, generatedAt); parseErr == nil {
			run.GeneratedAt = &parsed
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recent persisted runs: %w", err)
	}
	return runs, nil
}

type priorRun struct {
	GeneratedAt              *time.Time
	Mode                     string
	Actionable               bool
	OutcomeStatus            string
	ReplicasActionable       bool
	CPUActionable            bool
	MemoryActionable         bool
	CurrentReplicas          int32
	RecommendedReplicas      int32
	CurrentCPURequest        string
	RecommendedCPURequest    string
	CurrentMemoryRequest     string
	RecommendedMemoryRequest string
	Confidence               float64
	ReasonCodes              []string
}

type forecastScore struct {
	Signal               string
	Predicted            float64
	Observed             float64
	AbsolutePercentError float64
	BiasPercent          float64
	Classification       string
}

func (s *Store) forecastAccuracy(ctx context.Context, application, namespace, workload string) (*analyzer.ForecastAccuracy, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			fs.signal,
			COUNT(*),
			AVG(fs.absolute_percent_error),
			AVG(fs.bias_percent)
		FROM forecast_scores fs
		JOIN workload_runs wr ON wr.id = fs.run_id
		WHERE wr.application = ? AND wr.namespace = ? AND wr.workload_name = ?
		GROUP BY fs.signal
		ORDER BY fs.signal
	`, application, namespace, workload)
	if err != nil {
		return nil, fmt.Errorf("read forecast accuracy: %w", err)
	}
	defer rows.Close()

	accuracy := &analyzer.ForecastAccuracy{
		Enabled: true,
		Message: "no prior forecast scores for this workload",
	}
	for rows.Next() {
		var score analyzer.ForecastAccuracyScore
		if err := rows.Scan(&score.Signal, &score.Samples, &score.MeanAbsolutePercentError, &score.MeanBiasPercent); err != nil {
			return nil, fmt.Errorf("scan forecast accuracy: %w", err)
		}
		score.Classification = forecastAccuracyClassification(score.MeanAbsolutePercentError, score.MeanBiasPercent)
		accuracy.Signals = append(accuracy.Signals, score)
		accuracy.Samples += score.Samples
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate forecast accuracy: %w", err)
	}
	if accuracy.Samples == 0 {
		return accuracy, nil
	}

	accuracy.ConfidenceAdjustment = forecastConfidenceAdjustment(accuracy.Signals)
	accuracy.WasteReductionBias = wasteReductionBias(accuracy.Signals)
	accuracy.Message = "forecast accuracy loaded from prior recommendation outcomes"
	return accuracy, nil
}

func scoreForecastAccuracy(previous priorRun, current *analyzer.WorkloadReport) []forecastScore {
	var scores []forecastScore
	if predicted, ok := reasonFloat(previous.ReasonCodes, "traffic_forecast:"); ok {
		if observed, hasObserved := signalSample(current.MetricSignals, "request_rate"); hasObserved {
			scores = append(scores, newForecastScore("request_rate", predicted, observed))
		}
	}
	if predicted, ok := reasonFloatByPrefix(previous.ReasonCodes, "cpu_usage_p95_"); ok {
		if observed, hasObserved := signalSample(current.MetricSignals, "cpu_usage"); hasObserved {
			scores = append(scores, newForecastScore("cpu_usage", predicted, observed))
		}
	}
	if predicted, ok := reasonMemoryByPrefix(previous.ReasonCodes, "memory_working_set_p95_"); ok {
		if observed, hasObserved := signalSample(current.MetricSignals, "memory_working_set"); hasObserved {
			scores = append(scores, newForecastScore("memory_working_set", predicted, observed))
		}
	}
	if previous.RecommendedReplicas > 0 {
		observed := float64(current.ReadyReplicas)
		if available, hasAvailable := signalSample(current.MetricSignals, "available_replicas"); hasAvailable {
			observed = available
		}
		scores = append(scores, newForecastScore("replicas", float64(previous.RecommendedReplicas), observed))
	}
	return scores
}

func newForecastScore(signal string, predicted, observed float64) forecastScore {
	baseline := math.Abs(observed)
	if baseline < 1 {
		baseline = 1
	}
	bias := (predicted - observed) / baseline
	absolute := math.Abs(predicted-observed) / baseline
	return forecastScore{
		Signal:               signal,
		Predicted:            predicted,
		Observed:             observed,
		AbsolutePercentError: absolute,
		BiasPercent:          bias,
		Classification:       forecastAccuracyClassification(absolute, bias),
	}
}

func forecastAccuracyClassification(meanAbsolutePercentError, meanBiasPercent float64) string {
	switch {
	case meanBiasPercent > 0.20:
		return "overestimated"
	case meanBiasPercent < -0.20:
		return "underestimated"
	case meanAbsolutePercentError <= 0.15:
		return "accurate"
	case meanAbsolutePercentError <= 0.35:
		return "usable"
	default:
		return "noisy"
	}
}

func forecastConfidenceAdjustment(scores []analyzer.ForecastAccuracyScore) float64 {
	if len(scores) == 0 {
		return 0
	}
	var adjustment float64
	for _, score := range scores {
		if score.Samples < 3 {
			continue
		}
		switch score.Classification {
		case "accurate":
			adjustment += 0.02
		case "usable":
			adjustment += 0.005
		default:
			adjustment -= math.Min(0.08, score.MeanAbsolutePercentError*0.08)
		}
		if score.MeanBiasPercent < -0.20 {
			adjustment -= math.Min(0.06, math.Abs(score.MeanBiasPercent)*0.06)
		}
	}
	return clampFloat(adjustment, -0.25, 0.08)
}

func wasteReductionBias(scores []analyzer.ForecastAccuracyScore) string {
	var over, under int
	for _, score := range scores {
		if score.Samples < 3 {
			continue
		}
		if score.Signal != "request_rate" && score.Signal != "cpu_usage" && score.Signal != "memory_working_set" {
			continue
		}
		switch score.Classification {
		case "overestimated":
			over++
		case "underestimated":
			under++
		}
	}
	switch {
	case over > under:
		return "favor_waste_reduction"
	case under > over:
		return "preserve_headroom"
	default:
		return "neutral"
	}
}

func applyForecastAccuracyFeedback(workload *analyzer.WorkloadReport, accuracy *analyzer.ForecastAccuracy) {
	if accuracy == nil || accuracy.Samples == 0 {
		return
	}
	rec := &workload.Recommendation
	originalConfidence := rec.Confidence
	rec.Confidence = clampFloat(rec.Confidence+accuracy.ConfidenceAdjustment, 0.05, 0.99)
	if rec.Confidence != originalConfidence {
		rec.ReasonCodes = append(rec.ReasonCodes, fmt.Sprintf("forecast_confidence_adjustment:%+.3f", accuracy.ConfidenceAdjustment))
	}

	cpuBias, hasCPUBias := signalBias(accuracy.Signals, "cpu_usage")
	if hasCPUBias {
		if adjusted, ok := adjustedCPUDecrease(rec.CurrentCPURequest, rec.RecommendedCPURequest, cpuBias); ok {
			rec.ReasonCodes = append(rec.ReasonCodes, fmt.Sprintf("cpu_request_forecast_bias_adjusted:%s->%s,bias=%.3f", rec.RecommendedCPURequest, adjusted, cpuBias))
			rec.RecommendedCPURequest = adjusted
		}
	}
	memoryBias, hasMemoryBias := signalBias(accuracy.Signals, "memory_working_set")
	if hasMemoryBias {
		if adjusted, ok := adjustedMemoryDecrease(rec.CurrentMemoryRequest, rec.RecommendedMemoryRequest, memoryBias); ok {
			rec.ReasonCodes = append(rec.ReasonCodes, fmt.Sprintf("memory_request_forecast_bias_adjusted:%s->%s,bias=%.3f", rec.RecommendedMemoryRequest, adjusted, memoryBias))
			rec.RecommendedMemoryRequest = adjusted
		}
	}
	if accuracy.WasteReductionBias != "" {
		rec.ReasonCodes = append(rec.ReasonCodes, "forecast_waste_reduction_bias:"+accuracy.WasteReductionBias)
		rec.Learning.Decisions = append(rec.Learning.Decisions, analyzer.LearnedDecision{
			Subject:    "forecast.accuracy",
			Learned:    fmt.Sprintf("samples=%d confidence_adjustment=%+.3f waste_bias=%s", accuracy.Samples, accuracy.ConfidenceAdjustment, accuracy.WasteReductionBias),
			Observed:   forecastAccuracyObserved(accuracy.Signals),
			Conclusion: forecastAccuracyConclusion(accuracy.WasteReductionBias),
		})
	}
}

func signalBias(scores []analyzer.ForecastAccuracyScore, signal string) (float64, bool) {
	for _, score := range scores {
		if score.Signal == signal && score.Samples >= 3 {
			return score.MeanBiasPercent, true
		}
	}
	return 0, false
}

func adjustedCPUDecrease(current, recommended string, bias float64) (string, bool) {
	adjusted, ok := adjustedResourceDecrease(current, recommended, bias)
	if !ok {
		return "", false
	}
	milli := int64(math.Ceil(adjusted*1000/10) * 10)
	if milli < 1 {
		milli = 1
	}
	return fmt.Sprintf("%dm", milli), true
}

func adjustedMemoryDecrease(current, recommended string, bias float64) (string, bool) {
	adjusted, ok := adjustedResourceDecrease(current, recommended, bias)
	if !ok {
		return "", false
	}
	mi := int64(math.Ceil(adjusted / (1024 * 1024)))
	if mi < 1 {
		mi = 1
	}
	return fmt.Sprintf("%dMi", mi), true
}

func adjustedResourceDecrease(current, recommended string, bias float64) (float64, bool) {
	if math.Abs(bias) < 0.20 {
		return 0, false
	}
	currentQuantity, ok := parseQuantity(current)
	if !ok {
		return 0, false
	}
	recommendedQuantity, ok := parseQuantity(recommended)
	if !ok {
		return 0, false
	}
	if recommendedQuantity.Cmp(currentQuantity) >= 0 {
		return 0, false
	}
	currentValue := currentQuantity.AsApproximateFloat64()
	recommendedValue := recommendedQuantity.AsApproximateFloat64()
	change := currentValue - recommendedValue
	biasWeight := math.Min(math.Abs(bias), 0.50)
	adjusted := recommendedValue
	if bias > 0 {
		adjusted = recommendedValue - (change * biasWeight * 0.5)
	} else {
		adjusted = recommendedValue + (change * biasWeight)
	}
	if adjusted <= 0 {
		adjusted = recommendedValue
	}
	return adjusted, true
}

func forecastAccuracyObserved(scores []analyzer.ForecastAccuracyScore) string {
	var parts []string
	for _, score := range scores {
		parts = append(parts, fmt.Sprintf("%s:mape=%.1f%% bias=%+.1f%% samples=%d", score.Signal, score.MeanAbsolutePercentError*100, score.MeanBiasPercent*100, score.Samples))
	}
	if len(parts) == 0 {
		return "no scored forecasts yet"
	}
	return strings.Join(parts, " ")
}

func forecastAccuracyConclusion(bias string) string {
	switch bias {
	case "favor_waste_reduction":
		return "prior forecasts trend high; allow more assertive gated waste reduction"
	case "preserve_headroom":
		return "prior forecasts trend low; preserve additional headroom and reduce confidence"
	default:
		return "forecast error is not directional enough to change resource posture"
	}
}

func decodeReasonCodes(raw string) []string {
	var reasons []string
	if err := json.Unmarshal([]byte(raw), &reasons); err != nil {
		return nil
	}
	return reasons
}

func reasonFloat(reasons []string, prefix string) (float64, bool) {
	for _, reason := range reasons {
		if !strings.HasPrefix(reason, prefix) {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimPrefix(reason, prefix), 64)
		return value, err == nil
	}
	return 0, false
}

func reasonFloatByPrefix(reasons []string, prefix string) (float64, bool) {
	for _, reason := range reasons {
		if !strings.HasPrefix(reason, prefix) {
			continue
		}
		index := strings.LastIndex(reason, ":")
		if index < 0 || index == len(reason)-1 {
			return 0, false
		}
		value, err := strconv.ParseFloat(reason[index+1:], 64)
		return value, err == nil
	}
	return 0, false
}

func reasonMemoryByPrefix(reasons []string, prefix string) (float64, bool) {
	for _, reason := range reasons {
		if !strings.HasPrefix(reason, prefix) {
			continue
		}
		index := strings.LastIndex(reason, ":")
		if index < 0 || index == len(reason)-1 {
			return 0, false
		}
		return parseByteValue(reason[index+1:])
	}
	return 0, false
}

func parseByteValue(value string) (float64, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, false
	}
	units := map[string]float64{
		"B":  1,
		"Ki": 1024,
		"Mi": 1024 * 1024,
		"Gi": 1024 * 1024 * 1024,
		"Ti": 1024 * 1024 * 1024 * 1024,
		"Pi": 1024 * 1024 * 1024 * 1024 * 1024,
	}
	for suffix, multiplier := range units {
		if strings.HasSuffix(trimmed, suffix) {
			number := strings.TrimSpace(strings.TrimSuffix(trimmed, suffix))
			parsed, err := strconv.ParseFloat(number, 64)
			if err != nil {
				return 0, false
			}
			return parsed * multiplier, true
		}
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	return parsed, err == nil
}

func classifyOutcome(previous priorRun, current *analyzer.WorkloadReport) *analyzer.RecommendationOutcome {
	outcome := &analyzer.RecommendationOutcome{
		PreviousObservedAt:               previous.GeneratedAt,
		PreviousCurrentReplicas:          previous.CurrentReplicas,
		PreviousRecommendedReplicas:      previous.RecommendedReplicas,
		PreviousCurrentCPURequest:        previous.CurrentCPURequest,
		PreviousRecommendedCPURequest:    previous.RecommendedCPURequest,
		PreviousCurrentMemoryRequest:     previous.CurrentMemoryRequest,
		PreviousRecommendedMemoryRequest: previous.RecommendedMemoryRequest,
		CurrentReplicas:                  current.Recommendation.CurrentReplicas,
		CurrentCPURequest:                current.Recommendation.CurrentCPURequest,
		CurrentMemoryRequest:             current.Recommendation.CurrentMemoryRequest,
	}

	replicasChanged := previous.ReplicasActionable && previous.CurrentReplicas != previous.RecommendedReplicas
	cpuChanged := previous.CPUActionable && previous.CurrentCPURequest != previous.RecommendedCPURequest
	memoryChanged := previous.MemoryActionable && previous.CurrentMemoryRequest != previous.RecommendedMemoryRequest
	if !replicasChanged && !cpuChanged && !memoryChanged {
		if previous.Actionable {
			outcome.Status = "no_change_recommended"
			outcome.Details = append(outcome.Details, "previous recommendation held all actionable fields")
		} else {
			outcome.Status = "no_action_taken"
			outcome.Details = append(outcome.Details, "previous recommendation had no actionable field changes")
		}
		return outcome
	}

	expected := 0
	applied := 0
	if replicasChanged {
		expected++
		if current.Recommendation.CurrentReplicas == previous.RecommendedReplicas {
			applied++
			outcome.Details = append(outcome.Details, "replicas_applied")
		} else {
			outcome.Details = append(outcome.Details, "replicas_not_applied")
		}
	}
	if cpuChanged {
		expected++
		if current.Recommendation.CurrentCPURequest == previous.RecommendedCPURequest {
			applied++
			outcome.Details = append(outcome.Details, "cpu_request_applied")
		} else {
			outcome.Details = append(outcome.Details, "cpu_request_not_applied")
		}
	}
	if memoryChanged {
		expected++
		if current.Recommendation.CurrentMemoryRequest == previous.RecommendedMemoryRequest {
			applied++
			outcome.Details = append(outcome.Details, "memory_request_applied")
		} else {
			outcome.Details = append(outcome.Details, "memory_request_not_applied")
		}
	}

	switch {
	case applied == 0:
		if previous.recommendationMode() == "dry-run" {
			outcome.Status = "dry_run_not_applied"
			outcome.Details = append(outcome.Details, "controller_is_not_writing_git_or_patching_kubernetes")
		} else {
			outcome.Status = "not_applied"
		}
	case applied < expected:
		outcome.Status = "partially_applied"
	case workloadUnhealthy(current):
		outcome.Status = "too_aggressive"
		outcome.Details = append(outcome.Details, "current_workload_unhealthy_after_applied_recommendation")
	case stillMovingSameDirection(previous, current):
		outcome.Status = "too_conservative"
		outcome.Details = append(outcome.Details, "current_recommendation_continues_same_direction_after_applied_recommendation")
	default:
		outcome.Status = "applied_successful"
	}
	return outcome
}

func (run priorRun) recommendationMode() string {
	if strings.TrimSpace(run.Mode) == "" {
		return "dry-run"
	}
	return run.Mode
}

func workloadUnhealthy(workload *analyzer.WorkloadReport) bool {
	if workload.MetricsCondition != "healthy" {
		return true
	}
	if workload.ReadyReplicas < workload.Replicas {
		return true
	}
	for _, signal := range workload.MetricSignals {
		if signal.Anomaly.State == "warning" || signal.Anomaly.State == "critical" {
			return true
		}
	}
	return false
}

func hasActiveSignalAnomaly(workload *analyzer.WorkloadReport, name string) bool {
	for _, signal := range workload.MetricSignals {
		if signal.Name == name && (signal.Anomaly.State == "warning" || signal.Anomaly.State == "critical") {
			return true
		}
	}
	return false
}

func stillMovingSameDirection(previous priorRun, current *analyzer.WorkloadReport) bool {
	return sameIntDirection(previous.CurrentReplicas, previous.RecommendedReplicas, current.Recommendation.CurrentReplicas, current.Recommendation.RecommendedReplicas) ||
		sameResourceDirection(previous.CurrentCPURequest, previous.RecommendedCPURequest, current.Recommendation.CurrentCPURequest, current.Recommendation.RecommendedCPURequest) ||
		sameResourceDirection(previous.CurrentMemoryRequest, previous.RecommendedMemoryRequest, current.Recommendation.CurrentMemoryRequest, current.Recommendation.RecommendedMemoryRequest)
}

func sameIntDirection(previousCurrent, previousRecommended, current, recommended int32) bool {
	return (previousRecommended > previousCurrent && recommended > current) || (previousRecommended < previousCurrent && recommended < current)
}

func sameResourceDirection(previousCurrent, previousRecommended, current, recommended string) bool {
	previousDirection, ok := resourceDirection(previousCurrent, previousRecommended)
	if !ok {
		return false
	}
	currentDirection, ok := resourceDirection(current, recommended)
	if !ok {
		return false
	}
	return previousDirection == currentDirection
}

func resourceDirection(current, recommended string) (int, bool) {
	if current == "" || recommended == "" || current == recommended {
		return 0, false
	}
	currentQuantity, err := resource.ParseQuantity(current)
	if err != nil {
		return 0, false
	}
	recommendedQuantity, err := resource.ParseQuantity(recommended)
	if err != nil {
		return 0, false
	}
	return recommendedQuantity.Cmp(currentQuantity), true
}

func stabilizeRecommendation(workload *analyzer.WorkloadReport, summary *analyzer.PersistentLearning) {
	if summary == nil || summary.PriorRecommendationRuns == 0 {
		return
	}
	rec := &workload.Recommendation
	if stableResourceRecommendation(rec.CurrentCPURequest, rec.RecommendedCPURequest, summary.LastRecommendedCPURequest, "10m", 0.05) {
		rec.RecommendedCPURequest = summary.LastRecommendedCPURequest
		rec.ReasonCodes = append(rec.ReasonCodes, "cpu_request_stabilized_to_prior:"+summary.LastRecommendedCPURequest)
	}
	if stableResourceRecommendation(rec.CurrentMemoryRequest, rec.RecommendedMemoryRequest, summary.LastRecommendedMemoryRequest, "16Mi", 0.05) {
		rec.RecommendedMemoryRequest = summary.LastRecommendedMemoryRequest
		rec.ReasonCodes = append(rec.ReasonCodes, "memory_request_stabilized_to_prior:"+summary.LastRecommendedMemoryRequest)
	}
}

func stableResourceRecommendation(current, recommended, previousRecommended, minAbsolute string, maxRelative float64) bool {
	if current == "" || recommended == "" || previousRecommended == "" {
		return false
	}
	if recommended == current || previousRecommended == current || recommended == previousRecommended {
		return false
	}
	currentQuantity, ok := parseQuantity(current)
	if !ok {
		return false
	}
	recommendedQuantity, ok := parseQuantity(recommended)
	if !ok {
		return false
	}
	previousQuantity, ok := parseQuantity(previousRecommended)
	if !ok {
		return false
	}
	minAbsoluteQuantity, ok := parseQuantity(minAbsolute)
	if !ok {
		return false
	}
	if recommendedQuantity.Cmp(currentQuantity) != previousQuantity.Cmp(currentQuantity) {
		return false
	}
	delta := quantityAbsDiff(recommendedQuantity, previousQuantity)
	relativeThreshold := quantityAbs(currentQuantity) * maxRelative
	threshold := maxFloat64(quantityAbs(minAbsoluteQuantity), relativeThreshold)
	return delta <= threshold
}

func parseQuantity(value string) (resource.Quantity, bool) {
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return resource.Quantity{}, false
	}
	return quantity, true
}

func quantityAbsDiff(left, right resource.Quantity) float64 {
	delta := left.AsApproximateFloat64() - right.AsApproximateFloat64()
	if delta < 0 {
		return -delta
	}
	return delta
}

func quantityAbs(value resource.Quantity) float64 {
	raw := value.AsApproximateFloat64()
	if raw < 0 {
		return -raw
	}
	return raw
}

func maxFloat64(left, right float64) float64 {
	if left > right {
		return left
	}
	return right
}

func clampFloat(value, minValue, maxValue float64) float64 {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func signalSample(signals []analyzer.SignalReport, name string) (float64, bool) {
	for _, signal := range signals {
		if signal.Name == name && signal.Sample != nil {
			return *signal.Sample, true
		}
	}
	return 0, false
}

func evaluateStability(workload *analyzer.WorkloadReport, recent []priorRun) *analyzer.RecommendationStability {
	stability := &analyzer.RecommendationStability{
		Replicas: evaluateReplicaGate(workload, recent),
		CPU:      evaluateResourceGate("cpu", workload.Recommendation.CurrentCPURequest, workload.Recommendation.RecommendedCPURequest, recent, resourceCPU),
		Memory:   evaluateResourceGate("memory", workload.Recommendation.CurrentMemoryRequest, workload.Recommendation.RecommendedMemoryRequest, recent, resourceMemory),
	}
	if (stability.Memory.Status == "pending_stability" || stability.Memory.Status == "stable") && hasActiveSignalAnomaly(workload, "memory_working_set") {
		stability.Memory = analyzer.StabilityGate{Status: "blocked", Observed: stability.Memory.Observed, Required: stability.Memory.Required, Reason: "memory decrease blocked by active memory anomaly"}
	}
	stability.Actionable = gateActionable(stability.Replicas) && gateActionable(stability.CPU) && gateActionable(stability.Memory)
	return stability
}

func applyAvailabilityRecoveryStabilityGate(workload *analyzer.WorkloadReport) {
	if workload == nil || !workload.Recommendation.AvailabilityRecovery || workload.Recommendation.Stability == nil {
		return
	}
	recommendation := workload.Recommendation
	stability := workload.Recommendation.Stability
	if recommendation.RecommendedReplicas > recommendation.CurrentReplicas {
		stability.Replicas = analyzer.StabilityGate{Status: "stable", Observed: 1, Required: 1, Reason: "replica increase restores unavailable workload capacity"}
	}
	if direction, ok := resourceDirection(recommendation.CurrentCPURequest, recommendation.RecommendedCPURequest); ok && direction > 0 {
		stability.CPU = analyzer.StabilityGate{Status: "stable", Observed: 1, Required: 1, Reason: "cpu increase supports availability recovery"}
	}
	if direction, ok := resourceDirection(recommendation.CurrentMemoryRequest, recommendation.RecommendedMemoryRequest); ok && direction > 0 {
		stability.Memory = analyzer.StabilityGate{Status: "stable", Observed: 1, Required: 1, Reason: "memory increase supports availability recovery"}
	}
	stability.Actionable = gateActionable(stability.Replicas) && gateActionable(stability.CPU) && gateActionable(stability.Memory)
}

func applyOutcomeSafetyGate(stability *analyzer.RecommendationStability, outcome *analyzer.RecommendationOutcome, currentMode string) {
	if stability == nil || outcome == nil {
		return
	}
	if outcome.Status == "dry_run_not_applied" && currentMode != "dry-run" {
		return
	}
	reason := outcomeBlockReason(outcome.Status)
	if reason == "" {
		return
	}
	block := analyzer.StabilityGate{Status: "blocked", Reason: reason}
	stability.Replicas = block
	stability.CPU = block
	stability.Memory = block
	stability.Actionable = false
}

const postApplyObservationCooldownRuns = 3

func applyRecentOutcomeCooldownGate(stability *analyzer.RecommendationStability, recent []priorRun, currentMode string) {
	if stability == nil {
		return
	}
	for index, run := range recent {
		if run.OutcomeStatus == "" {
			continue
		}
		if run.OutcomeStatus == "dry_run_not_applied" && currentMode != "dry-run" {
			continue
		}
		reason := outcomeBlockReason(run.OutcomeStatus)
		if reason == "" {
			continue
		}
		block := analyzer.StabilityGate{
			Status: "blocked",
			Reason: fmt.Sprintf("%s; observation cooldown %d/%d", reason, index+1, postApplyObservationCooldownRuns),
		}
		stability.Replicas = block
		stability.CPU = block
		stability.Memory = block
		stability.Actionable = false
		return
	}
}

func outcomeBlockReason(status string) string {
	switch status {
	case "dry_run_not_applied":
		return "previous dry-run recommendation not applied"
	case "not_applied":
		return "previous recommendation has not been applied yet"
	case "partially_applied":
		return "previous recommendation is only partially applied"
	case "too_aggressive":
		return "previous recommendation left the workload unhealthy"
	case "too_conservative":
		return "previous recommendation still needs post-apply observation before another change"
	default:
		return ""
	}
}

type resourceKind string

const (
	resourceCPU    resourceKind = "cpu"
	resourceMemory resourceKind = "memory"
)

func evaluateResourceGate(name, current, recommended string, recent []priorRun, kind resourceKind) analyzer.StabilityGate {
	direction, ok := resourceDirection(current, recommended)
	if !ok {
		return analyzer.StabilityGate{Status: "hold", Observed: 0, Required: 0, Reason: name + " recommendation holds current request"}
	}
	if direction > 0 {
		return analyzer.StabilityGate{Status: "stable", Observed: 1, Required: 1, Reason: name + " increase is actionable for current pressure"}
	}
	const required = 3
	observed := 1
	for _, run := range recent {
		var priorCurrent, priorRecommended string
		if kind == resourceCPU {
			priorCurrent = run.CurrentCPURequest
			priorRecommended = run.RecommendedCPURequest
		} else {
			priorCurrent = run.CurrentMemoryRequest
			priorRecommended = run.RecommendedMemoryRequest
		}
		priorDirection, ok := resourceDirection(priorCurrent, priorRecommended)
		if !ok || priorDirection != direction {
			break
		}
		observed++
	}
	if observed >= required {
		return analyzer.StabilityGate{Status: "stable", Observed: observed, Required: required, Reason: name + " decrease met consecutive-run gate"}
	}
	return analyzer.StabilityGate{Status: "pending_stability", Observed: observed, Required: required, Reason: name + " decrease needs more consecutive evidence"}
}

func evaluateReplicaGate(workload *analyzer.WorkloadReport, recent []priorRun) analyzer.StabilityGate {
	rec := workload.Recommendation
	switch {
	case rec.RecommendedReplicas == rec.CurrentReplicas:
		return analyzer.StabilityGate{Status: "hold", Observed: 0, Required: 0, Reason: "replica recommendation holds current count"}
	case rec.RecommendedReplicas > rec.CurrentReplicas:
		if hasReasonPrefix(rec.ReasonCodes, "request_rate_anomaly_critical:") || hasReason(rec.ReasonCodes, "traffic_scale_up_allowed:true") {
			return analyzer.StabilityGate{Status: "stable", Observed: 1, Required: 1, Reason: "replica increase driven by urgent traffic pressure"}
		}
		const required = 3
		observed := 1 + consecutiveReplicaDirection(recent, 1)
		if observed >= required {
			return analyzer.StabilityGate{Status: "stable", Observed: observed, Required: required, Reason: "replica increase met consecutive-run gate"}
		}
		return analyzer.StabilityGate{Status: "pending_stability", Observed: observed, Required: required, Reason: "replica increase needs more consecutive evidence"}
	default:
		if hasReasonPrefix(rec.ReasonCodes, "availability_replica_floor:") || hasReasonPrefix(rec.ReasonCodes, "pdb_replica_floor:") || hasReasonPrefix(rec.ReasonCodes, "scale_down_blocked") {
			return analyzer.StabilityGate{Status: "blocked", Observed: 0, Required: 3, Reason: "replica decrease conflicts with safety floor or scale-down block"}
		}
		if workloadUnhealthy(workload) {
			return analyzer.StabilityGate{Status: "blocked", Observed: 0, Required: 3, Reason: "replica decrease blocked while workload is unhealthy or anomalous"}
		}
		const required = 3
		observed := 1 + consecutiveReplicaDirection(recent, -1)
		if observed >= required {
			return analyzer.StabilityGate{Status: "stable", Observed: observed, Required: required, Reason: "replica decrease met consecutive-run gate"}
		}
		return analyzer.StabilityGate{Status: "pending_stability", Observed: observed, Required: required, Reason: "replica decrease needs more consecutive evidence"}
	}
}

func consecutiveReplicaDirection(recent []priorRun, direction int) int {
	count := 0
	for _, run := range recent {
		priorDirection := 0
		switch {
		case run.RecommendedReplicas > run.CurrentReplicas:
			priorDirection = 1
		case run.RecommendedReplicas < run.CurrentReplicas:
			priorDirection = -1
		}
		if priorDirection != direction {
			break
		}
		count++
	}
	return count
}

func gateActionable(gate analyzer.StabilityGate) bool {
	return gate.Status == "stable" || gate.Status == "hold"
}

func hasReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}

func hasReasonPrefix(reasons []string, prefix string) bool {
	for _, reason := range reasons {
		if len(reason) >= len(prefix) && reason[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

func (s *Store) recordWorkload(ctx context.Context, application string, generatedAt time.Time, workload *analyzer.WorkloadReport, scores []forecastScore) error {
	reasons, err := json.Marshal(workload.Recommendation.ReasonCodes)
	if err != nil {
		return fmt.Errorf("encode reason codes: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin state tx: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO workload_runs (
			application,
			namespace,
			workload_name,
			deployment,
			generated_at,
			current_replicas,
			recommended_replicas,
			current_cpu_request,
			recommended_cpu_request,
			current_memory_request,
			recommended_memory_request,
				confidence,
				blocked,
				recommendation_mode,
				recommendation_actionable,
				outcome_status,
				replicas_actionable,
				cpu_actionable,
				memory_actionable,
				reason_codes_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		application,
		workload.Namespace,
		workload.Name,
		workload.Deployment,
		generatedAt.Format(time.RFC3339Nano),
		workload.Recommendation.CurrentReplicas,
		workload.Recommendation.RecommendedReplicas,
		workload.Recommendation.CurrentCPURequest,
		workload.Recommendation.RecommendedCPURequest,
		workload.Recommendation.CurrentMemoryRequest,
		workload.Recommendation.RecommendedMemoryRequest,
		workload.Recommendation.Confidence,
		boolInt(workload.Recommendation.Blocked),
		workload.Recommendation.Mode,
		boolInt(recommendationActionable(workload.Recommendation)),
		recommendationOutcomeStatus(workload.Recommendation),
		boolInt(replicasActionable(workload.Recommendation)),
		boolInt(cpuActionable(workload.Recommendation)),
		boolInt(memoryActionable(workload.Recommendation)),
		string(reasons),
	)
	if err != nil {
		return fmt.Errorf("insert workload run: %w", err)
	}
	runID, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("read workload run id: %w", err)
	}
	for _, signal := range workload.Recommendation.Learning.Signals {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO learned_signals (
				run_id,
				name,
				window,
				step,
				points,
				current,
				p50,
				p95,
				max,
				current_vs_p95,
				current_vs_max,
				classification
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			runID,
			signal.Name,
			signal.Window,
			signal.Step,
			signal.Points,
			sqlFinite(signal.Current),
			sqlFinite(signal.P50),
			sqlFinite(signal.P95),
			sqlFinite(signal.Max),
			sqlFinite(signal.CurrentVsP95),
			sqlFinite(signal.CurrentVsMax),
			signal.Classification,
		); err != nil {
			return fmt.Errorf("insert learned signal: %w", err)
		}
	}
	for _, decision := range workload.Recommendation.Learning.Decisions {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO learned_decisions (
				run_id,
				subject,
				learned,
				observed,
				conclusion
			) VALUES (?, ?, ?, ?, ?)
		`,
			runID,
			decision.Subject,
			decision.Learned,
			decision.Observed,
			decision.Conclusion,
		); err != nil {
			return fmt.Errorf("insert learned decision: %w", err)
		}
	}
	for _, score := range scores {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO forecast_scores (
				run_id,
				signal,
				predicted,
				observed,
				absolute_percent_error,
				bias_percent,
				classification
			) VALUES (?, ?, ?, ?, ?, ?, ?)
		`,
			runID,
			score.Signal,
			sqlFinite(score.Predicted),
			sqlFinite(score.Observed),
			sqlFinite(score.AbsolutePercentError),
			sqlFinite(score.BiasPercent),
			score.Classification,
		); err != nil {
			return fmt.Errorf("insert forecast score: %w", err)
		}
	}
	if err := s.recordSeasonalObservations(ctx, tx, application, generatedAt, workload); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit state tx: %w", err)
	}
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func recommendationActionable(recommendation analyzer.Recommendation) bool {
	return recommendation.Stability != nil && recommendation.Stability.Actionable
}

func recommendationOutcomeStatus(recommendation analyzer.Recommendation) string {
	if recommendation.Learning.Persistent == nil || recommendation.Learning.Persistent.LastOutcome == nil {
		return ""
	}
	return recommendation.Learning.Persistent.LastOutcome.Status
}

func replicasActionable(recommendation analyzer.Recommendation) bool {
	return recommendation.Stability != nil &&
		!recommendation.Blocked &&
		recommendation.CurrentReplicas != recommendation.RecommendedReplicas &&
		gateActionable(recommendation.Stability.Replicas)
}

func cpuActionable(recommendation analyzer.Recommendation) bool {
	return recommendation.Stability != nil &&
		!recommendation.Blocked &&
		recommendation.CurrentCPURequest != "" &&
		recommendation.RecommendedCPURequest != "" &&
		recommendation.CurrentCPURequest != recommendation.RecommendedCPURequest &&
		gateActionable(recommendation.Stability.CPU)
}

func memoryActionable(recommendation analyzer.Recommendation) bool {
	return recommendation.Stability != nil &&
		!recommendation.Blocked &&
		recommendation.CurrentMemoryRequest != "" &&
		recommendation.RecommendedMemoryRequest != "" &&
		recommendation.CurrentMemoryRequest != recommendation.RecommendedMemoryRequest &&
		gateActionable(recommendation.Stability.Memory)
}

func sqlFinite(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return value
}
