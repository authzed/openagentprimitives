package runner

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/toolenvelope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	memoryinmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/pttagcontent"
)

func putPtContent(t *testing.T, m memory.Memory, scope memory.Scope, tagID, content string) {
	t.Helper()
	body, err := json.Marshal(pttagcontent.ContentRecord{
		TagID: tagID, Content: content, MIME: "application/json", StoredAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	_, err = m.Put(memory.WithSystemApproval(context.Background(), "test"), memory.Entry{
		Scope: scope, Kind: pttagcontent.KindName, ID: memory.NewID(pttagcontent.Kind{}), Content: body,
	})
	require.NoError(t, err)
}

// TestVerifyPayloadTags_contentBinding is the C1 defense: a nonce-matched region
// only counts if its content matches what the platform stored for its id, so a
// model cannot pair a witnessed wide id with fabricated (sensitive) content.
func TestVerifyPayloadTags_contentBinding(t *testing.T) {
	m := memory.NewLocal(memoryinmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/s"}
	l := &Loop{Mem: m, SessionKey: memory.NamespacedName{Namespace: "ns", Name: "s"}}
	// verifyPayloadTags now asks the operator's verify route (component-side) to
	// content-bind, because pt_tag_content is component-read. This closure is that
	// route in-process — the same read + shared pttagcontent.Bind the real handler
	// (pkg/memory/httpsrv/pttagverify.go) and the e2e harness use.
	l.PtTagVerify = func(ctx context.Context, regions []memory.PtTagRegion) (memory.PtTagVerifyResponse, error) {
		recs, err := pttagcontent.List(memory.WithSystemApproval(memory.WithoutTokenSession(ctx), "pt_egress_verify"), l.Mem, scope)
		if err != nil {
			return memory.PtTagVerifyResponse{}, err
		}
		return pttagcontent.Bind(recs, regions), nil
	}
	putPtContent(t, m, scope, "pt_public", "the public quarterly figures")
	putPtContent(t, m, scope, "pt_narrow", "the secret merger terms")

	// Verbatim reuse of a real datum under its real id → verified.
	genuine := toolenvelope.WrapPt("the public quarterly figures", "n1", "pt_public")
	ids, covered := l.verifyPayloadTags(context.Background(), genuine)
	assert.True(t, covered)
	assert.Equal(t, []string{"pt_public"}, ids)

	// LAUNDERING ATTEMPT: a region with the real WIDE id but the SECRET content.
	// Content binding rejects it → coarse floor. This is the whole point.
	forged := toolenvelope.WrapPt("the secret merger terms", "n2", "pt_public")
	_, covered = l.verifyPayloadTags(context.Background(), forged)
	assert.False(t, covered, "content that does not match its claimed id must not bind (C1 defense)")

	// An id the platform never minted → not covered.
	ghost := toolenvelope.WrapPt("whatever", "n3", "pt_ghost")
	_, covered = l.verifyPayloadTags(context.Background(), ghost)
	assert.False(t, covered)

	// No memory to verify against → fail-closed to the coarse floor.
	bare := &Loop{SessionKey: memory.NamespacedName{Namespace: "ns", Name: "s"}}
	_, covered = bare.verifyPayloadTags(context.Background(), genuine)
	assert.False(t, covered)
}
