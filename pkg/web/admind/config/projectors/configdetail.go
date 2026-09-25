package projectors

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

// This file carries the curated DetailProjectors for the resources whose
// sections are a handful of key spec/status fields plus (where useful) a text
// or list tab: skills, sources, channels, identities, users, providers. Each
// dispatches namespaced-vs-cluster the same way its list Projector lists both.

// ---- skills (Skill / ClusterSkill) ----------------------------------------

type skillsDetailProjector struct{}

func (skillsDetailProjector) Resource() string { return "skills" }

func (skillsDetailProjector) Detail(ctx context.Context, c client.Client, ns, name string) (*config.ResourceDetail, error) {
	if ns != "" {
		var s spiceboxv1alpha1.Skill
		if found, err := getObj(ctx, c, client.ObjectKey{Namespace: ns, Name: name}, &s); err != nil || !found {
			return nil, err
		}
		return skillDetail(s.Name, s.Namespace, "namespaced", "skill", &s.Spec, &s.Status), nil
	}
	var cs spiceboxv1alpha1.ClusterSkill
	if found, err := getObj(ctx, c, client.ObjectKey{Name: name}, &cs); err != nil || !found {
		return nil, err
	}
	return skillDetail(cs.Name, "", "cluster", "clusterskill", &cs.Spec, &cs.Status), nil
}

func skillDetail(name, ns, scope, editKind string, spec *spiceboxv1alpha1.SkillSpec, status *spiceboxv1alpha1.SkillStatus) *config.ResourceDetail {
	st, reason := projectStatus(status.Conditions, spiceboxv1alpha1.SkillConditionValid, "Valid")
	delivery := "instruction"
	if spec.Bundle != nil {
		delivery = "executable"
	}
	d := &config.ResourceDetail{
		Name:         name,
		Namespace:    ns,
		Scope:        scope,
		Status:       st,
		StatusReason: reason,
		Description:  spec.Description,
		ManageCmd:    editCmd(editKind, name, ns),
	}
	d.Sections = appendSection(d.Sections, fieldsSection("overview", "Overview",
		fld("Canonical name", spec.CanonicalName),
		fld("Display name", spec.DisplayName),
		fld("Delivery", delivery),
		fld("Pin", skillPinStrength(status)),
		fld("License", spec.Frontmatter.License),
		fld("Compatibility", spec.Frontmatter.Compatibility),
	))
	if src := spec.Source; src != nil {
		sourceID := ""
		if src.SourceName != "" && ns != "" {
			sourceID = ns + "/" + src.SourceName
		} else if src.SourceName != "" {
			sourceID = src.SourceName
		}
		d.Sections = appendSection(d.Sections, fieldsSection("source", "Source",
			fld("Repo", src.RepoLocator),
			fld("Subpath", src.Subpath),
			fld("Ref", src.Ref),
			fld("Resolved SHA", shortSHA(src.ResolvedSHA)),
			fldLink("Source CR", src.SourceName, "source", sourceID),
		))
	}
	d.Sections = appendSection(d.Sections, textSection("body", "SKILL.md", spec.Body))
	return d
}

// ---- sources (SkillSource / ClusterSkillSource) ---------------------------

type sourcesDetailProjector struct{}

func (sourcesDetailProjector) Resource() string { return "sources" }

func (sourcesDetailProjector) Detail(ctx context.Context, c client.Client, ns, name string) (*config.ResourceDetail, error) {
	if ns != "" {
		var s spiceboxv1alpha1.SkillSource
		if found, err := getObj(ctx, c, client.ObjectKey{Namespace: ns, Name: name}, &s); err != nil || !found {
			return nil, err
		}
		skills, err := ownedSkillItems(ctx, c, s.Namespace, s.Name, false)
		if err != nil {
			return nil, err
		}
		return sourceDetail(s.Name, s.Namespace, "namespaced", "skillsource",
			s.Spec.RepoURL, s.Spec.Ref, s.Spec.Subpath, &s.Status, skills), nil
	}
	var cs spiceboxv1alpha1.ClusterSkillSource
	if found, err := getObj(ctx, c, client.ObjectKey{Name: name}, &cs); err != nil || !found {
		return nil, err
	}
	skills, err := ownedSkillItems(ctx, c, "", cs.Name, true)
	if err != nil {
		return nil, err
	}
	return sourceDetail(cs.Name, "", "cluster", "clusterskillsource",
		cs.Spec.RepoURL, cs.Spec.Ref, cs.Spec.Subpath, &cs.Status, skills), nil
}

