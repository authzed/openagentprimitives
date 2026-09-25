package projectors

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

// sectionByID finds the section with id, failing the test if absent.
func sectionByID(t *testing.T, d *config.ResourceDetail, id string) config.Section {
	t.Helper()
	for _, s := range d.Sections {
		if s.ID == id {
			return s
		}
	}
	require.Failf(t, "section not found", "no section id=%q in %+v", id, d.Sections)
	return config.Section{}
}

// hasSection reports whether a section with id exists.
func hasSection(d *config.ResourceDetail, id string) bool {
	for _, s := range d.Sections {
		if s.ID == id {
			return true
		}
	}
	return false
}

// fieldVal returns the value of the field with label in section, or "".
func fieldVal(s config.Section, label string) string {
	for _, f := range s.Fields {
		if f.Label == label {
			return f.Value
		}
	}
	return ""
}

// fieldLinkOf returns the link of the field with label in section, or nil.
func fieldLinkOf(s config.Section, label string) *config.Link {
	for _, f := range s.Fields {
		if f.Label == label {
			return f.Link
		}
	}
	return nil
}

// fieldHrefOf returns the external href of the field with label in section, or "".
func fieldHrefOf(s config.Section, label string) string {
	for _, f := range s.Fields {
		if f.Label == label {
			return f.Href
		}
	}
	return ""
}

func TestAgentDetail(t *testing.T) {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "support-bot", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Description:    "answers support tickets",
			IdentityMode:   "agent",
			AgentIdentity:  "bot-identity",
			SystemPrompt:   spiceboxv1alpha1.PromptSource{Inline: "You are a helpful support agent."},
			Model:          &spiceboxv1alpha1.ModelConfig{Provider: "anthropic", Name: "claude"},
			Budget:         &spiceboxv1alpha1.BudgetConfig{MaxTurns: 10, MaxTokens: 5000, MaxDuration: metav1.Duration{Duration: 30 * time.Minute}},
			ToolSessionLog: "highSignal",
			MCPServers:     []spiceboxv1alpha1.AgentClassMCPServerRef{{Name: "gh", Ref: "github-mcp"}},
			Skills:         []spiceboxv1alpha1.AgentSkill{{Name: "triage", Ref: "github.com/org/repo//skills/triage@v1"}},
		},
		Status: spiceboxv1alpha1.AgentClassStatus{
			Conditions: []metav1.Condition{cond(spiceboxv1alpha1.AgentClassConditionValid, metav1.ConditionTrue, "Ready")},
		},
	}
	c := newClient(t, ac)

	d, err := (agentsDetailProjector{}).Detail(context.Background(), c, "default", "support-bot")
	require.NoError(t, err)
	require.NotNil(t, d)

	assert.Equal(t, "support-bot", d.Name)
	assert.Equal(t, "namespaced", d.Scope)
	assert.Equal(t, "Valid", d.Status)
	assert.Equal(t, "answers support tickets", d.Description)

	// Prompt tab: the inline system prompt verbatim.
	prompt := sectionByID(t, d, "prompt")
	assert.Equal(t, config.SectionText, prompt.Kind)
	assert.Equal(t, "You are a helpful support agent.", prompt.Text)

	// Tools tab: the MCP ref, linked to its tool detail page (ns/ref).
	tools := sectionByID(t, d, "tools")
	require.Len(t, tools.Items, 1)
	assert.Equal(t, "gh", tools.Items[0].Title)
	require.NotNil(t, tools.Items[0].Link)
	assert.Equal(t, "tool", tools.Items[0].Link.Entity)
	assert.Equal(t, "default/github-mcp", tools.Items[0].Link.ID)

	// Skills tab: local Name as the label, canonical Ref as the value,
	// deliberately linkless.
	skills := sectionByID(t, d, "skills")
	require.Len(t, skills.Items, 1)
	assert.Equal(t, "triage", skills.Items[0].Title)
	assert.Equal(t, "github.com/org/repo//skills/triage@v1", skills.Items[0].Subtitle)
	assert.Nil(t, skills.Items[0].Link, "skills are linkless: canonical name != Skill CR name")

	// Settings tab: resolved model + budget.
	settings := sectionByID(t, d, "settings")
	assert.Equal(t, "anthropic/claude", fieldVal(settings, "Model"))
	assert.Equal(t, "10", fieldVal(settings, "Max turns"))

	// Identity tab: mode + linked identity ref.
	identity := sectionByID(t, d, "identity")
	assert.Equal(t, "agent", fieldVal(identity, "Identity mode"))
	link := fieldLinkOf(identity, "Agent identity")
	require.NotNil(t, link)
	assert.Equal(t, "identity", link.Entity)
	assert.Equal(t, "default/bot-identity", link.ID)

	// Healthy → no Health tab.
	assert.False(t, hasSection(d, "health"), "a Valid agent has no Health tab")

	// No status.oapInstall (created by kubectl apply / wizard) → no OAP tab.
	assert.False(t, hasSection(d, "oap"), "an AgentClass with no oapInstall has no OAP Bundle tab")
}

