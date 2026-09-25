package runner

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/agent/toolenvelope"
	"github.com/authzed/openagentprimitives/pkg/authz/provenance"
)

// ptWrapResult wraps a tagged, non-error tool result in its pt-untrusted
// envelope — untrusted-data framing AND provenance id under ONE nonce, chosen
// here (after the content is fixed) so no byte of the result can forge the
// boundary — and reports handled=true. The model sees the tag and carries the
// datum's provenance forward when it reuses the content.
//
// It returns handled=false, leaving the caller to apply the plain untrusted
// wrap, when:
//   - the result is an error (a later PostToolCall hook may have swapped the
//     text for a denial; wrapping that under the datum's id would bind the id to
//     content the datum never delivered), or
//   - no tag was minted for this result (the fine-grained capability is off, or
//     the resource didn't resolve) — today's behaviour, byte-for-byte, or
//   - there is no ledger (a non-tagging code path).
//
// It gates itself: a tag lands in the ledger only when the (capability-gated)
// mint ran, so a class without the capability never takes the pt-untrusted path.
func ptWrapResult(ctx context.Context, useID, content string, isError bool) (string, bool) {
	if isError {
		return "", false
	}
	led := provenance.TagLedgerFrom(ctx)
	if led == nil {
		return "", false
	}
	tagID, ok := led.MintedFor(useID)
	if !ok {
		return "", false
	}
	return toolenvelope.WrapPt(content, newUntrustedOutputNonce(), tagID), true
}