func sourceDetail(name, ns, scope, editKind, repo, ref, subpath string, status *spiceboxv1alpha1.SkillSourceStatus, skills []config.ListItem) *config.ResourceDetail {
	st, reason := projectStatus(status.Conditions, spiceboxv1alpha1.SkillSourceConditionReady, "Ready")
	d := &config.ResourceDetail{
		Name:         name,
		Namespace:    ns,
		Scope:        scope,
		Status:       st,
		StatusReason: reason,
		Description:  repo,
		ManageCmd:    editCmd(editKind, name, ns),
	}
	// The Commit field shows the short SHA as its label and links the full github
	// commit URL via Href. Both are empty (so fieldsSection drops the field) for a
	// non-github repo or an unresolved SHA — no bad link, no bare SHA.
	commitURL := githubCommitURL(repo, status.ResolvedSHA)
	commitLabel := ""
	if commitURL != "" {
		commitLabel = shortSHA(status.ResolvedSHA)
	}
	d.Sections = appendSection(d.Sections, fieldsSection("overview", "Overview",
		fld("Repo URL", repo),
		fld("Ref", ref),
		fld("Subpath", subpath),
		fld("Resolved SHA", shortSHA(status.ResolvedSHA)),
		fldHref("Commit", commitLabel, commitURL),
		fld("Discovered skills", fmt.Sprintf("%d", status.DiscoveredSkills)),
		fld("Last sync", timeOrEmpty(status.LastSyncTime)),
	))
	d.Sections = appendSection(d.Sections, listSection("skills", "Skills", skills))
	problems := make([]config.ListItem, 0, len(status.DiscoveryProblems))
	for _, p := range status.DiscoveryProblems {
		problems = append(problems, config.ListItem{Title: p})
	}
	d.Sections = appendSection(d.Sections, listSection("problems", "Discovery problems", problems))
	return d
}

// ownedSkillItems lists the Skill (namespaced) or ClusterSkill (cluster) CRs a
// SkillSource materialized — matched on Spec.Source.SourceName — as linked list
// items for the source detail's Skills tab. Each links to the skill's detail
// page (id = "<ns>/<name>" namespaced, bare "<name>" cluster); the canonical
// name is the subtitle and the resolved SHA a badge when set.
func ownedSkillItems(ctx context.Context, c client.Client, ns, sourceName string, cluster bool) ([]config.ListItem, error) {
	var items []config.ListItem
	appendItem := func(skName, skNS string, spec *spiceboxv1alpha1.SkillSpec) {
		if spec.Source == nil || spec.Source.SourceName != sourceName {
			return
		}
		id := skName
		if skNS != "" {
			id = skNS + "/" + skName
		}
		var badges []config.Badge
		if sha := shortSHA(spec.Source.ResolvedSHA); sha != "" {
			badges = append(badges, config.Badge{Key: "sha", Value: sha})
		}
		items = append(items, config.ListItem{
			Title:    skName,
			Subtitle: spec.CanonicalName,
			Link:     &config.Link{Entity: "skill", ID: id},
			Badges:   badges,
		})
	}
	if cluster {
		var list spiceboxv1alpha1.ClusterSkillList
		if err := c.List(ctx, &list); err != nil {
			return nil, err
		}
		for i := range list.Items {
			s := &list.Items[i]
			appendItem(s.Name, "", &s.Spec)
		}
		return items, nil
	}
	var list spiceboxv1alpha1.SkillList
	if err := c.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	for i := range list.Items {
		s := &list.Items[i]
		appendItem(s.Name, s.Namespace, &s.Spec)
	}
	return items, nil
}

// githubCommitURL builds a github.com commit deep-link (<repo>/commit/<sha>)
// for an http(s) github repo with a resolved SHA, else "". Only github repos
// are linked — the commit path is github-specific. A trailing "/" or ".git" on
// the repo is stripped so the path joins cleanly.
func githubCommitURL(repo, sha string) string {
	if repo == "" || sha == "" {
		return ""
	}
	u, err := url.Parse(repo)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host != "github.com" {
		return ""
	}
	base := strings.TrimSuffix(strings.TrimRight(repo, "/"), ".git")
	return base + "/commit/" + sha
}

