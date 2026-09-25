package scope

// MetaagentOutput is the structured product of the two-call LLM
// pipeline (extractor → classifier → composer). Delta is load-bearing;
// the *Explain fields are display text only.
//
// When CannotAddress=true, no approval round-trip is performed and
// CannotAddressMessage is posted as a metaagent_notice to the requester.
// Delta in that state is typically empty; consumers prefer
// CannotAddress over Delta if both are set (defensive).
type MetaagentOutput struct {
	// Delta is the scope change approving would commit; empty means no change.
	Delta ScopeDelta `json:"delta"`
	// Skipped is what the request asked for but the classifier dropped.
	Skipped []SkippedItem `json:"skipped,omitempty"`
	// Caveats are the conditions attached to Delta; empty means unconditional.
	Caveats []CaveatItem `json:"caveats,omitempty"`

	// ApproverSummary is the one-sentence headline of approving; display only.
	ApproverSummary string `json:"approverSummary,omitempty"`
	// SkippedExplain narrates Skipped in prose; empty when nothing was skipped.
	SkippedExplain string `json:"skippedExplain,omitempty"`
	// CaveatExplain narrates Caveats in prose; empty when there are none.
	CaveatExplain string `json:"caveatExplain,omitempty"`

	// CannotAddress means no approval is solicited at all; it wins over Delta.
	CannotAddress bool `json:"cannotAddress,omitempty"`
	// CannotAddressMessage is posted to the requester explaining the refusal.
	CannotAddressMessage string `json:"cannotAddressMessage,omitempty"`
}

// MetaagentApprovalPayload is the metaagent scope-approval prompt, from the hook
// that composes it through to the channel that renders it.
//
// It makes two hops with the same field set. In-process it rides
// pipeline.ApprovalAsk.Payload, whose generic contract is a map[string]any —
// ToAskPayload and MetaagentApprovalPayloadFromAsk are the codec for that hop.
// Then it is marshalled as the JSON body of
// ap.session.<ns>.<name>.out.metaagent_scope_approval.
//
// The json tags ARE the wire contract: channelsd's renderer, its Show Details
// path, and the metaagent_audit record all key off these exact names, so
// renaming one breaks every prompt already in flight. Presence matters as much
// as value — the renderer picks the five-button cold-start block over the
// three-button mid-session one by testing for the coldStart KEY, which is why
// the cold-start-only fields are omitempty rather than always emitted.
type MetaagentApprovalPayload struct {
	// RequestID is the approval id minted at publish time and echoed by the
	// approving click. Carried on the wire only: the Host mints it, so the
	// in-process ask deliberately has no copy to disagree with.
	RequestID string `json:"requestId,omitempty"`

	// Requester is the channel-native user id of whoever asked for the scope
	// change — who the outcome notice is addressed to, NOT who may approve it.
	Requester string `json:"requester"`

	// Verbatim is the requester's untrusted request text, rendered quoted so the
	// approver reads what was actually asked rather than a paraphrase of it.
	Verbatim string `json:"verbatim"`

	// ApproverSummary is the one-sentence composed effect of approving, and the
	// headline of the prompt. Never empty: the Decide hook substitutes a generic
	// line when the composer returns nothing.
	ApproverSummary string `json:"approverSummary"`

	// SkippedExplain narrates the parts of the request that were dropped rather
	// than applied. Empty when nothing was skipped.
	SkippedExplain string `json:"skippedExplain"`

	// CaveatExplain narrates the conditions attached to the delta. Empty when
	// there are none.
	CaveatExplain string `json:"caveatExplain"`

	// Applied is the delta that approving actually commits. It is the
	// load-bearing field — the prose fields only describe it.
	Applied ScopeDelta `json:"applied"`

	// Skipped is the structured form of what SkippedExplain narrates, kept so
	// Show Details can render the technical view without re-classifying.
	Skipped []SkippedItem `json:"skipped"`

	// Caveats is the structured form of what CaveatExplain narrates, kept for
	// the same reason as Skipped.
	Caveats []CaveatItem `json:"caveats"`

	// ColdStart marks a new-session first-turn approval, which renders five
	// actions instead of three.
	ColdStart bool `json:"coldStart,omitempty"`

	// CleanedTask is the PII- and scope-language-stripped restatement of the
	// task the agent runs if approved. Cold-start only.
	CleanedTask string `json:"cleanedTask,omitempty"`

	// InboxIdx is the turn the cold start belongs to. In-process ONLY — the Host
	// needs it to write the cold_start_task, and no channel renders it, so it is
	// kept off the wire rather than published for nobody.
	InboxIdx int `json:"-"`
}

// ToAskPayload renders the payload as the map[string]any that
// pipeline.ApprovalAsk.Payload requires.
//
// The cold-start-only keys are omitted entirely when ColdStart is false, so the
// map's key set matches the presence test the renderer applies downstream.
// RequestID is never included: the Host mints it at publish time, and carrying
// an empty one here would create a second place to look for it.
func (p MetaagentApprovalPayload) ToAskPayload() map[string]any {
	m := map[string]any{
		"requester":       p.Requester,
		"verbatim":        p.Verbatim,
		"approverSummary": p.ApproverSummary,
		"skippedExplain":  p.SkippedExplain,
		"caveatExplain":   p.CaveatExplain,
		"applied":         p.Applied,
		"skipped":         p.Skipped,
		"caveats":         p.Caveats,
	}
	if p.ColdStart {
		m["coldStart"] = true
		m["cleanedTask"] = p.CleanedTask
		m["inboxIdx"] = p.InboxIdx
	}
	return m
}

// MetaagentApprovalPayloadFromAsk decodes a pipeline.ApprovalAsk.Payload map
// back into the struct.
//
// A missing key or a value of the wrong type yields that field's zero value
// rather than an error. The map is built in-process by ToAskPayload, so a type
// mismatch is a wiring bug in a caller (in practice, a hand-built test fixture)
// rather than untrusted input — and a prompt missing one prose field is still a
// prompt a human can act on, whereas refusing to build it would strand the
// session waiting on an approval that never arrives.
func MetaagentApprovalPayloadFromAsk(m map[string]any) MetaagentApprovalPayload {
	p := MetaagentApprovalPayload{
		Requester:       askString(m, "requester"),
		Verbatim:        askString(m, "verbatim"),
		ApproverSummary: askString(m, "approverSummary"),
		SkippedExplain:  askString(m, "skippedExplain"),
		CaveatExplain:   askString(m, "caveatExplain"),
		CleanedTask:     askString(m, "cleanedTask"),
		InboxIdx:        askInt(m, "inboxIdx"),
	}
	if v, ok := m["applied"].(ScopeDelta); ok {
		p.Applied = v
	}
	if v, ok := m["skipped"].([]SkippedItem); ok {
		p.Skipped = v
	}
	if v, ok := m["caveats"].([]CaveatItem); ok {
		p.Caveats = v
	}
	if v, ok := m["coldStart"].(bool); ok {
		p.ColdStart = v
	}
	return p
}

func askString(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

// askInt accepts float64 as well as int: a map that has round-tripped through
// JSON carries every number as float64.
func askInt(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return 0
}
