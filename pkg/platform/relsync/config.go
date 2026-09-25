package relsync

import (
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
)

// ExistingConfig is what is already configured for one kind, used to
// prefill a re-run of the CLI wizard. The zero value means "nothing
// configured yet" — there is no RelationshipSource CR for this kind, or
// there is one but the field in question was left unset.
//
// Config carries spec.config UNINTERPRETED: the kind owns its own shape, and
// nothing between the CR and the kind parses it. It is not, however,
// byte-identical to the stored value the way SourceParams.Config is.
// SourceParams.Config is populated straight from the typed CR's
// apiextensionsv1.JSON.Raw on the controller's read path; a CLI caller
// building an ExistingConfig instead reads through client-go's
// dynamic.Interface, which has already decoded the wire response into
// map[string]interface{} before this value is re-marshalled — so an object
// whose keys were not already alphabetical can come back reordered. That is
// safe here: Configurable.ConfigScreens consumes this value by parsing it,
// never by comparing bytes, and a wizard re-run applies
// Configurable.BuildConfig's freshly-computed output as the new spec.config
// — never this value echoed back — so no SSA idempotency guarantee depends
// on byte identity.
type ExistingConfig struct {
	// Name is the CR's name, empty when none exists.
	Name string
	// Endpoint is spec.baseURL.
	Endpoint string
	// Config is spec.config, semantically unchanged but not necessarily
	// byte-identical to the stored value — see the type doc above.
	Config json.RawMessage
	// Identity is spec.auth.agentIdentity.
	Identity string
	// Credential is spec.auth.credential.
	Credential string
}

// ConfigScreen is one question the CLI wizard asks. Key is the tui.State
// key its answer lands under; Prompt and Help are shown to the human;
// Default is the prefilled value (from a prior ExistingConfig, or empty on
// a first run). A kind that wants a comma-separated answer (github's org
// list, for instance) says so in Help and parses the delimited string back
// out in BuildConfig — the wizard renders every screen as a plain text
// input regardless.
type ConfigScreen struct {
	Key, Prompt, Help, Default string
}

// Configurable is the optional interface a Kind implements to be
// configurable from the CLI beyond credential and endpoint. A kind that
// does not implement it can still be configured — it simply asks nothing
// more: Slack syncs the whole workspace its token reaches, so it has
// nothing to ask.
//
// The interface is optional, not required with an empty default, on
// purpose: a kind with genuinely nothing to ask and a kind that forgot to
// implement this would otherwise look identical. Making "asks nothing" a
// distinct, deliberate choice (implement the interface, return no screens)
// keeps the two cases distinguishable.
//
// relsync.All() already enumerates every registered Kind; a caller wanting
// to know whether a kind is configurable type-asserts to this interface,
// the way channelkinds.SchemaContributor is asserted — there is no second
// registry and no registration hook here.
type Configurable interface {
	// ConfigScreens returns the screens this kind needs answered, in
	// order, prefilled from prior. Returning none is legitimate — it means
	// this kind has nothing beyond credential and endpoint to ask.
	ConfigScreens(prior ExistingConfig) []ConfigScreen
	// BuildConfig turns the answered screens into spec.config bytes, or
	// nil when this kind needs none.
	//
	// It must be a pure function of its answers: the same answers must
	// always produce byte-identical output. The wizard applies the result
	// as spec.config verbatim via server-side apply, and CLAUDE.md's
	// server-side-apply rule makes that a hard requirement rather than a
	// nicety — a volatile value in an applied field (a timestamp, a
	// randomly-ordered map, anything not derived solely from answers)
	// means a byte-identical re-run of the wizard no longer produces a
	// byte-identical apply, so a re-apply stops being an SSA no-op: field
	// ownership churns and every watcher re-reconciles on every apply.
	BuildConfig(answers map[string]string) (json.RawMessage, error)
	// NeedsEndpoint reports whether spec.baseURL is required for this
	// kind. GHES and the 1Password bridge are customer-hosted; Slack is
	// not.
	NeedsEndpoint() bool
}

