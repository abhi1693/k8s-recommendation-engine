package backtest

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/abhi1693/k8s-recommendation-engine/internal/config"
	"github.com/abhi1693/k8s-recommendation-engine/internal/prom"
)

const (
	defaultWindow          = 7 * 24 * time.Hour
	defaultStep            = 5 * time.Minute
	defaultForecastHorizon = 30 * time.Minute
	defaultStabilityRuns   = 3
)

var replaySignals = []string{
	"request_rate",
	"latency_p95",
	"error_rate",
	"concurrent_requests",
	"cpu_usage",
	"memory_working_set",
	"available_replicas",
}

type Prometheus interface {
	QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (*prom.RangeQueryResult, error)
}

type Options struct {
	Window          time.Duration `json:"window"`
	Step            time.Duration `json:"step"`
	End             time.Time     `json:"end"`
	ForecastHorizon time.Duration `json:"forecastHorizon"`
	StabilityRuns   int           `json:"stabilityRuns"`
}

type Report struct {
	Application string           `json:"application"`
	Namespace   string           `json:"namespace"`
	GeneratedAt time.Time        `json:"generatedAt"`
	Start       time.Time        `json:"start"`
	End         time.Time        `json:"end"`
	Window      string           `json:"window"`
	Step        string           `json:"step"`
	Method      string           `json:"method"`
	Workloads   []WorkloadReport `json:"workloads"`
	Summary     Summary          `json:"summary"`
}

type Summary struct {
	WorkloadsTotal                int     `json:"workloadsTotal"`
	Points                        int     `json:"points"`
	Spikes                        int     `json:"spikes"`
	ProactiveScaleBeforeSpikes    int     `json:"proactiveScaleBeforeSpikes"`
	CoveredByExistingCapacity     int     `json:"coveredByExistingCapacity"`
	MissedSpikes                  int     `json:"missedSpikes"`
	OverProvisionedPoints         int     `json:"overProvisionedPoints"`
	UnderProvisionedPoints        int     `json:"underProvisionedPoints"`
	ObservedReplicaHours          float64 `json:"observedReplicaHours"`
	ReactiveReplicaHours          float64 `json:"reactiveReplicaHours"`
	PredictiveReplicaHours        float64 `json:"predictiveReplicaHours"`
	ComputeSavedReplicaHours      float64 `json:"computeSavedReplicaHours"`
	EstimatedGitCommits           int     `json:"estimatedGitCommits"`
	ReactiveChangeEvents          int     `json:"reactiveChangeEvents"`
	CommitReductionVsReactive     int     `json:"commitReductionVsReactive"`
	WorkloadsWithInsufficientData int     `json:"workloadsWithInsufficientData"`
}

type WorkloadReport struct {
	Name                       string         `json:"name"`
	Namespace                  string         `json:"namespace"`
	Deployment                 string         `json:"deployment"`
	MetricProfile              string         `json:"metricProfile"`
	ReplicaScalingEnabled      bool           `json:"replicaScalingEnabled"`
	Points                     int            `json:"points"`
	Spikes                     int            `json:"spikes"`
	ProactiveScaleBeforeSpikes int            `json:"proactiveScaleBeforeSpikes"`
	CoveredByExistingCapacity  int            `json:"coveredByExistingCapacity"`
	MissedSpikes               int            `json:"missedSpikes"`
	OverProvisionedPoints      int            `json:"overProvisionedPoints"`
	UnderProvisionedPoints     int            `json:"underProvisionedPoints"`
	ObservedReplicaHours       float64        `json:"observedReplicaHours"`
	ReactiveReplicaHours       float64        `json:"reactiveReplicaHours"`
	PredictiveReplicaHours     float64        `json:"predictiveReplicaHours"`
	ComputeSavedReplicaHours   float64        `json:"computeSavedReplicaHours"`
	EstimatedGitCommits        int            `json:"estimatedGitCommits"`
	ReactiveChangeEvents       int            `json:"reactiveChangeEvents"`
	CommitReductionVsReactive  int            `json:"commitReductionVsReactive"`
	TrafficCapacityPerReplica  float64        `json:"trafficCapacityPerReplica,omitempty"`
	CPUCapacityPerReplica      float64        `json:"cpuCapacityPerReplica,omitempty"`
	MemoryCapacityPerReplica   float64        `json:"memoryCapacityPerReplica,omitempty"`
	InsufficientData           bool           `json:"insufficientData"`
	InsufficientDataReasons    []string       `json:"insufficientDataReasons,omitempty"`
	Signals                    []SignalReport `json:"signals"`
	Events                     []Event        `json:"events,omitempty"`
}

