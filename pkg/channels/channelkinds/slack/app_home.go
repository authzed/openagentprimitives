// pkg/channels/channelkinds/slack/app_home.go
//
// app_home_opened handler — re-publishes the Home tab view on every open,
// per-user and per-app, with NO cache layer. Slack's per-app per-user view is
// already cached, so a second one would only add a place for staleness to
// surface, and views.publish is Tier 4 (~100/min) — ample for human-driven
// opens.
//
// Read at render time:
//   - The single AgentClass this listener's Channel is bound to
//     (l.deps.Channel.Spec.AgentClass). Each OAP agent is its own Slack app,
//     so this listener's Home tab is that one agent's — not a directory.
//   - The clicker's UserIdentity, for the agent's ✓/✗ linked-services state.
//     Best-effort: a missing one renders as "nothing linked yet".
//   - PortalLinkMinter, for the per-user /my/accounts deep-link. Unavailable
//     omits the Manage button rather than rendering it broken.
//
// Failures degrade visibly: a missing bound class logs and publishes the
// empty-state view — better than a blank Home or a retry loop.
package slack

import (
	"context"
	"fmt"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthrough"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughcatalog"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

// appHomePersonalizableClassLimit caps LookupPersonalizableClassRefs' answer.
// A rendering hint, not a security gate (see PersonalizableClassLookup), so
// this is sized generously against "how many classes has one user ever
// talked to", not tightly like an authorization check would be.
const appHomePersonalizableClassLimit = 200

// handleAppHomeOpened is invoked by the listener's events dispatcher
// when a user opens the bot's Home tab. Best-effort: a failure logs
// but does not interrupt the listener.
func (l *slackListener) handleAppHomeOpened(ctx context.Context, ev *slackevents.AppHomeOpenedEvent) {
	logger := log.FromContext(ctx).WithValues("event", "app_home_opened", "user", ev.User, "tab", ev.Tab)
	// Slack fires app_home_opened with tab="home" and (separately)
	// tab="messages" — only the home tab is interesting.
	if ev.Tab != "" && ev.Tab != "home" {
		return
	}
	in, err := l.buildHomeInputForUser(ctx, ev.User)
	if err != nil {
		logger.Info("app_home: build input failed; publishing empty-state", "err", err.Error())
		in = appHomeViewInput{} // empty-state placeholder still renders correctly
	}
	view := buildAppHomeView(in)
	req := slackapi.PublishViewContextRequest{UserID: ev.User, View: view}
	if _, err := l.api.PublishViewContext(ctx, req); err != nil {
		logger.Info("app_home: views.publish failed", "err", err.Error())
		return
	}
	logger.Info("app_home: published", "hasAgent", in.Agent != nil)
}

// buildHomeInputForUser assembles the appHomeViewInput for one user.
// All cross-resource lookups happen here so the view builder remains
// pure (testable without a live cluster).
//
// This Home tab belongs to ONE agent — the AgentClass this listener's Channel
// is bound to. Each OAP agent is a separate Slack app with its own Socket Mode
// connection, so the app_home_opened event that reached this listener names
// the agent whose tab the user opened. The view is scoped to that agent alone;
// it does NOT enumerate every Slack agent in the workspace.
func (l *slackListener) buildHomeInputForUser(ctx context.Context, slackUserID string) (appHomeViewInput, error) {
	c := l.deps.K8sClient
	if c == nil {
		return appHomeViewInput{}, fmt.Errorf("no k8s client")
	}
	// 1. Identify THIS listener's agent from its bound Channel. A listener with
	//    no bound Channel (or an unbound one) has no agent to show — return the
	//    empty state rather than guessing.
	if l.deps.Channel == nil || l.deps.Channel.Spec.AgentClass == "" {
		return appHomeViewInput{}, fmt.Errorf("listener has no bound AgentClass")
	}
	classNS := l.deps.Channel.Namespace
	className := l.deps.Channel.Spec.AgentClass

	// 2. Resolve the clicker's canonical subject so we can look up
	//    their UserIdentity for per-agent linked-services rendering.
	canonical, err := l.resolveCanonicalForSlackUser(ctx, slackUserID)
	if err != nil {
		// Don't fail the whole render — fall through with empty
		// canonical, which produces a "nothing linked yet" line for
		// a passthrough agent. Better than a blank Home.
		log.FromContext(ctx).V(1).Info("app_home: canonical resolution failed; linked-services lines will show 'nothing linked'",
			"slackUser", slackUserID, "err", err.Error())
	}
	var ui *spiceboxv1alpha1.UserIdentity
	if canonical != "" {
		var fetched spiceboxv1alpha1.UserIdentity
		if err := c.Get(ctx, client.ObjectKey{Name: useridentity.NameForSubject(identity.Subject(canonical))}, &fetched); err == nil {
			ui = &fetched
		}
	}
	userCredNames := map[string]struct{}{}
	if ui != nil {
		for _, cred := range ui.Spec.Credentials {
			userCredNames[cred.Name] = struct{}{}
		}
	}

	// 2a. Derive the bare canonical (no "user:" prefix) for the preferences
	// section: GetPreferencesFirstParty and LookupPersonalizableClassRefs
	// both key on the bare id. canonical is always a well-formed "user:<id>"
	// Subject when non-empty (resolveCanonicalForSlackUser's every success
	// path returns a Principal.Subject()), so CanonicalUserID() only fails
	// here for the empty string, which ObjectType() reports as "" —
	// IsZero() on the zero value then makes every check below a no-op, which
	// is exactly "skip preferences enumeration" for a guest whose canonical
	// resolution failed.
	var bareCanonical identity.CanonicalUserID
	if canonical != "" {
		if cid, err := identity.Subject(canonical).CanonicalUserID(); err == nil {
			bareCanonical = cid
		} else {
			log.FromContext(ctx).V(1).Info("app_home: canonical subject has no bare user id; preferences sections omitted",
				"slackUser", slackUserID, "err", err.Error())
		}
	}

	// 2b. Enumerate the classes this user has INTERACTED WITH (SpiceDB
	// agentclass#can_personalize), so the page builds a preferences section
	// only when the user has actually talked to THIS agent — never merely
	// because it is installed. Failure or an absent lookup degrades to an
	// empty set: the page still renders (manage-connections as before), just
	// with no preferences section.
	personalizableClassRefs := map[string]struct{}{}
	if !bareCanonical.IsZero() && l.personalizableClasses != nil {
		refs, err := l.personalizableClasses.LookupPersonalizableClassRefs(ctx, bareCanonical, appHomePersonalizableClassLimit, false)
		if err != nil {
			log.FromContext(ctx).Info("app_home: LookupPersonalizableClassRefs failed; preferences sections omitted for every class",
				"slackUser", slackUserID, "err", err.Error())
		} else {
			for _, ref := range refs {
				personalizableClassRefs[ref] = struct{}{}
			}
		}
	}

	// 3. Mint the portal link for the Manage-connections button. The portal
	//    page lists all credentials cluster-wide; per-agent filtering would be
	//    a nice polish but the "launch into /my/accounts" path was chosen
	//    deliberately.
	portalURL := ""
	if l.deps.PortalLinkMinter != nil && canonical != "" {
		// canonical is a pre-formed "user:<id>" string from resolveCanonicalForSlackUser;
		// use RawSubject so the string passes through verbatim. EmailVerified=false
		// is correct here — provenance of the string is unknown at this call site.
		if u, err := l.deps.PortalLinkMinter.MintPortalLink(ctx, identity.RawSubject(canonical)); err == nil {
			portalURL = u
		} else {
			log.FromContext(ctx).Info("app_home: portal-link mint failed; omitting Manage button",
				"err", err.Error())
		}
	}

	// 4. Load THIS listener's AgentClass and build its single page. A missing
	//    class (deleted, or the Channel binds a name that never existed) is the
	//    empty state — better than a blank tab or a stale card.
	externalBaseURL := ""
	if l.deps.ExternalBaseURL != nil {
		externalBaseURL = l.deps.ExternalBaseURL()
	}
	var ac spiceboxv1alpha1.AgentClass
	if err := c.Get(ctx, client.ObjectKey{Namespace: classNS, Name: className}, &ac); err != nil {
		log.FromContext(ctx).Info("app_home: bound AgentClass not found; publishing empty state",
			"namespace", classNS, "class", className, "err", err.Error())
		return appHomeViewInput{ExternalBaseURL: externalBaseURL}, nil
	}

	card := appHomeAgent{
		Name:         ac.Name,
		DisplayName:  ac.Spec.DisplayName,
		Description:  ac.Spec.Description,
		IdentityMode: ac.Spec.IdentityMode,
	}
	if ac.Spec.IdentityMode == spiceboxv1alpha1.IdentityModeUserPassthrough {
		// Required services: the union of provider labels across the
		// AgentClass's MCPServers, intersected with the user's linked
		// credentials gives the ✓ half.
		classCredNames := credentialNamesForClass(ctx, c, &ac)
		allLabels, err := passthroughcatalog.ResolveLinkedServiceLabels(ctx, c, classCredNames, nil)
		if err != nil {
			// The page degrades to a blank connections section. Say why in the
			// log: the blank and "this agent needs nothing" look identical on
			// the Home tab, so this is the only trace of the failure.
			log.FromContext(ctx).Info("app_home: resolving required service labels failed; page omits them",
				"namespace", classNS, "class", className, "err", err.Error())
		} else {
			card.AllRequiredServiceLabels = allLabels
		}
		// Linked subset: intersect the user's credentials with the class's,
		// then resolve each to its provider label. The resolver returns
		// (credential, label) PAIRS because it drops what it cannot resolve —
		// a toolkit-derived credential has no MCPServer, so this list is
		// routinely shorter than the intersection that went in — and the page
		// needs both halves of a surviving entry to compose its icon URL.
		var linkedNames []string
		for _, cn := range classCredNames {
			if _, ok := userCredNames[cn]; ok {
				linkedNames = append(linkedNames, cn)
			}
		}
		if len(linkedNames) > 0 {
			linked, err := passthroughcatalog.ResolveLinkedServices(ctx, c, linkedNames, nil)
			if err != nil {
				// The user HAS linked these credentials; failing here renders
				// the page as though they had not. Log it — an unexplained
				// "not connected" is the complaint this surface generates most.
				log.FromContext(ctx).Info("app_home: resolving linked service labels failed; page shows them as unlinked",
					"namespace", classNS, "class", className, "err", err.Error())
			} else {
				card.LinkedServices = linked
			}
		}
		card.PortalLinkURL = portalURL
	}

	// Preferences section: only when the user has INTERACTED WITH this class
	// (in personalizableClassRefs) AND it DECLARES at least one userPreferences
	// key. Outside either condition the page shows no section at all — the two
	// conditions are what "eligible for personalization" means here, not merely
	// "this is the agent's tab".
	classRef := classNS + "/" + className
	if _, interacted := personalizableClassRefs[classRef]; interacted && len(ac.Spec.UserPreferences) > 0 {
		card.ClassRef = classRef
		switch {
		case l.preferences == nil:
			// No preferences client configured on this listener — degrade
			// visibly rather than silently omit a section the user is otherwise
			// entitled to see.
			card.PreferencesLoadFailed = true
			log.FromContext(ctx).Info("app_home: no preferences client configured; page shows couldn't-load state",
				"namespace", classNS, "class", className)
		default:
			snap, err := l.preferences.GetPreferencesFirstParty(ctx, classNS, className, bareCanonical.String())
			if err != nil {
				// A fetch failure must not take down the tab — show the user a
				// "couldn't load" notice rather than silence (which reads
				// identically to "this agent has no preferences").
				card.PreferencesLoadFailed = true
				log.FromContext(ctx).Info("app_home: GetPreferencesFirstParty failed; page shows couldn't-load state",
					"namespace", classNS, "class", className, "err", err.Error())
			} else {
				card.Preferences = snap.Snapshot.Keys
			}
		}
	}

	return appHomeViewInput{Agent: &card, ExternalBaseURL: externalBaseURL}, nil
}

// credentialNamesForClass returns the full credential-name set the
// AgentClass would pull at runtime — union across MCPServer refs
// (Spec.Auth.Credential + credentialRemap overrides) AND ToolBundle
// refs (each toolspec's sensitive env vars).
//
// Delegates to the shared resolver at pkg/platform/identity/passthrough.Required
// so the Home view computes EXACTLY the same set the passthrough gate
// uses to park sessions, the standing portal suggests, and the
// channelsd DM publishes via SUI.Status.MissingCredentials.
//
// Read-only surface, so it takes the BEST-EFFORT resolver: a single
// dangling ref (a deleted MCPServer, a typo'd toolkit) is logged and
// skipped, and every credential that did resolve is still listed. The
// strict resolver would have returned nothing at all, blanking the
// card's required-services line — which reads to the user as "this
// agent needs nothing", indistinguishable from the truth.
func credentialNamesForClass(ctx context.Context, c client.Client, ac *spiceboxv1alpha1.AgentClass) []string {
	if ac == nil {
		return nil
	}
	return passthrough.RequiredBestEffort(ctx, c, ac,
		log.FromContext(ctx).WithValues("surface", "app_home", "agentClass", ac.Name, "namespace", ac.Namespace))
}

// resolveCanonicalForSlackUser maps a Slack user_id to the
// canonical SpiceDB subject identityd writes UserIdentity objects
// under. Uses the listener's email cache where possible (already
// populated during inbound message handling); falls back to a
// users.info lookup. A failure returns empty + error — callers
// degrade rather than fail.
func (l *slackListener) resolveCanonicalForSlackUser(ctx context.Context, slackUserID string) (string, error) {
	if slackUserID == "" {
		return "", fmt.Errorf("empty slackUserID")
	}
	// Cache hit? The listener already canonicalizes for every
	// inbound message; opening the Home is the user telling us
	// they're here, but we may not have processed any of their
	// messages yet.
	if l.idents != nil {
		if rec, ok := l.idents.Get(slackUserID); ok && rec.Email != "" {
			subj, err := identity.FromExternal(
				identity.KindSlack,
				identity.TeamScope(l.installedTeamID),
				identity.RawExternalID(slackUserID),
				identity.Email(rec.Email),
			).Subject()
			// identity boundary: resolveCanonicalForSlackUser returns (string, error) by contract; the Subject is serialized to a string here.
			return subj.String(), err
		}
	}
	if l.api == nil {
		return "", fmt.Errorf("no slack api client")
	}
	u, err := l.api.GetUserInfoContext(ctx, slackUserID)
	if err != nil {
		return "", fmt.Errorf("users.info: %w", err)
	}
	// Only trust the profile email for a full member of the bot's own installed
	// workspace. The canonical SpiceDB subject is derived from the email, so a
	// foreign-workspace (Slack Connect) user, guest, bot, or deleted account —
	// whose self-attested email is low-trust — must NOT be canonicalized by
	// email, or they could spoof another user's identity (e.g. set their profile
	// email to the session owner's and pass CheckInteract). This mirrors
	// resolveIdentity's emailTrusted() gate; the cache-hit branch above is
	// already safe because l.idents only ever stores a trusted (or empty) email.
	// An empty or untrusted email keys on the unforgeable synthetic
	// slack:<team>:<user> subject instead.
	if u.Profile.Email == "" || !emailTrusted(u, l.installedTeamID) {
		subj, err := identity.FromExternal(
			identity.KindSlack,
			identity.TeamScope(l.installedTeamID),
			identity.RawExternalID(slackUserID),
			"",
		).AllowSynthetic().Subject()
		// identity boundary: resolveCanonicalForSlackUser returns (string, error) by contract; the Subject is serialized to a string here.
		return subj.String(), err
	}
	subj, err := identity.FromExternal(
		identity.KindSlack,
		identity.TeamScope(l.installedTeamID),
		identity.RawExternalID(slackUserID),
		identity.Email(u.Profile.Email),
	).Subject()
	// identity boundary: resolveCanonicalForSlackUser returns (string, error) by contract; the Subject is serialized to a string here.
	return subj.String(), err
}

// Compile-time guard so the unused-import linter doesn't flag the
// channelkinds import (used by Deps.PortalLinkMinter via the
// embedded l.deps).
var _ = channelkinds.PortalLinkMinter(nil)

// slackapi alias kept to satisfy goimports — referenced via the
// view types used in handleAppHomeOpened above.
var _ = slackapi.VTHomeTab
