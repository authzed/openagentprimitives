package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observation"
)

// RecordObservationToolName is the observation-recording tool's name, exported
// because the runner's built-in WRITE declaration names this tool specifically:
// the info-leakage audience gate reads the write destination out of the call's
// `resource` argument, and the declaration that says so is built-in rather
// than CRD-settable — a spec author who could name a tool's write destination
// would be choosing where data lands.
//
// Exported rather than spelled as a literal at the declaration site for the
// reason the read path already paid for: a predicate keyed on a bare string
// cannot be told apart from any other occurrence of the same name, so a rename
// silently detaches the gate from the tool it governs.
const RecordObservationToolName = "record_observation"

// NewRecordObservation builds the tool a session calls to write an
// observation into a resource's memory pool, for a later session holding a
// slot on the same resource to read.
func NewRecordObservation() tool.Tool {
	return &recordObservationTool{}
}

type recordObservationTool struct{}

func (*recordObservationTool) Name() string    { return RecordObservationToolName }
func (*recordObservationTool) Kind() tool.Kind { return tool.KindMeta }

// Permission returns the TRIVIAL permission: Stateless, no Check.
//
// This tool reaches no SpiceDB resource the tool checker could ask about, and
// says so. Its destination is a resource pool named per call, so its TYPE
// varies between calls — one call writes to a "dossier", the next to a
// "ledger" — while PermissionCheck.ResourceType is a STATIC string with no
// template interpolation (only the resource ID is resolved from args). No fixed
// declaration can describe that, so there is no check for this permission to
// carry.
//
// What the call IS authorized by, in two places, neither of them here:
//
//   - the memory door, server-side, which proves the named destination is in
//     pools.ForSession(...).Write before honouring it (httpsrv.destinationFor);
//   - the PreToolCall info-leakage audience gate, which compares this session's
//     accumulated taint against the destination pool's audience.
//
// Reaching that second gate is a ROUTING question, and it is answered by the
// tool.PipelineRouted marker below rather than by this permission. An earlier
// version declared Readwrite for exactly that routing, on the false premise
// that apply_workspace shipped the same "check-requiring StateImpact + nil
// Check" shape. apply_workspace declares External, which takes a different arm
// of toolcheck.Checker.check entirely; Readwrite shares its arm with Readonly,
// and that arm denies a nil Check outright — "internal: stateImpact requires a
// check but none was supplied" — on every call in the CRD-default enforcing
// mode, with needsApproval false so no card is raised either. Since
// ToolCallAuthz runs at order 20 and InfoLeakAudience at order 50, that denial
// also made the cross-write gate unreachable. See tool.PipelineRouted.
func (*recordObservationTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}

// PipelineRouted marks record_observation as a meta tool that must flow through
// the CONTAINED dispatch pipeline even though its dispatch permission is
// trivial. The PreToolCall info-leakage audience gate — the thing that refuses
// a write whose destination pool has a wider audience than this session's reads
// permit — runs only on that path. Skip it and the marker's absence is silent:
// the hook never sees the call, and a cross-resource write lands ungated.
//
// Dispatch stays Stateless: no per-call SpiceDB check, no per-call approval.
// ToolCallAuthz and McpTrust no-op on the trivial permission exactly as they do
// for any ungated meta tool; only the leakage gate is added.
func (*recordObservationTool) PipelineRouted() bool { return true }

func (*recordObservationTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*recordObservationTool) Description() string {
	return "Record a durable observation about a resource, for a later session to read. " +
		"`resource` is \"<type>:<id>\" — the same reference a memory entry's scope reports. " +
		"Only resources this session holds a write grant on can be written to. " +
		"A write is refused when this session has read from a resource whose readers " +
		"do not all have access to the destination, so record an observation about a " +
		"resource before reading from an unrelated one where possible."
}

