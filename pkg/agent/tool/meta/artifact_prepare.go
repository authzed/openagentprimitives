package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

// Values accepted for artifactPrepareArgs.Source.
const (
	sourceInline        = "inline"
	sourceContainerFile = "container_file"
	sourceToolOutput    = "tool_output"
)

// FileDownloader downloads a provider file by id so its bytes can be inlined
// into an artifact. Satisfied structurally by the anthropic Files client
// (anthropicbridge.SDKFilesClient); kept minimal so meta does not import the
// SDK.
type FileDownloader interface {
	Download(ctx context.Context, fileID string) (io.ReadCloser, error)
}

// ArtifactPrepareConfig wires the artifact_prepare meta tool to the
// operator client, the version-chain service, and the channel-side
// renderer capabilities.
type ArtifactPrepareConfig struct {
	Client         client.Client
	Artifacts      *artifacts.Service
	AvailableKinds []string
	MaxInputBytes  int64
	// MaxInputBytesByKind overrides MaxInputBytes per renderer kind. When a
	// kind is present here, its cap applies; otherwise MaxInputBytes applies.
	MaxInputBytesByKind map[string]int64
	PollInterval        time.Duration
	// FileDownloader is the Tier-2 provider-native downloader used to pull
	// bytes for source=container_file out of the provider's code-execution
	// container. The bytes are inlined into Spec.Payload (subject to the same
	// size cap as source=inline) — the runner has no artifactstore write
	// access, so container files are always inlined, never store-referenced.
	// nil unless native file handling is active; a container_file request
	// with a nil FileDownloader fails closed (IsError), never silently falls
	// back to inline.
	FileDownloader FileDownloader
}

// NewArtifactPrepare constructs the artifact_prepare meta tool.
func NewArtifactPrepare(cfg ArtifactPrepareConfig) tool.Tool {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 250 * time.Millisecond
	}
	if cfg.MaxInputBytes <= 0 {
		cfg.MaxInputBytes = 256 << 10
	}
	return &artifactPrepareTool{cfg: cfg}
}

type artifactPrepareTool struct {
	cfg ArtifactPrepareConfig
}

func (*artifactPrepareTool) Name() string    { return "artifact_prepare" }
func (*artifactPrepareTool) Kind() tool.Kind { return tool.KindMeta }
func (*artifactPrepareTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Passthrough}
}
func (*artifactPrepareTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (t *artifactPrepareTool) Description() string {
	return "Render a versioned artifact (HTML report, etc.) for delivery to the user. " +
		"Create a new artifact, or revise/branch an existing one via `revises`. Returns a `handle`, " +
		"`artifact_id`, `revision_id`, and `seq`; pass the handle or artifact_id to respond_to_user via `attached`. " +
		"Inputs: `kind` (renderer; available on this channel: " + strings.Join(t.cfg.AvailableKinds, ", ") + "), " +
		"`payload` (UTF-8 for text/* kinds, base64 for binary), `source` (inline default, container_file to pull the payload " +
		"from a file you created in your code-execution environment, or tool_output to reuse a prior sandbox tool call's " +
		"captured output — same-session only), `name`/`description` (new artifact only), " +
		"`revises` (a handle, artifact_id, artifact_id#tag, or revision_id to branch from), " +
		"`change_description` (what changed), `tags` (move tags onto this revision; 'latest' is reserved), " +
		"`filename`, `alt_text`, `max_wait_seconds` (default 30, max 300). " +
		"Renderer sanitization may strip parts of HTML; the response includes a `warnings` field."
}

func (*artifactPrepareTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"kind":               {"type": "string"},
			"payload":            {"type": "string", "description": "UTF-8 for text/* kinds; base64 for binary."},
			"source":             {"type": "string", "enum": ["inline", "container_file", "tool_output"], "description": "inline (default): payload is the artifact bytes (UTF-8 or base64). container_file: payload is a provider container file id created via code execution; its bytes are pulled into the artifact store. tool_output: payload is an ArtifactRef from a prior sandbox tool call's captured output (stdout/stderr/output file); its bytes are stored as the artifact, same-session only."},
			"filename":           {"type": "string"},
			"alt_text":           {"type": "string"},
			"name":               {"type": "string", "description": "Human name for a NEW artifact (ignored when revising)."},
			"description":        {"type": "string", "description": "Human description for a NEW artifact (ignored when revising)."},
			"revises":            {"type": "string", "description": "Handle of an existing artifact/revision to revise. Omit to create a new artifact. Forms: artifact-ID (newest), artifact-ID#tag, artrev-ID, or a prior render handle."},
			"change_description": {"type": "string", "description": "What changed in this revision."},
			"tags":               {"type": "array", "items": {"type": "string"}, "description": "Tag names to move onto this revision (e.g. [\"draft\"]). 'latest' is reserved."},
			"max_wait_seconds":   {"type": "integer", "minimum": 1, "maximum": 300}
		},
		"required": ["kind", "payload"]
	}`)
}

type artifactPrepareArgs struct {
	// Kind selects the registered renderer.
	Kind string `json:"kind"`
	// Payload is the renderer's source content.
	Payload string `json:"payload"`
	Source  string `json:"source,omitempty"`
	// Filename and AltText travel with the rendered bytes to the channel.
	Filename string `json:"filename,omitempty"`
	AltText  string `json:"alt_text,omitempty"`
	// Name and Description label the artifact for humans.
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	// Revises is an existing artifact_id to revise; empty creates a new artifact.
	Revises string `json:"revises,omitempty"`
	// ChangeDesc explains this revision, and is meaningful only with Revises.
	ChangeDesc string   `json:"change_description,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	// MaxWaitSeconds bounds the synchronous wait for the render; zero takes the
	// tool's default.
	MaxWaitSeconds int `json:"max_wait_seconds,omitempty"`
}

