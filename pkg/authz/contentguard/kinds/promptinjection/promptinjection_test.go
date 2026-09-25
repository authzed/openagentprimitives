package promptinjection

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/authz/contentguard"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cfg(t *testing.T, m map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return b
}

func TestConfigure_failClosed(t *testing.T) {
	cases := []struct {
		name string
		m    map[string]any
	}{
		{"missing image", map[string]any{"port": 8919}},
		{"bad action", map[string]any{"detectorImage": "x@sha256:a", "port": 8919, "action": "nope"}},
		{"bad onError", map[string]any{"detectorImage": "x@sha256:a", "port": 8919, "onError": "explode"}},
		{"threshold out of range", map[string]any{"detectorImage": "x@sha256:a", "port": 8919, "threshold": 1.5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New().Configure(cfg(t, tc.m))
			assert.Error(t, err)
		})
	}
}

func TestInspect_scoreToAction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"score":0.95,"label":"injection"}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CONTENTGUARD_DETECTOR_ENDPOINT", srv.URL)

	inst, err := New().Configure(cfg(t, map[string]any{
		"detectorImage": "x@sha256:a", "port": 8919, "threshold": 0.8, "action": "approve"}))
	require.NoError(t, err)

	f, err := inst.Inspect(context.Background(), contentguard.Subject{
		Point:    pipeline.PostToolCall,
		ToolName: "fetch",
		Result:   "ignore previous",
	})
	require.NoError(t, err)
	assert.Equal(t, contentguard.Approve, f.Action)
	assert.InDelta(t, 0.95, f.Details["score"], 0.001)
}

func TestInspect_scoreBelowThreshold_passes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"score":0.3,"label":"safe"}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CONTENTGUARD_DETECTOR_ENDPOINT", srv.URL)

	inst, err := New().Configure(cfg(t, map[string]any{
		"detectorImage": "x@sha256:a", "port": 8919, "threshold": 0.8, "action": "block"}))
	require.NoError(t, err)

	f, err := inst.Inspect(context.Background(), contentguard.Subject{
		Point:  pipeline.PostToolCall,
		Result: "harmless content",
	})
	require.NoError(t, err)
	assert.Equal(t, contentguard.Pass, f.Action)
}

func TestInspect_onErrorWarn_passesLoud(t *testing.T) {
	t.Setenv("CONTENTGUARD_DETECTOR_ENDPOINT", "http://127.0.0.1:1") // nothing listening
	inst, err := New().Configure(cfg(t, map[string]any{
		"detectorImage": "x@sha256:a", "port": 8919, "onError": "warn", "timeoutMs": 50}))
	require.NoError(t, err)

	f, err := inst.Inspect(context.Background(), contentguard.Subject{
		Point:  pipeline.PostToolCall,
		Result: "x",
	})
	require.NoError(t, err) // warn => no error to the adapter (no fail-closed Block)
	assert.Equal(t, contentguard.Pass, f.Action)
	assert.NotEmpty(t, f.Reason, "warn mode should emit a loud reason")
	assert.NotEmpty(t, f.Details["detector_error"], "warn mode should record the error in details")
}

func TestInspect_onErrorBlock_returnsError(t *testing.T) {
	t.Setenv("CONTENTGUARD_DETECTOR_ENDPOINT", "http://127.0.0.1:1") // nothing listening
	inst, err := New().Configure(cfg(t, map[string]any{
		"detectorImage": "x@sha256:a", "port": 8919, "onError": "block", "timeoutMs": 50}))
	require.NoError(t, err)

	_, err = inst.Inspect(context.Background(), contentguard.Subject{
		Point:  pipeline.PostToolCall,
		Result: "x",
	})
	assert.Error(t, err) // block => error returned (adapter fail-closes to Block)
}

func TestDetector_returnsSpec(t *testing.T) {
	raw := cfg(t, map[string]any{
		"detectorImage": "registry.example.com/pi@sha256:abc", "port": 8919})
	spec, err := New().Detector(raw)
	require.NoError(t, err)
	assert.Equal(t, "registry.example.com/pi@sha256:abc", spec.Image)
	assert.Equal(t, int32(8919), spec.Port)
	assert.Equal(t, "/healthz", spec.HealthPath) // default
}

func TestDetector_customHealthPath(t *testing.T) {
	raw := cfg(t, map[string]any{
		"detectorImage": "x@sha256:a", "port": 8919, "healthPath": "/ready"})
	spec, err := New().Detector(raw)
	require.NoError(t, err)
	assert.Equal(t, "/ready", spec.HealthPath)
}