func (*recordObservationTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"additionalProperties":false,
		"properties":{
			"resource":{
				"type":"string",
				"description":"The resource to write this observation into, as \"<type>:<id>\" — the same reference a memory entry's scope reports. Refused unless this session holds a write grant on it."
			},
			"text":{
				"type":"string",
				"description":"The observation itself — what you concluded about the resource, for a later session's own judgement to read."
			},
			"tags":{
				"type":"array",
				"items":{"type":"string"},
				"description":"Free-form tags for later filtering by query_memory/search_memory."
			}
		},
		"required":["resource","text"]
	}`)
}

type recordObservationArgs struct {
	// Resource is "<type>:<id>", parsed through memory.ParseResourceRef — the
	// SAME parser httpsrv's destinationFor and the info-leakage audience
	// gate's evalPoolWrite use, so this tool cannot refuse a ref one of those
	// two would accept, or the reverse. Read from the TOP LEVEL of the call's
	// raw JSON — never from a nested `args` object — because that is exactly
	// how the PreToolCall gate resolves the same field (resolveToolArgAsExecuted
	// in pkg/authz/hooks/deps.go) for a call with no operation_id envelope,
	// which is what every well-formed call to this flat-schema meta tool looks
	// like. A call that disagrees with itself (a top-level `resource` alongside
	// a differing nested `args.resource`) is refused by the gate before
	// Execute ever runs, so this struct does not need its own
	// duplicate-detection — only to read the SAME field the gate does.
	Resource string   `json:"resource"`
	Text     string   `json:"text"`
	Tags     []string `json:"tags"`
}

func (t *recordObservationTool) Execute(ctx context.Context, raw json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	var a recordObservationArgs
	if res, ok := tool.ParseArgs(raw, &a, t.Name(), `{"resource":"dossier:d-1","text":"prefers async review"}`); !ok {
		return res, nil
	}

	dest, err := memory.ParseResourceRef(a.Resource)
	if err != nil {
		return tool.Result{
			Content: fmt.Sprintf("%s: resource %v", t.Name(), err),
			IsError: true, Trusted: true,
		}, nil
	}
	if strings.TrimSpace(a.Text) == "" {
		return tool.Result{Content: t.Name() + ": text must not be empty", IsError: true, Trusted: true}, nil
	}

	if sess == nil || sess.Mem == nil {
		return tool.Result{Content: t.Name() + ": memory not available", IsError: true, Trusted: true}, nil
	}
	// memory.PoolWriter, not tool.MemoryQuerier: PutToPool is deliberately not
	// part of memory.Memory (see its own doc), so this asserts a NARROWER
	// capability out of sess.Mem rather than growing a tool.SessionContext
	// field every tool would carry — the same shape K8sClient / ArtifactClient
	// already use, just via a named interface instead of `any`. A concrete
	// sess.Mem that fails this assertion refuses readably here, rather than
	// panicking; see memory.PoolWriter's own doc for why the FORWARDING half
	// (provenance.SigningMemory, which every production sess.Mem is wrapped
	// in) has to fail loudly on ITS OWN inner instead of silently answering
	// not-ok up through this same assertion.
	pw, ok := sess.Mem.(memory.PoolWriter)
	if !ok {
		return tool.Result{
			Content: t.Name() + ": this session's memory backend does not support writing into a resource pool",
			IsError: true, Trusted: true,
		}, nil
	}

	// observation.NewEntry, not a hand-assembled memory.Entry: it stamps ID
	// and CreatedAt, both of which EntryDigest signs — an entry built any
	// other way here previously reached the wire with neither, so
	// verify-on-write rejected every call once httpsrv assigned its own ID and
	// recomputed the digest over it. See NewEntry's own doc.
	entry, merr := observation.NewEntry(a.Text, a.Tags)
	if merr != nil {
		return tool.Result{Trusted: true}, fmt.Errorf("%s: %w", t.Name(), merr)
	}

	sessionScope := memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}
	stored, err := pw.PutToPool(ctx, sessionScope, dest, entry)
	if err != nil {
		// Trust is decided by the error's provenance, the same way recallError
		// decides it for the three recall tools (see platformAuthored): a
		// memory sentinel this codebase composes end to end stays Trusted, so
		// the model reliably sees why and can adjust; anything else — a
		// transport failure, an upstream body a 5xx interpolated — stays
		// UNtrusted, so the runner's content guard still inspects it. The
		// write-grant refusal from the pool-write gate (pkg/memory/httpsrv's
		// destinationFor) is not yet a registered sentinel, so it currently
		// reads as untrusted too; harmless, since its text is fixed and
		// platform-composed either way.
		return tool.Result{
			Content: fmt.Sprintf("%s: %v", t.Name(), err),
			IsError: true,
			Trusted: platformAuthored(err),
		}, nil
	}

	out, merr := json.Marshal(struct {
		ID    string       `json:"id"`
		Scope memory.Scope `json:"scope"`
	}{ID: stored.ID, Scope: stored.Scope})
	if merr != nil {
		return tool.Result{Trusted: true}, fmt.Errorf("%s: marshal result: %w", t.Name(), merr)
	}
	// Trusted: the response names only the stored entry's server-assigned id
	// and the (already-proved) destination scope — no content this session or
	// another one authored — so there is nothing here for the content guard
	// to usefully inspect.
	return tool.Result{Content: string(out), Trusted: true}, nil
}
