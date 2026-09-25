package registry_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	"github.com/authzed/openagentprimitives/pkg/channels/channelassets/registry"
)

type stubRenderer struct {
	kind     string
	output   string
	delivery channelassets.DeliveryMode
}

func (s stubRenderer) Kind() string { return s.kind }
func (stubRenderer) ExecutionMode() channelassets.ExecutionMode {
	return channelassets.ExecutionModeInProcess
}
func (s stubRenderer) Delivery() channelassets.DeliveryMode { return s.delivery }
func (stubRenderer) MaxInputSize() int64                    { return 256 << 10 }
func (stubRenderer) MaxOutputSize() int64                   { return 1 << 20 }
func (s stubRenderer) OutputMIMEs() []string                { return []string{s.output} }
func (stubRenderer) InputMIMEs() []string                   { return []string{"text/html"} }
func (stubRenderer) SupportsLiveView() bool                 { return false }
func (stubRenderer) AgentSelectable() bool                  { return true }
func (stubRenderer) Instructions() string                   { return "" }
func (stubRenderer) ServeTransform(content []byte) []byte   { return content }
func (stubRenderer) Render(context.Context, channelassets.Input) (channelassets.Output, error) {
	return channelassets.Output{}, nil
}

func TestRegistry_RegisterAndByKind(t *testing.T) {
	registry.Reset()
	registry.Register(stubRenderer{kind: "html", output: "text/html"})
	r, ok := registry.ByKind("html")
	require.True(t, ok, "ByKind(html) must succeed")
	assert.Equal(t, "html", r.Kind(), "Kind returned")
	_, ok = registry.ByKind("missing")
	assert.False(t, ok, "ByKind(missing) should miss")
}

func TestRegistry_RegisterPanicsOnDuplicate(t *testing.T) {
	registry.Reset()
	registry.Register(stubRenderer{kind: "html", output: "text/html"})
	assert.Panics(t, func() {
		registry.Register(stubRenderer{kind: "html", output: "text/html"})
	}, "duplicate registration must panic")
}

func TestRegistry_RegisterPanicsOnDeniedOutputMIME(t *testing.T) {
	registry.Reset()
	assert.Panics(t, func() {
		registry.Register(stubRenderer{kind: "svg-evil", output: "image/svg+xml"})
	}, "denied OutputMIME must panic")
}

func TestRegister_DeniedOutputMIME_BundledOnlyAllowed(t *testing.T) {
	registry.Reset()
	t.Cleanup(registry.Reset)
	assert.NotPanics(t, func() {
		registry.Register(stubRenderer{kind: "svg", output: "image/svg+xml", delivery: channelassets.DeliveryBundledOnly})
	})
}

// TestMatchAssetCapability exercises the cap-string matching used by
// channel kinds to advertise which asset MIMEs they accept. Rows
// declare a capability list, a candidate MIME, and the expected match
// result. Bundling exact + wildcard + denied-MIME cases into one
// matrix keeps each row focused on a single semantic.
func TestMatchAssetCapability(t *testing.T) {
	cases := []struct {
		name    string
		caps    []string
		mime    string
		want    bool
		comment string
	}{
		{"exact match: text/html", []string{"text", "markdown", "asset:text/html", "asset:image/png"}, "text/html", true, ""},
		{"exact match: image/png", []string{"text", "markdown", "asset:text/html", "asset:image/png"}, "image/png", true, ""},
		{"no match: application/pdf not listed", []string{"text", "markdown", "asset:text/html", "asset:image/png"}, "application/pdf", false, ""},
		{"wildcard: image/png matches asset:image/*", []string{"asset:image/*"}, "image/png", true, ""},
		{"wildcard: image/jpeg matches asset:image/*", []string{"asset:image/*"}, "image/jpeg", true, ""},
		{"wildcard does NOT match text/html under asset:image/*", []string{"asset:image/*"}, "text/html", false, ""},
		{"wildcard does NOT match denied image/svg+xml", []string{"asset:image/*"}, "image/svg+xml", false, "denied MIME bypasses wildcard"},
		{"explicit asset:image/svg+xml matches", []string{"asset:image/svg+xml"}, "image/svg+xml", true, "explicit override of deny-list"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := registry.MatchAssetCapability(tc.caps, tc.mime)
			assert.Equalf(t, tc.want, got, "caps=%v mime=%s (%s)", tc.caps, tc.mime, tc.comment)
		})
	}
}
