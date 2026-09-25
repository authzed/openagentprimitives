package agentsession_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
)

// TestBuildRunnerRBACGrantsSelfPatchForWake pins the grant the runner's
// post-Idle respawn request depends on. The runner writes the wake
// annotation on its OWN AgentSession — metadata, not the status subresource
// it already patches — so without `patch` on agentsessions the request 403s
// and a message that landed as the runner idled strands until the user writes
// again. Pinned by resourceName: the runner can ask for its own respawn and
// nothing else.
func TestBuildRunnerRBACGrantsSelfPatchForWake(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", UID: "uid-1"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac1"},
	}
	_, role, _, _, _ := agentsession.BuildRunnerRBAC(sess, nil, "tok", nil, nil, "", "", "", "", "", "", nil)

	var found *rbacv1.PolicyRule
	for i := range role.Rules {
		r := &role.Rules[i]
		if len(r.Resources) == 1 && r.Resources[0] == "agentsessions" {
			found = r
			break
		}
	}
	require.NotNil(t, found, "per-session Role must carry an agentsessions rule")
	assert.Contains(t, found.Verbs, "patch", "runner must be able to stamp its own wake annotation")
	assert.Equal(t, []string{"s1"}, found.ResourceNames, "the patch grant must be pinned to this session")
}

func TestBuildRunnerSA(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", UID: "uid-1"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac1"},
	}
	// AgentClass with an inline system prompt — no ConfigMap rule expected.
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
		},
	}
	sa, role, rb, sec, _ := agentsession.BuildRunnerRBAC(sess, ac, "tok-bytes-32", nil, nil, "", "", "", "", "", "", nil)
	assert.Equal(t, "s1-runner-sa", sa.Name, "ServiceAccount name")
	assert.Equal(t, "s1-runner", role.Name, "Role name")
	assert.Equal(t, role.Name, rb.RoleRef.Name, "RoleBinding RoleRef.Name")
	assert.Equal(t, "s1-memory-token", sec.Name, "memory-token Secret name")
	assert.Equal(t, "tok-bytes-32", string(sec.Data["token"]), "memory-token data")
	// Owner ref propagates, with BlockOwnerDeletion set.
	for _, obj := range []metav1.Object{sa, role, rb, sec} {
		refs := obj.GetOwnerReferences()
		require.Lenf(t, refs, 1, "owner ref missing on %T", obj)
		assert.Equalf(t, sess.UID, refs[0].UID, "owner UID on %T", obj)
		require.NotNilf(t, refs[0].BlockOwnerDeletion, "BlockOwnerDeletion not set on %T", obj)
		assert.Truef(t, *refs[0].BlockOwnerDeletion, "BlockOwnerDeletion=true on %T", obj)
	}
	// Sanity-check the role rule list contains the expected resources.
	roleStr := strings.Join(formatRules(role.Rules), "|")
	for _, r := range []string{"agentsessions", "agentclasses", "toolcalls", "artifactrenders", "skills"} {
		assert.Containsf(t, roleStr, r, "rule list missing %q", r)
	}
	// Inline prompt: no configmaps rule.
	assert.NotContains(t, roleStr, "configmaps",
		"configmaps rule should not be present for inline prompt")
}

// TestBuildRunnerRBAC_HasSessionUserIdentityRule pins the contract
// that bit a real user: in userPassthrough mode the runner reads its
// session's SessionUserIdentity to resolve which user credential
// backs each MCPServer call. Without a Role rule granting get on
// the SUI, the runner aborts with phase=Failed reason=MCP
// AuthResolutionFailed and the chat session goes silent.
//
// The rule must:
//   - be resourceName-scoped to the session's own name (same name
//     convention as the AgentSession the SUI is owned by) so a
//     compromised runner can't read other users' SUIs
//   - include at least get + watch — watch is what controller-
//     runtime informers need to react to MissingCredentials
//     transitioning to empty
func TestBuildRunnerRBAC_HasSessionUserIdentityRule(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-x", Namespace: "default", UID: "uid-x"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac"},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
		},
	}
	_, role, _, _, _ := agentsession.BuildRunnerRBAC(sess, ac, "tok", nil, nil, "", "", "", "", "", "", nil)

	var suiRule *rbacv1.PolicyRule
	for i := range role.Rules {
		for _, res := range role.Rules[i].Resources {
			if res == "sessionuseridentities" {
				suiRule = &role.Rules[i]
				break
			}
		}
		if suiRule != nil {
			break
		}
	}
	require.NotNil(t, suiRule,
		"runner Role MUST contain a rule for sessionuseridentities; the runner reads its own SUI to resolve MCP auth in userPassthrough mode. Missing this rule causes phase=Failed/MCPAuthResolutionFailed.")
	assert.Equal(t, []string{"agentprimitives.authzed.com"}, suiRule.APIGroups)
	assert.Equal(t, []string{sess.Name}, suiRule.ResourceNames,
		"SUI rule must be resourceName-scoped to the session's own name to prevent cross-session reads")
	assert.Contains(t, suiRule.Verbs, "get", "runner needs get on its SUI")
	assert.NotContains(t, suiRule.Verbs, "create",
		"runner should NOT be able to create SUIs — the operator owns writes")
	assert.NotContains(t, suiRule.Verbs, "update",
		"runner should NOT be able to update SUIs — the operator owns writes")
	assert.NotContains(t, suiRule.Verbs, "patch",
		"runner should NOT be able to patch SUIs — the operator owns writes")
	assert.NotContains(t, suiRule.Verbs, "delete",
		"runner should NOT be able to delete SUIs")
}

