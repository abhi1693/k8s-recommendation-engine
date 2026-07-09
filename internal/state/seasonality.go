package state

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/abhi1693/k8s-recommendation-engine/internal/analyzer"
)

const seasonalMinimumPoints = 3

var seasonalSignals = map[string]struct{}{
	"request_rate":        {},
	"cpu_usage":           {},
	"memory_working_set":  {},
	"latency_p95":         {},
	"error_rate":          {},
	"concurrent_requests": {},
}

func (s *Store) seasonality(ctx context.Context, application string, generatedAt time.Time, workload *analyzer.WorkloadReport) (*analyzer.SeasonalityLearning, error) {
	dayType := seasonalDayType(generatedAt)
	dayOfWeek := int(generatedAt.Weekday())
	hour := generatedAt.Hour()
	seasonality := &analyzer.SeasonalityLearning{
		Enabled:            true,
		CurrentHour:        hour,
		CurrentDayOfWeek:   dayOfWeek,
		CurrentDayType:     dayType,
		CurrentTrafficBand: trafficBandForWorkload(workload),
		Message:            "no prior seasonal observations for this workload",
	}

	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM seasonal_signal_observations
		WHERE application = ? AND namespace = ? AND workload_name = ?
	`, application, workload.Namespace, workload.Name)
	if err := row.Scan(&seasonality.ObservationCount); err != nil {
		return nil, fmt.Errorf("read seasonal observation count: %w", err)
	}

	for _, signal := range []string{"request_rate", "cpu_usage", "memory_working_set"} {
		if summary, ok, err := s.seasonalSignalSummary(ctx, application, workload, signal, "same_day_type_hour", hour, nil, dayType); err != nil {
			return nil, err
		} else if ok {
			seasonality.Signals = append(seasonality.Signals, summary)
		}
		if summary, ok, err := s.seasonalSignalSummary(ctx, application, workload, signal, "same_day_of_week_hour", hour, &dayOfWeek, ""); err != nil {
			return nil, err
		} else if ok {
			seasonality.Signals = append(seasonality.Signals, summary)
		}
	}
	latencyBands, err := s.seasonalLatencyBands(ctx, application, workload, hour, dayType)
	if err != nil {
		return nil, err
	}
	seasonality.LatencyByTrafficBand = latencyBands
	if seasonality.ObservationCount > 0 {
		seasonality.Message = "seasonal observations loaded from hourly SQLite buckets"
	}
	return seasonality, nil
}

func (s *Store) seasonalSignalSummary(ctx context.Context, application string, workload *analyzer.WorkloadReport, signal, bucket string, hour int, dayOfWeek *int, dayType string) (analyzer.SeasonalSignal, bool, error) {
	query := `
		SELECT value
		FROM seasonal_signal_observations
		WHERE application = ?
			AND namespace = ?
			AND workload_name = ?
			AND signal = ?
			AND hour_of_day = ?
	`
	args := []any{application, workload.Namespace, workload.Name, signal, hour}
	if dayOfWeek != nil {
		query += " AND day_of_week = ?"
		args = append(args, *dayOfWeek)
	}
	if dayType != "" {
		query += " AND day_type = ?"
		args = append(args, dayType)
	}
	values, err := s.seasonalValues(ctx, query, args...)
	if err != nil {
		return analyzer.SeasonalSignal{}, false, err
	}
	stats, ok := seasonalStats(values)
	if !ok {
		return analyzer.SeasonalSignal{}, false, nil
	}
	summary := analyzer.SeasonalSignal{
		Signal:  signal,
		Bucket:  bucket,
		Hour:    hour,
		DayType: dayType,
		Points:  stats.Points,
		P50:     stats.P50,
		P95:     stats.P95,
		Max:     stats.Max,
	}
	if dayOfWeek != nil {
		summary.DayOfWeek = *dayOfWeek
	}
	return summary, true, nil
}

func (s *Store) seasonalLatencyBands(ctx context.Context, application string, workload *analyzer.WorkloadReport, hour int, dayType string) ([]analyzer.SeasonalLatencyBand, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT traffic_band
		FROM seasonal_signal_observations
		WHERE application = ?
			AND namespace = ?
			AND workload_name = ?
			AND signal = 'latency_p95'
			AND hour_of_day = ?
			AND day_type = ?
			AND traffic_band != ''
		ORDER BY traffic_band
	`, application, workload.Namespace, workload.Name, hour, dayType)
	if err != nil {
		return nil, fmt.Errorf("read seasonal latency bands: %w", err)
	}
	defer rows.Close()
	var bands []string
	for rows.Next() {
		var band string
		if err := rows.Scan(&band); err != nil {
			return nil, fmt.Errorf("scan seasonal latency band: %w", err)
		}
		bands = append(bands, band)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var summaries []analyzer.SeasonalLatencyBand
	for _, band := range bands {
		values, err := s.seasonalValues(ctx, `
			SELECT value
			FROM seasonal_signal_observations
			WHERE application = ?
				AND namespace = ?
				AND workload_name = ?
				AND signal = 'latency_p95'
				AND hour_of_day = ?
				AND day_type = ?
				AND traffic_band = ?
		`, application, workload.Namespace, workload.Name, hour, dayType, band)
		if err != nil {
			return nil, err
		}
		stats, ok := seasonalStats(values)
		if !ok {
			continue
		}
		summaries = append(summaries, analyzer.SeasonalLatencyBand{
			TrafficBand: band,
			Hour:        hour,
			DayType:     dayType,
			Points:      stats.Points,
			P50:         stats.P50,
			P95:         stats.P95,
			Max:         stats.Max,
		})
	}
	return summaries, nil
}

