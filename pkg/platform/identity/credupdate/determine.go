// Package credupdate holds the credential-update determination: the decision
// about whether an agent's claim that a credential has died justifies putting
// a credential-entry form in front of a human.
//
// Determine is deliberately PURE — no k8s client, no network, no clock. All
// I/O (the OAuth refresh attempt, the live probe, the corroboration lookup)
// happens in the reconciler, which then hands the results here. That split is
// what lets the entire verdict matrix be tested without envtest, and it keeps
// the security-critical logic in one readable function.
package credupdate

import (
	"fmt"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
)

// Tier is how much confidence backs an opened card. It drives the card's
// wording, not whether the card appears.
type Tier string

const (
	// TierNone: no card. The request is refused.
	TierNone Tier = "none"
	// TierVerified: the provider itself rejected the credential.
	TierVerified Tier = "verified"
	// TierUnverified: we could not confirm with the provider, but the platform
	// independently observed auth-shaped failures. The card says so plainly.
	TierUnverified Tier = "unverified"
)

// Input is everything Determine needs, already gathered by the reconciler.
type Input struct {
	// CredType is the CredentialSource.Type: "static", "oauth", or "federated".
	CredType string

	// AgentOwned reports whether the dead credential belongs to the AGENT
	// itself (an AgentIdentity) rather than to a person. It changes who can act
	// on the card, and for one credential type it changes whether a card should
	// exist at all — see step 3.
	AgentOwned bool

	// RefreshRan reports whether an OAuth refresh was attempted; RefreshOK
	// whether it succeeded; RefreshErr the raw error text when it did not.
	RefreshRan bool
	RefreshOK  bool
	RefreshErr string

	// Probe is the VerifyCredential verdict against the current (possibly
	// just-refreshed) value, with ProbeDetail its human-readable detail.
	Probe       builtins.VerifyStatus
	ProbeDetail string

	// Corroborated reports whether the platform independently observed
	// auth-shaped failures on this origin — the runner's own
	// AgentSession.status.credentialAuthFailures entry for the request's
	// origin, recorded only after a real upstream failure matched the
	// provider's declared authFailure: shape and removed the moment a call
	// there succeeds. It is NEVER the agent's own assertion.
	//
	// It is consulted in exactly one branch below (a non-definitive probe).
	// A probe that says the credential is live beats it outright.
	Corroborated bool

	// ProviderTitle is the user-facing service name ("GitHub") for the reason
	// text. Falls back to "the provider" when empty.
	ProviderTitle string
}

// Outcome is Determine's verdict.
type Outcome struct {
	Tier          Tier
	Determination string
	// Reason is platform-authored, human-readable, and used in two places: the
	// agent's tool result and the card's verdict line. It is never empty.
	Reason string
}

// invalidGrant is the RFC 6749 error code meaning the refresh token itself is
// no longer honored. It is the one refresh failure that is evidence about the
// CREDENTIAL rather than about the network, which is why it alone is promoted
// to a definitive rejection.
const invalidGrant = "invalid_grant"

// ProbeWouldNotHelp reports whether Determine refuses on the credential's SHAPE
// alone, so a live probe's result cannot change the verdict. It is NOT the
// verdict (Determine still is) — it lets the reconciler skip an outbound call it
// would then ignore. A coherence test pins it against Determine: a predicate
// that drifted looser would suppress the probe for a credential whose verdict
// genuinely depends on it.
//
// A successful REFRESH also makes the probe irrelevant, but that is a fact about
// a run rather than the credential's shape; the reconciler skips on it
// separately.
func ProbeWouldNotHelp(in Input) bool {
	k, err := credkindregistry.Get(in.CredType)
	if err != nil {
		// Unknown type: assume a probe would not help rather than offering a
		// human a card for a credential shape nothing understands.
		return true
	}
	// Minted (federated today): nothing is stored, so there is nothing to
	// probe or replace. Agent-owned + NeedsRefresh (agent-owned oauth today):
	// the value is a bundle only the OAuth ceremony can mint, and the
	// ceremony a card could offer links the CLICKER's account, not the
	// agent's.
	return k.Minted() || (in.AgentOwned && k.NeedsRefresh())
}