// TestBuildRunnerRBAC_HasSkillsRule pins the contract that bit a real
// session: the runner Lists namespace Skills at startup (resolveSkills in
// internal/cmd/runner) to resolve AgentClass.Spec.Skills into the Agent Skills prompt
// section AND the load_skill tool body map. Without a Role rule granting
// list on skills, the List returns forbidden, resolveSkills logs + skips the
// skill, and the agent silently loses load_skill — the skill never reaches
// the LLM even though its CR is Valid and the AgentClass opts into it.
//
// The rule must:
//   - be unpinned: resolveSkills does a namespace-wide List, and Skill CR
//     names are content hashes, not the canonical name matched against, so
//     resourceName pinning would both break List authorization and never match.
//   - include list — the one verb resolveSkills actually exercises.
//
// Cluster-scoped ClusterSkills are granted SEPARATELY via the shared
// spicebox-toolspec-reader ClusterRole (a namespaced Role cannot grant a
// cluster-scoped resource), exactly mirroring spiceboxtoolspecs.
func TestBuildRunnerRBAC_HasSkillsRule(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-sk", Namespace: "default", UID: "uid-sk"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac"},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
		},
	}
	_, role, _, _, _ := agentsession.BuildRunnerRBAC(sess, ac, "tok", nil, nil, "", "", "", "", "", "", nil)

	skillsRule := findRuleForResource(role.Rules, "agentprimitives.authzed.com", "skills")
	require.NotNil(t, skillsRule,
		"runner Role MUST contain a rule for skills; the runner Lists namespace Skills to resolve AgentClass.Spec.Skills into load_skill + the Agent Skills prompt section. Missing it silently drops the agent's skills.")
	assert.Contains(t, skillsRule.Verbs, "list",
		"skills rule must grant list — resolveSkills does a namespace-wide List, not a get-by-name")
	assert.Empty(t, skillsRule.ResourceNames,
		"skills rule must be unpinned: resolveSkills Lists all skills (Skill CR names are content hashes, not the canonical name matched against)")

	// clusterskills is cluster-scoped → granted via the spicebox-toolspec-reader
	// ClusterRole, never the namespaced Role (same as spiceboxtoolspecs).
	assert.Nil(t, findRuleForResource(role.Rules, "agentprimitives.authzed.com", "clusterskills"),
		"namespaced Role must NOT contain a clusterskills rule (dead rule — cluster-scoped; granted via the ClusterRole)")
}