func TestAgentDetail_OapInstallRegistry(t *testing.T) {
	installedAt := metav1.NewTime(time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC))
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "default"},
		Status: spiceboxv1alpha1.AgentClassStatus{
			OapInstall: &spiceboxv1alpha1.OapInstallStatus{
				SourceRef:   "ghcr.io/example/demo-agent:1",
				Digest:      "sha256:abcdef0123456789",
				Version:     "1.2.0",
				SourceKind:  "registry",
				InstalledAt: installedAt,
			},
		},
	}
	c := newClient(t, ac)

	d, err := (agentsDetailProjector{}).Detail(context.Background(), c, "default", "demo-agent")
	require.NoError(t, err)
	require.NotNil(t, d)

	oap := sectionByID(t, d, "oap")
	assert.Equal(t, config.SectionFields, oap.Kind)
	assert.Equal(t, "ghcr.io/example/demo-agent:1", fieldVal(oap, "Source"))
	assert.Equal(t, "sha256:abcdef0123456789", fieldVal(oap, "Digest"))
	assert.Equal(t, "1.2.0", fieldVal(oap, "Version"))
	assert.NotEmpty(t, fieldVal(oap, "Installed"))
	assert.Equal(t,
		"oap agent pull ghcr.io/example/demo-agent:1@sha256:abcdef0123456789 -o demo-agent.oap",
		fieldVal(oap, "Pull command"))
}

func TestAgentDetail_OapInstallFile(t *testing.T) {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "local-agent", Namespace: "default"},
		Status: spiceboxv1alpha1.AgentClassStatus{
			OapInstall: &spiceboxv1alpha1.OapInstallStatus{
				Digest:      "sha256:fedcba9876543210",
				Version:     "0.1.0",
				SourceKind:  "file",
				InstalledAt: metav1.NewTime(time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)),
			},
		},
	}
	c := newClient(t, ac)

	d, err := (agentsDetailProjector{}).Detail(context.Background(), c, "default", "local-agent")
	require.NoError(t, err)
	require.NotNil(t, d)

	oap := sectionByID(t, d, "oap")
	assert.Equal(t, "local file", fieldVal(oap, "Source"))
	assert.Equal(t, "sha256:fedcba9876543210", fieldVal(oap, "Digest"))
	assert.Equal(t, "0.1.0", fieldVal(oap, "Version"))
	assert.Empty(t, fieldVal(oap, "Pull command"), "a file source has no OCI ref to pull from")
}

func TestAgentDetail_NotFound(t *testing.T) {
	c := newClient(t)
	d, err := (agentsDetailProjector{}).Detail(context.Background(), c, "default", "ghost")
	require.NoError(t, err)
	assert.Nil(t, d, "absent AgentClass → nil detail (handler 404)")
}

func TestAgentDetail_ConfigMapPromptAndDegraded(t *testing.T) {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "bot", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			SystemPrompt: spiceboxv1alpha1.PromptSource{
				ConfigMapRef: &spiceboxv1alpha1.ConfigMapKeyRef{Name: "prompts", Key: "bot"},
			},
		},
		Status: spiceboxv1alpha1.AgentClassStatus{
			Conditions: []metav1.Condition{{
				Type: spiceboxv1alpha1.AgentClassConditionValid, Status: metav1.ConditionFalse,
				Reason: "InvalidSkill", Message: "skill triage is not allowed", LastTransitionTime: metav1.Now(),
			}},
		},
	}
	c := newClient(t, ac)
	d, err := (agentsDetailProjector{}).Detail(context.Background(), c, "default", "bot")
	require.NoError(t, err)
	require.NotNil(t, d)

	assert.Equal(t, "Degraded", d.Status)
	prompt := sectionByID(t, d, "prompt")
	assert.Contains(t, prompt.Text, "ConfigMap", "configmap-sourced prompt shows a provenance note")

	// Degraded → a Health tab carrying the full condition message.
	health := sectionByID(t, d, "health")
	assert.Equal(t, config.SectionText, health.Kind)
	assert.Contains(t, health.Text, "not allowed")
}

