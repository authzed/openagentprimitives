package channelevents

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validMonitoringEvent() MonitoringEvent {
	return MonitoringEvent{
		Level:      MonitoringLevelError,
		Category:   "credential",
		Transition: MonitoringTransitionFailed,
		Source:     MonitoringSourceRef{Kind: "AgentIdentity", Namespace: "default", Name: "github-bot"},
		Condition:  "Refresh",
		Reason:     "TokenEndpointError",
		Summary:    "401 invalid_grant",
		Timestamp:  time.Now().UTC(),
	}
}

func TestMonitoringEvent_Validate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*MonitoringEvent)
		wantErr bool
	}{
		{"valid error/failed: ok", func(*MonitoringEvent) {}, false},
		{"valid warning/recovered: ok", func(e *MonitoringEvent) {
			e.Level = MonitoringLevelWarning
			e.Transition = MonitoringTransitionRecovered
		}, false},
		{"bad level: rejected", func(e *MonitoringEvent) { e.Level = "fatal" }, true},
		{"bad transition: rejected", func(e *MonitoringEvent) { e.Transition = "flapping" }, true},
		{"empty category: rejected", func(e *MonitoringEvent) { e.Category = "" }, true},
		{"empty source kind: rejected", func(e *MonitoringEvent) { e.Source.Kind = "" }, true},
		{"empty source name: rejected", func(e *MonitoringEvent) { e.Source.Name = "" }, true},
		{"empty condition: rejected", func(e *MonitoringEvent) { e.Condition = "" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := validMonitoringEvent()
			tc.mutate(&ev)
			err := ev.Validate()
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestMonitoringEvent_JSONRoundtrip(t *testing.T) {
	in := validMonitoringEvent()
	data, err := json.Marshal(in)
	require.NoError(t, err, "marshal")
	var got MonitoringEvent
	require.NoError(t, json.Unmarshal(data, &got), "unmarshal")
	assert.Equal(t, in.Level, got.Level)
	assert.Equal(t, in.Source, got.Source)
	assert.Equal(t, in.Reason, got.Reason)
	assert.True(t, in.Timestamp.Equal(got.Timestamp), "Timestamp must survive JSON roundtrip")
}

func TestPublishMonitoring(t *testing.T) {
	t.Run("valid event: publishes on MonitoringEventSubject", func(t *testing.T) {
		var gotSubject string
		var gotData []byte
		publish := func(subject string, data []byte) error {
			gotSubject = subject
			gotData = data
			return nil
		}
		require.NoError(t, PublishMonitoring(publish, validMonitoringEvent()))
		assert.Equal(t, MonitoringEventSubject, gotSubject)
		var decoded MonitoringEvent
		require.NoError(t, json.Unmarshal(gotData, &decoded))
		assert.Equal(t, "AgentIdentity", decoded.Source.Kind)
	})

	t.Run("invalid event: returns error, does not publish", func(t *testing.T) {
		called := false
		publish := func(string, []byte) error { called = true; return nil }
		ev := validMonitoringEvent()
		ev.Category = ""
		assert.Error(t, PublishMonitoring(publish, ev))
		assert.False(t, called, "publish must not be called for an invalid event")
	})
}
