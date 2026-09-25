package runner

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/contentguardaudit"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// recordingInspector passes everything and remembers the exact text it was
// handed, so a test can assert what actually reached the detector.
type recordingInspector struct {
	mu   sync.Mutex
	seen string
}

func (*recordingInspector) Points() []pipeline.Point { return []pipeline.Point{pipeline.PostToolCall} }

func (r *recordingInspector) Inspect(_ context.Context, s contentguard.Subject) (contentguard.Finding, error) {
	r.mu.Lock()
	r.seen = s.Result
	r.mu.Unlock()
	return contentguard.Finding{Action: contentguard.Pass, Details: map[string]any{"score": 0.1}}, nil
}

func (r *recordingInspector) submitted() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen
}

// TestInspectUntrustedResult_CapsSubmittedContent covers the inverted-coverage
// defect: an uncapped submission meant the LARGEST recall payloads were the
// least likely to be scanned at all.
//
// Nothing else bounds a meta result — toolguard's ingress budget is unset by
// default and, when set, denies rather than trims; query_memory clamps ENTRIES,
// not bytes, and each entry is a whole stored turn. The prompt-injection
// inspector wraps its detector POST in a 1s timeout and, under its default
// onError=warn, converts the timeout into a Pass ("detector unavailable
// (warn-mode, not scanned)"). So the biggest, most injection-prone bodies
// reached the model unscanned.
//
// The result the MODEL sees is unchanged — only what is handed to the inspector
// is capped — and the shortfall is stamped on the durable contentguard_audit
// entry so the coverage gap is never silent. The cap itself lives in
// pkg/authz/contentguard (Capped), shared byte-for-byte with the gated pipeline path.
func TestInspectUntrustedResult_CapsSubmittedContent(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	// A recall body several times the cap: 100 entries of stored turns easily
	// runs to hundreds of KiB.
	big := strings.Repeat("stored turn text. ", 12000)
	require.Greater(t, len(big), contentguard.MaxInspectBytes, "precondition: the fixture must exceed the cap")

	rec := &recordingInspector{}
	mem := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "default/recall"}
	l := &Loop{
		SessionKey:        memory.NamespacedName{Namespace: "default", Name: "recall"},
		ContentInspectors: []contentguard.Instance{rec},
		Mem:               mem,
	}

	got := l.inspectUntrustedResult(ctx, "query_memory", tool.Result{Content: big})

	assert.Equal(t, big, got.Content, "capping is about what the DETECTOR sees; the model still gets the whole recall")
	submitted := rec.submitted()
	assert.LessOrEqual(t, len(submitted), contentguard.MaxInspectBytes,
		"an unbounded submission times the detector out, and warn-mode turns that into a Pass")
	assert.True(t, strings.HasPrefix(big, submitted), "the submitted text is a prefix of the result, not a re-encoding")

	// The unscanned tail is a real coverage gap, so it is recorded rather than
	// silently dropped.
	recs, err := contentguardaudit.List(ctx, mem, scope)
	require.NoError(t, err, "the content-guard audit entry must be readable")
	require.Len(t, recs, 1)
	var details map[string]any
	require.NoError(t, json.Unmarshal([]byte(recs[0].Details), &details))
	assert.EqualValues(t, len(big), details["content_bytes"])
	assert.EqualValues(t, len(submitted), details["inspected_bytes"])
}

// TestInspectUntrustedResult_UndersizedContentIsNotAnnotated pins the other
// side: a result inside the cap is submitted whole and carries no truncation
// counters, so their presence in the audit stream means exactly one thing.
func TestInspectUntrustedResult_UndersizedContentIsNotAnnotated(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	small := `{"entries":[{"kind":"turn","content":"hello"}]}`

	rec := &recordingInspector{}
	mem := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "default/recall"}
	l := &Loop{
		SessionKey:        memory.NamespacedName{Namespace: "default", Name: "recall"},
		ContentInspectors: []contentguard.Instance{rec},
		Mem:               mem,
	}

	l.inspectUntrustedResult(ctx, "query_memory", tool.Result{Content: small})
	assert.Equal(t, small, rec.submitted(), "content inside the cap is inspected whole")

	recs, err := contentguardaudit.List(ctx, mem, scope)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	var details map[string]any
	require.NoError(t, json.Unmarshal([]byte(recs[0].Details), &details))
	assert.NotContains(t, details, "inspected_bytes", "no truncation, no truncation counters")
}
