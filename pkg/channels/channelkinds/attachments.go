package channelkinds

import (
	"context"
	"io"
)

// InboundAttachment is a reference to a file a user attached to an inbound
// message, recorded by the listener at delivery time. It carries no bytes —
// only enough to describe the attachment and, later, retrieve it.
//
// Populating InboundEvent.Attachments is deliberately UNGATED: noticing a file
// exists is always allowed, with no capability check and no Channel spec
// lookup. Whether the bytes are ever fetched is a separate downstream decision
// — a user who attaches a file must always get an agent that can at least say
// it saw one, even when nothing is configured to read it.
type InboundAttachment struct {
	// ExternalID is the kind-native, opaque handle FetchAttachment needs to
	// retrieve this attachment's bytes later (Slack: the file's ID, resolved
	// to a download URL inside FetchAttachment — never a URL itself, since
	// it is echoed into turns/logs/storage keys upstream of any fetch).
	ExternalID string

	// Filename is the user-supplied name, already sanitized by the listener
	// before it reached this struct (truncated, control characters
	// stripped) — it is untrusted input that may end up in a turn, a log
	// line, or a storage key.
	Filename string

	// MIME is the kind-reported content type (e.g. "image/png"). A hint from
	// the transport, not a verified fact — never trust it over sniffing the
	// bytes for anything security-relevant.
	MIME string

	// SizeBytes is the kind-reported size in bytes. May be 0 when the
	// transport didn't report one.
	SizeBytes int64
}

// AttachmentBounds are a kind's static, transport-level attachment limits.
// A caller with its own configured ceiling (a per-Channel setting) clamps
// to these rather than trusting the transport to serve anything larger.
type AttachmentBounds struct {
	// MaxSizeBytes is the largest single attachment this kind's transport
	// will serve.
	MaxSizeBytes int64
	// MaxPerMessage caps how many attachments from one inbound message this
	// kind will fetch.
	MaxPerMessage int
}

// AttachmentFetcher is implemented by channel kinds whose transport can
// download the bytes of a file a user attached. Optional and discovered by
// type assertion, so callers no-op rather than branching on kind.
//
// Fetching lives here rather than in the shared pipeline because only the kind
// holds the transport credential (Deps.Secret) a download authenticates with:
// the pipeline never sees a raw channel credential, and handing it one just to
// move attachment bytes would be one more place for it to leak from.
type AttachmentFetcher interface {
	// AttachmentBounds returns the static limits this kind enforces. MUST
	// NOT perform I/O — read at validation/gating time, not per fetch.
	AttachmentBounds() AttachmentBounds

	// FetchAttachment downloads the attachment identified by externalID (the
	// InboundAttachment.ExternalID recorded at listener time; kind-opaque).
	// Implementations MUST stream the result — return the live response
	// body as an io.ReadCloser rather than buffering the whole file into
	// memory — since attachments can be arbitrarily large and channelsd
	// serves every channel in the cluster from one process. The caller owns
	// closing the returned reader.
	FetchAttachment(ctx context.Context, deps Deps, externalID string) (io.ReadCloser, error)
}