// TestBuildRunnerRBAC_GrantsSidecarAndMCPReadsForLeakGate pins the RBAC the
// per-datum information-leakage gate needs. On every tool call the gate does a
// point Get of the referenced SidecarToolbox / MCPServer CR to resolve the
// calling tool's toolResourceMap (its read + destination audience). Without
// `get` on those CRs the read is forbidden, the tool reads as UNDECLARED, and
// its data falls to the session-wide coarse floor — so the per-datum gate
// silently never fires. Pinned to the referenced CRs; get only.
func TestBuildRunnerRBAC_GrantsSidecarAndMCPReadsForLeakGate(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-lg", Namespace: "default", UID: "uid-lg"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac-lg"},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac-lg", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			SystemPrompt:     spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
			SidecarToolboxes: []spiceboxv1alpha1.AgentClassSidecarToolboxRef{{Name: "pde", Ref: "pde-records"}},
			MCPServers:       []spiceboxv1alpha1.AgentClassMCPServerRef{{Ref: "linear"}},
		},
	}
	_, role, _, _, _ := agentsession.BuildRunnerRBAC(sess, ac, "tok", nil, nil, "", "", "", "", "", "", nil)

	stRule := findRuleForResource(role.Rules, "agentprimitives.authzed.com", "sidecartoolboxes")
	require.NotNil(t, stRule,
		"runner Role MUST grant sidecartoolboxes read — the leak gate Gets the CR to resolve the tool's audience; without it every sidecar tool floors to the coarse audience and the per-datum gate never fires")
	assert.Equal(t, []string{"get"}, stRule.Verbs, "point Get via the direct client; get only")
	assert.Equal(t, []string{"pde-records"}, stRule.ResourceNames, "pinned to the referenced SidecarToolbox")

	mcpRule := findRuleForResource(role.Rules, "agentprimitives.authzed.com", "mcpservers")
	require.NotNil(t, mcpRule, "runner Role MUST grant mcpservers read for the same leak-gate reason")
	assert.Equal(t, []string{"get"}, mcpRule.Verbs)
	assert.Equal(t, []string{"linear"}, mcpRule.ResourceNames)
}

// TestBuildRunnerRBAC_NoSidecarRuleWithoutRefs keeps the grant scoped: a class
// referencing neither kind gets neither rule.
func TestBuildRunnerRBAC_NoSidecarRuleWithoutRefs(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-none", Namespace: "default", UID: "uid-none"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac-none"},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac-none", Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentClassSpec{SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "x"}},
	}
	_, role, _, _, _ := agentsession.BuildRunnerRBAC(sess, ac, "tok", nil, nil, "", "", "", "", "", "", nil)
	assert.Nil(t, findRuleForResource(role.Rules, "agentprimitives.authzed.com", "sidecartoolboxes"),
		"no SidecarToolbox refs → no sidecartoolboxes rule (least privilege)")
	assert.Nil(t, findRuleForResource(role.Rules, "agentprimitives.authzed.com", "mcpservers"),
		"no MCPServer refs → no mcpservers rule")
}

func TestBuildRunnerSAWithConfigMapRef(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s2", Namespace: "default", UID: "uid-2"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac2"},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac2", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			SystemPrompt: spiceboxv1alpha1.PromptSource{
				ConfigMapRef: &spiceboxv1alpha1.ConfigMapKeyRef{Name: "my-prompt", Key: "prompt"},
			},
		},
	}
	_, role, _, _, _ := agentsession.BuildRunnerRBAC(sess, ac, "tok-bytes-32", nil, nil, "", "", "", "", "", "", nil)
	// ConfigMapRef set: configmaps rule must be present with correct resourceNames.
	var cmRule *rbacv1.PolicyRule
	for i := range role.Rules {
		for _, res := range role.Rules[i].Resources {
			if res == "configmaps" {
				cmRule = &role.Rules[i]
				break
			}
		}
	}
	require.NotNil(t, cmRule, "configmaps rule should be present when ConfigMapRef is set")
	assert.Equal(t, []string{"my-prompt"}, cmRule.ResourceNames, "configmaps rule pinned to the referenced name")
	assert.Equal(t, []string{"get"}, cmRule.Verbs, "configmaps rule verbs")
}

func TestBuildRunnerRBACWithBundles(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", UID: "uid"},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "x"}},
	}
	_, role, _, _, _ := agentsession.BuildRunnerRBAC(sess, ac, "tok",
		[]string{"s1-code", "s1-notify"}, nil, "", "", "", "", "", "", nil)

	foundBundleSessions := false
	foundToolspecs := false
	for _, rule := range role.Rules {
		for _, res := range rule.Resources {
			if res == "spiceboxsessions" && len(rule.ResourceNames) == 2 {
				foundBundleSessions = true
			}
			if res == "spiceboxtoolspecs" {
				foundToolspecs = true
			}
		}
	}
	assert.True(t, foundBundleSessions, "Role missing spiceboxsessions rule with pinned resourceNames")
	// spiceboxtoolspecs is a cluster-scoped CRD; access is granted via the
	// toolspec-reader ClusterRoleBinding, never via the namespaced Role.
	assert.False(t, foundToolspecs,
		"namespaced Role must NOT contain a spiceboxtoolspecs rule (dead rule — Role cannot grant cluster-scoped resources)")
}

