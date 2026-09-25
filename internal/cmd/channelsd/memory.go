package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpclient"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/channel_msg_ref"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/envelopefact"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/factcontent"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/triggerdelivery"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
)

// memoryClient talks to the operator's unified Kind-parameterized memory
// API. It is constructed once at startup and shared across every session
// channelsd touches — each Append/ReadAll builds a turn.Appender for the
// call's scope from the shared signing facade.
type memoryClient struct {
	baseURL string
	client  *httpclient.Client
	// signed wraps client with the channelsd component signer so every
	// append-only write (turn, …) is attested as "system:channelsd"
	// before reaching the operator's verify-on-write. Reads pass through
	// to client unchanged. The Signer maintains per-scope hash chains and
	// lazily seeds from memory, so it handles channelsd's fan-out across
	// many session scopes automatically.
	signed memory.Memory
	http   *http.Client
	// token is channelsd's system bearer, held here (in addition to being
	// baked into client's own transport) because UploadInboundAsset streams a
	// raw request body to a route outside httpclient.Client's JSON-only
	// /memory/... surface and so builds its own *http.Request.
	token string
	// uploadHTTP carries a generous client-level Timeout (unlike http, which
	// caps every request at 10s for the fast /healthz check) — an attachment
	// upload streams a whole file plus a server-side extraction round trip.
	// Nothing upstream of this client sets a deadline of its own, so a
	// stalled response body would otherwise leave this Do call — and the
	// Slack listener's single per-Channel dispatch goroutine that triggered
	// it — blocked forever. The caller (pkg/channels/channelsd/pipeline's
	// processAttachments) also bounds its own context (attachmentFetchTimeout),
	// so this Timeout is a second, independent backstop: it must never be
	// the only bound.
	uploadHTTP *http.Client
}

// uploadHTTPTimeout backstops uploadHTTP independently of whatever context
// pkg/channels/channelsd/pipeline's processAttachments passes in (attachmentFetchTimeout,
// 60s there) — deliberately larger, so the context deadline is expected to
// fire first in the normal case and this Timeout only bites if a caller ever
// forgets to bound its context.
const uploadHTTPTimeout = 90 * time.Second

// newMemoryClient builds the channelsd memory client. priv signs the
// component's append-only writes as "system:channelsd"; the matching key
// must already be registered with the operator (see RegisterPublisherKey
// in main).
func newMemoryClient(baseURL, token string, priv ed25519.PrivateKey) *memoryClient {
	client := httpclient.New(baseURL, token)
	return &memoryClient{
		baseURL:    baseURL,
		client:     client,
		signed:     provenance.NewSigningMemory(client, provenance.NewSigner(priv, "system:channelsd")),
		http:       &http.Client{Timeout: 10 * time.Second},
		token:      token,
		uploadHTTP: &http.Client{Timeout: uploadHTTPTimeout},
	}
}

// appenderFor builds a turn.Appender bound to the (ns, name) session scope
// over the SIGNING facade, so the append-only turn write is attested.
func (m *memoryClient) appenderFor(ns, name string) *turn.Appender {
	return turn.NewAppender(m.signed, memory.Scope{Kind: "session", ID: ns + "/" + name})
}

// Append writes a memory.Turn to the operator memory server via the turn
// Kind route. Index handling has two modes:
//   - When t.Index > 0 (inheritance-copy path): forward as-is, keeping the
//     turn's Role. Each copied turn keeps its source Index so it lands at
//     the same transcript position in the new session. turn.Appender.Append
//     is idempotent on a byte-identical (Index, Role) re-append, so
//     re-copying is safe.
//   - When t.Index == 0 (active-session reply path): write the inbound
//     human message under the distinct "inbox" role at max(Index)+1.
//
// The turn Kind keys entries on (Index, Role). The runner is the SOLE
// writer of "user"/"assistant" transcript turns — it appends a user-role
// tool_result turn every loop iteration — so channelsd MUST NOT write
// "user"-role turns for an active session: a max(Index)+1 race between
// channelsd's human reply and the runner's tool_result turn would collide
// on the (Index, "user") key and fail the session with
// MemoryUnavailable. Instead channelsd writes inbound messages under the
// distinct "inbox" role; the runner drains "inbox" turns into real
// "user" transcript turns at indices it controls (see the runner loop's
// drainInbox). A distinct role means the two writers can never share an
// (Index, Role) key, so the max(Index)+1 index assignment below cannot
// race with a runner write.
func (m *memoryClient) Append(ctx context.Context, ns, name string, t pipeline.MemTurn) error {
	idx := t.Index
	role := t.Role
	if idx == 0 {
		next, err := m.nextIndex(ctx, ns, name)
		if err != nil {
			return fmt.Errorf("memory append: compute next index: %w", err)
		}
		idx = next
		// Active-session reply: write under the distinct "inbox" role so
		// this write can never share an (Index, Role) key with a runner
		// "user"/"assistant" turn. The runner drains it into a real
		// "user" turn (loop.drainInbox).
		role = "inbox"
	}
	turnVal := memory.Turn{
		Index:     idx,
		Role:      role,
		Content:   convertContent(t.Content),
		CreatedAt: time.Now().UTC(),
		Author:    t.Author,
		Via:       t.Via,
	}
	if err := m.appenderFor(ns, name).Append(ctx, turnVal); err != nil {
		return fmt.Errorf("memory append: %w", err)
	}
	return nil
}