// ---- channels (Channel) ---------------------------------------------------

type channelsDetailProjector struct{}

func (channelsDetailProjector) Resource() string { return "channels" }

func (channelsDetailProjector) Detail(ctx context.Context, c client.Client, ns, name string) (*config.ResourceDetail, error) {
	var ch spiceboxv1alpha1.Channel
	if found, err := getObj(ctx, c, client.ObjectKey{Namespace: ns, Name: name}, &ch); err != nil || !found {
		return nil, err
	}
	st, reason := projectStatus(ch.Status.Conditions, spiceboxv1alpha1.ChannelConditionConnected, "Connected")
	d := &config.ResourceDetail{
		Name:         ch.Name,
		Namespace:    ch.Namespace,
		Scope:        "namespaced",
		Status:       st,
		StatusReason: reason,
		ManageCmd:    editCmd("channel", ch.Name, ch.Namespace),
	}

	role := ch.Spec.Role
	if role == "" {
		role = "both"
	}
	classID := ""
	if ch.Spec.AgentClass != "" {
		classID = ch.Namespace + "/" + ch.Spec.AgentClass
	}
	identityID := ""
	if ch.Spec.AgentIdentity != "" {
		identityID = ch.Namespace + "/" + ch.Spec.AgentIdentity
	}
	d.Sections = appendSection(d.Sections, fieldsSection("overview", "Overview",
		fld("Kind", ch.Spec.Kind),
		fld("Role", role),
		fldLink("Agent class", ch.Spec.AgentClass, "agent", classID),
		fldLink("Agent identity", ch.Spec.AgentIdentity, "identity", identityID),
		fld("Session scope", ch.Spec.SessionScope),
		fld("Authz subject", ch.Spec.AuthzSubject),
	))
	d.Sections = appendSection(d.Sections, fieldsSection("connection", "Connection",
		fld("Credentials secret", ch.Spec.CredentialsRef.SecretName),
	))
	return d, nil
}

// ---- identities (AgentIdentity) -------------------------------------------

type identitiesDetailProjector struct{}

func (identitiesDetailProjector) Resource() string { return "identities" }

func (identitiesDetailProjector) Detail(ctx context.Context, c client.Client, ns, name string) (*config.ResourceDetail, error) {
	var ai spiceboxv1alpha1.AgentIdentity
	if found, err := getObj(ctx, c, client.ObjectKey{Namespace: ns, Name: name}, &ai); err != nil || !found {
		return nil, err
	}
	st, reason := projectStatus(ai.Status.Conditions, spiceboxv1alpha1.AgentIdentityConditionValid, "Valid")
	d := &config.ResourceDetail{
		Name:         ai.Name,
		Namespace:    ai.Namespace,
		Scope:        "namespaced",
		Status:       st,
		StatusReason: reason,
		Description:  ai.Spec.Description,
		ManageCmd:    editCmd("agentidentity", ai.Name, ai.Namespace),
	}
	d.Sections = appendSection(d.Sections, credentialsSection(ai.Spec.Credentials, ai.Status.ResolvedCredentials))
	d.Sections = appendSection(d.Sections, fieldsSection("overview", "Overview",
		fld("Refresh threshold", durationRefOrEmpty(ai.Spec.RefreshThreshold)),
		fld("Resolved credentials", fmt.Sprintf("%d", len(ai.Status.ResolvedCredentials))),
		fld("Last setup", timeOrEmpty(ai.Status.LastSetupAt)),
		fld("Last refresh", timeOrEmpty(ai.Status.LastRefreshAt)),
	))
	return d, nil
}

// ---- users (UserIdentity) -------------------------------------------------

type usersDetailProjector struct{}

func (usersDetailProjector) Resource() string { return "users" }