type SignalReport struct {
	Name   string  `json:"name"`
	Query  string  `json:"query,omitempty"`
	Points int     `json:"points"`
	Min    float64 `json:"min,omitempty"`
	P50    float64 `json:"p50,omitempty"`
	P95    float64 `json:"p95,omitempty"`
	Max    float64 `json:"max,omitempty"`
	Error  string  `json:"error,omitempty"`
}

type Event struct {
	Time    time.Time `json:"time"`
	Type    string    `json:"type"`
	Detail  string    `json:"detail"`
	Current int       `json:"current,omitempty"`
	Target  int       `json:"target,omitempty"`
}

type signalSeries struct {
	name    string
	query   string
	values  map[int64]float64
	samples []prom.Sample
	stats   sampleStats
	err     error
}

type sampleStats struct {
	points int
	min    float64
	p50    float64
	p95    float64
	max    float64
}

type replayPoint struct {
	timestamp          time.Time
	requestRate        float64
	latencyP95         float64
	errorRate          float64
	concurrentRequests float64
	cpuUsage           float64
	memoryWorkingSet   float64
	availableReplicas  float64
}

type capacities struct {
	trafficPerReplica float64
	cpuPerReplica     float64
	memoryPerReplica  float64
}

func Run(ctx context.Context, prometheus Prometheus, profile *config.ApplicationProfile, options Options) (*Report, error) {
	if profile == nil {
		return nil, fmt.Errorf("profile is required")
	}
	if prometheus == nil {
		return nil, fmt.Errorf("prometheus client is required")
	}
	options = normalizeOptions(options)
	start := options.End.Add(-options.Window)
	report := &Report{
		Application: profile.Metadata.Name,
		Namespace:   profile.Spec.Namespace,
		GeneratedAt: options.End,
		Start:       start,
		End:         options.End,
		Window:      formatDuration(options.Window),
		Step:        formatDuration(options.Step),
		Method:      "historical replay: predictive forecast with stability-gated Git proposals vs reactive current-signal baseline",
	}

	for _, workload := range profile.Spec.Workloads {
		workloadReport := replayWorkload(ctx, prometheus, profile, workload, start, options.End, options)
		report.Workloads = append(report.Workloads, workloadReport)
		addSummary(&report.Summary, workloadReport)
	}
	report.Summary.WorkloadsTotal = len(report.Workloads)
	report.Summary.ComputeSavedReplicaHours = report.Summary.ObservedReplicaHours - report.Summary.PredictiveReplicaHours
	report.Summary.CommitReductionVsReactive = report.Summary.ReactiveChangeEvents - report.Summary.EstimatedGitCommits
	return report, nil
}

func normalizeOptions(options Options) Options {
	if options.Window <= 0 {
		options.Window = defaultWindow
	}
	if options.Step <= 0 {
		options.Step = defaultStep
	}
	if options.End.IsZero() {
		options.End = time.Now().UTC()
	} else {
		options.End = options.End.UTC()
	}
	if options.ForecastHorizon <= 0 {
		options.ForecastHorizon = defaultForecastHorizon
	}
	if options.StabilityRuns <= 0 {
		options.StabilityRuns = defaultStabilityRuns
	}
	return options
}