func (s *Store) seasonalValues(ctx context.Context, query string, args ...any) ([]float64, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read seasonal values: %w", err)
	}
	defer rows.Close()
	var values []float64
	for rows.Next() {
		var value float64
		if err := rows.Scan(&value); err != nil {
			return nil, fmt.Errorf("scan seasonal value: %w", err)
		}
		if finite(value) {
			values = append(values, value)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func (s *Store) recordSeasonalObservations(ctx context.Context, tx *sql.Tx, application string, generatedAt time.Time, workload *analyzer.WorkloadReport) error {
	dayType := seasonalDayType(generatedAt)
	trafficBand := trafficBandForWorkload(workload)
	for _, signal := range workload.MetricSignals {
		if _, ok := seasonalSignals[signal.Name]; !ok {
			continue
		}
		if signal.Sample == nil || !finite(*signal.Sample) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO seasonal_signal_observations (
				application,
				namespace,
				workload_name,
				deployment,
				generated_at,
				hour_of_day,
				day_of_week,
				day_type,
				signal,
				value,
				traffic_band
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			application,
			workload.Namespace,
			workload.Name,
			workload.Deployment,
			generatedAt.Format(time.RFC3339Nano),
			generatedAt.Hour(),
			int(generatedAt.Weekday()),
			dayType,
			signal.Name,
			sqlFinite(*signal.Sample),
			trafficBand,
		); err != nil {
			return fmt.Errorf("insert seasonal observation: %w", err)
		}
	}
	return nil
}

func applySeasonalityFeedback(workload *analyzer.WorkloadReport, seasonality *analyzer.SeasonalityLearning) {
	if seasonality == nil || seasonality.ObservationCount == 0 {
		return
	}
	rec := &workload.Recommendation
	if rec.RecommendedReplicas < rec.CurrentReplicas {
		if summary, ok := seasonalPressure(seasonality, workload, "request_rate"); ok {
			rec.RecommendedReplicas = rec.CurrentReplicas
			rec.ReasonCodes = append(rec.ReasonCodes, fmt.Sprintf("replica_scale_down_blocked_by_seasonality:%s_p95=%.4g,points=%d", summary.Bucket, summary.P95, summary.Points))
			addSeasonalityDecision(rec, workload, "replicas", summary, "hold replica scale-down because this hour historically carries higher traffic")
		}
	}
	if resourceDecreases(rec.CurrentCPURequest, rec.RecommendedCPURequest) {
		if summary, ok := seasonalPressure(seasonality, workload, "cpu_usage"); ok {
			rec.RecommendedCPURequest = rec.CurrentCPURequest
			rec.ReasonCodes = append(rec.ReasonCodes, fmt.Sprintf("cpu_request_hold_seasonal_hour_pressure:%s_p95=%.4g,points=%d", summary.Bucket, summary.P95, summary.Points))
			addSeasonalityDecision(rec, workload, "cpu", summary, "hold CPU request decrease because this hour historically needs more CPU")
		}
	}
	if resourceDecreases(rec.CurrentMemoryRequest, rec.RecommendedMemoryRequest) {
		if summary, ok := seasonalPressure(seasonality, workload, "memory_working_set"); ok {
			rec.RecommendedMemoryRequest = rec.CurrentMemoryRequest
			rec.ReasonCodes = append(rec.ReasonCodes, fmt.Sprintf("memory_request_hold_seasonal_hour_pressure:%s_p95=%s,points=%d", summary.Bucket, formatSeasonalValue(summary.Signal, summary.P95), summary.Points))
			addSeasonalityDecision(rec, workload, "memory", summary, "hold memory request decrease because this hour historically needs more memory")
		}
	}
}

func applyReplicaOutcomeFeedback(workload *analyzer.WorkloadReport, outcome *analyzer.RecommendationOutcome) {
	if outcome == nil || workload.Recommendation.ReplicaDecision == nil {
		return
	}
	component := analyzer.ReplicaDecisionComponent{
		Name:      "prior_replica_outcome",
		Replicas:  workload.Recommendation.RecommendedReplicas,
		Observed:  outcome.Status,
		Influence: "hold",
	}
	switch outcome.Status {
	case "applied_successful":
		if containsString(outcome.Details, "replicas_applied") {
			component.Score = 0.05
			component.Basis = "prior_replica_change_successful"
			component.Influence = "pressure"
			workload.Recommendation.Confidence = clampFloat(workload.Recommendation.Confidence+0.01, 0.05, 0.99)
		} else {
			return
		}
	case "too_conservative":
		component.Score = 0.10
		component.Basis = "prior_replica_change_too_conservative"
		component.Influence = "pressure"
		workload.Recommendation.Confidence = clampFloat(workload.Recommendation.Confidence-0.02, 0.05, 0.99)
	case "too_aggressive", "partially_applied", "not_applied":
		component.Score = -0.25
		component.Basis = "prior_replica_change_not_safe"
		component.Influence = "hold"
		workload.Recommendation.Confidence = clampFloat(workload.Recommendation.Confidence-0.05, 0.05, 0.99)
	default:
		return
	}
	decision := workload.Recommendation.ReplicaDecision
	decision.Components = append(decision.Components, component)
	decision.Score = clampFloat(decision.Score+component.Score, -1.5, 1.5)
	decision.Basis = decision.Basis + ", prior outcome"
	workload.Recommendation.ReasonCodes = append(workload.Recommendation.ReasonCodes,
		fmt.Sprintf("replica_signal_component:%s score=%.3g replicas=%d influence=%s basis=%s observed=%s", component.Name, component.Score, component.Replicas, component.Influence, component.Basis, component.Observed),
		fmt.Sprintf("replica_signal_score_after_outcome:%.3g", decision.Score),
	)
}

func seasonalPressure(seasonality *analyzer.SeasonalityLearning, workload *analyzer.WorkloadReport, signal string) (analyzer.SeasonalSignal, bool) {
	var best analyzer.SeasonalSignal
	for _, summary := range seasonality.Signals {
		if summary.Signal != signal || summary.Bucket != "same_day_type_hour" || summary.Points < seasonalMinimumPoints {
			continue
		}
		best = summary
		break
	}
	if best.Points == 0 {
		return analyzer.SeasonalSignal{}, false
	}
	baseline := currentSignalP95(workload, signal)
	if sample, ok := signalSample(workload.MetricSignals, signal); ok {
		baseline = math.Max(baseline, sample)
	}
	if baseline <= 0 {
		return best, best.P95 > 0
	}
	return best, best.P95 > baseline*1.15
}

func addSeasonalityDecision(rec *analyzer.Recommendation, workload *analyzer.WorkloadReport, subject string, summary analyzer.SeasonalSignal, conclusion string) {
	rec.Learning.Decisions = append(rec.Learning.Decisions, analyzer.LearnedDecision{
		Subject:    "seasonality." + subject,
		Learned:    fmt.Sprintf("%s hour=%d dayType=%s points=%d p50=%s p95=%s max=%s", summary.Bucket, summary.Hour, summary.DayType, summary.Points, formatSeasonalValue(summary.Signal, summary.P50), formatSeasonalValue(summary.Signal, summary.P95), formatSeasonalValue(summary.Signal, summary.Max)),
		Observed:   fmt.Sprintf("current learned p95=%s", formatSeasonalValue(summary.Signal, currentSignalP95(workload, summary.Signal))),
		Conclusion: conclusion,
	})
}

func currentSignalP95(workload *analyzer.WorkloadReport, signal string) float64 {
	for _, item := range workload.MetricSignals {
		if item.Name == signal && item.History != nil {
			return item.History.P95
		}
	}
	return 0
}

func trafficBandForWorkload(workload *analyzer.WorkloadReport) string {
	requestRate, ok := signalSample(workload.MetricSignals, "request_rate")
	if !ok {
		return ""
	}
	for _, signal := range workload.MetricSignals {
		if signal.Name == "request_rate" {
			return trafficBand(requestRate, signal.History)
		}
	}
	return ""
}

func trafficBand(value float64, history *analyzer.SignalHistory) string {
	if value <= 0 {
		return "idle"
	}
	if history == nil || history.Points == 0 {
		return "unknown"
	}
	switch {
	case history.P95 > 0 && value >= history.P95:
		return "peak"
	case history.P50 > 0 && value >= history.P50:
		return "high"
	case history.P50 > 0 && value >= history.P50*0.25:
		return "medium"
	default:
		return "low"
	}
}

func seasonalDayType(t time.Time) string {
	switch t.Weekday() {
	case time.Saturday, time.Sunday:
		return "weekend"
	default:
		return "weekday"
	}
}

type seasonalStatsResult struct {
	Points int
	P50    float64
	P95    float64
	Max    float64
}

func seasonalStats(values []float64) (seasonalStatsResult, bool) {
	if len(values) == 0 {
		return seasonalStatsResult{}, false
	}
	sort.Float64s(values)
	return seasonalStatsResult{
		Points: len(values),
		P50:    seasonalPercentile(values, 0.50),
		P95:    seasonalPercentile(values, 0.95),
		Max:    values[len(values)-1],
	}, true
}

func seasonalPercentile(sorted []float64, quantile float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	position := quantile * float64(len(sorted)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return sorted[lower]
	}
	weight := position - float64(lower)
	return sorted[lower]*(1-weight) + sorted[upper]*weight
}

func resourceDecreases(current, recommended string) bool {
	direction, ok := resourceDirection(current, recommended)
	return ok && direction < 0
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func formatSeasonalValue(signal string, value float64) string {
	if strings.Contains(strings.ToLower(signal), "memory") {
		return formatSeasonalBytes(value)
	}
	return fmt.Sprintf("%.4g", value)
}

func formatSeasonalBytes(value float64) string {
	units := []string{"B", "Ki", "Mi", "Gi", "Ti", "Pi"}
	scaled := math.Abs(value)
	unit := units[0]
	for i := 0; i < len(units)-1 && scaled >= 1024; i++ {
		scaled /= 1024
		unit = units[i+1]
	}
	if value < 0 {
		scaled = -scaled
	}
	if unit == "B" {
		return fmt.Sprintf("%.0f%s", scaled, unit)
	}
	return fmt.Sprintf("%.3g%s", scaled, unit)
}
