package agentsession

import (
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// agentSessionSecretMemoryToken is the per-session Secret key holding the
// memory-API bearer token. It is the DURABLE half of the token registry —
// the registry itself is process memory — so this key is what a restarted
// operator re-registers a session from (see reregisterMemoryToken).
const agentSessionSecretMemoryToken = "token"

// agentSessionSecretArgsHashKey is the per-session Secret key holding
// the HMAC key for approval-grant arguments_hash values. Written by
// BuildRunnerRBAC / the reconciler's ensure path; mounted into the
// runner pod as a SubPath file.
const agentSessionSecretArgsHashKey = "args-hash-key"

// agentSessionSecretAuditSigningKey is the per-session Secret key
// holding the Ed25519 seed (hex) that signs append-only audit entries.
const agentSessionSecretAuditSigningKey = "audit-signing-key"

// agentSessionSecretLLMAPIKey is the per-session Secret key holding the LLM API
// token materialized from a model-catalog entry's central token. Mounted into
// the runner pod at /var/run/agent/llm-api-key (SubPath "api-key").
const agentSessionSecretLLMAPIKey = "llm-api-key"

// BuildRunnerRBAC produces the per-session ServiceAccount, Role,
// RoleBinding, memory-token Secret, and the toolspec-reader
// ClusterRoleBinding. The first four are owned by the session for
// cascade delete; the ClusterRoleBinding is cluster-scoped and CANNOT
// have a namespaced owner — the controller creates it explicitly and
// the AgentSession finalizer reaps it by its deterministic name.
//
// memToken is the freshly-generated token bytes (32 random bytes hex-encoded).
// ac is the resolved AgentClass; it is used to conditionally emit the ConfigMap
// RBAC rule only when the system prompt uses a configMapRef.
// bundleSessionNames lists the SpiceboxSession names to pin with get/watch.
// channelSecretNames are the credentials Secrets of every Channel this
// session speaks through. BuildRunnerRBAC pins `get` on each — INDEPENDENTLY
// of the `get` it pins on every bound Channel CR (input AND output), which is
// derived from the session's own bindings and needs no Secret to exist.
//
// Both bindings must be granted: resolve.ForSession prefers OutputChannel, so
// a Role pinned only to the input Channel makes that read forbidden and the
// runner silently loses every channel-sourced capability (mention lookup, the
// info-leakage gate). The output Channel's Secret carries the bot token the
// directory API is actually called with. An empty channelSecretNames means
// only that no bound Channel references a Secret — a kind=agent Channel
// carries no credentialsRef at all — and suppresses only the secrets rule.
// natsCreds, when non-empty, is the per-session decorated NATS creds file
// (user JWT + seed); it is stored in the returned Secret under the
// "nats.creds" key alongside "token". Empty string omits the key — the
// not-channel-attached case, where the runner needs no NATS credentials.
// spicedbToken, when non-empty, is the SpiceDB preshared gRPC token; it
// is stored in the returned Secret under the "spicedb-token" key so the
// runner can mount it as a file instead of receiving it as a plaintext
// env var. Every runner needs SpiceDB, so the operator passes it for
// every session — empty string only on the throwaway update-path call
// whose Secret is discarded.
// argsHashKey, when non-empty, is the per-session HMAC key for approval
// grant arguments_hash values; it is stored in the returned Secret under
// the "args-hash-key" key. Every runner needs it, so the operator passes
// it for every session — empty string only on the throwaway update-path
// call whose Secret is discarded (the update path ensures the key on the
// live Secret directly).
// auditSigningKey, when non-empty, is the per-session Ed25519 seed (hex)
// that signs append-only audit entries; it is stored in the returned
// Secret under the "audit-signing-key" key. Every runner needs it, so
// the operator passes it on first reconcile — empty string only on the
// throwaway update-path call whose Secret is discarded (the update path
// ensures the key on the live Secret directly).
// mcpAgentIdentityName and mcpSecretNames pin the MCP-session credential
// access. When the AgentClass references MCPServers, the runner resolves
// MCP auth from the class's single AgentIdentity (mcpAgentIdentityName)
// whose credentials each carry a Secret name (mcpSecretNames, already
// deduplicated + sorted by the controller). The MCP branch pins the
// agentidentities `get` rule to that one identity name and the secrets
// `get` rule to that name slice. Empty mcpAgentIdentityName → no
// agentidentities MCP rule; empty mcpSecretNames → no secrets MCP rule.
func BuildRunnerRBAC(
	s *spiceboxv1alpha1.AgentSession,
	ac *spiceboxv1alpha1.AgentClass,
	memToken string,
	bundleSessionNames []string,
	channelSecretNames []string,
	natsCreds string,
	spicedbToken string,
	argsHashKey string,
	auditSigningKey string,
	llmAPIKey string,
	mcpAgentIdentityName string,
	mcpSecretNames []string,
) (
	*corev1.ServiceAccount, *rbacv1.Role, *rbacv1.RoleBinding, *corev1.Secret,
	*rbacv1.ClusterRoleBinding,
) {
	owner := sessionOwnerRef(s)
	sa := &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{
			Name: s.Name + "-runner-sa", Namespace: s.Namespace,
			OwnerReferences: owner,
		},
	}
	role := &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{
			Name: s.Name + "-runner", Namespace: s.Namespace,
			OwnerReferences: owner,
		},
		Rules: []rbacv1.PolicyRule{
			// `patch` on the session object itself (not just its status
			// subresource) is what lets the exiting runner stamp the
			// wake-requested-at annotation when a message lands as it idles —
			// without it that turn strands until the user writes again. Pinned to
			// this one session by name: the runner can only ask for its own
			// respawn.
			{
				APIGroups:     []string{"agentprimitives.authzed.com"},
				Resources:     []string{"agentsessions"},
				ResourceNames: []string{s.Name},
				Verbs:         []string{"get", "watch", "patch"},
			},
			{
				APIGroups:     []string{"agentprimitives.authzed.com"},
				Resources:     []string{"agentsessions/status"},
				ResourceNames: []string{s.Name},
				Verbs:         []string{"get", "patch"},
			},
			// SessionUserIdentity is created by the operator under the same
			// name as the AgentSession (see passthrough.go). The runner reads
			// it to resolve which user credential backs each MCPServer call
			// in userPassthrough mode — without this rule the runner aborts
			// with MCPAuthResolutionFailed. Read-only; the operator owns
			// writes.
			{
				APIGroups:     []string{"agentprimitives.authzed.com"},
				Resources:     []string{"sessionuseridentities"},
				ResourceNames: []string{s.Name},
				Verbs:         []string{"get", "watch"},
			},
			{
				APIGroups:     []string{"agentprimitives.authzed.com"},
				Resources:     []string{"agentclasses"},
				ResourceNames: []string{s.Spec.Class},
				Verbs:         []string{"get"},
			},
			// Skills: the runner Lists namespace Skills at startup
			// (resolveSkills in internal/cmd/runner) to resolve AgentClass.Spec.Skills
			// (canonical names) into the Agent Skills prompt section and the
			// load_skill tool body map. Without this rule the List returns
			// forbidden and resolveSkills fail-closes the session
			// (Failed/SkillResolutionFailed) — better than the old silent
			// log-and-skip, but still a session that never starts. Unpinned: it
			// is a namespace-wide List, and Skill CR names are content hashes,
			// not the canonical name matched against, so resourceName pinning
			// would both break List authorization and never match. Skills carry
			// instructions, not secrets — namespace-wide read is acceptable,
			// the same posture as spiceboxtoolspecs. Cluster-scoped ClusterSkills
			// are granted separately via the spicebox-toolspec-reader ClusterRole
			// (a namespaced Role cannot grant a cluster-scoped resource).
			{
				APIGroups: []string{"agentprimitives.authzed.com"},
				Resources: []string{"skills"},
				Verbs:     []string{"get", "list", "watch"},
			},
			// No resourceNames — created dynamically. Future hardening:
			// validating admission webhook that requires the agentsession=<X>
			// label to match the SA's session.
			{
				APIGroups: []string{"agentprimitives.authzed.com"},
				Resources: []string{"toolcalls"},
				Verbs:     []string{"create", "get", "list", "watch", "delete"},
			},
			// ArtifactRender: created by artifact_prepare meta-tool, read by
			// artifact_await and by respond_to_user's attachment validation.
			// Names contain a random suffix → unpinned.
			{
				APIGroups: []string{"agentprimitives.authzed.com"},
				Resources: []string{"artifactrenders"},
				Verbs:     []string{"create", "get", "list", "watch"},
			},
			// CredentialUpdateRequest: created and polled by the
			// request_credential_update meta-tool (pkg/agent/tool/meta/
			// credential_update.go) — List first (reattach to a request already
			// parking this session), then Create, then Get in the poll loop, and
			// on give-up a Get of the request this one COLLAPSED ONTO, to
			// establish whether a human was really asked. That last read is of
			// ANOTHER session's object, which is why the rule must stay unpinned
			// rather than scoped to this session's own requests — the same reason
			// artifactrenders is unpinned, plus one: the name carries a random
			// suffix chosen at Create time, so there is nothing the controller
			// could pin at RBAC-build time. Without this rule EVERY invocation
			// 403s on that first List, which is the whole feature.
			//
			// No `watch`: the meta tool POLLS with Get through the runner's
			// direct, uncached client (internal/cmd/runner wires the same client for
			// CredentialUpdateClient that every other RunnerEnv.Client use gets),
			// so no informer is ever established and the verb was granted to
			// nobody. Unlike the neighbouring rules, whose watch verbs back real
			// caches, this one only widened what a compromised runner could do.
			{
				APIGroups: []string{"agentprimitives.authzed.com"},
				Resources: []string{"credentialupdaterequests"},
				Verbs:     []string{"create", "get", "list"},
			},
			// SubagentRequest: created and polled by the delegate meta-tool
			// (pkg/agent/tool/meta/capability/subagents.go) via the runner's
			// direct, uncached client (internal/cmd/runner wires the same client
			// c for SubagentCreate/SubagentPoll that CredentialUpdateClient
			// gets). No `list`, no `watch`: unlike credentialupdaterequests,
			// delegate never reattaches by List — it polls a single request by
			// the name it just Created, through the direct client, so no
			// informer is ever established and neither verb backs anything.
			// Without this rule every `delegate` call 403s on the Create, which
			// is the whole feature.
			{
				APIGroups: []string{"agentprimitives.authzed.com"},
				Resources: []string{"subagentrequests"},
				Verbs:     []string{"create", "get"},
			},
		},
	}
	// ConfigMap rule is only emitted when the AgentClass system prompt uses
	// configMapRef; resourceNames is pinned to the specific ConfigMap so the
	// runner's SA can't read arbitrary ConfigMaps.
	if ac != nil && ac.Spec.SystemPrompt.ConfigMapRef != nil {
		role.Rules = append(role.Rules, rbacv1.PolicyRule{
			APIGroups:     []string{""},
			Resources:     []string{"configmaps"},
			ResourceNames: []string{ac.Spec.SystemPrompt.ConfigMapRef.Name},
			Verbs:         []string{"get"},
		})
	}
	// Bundle SpiceboxSession rules: pinned get/watch per bundle
	// session. Cluster-scoped spiceboxtoolspecs access is granted
	// separately via the toolspec-reader ClusterRoleBinding below — a
	// namespaced Role cannot grant a cluster-scoped resource.
	if len(bundleSessionNames) > 0 {
		role.Rules = append(role.Rules,
			rbacv1.PolicyRule{
				APIGroups:     []string{"agentprimitives.authzed.com"},
				Resources:     []string{"spiceboxsessions"},
				ResourceNames: bundleSessionNames,
				Verbs:         []string{"get", "watch"},
			},
		)
	}
	// The mcpservers `get` rule is MCP-specific: when the AgentClass references
	// MCPServer CRs, the runner loads each at session start to build the
	// tools/list + tools/call requests. Pinned by name.
	if ac != nil && len(ac.Spec.MCPServers) > 0 {
		mcpServerNames := make([]string, 0, len(ac.Spec.MCPServers))
		for _, ref := range ac.Spec.MCPServers {
			mcpServerNames = append(mcpServerNames, ref.Ref)
		}
		role.Rules = append(role.Rules,
			rbacv1.PolicyRule{
				APIGroups:     []string{"agentprimitives.authzed.com"},
				Resources:     []string{"mcpservers"},
				ResourceNames: mcpServerNames,
				Verbs:         []string{"get"},
			},
		)
	}
	// The sidecartoolboxes `get` rule mirrors the mcpservers one, but for a
	// different reason: the runner gets a sidecar's runtime config from
	// AgentSession.status.resolvedSidecarToolboxes (never the CR) at boot, so this
	// grant exists SOLELY for the per-datum information-leakage gate, which on
	// every tool call Gets the referenced SidecarToolbox to resolve the calling
	// tool's read + destination audience from its toolResourceMap (via the
	// runner's direct client — pkg/agent/runner/leakagewiring). Without it that
	// Get is forbidden, leakagewiring logs "lookup failed; tool may appear
	// unmapped" and returns nil, the tool reads as UNDECLARED, and its data falls
	// to the session-wide coarse floor — so the per-datum gate silently NEVER
	// fires for a sidecar tool. (mcpservers already grants the MCP side of this,
	// incidentally, via the rule above.) It went unnoticed because the feature's
	// only live exercise was the in-process bronze harness, which bypasses this
	// RBAC. Pinned by name; `get` only (a point Get, no informer).
	if ac != nil && len(ac.Spec.SidecarToolboxes) > 0 {
		sidecarNames := make([]string, 0, len(ac.Spec.SidecarToolboxes))
		for _, ref := range ac.Spec.SidecarToolboxes {
			if ref.Ref != "" {
				sidecarNames = append(sidecarNames, ref.Ref)
			}
		}
		if len(sidecarNames) > 0 {
			role.Rules = append(role.Rules,
				rbacv1.PolicyRule{
					APIGroups:     []string{"agentprimitives.authzed.com"},
					Resources:     []string{"sidecartoolboxes"},
					ResourceNames: sidecarNames,
					Verbs:         []string{"get"},
				},
			)
		}
	}
	// The agentuis `get` rule is emitted whenever the AgentClass carries a UI
	// grant (spec.agentUI != nil): internal/cmd/runner fetches the referenced AgentUI
	// CR at session start to read spec.tools — condition (1) of the
	// three-way browser-tool grant it materializes into Loop.AppTools
	// (pkg/web/uigrant.Materialize). Pinned by name; an unpinned rule would let
	// the session's runner SA read every AgentUI in the namespace.
	if ac != nil && ac.Spec.AgentUI != nil && ac.Spec.AgentUI.Ref != "" {
		role.Rules = append(role.Rules,
			rbacv1.PolicyRule{
				APIGroups:     []string{"agentprimitives.authzed.com"},
				Resources:     []string{"agentuis"},
				ResourceNames: []string{ac.Spec.AgentUI.Ref},
				Verbs:         []string{"get"},
			},
		)
	}
	// The agentidentities + secrets `get` rules are emitted whenever the
	// controller resolved a tool identity — for MCP Authorization headers OR a
	// sandbox toolBundle credential projected into the runner (e.g. GITHUB_TOKEN
	// for gh). BOTH are resolved runner-side: the runner reads the named
	// AgentIdentity and its credential-backed Secrets, so the runner SA needs
	// `get` on exactly those. Pinned by name: the controller resolves the
	// class's AgentIdentity (mcpAgentIdentityName) and walks its credentials to
	// collect the deduplicated, sorted Secret names (mcpSecretNames) at
	// RBAC-build time — and only populates them when the class actually
	// references a tool that needs them (see controller.go). An empty identity
	// name emits no agentidentities rule; an empty secret-name slice emits no
	// secrets rule (an AgentIdentity with only non-secret-backed credentials,
	// or no AgentIdentity at all).
	if mcpAgentIdentityName != "" {
		role.Rules = append(role.Rules, rbacv1.PolicyRule{
			APIGroups:     []string{"agentprimitives.authzed.com"},
			Resources:     []string{"agentidentities"},
			ResourceNames: []string{mcpAgentIdentityName},
			Verbs:         []string{"get"},
		})
	}
	if len(mcpSecretNames) > 0 {
		role.Rules = append(role.Rules, rbacv1.PolicyRule{
			APIGroups:     []string{""},
			Resources:     []string{"secrets"},
			ResourceNames: mcpSecretNames,
			Verbs:         []string{"get"},
		})
	}
	// The runner creates + polls + deletes a reconcile Job (workspace
	// sync_workspace/apply_workspace) only when the class binds a
	// WorkspaceSource. Gated so non-workspace agents don't get batch/jobs.
	if ac != nil && ac.Spec.WorkspaceSource != nil {
		// PINNED, and that matters more here than anywhere else in this Role.
		// RBAC cannot constrain a pod template. The workspacejob admission
		// webhook closes this gap for runner-created Jobs by matching the
		// operator-derived session template; it must remain fail-closed.
		//
		// The two names are computed the same way the runner computes them
		// (pkg/agent/tool/meta/workspace_tools.go: "ws-"+op+"-"+session), so
		// pinning costs nothing.
		//
		// SPLIT, because resourceNames cannot authorize every verb and a single
		// pinned rule silently REFUSED the ones it cannot.
		//
		// Kubernetes evaluates resourceNames against the request's name, and a
		// CREATE has no name at admission time — so `create` under a pinned rule
		// is not authorized at all. Folding create into the pin therefore did
		// not tighten the grant, it removed it: the runner could no longer make
		// its workspace Jobs, and nothing said so until an authorization review
		// asked. The same applies to list and watch, which are collection verbs
		// with no single name to match.
		//
		// So: the verbs RBAC CAN pin are pinned, and the ones it cannot are a
		// separate unpinned rule. That is strictly better than one rule that
		// pins nothing usable, and strictly honest about what is still open.
		//
		// CREATE remains unpinned because Kubernetes cannot authorize it with
		// resourceNames. The workspacejob webhook validates the name, owner,
		// image, ServiceAccount, volumes and commands at admission instead.
		role.Rules = append(role.Rules,
			rbacv1.PolicyRule{
				APIGroups:     []string{"batch"},
				Resources:     []string{"jobs"},
				ResourceNames: []string{"ws-sync-" + s.Name, "ws-apply-" + s.Name},
				Verbs:         []string{"get", "delete"},
			},
			rbacv1.PolicyRule{
				APIGroups: []string{"batch"},
				Resources: []string{"jobs"},
				// The verbs resourceNames cannot express. Kept as narrow as the
				// API allows: this session's own Role, in its own namespace,
				// created only when the class binds a WorkspaceSource.
				Verbs: []string{"create", "list", "watch"},
			})
	}
	// Channel-attached sessions: pin runner SA access to the Channel CR
	// (so resolve.ForSession can fetch it) and its credentials Secret (so
	// the kind's LookupUser can talk to the directory API). Both pinned by
	// name; the controller resolved the secret name at RBAC-build time.
	//
	// TWO INDEPENDENT CONDITIONS, deliberately not one. The channels rule is
	// derived from the session's own bindings; the secrets rule from what
	// those Channels happen to reference. Gating the first on the second made
	// a Channel with no credentialsRef take the Channel read down with it — a
	// kind=agent Channel (the surface a conversational delegated child talks
	// over) carries none by design, so its runner got no `channels get` at
	// all, and resolve.ForSession failed with a Forbidden that is warned-about
	// and carried on from, silently disabling every channel-sourced
	// capability. Every other credential-less kind (local, browser, bento)
	// still names a Secret for CRD-shape reasons and so never exposed this.
	if chNames := boundChannelNames(s); len(chNames) > 0 {
		role.Rules = append(role.Rules, rbacv1.PolicyRule{
			APIGroups:     []string{"agentprimitives.authzed.com"},
			Resources:     []string{"channels"},
			ResourceNames: chNames,
			Verbs:         []string{"get"},
		})
	}
	if secNames := dedupeNonEmpty(channelSecretNames); len(secNames) > 0 {
		role.Rules = append(role.Rules, rbacv1.PolicyRule{
			APIGroups:     []string{""},
			Resources:     []string{"secrets"},
			ResourceNames: secNames,
			Verbs:         []string{"get"},
		})
	}
	rb := &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name: s.Name + "-runner", Namespace: s.Namespace,
			OwnerReferences: owner,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name,
		},
		Subjects: []rbacv1.Subject{{
			Kind: "ServiceAccount", Name: sa.Name, Namespace: s.Namespace,
		}},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: MemoryTokenSecretName(s), Namespace: s.Namespace,
			OwnerReferences: owner,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{agentSessionSecretMemoryToken: []byte(memToken)},
	}
	if natsCreds != "" {
		sec.Data["nats.creds"] = []byte(natsCreds)
	}
	if spicedbToken != "" {
		sec.Data["spicedb-token"] = []byte(spicedbToken)
	}
	if argsHashKey != "" {
		sec.Data[agentSessionSecretArgsHashKey] = []byte(argsHashKey)
	}
	if auditSigningKey != "" {
		sec.Data[agentSessionSecretAuditSigningKey] = []byte(auditSigningKey)
	}
	if llmAPIKey != "" {
		sec.Data[agentSessionSecretLLMAPIKey] = []byte(llmAPIKey)
	}
	// Per-session ClusterRoleBinding granting this session's runner SA
	// read access to the cluster-scoped spiceboxtoolspecs CRD via the
	// shared spicebox-toolspec-reader ClusterRole. The name is derived
	// from the AgentSession UID (globally unique) because ClusterRoleBinding
	// names are cluster-scoped and a namespace+name concatenation is
	// collision-prone (dashes are valid in both). It has NO OwnerReferences —
	// a cluster-scoped object cannot be owned by a namespaced AgentSession —
	// so the AgentSession finalizer deletes it explicitly by this same name.
	crb := &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name: ToolspecReaderCRBName(s),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     "spicebox-toolspec-reader",
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      sa.Name,
			Namespace: s.Namespace,
		}},
	}
	return sa, role, rb, sec, crb
}