func (usersDetailProjector) Detail(ctx context.Context, c client.Client, ns, name string) (*config.ResourceDetail, error) {
	// The users LIST projector keys each row on the human identity
	// (displayName else subject), NOT the CR's metadata.name (an opaque hash of
	// the subject). So a detail link's id is a subject/displayName/hash — a bare
	// Get by metadata.name would miss. Resolve by listing and matching the id
	// against subject, displayName, or metadata.name (first match wins); 404
	// only when truly none match.
	var list spiceboxv1alpha1.UserIdentityList
	if err := c.List(ctx, &list); err != nil {
		return nil, err
	}
	var ui *spiceboxv1alpha1.UserIdentity
	for i := range list.Items {
		u := &list.Items[i]
		if u.Spec.Subject == name || u.Spec.DisplayName == name || u.Name == name {
			ui = u
			break
		}
	}
	if ui == nil {
		return nil, nil
	}
	st, reason := projectStatus(ui.Status.Conditions, spiceboxv1alpha1.UserIdentityConditionValid, "Valid")
	display := ui.Spec.DisplayName
	if display == "" {
		display = ui.Spec.Subject
	}
	d := &config.ResourceDetail{
		Name:         display,
		Scope:        "cluster",
		Status:       st,
		StatusReason: reason,
		Description:  ui.Spec.Subject,
		// No ManageCmd: UserIdentity CRs are owned by the identity setup flow,
		// not hand-edited via `kubectl edit` — so the detail page shows no
		// Manage block (matching the users list projector, commit 0efac721).
	}
	d.Sections = appendSection(d.Sections, fieldsSection("overview", "Overview",
		fld("Subject", ui.Spec.Subject),
		fld("Display name", ui.Spec.DisplayName),
		fld("Available credentials", fmt.Sprintf("%d", len(ui.Status.AvailableCredentials))),
		fld("Resolved credentials", fmt.Sprintf("%d", len(ui.Status.ResolvedCredentials))),
		fld("Last refresh", timeOrEmpty(ui.Status.LastRefreshAt)),
	))
	d.Sections = appendSection(d.Sections, credentialsSection(ui.Spec.Credentials, ui.Status.ResolvedCredentials))
	d.Sections = appendSection(d.Sections, channelIdentitiesSection(ui.Status.ChannelIdentities))
	if subjectIdentityReader != nil {
		ids, idErr := subjectIdentityReader(ctx, ui.Spec.Subject)
		d.Sections = appendSection(d.Sections, directoryIdentitiesSection(ids, idErr))
	}
	return d, nil
}

// ---- providers (ClusterIdentityProvider) ----------------------------------

type providersDetailProjector struct{}

func (providersDetailProjector) Resource() string { return "providers" }

func (providersDetailProjector) Detail(ctx context.Context, c client.Client, ns, name string) (*config.ResourceDetail, error) {
	var p spiceboxv1alpha1.ClusterIdentityProvider
	if found, err := getObj(ctx, c, client.ObjectKey{Name: name}, &p); err != nil || !found {
		return nil, err
	}
	st, reason := projectStatus(p.Status.Conditions, spiceboxv1alpha1.ConditionIdPValid, "Valid")
	federation := "no"
	if p.Spec.Federation != nil && p.Spec.Federation.Enabled {
		federation = "yes"
	}
	d := &config.ResourceDetail{
		Name:         p.Name,
		Scope:        "cluster",
		Status:       st,
		StatusReason: reason,
		ManageCmd:    editCmd("clusteridentityprovider", p.Name, ""),
	}
	d.Sections = appendSection(d.Sections, fieldsSection("overview", "Overview",
		fld("Kind", p.Spec.Kind),
		fld("Issuer", p.Spec.Issuer),
		fld("Client ID", p.Spec.ClientID),
		fld("Allowed email domains", join(p.Spec.AllowedEmailDomains)),
		fld("Allow any email", yesNo(p.Spec.AllowAnyEmail)),
		fld("Session TTL", durationRefOrEmpty(p.Spec.SessionTTL)),
		fld("Federation", federation),
	))
	d.Sections = appendSection(d.Sections, fieldsSection("connection", "Connection",
		fld("Client secret", secretRefString(p.Spec.ClientSecretRef.Namespace, p.Spec.ClientSecretRef.Name, p.Spec.ClientSecretRef.Key)),
	))
	return d, nil
}

// ---- directory (RelationshipSource) ---------------------------------------

type directoryDetailProjector struct{}

func (directoryDetailProjector) Resource() string { return "directory" }

