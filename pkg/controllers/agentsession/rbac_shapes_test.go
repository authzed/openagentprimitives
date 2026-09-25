package agentsession_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"

	// Every channel kind, so TestBuildRunnerRBACPinsTheChannelForEveryRegisteredKind
	// enumerates the registry rather than a transcribed list — a kind added later
	// is covered by adding its blank import, not by editing a table.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// rbac_shapes_test.go is the sufficiency harness for the ONE Role this repo
// builds at runtime.
//
// TestRBACSufficiency (pkg/platform/manifests/rbac_sufficiency_test.go) puts the
// per-session runner Role under a real SubjectAccessReview — but only in its
// MAXIMAL shape, the fixture where every conditional rule is emitted at once.
// The Role that actually ships to a pod varies with the session: its bindings,
// whether those Channels reference a Secret, what the class binds. A rule that
// disappears in one of those narrower shapes is invisible to a maximal fixture,
// which is exactly how a single conjunction came to gate two independent rules
// and took `channels get` down with the secrets rule it was about.
//
// BuildRunnerRBAC is a pure function of (session, class, resolved names), so the
// shapes are enumerable. Each case below asserts the COMPLETE emitted rule set,
// not the presence of one rule: a rule that vanishes fails the case that no
// longer lists it, and a rule that appears where it should not fails every case
// that does.
//
// Companion, not replacement: this proves WHICH rules are emitted per shape;
// the SAR harness proves the apiserver actually grants what those rules say.

const rbacShapesGroup = "agentprimitives.authzed.com"

// canonRule renders one PolicyRule as a single comparable line:
//
//	<groups>/<resources>[<resourceNames>]:<verbs>
//
// Groups, resources and resourceNames keep their emitted order — resourceNames
// are deduped order-preservingly and a reordering there is a real change.
// Verbs are sorted: RBAC evaluates the verb list as a set, so its emitted order
// carries no meaning and pinning it would only make the table brittle.
func canonRule(r rbacv1.PolicyRule) string {
	verbs := append([]string(nil), r.Verbs...)
	sort.Strings(verbs)
	return strings.Join(r.APIGroups, "|") + "/" + strings.Join(r.Resources, "|") +
		"[" + strings.Join(r.ResourceNames, ",") + "]:" + strings.Join(verbs, ",")
}

// canonRules renders a whole rule set, sorted so the comparison is over the set
// of rules rather than the order BuildRunnerRBAC happens to append them in.
// Duplicates survive sorting, so a rule emitted twice still fails.
func canonRules(rules []rbacv1.PolicyRule) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, canonRule(r))
	}
	sort.Strings(out)
	return out
}

// wantRule builds the same line from the expectation side.
func wantRule(group, resource string, names []string, verbs ...string) string {
	sorted := append([]string(nil), verbs...)
	sort.Strings(sorted)
	return group + "/" + resource + "[" + strings.Join(names, ",") + "]:" + strings.Join(sorted, ",")
}

// baseRuleSet is what EVERY runner Role carries, whatever the session looks
// like: its own object and status, its SessionUserIdentity, its class, the
// namespace Skills list, and the four dynamically-named resources the meta
// tools create (toolcalls, artifactrenders, credentialupdaterequests,
// subagentrequests).
func baseRuleSet(sessName, className string) []string {
	self := []string{sessName}
	return []string{
		wantRule(rbacShapesGroup, "agentsessions", self, "get", "watch", "patch"),
		wantRule(rbacShapesGroup, "agentsessions/status", self, "get", "patch"),
		wantRule(rbacShapesGroup, "sessionuseridentities", self, "get", "watch"),
		wantRule(rbacShapesGroup, "agentclasses", []string{className}, "get"),
		wantRule(rbacShapesGroup, "skills", nil, "get", "list", "watch"),
		wantRule(rbacShapesGroup, "toolcalls", nil, "create", "get", "list", "watch", "delete"),
		wantRule(rbacShapesGroup, "artifactrenders", nil, "create", "get", "list", "watch"),
		wantRule(rbacShapesGroup, "credentialupdaterequests", nil, "create", "get", "list"),
		wantRule(rbacShapesGroup, "subagentrequests", nil, "create", "get"),
	}
}

// shapeSession builds the session under test. Only the fields BuildRunnerRBAC
// reads are set; `kind` is carried on the bindings precisely because the
// builder must NOT vary with it.
func shapeSession(in, out *spiceboxv1alpha1.ChannelBinding) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "shape-session", Namespace: "default", UID: "uid-shape",
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:         "shape-class",
			InputChannel:  in,
			OutputChannel: out,
		},
	}
}

