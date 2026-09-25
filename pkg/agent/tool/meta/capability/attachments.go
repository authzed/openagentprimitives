package capability

import (
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/platform/extract"
)

func init() { Register(&attachmentsCapability{}) }

// attachmentsCapability is the AgentClass half of the inbound-attachment gate;
// the other two halves (Channel.spec.attachments.enabled and the bound kind
// implementing AttachmentFetcher) live in channelsd's pipeline, a different
// process, which reads this grant through pkg/agent/agentcaps rather than
// through Offer. OPT-IN: a class that says nothing gets no attachment
// ingestion, matching the CRD field's own "absent ⇒ disabled" contract.
//
// Offer injects the same modality meta tools artifactsCapability does
// (fetch_artifact) and NOTHING else — no artifact_prepare/await/history/
// offer_view. Those four create, await, list and publish an ArtifactRender: a
// far broader grant than "let the agent read a file a user sent it", and
// requiring the artifacts capability for fetch_artifact would force an operator
// who wants the narrow thing into granting the broad one. fetch_artifact is
// structurally read-by-handle over the session's own memory scope, already
// bounded by the session token, so its only requirement is a wired
// RunnerEnv.ArtifactReader — checked directly below.
//
// Granting BOTH attachments and artifacts is fine: Assemble dedups by tool name
// and both copies are built from the same RunnerEnv.
type attachmentsCapability struct{}

func (attachmentsCapability) Name() string          { return "attachments" }
func (attachmentsCapability) DefaultOn() bool       { return false }
func (attachmentsCapability) Infrastructural() bool { return false }

// ParseConfig validates the archive bounds at APPLY time, so a malformed or
// out-of-range config fails where an operator is looking — on the AgentClass's
// CapabilitiesValid condition — rather than silently at the moment a user
// uploads a zip weeks later.
//
// It returns no Config: the resolved limits are needed in the OPERATOR, which
// re-parses the same raw bytes with the same function when an archive actually
// arrives. Handing them back here would create a second copy that the runner
// has no use for and that could drift from the one that is enforced.
func (attachmentsCapability) ParseConfig(raw json.RawMessage) (Config, error) {
	if _, err := extract.ParseLimits(raw, extract.DefaultLimits); err != nil {
		return nil, err
	}
	return nil, nil
}

// Offer mirrors artifactsCapability.Offer's channel-attached gate (nil
// Binding ⇒ inactive, not a skip — there is nothing to read attachments
// from) but has exactly one further requirement: a wired ArtifactReader.
// Missing that is a SkipReason, never silent — an operator who granted
// attachments but is running a runner build/config that never wires
// ArtifactReader (or, in the e2e harness, forgot Harness.SetArtifactStore)
// needs a log line naming why fetch_artifact didn't show up, not quiet
// absence.
func (attachmentsCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	if o.Binding == nil {
		return nil, nil // not channel-attached → inactive, not a skip
	}
	if o.Env.ArtifactReader == nil {
		return nil, &SkipReason{Capability: "attachments", Reason: "artifact reader not available"}
	}
	tools := modalityMetaTools(o.Env)
	// show_attachment is offered only when the runner wired a pin function.
	// Absent (kubectl-driven runs, older wiring), the agent simply never sees
	// the tool — better than offering one whose every call would fail. Not a
	// SkipReason: unlike ArtifactReader, its absence costs no capability the
	// operator asked for, since the window only evicts in image-heavy
	// sessions.
	if o.Env.PinAttachment != nil {
		tools = append(tools, meta.NewShowAttachment(meta.ShowAttachmentConfig{Pin: o.Env.PinAttachment}))
	}
	return tools, nil
}