func (directoryDetailProjector) Detail(ctx context.Context, c client.Client, ns, name string) (*config.ResourceDetail, error) {
	var src spiceboxv1alpha1.RelationshipSource
	if found, err := getObj(ctx, c, client.ObjectKey{Namespace: ns, Name: name}, &src); err != nil || !found {
		return nil, err
	}
	// Two words, deliberately. st folds Ready + PartialFailure for the header
	// (so a source that synced most of a directory and failed the rest does
	// not read as healthy); readyWord is Ready ALONE, and is what the Health
	// tab below is keyed on — passing the folded word there would make a
	// partial failure print Ready's own message, which on a partial failure is
	// the word "Synced".
	st, reason := projectRelationshipSourceStatus(src.Status.Conditions)
	readyWord, _ := projectStatus(src.Status.Conditions,
		spiceboxv1alpha1.RelationshipSourceConditionReady, "Ready")

	d := &config.ResourceDetail{
		Name:         src.Name,
		Namespace:    src.Namespace,
		Scope:        "namespaced",
		Status:       st,
		StatusReason: reason,
		Description:  "Directory sync (" + src.Spec.Kind + ")",
		ManageCmd:    "oap directory configure",
	}

	d.Sections = appendSection(d.Sections, fieldsSection("overview", "Overview",
		fld("Kind", src.Spec.Kind),
		fldLink("Credential identity", src.Spec.Auth.AgentIdentity, "identity", src.Namespace+"/"+src.Spec.Auth.AgentIdentity),
		fld("Credential", src.Spec.Auth.Credential),
		fld("Endpoint", src.Spec.BaseURL),
		fld("Interval", syncIntervalOrDefault(src.Spec.Sync.Interval.Duration)),
	))

	// Counts render only once a pass has been recorded; see the list
	// projector's own note on why zeros would be a different claim.
	syncFields := []config.Field{
		fld("Last sync", timeOrEmpty(src.Status.Sync.LastSyncTime)),
		fld("Cycle started", timeOrEmpty(src.Status.Sync.CycleStartedAt)),
		fld("Enumeration complete", yesNo(src.Status.Sync.EnumComplete)),
		fld("Resume after", src.Status.Sync.ResumeAfter),
	}
	if lp := src.Status.Sync.LastPass; lp != nil {
		syncFields = append(syncFields,
			fld("Last pass finished", timeOrEmpty(lp.FinishedAt)),
			fld("Scoped pass", yesNo(lp.Scoped)),
			fld("Scopes processed", strconv.Itoa(int(lp.ScopesProcessed))),
			fld("Written", strconv.Itoa(int(lp.Written))),
			fld("Pruned", strconv.Itoa(int(lp.Pruned))),
			fld("Reaped scopes", strconv.Itoa(int(lp.ReapedScopes))),
			fld("Join misses", strconv.Itoa(int(lp.JoinMisses))),
			fld("Scope errors", strconv.Itoa(int(lp.ScopeErrors))),
		)
	}
	d.Sections = appendSection(d.Sections, fieldsSection("sync", "Sync", syncFields...))

	// The sampled failures themselves. Only a SAMPLE reaches status (a failing
	// directory must not grow the CR), so when the count exceeds what is shown
	// the list carries the notice Section.Text is for — the list is partial,
	// and presenting it as complete is how "5 repos are broken" gets read off a
	// page that means "at least 156 are".
	if lp := src.Status.Sync.LastPass; lp != nil && len(lp.ScopeErrorSamples) > 0 {
		items := make([]config.ListItem, 0, len(lp.ScopeErrorSamples))
		for _, se := range lp.ScopeErrorSamples {
			title := se.Scope
			if title == "" {
				// relsync reports an enumeration or reap-scan failure with no
				// scope at all (relsync.ScopeError's own doc); a blank title
				// would read as a scope whose name we lost.
				title = "(source-level)"
			}
			items = append(items, config.ListItem{Title: title, Subtitle: se.Message})
		}
		sec := listSection("scopeerrors", "Scope errors", items)
		if int(lp.ScopeErrors) > len(items) {
			sec.Text = fmt.Sprintf("%d scopes failed in the last pass; showing %d.",
				lp.ScopeErrors, len(items))
		}
		d.Sections = appendSection(d.Sections, sec)
	}

	if src.Spec.Config != nil && len(src.Spec.Config.Raw) > 0 {
		d.Sections = appendSection(d.Sections, textSection("config", "Config", string(src.Spec.Config.Raw)))
	}
	// A PARKED source (another RelationshipSource already claims this
	// spec.kind — see the relationshipsource controller's kind-claim gate,
	// reason KindClaimed) never binds a writer and never syncs a tuple. The
	// #relhash sentinel ListSourceScopes reads carries no source identity
	// (its subject is a string digest, not a reference back to the CR that
	// wrote it), so a reader keyed on kind alone cannot tell a parked source
	// from the incumbent that actually won the kind. Showing the incumbent's
	// scopes here would be a false positive claim for a CR that has nothing
	// of its own — reason is already computed two lines above (projectStatus),
	// so this reuses it rather than re-deriving from src.Status.Conditions.
	if sourceScopeReader != nil && reason != spiceboxv1alpha1.ReasonRelationshipSourceKindClaimed {
		scopes, scopeErr := sourceScopeReader(ctx, src.Spec.Kind, directoryScopesCapPerDefinition)
		d.Sections = appendSection(d.Sections, directoryScopesSection(scopes, scopeErr))
	}
	// One Health tab, never two — both branches use the id "health". A hard
	// failure dominates (it is the strictly worse state, and the same order
	// projectRelationshipSourceStatus folds the status word in); a partial
	// failure gets the tab only when Ready itself is fine, which is the case
	// that used to surface nowhere at all.
	if health := healthSection(src.Status.Conditions,
		spiceboxv1alpha1.RelationshipSourceConditionReady, "Ready", readyWord); health.Text != "" {
		d.Sections = appendSection(d.Sections, health)
	} else if pf := relationshipSourcePartialFailure(src.Status.Conditions); pf != nil && pf.Message != "" {
		d.Sections = appendSection(d.Sections, textSection("health", "Health", pf.Message))
	}
	return d, nil
}