func replayWorkload(ctx context.Context, prometheus Prometheus, profile *config.ApplicationProfile, workload config.WorkloadSpec, start, end time.Time, options Options) WorkloadReport {
	result := WorkloadReport{
		Name:                  workload.Name,
		Namespace:             profile.Spec.Namespace,
		Deployment:            workload.TargetRef.Name,
		MetricProfile:         workload.MetricProfileRef,
		ReplicaScalingEnabled: workload.Scaling.Replicas,
	}
	metricProfile := profile.MetricProfiles[workload.MetricProfileRef]
	series := map[string]signalSeries{}
	for _, name := range replaySignals {
		signal, ok := metricProfile.Signals[name]
		if !ok {
			continue
		}
		queried := querySignal(ctx, prometheus, workload, profile.Spec.Namespace, name, signal, start, end, options.Step)
		series[name] = queried
		result.Signals = append(result.Signals, signalReport(queried))
	}
	sort.Slice(result.Signals, func(i, j int) bool {
		return result.Signals[i].Name < result.Signals[j].Name
	})

	points := buildReplayPoints(series)
	result.Points = len(points)
	if len(points) == 0 {
		result.InsufficientData = true
		result.InsufficientDataReasons = append(result.InsufficientDataReasons, "no aligned Prometheus samples for workload")
		return result
	}
	if _, ok := series["available_replicas"]; !ok {
		result.InsufficientData = true
		result.InsufficientDataReasons = append(result.InsufficientDataReasons, "available_replicas signal missing; replica-hours are estimated from configured minimum")
	}
	if _, ok := series["request_rate"]; !ok {
		result.InsufficientData = true
		result.InsufficientDataReasons = append(result.InsufficientDataReasons, "request_rate signal missing; spike prediction is resource-only")
	}

	bounds := replicaBounds(workload, points)
	caps := learnedCapacities(series, bounds.initialReplicas)
	result.TrafficCapacityPerReplica = caps.trafficPerReplica
	result.CPUCapacityPerReplica = caps.cpuPerReplica
	result.MemoryCapacityPerReplica = caps.memoryPerReplica

	replayDecisions(&result, workload, points, caps, bounds, options)
	result.ComputeSavedReplicaHours = result.ObservedReplicaHours - result.PredictiveReplicaHours
	result.CommitReductionVsReactive = result.ReactiveChangeEvents - result.EstimatedGitCommits
	return result
}

func querySignal(ctx context.Context, prometheus Prometheus, workload config.WorkloadSpec, namespace, name string, signal config.Signal, start, end time.Time, step time.Duration) signalSeries {
	rendered, err := config.RenderQuery(signal.Query, workload.VarsWithDefaults(namespace))
	if err != nil {
		return signalSeries{name: name, err: err}
	}
	rangeResult, err := prometheus.QueryRange(ctx, rendered, start, end, step)
	if err != nil {
		return signalSeries{name: name, query: rendered, err: err}
	}
	samples := aggregateRangeSamples(rangeResult)
	values := make(map[int64]float64, len(samples))
	for _, sample := range samples {
		values[int64(sample.Timestamp)] = sample.Value
	}
	return signalSeries{
		name:    name,
		query:   rendered,
		values:  values,
		samples: samples,
		stats:   summarizeSamples(samples),
	}
}

func aggregateRangeSamples(result *prom.RangeQueryResult) []prom.Sample {
	if result == nil {
		return nil
	}
	byTimestamp := map[int64]float64{}
	for _, series := range result.Series {
		for _, sample := range series.Values {
			if validFloat(sample.Value) {
				byTimestamp[int64(sample.Timestamp)] += sample.Value
			}
		}
	}
	samples := make([]prom.Sample, 0, len(byTimestamp))
	for timestamp, value := range byTimestamp {
		samples = append(samples, prom.Sample{Timestamp: float64(timestamp), Value: value})
	}
	sort.Slice(samples, func(i, j int) bool {
		return samples[i].Timestamp < samples[j].Timestamp
	})
	return samples
}