// TestBuildRunnerRBACClusterRoleBinding asserts the per-session
// toolspec-reader ClusterRoleBinding: UID-based name, RoleRef to the shared
// spicebox-toolspec-reader ClusterRole, exactly one subject (the runner SA
// with the right name + namespace), and no OwnerReferences (a cluster-scoped
// object cannot be owned by a namespaced AgentSession).
func TestBuildRunnerRBACClusterRoleBinding(t *testing.T) {
	uid := types.UID("uid-1")
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "team-a", UID: uid},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac1"},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
		},
	}

	sa, _, _, _, crb := agentsession.BuildRunnerRBAC(sess, ac, "tok-bytes-32", nil, nil, "", "", "", "", "", "", nil)
	require.NotNil(t, crb, "BuildRunnerRBAC must return a ClusterRoleBinding")

	assert.Equal(t, "toolspec-reader-"+string(uid), crb.Name,
		"ClusterRoleBinding name must be derived from AgentSession UID for global uniqueness")
	assert.Equal(t, agentsession.ToolspecReaderCRBName(sess), crb.Name,
		"ClusterRoleBinding name must match the deterministic helper used by the finalizer")

	assert.Equal(t, "ClusterRole", crb.RoleRef.Kind, "RoleRef.Kind")
	assert.Equal(t, "spicebox-toolspec-reader", crb.RoleRef.Name, "RoleRef.Name")
	assert.Equal(t, rbacv1.GroupName, crb.RoleRef.APIGroup, "RoleRef.APIGroup")

	require.Len(t, crb.Subjects, 1, "ClusterRoleBinding must bind exactly one subject")
	assert.Equal(t, "ServiceAccount", crb.Subjects[0].Kind, "subject Kind")
	assert.Equal(t, sa.Name, crb.Subjects[0].Name, "subject must be the per-session runner SA")
	assert.Equal(t, "team-a", crb.Subjects[0].Namespace, "subject SA namespace")

	assert.Empty(t, crb.OwnerReferences,
		"ClusterRoleBinding must have no OwnerReferences — a cluster-scoped object cannot be owned by a namespaced AgentSession")
}

// TestToolspecReaderCRBNameCollisionRegression is a regression test for the
// namespace+name concatenation collision: {ns:"team-a", name:"b"} and
// {ns:"team", name:"a-b"} both produce "team-a-b" with a single-dash
// separator. The UID-based naming scheme must produce distinct names for
// sessions with distinct UIDs regardless of namespace+name overlap.
func TestToolspecReaderCRBNameCollisionRegression(t *testing.T) {
	sessA := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "team-a", UID: "u1"},
	}
	sessB := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "a-b", Namespace: "team", UID: "u2"},
	}
	nameA := agentsession.ToolspecReaderCRBName(sessA)
	nameB := agentsession.ToolspecReaderCRBName(sessB)
	assert.NotEqual(t, nameA, nameB,
		"sessions with colliding namespace+name concatenations but distinct UIDs must produce distinct CRB names; got %q for both", nameA)
	assert.Equal(t, "toolspec-reader-u1", nameA, "CRB name for sessA")
	assert.Equal(t, "toolspec-reader-u2", nameB, "CRB name for sessB")
}

func TestBuildRunnerRBACChannelAttachedPinsChannelAndSecret(t *testing.T) {
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "sess1"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:        "ac",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "ch1", Kind: "slack"},
		},
	}
	ac := &spiceboxv1alpha1.AgentClass{}

	_, role, _, _, _ := agentsession.BuildRunnerRBAC(s, ac, "tok", nil, []string{"ch1-creds"}, "", "", "", "", "", "", nil)

	assert.True(t,
		hasPinnedRule(role.Rules, "agentprimitives.authzed.com", "channels", "ch1", "get"),
		"missing channels[ch1] get rule; rules=%+v", role.Rules)
	assert.True(t,
		hasPinnedRule(role.Rules, "", "secrets", "ch1-creds", "get"),
		"missing secrets[ch1-creds] get rule; rules=%+v", role.Rules)
}