func TestToolDetail_MCPServer(t *testing.T) {
	m := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "github-mcp", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Intent: "GitHub operations",
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://mcp.example.com", Transport: "http"},
			Auth:   spiceboxv1alpha1.MCPServerAuth{Type: "oauth", Credential: "gh-token"},
			Tools:  []spiceboxv1alpha1.MCPServerTool{{Name: "create_issue", Intent: "open an issue"}},
		},
		Status: spiceboxv1alpha1.MCPServerStatus{
			ObservedTools: []string{"create_issue", "list_issues"},
			Conditions:    []metav1.Condition{cond(spiceboxv1alpha1.MCPServerConditionReachable, metav1.ConditionTrue, "Reachable")},
		},
	}
	c := newClient(t, m)

	d, err := (toolsDetailProjector{}).Detail(context.Background(), c, "default", "github-mcp")
	require.NoError(t, err)
	require.NotNil(t, d)

	assert.Equal(t, "namespaced", d.Scope)
	assert.Equal(t, "Reachable", d.Status)
	assert.Equal(t, "GitHub operations", d.Description)

	conn := sectionByID(t, d, "connection")
	assert.Equal(t, "mcpserver", fieldVal(conn, "Kind"))
	assert.Equal(t, "https://mcp.example.com", fieldVal(conn, "Endpoint"))
	assert.Equal(t, "gh-token", fieldVal(conn, "Credential"))

	tools := sectionByID(t, d, "tools")
	require.Len(t, tools.Items, 1)
	assert.Equal(t, "create_issue", tools.Items[0].Title)

	observed := sectionByID(t, d, "observed")
	assert.Len(t, observed.Items, 2)
}

// TestToolDetail_SidecarToolbox_DeferredReachability proves the admin UI folds a
// secret-gated sidecar's Valid=True + Reachable=Unknown/DeferredToSession into
// the display status "Deferred" — which the frontend maps to a distinct
// primary/blue tone (see lib/status.ts), NOT green (Valid), amber (Degraded), or
// grey (Unknown). This is the "show Reachable in a different color when deferred"
// requirement.
func TestToolDetail_SidecarToolbox_DeferredReachability(t *testing.T) {
	b := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-tb", Namespace: "default"},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Intent:       "Kubernetes debugging",
			Source:       spiceboxv1alpha1.SidecarToolboxSource{Image: "sre-k8s-mcp:dev"},
			Sandbox:      spiceboxv1alpha1.SidecarToolboxSandbox{Class: "sre-sandbox"},
			Transport:    spiceboxv1alpha1.SidecarToolboxTransport{Port: 8080},
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "tailscale-authkey"},
			Tools:        []spiceboxv1alpha1.MCPServerTool{{Name: "cluster_info"}},
		},
		Status: spiceboxv1alpha1.SidecarToolboxStatus{
			Conditions: []metav1.Condition{
				cond(spiceboxv1alpha1.SidecarToolboxConditionValid, metav1.ConditionTrue, spiceboxv1alpha1.ReasonSidecarToolboxSpecOK),
				cond(spiceboxv1alpha1.SidecarToolboxConditionReachable, metav1.ConditionUnknown, spiceboxv1alpha1.ReasonSidecarToolboxDeferredToSession),
			},
		},
	}
	c := newClient(t, b)

	d, err := (toolsDetailProjector{}).Detail(context.Background(), c, "default", "kube-tb")
	require.NoError(t, err)
	require.NotNil(t, d)

	assert.Equal(t, "Deferred", d.Status, "Valid+deferred-reachable folds to the distinct Deferred status")
	assert.Equal(t, spiceboxv1alpha1.ReasonSidecarToolboxDeferredToSession, d.StatusReason)
}