// nextIndex returns max(existing turn indices) + 1, or 0 if the session has
// no turns yet. Used by Append's auto-assign path on the active-session
// reply flow so the inbound "inbox"-role turn lands past every existing
// turn. The "inbox" role guarantees no (Index, Role) collision with a
// runner "user"/"assistant" turn even at a shared numeric index.
func (m *memoryClient) nextIndex(ctx context.Context, ns, name string) (int, error) {
	turns, err := m.ReadAll(ctx, ns, name)
	if err != nil {
		return 0, err
	}
	max := -1
	for _, t := range turns {
		if t.Index > max {
			max = t.Index
		}
	}
	return max + 1, nil
}

// ReadAll fetches all turns for the given session via channelsd's system
// bearer (which permits read-any per the httpsrv auth rules). Returns an
// empty slice (or nil) and nil error when the session has no turns. The
// HTTP server does not distinguish empty from non-existent sessions — both
// return an empty QueryResult.
func (m *memoryClient) ReadAll(ctx context.Context, ns, name string) ([]pipeline.MemTurn, error) {
	raw, err := m.appenderFor(ns, name).ReadAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("memory read: %w", err)
	}
	out := make([]pipeline.MemTurn, len(raw))
	for i, t := range raw {
		out[i] = pipeline.MemTurn{
			Index:   t.Index,
			Role:    t.Role,
			Content: convertContentBack(t.Content),
			Author:  t.Author,
			Via:     t.Via,
		}
	}
	return out, nil
}

// Healthz checks that the operator memory server is up.
func (m *memoryClient) Healthz(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.baseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("memory /healthz: HTTP %d", resp.StatusCode)
	}
	return nil
}

// convertContent maps pipeline.MemContent slices to memory.ContentBlock slices.
func convertContent(in []pipeline.MemContent) []memory.ContentBlock {
	out := make([]memory.ContentBlock, len(in))
	for i, c := range in {
		block := memory.ContentBlock{
			Type: c.Type,
			Text: c.Text,
		}
		if c.Type == "attachment" {
			block.Attachment = &memory.AttachmentBlock{
				Filename: c.Filename, MIME: c.MIME, SizeBytes: c.SizeBytes,
				Ref: c.Ref, TextRef: c.TextRef, Pages: c.Pages,
			}
		}
		out[i] = block
	}
	return out
}

// convertContentBack converts memory.ContentBlock → pipeline.MemContent.
// Inverse of convertContent. pipeline.MemContent only models Type, Text, and
// the attachment fields; tool_use/tool_result blocks' structured fields
// (ToolUse, ToolResult) are not preserved on this round-trip. For v1 memory
// inheritance this is acceptable — the model on resume reconstructs context
// from text turns, and tool history detail is mostly noise on a fresh
// continuation.
func convertContentBack(in []memory.ContentBlock) []pipeline.MemContent {
	out := make([]pipeline.MemContent, len(in))
	for i, b := range in {
		c := pipeline.MemContent{Type: b.Type, Text: b.Text}
		if b.Attachment != nil {
			c.Filename = b.Attachment.Filename
			c.MIME = b.Attachment.MIME
			c.SizeBytes = b.Attachment.SizeBytes
			c.Ref = b.Attachment.Ref
			c.TextRef = b.Attachment.TextRef
			c.Pages = b.Attachment.Pages
		}
		out[i] = c
	}
	return out
}

// inboundAssetResponse mirrors pkg/memory/httpsrv's POST /inbound-asset JSON
// response shape exactly. There is no shared type, so a change to either side
// MUST change the other.
type inboundAssetResponse struct {
	// Ref is the stored asset's handle, used to fetch the bytes back later.
	Ref string `json:"ref"`
	// Extracted reports whether text was recovered; false with Unsupported=false means a transient failure.
	Extracted bool `json:"extracted"`
	// TextRef is the handle for the extracted text; empty when nothing was extracted.
	TextRef string `json:"textRef,omitempty"`
	// Pages is the page count for paginated documents; 0 when not applicable.
	Pages int `json:"pages,omitempty"`
	// Unsupported marks a permanently unreadable type, so retrying cannot help.
	Unsupported bool `json:"unsupported,omitempty"`
}