// Determine applies the verdict table. The ordering matters and is the whole
// design: the steps that can resolve without a human run first.
func Determine(in Input) Outcome {
	title := in.ProviderTitle
	if title == "" {
		title = "the provider"
	}

	k, err := credkindregistry.Get(in.CredType)
	if err != nil {
		// Unknown type: Determine is deliberately pure (no client, no clock, no
		// logger to surface this to), so the refusal reason IS the surfaced
		// signal an operator has to find the wiring gap. Mirrors
		// ProbeWouldNotHelp's own fail-closed answer for the same input — a
		// card for a credential shape nothing understands is worse than
		// staying quiet — so the two predicates cannot drift apart on it.
		return Outcome{
			Tier:          TierNone,
			Determination: spiceboxv1alpha1.CredentialUpdateDeterminationNotUpdatable,
			Reason: fmt.Sprintf("This credential's type (%q) is not recognized by this build, so there is "+
				"nothing here we know how to walk a human through replacing. Report the failure instead of "+
				"asking again.", in.CredType),
		}
	}

	// Step 1 — a MINTED credential (federated today; any future minted kind,
	// e.g. githubApp, tomorrow) has no stored token to re-enter. Asking a human
	// to paste one would be asking for something that does not exist. The
	// reason text is deliberately type-neutral: it must read correctly for a
	// kind minted via an ID-JAG exchange, an App-installation-token exchange,
	// or anything registered later — never naming a specific minting mechanism
	// like "the user's enterprise identity" or "re-authenticate with the
	// identity provider", both of which are true of federated only.
	if k.Minted() {
		// Absent a ProviderTitle (no provider.ByID match), the hand-built
		// "the provider" fallback reads as broken grammar here ("This the
		// provider credential..."); the credential kind's own DisplayName is
		// the more specific, grammatical stand-in.
		label := title
		if in.ProviderTitle == "" {
			label = k.DisplayName()
		}
		return Outcome{
			Tier:          TierNone,
			Determination: spiceboxv1alpha1.CredentialUpdateDeterminationNotUpdatable,
			Reason: fmt.Sprintf("This %s credential is minted on demand; there is no stored token to replace. "+
				"If it is failing, an operator needs to fix how it is issued — pasting a new value would not help.",
				label),
		}
	}

	// Step 2 — never ask a human for something the machine already did.
	if in.RefreshRan && in.RefreshOK {
		return Outcome{
			Tier:          TierNone,
			Determination: spiceboxv1alpha1.CredentialUpdateDeterminationSelfHealed,
			Reason: fmt.Sprintf("The %s credential was expired but has now been refreshed automatically. "+
				"Retry your call.", title),
		}
	}

	// Step 3 — an agent's OWN credential of a type that NEEDS REFRESH (oauth
	// today) has no self-service replacement: the value is a multi-key bundle
	// only the OAuth ceremony can mint, and the ceremony behind the card links
	// the CLICKER's account, not the agent's. agentidentity.Resolve does
	// refuse it, but only at CLICK time — so without this step an admin opens
	// the card, is told it can't be replaced, and the request expires
	// announcing that nobody updated the credential.
	//
	// Ordering: AFTER step 2, because a refresh that self-heals needs no human;
	// BEFORE the invalid_grant step, because knowing the refresh token is dead
	// still does not make the bundle pasteable.
	//
	// A USER-owned oauth credential is untouched: its card offers the visitor the
	// OAuth ceremony for their own account, which genuinely does fix it.
	if in.AgentOwned && k.NeedsRefresh() {
		return Outcome{
			Tier:          TierNone,
			Determination: spiceboxv1alpha1.CredentialUpdateDeterminationNotUpdatable,
			Reason: fmt.Sprintf("This %s credential belongs to the agent itself and is issued by an OAuth flow, "+
				"so there is no value a human can paste to replace it — it has to be re-authorized by an operator. "+
				"Report the failure instead of asking again.", title),
		}
	}

	// Step 4 — a dead refresh token is a definitive rejection, and it is the
	// ONLY definitive signal available for an OAuth provider that declares no
	// verify probe (most of the catalog).
	if in.RefreshRan && !in.RefreshOK && strings.Contains(strings.ToLower(in.RefreshErr), invalidGrant) {
		return Outcome{
			Tier:          TierVerified,
			Determination: spiceboxv1alpha1.CredentialUpdateDeterminationRejectedRefreshDead,
			Reason: fmt.Sprintf("%s refused to refresh this connection (invalid_grant); it needs re-authorizing.",
				title),
		}
	}

	// Step 5 — the probe.
	//
	// unconfirmed is the verdict for every probe outcome that did not settle
	// the question: corroboration alone decides, and without it there is no
	// card. Named and shared by the arms below so they cannot drift apart on
	// what "we don't know" costs.
	unconfirmed := func() Outcome {
		if in.Corroborated {
			return Outcome{
				Tier:          TierUnverified,
				Determination: spiceboxv1alpha1.CredentialUpdateDeterminationRejectedUnverified,
				Reason: fmt.Sprintf("We could not confirm with %s. This agent's calls to %s have been failing "+
					"with authentication errors.", title, title),
			}
		}
		return Outcome{
			Tier:          TierNone,
			Determination: spiceboxv1alpha1.CredentialUpdateDeterminationUnverified,
			Reason: fmt.Sprintf("We could not confirm with %s whether this credential is still valid, and no "+
				"independent evidence of authentication failure was recorded. Report the failure you saw instead.",
				title),
		}
	}

	switch in.Probe {
	case builtins.VerifyValid:
		// Deliberate: a live probe beats an observed failure, corroborated or
		// not. An observed 403 next to a live token is precisely what a scope
		// problem looks like, and re-entering the same token cannot fix it.
		return Outcome{
			Tier:          TierNone,
			Determination: spiceboxv1alpha1.CredentialUpdateDeterminationCredentialLive,
			Reason: fmt.Sprintf("The %s credential still authenticates successfully, so it has not expired. "+
				"This is a permissions or scope problem, not an expired token — replacing it would not help. "+
				"Report what you were trying to do and which resource was refused.", title),
		}

	case builtins.VerifyRejected:
		// Reason embeds PROVIDER-written text (the probe detail's error body).
		// Safe only because TierVerified routes to phase Open, whose verdict line
		// is rendered to a HUMAN — never to Refused, where the same string would
		// come back to the agent as the platform's own explanation. The safety
		// rests on that tier→phase mapping, not on anything visible here: route
		// TierVerified elsewhere and this starts feeding provider-controlled text
		// into an agent-facing verdict.
		//
		// Only a 401 produces VerifyRejected — a 403 is VerifyForbidden (see
		// builtins.VerifyHTTPBearer) — so the credential is never declared dead on
		// a status a live token also returns.
		return Outcome{
			Tier:          TierVerified,
			Determination: spiceboxv1alpha1.CredentialUpdateDeterminationRejectedVerified,
			Reason:        in.ProbeDetail,
		}

	case builtins.VerifyForbidden:
		// A 403 is a statement about the REQUEST, not the credential: a live token
		// that is SSO-restricted, org-blocked, or missing a scope answers exactly
		// as a revoked one does. So it can never open a card unaided — that would
		// ask a human to replace a credential that works. It is equally not proof
		// of validity, so it must not SUPPRESS a card either: defer to independent
		// evidence, which for an HTTP origin means an observed 401 the agent
		// cannot provoke.
		//
		// Named explicitly rather than left to the default arm so an edit there
		// cannot quietly change what a 403 means here.
		return unconfirmed()

	case builtins.VerifyIndeterminate, builtins.VerifyUnsupported:
		return unconfirmed()

	default:
		// Exhaustiveness backstop: an unset status, or a verdict added to
		// VerifyStatus without a branch here. "Did not settle the question" is the
		// fail-closed reading — it withholds the card absent independent evidence
		// — so the next verdict added cannot fall open into an unauthorized card.
		return unconfirmed()
	}
}
