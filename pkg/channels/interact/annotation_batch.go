package interact

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/viewurn"
)

func init() { Register(annotationBatchKind{}) }

const (
	maxAnnotations   = 50
	maxCommentChars  = 2000
	maxDOMFieldChars = 2000
	// maxChipChars bounds the short enum-ish fields (Intent/Severity/Target):
	// not a DoS control (the request body is already 64 KB-capped upstream),
	// just defense-in-depth bounding before these reach the LLM.
	maxChipChars = 64
)

// annotationBatchKind routes a browser annotation batch into the session as one
// numbered, untrusted-delimited user turn (see annotation_envelope.go). It
// defers the raw echo: the bundle is too large and too structured to mirror
// verbatim, so the runner authors the human-readable summary echo instead.
type annotationBatchKind struct{}

// annotationBatchPayload is the wire shape of the raw JSON /interact submits
// for this kind.
type annotationBatchPayload struct {
	// Annotations are the batch's entries in client order; empty is refused,
	// and more than maxAnnotations is refused rather than truncated.
	Annotations []annotation `json:"annotations"`
}

func (annotationBatchKind) Name() string { return "annotation_batch" }

// Permission is "interact" (agentsession#interact) — any principal who may
// converse with the session, not just its owner/approver.
func (annotationBatchKind) Permission() string { return "interact" }

// ViaSub places annotation batches at the artifact-view /annotations sub-URN, so
// the turn's Via (urn:ap:view:artifact:<id>/annotations) verifiably marks it as
// an annotation batch — the runner recognizes it by this, not by scanning text.
func (annotationBatchKind) ViaSub() string { return viewurn.SubAnnotations }

func (annotationBatchKind) Submit(ctx context.Context, deps Deps, ns, name, subject, via string, raw json.RawMessage) (Result, error) {
	var pl annotationBatchPayload
	if err := json.Unmarshal(raw, &pl); err != nil {
		return Result{}, fmt.Errorf("interact: annotation_batch: decode payload: %w", err)
	}
	if len(pl.Annotations) == 0 {
		// Refused before any NATS request: an empty batch is a client bug (or a
		// UI double-submit), never a routable message.
		return Result{}, fmt.Errorf("interact: annotation_batch: empty batch")
	}
	if len(pl.Annotations) > maxAnnotations {
		return Result{}, fmt.Errorf("interact: annotation_batch: too many annotations (%d > %d)", len(pl.Annotations), maxAnnotations)
	}
	// Renumber authoritatively server-side (1..n) and truncate untrusted strings
	// before they reach the LLM.
	for i := range pl.Annotations {
		pl.Annotations[i].Index = i + 1
		clampAnnotation(&pl.Annotations[i])
	}

	text, err := buildAnnotationEnvelope(pl.Annotations)
	if err != nil {
		return Result{}, err
	}

	// The canonical subject is "user:<base64url(email)>"; decoding recovers the
	// original email, from which channelsd's HandleViewMessage re-derives the
	// SAME canonical id, so this round-trips.
	email := identity.DecodeForDisplay(subject)
	ext := channelkinds.ExternalIdentity{Kind: "idp", Email: identity.Email(email), ExternalID: identity.RawExternalID(email)}

	// deferEcho=true: channelsd suppresses the raw echo (the bundle is too
	// large and structured to mirror verbatim) and the runner authors a
	// human-readable summary echo instead.
	dec, err := channelkinds.RequestViewMessage(channelkinds.Deps{NATSRequest: deps.NATSRequest}, ns, name, text, via, ext, true, "")
	if err != nil {
		return Result{Outcome: dec.Outcome.String(), Notice: dec.Notice.ToWire()}, fmt.Errorf("interact: annotation_batch: %w", err)
	}
	return Result{Outcome: dec.Outcome.String(), Notice: dec.Notice.ToWire()}, nil
}

// clampAnnotation truncates untrusted, DOM-derived strings before they reach
// the LLM. Comment is user-authored but still bounded, so one annotation
// cannot dominate the batch.
func clampAnnotation(a *annotation) {
	clamp := func(s string, n int) string {
		if len(s) > n {
			return s[:n]
		}
		return s
	}
	a.Comment = clamp(a.Comment, maxCommentChars)
	a.ElementText = clamp(a.ElementText, maxDOMFieldChars)
	a.SelectedText = clamp(a.SelectedText, maxDOMFieldChars)
	a.NearbyText = clamp(a.NearbyText, maxDOMFieldChars)
	a.Intent = clamp(a.Intent, maxChipChars)
	a.Severity = clamp(a.Severity, maxChipChars)
	a.Target = clamp(a.Target, maxChipChars)
}