func signalReport(series signalSeries) SignalReport {
	report := SignalReport{
		Name:   series.name,
		Query:  strings.TrimSpace(series.query),
		Points: series.stats.points,
		Min:    series.stats.min,
		P50:    series.stats.p50,
		P95:    series.stats.p95,
		Max:    series.stats.max,
	}
	if series.err != nil {
		report.Error = series.err.Error()
	}
	return report
}

func summarizeSamples(samples []prom.Sample) sampleStats {
	values := make([]float64, 0, len(samples))
	for _, sample := range samples {
		if validFloat(sample.Value) {
			values = append(values, sample.Value)
		}
	}
	if len(values) == 0 {
		return sampleStats{}
	}
	sort.Float64s(values)
	return sampleStats{
		points: len(values),
		min:    values[0],
		p50:    percentile(values, 0.50),
		p95:    percentile(values, 0.95),
		max:    values[len(values)-1],
	}
}

func buildReplayPoints(series map[string]signalSeries) []replayPoint {
	timestamps := map[int64]struct{}{}
	for _, item := range series {
		for timestamp := range item.values {
			timestamps[timestamp] = struct{}{}
		}
	}
	ordered := make([]int64, 0, len(timestamps))
	for timestamp := range timestamps {
		ordered = append(ordered, timestamp)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i] < ordered[j]
	})
	points := make([]replayPoint, 0, len(ordered))
	for _, timestamp := range ordered {
		point := replayPoint{timestamp: time.Unix(timestamp, 0).UTC()}
		point.requestRate = valueAt(series, "request_rate", timestamp)
		point.latencyP95 = valueAt(series, "latency_p95", timestamp)
		point.errorRate = valueAt(series, "error_rate", timestamp)
		point.concurrentRequests = valueAt(series, "concurrent_requests", timestamp)
		point.cpuUsage = valueAt(series, "cpu_usage", timestamp)
		point.memoryWorkingSet = valueAt(series, "memory_working_set", timestamp)
		point.availableReplicas = valueAt(series, "available_replicas", timestamp)
		points = append(points, point)
	}
	return points
}

func valueAt(series map[string]signalSeries, name string, timestamp int64) float64 {
	item, ok := series[name]
	if !ok {
		return math.NaN()
	}
	value, ok := item.values[timestamp]
	if !ok {
		return math.NaN()
	}
	return value
}

type replayBounds struct {
	minReplicas     int
	maxReplicas     int
	initialReplicas int
}

func replicaBounds(workload config.WorkloadSpec, points []replayPoint) replayBounds {
	minReplicas := workload.Bounds.Replicas.Min
	if minReplicas <= 0 {
		minReplicas = 1
	}
	maxReplicas := workload.Bounds.Replicas.Max
	observedMax := minReplicas
	initial := minReplicas
	for index, point := range points {
		if validFloat(point.availableReplicas) && point.availableReplicas > 0 {
			replicas := int(math.Round(point.availableReplicas))
			if index == 0 {
				initial = replicas
			}
			if replicas > observedMax {
				observedMax = replicas
			}
		}
	}
	if maxReplicas <= 0 {
		maxReplicas = maxInt(observedMax*2, minReplicas)
	}
	if maxReplicas < minReplicas {
		maxReplicas = minReplicas
	}
	if !workload.Scaling.Replicas {
		minReplicas = initial
		maxReplicas = initial
	}
	return replayBounds{
		minReplicas:     minReplicas,
		maxReplicas:     maxReplicas,
		initialReplicas: clampInt(initial, minReplicas, maxReplicas),
	}
}

func learnedCapacities(series map[string]signalSeries, replicas int) capacities {
	if replicas <= 0 {
		replicas = 1
	}
	caps := capacities{}
	if stats := series["request_rate"].stats; stats.points > 0 {
		caps.trafficPerReplica = math.Max(stats.p95, stats.max*0.75)
	}
	if stats := series["cpu_usage"].stats; stats.points > 0 {
		caps.cpuPerReplica = math.Max(stats.p95, stats.max*0.75)
	}
	if stats := series["memory_working_set"].stats; stats.points > 0 {
		caps.memoryPerReplica = math.Max(stats.p95, stats.max*0.75)
	}
	return caps
}