// TestToolDetail_SidecarToolbox_AllowedHosts proves the toolbox's own
// contribution to the effective egress allowlist (spec.sandbox.network.allowedHosts)
// is surfaced — it was previously dropped entirely, leaving an operator unable
// to see it without kubectl even though the Sandbox class field right next to
// it was shown.
func TestToolDetail_SidecarToolbox_AllowedHosts(t *testing.T) {
	b := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-tb", Namespace: "default"},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Source:  spiceboxv1alpha1.SidecarToolboxSource{Image: "sre-k8s-mcp:dev"},
			Sandbox: spiceboxv1alpha1.SidecarToolboxSandbox{Class: "sre-sandbox", Network: spiceboxv1alpha1.SidecarToolboxNetwork{AllowedHosts: []string{"api.example.com", "auth.example.com"}}},
		},
	}
	c := newClient(t, b)

	d, err := (toolsDetailProjector{}).Detail(context.Background(), c, "default", "kube-tb")
	require.NoError(t, err)
	require.NotNil(t, d)

	policy := sectionByID(t, d, "policy")
	assert.Equal(t, "api.example.com, auth.example.com", fieldVal(policy, "Allowed hosts"))
}

// TestToolDetail_SidecarToolbox_NoAllowedHostsOmitsPolicySection proves an
// empty allowlist drops the Policy section entirely (fieldsSection/appendSection's
// established empty-drops convention), rather than rendering a blank tab.
func TestToolDetail_SidecarToolbox_NoAllowedHostsOmitsPolicySection(t *testing.T) {
	b := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-tb", Namespace: "default"},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Source:  spiceboxv1alpha1.SidecarToolboxSource{Image: "sre-k8s-mcp:dev"},
			Sandbox: spiceboxv1alpha1.SidecarToolboxSandbox{Class: "sre-sandbox"},
		},
	}
	c := newClient(t, b)

	d, err := (toolsDetailProjector{}).Detail(context.Background(), c, "default", "kube-tb")
	require.NoError(t, err)
	require.NotNil(t, d)

	assert.False(t, hasSection(d, "policy"), "no allowedHosts -> no Policy section")
}

func TestToolDetail_ToolspecClusterScoped(t *testing.T) {
	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "kubectl-ro"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Intent:           "read-only kubectl",
			Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: "kubectl", Revision: "v1"},
			AllowSubcommands: []string{"get", "describe"},
			Allow: spiceboxv1alpha1.ToolspecAllow{
				Creds: spiceboxv1alpha1.ToolspecAllowCreds{Required: []string{"kubeconfig"}},
			},
		},
		Status: spiceboxv1alpha1.SpiceboxToolspecStatus{
			ResolvedToolkit: "kubectl",
			Conditions:      []metav1.Condition{cond(spiceboxv1alpha1.SpiceboxToolspecConditionValid, metav1.ConditionTrue, "Ready")},
		},
	}
	c := newClient(t, ts)

	// Cluster-scoped → ns == "".
	d, err := (toolsDetailProjector{}).Detail(context.Background(), c, "", "kubectl-ro")
	require.NoError(t, err)
	require.NotNil(t, d)

	assert.Equal(t, "cluster", d.Scope)
	cmd := sectionByID(t, d, "command")
	assert.Equal(t, "toolspec", fieldVal(cmd, "Kind"))
	assert.Equal(t, "kubectl@v1", fieldVal(cmd, "Toolkit"))
	assert.Equal(t, "get, describe", fieldVal(cmd, "Allowed subcommands"))

	policy := sectionByID(t, d, "policy")
	assert.Equal(t, "kubeconfig", fieldVal(policy, "Required credentials"))
}

func TestToolDetail_NotFound(t *testing.T) {
	c := newClient(t)
	d, err := (toolsDetailProjector{}).Detail(context.Background(), c, "default", "ghost")
	require.NoError(t, err)
	assert.Nil(t, d, "no tool CR at ns/name → nil detail")
}