// TestBuildRunnerRBACCredentiallessChannelStillPinsChannel pins the half of
// the rule above that has no Secret to go with it.
//
// A conversational delegated child is bound to a Channel of kind `agent`,
// which the SubagentRequest reconciler creates with NO credentialsRef: its
// counterparty is another AgentSession in this cluster, so there is nothing to
// hold a credential for. That made channelSecretNames empty, and while the two
// rules shared one condition the empty Secret list took the CHANNEL read down
// with it — the child's runner got no `channels get`, resolve.ForSession
// returned Forbidden, and every channel-sourced capability was disabled behind
// a single Warn line. The secrets rule must still be absent: there is no
// Secret to name, and RBAC resourceNames may not carry an empty string.
func TestBuildRunnerRBACCredentiallessChannelStillPinsChannel(t *testing.T) {
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "req1-child"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:        "ac",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "req1-inbox", Kind: "agent"},
		},
	}

	_, role, _, _, _ := agentsession.BuildRunnerRBAC(s, &spiceboxv1alpha1.AgentClass{},
		"tok", nil, nil /* no channel Secret */, "", "", "", "", "", "", nil)

	assert.True(t,
		hasPinnedRule(role.Rules, "agentprimitives.authzed.com", "channels", "req1-inbox", "get"),
		"a Channel with no credentialsRef must still be readable by its runner; rules=%+v", role.Rules)
	assert.Nil(t, findRuleForResource(role.Rules, "", "secrets"),
		"no channel Secret exists, so no secrets rule may be emitted; rules=%+v", role.Rules)
}

func TestBuildRunnerRBACKubectlSessionNoChannelRules(t *testing.T) {
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "sess1"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac"}, // no Channel
	}
	ac := &spiceboxv1alpha1.AgentClass{}

	_, role, _, _, _ := agentsession.BuildRunnerRBAC(s, ac, "tok", nil, nil, "", "", "", "", "", "", nil)

	for _, rule := range role.Rules {
		for _, res := range rule.Resources {
			assert.NotEqualf(t, "channels", res,
				"unexpected channels rule on kubectl session: %+v", rule)
		}
	}
}

// TestBuildRunnerRBACSecretCarriesNATSCreds asserts the per-session
// Secret carries the minted NATS creds under "nats.creds" when present,
// always retains "token", and omits "nats.creds" when no creds were
// minted (the not-channel-attached case).
func TestBuildRunnerRBACSecretCarriesNATSCreds(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", UID: "uid-1"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac1"},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
		},
	}

	cases := []struct {
		name      string
		natsCreds string
		wantCreds bool
	}{
		{name: "non-empty creds: Secret carries nats.creds", natsCreds: "-----BEGIN NATS USER JWT-----\nfake\n", wantCreds: true},
		{name: "empty creds: nats.creds key absent", natsCreds: "", wantCreds: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, sec, _ := agentsession.BuildRunnerRBAC(sess, ac, "mem-token-bytes", nil, nil, tc.natsCreds, "", "", "", "", "", nil)
			require.NotNil(t, sec.Data, "Secret Data must be initialized")
			assert.Equal(t, "mem-token-bytes", string(sec.Data["token"]), "token must always be present")
			if tc.wantCreds {
				assert.Equal(t, tc.natsCreds, string(sec.Data["nats.creds"]), "nats.creds must match minted creds")
			} else {
				_, ok := sec.Data["nats.creds"]
				assert.False(t, ok, "nats.creds must be absent when no creds minted")
			}
		})
	}
}

// TestBuildRunnerRBACSecretCarriesSpiceDBToken asserts the per-session
// Secret carries the SpiceDB preshared token under "spicedb-token" when
// a non-empty token is passed, and omits the key when it is empty. The
// runner mounts this key as a file rather than receiving it as a
// plaintext env var.
func TestBuildRunnerRBACSecretCarriesSpiceDBToken(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", UID: "uid-1"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac1"},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
		},
	}

	cases := []struct {
		name         string
		spicedbToken string
		wantToken    bool
	}{
		{name: "non-empty token: Secret carries spicedb-token", spicedbToken: "preshared-spicedb-token", wantToken: true},
		{name: "empty token: spicedb-token key absent", spicedbToken: "", wantToken: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, sec, _ := agentsession.BuildRunnerRBAC(sess, ac, "mem-token-bytes", nil, nil, "", tc.spicedbToken, "", "", "", "", nil)
			require.NotNil(t, sec.Data, "Secret Data must be initialized")
			assert.Equal(t, "mem-token-bytes", string(sec.Data["token"]), "token must always be present")
			if tc.wantToken {
				assert.Equal(t, tc.spicedbToken, string(sec.Data["spicedb-token"]), "spicedb-token must match the passed token")
			} else {
				_, ok := sec.Data["spicedb-token"]
				assert.False(t, ok, "spicedb-token must be absent when no token passed")
			}
		})
	}
}