// ---- shared helpers -------------------------------------------------------

// credentialsSection renders an identity's declared credentials as a list,
// badging each with its type and whether it currently resolves.
func credentialsSection(creds []spiceboxv1alpha1.AgentCredential, resolved []string) config.Section {
	items := make([]config.ListItem, 0, len(creds))
	for _, cr := range creds {
		badges := []config.Badge{{Key: "type", Value: cr.Type}}
		// The UI renders each badge VALUE-only (the key is dropped), so a bare
		// "yes"/"no" reads ambiguously. Use a self-describing value that says
		// WHAT the credential's state is — "resolved" (configured & available)
		// or "unresolved" — alongside the type chip (static/oauth/federated).
		avail := "unresolved"
		if contains(resolved, cr.Name) {
			avail = "resolved"
		}
		badges = append(badges, config.Badge{Key: "resolved", Value: avail})
		items = append(items, config.ListItem{Title: cr.Name, Badges: badges})
	}
	return listSection("credentials", "Credentials", items)
}

// syncIntervalOrDefault renders a RelationshipSource's poll cadence.
//
// An unset spec.sync.interval is the zero Duration, and Duration.String()
// renders that as "0s" — which in a monitoring panel reads as "syncs
// continuously", the opposite of what zero means. Zero means "no override":
// the controller falls back to its own --sync-interval flag and, failing that,
// a built-in cadence (relationshipsource's (*Reconciler).interval). The number itself
// is deliberately NOT rendered here — admind cannot see the operator's flag, so
// naming a concrete duration would be a guess that is wrong on any cluster that
// set one.
func syncIntervalOrDefault(d time.Duration) string {
	if d <= 0 {
		return "default (controller-chosen)"
	}
	return d.String()
}

func timeOrEmpty(t *metav1.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.Time.UTC().Format("2006-01-02 15:04 UTC")
}

func durationRefOrEmpty(d *metav1.Duration) string {
	if d == nil {
		return ""
	}
	return d.Duration.String()
}

func secretRefString(namespace, name, key string) string {
	if name == "" {
		return ""
	}
	if namespace != "" {
		return fmt.Sprintf("%s/%s#%s", namespace, name, key)
	}
	return fmt.Sprintf("%s#%s", name, key)
}

func init() {
	config.RegisterDetail(&skillsDetailProjector{})
	config.RegisterDetail(&sourcesDetailProjector{})
	config.RegisterDetail(&channelsDetailProjector{})
	config.RegisterDetail(&identitiesDetailProjector{})
	config.RegisterDetail(&usersDetailProjector{})
	config.RegisterDetail(&providersDetailProjector{})
	config.RegisterDetail(&directoryDetailProjector{})
}
