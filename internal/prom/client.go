package prom

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	baseURL    string
	httpClient *http.Client
}

type QueryResult struct {
	Query      string          `json:"query"`
	ResultType string          `json:"resultType"`
	Series     []InstantVector `json:"series"`
	Warnings   []string        `json:"warnings,omitempty"`
}

type RangeQueryResult struct {
	Query      string        `json:"query"`
	ResultType string        `json:"resultType"`
	Series     []RangeVector `json:"series"`
	Warnings   []string      `json:"warnings,omitempty"`
}

type InstantVector struct {
	Metric map[string]string `json:"metric"`
	Value  float64           `json:"value"`
}

type RangeVector struct {
	Metric map[string]string `json:"metric"`
	Values []Sample          `json:"values"`
}

type Sample struct {
	Timestamp float64 `json:"timestamp"`
	Value     float64 `json:"value"`
}

func NewClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: httpClient,
	}
}

func (c *Client) Query(ctx context.Context, query string) (*QueryResult, error) {
	endpoint, err := url.Parse(c.baseURL + "/api/v1/query")
	if err != nil {
		return nil, err
	}
	values := endpoint.Query()
	values.Set("query", query)
	endpoint.RawQuery = values.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus query failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("prometheus query status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var decoded apiResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode prometheus response: %w", err)
	}
	if decoded.Status != "success" {
		return nil, fmt.Errorf("prometheus returned status %q: %s", decoded.Status, decoded.Error)
	}
	if decoded.Data.ResultType != "vector" {
		return &QueryResult{
			Query:      query,
			ResultType: decoded.Data.ResultType,
			Warnings:   decoded.Warnings,
		}, nil
	}

	result := &QueryResult{
		Query:      query,
		ResultType: decoded.Data.ResultType,
		Warnings:   decoded.Warnings,
	}
	for _, item := range decoded.Data.Result {
		value, err := parsePromValue(item.Value)
		if err != nil {
			return nil, err
		}
		result.Series = append(result.Series, InstantVector{
			Metric: item.Metric,
			Value:  value,
		})
	}
	return result, nil
}

func (c *Client) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (*RangeQueryResult, error) {
	endpoint, err := url.Parse(c.baseURL + "/api/v1/query_range")
	if err != nil {
		return nil, err
	}
	values := endpoint.Query()
	values.Set("query", query)
	values.Set("start", strconv.FormatFloat(float64(start.Unix()), 'f', -1, 64))
	values.Set("end", strconv.FormatFloat(float64(end.Unix()), 'f', -1, 64))
	values.Set("step", fmt.Sprintf("%ds", int(step.Seconds())))
	endpoint.RawQuery = values.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus range query failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("prometheus range query status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var decoded rangeAPIResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode prometheus range response: %w", err)
	}
	if decoded.Status != "success" {
		return nil, fmt.Errorf("prometheus returned status %q: %s", decoded.Status, decoded.Error)
	}
	if decoded.Data.ResultType != "matrix" {
		return &RangeQueryResult{
			Query:      query,
			ResultType: decoded.Data.ResultType,
			Warnings:   decoded.Warnings,
		}, nil
	}

	result := &RangeQueryResult{
		Query:      query,
		ResultType: decoded.Data.ResultType,
		Warnings:   decoded.Warnings,
	}
	for _, item := range decoded.Data.Result {
		vector := RangeVector{Metric: item.Metric}
		for _, raw := range item.Values {
			value, err := parsePromValue(raw)
			if err != nil {
				return nil, err
			}
			ts, _ := raw[0].(float64)
			vector.Values = append(vector.Values, Sample{Timestamp: ts, Value: value})
		}
		result.Series = append(result.Series, vector)
	}
	return result, nil
}

func parsePromValue(raw []any) (float64, error) {
	if len(raw) != 2 {
		return math.NaN(), fmt.Errorf("unexpected prometheus value shape: %v", raw)
	}
	value, ok := raw[1].(string)
	if !ok {
		return math.NaN(), fmt.Errorf("unexpected prometheus value type: %T", raw[1])
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return math.NaN(), fmt.Errorf("parse prometheus value %q: %w", value, err)
	}
	return parsed, nil
}

type apiResponse struct {
	Status   string   `json:"status"`
	Error    string   `json:"error"`
	Warnings []string `json:"warnings"`
	Data     struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

type rangeAPIResponse struct {
	Status   string   `json:"status"`
	Error    string   `json:"error"`
	Warnings []string `json:"warnings"`
	Data     struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Values [][]any           `json:"values"`
		} `json:"result"`
	} `json:"data"`
}