// TestBuildRunnerRBACMCPPinsIdentityAndSecrets covers the MCP branch's
// pinned agentidentities/secrets rules. The runner resolves MCP auth from
// the AgentClass's single AgentIdentity; both the identity rule and the
// secrets rule must be pinned by name (no namespace-wide get).
func TestBuildRunnerRBACMCPPinsIdentityAndSecrets(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", UID: "uid-1"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac1"},
	}
	mcpClass := func() *spiceboxv1alpha1.AgentClass {
		return &spiceboxv1alpha1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"},
			Spec: spiceboxv1alpha1.AgentClassSpec{
				SystemPrompt:  spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
				AgentIdentity: "id1",
				MCPServers: []spiceboxv1alpha1.AgentClassMCPServerRef{
					{Name: "hubspot", Ref: "hubspot-mcp"},
				},
			},
		}
	}
	nonMCPClass := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
		},
	}

	cases := []struct {
		name             string
		ac               *spiceboxv1alpha1.AgentClass
		mcpIdentityName  string
		mcpSecretNames   []string
		wantIdentityRule bool   // an agentidentities get rule present
		wantIdentityName string // pinned resourceName when wantIdentityRule
		wantSecretsRule  bool   // a secrets get rule present
		wantSecretNames  []string
	}{
		{
			name:             "MCP class + identity + two secret-backed creds: both rules pinned",
			ac:               mcpClass(),
			mcpIdentityName:  "id1",
			mcpSecretNames:   []string{"hubspot-static", "hubspot-oauth"},
			wantIdentityRule: true,
			wantIdentityName: "id1",
			wantSecretsRule:  true,
			wantSecretNames:  []string{"hubspot-static", "hubspot-oauth"},
		},
		{
			name:             "MCP class + identity but no secret-backed creds: identity pinned, no secrets rule",
			ac:               mcpClass(),
			mcpIdentityName:  "id1",
			mcpSecretNames:   nil,
			wantIdentityRule: true,
			wantIdentityName: "id1",
			wantSecretsRule:  false,
		},
		{
			name:             "non-MCP class: neither agentidentities nor secrets MCP rule",
			ac:               nonMCPClass,
			mcpIdentityName:  "",
			mcpSecretNames:   nil,
			wantIdentityRule: false,
			wantSecretsRule:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, role, _, _, _ := agentsession.BuildRunnerRBAC(
				sess, tc.ac, "tok", nil, nil, "", "", "", "", "", tc.mcpIdentityName, tc.mcpSecretNames)

			idRule := findRuleForResource(role.Rules, "agentprimitives.authzed.com", "agentidentities")
			if tc.wantIdentityRule {
				require.NotNil(t, idRule, "agentidentities rule must be present")
				assert.Equal(t, []string{tc.wantIdentityName}, idRule.ResourceNames,
					"agentidentities rule must be pinned to the class's AgentIdentity")
				assert.Equal(t, []string{"get"}, idRule.Verbs, "agentidentities rule verbs")
			} else {
				assert.Nil(t, idRule, "no agentidentities rule expected; rules=%+v", role.Rules)
			}

			secRule := findRuleForResource(role.Rules, "", "secrets")
			if tc.wantSecretsRule {
				require.NotNil(t, secRule, "secrets rule must be present")
				assert.ElementsMatch(t, tc.wantSecretNames, secRule.ResourceNames,
					"secrets rule must be pinned to the collected Secret names")
				assert.Equal(t, []string{"get"}, secRule.Verbs, "secrets rule verbs")
			} else {
				assert.Nil(t, secRule,
					"no secrets rule expected (no channel secret, empty MCP secret slice); rules=%+v", role.Rules)
			}
		})
	}
}