// CredentialSetup is the optional interface a Kind implements to say how an
// operator who does not yet HAVE a credential for it gets one. Configurable
// says what to ask once a credential is in hand; this says where the
// credential itself comes from.
//
// It names a flow rather than returning one because relsync is imported by
// the controller that runs a sync, and the setup flows live in the CLI's
// credential-acquisition registry (pkg/platform/identity/setup/builtins).
// Returning a builtins.Flow here would drag that registry — and the terminal
// UI it describes screens for — into every binary that merely syncs a
// directory. A name is resolved by the one caller that can present screens.
//
// Optional for the same reason Configurable is: a kind whose credential
// cannot be obtained by any flow we ship simply does not implement this, and
// the CLI then offers only credentials that already exist rather than
// pretending it can make one. A kind that DOES implement it must return a
// non-empty flow name — an empty one is a contract violation the caller
// fails closed on rather than silently treating as "not implemented".
type CredentialSetup interface {
	// SetupFlow names the builtins.Flow that guides an operator through
	// obtaining this kind's credential, and the intent string that flow uses to
	// tailor its guidance.
	//
	// intent is prose the flow reads, not a key it switches on: github-pat
	// narrows its suggested scopes by looking for "read"/"write" in it, so an
	// intent that names read-only access is what keeps a directory sync's
	// token from being recommended write scopes it never uses.
	SetupFlow() (flow, intent string)

	// SetupScopes returns the permission lines that flow should print, one per
	// line, in place of whatever it would otherwise recommend from intent
	// alone. Nil keeps the flow's own recommendation.
	//
	// It exists because intent is a blunt instrument and a flow's own guess is
	// about the provider, not about this kind's calls. github-pat's guess is
	// derived from the REPOSITORY permissions its usual callers want; a
	// directory sync reads an ORGANIZATION's members and teams, which none of
	// those lines grants — and the failure is silent, because the flow's
	// verification probe is GET /user, which any token answers. The token
	// stores clean and the sync 403s forever, after the operator has already
	// waited on org-admin approval for the wrong permissions.
	//
	// A kind returning lines here owes them a test against what it ACTUALLY
	// calls. The lines are printed to a human about to grant real access, so a
	// list nobody checks is worse than no list: it is a wrong answer in an
	// authoritative voice.
	SetupScopes() []string
}

// ScopeLabeler is the optional interface a Kind implements to say HOW its
// scope ids resolve to human names — or, by not implementing it, that they do
// not.
//
// A forge's key is rarely a name. GitHub keys a repository scope by a numeric
// forge id, so the admin console's Directory panel showed a wall of
// `github_repo:1005857813` — stable, correct, and meaningless to the person
// reading it. The mapping to a name was already in SpiceDB (the sync writes a
// URL-keyed bridge tuple alongside every repo scope); nothing read it.
//
// Optional for exactly the reason Configurable is, and the distinction is the
// same one: Slack fetches a channel's name during a sync and keeps only the
// id, and 1Password does the same for a group, so for those kinds there is
// genuinely nothing stored to read and raw ids are the honest answer. Making
// "resolves to nothing" a deliberate non-implementation rather than an empty
// return keeps a kind that has no names distinguishable from a kind that
// forgot — and a kind that later gains a bridge implements this and nothing
// else changes.
//
// The consumer type-asserts rather than consulting a second registry, exactly
// as Configurable and CredentialSetup are consulted. The console's panel
// itself never learns which kind it is rendering: what it gets back is a
// resolved name or nothing, so a kind-specific branch never enters it.
//
// It answers in spicedb.ScopeLabelBridge because a bridge IS a SpiceDB read —
// definition, relation, and how to decode the object id — and relsync.Kind
// already speaks that package (Source() returns a relsource.Source). A
// relsync-local mirror would be the same struct under a second name plus a
// conversion in every consumer.
type ScopeLabeler interface {
	// ScopeLabelBridges returns the bridges a reader joins this kind's scope
	// ids against, in preference order. Each one names an
	// ALREADY-WRITTEN relation: this is a declaration of where to READ, and
	// implementing it neither writes a tuple nor changes what the sync writes.
	//
	// Returning none is a contract violation, not an empty answer — a kind
	// with nothing to resolve does not implement this interface at all. A
	// reader treats an empty return the same as a non-implementation, so the
	// failure is inert rather than fatal, but there is no reason to write one.
	ScopeLabelBridges() []spicedb.ScopeLabelBridge
}
