package meta_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// erroringKG is a KGQuerier whose every mode fails with a fixed error, so a
// test can drive query_knowledge's four error arms from one fixture.
type erroringKG struct{ err error }

func (erroringKG) Ingest(_ context.Context, _ memory.KGInput) error { return nil }
func (k erroringKG) SearchFacts(_ context.Context, _ string, _ int) ([]memory.KGFact, error) {
	return nil, k.err
}
func (k erroringKG) GetEntity(_ context.Context, _ string) (*memory.KGEntity, error) {
	return nil, k.err
}
func (k erroringKG) EntityFacts(_ context.Context, _ string) ([]memory.KGFact, error) {
	return nil, k.err
}
func (k erroringKG) RelatedEntities(_ context.Context, _ string, _ int) ([]memory.KGEntity, error) {
	return nil, k.err
}
func (k erroringKG) Communities(_ context.Context, _ string) ([]memory.KGCommunity, error) {
	return nil, k.err
}

// invalidQueryErr reproduces the error memory.validateFieldPaths returns for an
// unresolvable FieldEquals path: entirely platform-authored (the caller's own
// path plus the kind's known content keys) and carrying the "did you mean"
// recovery hint the model needs to fix its next call in ONE turn instead of
// burning the memory HTTP client's ~17s of retries.
func invalidQueryErr() error {
	return fmt.Errorf("memory: query: FieldEquals path %q is unresolvable for kinds [turn]: did you mean %q? (known keys: content, outcome)",
		"outcom", "outcome")
}

// TestMemoryRecallTools_PlatformSentinelErrors_StayTrusted is the regression
// test for the "a guard can withhold the recovery hint" defect.
//
// Untrusting the memory/KG error arms was right for provider/transport errors —
// the graphiti providers interpolate up to 1KiB of upstream response body into
// theirs. But the same return also carries the memory sentinels whose text is
// authored entirely inside this repo: ErrInvalidQuery's "did you mean %q?",
// ErrMissingApproval's permission refusal, ErrKGScopeMismatch's policy refusal.
// The rule those returns are supposed to follow is that a fixed platform string
// keeps Trusted so a content guard cannot withhold it; with a Block/Approve
// inspector (or a transient inspector error, which also fails closed) the model
// would otherwise see "content withheld" and could never learn the field name it
// got wrong — and, because it never learns, it repeats the same bad query.
func TestMemoryRecallTools_PlatformSentinelErrors_StayTrusted(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")

	cases := []struct {
		name string
		exec func(t *testing.T) tool.Result
		// want is a fragment of the platform text that must survive.
		want string
	}{
		{
			name: "query_memory ErrInvalidQuery: Trusted=true so the did-you-mean hint cannot be withheld",
			want: "did you mean",
			exec: func(t *testing.T) tool.Result {
				t.Helper()
				fm := &fakeMemSearcher{err: fmt.Errorf("%w: %s", memory.ErrInvalidQuery, invalidQueryErr())}
				sess := &tool.SessionContext{Namespace: "default", Name: "sess1", Mem: fm}
				res, err := meta.NewQueryMemory().Execute(ctx,
					json.RawMessage(`{"field_filters":[{"path":"outcom","value":"ok"}]}`), sess)
				require.NoError(t, err)
				return res
			},
		},
		{
			name: "query_memory ErrMissingApproval: Trusted=true so the refusal reaches the model",
			want: "missing capability approval",
			exec: func(t *testing.T) tool.Result {
				t.Helper()
				fm := &fakeMemSearcher{err: fmt.Errorf("%w: perm=read_memory resource=default/other", memory.ErrMissingApproval)}
				sess := &tool.SessionContext{Namespace: "default", Name: "sess1", Mem: fm}
				res, err := meta.NewQueryMemory().Execute(ctx, json.RawMessage(`{"kinds":["turn"]}`), sess)
				require.NoError(t, err)
				return res
			},
		},
		{
			name: "search_memory ErrInvalidQuery: Trusted=true so the did-you-mean hint cannot be withheld",
			want: "did you mean",
			exec: func(t *testing.T) tool.Result {
				t.Helper()
				fm := &fakeMemSearcher{err: fmt.Errorf("%w: %s", memory.ErrInvalidQuery, invalidQueryErr())}
				sess := &tool.SessionContext{Namespace: "default", Name: "sess1", Mem: fm}
				res, err := meta.NewSearchMemory().Execute(ctx, json.RawMessage(`{"text":"x"}`), sess)
				require.NoError(t, err)
				return res
			},
		},
		{
			name: "search_memory ErrMissingApproval: Trusted=true so the refusal reaches the model",
			want: "missing capability approval",
			exec: func(t *testing.T) tool.Result {
				t.Helper()
				fm := &fakeMemSearcher{err: fmt.Errorf("%w: perm=read_memory resource=default/other", memory.ErrMissingApproval)}
				sess := &tool.SessionContext{Namespace: "default", Name: "sess1", Mem: fm}
				res, err := meta.NewSearchMemory().Execute(ctx, json.RawMessage(`{"text":"x"}`), sess)
				require.NoError(t, err)
				return res
			},
		},
		{
			name: "query_knowledge ErrInvalidQuery on a non-UUID entity: Trusted=true so the model can correct it",
			want: "invalid query",
			exec: func(t *testing.T) tool.Result {
				t.Helper()
				kg := erroringKG{err: fmt.Errorf("%w: entity uuid %q is not UUID-shaped", memory.ErrInvalidQuery, "bob")}
				sess := &tool.SessionContext{Namespace: "default", Name: "sess1", KG: kg}
				res, err := meta.NewQueryKnowledge().Execute(ctx,
					json.RawMessage(`{"mode":"entity_facts","entity_uuid":"bob"}`), sess)
				require.NoError(t, err)
				return res
			},
		},
		{
			name: "query_knowledge ErrKGScopeMismatch: Trusted=true so the policy refusal reaches the model",
			want: "not in this session's graph",
			exec: func(t *testing.T) tool.Result {
				t.Helper()
				kg := erroringKG{err: memory.ErrKGScopeMismatch}
				sess := &tool.SessionContext{Namespace: "default", Name: "sess1", KG: kg}
				res, err := meta.NewQueryKnowledge().Execute(ctx,
					json.RawMessage(`{"mode":"related","entity_uuid":"6f1c1d2e-0000-4000-8000-000000000000"}`), sess)
				require.NoError(t, err)
				return res
			},
		},
		{
			name: "query_knowledge ErrKGUnsupported: Trusted=true so the model learns the mode has no data rather than retrying it",
			want: "not supported",
			exec: func(t *testing.T) tool.Result {
				t.Helper()
				kg := erroringKG{err: memory.ErrKGUnsupported}
				sess := &tool.SessionContext{Namespace: "default", Name: "sess1", KG: kg}
				res, err := meta.NewQueryKnowledge().Execute(ctx,
					json.RawMessage(`{"mode":"communities"}`), sess)
				require.NoError(t, err)
				return res
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.exec(t)
			require.True(t, res.IsError, "precondition: the refusal surfaced as an error result")
			assert.Contains(t, res.Content, tc.want, "precondition: the platform text is in the result")
			assert.True(t, res.Trusted,
				"a memory sentinel's message is authored entirely by this codebase; a content guard must not be able to withhold it")
		})
	}
}