// artifactPrepareResult is the JSON the tool renders back to the model.
type artifactPrepareResult struct {
	// Handle addresses the prepared artifact in later tool calls.
	Handle string `json:"handle"`
	// ArtifactID is the stable identity across revisions; RevisionID names this
	// one, and Seq is its 1-based position in the revision chain.
	ArtifactID string   `json:"artifact_id,omitempty"`
	RevisionID string   `json:"revision_id,omitempty"`
	Seq        int      `json:"seq,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	// Status is the render outcome the model must branch on.
	Status string `json:"status"`
	// MIME, Size (bytes) and Filename describe the rendered bytes; absent until
	// the render completes.
	MIME     string `json:"mime,omitempty"`
	Size     int64  `json:"size,omitempty"`
	Filename string `json:"filename,omitempty"`
	// Warnings are the sanitizer's findings — content it stripped or rewrote.
	Warnings []spiceboxv1alpha1.SanitizerWarning `json:"warnings,omitempty"`
	// Message carries the failure reason when Status is not a success.
	Message string `json:"message,omitempty"`
}

func (t *artifactPrepareTool) Execute(ctx context.Context, raw json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	var args artifactPrepareArgs
	if res, ok := tool.ParseArgs(raw, &args, t.Name(), `{"kind": "html", "payload": "<h1>hi</h1>"}`); !ok {
		return res, nil
	}
	if !containsString(t.cfg.AvailableKinds, args.Kind) {
		return tool.Result{Content: fmt.Sprintf("artifact_prepare: kind %q is not available on this channel. Available: %v", args.Kind, t.cfg.AvailableKinds), IsError: true, Trusted: true}, nil
	}
	switch args.Source {
	case "", sourceInline, sourceContainerFile, sourceToolOutput:
	default:
		return tool.Result{Content: fmt.Sprintf("artifact_prepare: invalid source %q (must be %q, %q, or %q).", args.Source, sourceInline, sourceContainerFile, sourceToolOutput), IsError: true, Trusted: true}, nil
	}
	maxBytes := t.cfg.MaxInputBytes
	if v, ok := t.cfg.MaxInputBytesByKind[args.Kind]; ok && v > 0 {
		maxBytes = v
	}

	inlinePayload := []byte(args.Payload)
	switch args.Source {
	case sourceContainerFile:
		if t.cfg.FileDownloader == nil {
			return tool.Result{Content: "artifact_prepare: native file handling not available", IsError: true, Trusted: true}, nil
		}
		rc, err := t.cfg.FileDownloader.Download(ctx, args.Payload)
		if err != nil {
			return tool.Result{Content: fmt.Sprintf("artifact_prepare: downloading container file: %v", err), IsError: true, Trusted: true}, nil
		}
		defer rc.Close()
		b, err := io.ReadAll(io.LimitReader(rc, maxBytes+1))
		if err != nil {
			return tool.Result{Content: fmt.Sprintf("artifact_prepare: reading container file: %v", err), IsError: true, Trusted: true}, nil
		}
		if int64(len(b)) > maxBytes {
			return tool.Result{Content: fmt.Sprintf("artifact_prepare: container file exceeds the %d-byte inline limit; large-file (>256KiB) support is not yet implemented.", maxBytes), IsError: true, Trusted: true}, nil
		}
		inlinePayload = b
	case sourceToolOutput:
		artClient, ok := sess.ArtifactClient.(sandbox.ArtifactClient)
		if !ok || artClient == nil {
			return tool.Result{Content: "artifact_prepare: tool_output source unavailable (no artifact client)", IsError: true, Trusted: true}, nil
		}
		if err := validateToolOutputRef(ctx, t.cfg.Client, sess, args.Payload); err != nil {
			return tool.Result{Content: "artifact_prepare: " + err.Error(), IsError: true, Trusted: true}, nil
		}
		rc, err := artClient.Get(ctx, args.Payload)
		if err != nil {
			return tool.Result{Content: fmt.Sprintf("artifact_prepare: reading tool output %q: %v", args.Payload, err), IsError: true, Trusted: true}, nil
		}
		defer rc.Close()
		b, err := io.ReadAll(io.LimitReader(rc, maxBytes+1))
		if err != nil {
			return tool.Result{Content: fmt.Sprintf("artifact_prepare: reading tool output: %v", err), IsError: true, Trusted: true}, nil
		}
		if int64(len(b)) > maxBytes {
			return tool.Result{Content: fmt.Sprintf("artifact_prepare: tool output exceeds the %d-byte inline limit.", maxBytes), IsError: true, Trusted: true}, nil
		}
		inlinePayload = b
	default: // "" or inline
		if int64(len(args.Payload)) > maxBytes {
			return tool.Result{Content: fmt.Sprintf("artifact_prepare: PayloadTooLarge — input %d bytes > max %d. Reduce content and retry.", len(args.Payload), maxBytes), IsError: true, Trusted: true}, nil
		}
	}
	if strings.ContainsAny(args.Filename, "/\\") || len(args.Filename) > 255 {
		return tool.Result{Content: fmt.Sprintf("artifact_prepare: invalid filename %q (no path separators, <=255 chars).", args.Filename), IsError: true, Trusted: true}, nil
	}
	for _, tg := range args.Tags {
		if tg == artifacts.TagLatest {
			return tool.Result{Content: "artifact_prepare: tag \"latest\" is reserved and maintained automatically; choose another name.", IsError: true, Trusted: true}, nil
		}
		if strings.ContainsAny(tg, " #") || tg == "" {
			return tool.Result{Content: fmt.Sprintf("artifact_prepare: invalid tag %q (no spaces or '#', non-empty).", tg), IsError: true, Trusted: true}, nil
		}
	}

	scope := memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}

	var headID, parentRev string
	if args.Revises != "" {
		hID, parent, rendererKind, err := t.cfg.Artifacts.ResolveRevisionTarget(ctx, scope, args.Revises)
		if err != nil {
			return tool.Result{Content: "artifact_prepare: " + err.Error(), IsError: true, Trusted: true}, nil
		}
		if rendererKind != args.Kind {
			return tool.Result{Content: fmt.Sprintf("artifact_prepare: kind %q does not match the artifact's renderer kind %q; revisions must keep the same kind.", args.Kind, rendererKind), IsError: true, Trusted: true}, nil
		}
		headID, parentRev = hID, parent
	} else {
		headID = t.cfg.Artifacts.NewArtifactID()
	}

	maxWait := time.Duration(args.MaxWaitSeconds) * time.Second
	if maxWait <= 0 {
		maxWait = 30 * time.Second
	}
	if maxWait > 300*time.Second {
		maxWait = 300 * time.Second
	}

	spec := spiceboxv1alpha1.ArtifactRenderSpec{
		Kind: args.Kind, Filename: args.Filename, AltText: args.AltText,
		TimeoutSeconds: int32(maxWait / time.Second),
		Payload:        inlinePayload,
	}
	// One constructor for every render CR (artifacts.NewRender): the name is
	// the model-facing `handle`, so the service mints it (see artifacts.RenderName).
	cr := artifacts.NewRender(t.cfg.Artifacts.NewRenderName(sess.Name), sess.Namespace, sess.Name, sess.AgentSessionUID, headID, spec, artifactIntentAnnotations(args, parentRev))
	if err := t.cfg.Client.Create(ctx, cr); err != nil {
		return tool.Result{Content: fmt.Sprintf("artifact_prepare: create CR: %v", err), IsError: true, Trusted: true}, nil
	}

	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return tool.Result{Content: "artifact_prepare: " + err.Error(), IsError: true, Trusted: true}, nil
		}
		var fresh spiceboxv1alpha1.ArtifactRender
		if err := t.cfg.Client.Get(ctx, client.ObjectKeyFromObject(cr), &fresh); err != nil {
			if errors.IsNotFound(err) {
				return tool.Result{Content: "artifact_prepare: CR vanished before completion", IsError: true, Trusted: true}, nil
			}
			time.Sleep(t.cfg.PollInterval)
			continue
		}
		switch fresh.Status.Phase {
		case spiceboxv1alpha1.ArtifactRenderPhaseReady:
			rev, err := t.cfg.Artifacts.FinalizeRevision(ctx, scope, &fresh)
			if err != nil {
				return tool.Result{Content: fmt.Sprintf("artifact_prepare: render Ready but recording the revision failed: %v", err), IsError: true, Trusted: true}, nil
			}
			return marshalArtifactPrepareResult(artifactPrepareResult{
				Handle: fresh.Name, ArtifactID: rev.ArtifactID, RevisionID: rev.RevisionID, Seq: rev.Seq,
				Tags: rev.Tags, Status: "ready", MIME: fresh.Status.OutputMIME, Size: fresh.Status.OutputSize,
				Filename: fresh.Status.OutputFilename, Warnings: fresh.Status.Warnings,
			})
		case spiceboxv1alpha1.ArtifactRenderPhaseFailed:
			return tool.Result{Content: fmt.Sprintf("artifact_prepare: %s — %s", fresh.Status.FailureReason, fresh.Status.FailureMessage), IsError: true, Trusted: true}, nil
		}
		time.Sleep(t.cfg.PollInterval)
	}

	return marshalArtifactPrepareResult(artifactPrepareResult{
		Handle: cr.Name, ArtifactID: headID, Status: "pending",
		Message: "Render still running. Call artifact_await(handle) to wait for completion and record the revision before referencing it in respond_to_user.",
	})
}

// artifactIntentAnnotations stamps versioning intent onto the CR so
// FinalizeRevision (run from artifact_prepare OR artifact_await) can
// record the revision. The render controller ignores these.
func artifactIntentAnnotations(args artifactPrepareArgs, parentRev string) map[string]string {
	a := map[string]string{}
	if parentRev != "" {
		a[artifacts.AnnoParentRevision] = parentRev
	}
	if args.ChangeDesc != "" {
		a[artifacts.AnnoChangeDescription] = args.ChangeDesc
	}
	if len(args.Tags) > 0 {
		a[artifacts.AnnoAppliedTags] = strings.Join(args.Tags, ",")
	}
	if args.Revises == "" {
		if args.Name != "" {
			a[artifacts.AnnoArtifactName] = args.Name
		}
		if args.Description != "" {
			a[artifacts.AnnoArtifactDescription] = args.Description
		}
	}
	return a
}

func marshalArtifactPrepareResult(r artifactPrepareResult) (tool.Result, error) {
	body, err := encodeArtifactPrepareResult(r)
	if err != nil {
		return tool.Result{Trusted: true}, err
	}
	return tool.Result{Content: string(body), Trusted: true}, nil
}

// encodeArtifactPrepareResult is the ONE encoder for the bytes artifact_prepare
// and artifact_await hand the model.
//
// It puts EVERY unordered collection in the result into its canonical order
// rather than trusting its input to arrive sorted. Both producers sort too —
// channelassets.SortWarnings at the renderer's map-diff, and
// artifacts.tagsPointingAt at the head-tag scan — so for a live call this is a
// no-op. Sorting here anyway makes the ENCODER, not its callers, the definition
// of the canonical bytes, which is what CanonicalizeResult rests on.
//
// Two fields have now needed this, discovered one replay apart. When a third
// collection is added to artifactPrepareResult it belongs here on the same day:
// TestMetaToolResults_AreDeterministic is what says so out loud.
func encodeArtifactPrepareResult(r artifactPrepareResult) ([]byte, error) {
	// Every collection below is COPIED, never sorted in place. r's slice
	// headers come from the caller's fetched CR status and from the artifact
	// service's RevisionResult, so an in-place sort would reorder an object the
	// caller still holds — an encoder mutating its argument is a side effect
	// nobody reading the call site would expect.
	if len(r.Warnings) > 0 {
		ws := slices.Clone(r.Warnings)
		slices.SortStableFunc(ws, func(a, b spiceboxv1alpha1.SanitizerWarning) int {
			return strings.Compare(
				channelassets.WarningSortKey(a.Kind, a.Name, a.Action),
				channelassets.WarningSortKey(b.Kind, b.Name, b.Action))
		})
		r.Warnings = ws
	}
	if len(r.Tags) > 0 {
		tags := slices.Clone(r.Tags)
		slices.Sort(tags)
		r.Tags = tags
	}
	body, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("marshal result: %w", err)
	}
	return body, nil
}

// resultCanonicalizers maps a meta tool's LLM-facing name to the function that
// re-expresses one of its RECORDED results the way this package emits results
// today. See CanonicalizeResult.
//
// A map rather than a switch for the usual reason: a tool whose result gains a
// canonical form registers here and nothing that consumes this branches on a
// name.
var resultCanonicalizers = map[string]func([]byte) ([]byte, error){
	"artifact_prepare": canonicalizeArtifactPrepareResult,
	"artifact_await":   canonicalizeArtifactPrepareResult,
}

// CanonicalizeResult re-expresses a RECORDED result of one of this package's
// tools the way this package's own encoder emits it today, reporting whether it
// knows the tool at all.
//
// # What it is for
//
// A whole-session capture derives a step's expectation from the bytes a meta
// tool returned during the recorded run. A meta tool RUNS at replay, so those
// bytes have to be reproducible — but a value that carries no information can
// still differ, and one did: the sanitizer emitted its warnings in Go map
// order, so two identical renders produced the same set in a different
// sequence. Every captured session recorded one arbitrary permutation.
//
// The order is defined now (channelassets.SortWarnings). This lets a capture
// state the recorded VALUES in that definition, instead of pinning whichever
// permutation the recorded run happened to get — a permutation current code
// cannot produce, so the bundle would emit clean and then fail.
//
// # Why it cannot paper over a regression
//
// It decodes into this package's own result type and re-encodes with this
// package's own encoder, so what comes out is exactly what the tool would emit
// for those values. Every value survives: a changed size, a dropped tag, a
// different status or a warning that is no longer reported all still differ at
// replay and still fail the step. The only thing it can absorb is an ordering
// the encoder now fixes — and if that sort were removed, the replay would emit
// a random order and fail against this canonical one, which is the correct
// answer rather than a hidden one.
//
// Returns (payload, false) unchanged for a tool it does not own, and for a
// payload it cannot decode — a refusal message is not a result.
func CanonicalizeResult(toolName string, payload []byte) ([]byte, bool) {
	fn, ok := resultCanonicalizers[toolName]
	if !ok {
		return payload, false
	}
	out, err := fn(payload)
	if err != nil {
		return payload, false
	}
	return out, true
}

func canonicalizeArtifactPrepareResult(payload []byte) ([]byte, error) {
	var r artifactPrepareResult
	dec := json.NewDecoder(bytes.NewReader(payload))
	// A field this type does not know is a field the encoder cannot re-emit, so
	// re-encoding would silently DROP it and the canonical form would be a
	// weaker claim than the recording. Refusing is what keeps the
	// normalization order-only.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, err
	}
	return encodeArtifactPrepareResult(r)
}

// validateToolOutputRef confirms ref was produced by a ToolCall owned by
// sess (same AgentSession, by Kind+Name+UID — see tool.OwnedBySession) —
// either as its captured stdout/stderr or one of its OutputArtifacts. The
// operator's /debug/artifact endpoint already session-scopes reads, but this
// check is defense-in-depth and gives a precise rejection message instead of
// a generic fetch failure.
func validateToolOutputRef(ctx context.Context, c client.Client, sess *tool.SessionContext, ref string) error {
	if ref == "" {
		return fmt.Errorf("tool_output ref %q was not produced by this session (cross-session refs are rejected)", ref)
	}
	var list spiceboxv1alpha1.ToolCallList
	if err := c.List(ctx, &list, client.InNamespace(sess.Namespace)); err != nil {
		return fmt.Errorf("listing session tool calls: %w", err)
	}
	for _, tc := range list.Items {
		if !tool.OwnedBySession(tc.OwnerReferences, sess) {
			continue
		}
		if tc.Status.StdoutArtifactRef == ref || tc.Status.StderrArtifactRef == ref {
			return nil
		}
		for _, oa := range tc.Status.OutputArtifacts {
			if oa.ArtifactRef == ref {
				return nil
			}
		}
	}
	return fmt.Errorf("tool_output ref %q was not produced by this session (cross-session refs are rejected)", ref)
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
