// pkg/channels/channelkinds/slack/metaagent_approval_refs.go
//
// In-process cache shared between the metaagent scope-approval sender
// (writes) and the listener's Show Details handler (reads).
//
// Also provides public helpers used by internal/cmd/channelsd/metaagent_handlers.go:
//
//   - NewBotClient(sec) wraps the package-private newSlackAPIClient.
//   - MetaagentApprovalButtonValue builds the JSON value embedded in
//     metaagent scope-approval buttons.
//   - ResolveUserIDFromCanonical wraps the package-private canonical resolver.
package slack

import (
	"context"
	"encoding/json"
	"sync"

	corev1 "k8s.io/api/core/v1"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// NewBotClient returns a *slackapi.Client configured with the bot-token
// from sec. Returns nil when the token is absent. Exposed so
// internal/cmd/channelsd can build a client from a resolved Secret without
// importing the unexported newSlackAPIClient helper.
func NewBotClient(sec *corev1.Secret) slackClient {
	return newSlackAPIClient(sec)
}

// ResolveUserIDFromCanonical translates a canonical SpiceDB subject into a
// Slack user_id (via users.lookupByEmail for email-typed canonicals). Exposed
// so internal/cmd/channelsd's raw-JSON metaagent handlers can address a notice whose
// publisher only knew the canonical subject — the operator's SessionFork deny
// path, which holds PendingRestart.TriggeredBy and no channel-native id.
//
// Fail-loud, like the helper it wraps: an unresolvable canonical is an error,
// never ""+nil.
func ResolveUserIDFromCanonical(ctx context.Context, cli slackClient, canonical identity.CanonicalUserID) (string, error) {
	return resolveSlackUserIDFromCanonical(ctx, cli, canonical)
}

// Metaagent approval-button decision strings. These are the values carried
// in the button-value JSON's "d" field and decoded by the listener's switch.
// Mid-session uses Approve/Deny/ShowDetails; cold-start uses the five
// ApproveCleaned/ApproveOriginal/RunWithoutScope/Deny/ShowDetails. The three
// cold-start "proceed" variants must match the ColdStart* action constants in
// internal/cmd/authzd so the action string flows button → applied-payload →
// Decision.Action → cold-start decider unchanged.
const (
	MetaagentDecisionApprove         = "approve"
	MetaagentDecisionDeny            = "deny"
	MetaagentDecisionShowDetails     = "show_details"
	MetaagentDecisionApproveCleaned  = "approve_cleaned"
	MetaagentDecisionApproveOriginal = "approve_original"
	MetaagentDecisionRunWithoutScope = "run_without_scope"
)

// MetaagentApprovalButtonValue encodes the JSON value that must be
// embedded in every metaagent scope-approval button. The listener
// extracts the session ref, requestId, and decision from this value.
func MetaagentApprovalButtonValue(requestID, sessRef, decision string) string {
	v := metaagentApprovalValue{
		V: "metaagent_approval",
		R: requestID,
		S: sessRef,
		D: decision,
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// MetaagentApprovalRef holds the information needed to render the Show Details
// ephemeral after a channelsd restart or from the listener's block_action path.
type MetaagentApprovalRef struct {
	RequestID       string
	Requester       string // Slack user_id of the requester
	Verbatim        string
	ApproverSummary string
	SkippedExplain  string
	CaveatExplain   string
	// ColdStart + CleanedTask are populated for new-session first-turn
	// approvals so Show Details can echo the task the agent will run.
	ColdStart   bool
	CleanedTask string
	// ChannelID + ThreadTS locate where the (ephemeral, cold-start) approval
	// block was posted, so the button handler can post a permanent resolution
	// message into the same thread once the approver decides.
	ChannelID string
	ThreadTS  string
}

// RememberMetaagentApproval records the details of a posted metaagent
// scope-approval block so the listener's Show Details handler can render
// them when the approver clicks. Called by internal/cmd/channelsd after it posts
// the block. Keyed by ref.RequestID. The cache is the per-process shared
// instance the listener reads from (see Kind.NewListener), so a write here
// is visible to handleMetaagentShowDetails.
func (k *Kind) RememberMetaagentApproval(ref MetaagentApprovalRef) {
	if ref.RequestID == "" {
		return
	}
	k.sharedMetaagentApprovalRefs().put(ref)
}

// metaagentApprovalRefCache is an in-memory map from RequestID →
// MetaagentApprovalRef. Thread-safe; no TTL (entries are small and approval
// blocks are cleared after the decision lands). Shared between the sender
// (writes) and the listener's Show Details path (reads).
type metaagentApprovalRefCache struct {
	mu sync.Mutex
	m  map[string]MetaagentApprovalRef
}

func newMetaagentApprovalRefCache() *metaagentApprovalRefCache {
	return &metaagentApprovalRefCache{m: map[string]MetaagentApprovalRef{}}
}

func (c *metaagentApprovalRefCache) put(r MetaagentApprovalRef) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[r.RequestID] = r
}

func (c *metaagentApprovalRefCache) get(requestID string) (MetaagentApprovalRef, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.m[requestID]
	return r, ok
}