func replayDecisions(report *WorkloadReport, workload config.WorkloadSpec, points []replayPoint, caps capacities, bounds replayBounds, options Options) {
	stepHours := options.Step.Hours()
	predictiveCommitted := bounds.initialReplicas
	pendingTarget := predictiveCommitted
	pendingCount := 0
	previousReactive := demandReplicas(points[0], caps, bounds, false)
	lastScaleUpTime := time.Time{}
	reactiveReplicas := previousReactive

	for index, point := range points {
		reactiveRequired := demandReplicas(point, caps, bounds, false)
		predictiveTarget := predictiveReplicas(points, index, caps, bounds, options)
		if !workload.Scaling.Replicas {
			predictiveTarget = bounds.initialReplicas
			reactiveRequired = bounds.initialReplicas
		}

		if reactiveRequired != reactiveReplicas {
			report.ReactiveChangeEvents++
			reactiveReplicas = reactiveRequired
		}

		if predictiveTarget == pendingTarget {
			pendingCount++
		} else {
			pendingTarget = predictiveTarget
			pendingCount = 1
		}
		if pendingTarget != predictiveCommitted && pendingCount >= options.StabilityRuns {
			previous := predictiveCommitted
			predictiveCommitted = pendingTarget
			report.EstimatedGitCommits++
			if predictiveCommitted > previous {
				lastScaleUpTime = point.timestamp
				appendEvent(report, Event{
					Time:    point.timestamp,
					Type:    "predictive_scale_up",
					Current: previous,
					Target:  predictiveCommitted,
					Detail:  fmt.Sprintf("forecast target held stable for %d replay points", options.StabilityRuns),
				})
			} else {
				appendEvent(report, Event{
					Time:    point.timestamp,
					Type:    "predictive_scale_down",
					Current: previous,
					Target:  predictiveCommitted,
					Detail:  fmt.Sprintf("lower target held stable for %d replay points", options.StabilityRuns),
				})
			}
		}

		if isSpike(points, index) {
			report.Spikes++
			covered := predictiveCommitted >= reactiveRequired
			proactive := predictiveCommitted >= reactiveRequired &&
				reactiveRequired > bounds.minReplicas &&
				!lastScaleUpTime.IsZero() &&
				lastScaleUpTime.Before(point.timestamp) &&
				point.timestamp.Sub(lastScaleUpTime) <= options.ForecastHorizon+options.Step
			switch {
			case proactive:
				report.ProactiveScaleBeforeSpikes++
				appendEvent(report, Event{
					Time:   point.timestamp,
					Type:   "spike_protected",
					Detail: fmt.Sprintf("predictive replicas=%d met reactive need=%d before spike", predictiveCommitted, reactiveRequired),
				})
			case covered:
				report.CoveredByExistingCapacity++
			default:
				report.MissedSpikes++
				appendEvent(report, Event{
					Time:   point.timestamp,
					Type:   "spike_missed",
					Detail: fmt.Sprintf("predictive replicas=%d below reactive need=%d at spike", predictiveCommitted, reactiveRequired),
				})
			}
		}

		if predictiveCommitted < reactiveRequired {
			report.UnderProvisionedPoints++
		}
		if predictiveCommitted > reactiveRequired+maxInt(1, int(math.Ceil(float64(reactiveRequired)*0.25))) {
			report.OverProvisionedPoints++
		}
		observedReplicas := bounds.initialReplicas
		if validFloat(point.availableReplicas) && point.availableReplicas > 0 {
			observedReplicas = clampInt(int(math.Round(point.availableReplicas)), bounds.minReplicas, bounds.maxReplicas)
		}
		report.ObservedReplicaHours += float64(observedReplicas) * stepHours
		report.ReactiveReplicaHours += float64(reactiveRequired) * stepHours
		report.PredictiveReplicaHours += float64(predictiveCommitted) * stepHours
		previousReactive = reactiveRequired
	}
	_ = previousReactive
}

