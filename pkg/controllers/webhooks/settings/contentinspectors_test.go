package settings_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard/registry"
	settings "github.com/authzed/openagentprimitives/pkg/controllers/webhooks/settings"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"

	_ "github.com/authzed/openagentprimitives/pkg/authz/contentguard/kinds/urlallowlist"
)

func ci(id, cfg string) v1.ContentInspectorConfig {
	return v1.ContentInspectorConfig{ID: id, Config: apiextv1.JSON{Raw: json.RawMessage(cfg)}}
}

// fakeDetectorInspector is a test-only inspector that implements
// DetectorProvider, used to exercise the "at most one detector" admission
// guard (today only prompt-injection ships as a real DetectorProvider).
type fakeDetectorInspector struct{ id string }

func (f fakeDetectorInspector) ID() string { return f.id }
func (f fakeDetectorInspector) Configure(json.RawMessage) (contentguard.Instance, error) {
	return fakeDetectorInstance{}, nil
}
func (f fakeDetectorInspector) Detector(json.RawMessage) (*contentguard.DetectorSpec, error) {
	return &contentguard.DetectorSpec{Image: "example.com/detector:test", Port: 9999}, nil
}

type fakeDetectorInstance struct{}

func (fakeDetectorInstance) Points() []pipeline.Point { return []pipeline.Point{pipeline.PostToolCall} }
func (fakeDetectorInstance) Inspect(context.Context, contentguard.Subject) (contentguard.Finding, error) {
	return contentguard.Finding{Action: contentguard.Pass}, nil
}

// registerFakeDetectors registers detector-providing fakes into the
// process-wide registry for the duration of the test, alongside the
// already-registered url-allowlist (blank import). On cleanup the registry is
// restored to exactly its pre-test contents — captured before any mutation so a
// later Reset can faithfully rebuild it (the blank import's init() runs once).
func registerFakeDetectors(t *testing.T, ids ...string) {
	t.Helper()
	preexisting := registry.All() // capture before mutating (incl. url-allowlist)
	for _, id := range ids {
		registry.Register(fakeDetectorInspector{id: id})
	}
	t.Cleanup(func() {
		registry.Reset()
		for _, insp := range preexisting {
			registry.Register(insp)
		}
	})
}

// TestContentInspectorsError_atMostOneDetector exercises the multi-detector
// admission guard: two inspectors that both implement DetectorProvider is
// rejected (they would shadow each other on the single
// CONTENTGUARD_DETECTOR_ENDPOINT env var); one is accepted.
func TestContentInspectorsError_atMostOneDetector(t *testing.T) {
	cases := []struct {
		name     string
		register []string
		entries  []v1.ContentInspectorConfig
		wantErr  string
	}{
		{
			name:     "two detector-providing inspectors: rejected",
			register: []string{"det-a", "det-b"},
			entries:  []v1.ContentInspectorConfig{ci("det-a", `{}`), ci("det-b", `{}`)},
			wantErr:  "at most one detector-providing content inspector",
		},
		{
			name:     "one detector-providing inspector: ok",
			register: []string{"det-a"},
			entries:  []v1.ContentInspectorConfig{ci("det-a", `{}`)},
			wantErr:  "",
		},
		{
			name:     "one detector + one non-detector (url-allowlist): ok",
			register: []string{"det-a"},
			entries: []v1.ContentInspectorConfig{
				ci("det-a", `{}`),
				ci("url-allowlist", `{"rules":[{"domain":"x.com","action":"allow"}]}`),
			},
			wantErr: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registerFakeDetectors(t, tc.register...)
			ents := tc.entries
			spec := &v1.SettingsSpec{Limits: &v1.SettingsLimits{ContentInspectors: &ents}}
			got := settings.ContentInspectorsError(spec)
			if tc.wantErr == "" {
				assert.Empty(t, got)
			} else {
				assert.Contains(t, got, tc.wantErr)
				// Both offending ids must be named in the error.
				for _, id := range tc.register {
					assert.Contains(t, got, id, "error must list the offending detector id %q", id)
				}
			}
		})
	}
}

func TestContentInspectorsError(t *testing.T) {
	cases := []struct {
		name    string
		entries []v1.ContentInspectorConfig
		wantErr string
	}{
		{
			name:    "valid url-allowlist: empty error",
			entries: []v1.ContentInspectorConfig{ci("url-allowlist", `{"rules":[{"domain":"x.com","action":"allow"}]}`)},
			wantErr: "",
		},
		{
			name:    "unregistered id: denied",
			entries: []v1.ContentInspectorConfig{ci("nope", `{}`)},
			wantErr: "not registered",
		},
		{
			name:    "bad config (empty rules): denied",
			entries: []v1.ContentInspectorConfig{ci("url-allowlist", `{"rules":[]}`)},
			wantErr: "invalid config",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ents := tc.entries
			spec := &v1.SettingsSpec{Limits: &v1.SettingsLimits{ContentInspectors: &ents}}
			got := settings.ContentInspectorsError(spec)
			if tc.wantErr == "" {
				assert.Empty(t, got)
			} else {
				assert.Contains(t, got, tc.wantErr)
			}
		})
	}
}