// TestToolDetail_ToolspecToolkitLinkResolvesByResolvedName proves the toolspec's
// Toolkit link targets the resolved SpiceboxToolkit CR's metadata.name
// (status.resolvedToolkit, e.g. "git-v1"), NOT the canonical spec ref
// (spec.toolkit.name, "git") — the two differ and the tool detail resolves a
// cluster id by metadata.name, so a canonical-keyed link would 404. The seeded
// toolkit CR proves the link id actually resolves to a detail.
func TestToolDetail_ToolspecToolkitLinkResolvesByResolvedName(t *testing.T) {
	tk := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "git-v1"}, // metadata.name != canonical "git"
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Name:            "git",
			ToolkitRevision: "2026-04-24",
			Target:          spiceboxv1alpha1.ToolkitTarget{Binary: "git"},
		},
		Status: spiceboxv1alpha1.SpiceboxToolkitStatus{
			Conditions: []metav1.Condition{cond(spiceboxv1alpha1.SpiceboxToolkitConditionValid, metav1.ConditionTrue, "Ready")},
		},
	}
	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "git-rw"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Intent:           "read-write git",
			Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: "git", Revision: "2026-04-24"},
			AllowSubcommands: []string{"commit"},
		},
		Status: spiceboxv1alpha1.SpiceboxToolspecStatus{
			ResolvedToolkit: "git-v1", // the actual CR name the controller resolved
			Conditions:      []metav1.Condition{cond(spiceboxv1alpha1.SpiceboxToolspecConditionValid, metav1.ConditionTrue, "Ready")},
		},
	}
	c := newClient(t, tk, ts)

	d, err := (toolsDetailProjector{}).Detail(context.Background(), c, "", "git-rw")
	require.NoError(t, err)
	require.NotNil(t, d)
	cmd := sectionByID(t, d, "command")
	assert.Equal(t, "git@2026-04-24", fieldVal(cmd, "Toolkit"), "display value stays the canonical ref")
	link := fieldLinkOf(cmd, "Toolkit")
	require.NotNil(t, link, "Toolkit must carry a link")
	assert.Equal(t, "tool", link.Entity)
	assert.Equal(t, "git-v1", link.ID, "link id must be the resolved CR name, not the canonical ref")

	// The link id actually resolves to the toolkit detail (no 404).
	kd, err := (toolsDetailProjector{}).Detail(context.Background(), c, "", link.ID)
	require.NoError(t, err)
	require.NotNil(t, kd, "toolkit link id must resolve to a detail")
	assert.Equal(t, "cluster", kd.Scope)
	assert.Equal(t, "toolkit", fieldVal(sectionByID(t, kd, "command"), "Kind"))
}

// TestToolDetail_ToolspecBuiltinToolkitNoLink proves a builtin (CR-less)
// resolved toolkit carries no dead link.
func TestToolDetail_ToolspecBuiltinToolkitNoLink(t *testing.T) {
	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "echo"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: "echo", Revision: "v1"},
			AllowSubcommands: []string{"echo"},
		},
		Status: spiceboxv1alpha1.SpiceboxToolspecStatus{
			ResolvedToolkit: "<builtin>",
			Conditions:      []metav1.Condition{cond(spiceboxv1alpha1.SpiceboxToolspecConditionValid, metav1.ConditionTrue, "Ready")},
		},
	}
	c := newClient(t, ts)
	d, err := (toolsDetailProjector{}).Detail(context.Background(), c, "", "echo")
	require.NoError(t, err)
	require.NotNil(t, d)
	assert.Nil(t, fieldLinkOf(sectionByID(t, d, "command"), "Toolkit"),
		"a builtin toolkit has no CR — no link")
}

func TestIdentityDetail_Credentials(t *testing.T) {
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "bot-identity", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Description: "the bot's tokens",
			Credentials: []spiceboxv1alpha1.AgentCredential{
				{Name: "gh-token", Type: "static"},
				{Name: "linear", Type: "oauth"},
			},
		},
		Status: spiceboxv1alpha1.AgentIdentityStatus{
			ResolvedCredentials: []string{"gh-token"},
			Conditions:          []metav1.Condition{cond(spiceboxv1alpha1.AgentIdentityConditionValid, metav1.ConditionTrue, "Ready")},
		},
	}
	c := newClient(t, ai)

	d, err := (identitiesDetailProjector{}).Detail(context.Background(), c, "default", "bot-identity")
	require.NoError(t, err)
	require.NotNil(t, d)

	creds := sectionByID(t, d, "credentials")
	require.Len(t, creds.Items, 2)
	// gh-token resolves; linear does not. The chip value is self-describing
	// ("resolved"/"unresolved"), not a bare "yes"/"no" — the UI renders the
	// value without its key.
	assert.Equal(t, "gh-token", creds.Items[0].Title)
	assert.Equal(t, "static", badgeValItem(creds.Items[0], "type"))
	assert.Equal(t, "resolved", badgeValItem(creds.Items[0], "resolved"))
	assert.Equal(t, "unresolved", badgeValItem(creds.Items[1], "resolved"))
}