func predictiveReplicas(points []replayPoint, index int, caps capacities, bounds replayBounds, options Options) int {
	point := points[index]
	forecast := replayPoint{
		timestamp:          point.timestamp,
		requestRate:        forecastValue(points, index, options, func(p replayPoint) float64 { return p.requestRate }),
		latencyP95:         point.latencyP95,
		errorRate:          point.errorRate,
		concurrentRequests: forecastValue(points, index, options, func(p replayPoint) float64 { return p.concurrentRequests }),
		cpuUsage:           trailingP95(points, index, options, func(p replayPoint) float64 { return p.cpuUsage }),
		memoryWorkingSet:   trailingP95(points, index, options, func(p replayPoint) float64 { return p.memoryWorkingSet }),
		availableReplicas:  point.availableReplicas,
	}
	return demandReplicas(forecast, caps, bounds, true)
}

func demandReplicas(point replayPoint, caps capacities, bounds replayBounds, predictive bool) int {
	target := bounds.minReplicas
	if validFloat(point.requestRate) && caps.trafficPerReplica > 0 {
		multiplier := 1.0
		if validFloat(point.latencyP95) && point.latencyP95 > 1 {
			multiplier += 0.15
		}
		if validFloat(point.errorRate) && point.errorRate > 0 {
			multiplier += 0.10
		}
		target = maxInt(target, int(math.Ceil((point.requestRate*multiplier)/caps.trafficPerReplica)))
	}
	if validFloat(point.concurrentRequests) && caps.trafficPerReplica > 0 {
		target = maxInt(target, int(math.Ceil(point.concurrentRequests/(caps.trafficPerReplica*5))))
	}
	if validFloat(point.cpuUsage) && caps.cpuPerReplica > 0 {
		target = maxInt(target, int(math.Ceil(point.cpuUsage/caps.cpuPerReplica)))
	}
	if validFloat(point.memoryWorkingSet) && caps.memoryPerReplica > 0 {
		target = maxInt(target, int(math.Ceil(point.memoryWorkingSet/caps.memoryPerReplica)))
	}
	return clampInt(target, bounds.minReplicas, bounds.maxReplicas)
}

func forecastValue(points []replayPoint, index int, options Options, value func(replayPoint) float64) float64 {
	start := index - int(math.Ceil(float64(options.ForecastHorizon)/float64(options.Step)))
	if start < 0 {
		start = 0
	}
	var samples []prom.Sample
	for i := start; i < index; i++ {
		current := value(points[i])
		if validFloat(current) {
			samples = append(samples, prom.Sample{Timestamp: float64(points[i].timestamp.Unix()), Value: current})
		}
	}
	if len(samples) == 0 {
		return value(points[index])
	}
	stats := summarizeSamples(samples)
	if len(samples) < 3 {
		return maxFloat(value(points[index]), stats.p95)
	}
	slope, residualP95 := linearTrend(samples)
	last := samples[len(samples)-1].Value
	hours := options.ForecastHorizon.Hours()
	forecast := math.Max(0, last+slope*hours)
	band := maxFloat(residualP95, math.Abs(forecast)*0.10, math.Max(0, stats.p95-stats.p50)*0.50)
	return maxFloat(value(points[index]), stats.p95, forecast+band)
}

func trailingP95(points []replayPoint, index int, options Options, value func(replayPoint) float64) float64 {
	start := index - int(math.Ceil(float64(options.ForecastHorizon)/float64(options.Step)))
	if start < 0 {
		start = 0
	}
	var values []float64
	for i := start; i <= index; i++ {
		current := value(points[i])
		if validFloat(current) {
			values = append(values, current)
		}
	}
	if len(values) == 0 {
		return math.NaN()
	}
	sort.Float64s(values)
	return percentile(values, 0.95)
}

