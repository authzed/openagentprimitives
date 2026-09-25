package fake

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// The fake kind implements AttachmentFetcher so bronzethread bundles can carry
// inbound files. Without it no bundle can exercise the attachment path at all:
// the gate's third leg is "the bound kind implements AttachmentFetcher", so a
// fake that does not implement it can only ever produce the gate-closed
// outcome, whatever the Channel and AgentClass say.
var _ channelkinds.AttachmentFetcher = (*Kind)(nil)

// attachmentSource serves the bytes for an externalID.
//
// Package-level and test-only: the fake kind exists to be driven by tests and
// has no transport behind it to fetch from. Guarded by a mutex because
// channelsd dispatches each Channel on its own goroutine and the fetch happens
// inline on it.
//
// NOT parallel-safe across tests that install DIFFERENT sources — the same
// caveat AGENTS.md records for refresh.SetHTTPClient. Install it with
// t.Cleanup(ResetAttachmentSource).
var (
	attachmentMu     sync.RWMutex
	attachmentSource func(externalID string) (io.ReadCloser, error)
)

// SetAttachmentSource installs the byte source FetchAttachment serves from.
func SetAttachmentSource(fn func(externalID string) (io.ReadCloser, error)) {
	attachmentMu.Lock()
	defer attachmentMu.Unlock()
	attachmentSource = fn
}

// ResetAttachmentSource removes it, restoring "this kind has nothing to serve".
func ResetAttachmentSource() { SetAttachmentSource(nil) }

// AttachmentBounds mirrors a generous real transport on purpose: the point of
// the fake is to exercise the PIPELINE's own clamping — effectiveLimit clamps
// to the operator's ceiling regardless of what a kind advertises — not to
// impose limits of its own that would mask it.
func (Kind) AttachmentBounds() channelkinds.AttachmentBounds {
	return channelkinds.AttachmentBounds{MaxSizeBytes: 25 << 20, MaxPerMessage: 10}
}

// FetchAttachment serves externalID from the installed source.
//
// With no source installed this ERRORS rather than returning empty bytes: a
// silent empty read looks like a successful fetch of a 0-byte file and sends
// the pipeline down outcomeStored, which is a materially different bug to
// diagnose than "the test forgot to install a source".
func (Kind) FetchAttachment(_ context.Context, _ channelkinds.Deps, externalID string) (io.ReadCloser, error) {
	attachmentMu.RLock()
	fn := attachmentSource
	attachmentMu.RUnlock()
	if fn == nil {
		return nil, fmt.Errorf("fake: no attachment source installed for %q; call fake.SetAttachmentSource", externalID)
	}
	return fn(externalID)
}