func binding(name, kind string) *spiceboxv1alpha1.ChannelBinding {
	return &spiceboxv1alpha1.ChannelBinding{Name: name, Kind: kind, Key: "dm:shape"}
}

// inlineClass is the do-nothing AgentClass: an inline prompt and no optional
// binding, so it contributes no conditional rule.
func inlineClass() *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "shape-class", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
		},
	}
}

// TestBuildRunnerRBACRuleSetPerSessionShape enumerates the session shapes the
// per-session Role is built from and asserts the complete rule set each one
// produces.
func TestBuildRunnerRBACRuleSetPerSessionShape(t *testing.T) {
	cases := []struct {
		name string
		sess *spiceboxv1alpha1.AgentSession
		ac   *spiceboxv1alpha1.AgentClass
		// Everything the controller resolves before calling the builder.
		bundleSessionNames   []string
		channelSecretNames   []string
		mcpAgentIdentityName string
		mcpSecretNames       []string
		// extra is what this shape adds on top of baseRuleSet.
		extra []string
	}{
		{
			name: "no bindings and a bare class: only the unconditional rules",
			sess: shapeSession(nil, nil),
			ac:   inlineClass(),
		},
		{
			name: "nil AgentClass: every class-derived rule is suppressed, the base set survives",
			sess: shapeSession(nil, nil),
			ac:   nil,
		},
		{
			name:               "input-only credential-bearing binding: the Channel and its Secret are both pinned",
			sess:               shapeSession(binding("ch-in", "slack"), nil),
			ac:                 inlineClass(),
			channelSecretNames: []string{"ch-in-creds"},
			extra: []string{
				wantRule(rbacShapesGroup, "channels", []string{"ch-in"}, "get"),
				wantRule("", "secrets", []string{"ch-in-creds"}, "get"),
			},
		},
		{
			// The regression. A Channel with no credentialsRef — the kind=agent
			// inbox a conversational delegated child speaks over — must keep its
			// `channels get`: without it resolve.ForSession is Forbidden, which
			// is warned-about and carried on from, silently disabling every
			// channel-sourced capability.
			name: "credential-less binding: `channels get` survives with no secrets rule",
			sess: shapeSession(binding("req1-inbox", "agent"), nil),
			ac:   inlineClass(),
			extra: []string{
				wantRule(rbacShapesGroup, "channels", []string{"req1-inbox"}, "get"),
			},
		},
		{
			name:               "split input/output bindings: both Channels and both Secrets are pinned",
			sess:               shapeSession(binding("cron-in", "bento"), binding("cron-out", "slack")),
			ac:                 inlineClass(),
			channelSecretNames: []string{"cron-in-creds", "cron-out-creds"},
			extra: []string{
				wantRule(rbacShapesGroup, "channels", []string{"cron-in", "cron-out"}, "get"),
				wantRule("", "secrets", []string{"cron-in-creds", "cron-out-creds"}, "get"),
			},
		},
		{
			name:               "output-only binding: the output Channel alone is pinned",
			sess:               shapeSession(nil, binding("out-only", "slack")),
			ac:                 inlineClass(),
			channelSecretNames: []string{"out-only-creds"},
			extra: []string{
				wantRule(rbacShapesGroup, "channels", []string{"out-only"}, "get"),
				wantRule("", "secrets", []string{"out-only-creds"}, "get"),
			},
		},
		{
			name:               "one Channel bound as both input and output: resourceNames are deduped",
			sess:               shapeSession(binding("ch1", "slack"), binding("ch1", "slack")),
			ac:                 inlineClass(),
			channelSecretNames: []string{"ch1-creds", "ch1-creds"},
			extra: []string{
				wantRule(rbacShapesGroup, "channels", []string{"ch1"}, "get"),
				wantRule("", "secrets", []string{"ch1-creds"}, "get"),
			},
		},
		{
			name: "a binding whose Channel resolution failed: empty secret names emit no secrets rule",
			sess: shapeSession(binding("ch1", "slack"), nil),
			ac:   inlineClass(),
			// The controller appends nothing for a Channel it could not Get, and
			// an empty string in resourceNames is rejected by the apiserver.
			channelSecretNames: []string{""},
			extra: []string{
				wantRule(rbacShapesGroup, "channels", []string{"ch1"}, "get"),
			},
		},
		{
			name: "configMapRef system prompt: configmaps is pinned to that one ConfigMap",
			sess: shapeSession(nil, nil),
			ac: func() *spiceboxv1alpha1.AgentClass {
				ac := inlineClass()
				ac.Spec.SystemPrompt = spiceboxv1alpha1.PromptSource{
					ConfigMapRef: &spiceboxv1alpha1.ConfigMapKeyRef{Name: "prompt-cm", Key: "prompt"},
				}
				return ac
			}(),
			extra: []string{wantRule("", "configmaps", []string{"prompt-cm"}, "get")},
		},
		{
			name:               "bundle sessions resolved: spiceboxsessions is pinned get/watch per bundle",
			sess:               shapeSession(nil, nil),
			ac:                 inlineClass(),
			bundleSessionNames: []string{"bundle-a", "bundle-b"},
			extra: []string{
				wantRule(rbacShapesGroup, "spiceboxsessions", []string{"bundle-a", "bundle-b"}, "get", "watch"),
			},
		},
		{
			name: "class references MCPServers: mcpservers is pinned to each ref",
			sess: shapeSession(nil, nil),
			ac: func() *spiceboxv1alpha1.AgentClass {
				ac := inlineClass()
				ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{
					{Name: "up", Ref: "mcp-a"}, {Name: "down", Ref: "mcp-b"},
				}
				return ac
			}(),
			extra: []string{
				wantRule(rbacShapesGroup, "mcpservers", []string{"mcp-a", "mcp-b"}, "get"),
			},
		},
		{
			// SidecarToolboxes get their own get rule — for the per-datum leak
			// gate's toolResourceMap lookup, not (like mcpservers) for tool
			// synthesis, which reads the resolved snapshot on status instead.
			name: "class references SidecarToolboxes: sidecartoolboxes is pinned to each ref",
			sess: shapeSession(nil, nil),
			ac: func() *spiceboxv1alpha1.AgentClass {
				ac := inlineClass()
				ac.Spec.SidecarToolboxes = []spiceboxv1alpha1.AgentClassSidecarToolboxRef{
					{Name: "one", Ref: "st-a"}, {Name: "two", Ref: "st-b"},
				}
				return ac
			}(),
			extra: []string{
				wantRule(rbacShapesGroup, "sidecartoolboxes", []string{"st-a", "st-b"}, "get"),
			},
		},
		{
			name:                 "tool identity with secret-backed credentials: agentidentities and secrets are both pinned",
			sess:                 shapeSession(nil, nil),
			ac:                   inlineClass(),
			mcpAgentIdentityName: "tool-identity",
			mcpSecretNames:       []string{"cred-a", "cred-b"},
			extra: []string{
				wantRule(rbacShapesGroup, "agentidentities", []string{"tool-identity"}, "get"),
				wantRule("", "secrets", []string{"cred-a", "cred-b"}, "get"),
			},
		},
		{
			name:                 "tool identity with no secret-backed credentials: agentidentities is pinned, no secrets rule",
			sess:                 shapeSession(nil, nil),
			ac:                   inlineClass(),
			mcpAgentIdentityName: "tool-identity",
			extra: []string{
				wantRule(rbacShapesGroup, "agentidentities", []string{"tool-identity"}, "get"),
			},
		},
		{
			name: "class binds a WorkspaceSource: batch/jobs is granted",
			sess: shapeSession(nil, nil),
			ac: func() *spiceboxv1alpha1.AgentClass {
				ac := inlineClass()
				ac.Spec.WorkspaceSource = &spiceboxv1alpha1.AgentClassWorkspaceSourceRef{Ref: "ws"}
				return ac
			}(),
			extra: []string{
				wantRule("batch", "jobs", []string{"ws-sync-shape-session", "ws-apply-shape-session"}, "get", "delete"),
				// The verbs resourceNames cannot express, as their own rule.
				wantRule("batch", "jobs", nil, "create", "list", "watch"),
			},
		},
		{
			name: "class carries an AgentUI grant: agentuis is pinned to that ref",
			sess: shapeSession(nil, nil),
			ac: func() *spiceboxv1alpha1.AgentClass {
				ac := inlineClass()
				ac.Spec.AgentUI = &spiceboxv1alpha1.AgentClassUIGrant{Ref: "ui-a"}
				return ac
			}(),
			extra: []string{wantRule(rbacShapesGroup, "agentuis", []string{"ui-a"}, "get")},
		},
		{
			name: "AgentUI grant with an empty ref: no agentuis rule, since there is nothing to pin",
			sess: shapeSession(nil, nil),
			ac: func() *spiceboxv1alpha1.AgentClass {
				ac := inlineClass()
				ac.Spec.AgentUI = &spiceboxv1alpha1.AgentClassUIGrant{}
				return ac
			}(),
		},
		{
			// Mirrors the fixture the SAR harness applies, so the two stay in
			// step: if a conditional rule is added, this case fails here and the
			// integration harness's required-access table fails there.
			name: "maximal shape: every conditional rule is emitted at once",
			sess: shapeSession(binding("ch-in", "slack"), binding("ch-out", "slack")),
			ac: func() *spiceboxv1alpha1.AgentClass {
				ac := inlineClass()
				ac.Spec.SystemPrompt = spiceboxv1alpha1.PromptSource{
					ConfigMapRef: &spiceboxv1alpha1.ConfigMapKeyRef{Name: "prompt-cm", Key: "prompt"},
				}
				ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{{Name: "up", Ref: "mcp-a"}}
				ac.Spec.SidecarToolboxes = []spiceboxv1alpha1.AgentClassSidecarToolboxRef{{Name: "sc", Ref: "st-a"}}
				ac.Spec.WorkspaceSource = &spiceboxv1alpha1.AgentClassWorkspaceSourceRef{Ref: "ws"}
				ac.Spec.AgentUI = &spiceboxv1alpha1.AgentClassUIGrant{Ref: "ui-a"}
				return ac
			}(),
			bundleSessionNames:   []string{"bundle-a"},
			channelSecretNames:   []string{"ch-in-creds", "ch-out-creds"},
			mcpAgentIdentityName: "tool-identity",
			mcpSecretNames:       []string{"cred-a"},
			extra: []string{
				wantRule("", "configmaps", []string{"prompt-cm"}, "get"),
				wantRule(rbacShapesGroup, "spiceboxsessions", []string{"bundle-a"}, "get", "watch"),
				wantRule(rbacShapesGroup, "mcpservers", []string{"mcp-a"}, "get"),
				wantRule(rbacShapesGroup, "sidecartoolboxes", []string{"st-a"}, "get"),
				wantRule(rbacShapesGroup, "agentuis", []string{"ui-a"}, "get"),
				wantRule(rbacShapesGroup, "agentidentities", []string{"tool-identity"}, "get"),
				wantRule("", "secrets", []string{"cred-a"}, "get"),
				wantRule("batch", "jobs", []string{"ws-sync-shape-session", "ws-apply-shape-session"}, "get", "delete"),
				// The verbs resourceNames cannot express, as their own rule.
				wantRule("batch", "jobs", nil, "create", "list", "watch"),
				wantRule(rbacShapesGroup, "channels", []string{"ch-in", "ch-out"}, "get"),
				wantRule("", "secrets", []string{"ch-in-creds", "ch-out-creds"}, "get"),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, role, _, _, _ := agentsession.BuildRunnerRBAC(
				tc.sess, tc.ac, "tok", tc.bundleSessionNames, tc.channelSecretNames,
				"", "", "", "", "", tc.mcpAgentIdentityName, tc.mcpSecretNames,
			)
			require.NotNil(t, role, "BuildRunnerRBAC must always return a Role")

			want := append(baseRuleSet(tc.sess.Name, tc.sess.Spec.Class), tc.extra...)
			sort.Strings(want)
			assert.Equal(t, want, canonRules(role.Rules),
				"the emitted rule set must be exactly the rules this shape calls for")

			// The apiserver rejects an empty string in resourceNames, and a rule
			// carrying one is dead weight at best.
			for _, r := range role.Rules {
				assert.NotContains(t, r.ResourceNames, "",
					"rule %s carries an empty resourceName", canonRule(r))
			}
		})
	}
}