// UploadInboundAsset streams body to the operator's
// POST /inbound-asset/{ns}/{sess} route. body is passed straight into
// http.NewRequestWithContext as the request body — never read into memory
// here — so the whole fetch→upload leg stays a single streamed copy.
func (m *memoryClient) UploadInboundAsset(ctx context.Context, ns, sess, mime, filename string, body io.Reader) (pipeline.InboundAssetResult, error) {
	url := m.baseURL + "/inbound-asset/" + ns + "/" + sess
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return pipeline.InboundAssetResult{}, fmt.Errorf("UploadInboundAsset: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	if mime != "" {
		req.Header.Set("Content-Type", mime)
	}
	if filename != "" {
		req.Header.Set("X-Attachment-Filename", filename)
	}
	resp, err := m.uploadHTTP.Do(req)
	if err != nil {
		return pipeline.InboundAssetResult{}, fmt.Errorf("UploadInboundAsset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		// A typed sentinel, not a generic error string: attachments.go maps
		// this to outcomeOversize (permanent — the file IS too large) rather
		// than the generic outcomeFailed (transient) every other upload
		// failure gets. This is the backstop for when the pre-fetch clamp
		// (effectiveLimit, clamped to httpsrv.MaxInboundAssetBytes) is
		// bypassed — e.g. a channel kind that reports SizeBytes=0.
		return pipeline.InboundAssetResult{}, pipeline.ErrInboundAssetTooLarge
	}
	if resp.StatusCode != http.StatusOK {
		return pipeline.InboundAssetResult{}, fmt.Errorf("UploadInboundAsset: HTTP %d", resp.StatusCode)
	}
	var out inboundAssetResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return pipeline.InboundAssetResult{}, fmt.Errorf("UploadInboundAsset: decode response: %w", err)
	}
	return pipeline.InboundAssetResult{
		Ref: out.Ref, Extracted: out.Extracted, TextRef: out.TextRef,
		Pages: out.Pages, Unsupported: out.Unsupported,
	}, nil
}

// RecordChannelMsgRef writes a channel_msg_ref memory entry mapping
// (kind, ref) → turnIndex at the (ns, name) session scope. Used by
// the inbound pipeline to index inbox turns by their channel message ref
// so restart UIs can resolve a clicked message to a turn.
func (m *memoryClient) RecordChannelMsgRef(ctx context.Context, ns, name, kind, ref string, turnIndex int) error {
	scope := memory.Scope{Kind: "session", ID: ns + "/" + name}
	// channel_msg_ref is a mutable Kind, so signing is a pass-through, but
	// routing every write through m.signed keeps the write path uniform.
	if err := channel_msg_ref.Record(ctx, m.signed, scope, kind, ref, turnIndex); err != nil {
		return fmt.Errorf("RecordChannelMsgRef: %w", err)
	}
	return nil
}

// RecordTriggerDelivery writes the trigger_delivery memory entry for the
// (ns, name) session — the signed webhook delivery that opened it. Routed
// through m.signed (not client directly): trigger_delivery is append-only,
// and the operator's facade rejects an unsigned write.
func (m *memoryClient) RecordTriggerDelivery(ctx context.Context, ns, name, kind, event, channelKey string, body []byte) error {
	scope := memory.Scope{Kind: "session", ID: ns + "/" + name}
	if err := triggerdelivery.Record(ctx, m.signed, scope, triggerdelivery.Content{
		Kind: kind, Event: event, ChannelKey: channelKey, Body: body,
	}); err != nil {
		return fmt.Errorf("RecordTriggerDelivery: %w", err)
	}
	return nil
}

// RecordEnvelopeFacts writes one envelope_fact entry per (subject, fact) pair
// for every observation the delivery produced — see
// pkg/memory/kinds/envelopefact. Routed through m.signed for the same reason
// as RecordTriggerDelivery: envelope_fact is append-only, and the operator's
// facade rejects an unsigned write.
//
// Uses envelopefact.Record (RULING T1-C), never factcontent.Record with a
// hand-passed (kindName, idPrefix) pair — that wrapper is the only place
// outside the envelopefact package allowed to know that pair, so a caller can
// never accidentally file a fact into the wrong Kind's namespace.
func (m *memoryClient) RecordEnvelopeFacts(ctx context.Context, ns, name, kind, event string, facts []channelkinds.TriggerFact) error {
	scope := memory.Scope{Kind: "session", ID: ns + "/" + name}
	for _, f := range facts {
		if err := envelopefact.Record(ctx, m.signed, scope, factcontent.Observation{
			Subjects: f.Subjects,
			Facts:    f.Facts,
			Source:   factcontent.Source{ChannelKind: kind, Event: event},
		}); err != nil {
			return fmt.Errorf("RecordEnvelopeFacts %s/%s: %w", ns, name, err)
		}
	}
	return nil
}

// MemoryV2 returns the signing memory facade. Used by channelManager to
// wire Memory into Deps so the Slack listener's "Restart from here"
// shortcut can query channel_msg_ref and turn entries directly. The Slack
// restart path is read-only today; returning the signing facade means any
// future append-only write through it is attested automatically.
func (m *memoryClient) MemoryV2() memory.Memory { return m.signed }

// Preferences returns the first-party preferences client. Used by
// channelManager to wire Deps.Preferences so a channel kind's own UI
// surface (the Slack App Home preferences pane) can read/edit a verified
// user's own preferences directly against the operator's first-party
// preferences endpoints. m.client is constructed unconditionally in
// newMemoryClient, so this never wraps a typed-nil pointer into the
// interface return.
func (m *memoryClient) Preferences() channelkinds.PreferencesClient { return m.client }

// Compile-time check: *memoryClient implements pipeline.Memory.
var _ pipeline.Memory = (*memoryClient)(nil)
