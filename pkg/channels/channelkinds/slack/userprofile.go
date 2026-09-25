// pkg/channels/channelkinds/slack/userprofile.go
//
// Slack implementation of channelkinds.UserProfileProvider. Deliberately thin:
// it reuses the clientFactory + identity cache lookup.go already owns rather
// than opening a second path to the Slack API, and it applies NO allowlist —
// that is userprofile.Filter's single job.
package slack

import (
	"context"
	"strings"

	slackapi "github.com/slack-go/slack"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/agent/userprofile"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

var _ channelkinds.UserProfileProvider = (*Kind)(nil)

// authTester — the optional auth.test capability of a slackClient — is
// already declared in wizard.go with this exact signature (the wizard uses it
// the same way: an injectable narrow interface discovered/constructed rather
// than widening slackClient). Reused here rather than redeclared so the
// package has exactly one notion of "a client that can answer auth.test",
// discovered by type-assertion so the shared slackClient interface —
// implemented by fakeslack and several test doubles — does not have to widen.

// ProfileFields implements channelkinds.UserProfileProvider. Slack's
// users.info payload maps onto every kind-neutral field today.
func (k *Kind) ProfileFields() []userprofile.Field { return userprofile.AllFields() }

// FetchProfileByEmail implements channelkinds.UserProfileProvider by calling
// cli.GetUserByEmailContext (users.lookupByEmail) directly — not the
// LRU-wrapped lookupByEmail in lookup.go, which only returns a user-id and
// display name and so cannot serve a full profile.
//
// The emailTrusted gate is the same one resolveIdentity applies before
// trusting a profile email. It is if anything MORE important here: a
// foreign-workspace or guest account's self-set title is precisely the text an
// attacker controls most cheaply, and it would be arriving in the agent's
// context on every turn.
func (k *Kind) FetchProfileByEmail(
	ctx context.Context, deps channelkinds.LookupDeps, email string,
) (userprofile.Profile, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return userprofile.Profile{}, channelkinds.ErrProfileNotFound
	}
	k.resolverInit()
	cli := k.clientFactory(deps)
	if cli == nil {
		return userprofile.Profile{}, errNoAPIClient
	}
	teamID := k.resolveInstalledTeamID(ctx, cli)
	if teamID == "" {
		// Without knowing which workspace is ours we cannot tell a member from
		// a foreign-workspace account, and emailTrusted would reject anyway.
		// Fail closed rather than trusting an unverified profile.
		return userprofile.Profile{}, channelkinds.ErrProfileNotFound
	}
	u, err := cli.GetUserByEmailContext(ctx, email)
	if err != nil {
		if isUsersNotFound(err) {
			return userprofile.Profile{}, channelkinds.ErrProfileNotFound
		}
		return userprofile.Profile{}, err
	}
	if u == nil || !emailTrusted(u, teamID) {
		return userprofile.Profile{}, channelkinds.ErrProfileNotFound
	}
	return profileFromSlackUser(u), nil
}

// resolveInstalledTeamID returns the team ID of the workspace this bot token
// belongs to, resolved via auth.test and cached ONLY on success. Returns ""
// when the client cannot answer (no authTester, or the call failed) —
// callers must treat that as "cannot establish trust" and fail closed. A
// failure is deliberately NOT cached: an auth.test error is far more likely
// to be transient (rate limit, network blip) than permanent, and caching it
// would silently disable the profile capability for the rest of the
// process's life with no way to recover short of a restart.
//
// This is the SAME source the listener uses (listener.go's Start), so the whole
// process has one notion of "our workspace". The runner has no listener to
// inherit it from, hence resolving it here.
// cli is `any` because the callers hold different client interfaces — the
// profile path a slackClient, the history path a historyClient — and this only
// ever needs the authTester half, which it type-asserts for anyway.
func (k *Kind) resolveInstalledTeamID(ctx context.Context, cli any) string {
	k.installedTeamMu.Lock()
	defer k.installedTeamMu.Unlock()
	if k.installedTeamID != "" {
		return k.installedTeamID
	}
	at, ok := cli.(authTester)
	if !ok {
		return ""
	}
	resp, err := at.AuthTestContext(ctx)
	if err != nil {
		log.FromContext(ctx).Info("slack: auth.test failed; no profile this turn", "err", err.Error())
		return "" // NOT cached — retried next turn
	}
	if resp != nil {
		k.installedTeamID = resp.TeamID
	}
	return k.installedTeamID
}

// profileFromSlackUser maps slack-go's User onto the kind-neutral Profile.
// Pure and total: no I/O, no error path, zero fields for whatever Slack left
// empty.
func profileFromSlackUser(u *slackapi.User) userprofile.Profile {
	if u == nil {
		return userprofile.Profile{}
	}
	return userprofile.Profile{
		DisplayName:   preferredDisplayName(u),
		RealName:      strings.TrimSpace(u.Profile.RealName),
		Email:         u.Profile.Email,
		Title:         u.Profile.Title,
		Pronouns:      u.Profile.Pronouns,
		Timezone:      u.TZ,
		TimezoneLabel: u.TZLabel,
		Locale:        u.Locale,
		StatusText:    u.Profile.StatusText,
		StartDate:     u.Profile.StartDate,
		AccountType:   accountTypeOf(u),
		Phone:         u.Profile.Phone,
	}
}

// accountTypeOf renders a coarse standing label. Descriptive only — nothing in
// this codebase may branch on it for authorization.
func accountTypeOf(u *slackapi.User) string {
	switch {
	case u.IsRestricted || u.IsUltraRestricted:
		return "guest"
	case u.IsOwner || u.IsPrimaryOwner:
		return "owner"
	case u.IsAdmin:
		return "admin"
	default:
		return "member"
	}
}
