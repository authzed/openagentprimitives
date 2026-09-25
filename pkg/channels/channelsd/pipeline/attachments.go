package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/dustin/go-humanize"
	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/agent/agentcaps"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/resolve"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/memory/assetlimits"
	"github.com/authzed/openagentprimitives/pkg/platform/extract"
)

// attachmentsCapabilityName is the AgentClass capability key
// (attachmentsCapability.Name() in pkg/agent/tool/meta/capability). Kept as a
// local constant, not an import of that package, because that package pulls
// in the runner's tool/modality graph — pkg/agent/agentcaps is deliberately the
// only capability-grant dependency this process-boundary code takes on.
const attachmentsCapabilityName = "attachments"

// attachmentBatchTimeout and attachmentFetchTimeout bound the whole
// attachment-processing chain: files.info, the CDN download, the operator
// upload, and the operator's extractord round trip. This loop runs serially and
// inline on the ONE goroutine per Channel that dispatches every inbound event,
// so without them a hang anywhere in the chain blocks that whole Channel
// forever, with no watchdog on listener liveness. The realistic case is a CDN
// accepting the connection then blackholing the body — Go's default transport
// bounds dial and TLS handshake, not body reads.
//
// Vars, not consts, so a test can shrink them and prove the mechanism without
// waiting out the production value.
var (
	// attachmentBatchTimeout bounds the ENTIRE serial loop for one inbound
	// message. Generous on purpose: a message can carry up to the kind's
	// AttachmentBounds.MaxPerMessage files, each separately bounded by
	// attachmentFetchTimeout, so this is the outer net freeing the Channel's
	// dispatch goroutine even when several attachments are independently
	// slow-but-legitimate. Running past it aborts the REMAINING attachments as
	// outcomeFailed (transient — the user resends), never the whole session.
	//
	// It is the SHARED constant, not a local literal, because the runner's
	// first-turn fence waits exactly this long for the turn this batch writes
	// (AnnotationInboundAttachmentCount). Two independently chosen numbers
	// would drift into a fence expiring before the work it fences finishes.
	attachmentBatchTimeout = spiceboxv1alpha1.InboundAttachmentWriteBudget

	// attachmentFetchTimeout bounds ONE attachment's fetch+upload+extract round
	// trip, including the operator's synchronous call to extractord. That
	// extraction step is separately capped at 30s, so this leaves headroom to
	// transfer a file up to assetlimits.MaxInboundAssetBytes over a slow link
	// without letting one bad attachment eat the whole batch budget above.
	attachmentFetchTimeout = 60 * time.Second

	// archiveFetchTimeout replaces attachmentFetchTimeout for an upload the
	// operator will EXPLODE. One such upload costs explode + N stores + N
	// extracts inside a single call, so it cannot share the single-file
	// budget.
	//
	// It is clamped to what remains of attachmentBatchTimeout, never added to
	// it: that batch bound IS InboundAttachmentWriteBudget, the constant the
	// runner's first-turn fence waits on. Stretching it makes the agent answer
	// before the files land — the exact race the fence exists to close.
	archiveFetchTimeout = 4 * time.Minute

	// archiveFloor is the least time worth STARTING an explode with. Below it
	// the archive is stored whole rather than half-exploded: a partial bundle
	// the agent believes is complete is worse than one it was told to resend.
	archiveFloor = 30 * time.Second
)

// attachmentOutcome is what happened to one inbound attachment.
type attachmentOutcome int

const (
	outcomeRead        attachmentOutcome = iota // bytes stored and text extracted
	outcomeDisabled                             // gate closed: capability or channel opt-in off
	outcomeUnfetchable                          // kind cannot fetch attachments at all
	// outcomeUnsupported: reserved for a permanent path storing NO bytes at
	// all. No current caller produces it — a no-extractor MIME still stores its
	// bytes and takes outcomeStored below.
	outcomeUnsupported
	outcomeOversize // exceeds the channel's byte-size limit
	outcomeTooMany  // exceeds the channel's per-message attachment count limit
	// outcomeFailed: no bytes reached the store (transient). The download
	// failed, the upload failed, or the channel kind could not be resolved —
	// in every case a resend is the user's recourse, which is what its
	// "temporary failure" wording promises.
	outcomeFailed
	// outcomeStored: bytes were fetched and stored, but no text came back —
	// either no extractor claims the type, or extraction failed/was never
	// configured. NOT a failure, and the cause does not change the state: a
	// stored file with no text handle. Whether it is readable depends on the
	// model resolved at send time, which is not known here. The runner's
	// hydration pass decides, and is the only place that can.
	outcomeStored
	// outcomeMember: one member of an exploded archive. Its bytes are stored
	// and it is reachable through the archive's INDEX rather than a manifest
	// line of its own — a support bundle of 187 files must not become 187
	// lines in the turn.
	//
	// NOT a failure, so the notice classes exclude it. It still gets a
	// content block (composeAttachmentBlocks keys on Ref, not Outcome), which
	// is what makes it pinnable by show_attachment.
	outcomeMember
)

func (o attachmentOutcome) String() string {
	switch o {
	case outcomeRead:
		return "read"
	case outcomeDisabled:
		return "disabled"
	case outcomeUnfetchable:
		return "unfetchable"
	case outcomeUnsupported:
		return "unsupported"
	case outcomeOversize:
		return "oversize"
	case outcomeTooMany:
		return "too_many"
	case outcomeFailed:
		return "failed"
	case outcomeMember:
		return "member"
	case outcomeStored:
		return "stored"
	default:
		return "unknown"
	}
}

