package tool_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
	uitool "github.com/authzed/openagentprimitives/pkg/web/uibindings/tool"
	"github.com/authzed/openagentprimitives/pkg/web/uiselect"
)

type fakeDeps struct {
	req func(subject string, data []byte, timeout time.Duration) ([]byte, error)
}

func (f fakeDeps) Memory() memory.Memory                                   { return nil }
func (f fakeDeps) Artifacts() *artifacts.Service                           { return nil }
func (f fakeDeps) NATSRequest() channelevents.RequestFunc                  { return f.req }
func (f fakeDeps) ArtifactRenderBytes() uibindings.ArtifactRenderBytesFunc { return nil }
func (f fakeDeps) Logger() logr.Logger                                     { return logr.Discard() }

func reply(t *testing.T, resp channelevents.AppToolCallResponse) []byte {
	t.Helper()
	b, err := json.Marshal(resp)
	require.NoError(t, err)
	return b
}

func TestToolResolver(t *testing.T) {
	r := uitool.New()
	assert.Equal(t, "tool", r.Source())

	t.Run("subject and normalized tool name reach the runner; a JSON-text result is unwrapped", func(t *testing.T) {
		var gotSubject string
		var gotReq channelevents.AppToolCallRequest
		d := fakeDeps{req: func(subject string, data []byte, _ time.Duration) ([]byte, error) {
			gotSubject = subject
			var env channelevents.Envelope
			require.NoError(t, json.Unmarshal(data, &env))
			require.NoError(t, json.Unmarshal(env.Payload, &gotReq))
			return reply(t, channelevents.AppToolCallResponse{
				Status: channelevents.AppToolCallStatusOK,
				Result: json.RawMessage(`"[{\"name\":\"Acme\"}]"`),
			}), nil
		}}
		res, err := r.Resolve(context.Background(), d, uibindings.Request{
			Namespace: "demo-ns", Session: "demo-session", Subject: "user:abc",
			Path: "root/#rows", Ref: "crm_listLeads", Args: json.RawMessage(`{"span":"30d"}`),
		})
		require.NoError(t, err)
		assert.Equal(t, "ap.session.demo-ns.demo-session.in.ui_data_binding", gotSubject)
		assert.Equal(t, "crm_listleads", gotReq.ToolName, "the ref is normalized to the AppTools registry key")
		assert.Equal(t, "user:abc", gotReq.Requester)
		assert.NotEmpty(t, gotReq.RequestID)
		assert.JSONEq(t, `[{"name":"Acme"}]`, string(res.Value),
			"a tool that returns JSON as text is unwrapped so ap:table can bind rows")
	})

	t.Run("a non-JSON text result stays a JSON string", func(t *testing.T) {
		d := fakeDeps{req: func(string, []byte, time.Duration) ([]byte, error) {
			return reply(t, channelevents.AppToolCallResponse{
				Status: channelevents.AppToolCallStatusOK,
				Result: json.RawMessage(`"all good"`),
			}), nil
		}}
		res, err := r.Resolve(context.Background(), d, uibindings.Request{
			Namespace: "demo-ns", Session: "demo-session", Ref: "t",
		})
		require.NoError(t, err)
		assert.JSONEq(t, `"all good"`, string(res.Value))
	})

	// The two subtests below pin the FULL unwrapResult -> uiselect.Apply
	// composition that pkg/web/webui/agentui/bindings.go's resolveOneBinding
	// chains at runtime (this package's own Resolve stops at unwrapResult;
	// nothing here calls uiselect otherwise). No fake stands between them —
	// task-9-brief.md's own point is that nothing before the e2e exercises
	// this pair together.
	t.Run("an envelope Result composes with a selector to extract records", func(t *testing.T) {
		d := fakeDeps{req: func(string, []byte, time.Duration) ([]byte, error) {
			return reply(t, channelevents.AppToolCallResponse{
				Status: channelevents.AppToolCallStatusOK,
				Result: json.RawMessage(`"{\"results\":[{\"properties\":{\"name\":\"Acme\"}}],\"total\":1}"`),
			}), nil
		}}
		res, err := r.Resolve(context.Background(), d, uibindings.Request{
			Namespace: "demo-ns", Session: "demo-session", Ref: "t",
		})
		require.NoError(t, err)
		assert.JSONEq(t, `{"results":[{"properties":{"name":"Acme"}}],"total":1}`, string(res.Value),
			"unwrapResult must return the envelope OBJECT, not a quoted string, for select to apply to")

		sel, err := uiselect.Parse("results[].properties")
		require.NoError(t, err)
		got, err := sel.Apply(res.Value)
		require.NoError(t, err)
		assert.JSONEq(t, `[{"name":"Acme"}]`, string(got))
	})

	t.Run("a prose Result composes with a selector to fail loudly, not silently bind the string", func(t *testing.T) {
		d := fakeDeps{req: func(string, []byte, time.Duration) ([]byte, error) {
			return reply(t, channelevents.AppToolCallResponse{
				Status: channelevents.AppToolCallStatusOK,
				Result: json.RawMessage(`"all good"`),
			}), nil
		}}
		res, err := r.Resolve(context.Background(), d, uibindings.Request{
			Namespace: "demo-ns", Session: "demo-session", Ref: "t",
		})
		require.NoError(t, err)

		sel, err := uiselect.Parse("results[].properties")
		require.NoError(t, err)
		_, err = sel.Apply(res.Value)
		require.Error(t, err)
		assert.ErrorIs(t, err, uiselect.ErrNotObject,
			"a selector against a resolved STRING must fail loudly, never silently bind the string")
	})

	cases := []struct {
		name    string
		resp    channelevents.AppToolCallResponse
		wantErr string
	}{
		{name: "denied surfaces the runner's viewer-authored copy", resp: channelevents.AppToolCallResponse{
			Status:  channelevents.AppToolCallStatusDenied,
			Message: "this view can only read data", ViewerMessage: "this view can only read data"},
			wantErr: "this view can only read data"},
		{name: "not_found reads as a dead binding, not a crash", resp: channelevents.AppToolCallResponse{
			Status: channelevents.AppToolCallStatusNotFound}, wantErr: "not available"},
		{name: "rate_limited surfaces a retry hint", resp: channelevents.AppToolCallResponse{
			Status:  channelevents.AppToolCallStatusRateLimited,
			Message: "try again shortly", ViewerMessage: "try again shortly"},
			wantErr: "try again shortly"},
		{name: "requires_approval is impossible on this path and is an error, not a hang",
			resp:    channelevents.AppToolCallResponse{Status: channelevents.AppToolCallStatusRequiresApproval},
			wantErr: "cannot be read"},
		{name: "a tool-level error result is an error, not a rendered payload",
			resp:    channelevents.AppToolCallResponse{Status: channelevents.AppToolCallStatusOK, IsError: true, Result: json.RawMessage(`"boom"`)},
			wantErr: "boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := fakeDeps{req: func(string, []byte, time.Duration) ([]byte, error) { return reply(t, tc.resp), nil }}
			_, err := r.Resolve(context.Background(), d, uibindings.Request{
				Namespace: "demo-ns", Session: "demo-session", Ref: "t",
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}

	// Every deny/rate-limit reply that did NOT author its own viewer copy
	// carries a Message written for somewhere else — appToolResponseFor relays
	// the containment pipeline's deny hook reason verbatim, and those reasons
	// name SpiceDB permissions, resource types and IDs, CRD field names, and
	// the raw subject. None of that may reach a browser (AGENTS.md: no
	// internal ops in user messages). The empty ViewerMessage is the signal,
	// and it is the ZERO value, so a future reply site that forgets to author
	// viewer copy fails safe instead of leaking.
	t.Run("a deny reason authored by the containment pipeline never reaches the browser", func(t *testing.T) {
		leaky := []struct {
			name string
			resp channelevents.AppToolCallResponse
		}{
			{name: "a SpiceDB permission, resource ID, and subject", resp: channelevents.AppToolCallResponse{
				Status:  channelevents.AppToolCallStatusDenied,
				Message: "permission denied: user:alice@example.com does not have read on github_repo:spicedb"}},
			{name: "a session-scope resource ID", resp: channelevents.AppToolCallResponse{
				Status:  channelevents.AppToolCallStatusDenied,
				Message: "session scope: access to document:acct-42 is disallowed for this session"}},
			{name: "a CRD field name", resp: channelevents.AppToolCallResponse{
				Status:  channelevents.AppToolCallStatusDenied,
				Message: `info-leakage: tool "crm_list_leads" has no toolResourceMap entry and is not marked noTaint`}},
			{name: "LLM-directed toolguard copy on the rate-limited arm", resp: channelevents.AppToolCallResponse{
				Status:  channelevents.AppToolCallStatusRateLimited,
				Message: `tool "crm_list_leads": the call was blocked because its arguments exceeded the 4096-byte outbound limit. Send less data`}},
		}
		for _, tc := range leaky {
			t.Run(tc.name, func(t *testing.T) {
				d := fakeDeps{req: func(string, []byte, time.Duration) ([]byte, error) { return reply(t, tc.resp), nil }}
				_, err := r.Resolve(context.Background(), d, uibindings.Request{
					Namespace: "demo-ns", Session: "demo-session", Ref: "t",
				})
				require.Error(t, err)
				assert.NotContains(t, err.Error(), tc.resp.Message,
					"a Message with no ViewerMessage is operator copy; it must be logged, never rendered")
				for _, leak := range []string{"permission denied", "github_repo:", "document:acct-42", "toolResourceMap", "alice@example.com", "4096-byte"} {
					assert.NotContains(t, err.Error(), leak, "internal vocabulary reached the browser")
				}
				assert.NotEmpty(t, err.Error(), "the viewer still gets a cause, just a safe one")
			})
		}
	})

	t.Run("a nil NATSRequest fails closed with a returned error", func(t *testing.T) {
		_, err := r.Resolve(context.Background(), fakeDeps{}, uibindings.Request{Ref: "t"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "NATSRequest is not configured",
			"the resolver's own guard fires, not just channelevents.RequestIn's fallback nil check")
	})
}