func TestDirectoryDetail_ShowsSyncStateAndKindConfig(t *testing.T) {
	finished := metav1.NewTime(time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC))
	src := &spiceboxv1alpha1.RelationshipSource{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-github", Namespace: "default"},
		Spec: spiceboxv1alpha1.RelationshipSourceSpec{
			Kind:    "github",
			Auth:    spiceboxv1alpha1.RelationshipSourceAuth{AgentIdentity: "forge-identity", Credential: "forge-pat"},
			BaseURL: "https://ghe.example.internal",
			Config:  &apiextensionsv1.JSON{Raw: []byte(`{"orgs":["acme","widgets"]}`)},
		},
		Status: spiceboxv1alpha1.RelationshipSourceStatus{
			Sync: spiceboxv1alpha1.RelationshipSourceSyncStatus{
				EnumComplete: true,
				LastPass: &spiceboxv1alpha1.RelationshipSourcePassStats{
					ScopesProcessed: 3, Written: 40, JoinMisses: 7, FinishedAt: &finished,
				},
			},
		},
	}
	c := newClient(t, src)

	p, ok := config.GetDetail("directory")
	require.True(t, ok)

	d, err := p.Detail(context.Background(), c, "default", "acme-github")
	require.NoError(t, err)
	require.NotNil(t, d)

	assert.Equal(t, "acme-github", d.Name)
	assert.Equal(t, "oap directory configure", d.ManageCmd)

	sync := sectionByID(t, d, "sync")
	assert.Equal(t, "7", fieldVal(sync, "Join misses"),
		"join misses is the number that says the identity join is broken")

	cfg := sectionByID(t, d, "config")
	assert.Contains(t, cfg.Text, "widgets", "the kind's own config must be visible")
}

// An unset spec.sync.interval means "use the controller's cadence", and
// Duration.String() renders that zero as "0s" — which on a monitoring panel
// reads as "syncs continuously", the opposite claim.
func TestDirectoryDetail_UnsetIntervalReadsAsDefaultNotZeroSeconds(t *testing.T) {
	cases := []struct {
		name     string
		interval metav1.Duration
		want     string
	}{
		{name: "unset interval: reads as the controller default, never \"0s\"", want: "default (controller-chosen)"},
		{name: "explicit interval: rendered verbatim", interval: metav1.Duration{Duration: 5 * time.Minute}, want: "5m0s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &spiceboxv1alpha1.RelationshipSource{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-sync", Namespace: "default"},
				Spec: spiceboxv1alpha1.RelationshipSourceSpec{
					Kind: "slack",
					Sync: spiceboxv1alpha1.RelationshipSourceSync{Interval: tc.interval},
				},
			}
			c := newClient(t, src)
			p, ok := config.GetDetail("directory")
			require.True(t, ok)

			d, err := p.Detail(context.Background(), c, "default", "demo-sync")
			require.NoError(t, err)
			require.NotNil(t, d)

			overview := sectionByID(t, d, "overview")
			assert.Equal(t, tc.want, fieldVal(overview, "Interval"))
			assert.NotEqual(t, "0s", fieldVal(overview, "Interval"))
		})
	}
}

// A missing CR is a 404, not a fault. Returning an error here turns an ordinary
// not-found into a 500 (see DetailProjector's own doc).
func TestDirectoryDetail_MissingSourceIsNotFoundNotError(t *testing.T) {
	c := newClient(t)
	p, _ := config.GetDetail("directory")

	d, err := p.Detail(context.Background(), c, "default", "absent")

	require.NoError(t, err, "a missing CR must not be an error")
	assert.Nil(t, d, "a missing CR must be (nil, nil) so the handler 404s")
}

// badgeValItem returns the value of the first badge with key on a ListItem.
func badgeValItem(it config.ListItem, key string) string {
	for _, b := range it.Badges {
		if b.Key == key {
			return b.Value
		}
	}
	return ""
}

// Unavailable and empty are different answers. An empty list says "this person
// has no linked identities", which is a confident claim; rendering it for a
// failed read is the no-silent-errors failure mode.
func TestDirectoryIdentitiesSection_FailedReadSaysUnavailable(t *testing.T) {
	sec := directoryIdentitiesSection(spicedb.SubjectIdentities{}, errors.New("spicedb unreachable"))

	assert.Equal(t, config.SectionText, sec.Kind,
		"a fault renders as text, not as an empty list")
	assert.Contains(t, sec.Text, "unavailable")
	assert.Contains(t, sec.Text, "spicedb unreachable",
		"the operator needs the reason, not just the fact")
}