// attachmentResult is one attachment's disposition after processing. It feeds
// composeAttachmentNote (every result) and composeAttachmentBlocks (every
// result whose Ref is set — see that function's doc for which outcomes
// that includes).
type attachmentResult struct {
	Filename  string
	MIME      string
	SizeBytes int64
	Outcome   attachmentOutcome

	// Limit is set for outcomeOversize (the effective byte-size ceiling —
	// channel config clamped to the kind's hard max and the operator's own
	// absolute cap — the file exceeded) and for outcomeTooMany (the
	// effective per-message attachment count, reused as int64 rather than
	// adding a second count-typed field for one outcome).
	Limit int64

	// Ref is set whenever bytes reached the store, which is exactly
	// outcomeRead (text extracted too) and outcomeStored (no text, whatever
	// the cause — see the outcome constants). Every other outcome stored
	// nothing and leaves it empty. TextRef and Pages are set only for
	// outcomeRead.
	Ref string
	// ArchiveRef is the archive this member came out of; empty for a
	// directly-attached file. Carried into the content block so the runner
	// can keep members out of the native-block window unless pinned.
	ArchiveRef string
	TextRef    string
	Pages      int
}

// noteFilenameMaxRunes caps how much of a filename composeAttachmentNote
// renders. Filenames are attacker-controlled text arriving in the agent's
// context (see the design's "filenames are untrusted input" note), so the
// cap and the control-character strip below apply independently of whatever
// sanitization already ran upstream (the Slack listener's
// sanitizeAttachmentFilename) — this function must be safe even fed a raw,
// unsanitized name directly.
const noteFilenameMaxRunes = 80

// sanitizeForNote strips control characters (runes IsControl reports true
// for — this removes newlines/CR/tabs, keeping the composed note a single
// line) and truncates to noteFilenameMaxRunes surviving runes, appending an
// ellipsis when truncated. Rune-based so a multi-byte UTF-8 name is never
// split mid-codepoint, mirroring pkg/channels/channelkinds/slack's
// sanitizeAttachmentFilename.
func sanitizeForNote(name string) string {
	var b strings.Builder
	count := 0
	truncated := false
	for _, r := range name {
		if count >= noteFilenameMaxRunes {
			truncated = true
			break
		}
		if unicode.IsControl(r) {
			continue // dropped; must not consume the truncation budget
		}
		b.WriteRune(r)
		count++
	}
	out := b.String()
	if truncated {
		out += "…"
	}
	return out
}

func mimeOrUnknown(m string) string {
	if m == "" {
		return "unknown type"
	}
	return m
}

// composeAttachmentNote is the agent-visible contract for what happened to a
// message's attachments. Pure, so it is tested exhaustively with no fakes.
// Returns "" when every attachment was read; otherwise one bracketed,
// pipeline-authored line per non-read attachment, the same idiom
// formatTranscript uses for truncated history.
//
// Every line names the remedy at an administrative altitude — never a CRD
// field, capability key or command, since the agent paraphrases this straight
// to a user — and keeps "cannot be read" (permanent) and "temporary failure"
// (transient) mutually exclusive, so the agent can never report an unsupported
// format for a transient hiccup, or vice versa.
func composeAttachmentNote(items []attachmentResult) string {
	readCount := 0
	nonRead := make([]attachmentResult, 0, len(items))
	for _, it := range items {
		if it.Outcome == outcomeRead {
			readCount++
			continue
		}
		if it.Outcome == outcomeMember {
			// A member is neither read-in-its-own-right nor a failure. It is
			// reachable through the archive's index, and the archive's own
			// result already spoke for the whole upload.
			continue
		}
		nonRead = append(nonRead, it)
	}
	if len(nonRead) == 0 {
		return "" // nothing went wrong — no note needed
	}
	lines := make([]string, 0, len(nonRead))
	for _, it := range nonRead {
		lines = append(lines, attachmentNoteLine(it, readCount))
	}
	return strings.Join(lines, "\n")
}

// attachmentNoteLine renders one bracketed line. The partial-outcome clause
// (readCount > 0) is appended uniformly across every outcome that can co-occur
// with a read in the same batch, so the agent never implies it saw everything
// just because the FIRST non-read file happened to be oversize rather than
// unsupported. outcomeDisabled and outcomeUnfetchable are excluded: the gate is
// evaluated once per message, so a gate-closed batch shares one outcome with
// readCount always 0 — there is nothing partial to state.
func attachmentNoteLine(r attachmentResult, readCount int) string {
	// Quoted for the same reason as attachmentManifestLine: an attacker-chosen
	// filename must not read as this pipeline note's own narration.
	name := strconv.Quote(sanitizeForNote(r.Filename))
	if r.Outcome == outcomeDisabled || r.Outcome == outcomeUnfetchable {
		// Same shape for both: the agent only needs to know it cannot read
		// attachments here, not which of the three gate legs failed.
		return fmt.Sprintf(
			"[%s (%s) was attached, but this agent cannot read attachments. Attachment support is not enabled here; an administrator can turn it on.]",
			name, mimeOrUnknown(r.MIME))
	}

	var base string
	switch r.Outcome {
	case outcomeStored:
		// Deliberately does NOT say "cannot be read": that is a send-time
		// question and this line is composed far too early to answer it. The
		// runner's hydration pass carries the other half — when the resolved
		// model does not take the type natively, appendUnreadableNote appends
		// its own "attached but not readable" line naming the file. So the
		// agent is never left with only this neutral line; do not "complete"
		// the sentence here, where the answer is genuinely unknown.
		base = fmt.Sprintf("%s (%s) was attached.", name, mimeOrUnknown(r.MIME))
	case outcomeUnsupported:
		base = fmt.Sprintf("%s (%s) was attached, but this file type cannot be read.", name, mimeOrUnknown(r.MIME))
	case outcomeOversize:
		if r.SizeBytes > 0 {
			base = fmt.Sprintf("%s (%s) was attached, but exceeds this channel's %s attachment limit and was not read.",
				name, humanize.Bytes(uint64(r.SizeBytes)), humanize.Bytes(uint64(max64(r.Limit, 0))))
		} else {
			// SizeBytes is unknown, not zero: a real 0-byte upload cannot happen.
			// This is the operator's upload-time check catching a file the kind
			// under-reported, so the pre-fetch check never saw its size —
			// rendering "(0 B)" would assert a size we do not have. Name only
			// the ceiling.
			base = fmt.Sprintf("%s was attached, but exceeds this channel's %s attachment limit and was not read.",
				name, humanize.Bytes(uint64(max64(r.Limit, 0))))
		}
	case outcomeTooMany:
		base = fmt.Sprintf("%s was attached, but only the first %s on a message can be read.", name, pluralAttachmentCount(r.Limit))
	case outcomeFailed:
		base = fmt.Sprintf("%s was attached, but could not be retrieved/read. This is a temporary failure, not an unsupported type.", name)
	default:
		// Unreachable: every non-outcomeRead value is handled above. Kept as
		// an honest fallback rather than a panic — a note that says too
		// little is still better than one silently dropped.
		base = fmt.Sprintf("%s was attached, but its status could not be determined.", name)
	}
	if readCount > 0 {
		base += " " + readCountClause(readCount)
	}
	return "[" + base + "]"
}

