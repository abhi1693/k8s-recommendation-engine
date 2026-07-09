package analyzer

import "encoding/json"

type signalReportJSON SignalReport

func (signal SignalReport) MarshalJSON() ([]byte, error) {
	output := struct {
		signalReportJSON
		SampleDisplay  string                `json:"sampleDisplay,omitempty"`
		HistoryDisplay *signalHistoryDisplay `json:"historyDisplay,omitempty"`
	}{
		signalReportJSON: signalReportJSON(signal),
	}
	if isByteSignal(signal.Name) {
		if signal.Sample != nil {
			output.SampleDisplay = formatSignalValue(signal.Name, *signal.Sample)
		}
		if signal.History != nil {
			output.HistoryDisplay = newSignalHistoryDisplay(signal.Name, *signal.History)
		}
	}
	return json.Marshal(output)
}

type learnedSignalJSON LearnedSignal

func (signal LearnedSignal) MarshalJSON() ([]byte, error) {
	output := struct {
		learnedSignalJSON
		CurrentDisplay string `json:"currentDisplay,omitempty"`
		P50Display     string `json:"p50Display,omitempty"`
		P95Display     string `json:"p95Display,omitempty"`
		MaxDisplay     string `json:"maxDisplay,omitempty"`
	}{
		learnedSignalJSON: learnedSignalJSON(signal),
	}
	if isByteSignal(signal.Name) {
		output.CurrentDisplay = formatSignalValue(signal.Name, signal.Current)
		output.P50Display = formatSignalValue(signal.Name, signal.P50)
		output.P95Display = formatSignalValue(signal.Name, signal.P95)
		output.MaxDisplay = formatSignalValue(signal.Name, signal.Max)
	}
	return json.Marshal(output)
}

type seasonalSignalJSON SeasonalSignal

func (signal SeasonalSignal) MarshalJSON() ([]byte, error) {
	output := struct {
		seasonalSignalJSON
		P50Display string `json:"p50Display,omitempty"`
		P95Display string `json:"p95Display,omitempty"`
		MaxDisplay string `json:"maxDisplay,omitempty"`
	}{
		seasonalSignalJSON: seasonalSignalJSON(signal),
	}
	if isByteSignal(signal.Signal) {
		output.P50Display = formatSignalValue(signal.Signal, signal.P50)
		output.P95Display = formatSignalValue(signal.Signal, signal.P95)
		output.MaxDisplay = formatSignalValue(signal.Signal, signal.Max)
	}
	return json.Marshal(output)
}

type containerReportJSON ContainerReport

func (container ContainerReport) MarshalJSON() ([]byte, error) {
	output := struct {
		containerReportJSON
		MemoryRequestBytesDisplay string `json:"memoryRequestBytesDisplay,omitempty"`
	}{
		containerReportJSON: containerReportJSON(container),
	}
	if container.MemoryRequestBytes > 0 {
		output.MemoryRequestBytesDisplay = formatBytes(container.MemoryRequestBytes)
	}
	return json.Marshal(output)
}

type signalHistoryDisplay struct {
	Min string `json:"min"`
	P50 string `json:"p50"`
	P95 string `json:"p95"`
	Max string `json:"max"`
}

func newSignalHistoryDisplay(name string, history SignalHistory) *signalHistoryDisplay {
	return &signalHistoryDisplay{
		Min: formatSignalValue(name, history.Min),
		P50: formatSignalValue(name, history.P50),
		P95: formatSignalValue(name, history.P95),
		Max: formatSignalValue(name, history.Max),
	}
}
