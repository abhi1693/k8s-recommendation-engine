package config

import "testing"

func TestRenderQuery(t *testing.T) {
	got, err := RenderQuery(`up{namespace="{{ .namespace }}",deployment="{{ .deployment }}"}`, map[string]string{
		"namespace":  "shipyardhq",
		"deployment": "shipyardhq",
	})
	if err != nil {
		t.Fatalf("RenderQuery() error = %v", err)
	}
	want := `up{namespace="shipyardhq",deployment="shipyardhq"}`
	if got != want {
		t.Fatalf("RenderQuery() = %q, want %q", got, want)
	}
}

func TestRenderQueryMissingKey(t *testing.T) {
	if _, err := RenderQuery(`up{namespace="{{ .namespace }}"}`, map[string]string{}); err == nil {
		t.Fatalf("RenderQuery() expected missing key error")
	}
}