func readCountClause(n int) string {
	if n == 1 {
		return "1 other attachment was read."
	}
	return fmt.Sprintf("%d other attachments were read.", n)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// pluralAttachedFiles renders "1 attached file" vs "N attached files" — the
// singular/plural split BOTH the ephemeral reading-status text and the
// failure notice's lead need, factored once so the two user-facing surfaces
// can never drift on how they count the same batch.
func pluralAttachedFiles(n int) string {
	if n == 1 {
		return "1 attached file"
	}
	return fmt.Sprintf("%d attached files", n)
}

// pluralAttachmentCount renders "1 attachment" vs "N attachments" for the
// outcomeTooMany wording (the per-message limit, r.Limit), so a limit of
// exactly 1 does not read as "only the first 1 attachments".
func pluralAttachmentCount(n int64) string {
	if n == 1 {
		return "1 attachment"
	}
	return fmt.Sprintf("%d attachments", n)
}

// attachmentFailureReason renders one file's outcome for the human-facing
// notice's Fields: the same permanent-vs-transient distinction and
// administrative altitude as attachmentNoteLine's agent-facing sibling
// (never a CRD field, capability key, or command), but without the filename
// (the Field's Label already carries it) or the partial-outcome clause (the
// notice's Lead already states how many files failed, across the whole
// batch, in one place).
func attachmentFailureReason(r attachmentResult) string {
	switch r.Outcome {
	case outcomeDisabled, outcomeUnfetchable:
		// Same shape for both: the user only needs to know attachments cannot
		// be read here, not which of the three gate legs failed.
		return "attachment reading is not turned on here; an administrator can enable it."
	case outcomeUnsupported:
		return fmt.Sprintf("this file type (%s) cannot be read.", mimeOrUnknown(r.MIME))
	case outcomeOversize:
		if r.SizeBytes > 0 {
			return fmt.Sprintf("too large (%s over this channel's %s limit) to read.",
				humanize.Bytes(uint64(r.SizeBytes)), humanize.Bytes(uint64(max64(r.Limit, 0))))
		}
		// SizeBytes unknown, not zero — see attachmentNoteLine's identical
		// carve-out. Name only the ceiling, never a false "(0 B)" claim.
		return fmt.Sprintf("too large to read (over this channel's %s limit).", humanize.Bytes(uint64(max64(r.Limit, 0))))
	case outcomeTooMany:
		return fmt.Sprintf("only the first %s on a message can be read.", pluralAttachmentCount(r.Limit))
	case outcomeFailed:
		return "couldn't be retrieved right now."
	default:
		// Unreachable: every non-outcomeRead value is handled above. Kept as an
		// honest fallback, same reasoning as attachmentNoteLine's default case.
		return "its status could not be determined."
	}
}

// noticeClass is the cause class an outcome belongs to for USER-FACING
// reporting, and the category that class publishes under.
//
// A function of the outcome ALONE: the class must not depend on what else was
// in the batch, or one file's explanation would change depending on its
// neighbours.
func noticeClass(o attachmentOutcome) (category string, ok bool) {
	switch o {
	case outcomeDisabled, outcomeUnfetchable:
		// Nothing was attempted. Not a read failure, and no remedy exists for
		// anyone in the conversation.
		return categories.AttachmentNotEnabled, true
	case outcomeUnsupported, outcomeOversize, outcomeTooMany:
		// The gate was open and this file was refused on its own merits. The
		// remedy is the user's: another format, a smaller file, fewer files.
		return categories.AttachmentUnreadable, true
	case outcomeFailed:
		return categories.AttachmentReadFailed, true
	default:
		// outcomeRead and outcomeStored are not failures. outcomeStored's
		// readability is a send-time question this component cannot answer,
		// and folding it in here would be the premature verdict
		// attachmentNoteLine deliberately avoids.
		return "", false
	}
}

// noticeCopy is the per-class lead and next step, in ONE place so the three
// cannot drift back into hedging across each other.
//
// Each next step names only its OWN class's remedy. A string that named every
// remedy at once would give one of them top billing on a batch it cannot help,
// which is why the copy is keyed on the class rather than shared.
func noticeCopy(category string, n int) (lead, nextStep string) {
	switch category {
	case categories.AttachmentNotEnabled:
		return "Attachment reading isn't enabled here",
			"An administrator can turn on attachment support for this agent."
	case categories.AttachmentUnreadable:
		return "Couldn't read " + pluralAttachedFiles(n),
			"Try sending it in another format, or paste the relevant part as text."
	default:
		return "Couldn't read " + pluralAttachedFiles(n),
			"This looks temporary. Try sending the file again."
	}
}

// attachmentFailureNotices builds ONE notice per cause class present in
// results — never one per file, and never one hedged across classes.
//
// Grouping by class is what lets each notice state only its own remedy.
// Almost every message has exactly one class, so the common case is a single
// card; a mixed batch gets one card per cause rather than one card whose
// advice is wrong for two thirds of it.
//
// Returns nil when nothing failed; callers must check before publishing
// (notice.Notice's nil-is-inert contract makes that safe even if they don't).
//
// Order is fixed rather than map-iteration order, so a mixed batch reads the
// same way every time.
//
// The user-facing sibling of composeAttachmentNote: same wording rules and
// source data. The two must never disagree about WHICH files failed —
// attachments_status_test.go's cross-audience assertions guard that.
func attachmentFailureNotices(results []attachmentResult) []*notice.Notice {
	byClass := map[string][]channelevents.InteractionField{}
	for _, r := range results {
		cat, ok := noticeClass(r.Outcome)
		if !ok {
			continue
		}
		byClass[cat] = append(byClass[cat], channelevents.InteractionField{
			Label: sanitizeForNote(r.Filename),
			Value: attachmentFailureReason(r),
		})
	}
	if len(byClass) == 0 {
		return nil
	}
	order := []string{
		categories.AttachmentNotEnabled,
		categories.AttachmentUnreadable,
		categories.AttachmentReadFailed,
	}
	out := make([]*notice.Notice, 0, len(byClass))
	for _, cat := range order {
		fields, present := byClass[cat]
		if !present {
			continue
		}
		lead, nextStep := noticeCopy(cat, len(fields))
		out = append(out, notice.New(cat, notice.Args{
			Lead:     lead,
			Fields:   fields,
			NextStep: nextStep,
			Audience: participantsAudience(),
		}))
	}
	return out
}

// attachmentsNotCarriedNotice covers the Deliver paths that route a message
// onward WITHOUT reaching processAttachments: the pending-join stash, the
// inherit- and takeover-fork triggers, the portal-access trigger, and live
// interactive-tool routing. Each keeps ev.MessageText — stashed into
// PendingRequesters or PendingRestart, or forwarded to a tool's stdin — but has
// nowhere to put ev.Attachments, so without this a file would vanish with no
// trace anywhere. Full carry-through remains unimplemented; this is the floor
// meanwhile: tell the user once, plainly.
func attachmentsNotCarriedNotice(n int) *notice.Notice {
	return notice.New(categories.AttachmentReadFailed, notice.Args{
		Lead:     "Couldn't carry " + pluralAttachedFiles(n) + " forward",
		Body:     "This message is being handled in a way that doesn't carry file attachments along with it.",
		NextStep: "Please re-send the file(s) once this is resolved.",
		Audience: participantsAudience(),
	})
}

// logAttachmentsDropped is the logging half of attachmentsNotCarriedNotice,
// shared by every Deliver site named on its doc. Each dropped attachment is
// logged individually with session, channel and filename REGARDLESS of whether
// a user-facing notice is raised there — some sites deliberately stay silent
// toward the user for unrelated reasons (the blocklist branch must not confirm
// the session exists), and the log must never depend on that. No-op when there
// is nothing to report.
func logAttachmentsDropped(ctx context.Context, ns, name, channelRef, transition string, atts []channelkinds.InboundAttachment) {
	if len(atts) == 0 {
		return
	}
	logger := log.FromContext(ctx)
	for _, a := range atts {
		logger.Info("attachment dropped: not carried across "+transition,
			"session", ns+"/"+name, "channel", channelRef, "filename", sanitizeForNote(a.Filename))
	}
}

// publishAttachmentsNotCarriedNotice best-effort publishes an
// attachmentsNotCarriedNotice straight onto the session's OUT subject rather
// than attaching it to the caller's InboundDecision. Both call sites report
// OutcomeRouted, and no channel kind's Routed branch reads
// InboundDecision.Notice — only the terminal-continuation outcomes do — so a
// Notice on a Routed decision is dead weight, never delivered. Deliver's mid-turn
// enqueue-ack publishes directly for the identical reason.
func (p *Pipeline) publishAttachmentsNotCarriedNotice(ctx context.Context, ns, sessName, channelRef string, n int) {
	if n == 0 {
		return
	}
	sess := channelevents.SessionRef{Namespace: ns, Name: sessName}
	if err := attachmentsNotCarriedNotice(n).Publish(p.NATS.Publish, sess, mintRequestID()); err != nil {
		log.FromContext(ctx).Info("attachments: publish not-carried notice failed",
			"session", ns+"/"+sessName, "channel", channelRef, "err", err.Error())
	}
}

// publishAttachmentReadingStatus emits the ephemeral "Reading N attached files…"
// status BEFORE the fetch/upload/extract loop starts, since those are network
// round trips against files that may be tens of megabytes. It clears naturally
// when the agent's reply lands, per NotificationPayload's ephemeral-status
// contract, so there is nothing to retract on the way out.
//
// Gate-open path only: with the gate closed nothing is being read, so claiming
// otherwise would be a lie and publishAttachmentFailureNotice covers it instead.
//
// Best-effort: a publish failure is logged with session+channel context but must
// not fail the turn — the attachments are processed either way.
func (p *Pipeline) publishAttachmentReadingStatus(ns, sessName, sessRef, channelRef string, n int, logger logr.Logger) {
	text := "Reading " + pluralAttachedFiles(n) + "…"
	if err := channelevents.PublishOut(p.NATS.Publish, ns, sessName,
		channelevents.KindNotification,
		channelevents.NotificationPayload{Text: text, Short: text},
	); err != nil {
		logger.Info("attachments: publish reading-status notification failed",
			"session", sessRef, "channel", channelRef, "err", err.Error())
	}
}

// publishAttachmentFailureNotice best-effort publishes one user-visible notice
// per cause class present (see attachmentFailureNotices) for this message's
// non-read attachments, independent of whatever the agent chooses to say in
// its reply: composeAttachmentNote's bracketed line only reaches the model, and
// an agent that judges the failure unimportant would otherwise leave the
// human staring at a reply that silently ignored their upload. A publish
// failure is logged with session+channel context (never silently dropped)
// but does not fail the turn — the agent-facing note in text is still
// written regardless.
// written regardless.
func (p *Pipeline) publishAttachmentFailureNotice(ns, sessName, sessRef, channelRef string, results []attachmentResult, logger logr.Logger) {
	sess := channelevents.SessionRef{Namespace: ns, Name: sessName}
	for _, n := range attachmentFailureNotices(results) {
		if err := n.Publish(p.NATS.Publish, sess, mintRequestID()); err != nil {
			logger.Info("attachments: publish failure notice failed",
				"session", sessRef, "channel", channelRef,
				"category", n.Category(), "err", err.Error())
		}
	}
}

// composeAttachmentManifest renders one bracketed line per successfully-read
// attachment, naming the text handle fetch_artifact needs to read it. Necessary
// because the durable turn's structured MemContent{Type:"attachment"} block is
// NOT rendered to the model — contentBlocksFromMemory drops any block type no
// provider adapter understands, by design. Without this line in the SAME turn's
// text, a fully-successful read leaves the agent unable to learn the extracted
// text exists: it sees nothing wrong, since composeAttachmentNote returns "",
// and never calls fetch_artifact.
//
// So unlike composeAttachmentNote — the FAILURE note, correctly "" when nothing
// went wrong — this composer's "all read" case is when it has the most to say.
func composeAttachmentManifest(items []attachmentResult) string {
	lines := make([]string, 0, len(items))
	for _, it := range items {
		if it.Outcome != outcomeRead {
			continue
		}
		lines = append(lines, attachmentManifestLine(it))
	}
	return strings.Join(lines, "\n")
}

// composeAttachmentBlocks builds one MemContent{Type:"attachment"} block per
// attachment whose bytes reached the store — outcomeRead (text extracted too)
// and outcomeStored (no text, whatever the cause) both qualify. Every other
// outcome stores nothing, so Ref is the one signal this checks — not Outcome —
// matching attachmentResult's own doc on when Ref is set, and keeping a future
// outcome that also stores bytes from being silently left out of the record.
//
// A block is written whenever bytes reached the store, with or without
// extracted text. TextRef distinguishes the two, and it is what the
// runner's hydration pass keys native-block lifetime on: empty means
// nothing else represents the file, so the block persists; set means the
// extracted text carries the file after the arrival turn.
func composeAttachmentBlocks(items []attachmentResult) []MemContent {
	var blocks []MemContent
	for _, it := range items {
		if it.Ref == "" {
			continue
		}
		blocks = append(blocks, MemContent{
			Type: "attachment", Filename: sanitizeForNote(it.Filename), MIME: it.MIME,
			SizeBytes: it.SizeBytes, Ref: it.Ref, TextRef: it.TextRef, Pages: it.Pages,
			ArchiveRef: it.ArchiveRef,
		})
	}
	return blocks
}

func attachmentManifestLine(r attachmentResult) string {
	// QUOTED: this name is attacker-controlled and lands in a bracketed,
	// pipeline-authored note that becomes trusted user-turn text OUTSIDE the
	// untrusted-attachment envelope (which wraps only the bytes). Quoting keeps
	// a name like `x] ... [y` from reading as the note's own words — the same
	// reason the runner's out-of-view note and the archive index quote.
	name := strconv.Quote(sanitizeForNote(r.Filename))
	pages := ""
	if r.Pages > 0 {
		pages = fmt.Sprintf(", %d pages", r.Pages)
	}
	return fmt.Sprintf(
		"[%s (%s%s) was attached and read. Its text is available at %s — use fetch_artifact to read it.]",
		name, mimeOrUnknown(r.MIME), pages, r.TextRef)
}

// attachmentContent builds the full content slice for one inbound turn: the
// text block (the user's raw message with the composed attachment text
// appended, if any — a manifest line per successfully-read file, a failure
// note per one that wasn't, or both) followed by one
// MemContent{Type:"attachment"} block per attachment whose bytes reached the
// store (see composeAttachmentBlocks). Called from both Deliver's
// active-session append and ResubmitAuthorized so a queued message's
// attachments get identical treatment to a live one.
func (p *Pipeline) attachmentContent(ctx context.Context, ev channelkinds.InboundEvent, className, ns, sessName string) []MemContent {
	attachmentText, blocks := p.processAttachments(ctx, ev, className, ns, sessName)
	text := ev.MessageText
	switch {
	case attachmentText == "":
		// nothing to append
	case text == "":
		text = attachmentText
	default:
		text = text + "\n\n" + attachmentText
	}
	content := make([]MemContent, 0, 1+len(blocks))
	content = append(content, MemContent{Type: "text", Text: text})
	content = append(content, blocks...)
	return content
}

// processAttachments is the gate: Channel.spec.attachments.enabled AND the
// bound AgentClass granting the "attachments" capability AND the bound kind
// implementing channelkinds.AttachmentFetcher. All three must hold before a
// single fetch call is made — this is the load-bearing property the whole
// feature exists to prove: gating controls bytes, never awareness. Every
// attachment gets an attachmentResult (and therefore a chance to appear in
// text) whether or not the gate is open.
//
// text combines composeAttachmentManifest (one line per read file, naming the
// fetch_artifact handle — the ONLY way the agent learns that handle exists,
// since the structured "attachment" block is stripped before any LLM request)
// and composeAttachmentNote (one line per file not read). Either half may be
// empty, but text is never "" when ev carries attachments, since a read always
// produces a manifest line.
//
// It also speaks to the human directly on the channel, independent of the
// agent's reply: an ephemeral "Reading N attached files…" status during the
// gate-open loop's network I/O, and one notice per message summarizing every
// FAILED outcome (outcomeStored is not a failure and is excluded). Both are
// best-effort, so a publish failure never blocks the turn.
func (p *Pipeline) processAttachments(ctx context.Context, ev channelkinds.InboundEvent, className, ns, sessName string) (text string, blocks []MemContent) {
	if len(ev.Attachments) == 0 {
		return "", nil
	}
	// Bound the whole batch. This function runs inline on the Slack
	// listener's single per-Channel dispatch goroutine (see
	// attachmentBatchTimeout's doc) — nothing below this point may run
	// unbounded. logger below deliberately reads from the now-bounded ctx (it
	// only carries values, not the deadline itself, but keeping it downstream
	// of the wrap avoids a second FromContext call sharing a stale parent).
	ctx, cancel := context.WithTimeout(ctx, attachmentBatchTimeout)
	defer cancel()
	logger := log.FromContext(ctx)
	channelRef := channelRefForLog(ev.Channel)
	sessRef := ns + "/" + sessName

	var spec *spiceboxv1alpha1.ChannelAttachmentsSpec
	if ev.Channel != nil {
		spec = ev.Channel.Spec.Attachments
	}
	channelEnabled := spec != nil && spec.Enabled

	capabilityGranted := false
	if channelEnabled && ev.Channel != nil {
		capabilityGranted = p.attachmentsCapabilityGranted(ctx, ev.Channel.Namespace, className)
	}

	var fetcher channelkinds.AttachmentFetcher
	var deps channelkinds.Deps
	kindImplementsFetcher := false
	// resolveErrored marks a resolve.ForChannel failure — a Secret Get error,
	// apiserver blip or transient RBAC hiccup — the textbook TRANSIENT case.
	// Folding it into outcomeUnfetchable would render the permanent,
	// admin-blaming "attachment support is not enabled here" note for a passing
	// fault. Its own bool rather than part of kindImplementsFetcher so the two
	// "the kind can't fetch" reasons — resolve failed vs. the kind structurally
	// not implementing AttachmentFetcher — reach differently-worded outcomes.
	resolveErrored := false
	if channelEnabled && capabilityGranted {
		sec, k, rerr := resolve.ForChannel(ctx, p.K8s, ev.Channel)
		if rerr != nil {
			resolveErrored = true
			logger.Info("attachments: resolve channel kind/secret failed; treating as a transient failure",
				"session", sessRef, "channel", channelRef, "err", rerr.Error())
		} else if af, ok := k.(channelkinds.AttachmentFetcher); ok {
			fetcher = af
			deps = channelkinds.Deps{Channel: ev.Channel, Secret: sec}
			kindImplementsFetcher = true
		}
	}

	gateOpen := channelEnabled && capabilityGranted && kindImplementsFetcher
	if !gateOpen {
		// NO fetch call is made past this point for any attachment on this
		// event — the load-bearing property attachments_test.go's gate-closed
		// tests exist to prove (a fake kind whose FetchAttachment fails the
		// test if invoked).
		reason := outcomeDisabled
		switch {
		case resolveErrored:
			// Transient (see resolveErrored's doc above) — never the permanent,
			// admin-blaming outcomeDisabled/outcomeUnfetchable shape.
			reason = outcomeFailed
		case channelEnabled && capabilityGranted && !kindImplementsFetcher:
			reason = outcomeUnfetchable
		}
		results := make([]attachmentResult, 0, len(ev.Attachments))
		for _, a := range ev.Attachments {
			results = append(results, attachmentResult{Filename: a.Filename, MIME: a.MIME, SizeBytes: a.SizeBytes, Outcome: reason})
			logger.Info("attachment not fetched: gate closed",
				"session", sessRef, "channel", channelRef,
				"filename", sanitizeForNote(a.Filename), "reason", reason.String())
		}
		// No ephemeral "Reading…" status here — nothing is being read, so one
		// would be a lie. The failure notice below is what tells the human,
		// independent of whatever the agent goes on to say.
		p.publishAttachmentFailureNotice(ns, sessName, sessRef, channelRef, results, logger)
		return composeAttachmentNote(results), nil
	}

	bounds := fetcher.AttachmentBounds()
	limit := effectiveLimit(spec, bounds)
	maxPerMessage := effectiveMaxPerMessage(spec, bounds)

	// Published before the fetch/upload/extract loop below — those are
	// network round trips against files that may be tens of megabytes, and
	// the user would otherwise wait on silence.
	p.publishAttachmentReadingStatus(ns, sessName, sessRef, channelRef, len(ev.Attachments), logger)

	results := make([]attachmentResult, 0, len(ev.Attachments))
	for i, a := range ev.Attachments {
		if maxPerMessage > 0 && i >= maxPerMessage {
			results = append(results, attachmentResult{Filename: a.Filename, MIME: a.MIME, SizeBytes: a.SizeBytes, Outcome: outcomeTooMany, Limit: int64(maxPerMessage)})
			logger.Info("attachment not fetched: exceeds this message's attachment count limit",
				"session", sessRef, "channel", channelRef,
				"filename", sanitizeForNote(a.Filename), "maxPerMessage", maxPerMessage)
			continue
		}
		if limit > 0 && a.SizeBytes > limit {
			results = append(results, attachmentResult{Filename: a.Filename, MIME: a.MIME, SizeBytes: a.SizeBytes, Outcome: outcomeOversize, Limit: limit})
			logger.Info("attachment not fetched: exceeds size limit",
				"session", sessRef, "channel", channelRef,
				"filename", sanitizeForNote(a.Filename), "size", a.SizeBytes, "limit", limit)
			continue
		}

		// A per-attachment sub-deadline nested inside the batch-level ctx above.
		// It covers BOTH calls below — FetchAttachment (the kind's own metadata
		// lookup plus CDN download, on a client with no timeout of its own) and
		// UploadInboundAsset (streams to the operator, which calls extractord
		// synchronously in the SAME round trip) — because a hang in either leg
		// must not block the batch, let alone the dispatch goroutine, forever.
		// An archive costs explode + N stores + N extracts in ONE upload call,
		// so it gets its own budget, clamped to what the batch has left after
		// reserving time for the attachments still to come.
		remaining := time.Until(deadlineOf(ctx))
		budget, _ := attachmentBudget(a.MIME, remaining, len(ev.Attachments)-i-1)
		fetchCtx, fetchCancel := context.WithTimeout(ctx, budget)
		rc, ferr := fetcher.FetchAttachment(fetchCtx, deps, a.ExternalID)
		if ferr != nil {
			fetchCancel()
			results = append(results, attachmentResult{Filename: a.Filename, MIME: a.MIME, SizeBytes: a.SizeBytes, Outcome: outcomeFailed})
			logger.Info("attachment fetch failed",
				"session", sessRef, "channel", channelRef,
				"filename", sanitizeForNote(a.Filename), "err", ferr.Error())
			continue
		}

		up, uerr := p.Memory.UploadInboundAsset(fetchCtx, ns, sessName, a.MIME, a.Filename, rc)
		closeErr := rc.Close()
		fetchCancel()
		if uerr != nil {
			// The pre-fetch size check above should make this unreachable in the
			// common case (effectiveLimit is clamped to the operator's own
			// ceiling), but a channel kind that under-reports SizeBytes as 0
			// bypasses that check — so this backstop must stay honest too: a
			// too-large upload is permanent (outcomeOversize), never the
			// generic transient outcomeFailed.
			outcome := outcomeFailed
			if errors.Is(uerr, ErrInboundAssetTooLarge) {
				outcome = outcomeOversize
			}
			results = append(results, attachmentResult{Filename: a.Filename, MIME: a.MIME, SizeBytes: a.SizeBytes, Outcome: outcome, Limit: limit})
			logger.Info("attachment upload failed",
				"session", sessRef, "channel", channelRef,
				"filename", sanitizeForNote(a.Filename), "err", uerr.Error())
			continue
		}
		if closeErr != nil {
			// Bytes are already durably stored (uerr == nil above); a close
			// failure on the source stream is not the upload's failure, but it
			// must still be logged, not swallowed.
			logger.Info("attachment source stream close failed after a successful upload",
				"session", sessRef, "channel", channelRef,
				"filename", sanitizeForNote(a.Filename), "err", closeErr.Error())
		}

		switch {
		case up.Unsupported:
			// Bytes ARE stored (up.Ref is set unconditionally on a nil error —
			// see InboundAssetResult's doc); no extractor claims the MIME. NOT a
			// failure: outcomeStored leaves the readability verdict to send time,
			// where the resolved model is actually known.
			results = append(results, attachmentResult{Filename: a.Filename, MIME: a.MIME, SizeBytes: a.SizeBytes, Outcome: outcomeStored, Ref: up.Ref})
		case !up.Extracted && up.Ref != "":
			// No text — extraction failed, or none is configured — but the BYTES
			// ARE STORED. Same state as up.Unsupported above (a ref, no text
			// handle) by a different cause, so the same outcome: a stored file
			// with no text may still be shown to the model natively at send
			// time, and hydration renders it persistently when it can.
			//
			// It must NOT take outcomeFailed, whose "temporary failure" wording
			// is plainly false while the file sits in the request being looked
			// at. The commonest way to land here is a scanned, image-only PDF —
			// precisely what native passthrough exists to serve.
			results = append(results, attachmentResult{Filename: a.Filename, MIME: a.MIME, SizeBytes: a.SizeBytes, Outcome: outcomeStored, Ref: up.Ref})
			logger.Info("attachment stored but extraction unavailable or failed; readability is a send-time question",
				"session", sessRef, "channel", channelRef, "filename", sanitizeForNote(a.Filename))
		case up.Ref == "":
			// No stored bytes, with or without extracted text. Nothing
			// represents this file anywhere, so the transient wording is honest:
			// the upload reported success without a ref, which no implementation
			// does today (InboundAssetResult: Ref is set on a nil error), and a
			// resend would be the user's only recourse if one ever did.
			//
			// Keyed on the MISSING REF, not on !up.Extracted, so the
			// extracted-but-unstored combination lands here too. Keying on
			// extraction sends it to outcomeRead, whose manifest line names a
			// fetch_artifact handle that composeAttachmentBlocks never wrote a
			// block for — handing the agent a handle that does not exist.
			results = append(results, attachmentResult{Filename: a.Filename, MIME: a.MIME, SizeBytes: a.SizeBytes, Outcome: outcomeFailed})
			logger.Info("attachment upload reported success but stored no bytes",
				"session", sessRef, "channel", channelRef, "filename", sanitizeForNote(a.Filename),
				"extracted", up.Extracted)
		default:
			results = append(results, attachmentResult{
				Filename: a.Filename, MIME: a.MIME, SizeBytes: a.SizeBytes, Outcome: outcomeRead,
				Ref: up.Ref, TextRef: up.TextRef, Pages: up.Pages,
			})
			// An exploded archive contributes its members too. They carry no
			// manifest line of their own — the index the archive's TextRef
			// points at is what names them — but they DO get content blocks,
			// which is what makes any of them pinnable later.
			results = append(results, memberResults(up.Ref, up.Members)...)
			if up.ArchiveTruncated {
				logger.Info("archive was only partly opened; the index says so",
					"session", sessRef, "channel", channelRef,
					"filename", sanitizeForNote(a.Filename), "reason", up.ArchiveTruncatedReason)
			}
		}
	}

	// A block is written whenever bytes reached the store, with or without
	// extracted text — see composeAttachmentBlocks for exactly which outcomes
	// that covers.
	blocks = composeAttachmentBlocks(results)

	// One notice for the whole batch, built from the same results the
	// agent-facing note below is built from — the two must never disagree
	// about which files failed.
	p.publishAttachmentFailureNotice(ns, sessName, sessRef, channelRef, results, logger)

	manifest := composeAttachmentManifest(results)
	note := composeAttachmentNote(results)
	switch {
	case manifest == "":
		text = note
	case note == "":
		text = manifest
	default:
		text = manifest + "\n" + note
	}
	return text, blocks
}

// attachmentsCapabilityGranted reads the bound AgentClass's capabilities map
// via pkg/agent/agentcaps — the same grant/active semantics the runner's capability
// Assemble uses, kept in one place so a class appears granted to one
// component and not another can never happen (see pkg/agent/agentcaps's own package
// doc). DefaultOn=false: an AgentClass that says nothing about "attachments"
// is not granted it, matching the CRD's own "absent ⇒ disabled" contract.
func (p *Pipeline) attachmentsCapabilityGranted(ctx context.Context, ns, className string) bool {
	if className == "" {
		return false
	}
	var class spiceboxv1alpha1.AgentClass
	if err := p.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: className}, &class); err != nil {
		log.FromContext(ctx).Info("attachments: AgentClass lookup failed; treating capability as not granted",
			"class", ns+"/"+className, "err", err.Error())
		return false
	}
	grant, gerr := agentcaps.GrantOf(&class, attachmentsCapabilityName)
	if gerr != nil {
		// A malformed {enabled} blob fails closed inside GrantOf/Active already;
		// still log it per AGENTS.md's never-silently-drop-errors rule.
		log.FromContext(ctx).Info("attachments: capability grant config malformed; treating as not granted",
			"class", ns+"/"+className, "err", gerr.Error())
	}
	return agentcaps.Active(false, grant)
}