// TestBuildRunnerRBACPinsTheChannelForEveryRegisteredKind holds the invariant
// the conjunction bug broke, for every kind rather than for the one that
// exposed it: the `channels get` rule is derived from the session's own
// bindings and does not depend on the Channel referencing a Secret.
//
// Kinds differ in whether their Channel carries a credentialsRef — local,
// browser and bento name a Secret for CRD-shape reasons even though they need
// no credential, and agent carries none at all — so this drives the credential-
// less case, which is the one that fails when the two rules are conjoined.
// The kind list comes from the registry, so a kind added later is covered by
// its blank import.
func TestBuildRunnerRBACPinsTheChannelForEveryRegisteredKind(t *testing.T) {
	kinds := chregistry.All()
	require.NotEmpty(t, kinds, "the channel-kind registry must be populated by the blank imports")

	for _, k := range kinds {
		t.Run(k.Name()+": bound with no Secret still gets `channels get`", func(t *testing.T) {
			chName := "ch-" + k.Name()
			sess := shapeSession(binding(chName, k.Name()), nil)

			_, role, _, _, _ := agentsession.BuildRunnerRBAC(
				sess, inlineClass(), "tok", nil, nil, "", "", "", "", "", "", nil)

			want := append(baseRuleSet(sess.Name, sess.Spec.Class),
				wantRule(rbacShapesGroup, "channels", []string{chName}, "get"))
			sort.Strings(want)
			assert.Equal(t, want, canonRules(role.Rules),
				"a credential-less %s Channel must still be readable by its runner", k.Name())
		})
	}
}