// TestMemoryRecallTools_ProviderErrors_StayUntrusted is the coverage-parity half:
// splitting the sentinel arms out must NOT quietly trust the provider/transport
// errors, which interpolate up to 1KiB of an upstream HTTP response body.
// query_knowledge is covered here for the first time (query_memory and
// search_memory are also covered by TestMemoryRecallTools_BackendErrorText_IsUntrusted).
func TestMemoryRecallTools_ProviderErrors_StayUntrusted(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	upstream := "graphiti: status 500: " + injectedText

	cases := []struct {
		name string
		args string
	}{
		{name: "search mode provider error: Trusted=false", args: `{"mode":"search","text":"x"}`},
		{name: "entity_facts mode provider error: Trusted=false", args: `{"mode":"entity_facts","entity_uuid":"e1"}`},
		{name: "related mode provider error: Trusted=false", args: `{"mode":"related","entity_uuid":"e1"}`},
		{name: "communities mode provider error: Trusted=false", args: `{"mode":"communities"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kg := erroringKG{err: errString(upstream)}
			sess := &tool.SessionContext{Namespace: "default", Name: "sess1", KG: kg}
			res, err := meta.NewQueryKnowledge().Execute(ctx, json.RawMessage(tc.args), sess)
			require.NoError(t, err)
			require.True(t, res.IsError, "precondition: the provider error surfaced as an error result")
			assert.Contains(t, res.Content, injectedText, "precondition: the upstream text reaches the model verbatim")
			assert.False(t, res.Trusted,
				"an error string carrying upstream bytes is not framework-authored and must be inspected")
		})
	}
}