// effectiveLimit is the min of the channel's configured MaxSizeBytes (when
// positive), the kind's hard maximum, and the operator's absolute upload
// ceiling. That ceiling comes from the leaf assetlimits package, NOT
// pkg/memory/httpsrv: this package is reachable from browser-facing code via
// pipelinehost, and httpsrv pulls in the audit-signing package.
//
// The third clamp matters independently of the first two. A kind may advertise
// AttachmentBounds far above the operator's own per-upload cap, and a file in
// that window would otherwise pass this pre-fetch check, download in FULL, and
// only then trip MaxBytesReader — a wasted download landing on the wrong
// (transient) outcome instead of the cheap, correct pre-fetch oversize note.
func effectiveLimit(spec *spiceboxv1alpha1.ChannelAttachmentsSpec, bounds channelkinds.AttachmentBounds) int64 {
	limit := bounds.MaxSizeBytes
	if spec != nil && spec.MaxSizeBytes != nil && *spec.MaxSizeBytes > 0 &&
		(limit <= 0 || *spec.MaxSizeBytes < limit) {
		limit = *spec.MaxSizeBytes
	}
	if limit <= 0 || assetlimits.MaxInboundAssetBytes < limit {
		limit = assetlimits.MaxInboundAssetBytes
	}
	return limit
}