// TestBuildRunnerRBAC_AgentUIGatesAgentUIsRule pins the contract that the
// runner SA only gets `get` on the referenced AgentUI CR — pinned by name —
// when the AgentClass carries a UI grant (spec.agentUI != nil). Without this
// rule, internal/cmd/runner's resolution of AgentUI.spec.tools (condition 1 of the
// three-way browser-tool grant it materializes into Loop.AppTools) 403s and
// the grant silently fails closed to "no AgentUI resolvable" for every
// session of a class that legitimately configured one.
func TestBuildRunnerRBAC_AgentUIGatesAgentUIsRule(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", UID: "uid-1"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac1"},
	}
	uiClass := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
			AgentUI: &spiceboxv1alpha1.AgentClassUIGrant{
				Ref:          "widget-ui",
				GrantedTools: []string{"widgets_list_items"},
			},
		},
	}
	noUIClass := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
		},
	}
	// emptyRefClass exercises the Spec.AgentUI != nil && Ref == "" guard
	// branch directly — Spec.AgentUI is non-nil, but nothing should be
	// pinned to an empty resourceName (which would be a no-op-widening bug,
	// not a real grant).
	emptyRefClass := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
			AgentUI:      &spiceboxv1alpha1.AgentClassUIGrant{Ref: ""},
		},
	}

	cases := []struct {
		name           string
		ac             *spiceboxv1alpha1.AgentClass
		wantRule       bool
		wantResourceNm string
	}{
		{name: "class carries a UI grant: agentuis rule pinned to the ref", ac: uiClass, wantRule: true, wantResourceNm: "widget-ui"},
		{name: "class carries no AgentUI at all: no agentuis rule", ac: noUIClass, wantRule: false},
		{name: "nil AgentClass (throwaway call): no agentuis rule", ac: nil, wantRule: false},
		{name: "AgentUI set but Ref empty: no agentuis rule (guard branch)", ac: emptyRefClass, wantRule: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, role, _, _, _ := agentsession.BuildRunnerRBAC(
				sess, tc.ac, "tok", nil, nil, "", "", "", "", "", "", nil)

			rule := findRuleForResource(role.Rules, "agentprimitives.authzed.com", "agentuis")
			if tc.wantRule {
				require.NotNil(t, rule, "agentuis rule must be present; rules=%+v", role.Rules)
				assert.Equal(t, []string{tc.wantResourceNm}, rule.ResourceNames,
					"agentuis rule must be pinned to EXACTLY the class's AgentUI ref, no more no less")
				assert.Equal(t, []string{"get"}, rule.Verbs, "agentuis rule verbs")
			} else {
				assert.Nil(t, rule, "no agentuis rule expected; rules=%+v", role.Rules)
			}
		})
	}
}

// TestBuildRunnerRBAC_WorkspaceSourceGatesBatchJobsRule pins the contract
// that the runner SA only gets batch/jobs access (needed to create + poll +
// delete the sync_workspace/apply_workspace reconcile Job) when the
// AgentClass actually binds a WorkspaceSource — a non-workspace agent must
// not carry the ability to create arbitrary batch Jobs in its namespace.
func TestBuildRunnerRBAC_WorkspaceSourceGatesBatchJobsRule(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", UID: "uid-1"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac1"},
	}
	baseSpec := spiceboxv1alpha1.AgentClassSpec{
		SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
	}

	cases := []struct {
		name      string
		ac        *spiceboxv1alpha1.AgentClass
		wantRule  bool
		wantVerbs []string
	}{
		{
			name: "WorkspaceSource bound: batch/jobs pinned to this session's two job names",
			ac: &spiceboxv1alpha1.AgentClass{
				ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"},
				Spec: func() spiceboxv1alpha1.AgentClassSpec {
					s := baseSpec
					s.WorkspaceSource = &spiceboxv1alpha1.AgentClassWorkspaceSourceRef{Ref: "demo-source"}
					return s
				}(),
			},
			wantRule: true,
			// The pinned half only. get and delete address one object by name,
			// so resourceNames authorizes them; create/list/watch live in a
			// separate unpinned rule because RBAC cannot express them by name
			// at all — see rbac_jobs_test.go for both halves and for what stays
			// open.
			wantVerbs: []string{"get", "delete"},
		},
		{
			name: "no WorkspaceSource: no batch/jobs rule",
			ac: &spiceboxv1alpha1.AgentClass{
				ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"},
				Spec:       baseSpec,
			},
			wantRule: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, role, _, _, _ := agentsession.BuildRunnerRBAC(
				sess, tc.ac, "tok", nil, nil, "", "", "", "", "", "", nil)

			jobsRule := findPinnedRuleForResource(role.Rules, "batch", "jobs")
			if tc.wantRule {
				require.NotNil(t, jobsRule, "the pinned batch/jobs rule must be present when WorkspaceSource is bound")
				assert.ElementsMatch(t, tc.wantVerbs, jobsRule.Verbs, "pinned batch/jobs rule verbs")
			} else {
				assert.Nil(t, jobsRule, "no batch/jobs rule expected without a bound WorkspaceSource; rules=%+v", role.Rules)
				assert.Nil(t, findRuleForResource(role.Rules, "batch", "jobs"),
					"nor an unpinned one")
			}
		})
	}
}

// findPinnedRuleForResource is findRuleForResource restricted to a rule that
// carries resourceNames. It exists because one resource can legitimately need
// two rules — the verbs RBAC can pin by name, and the ones it cannot — and a
// first-match helper would return whichever happened to be appended first.
func findPinnedRuleForResource(rules []rbacv1.PolicyRule, apiGroup, resource string) *rbacv1.PolicyRule {
	for i := range rules {
		if containsStr(rules[i].APIGroups, apiGroup) && containsStr(rules[i].Resources, resource) &&
			len(rules[i].ResourceNames) > 0 {
			return &rules[i]
		}
	}
	return nil
}

