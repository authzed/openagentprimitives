package github

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

var _ relsync.Configurable = (*SyncKind)(nil)

// githubOrgsScreenKey is the tui.State key the single ConfigScreens screen
// answers land under, and the same key BuildConfig reads back out of
// answers.
const githubOrgsScreenKey = "orgs"

// ConfigScreens asks for the explicit org list to sync — githubConfig's own
// doc is why there is no safe default: the synced set is the manifest's,
// never a credential's reach. Prefilled from prior.Config (best-effort: a
// prior value that fails to parse just shows no default, since this method
// has no error return to refuse the wizard with) so a re-run shows what is
// already configured.
func (k *SyncKind) ConfigScreens(prior relsync.ExistingConfig) []relsync.ConfigScreen {
	var def string
	if len(prior.Config) > 0 {
		if cfg, err := parseGithubConfig(prior.Config); err == nil {
			def = strings.Join(cfg.Orgs, ", ")
		}
	}
	return []relsync.ConfigScreen{
		{
			Key:     githubOrgsScreenKey,
			Prompt:  "GitHub organizations to sync",
			Help:    "Comma-separated list of GitHub organization logins. The synced set is exactly this list — a credential that can see more orgs does not widen it.",
			Default: def,
		},
	}
}

// errGitHubOrgsRequired is BuildConfig's refusal when the answered "orgs"
// screen has nothing usable in it — refused here, at the CLI seam, rather
// than only later by parseGithubConfig at sync time against a CR already
// written with a condition nobody is watching (errNoOrgsConfigured's own
// doc).
var errGitHubOrgsRequired = errors.New(`github: "orgs" must name at least one organization`)

// BuildConfig parses the comma-separated "orgs" answer into the same
// githubConfig{Orgs []string} shape parseGithubConfig reads — reusing that
// type rather than a parallel one, so the writer here and the reader there
// cannot drift. Each entry is trimmed; empty entries (a trailing comma, a
// blank answer) are dropped; an org list that survives trimming down to
// nothing is refused rather than written as an empty spec.config.Orgs.
//
// Deterministic by construction: it derives solely from the "orgs" string,
// split and trimmed in order, never a map range — the same input always
// produces the same byte output, which relsync.Configurable.BuildConfig's
// own doc requires for the wizard's server-side apply to stay idempotent.
func (k *SyncKind) BuildConfig(answers map[string]string) (json.RawMessage, error) {
	var orgs []string
	for _, raw := range strings.Split(answers[githubOrgsScreenKey], ",") {
		org := strings.TrimSpace(raw)
		if org == "" {
			continue
		}
		orgs = append(orgs, org)
	}
	if len(orgs) == 0 {
		return nil, errGitHubOrgsRequired
	}
	return json.Marshal(githubConfig{Orgs: orgs})
}

// NeedsEndpoint reports false: github.com needs no endpoint, and a GHES
// operator supplies one through the optional endpoint screen the wizard
// offers every kind — the controller's existing credhost.Check gates that
// host once it lands on spec.baseURL. Returning true here would make
// github.com itself unconfigurable without a URL.
func (k *SyncKind) NeedsEndpoint() bool {
	return false
}

var _ relsync.CredentialSetup = (*SyncKind)(nil)

const (
	// githubSetupFlowName is the builtins.Flow that mints this kind's
	// credential: the same fine-grained-PAT flow `oap identity setup` uses.
	// There is no directory-sync-specific GitHub credential — a PAT that can
	// read an org's members is a PAT — so a second flow would be the same
	// browser page with a different name on it.
	githubSetupFlowName = "github-pat"

	// githubSetupIntent is the prose that flow narrows its suggested scopes
	// from. It says "read" and never "write" or "create" on purpose: the flow
	// widens its recommendation to contents/pull_requests write on either of
	// those words, and this sync only ever reads org membership.
	githubSetupIntent = "read-only access to organization membership for directory sync"
)

// SetupFlow names the flow an operator with no GitHub credential yet is
// walked through, and the intent that keeps its scope recommendation
// read-only.
func (k *SyncKind) SetupFlow() (flow, intent string) {
	return githubSetupFlowName, githubSetupIntent
}

// githubSetupScopes are the permission lines the setup flow prints, in place
// of github-pat's own recommendation.
//
// Fine-grained permission names, not classic OAuth scopes, because that is
// what github-pat mints: on a fine-grained token "read:org" does not exist,
// and the equivalent is an Organization permission that must be granted with
// the ORGANIZATION as the token's resource owner. Overriding the flow's guess
// is not a preference — every line github-pat would otherwise print
// (contents, pull_requests, metadata) is a REPOSITORY permission, and none of
// them lets this sync read /orgs/{org}/members or /orgs/{org}/teams at all.
//
// The pair of permissions is this kind's own FeatureSupport declaration for
// channelfeatures.DirectorySync (kind.go), restated in the vocabulary a
// fine-grained token's UI uses. That declaration is pinned to every endpoint
// ListScopes and FetchScope really call by
// TestDirectorySyncFeatureDeclaresTheScopesItsCallsNeed
// (relsync_scopes_test.go), and this restatement is pinned to that
// declaration by TestSetupScopesNameEveryDeclaredDirectorySyncScope — so a
// new endpoint that needs a new scope fails the first test, and a scope added
// there without a line here fails the second. Neither can drift into a
// recommendation that sends an operator to request the wrong access.
var githubSetupScopes = []string{
	"Resource owner: the organization, not you",
	"Organization permissions",
	"  Members: read",
	"Repository permissions",
	"  Metadata: read",
}

// SetupScopes returns the fine-grained permissions this sync's calls need.
// See githubSetupScopes for why the flow's own recommendation is replaced
// rather than accepted.
func (k *SyncKind) SetupScopes() []string { return githubSetupScopes }
