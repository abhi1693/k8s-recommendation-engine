package backtest

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/abhi1693/k8s-recommendation-engine/internal/config"
	"github.com/abhi1693/k8s-recommendation-engine/internal/prom"
)

type fakePrometheus struct {
	results map[string]*prom.RangeQueryResult
}

func (f fakePrometheus) QueryRange(_ context.Context, query string, _, _ time.Time, _ time.Duration) (*prom.RangeQueryResult, error) {
	result, ok := f.results[query]
	if !ok {
		return &prom.RangeQueryResult{Query: query, ResultType: "matrix"}, nil
	}
	return result, nil
}

func TestRunEstimatesComputeSavedAgainstObservedReplicas(t *testing.T) {
	start := time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC)
	step := 5 * time.Minute
	profile := testProfile()
	fake := fakePrometheus{results: map[string]*prom.RangeQueryResult{
		"request web":  rangeResult(start, step, []float64{0.2, 0.2, 0.2, 0.2, 0.2, 0.2, 0.2, 0.2, 0.2, 0.2, 0.2, 0.2}),
		"cpu web":      rangeResult(start, step, []float64{0.05, 0.05, 0.05, 0.05, 0.05, 0.05, 0.05, 0.05, 0.05, 0.05, 0.05, 0.05}),
		"memory web":   rangeResult(start, step, []float64{128 << 20, 128 << 20, 128 << 20, 128 << 20, 128 << 20, 128 << 20, 128 << 20, 128 << 20, 128 << 20, 128 << 20, 128 << 20, 128 << 20}),
		"replicas web": rangeResult(start, step, []float64{2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2}),
	}}

	report, err := Run(context.Background(), fake, profile, Options{
		Window:          time.Hour,
		Step:            step,
		End:             start.Add(time.Hour),
		ForecastHorizon: 15 * time.Minute,
		StabilityRuns:   3,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	workload := report.Workloads[0]
	if workload.EstimatedGitCommits == 0 {
		t.Fatalf("EstimatedGitCommits = 0, want scale-down commit")
	}
	if workload.ComputeSavedReplicaHours <= 0 {
		t.Fatalf("ComputeSavedReplicaHours = %v, want positive savings", workload.ComputeSavedReplicaHours)
	}
	if report.Summary.ComputeSavedReplicaHours != workload.ComputeSavedReplicaHours {
		t.Fatalf("summary savings = %v, workload savings = %v", report.Summary.ComputeSavedReplicaHours, workload.ComputeSavedReplicaHours)
	}
}

func TestRunCountsMissedSpikesWhenPredictionCannotActInTime(t *testing.T) {
	start := time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC)
	step := 5 * time.Minute
	profile := testProfile()
	requests := []float64{1, 1, 1, 1, 10, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}
	cpu := []float64{0.1, 0.1, 0.1, 0.1, 0.2, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1}
	memory := repeatFloat(128<<20, len(requests))
	replicas := repeatFloat(1, len(requests))
	fake := fakePrometheus{results: map[string]*prom.RangeQueryResult{
		"request web":  rangeResult(start, step, requests),
		"cpu web":      rangeResult(start, step, cpu),
		"memory web":   rangeResult(start, step, memory),
		"replicas web": rangeResult(start, step, replicas),
	}}

	report, err := Run(context.Background(), fake, profile, Options{
		Window:          time.Duration(len(requests)) * step,
		Step:            step,
		End:             start.Add(time.Duration(len(requests)) * step),
		ForecastHorizon: 15 * time.Minute,
		StabilityRuns:   3,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	workload := report.Workloads[0]
	if workload.Spikes == 0 {
		t.Fatalf("Spikes = 0, want detected spike")
	}
	if workload.MissedSpikes == 0 {
		t.Fatalf("MissedSpikes = 0, want missed spike before stability gate can act")
	}
}

func TestWriteTextReportIncludesProofSections(t *testing.T) {
	report := &Report{
		Application: "shipyard",
		Namespace:   "shipyardhq",
		GeneratedAt: time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC),
		Start:       time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC),
		End:         time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC),
		Window:      "24h",
		Step:        "5m",
		Method:      "test",
		Summary: Summary{
			WorkloadsTotal:           1,
			ComputeSavedReplicaHours: 1.5,
		},
		Workloads: []WorkloadReport{
			{
				Namespace:                 "shipyardhq",
				Deployment:                "shipyardhq",
				Name:                      "web",
				MetricProfile:             "test",
				Points:                    12,
				ComputeSavedReplicaHours:  1.5,
				TrafficCapacityPerReplica: 2.5,
				CPUCapacityPerReplica:     0.3,
				MemoryCapacityPerReplica:  512 << 20,
			},
		},
	}
	var output bytes.Buffer
	if err := WriteTextReport(&output, report); err != nil {
		t.Fatalf("WriteTextReport() error = %v", err)
	}
	text := output.String()
	for _, want := range []string{"Proof Summary", "spike proof", "computeSaved=1.50 replica-hours", "learned capacity"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q:\n%s", want, text)
		}
	}
}

func testProfile() *config.ApplicationProfile {
	return &config.ApplicationProfile{
		Metadata: config.Metadata{Name: "test"},
		Spec: config.ApplicationSpec{
			Namespace: "default",
			Workloads: []config.WorkloadSpec{
				{
					Name: "web",
					TargetRef: config.TargetRef{
						Kind: "Deployment",
						Name: "web",
					},
					MetricProfileRef: "http",
					Scaling: config.ScalingSpec{
						Replicas: true,
						CPU:      true,
						Memory:   true,
					},
					Bounds: config.BoundsSpec{
						Replicas: config.ReplicaBounds{Min: 1, Max: 4},
					},
					Vars: map[string]string{"deployment": "web"},
				},
			},
		},
		MetricProfiles: map[string]config.MetricProfile{
			"http": {
				Signals: map[string]config.Signal{
					"request_rate":       {Required: true, Query: "request {{ .deployment }}"},
					"cpu_usage":          {Required: true, Query: "cpu {{ .deployment }}"},
					"memory_working_set": {Required: true, Query: "memory {{ .deployment }}"},
					"available_replicas": {Required: true, Query: "replicas {{ .deployment }}"},
				},
			},
		},
	}
}

func rangeResult(start time.Time, step time.Duration, values []float64) *prom.RangeQueryResult {
	vector := prom.RangeVector{Metric: map[string]string{"test": "true"}}
	for index, value := range values {
		vector.Values = append(vector.Values, prom.Sample{
			Timestamp: float64(start.Add(time.Duration(index) * step).Unix()),
			Value:     value,
		})
	}
	return &prom.RangeQueryResult{
		ResultType: "matrix",
		Series:     []prom.RangeVector{vector},
	}
}

func repeatFloat(value float64, count int) []float64 {
	values := make([]float64, count)
	for index := range values {
		values[index] = value
	}
	return values
}