// findRuleForResource returns the first PolicyRule whose APIGroups contains
// apiGroup and Resources contains resource, or nil. Unlike hasPinnedRule it
// returns the rule so callers can assert on ResourceNames/Verbs (including
// the "no resourceNames" / "no rule at all" distinction).
func findRuleForResource(rules []rbacv1.PolicyRule, apiGroup, resource string) *rbacv1.PolicyRule {
	for i := range rules {
		if containsStr(rules[i].APIGroups, apiGroup) && containsStr(rules[i].Resources, resource) {
			return &rules[i]
		}
	}
	return nil
}

// hasPinnedRule searches role.Rules for an exact match: APIGroup, single
// Resource, single ResourceName, single Verb. Tighter than a substring
// match — catches accidentally-unpinned rules.
func hasPinnedRule(rules []rbacv1.PolicyRule, apiGroup, resource, resourceName, verb string) bool {
	for _, r := range rules {
		if !containsStr(r.APIGroups, apiGroup) {
			continue
		}
		if !containsStr(r.Resources, resource) {
			continue
		}
		if !containsStr(r.ResourceNames, resourceName) {
			continue
		}
		if !containsStr(r.Verbs, verb) {
			continue
		}
		return true
	}
	return false
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func formatRules(rules []rbacv1.PolicyRule) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, strings.Join(r.Resources, "+"))
	}
	return out
}

// A cron/split-channel session's runner must be able to read its OUTPUT
// Channel and that Channel's credentials Secret.
//
// resolve.ForSession resolves the channel a session speaks through by
// preferring OutputChannel, so a Role pinned only to the input Channel makes
// that GET forbidden. The runner then logs "channel resolve failed;
// channel-sourced meta tools disabled" and drops every channel-sourced
// capability — lookup_user_for_mention is not injected (so the agent renders
// people as plain text instead of @-mentions) and the info-leakage gate
// degrades to "unsupported".
//
// The secret matters as much as the Channel: the mention lookup calls the
// directory API with the OUTPUT channel's bot token, not the input's.
func TestBuildRunnerRBAC_GrantsOutputChannelAndItsSecret(t *testing.T) {
	s := &spiceboxv1alpha1.AgentSession{}
	s.Namespace, s.Name = "default", "cron-1"
	s.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{Name: "cron-in", Kind: "bento"}
	s.Spec.OutputChannel = &spiceboxv1alpha1.ChannelBinding{Name: "cron-out", Kind: "slack"}
	ac := &spiceboxv1alpha1.AgentClass{}

	_, role, _, _, _ := agentsession.BuildRunnerRBAC(
		s, ac, "tok", nil, []string{"cron-in-creds", "cron-out-creds"},
		"", "", "", "", "", "", nil)

	assert.True(t,
		hasPinnedRule(role.Rules, "agentprimitives.authzed.com", "channels", "cron-out", "get"),
		"runner must be able to GET its OUTPUT channel; rules=%+v", role.Rules)
	assert.True(t,
		hasPinnedRule(role.Rules, "agentprimitives.authzed.com", "channels", "cron-in", "get"),
		"the input channel grant must survive; rules=%+v", role.Rules)
	assert.True(t,
		hasPinnedRule(role.Rules, "", "secrets", "cron-out-creds", "get"),
		"runner must be able to read the OUTPUT channel's credentials; rules=%+v", role.Rules)
}

// Same-channel sessions must be unaffected: one channel, one secret, no
// duplicate resourceNames.
func TestBuildRunnerRBAC_SameChannelSessionUnchanged(t *testing.T) {
	s := &spiceboxv1alpha1.AgentSession{}
	s.Namespace, s.Name = "default", "slack-1"
	s.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{Name: "ch1", Kind: "slack"}
	ac := &spiceboxv1alpha1.AgentClass{}

	_, role, _, _, _ := agentsession.BuildRunnerRBAC(
		s, ac, "tok", nil, []string{"ch1-creds"}, "", "", "", "", "", "", nil)

	assert.True(t,
		hasPinnedRule(role.Rules, "agentprimitives.authzed.com", "channels", "ch1", "get"),
		"rules=%+v", role.Rules)
	assert.True(t,
		hasPinnedRule(role.Rules, "", "secrets", "ch1-creds", "get"),
		"rules=%+v", role.Rules)
}