// effectiveMaxPerMessage is effectiveLimit's counterpart for the per-message
// attachment count.
func effectiveMaxPerMessage(spec *spiceboxv1alpha1.ChannelAttachmentsSpec, bounds channelkinds.AttachmentBounds) int {
	if spec != nil && spec.MaxPerMessage != nil && *spec.MaxPerMessage > 0 &&
		(bounds.MaxPerMessage <= 0 || int(*spec.MaxPerMessage) < bounds.MaxPerMessage) {
		return int(*spec.MaxPerMessage)
	}
	return bounds.MaxPerMessage
}

// attachmentBudget picks the per-attachment sub-deadline and reports whether
// there is enough of the batch budget left to bother.
//
// An archive gets archiveFetchTimeout, clamped to what remains after reserving
// attachmentFetchTimeout for each attachment still to be processed. Without
// that reserve one big archive early in a message would eat the whole batch
// budget and every file after it would report a transient failure it did not
// have.
func attachmentBudget(mime string, remaining time.Duration, laterAttachments int) (budget time.Duration, explode bool) {
	if !extract.IsContainerMIME(mime) {
		return attachmentFetchTimeout, false
	}
	reserve := attachmentFetchTimeout * time.Duration(laterAttachments)
	allowed := remaining - reserve
	if allowed > archiveFetchTimeout {
		allowed = archiveFetchTimeout
	}
	if allowed < archiveFloor {
		// Not enough left to finish an explode. Store the archive whole on the
		// ordinary budget and say nothing false about it.
		return attachmentFetchTimeout, false
	}
	return allowed, true
}

// memberResults turns one archive upload's stored members into
// attachmentResults, each carrying ArchiveRef back to the archive.
//
// They take outcomeMember, so they produce a content block (keyed on Ref) and
// NO manifest line: the archive's index is what names them.
func memberResults(archiveRef string, members []InboundAssetMember) []attachmentResult {
	if len(members) == 0 {
		return nil
	}
	out := make([]attachmentResult, 0, len(members))
	for _, m := range members {
		out = append(out, attachmentResult{
			Filename:   m.Name,
			MIME:       m.MIME,
			SizeBytes:  m.SizeBytes,
			Outcome:    outcomeMember,
			Ref:        m.Ref,
			TextRef:    m.TextRef,
			Pages:      m.Pages,
			ArchiveRef: archiveRef,
		})
	}
	return out
}

// deadlineOf reports ctx's deadline, or a zero-value-safe fallback when it has
// none. processAttachments always wraps ctx with attachmentBatchTimeout before
// the loop, so the fallback is unreachable in production; it exists so a unit
// test calling the loop with a bare context does not silently get a negative
// budget.
func deadlineOf(ctx context.Context) time.Time {
	if d, ok := ctx.Deadline(); ok {
		return d
	}
	return time.Now().Add(attachmentBatchTimeout)
}
