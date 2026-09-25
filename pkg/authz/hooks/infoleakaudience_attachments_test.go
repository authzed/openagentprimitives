package hooks_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// preResponseWithAttachments is preResponse plus the fact that the reply
// carries files.
func preResponseWithAttachments(text string) pipeline.Input {
	in := preResponse(text)
	in.Response.HasAttachments = true
	return in
}

// The respond-time gate measures the reply TEXT and nothing else. When the
// text is fully covered by verified tags, the fine-grained path returns and
// the coarse session-wide check never runs — so a fully-tagged sentence
// delivered alongside an untagged file was allowed on the strength of the
// sentence.
//
// Attachments are not a hypothetical side channel here: artifact#view
// resolves through parent->interact, so every channel member who can interact
// can open the render. And the artifact-preparation path carries no leakage
// hook at all, so nothing else measured them either.
//
// The fix keeps the stated invariant that tags only ever REFINE: when the
// gate cannot see part of what is being sent, the fine-grained refinement does
// not apply and the coarse floor decides, exactly as it does in coarse mode.
// Asserts WHICH BRANCH ran, not what it concluded — the coarse path's verdict
// for a given fixture is its own business, and pinning it here would be
// asserting something about the coarse path rather than about this guard.
func TestReplyWithAttachmentsFallsThroughToTheCoarsePath(t *testing.T) {
	h := audienceHook(t, hooks.InfoLeakAudienceDeps{
		LookupSubjects: func(context.Context, string, string) ([]string, error) {
			return documentReaders, nil
		},
		// An audience the per-datum path WOULD answer on, so if it ran we
		// would see its marker.
		FineGrained: fineGrainedDeps(true, map[string][]string{"ptt-doc": channelAudience}),
	})

	dec := h.Eval(context.Background(), preResponseWithAttachments(taggedPayload))

	assertNoPerDatumAudit(t, dec,
		"a reply carrying unmeasured attachments must be decided by the coarse path, whatever it decides")
}

// The same reply WITHOUT attachments still takes the fine-grained path, so the
// guard costs nothing in the ordinary case. Without this, a guard that simply
// disabled per-datum refinement everywhere would pass the test above.
func TestReplyWithoutAttachmentsStillUsesTheFineGrainedPath(t *testing.T) {
	h := audienceHook(t, hooks.InfoLeakAudienceDeps{
		LookupSubjects: func(context.Context, string, string) ([]string, error) {
			return documentReaders, nil
		},
		FineGrained: fineGrainedDeps(true, map[string][]string{"ptt-doc": channelAudience}),
	})

	dec := h.Eval(context.Background(), preResponse(taggedPayload))

	var sawPerDatum bool
	for _, a := range dec.Audit {
		if strings.Contains(a.Kind, "_per_datum") {
			sawPerDatum = true
		}
	}
	assert.True(t, sawPerDatum,
		"an attachment-free reply must still be refined per-datum; a guard that disabled it everywhere would be a regression, not a fix")
}