// ToolspecReaderCRBName returns the cluster-unique name of the per-session
// toolspec-reader ClusterRoleBinding. It is derived from the AgentSession UID
// (not namespace+name) because ClusterRoleBinding names are cluster-scoped and
// must be globally unique: a namespace+name concatenation is collision-prone
// since dashes are valid in both. The UID is unique and stable for the
// session's lifetime, and the finalizer has the same AgentSession object so it
// reaps the exact CRB.
func ToolspecReaderCRBName(s *spiceboxv1alpha1.AgentSession) string {
	return "toolspec-reader-" + string(s.UID)
}

// boundChannelNames returns every Channel CR this session speaks through:
// its input binding and, when it differs, its output binding.
//
// Both are required. resolve.ForSession prefers OutputChannel, so a Role
// pinned only to the input Channel makes that read forbidden — the runner
// then loses every channel-sourced capability (mention lookup is not
// injected, the info-leakage gate degrades to unsupported) with only a Warn
// in its log.
func boundChannelNames(s *spiceboxv1alpha1.AgentSession) []string {
	var names []string
	if s.Spec.InputChannel != nil {
		names = append(names, s.Spec.InputChannel.Name)
	}
	if s.Spec.OutputChannel != nil {
		names = append(names, s.Spec.OutputChannel.Name)
	}
	return dedupeNonEmpty(names)
}

// dedupeNonEmpty returns in's unique non-empty entries, order-preserving.
// RBAC resourceNames must not carry duplicates or an empty string.
func dedupeNonEmpty(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