func isSpike(points []replayPoint, index int) bool {
	point := points[index]
	if index < 3 {
		return false
	}
	requestBaseline := trailingP95(points, index-1, Options{ForecastHorizon: time.Hour, Step: defaultStep}, func(p replayPoint) float64 { return p.requestRate })
	if validFloat(point.requestRate) && requestBaseline > 0 && point.requestRate > requestBaseline*1.50 {
		return true
	}
	latencyBaseline := trailingP95(points, index-1, Options{ForecastHorizon: time.Hour, Step: defaultStep}, func(p replayPoint) float64 { return p.latencyP95 })
	if validFloat(point.latencyP95) && latencyBaseline > 0 && point.latencyP95 > latencyBaseline*1.75 {
		return true
	}
	errorBaseline := trailingP95(points, index-1, Options{ForecastHorizon: time.Hour, Step: defaultStep}, func(p replayPoint) float64 { return p.errorRate })
	if validFloat(point.errorRate) && point.errorRate > 0 && (!validFloat(errorBaseline) || point.errorRate > math.Max(errorBaseline*2, 0.01)) {
		return true
	}
	return false
}

func linearTrend(samples []prom.Sample) (float64, float64) {
	if len(samples) < 2 {
		return 0, 0
	}
	base := samples[0].Timestamp
	var sumX, sumY float64
	for _, sample := range samples {
		x := (sample.Timestamp - base) / 3600
		sumX += x
		sumY += sample.Value
	}
	meanX := sumX / float64(len(samples))
	meanY := sumY / float64(len(samples))
	var numerator, denominator float64
	for _, sample := range samples {
		x := (sample.Timestamp - base) / 3600
		numerator += (x - meanX) * (sample.Value - meanY)
		denominator += (x - meanX) * (x - meanX)
	}
	slope := 0.0
	if denominator > 0 {
		slope = numerator / denominator
	}
	intercept := meanY - slope*meanX
	residuals := make([]float64, 0, len(samples))
	for _, sample := range samples {
		x := (sample.Timestamp - base) / 3600
		residuals = append(residuals, math.Abs(sample.Value-(intercept+slope*x)))
	}
	sort.Float64s(residuals)
	return slope, percentile(residuals, 0.95)
}

func addSummary(summary *Summary, workload WorkloadReport) {
	summary.Points += workload.Points
	summary.Spikes += workload.Spikes
	summary.ProactiveScaleBeforeSpikes += workload.ProactiveScaleBeforeSpikes
	summary.CoveredByExistingCapacity += workload.CoveredByExistingCapacity
	summary.MissedSpikes += workload.MissedSpikes
	summary.OverProvisionedPoints += workload.OverProvisionedPoints
	summary.UnderProvisionedPoints += workload.UnderProvisionedPoints
	summary.ObservedReplicaHours += workload.ObservedReplicaHours
	summary.ReactiveReplicaHours += workload.ReactiveReplicaHours
	summary.PredictiveReplicaHours += workload.PredictiveReplicaHours
	summary.EstimatedGitCommits += workload.EstimatedGitCommits
	summary.ReactiveChangeEvents += workload.ReactiveChangeEvents
	if workload.InsufficientData {
		summary.WorkloadsWithInsufficientData++
	}
}

func appendEvent(report *WorkloadReport, event Event) {
	if len(report.Events) < 20 {
		report.Events = append(report.Events, event)
	}
}

func validFloat(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func percentile(sorted []float64, quantile float64) float64 {
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

func maxFloat(values ...float64) float64 {
	maximum := math.Inf(-1)
	for _, value := range values {
		if validFloat(value) && value > maximum {
			maximum = value
		}
	}
	if math.IsInf(maximum, -1) {
		return math.NaN()
	}
	return maximum
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func clampInt(value, minimum, maximum int) int {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func formatDuration(duration time.Duration) string {
	if duration%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", int(duration/(24*time.Hour)))
	}
	if duration%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(duration/time.Hour))
	}
	if duration%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(duration/time.Minute))
	}
	return duration.String()
}