// A successful read that found nothing renders nothing at all — appendSection
// drops an empty section, matching how channel identities behave.
func TestDirectoryIdentitiesSection_EmptySucceedsSilently(t *testing.T) {
	sec := directoryIdentitiesSection(spicedb.SubjectIdentities{}, nil)

	assert.Empty(t, sec.Items)
	assert.Empty(t, sec.Text,
		"a successful empty read must NOT claim to be unavailable")
}

// The third answer: some probes read, some did not. Neither half may be
// dropped — the rows alone would present a short list as a complete one, and
// the notice alone would hide what IS known over one absent definition.
func TestDirectoryIdentitiesSection_PartialReadKeepsRowsAndFlagsTheGap(t *testing.T) {
	sec := directoryIdentitiesSection(spicedb.SubjectIdentities{
		Identities: []spicedb.SubjectIdentity{
			{Definition: "slack_user", Relation: "user", ObjectID: "U0FKE", Source: "Slack"},
		},
		Unavailable: []spicedb.UnavailableProbe{
			{Source: "GitHub", Definition: "github_org", Relation: "member", Err: "object definition `github_org` not found"},
		},
	}, nil)

	require.Len(t, sec.Items, 1, "the link that WAS read must survive")
	assert.Equal(t, "slack_user:U0FKE", sec.Items[0].Title)
	assert.Contains(t, sec.Text, "INCOMPLETE",
		"a partially-read list must say so rather than look finished")
	assert.Contains(t, sec.Text, "github_org#member")
	assert.Contains(t, sec.Text, "GitHub", "and name the source, so an admin knows what to fix")
}

// The worst case: every probe failed. There are no rows, but the section must
// still carry the notice — appendSection drops a section with no fields, no
// text and no items, so a notice-less one here would vanish and the page would
// read as "this person has no linked identities".
func TestDirectoryIdentitiesSection_AllProbesUnavailableStillRendersTheNotice(t *testing.T) {
	sec := directoryIdentitiesSection(spicedb.SubjectIdentities{
		Unavailable: []spicedb.UnavailableProbe{
			{Source: "GitHub", Definition: "github_org", Relation: "member", Err: "boom"},
		},
	}, nil)

	assert.Empty(t, sec.Items)
	assert.Contains(t, sec.Text, "INCOMPLETE")
	kept := appendSection(nil, sec)
	require.Len(t, kept, 1, "appendSection must KEEP a rowless section that carries a notice")
	assert.Contains(t, kept[0].Text, "INCOMPLETE",
		"and keep the notice itself — the one thing on the page saying the list is short")
}

// The section renders in the order it is given (ListSubjectIdentities sorts
// source-first), naming the object and attributing it to the sync that
// asserted it. Attribution is the point: two syncs can legitimately link the
// same person, and an admin must be able to tell which one said what.
func TestDirectoryIdentitiesSection_NamesTheObjectAndItsSource(t *testing.T) {
	sec := directoryIdentitiesSection(spicedb.SubjectIdentities{Identities: []spicedb.SubjectIdentity{
		{Definition: "github_user", Relation: "user", ObjectID: "12345", Source: "GitHub"},
		{Definition: "onepassword_group", Relation: "member", ObjectID: "g-77", Source: "1Password"},
	}}, nil)

	require.Len(t, sec.Items, 2)

	assert.Equal(t, "github_user:12345", sec.Items[0].Title)
	assert.Equal(t, "asserted by GitHub · user", sec.Items[0].Subtitle,
		"each row must name the source that asserted it")
	assert.Equal(t, []config.Badge{{Key: "source", Value: "GitHub"}}, sec.Items[0].Badges)

	assert.Equal(t, "onepassword_group:g-77", sec.Items[1].Title)
	assert.Equal(t, "asserted by 1Password · member", sec.Items[1].Subtitle)
	assert.Equal(t, []config.Badge{{Key: "source", Value: "1Password"}}, sec.Items[1].Badges)

	assert.Empty(t, sec.Text, "a complete read carries no incompleteness notice")
}
