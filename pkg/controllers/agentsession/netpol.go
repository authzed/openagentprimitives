// Package-internal NetworkPolicy stamping for per-session pods. The
// builders are pure; the ensure methods server-side-apply their output.
package agentsession

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/cosidecar"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/netpolrule"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
)

// spicedbInClusterService is the in-cluster SpiceDB Service name
// (pkg/platform/manifests/spicedb/). A SPICEDB_ENDPOINT whose hostname's first label is
// this name gets a scoped egress peer rule; any other endpoint is
// treated as external and rides the external-443 rule (nonstandard
// external ports are a documented limitation; use the opt-out flag).
const spicedbInClusterService = "spicebox-spicedb"

// ParseNATSPort extracts the TCP port from a NATS URL like
// nats://host:4222. A URL without an explicit port yields the NATS
// default 4222. The NATS client accepts comma-separated multi-server
// URLs, but --nats-url is a single URL by convention here; a
// comma-separated value fails parsing and the operator exits at startup
// (with stamping enabled) rather than guessing.
func ParseNATSPort(natsURL string) (int32, error) {
	u, err := url.Parse(natsURL)
	if err != nil {
		return 0, fmt.Errorf("parse NATS URL %q: %w", natsURL, err)
	}
	p := u.Port()
	if p == "" {
		if u.Host == "" {
			return 0, fmt.Errorf("parse NATS URL %q: no host", natsURL)
		}
		return 4222, nil
	}
	n, err := strconv.ParseUint(p, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("parse NATS URL port %q: %w", p, err)
	}
	return int32(n), nil
}

// ParseSpiceDBEndpoint splits a SpiceDB gRPC endpoint (host:port, with
// or without a scheme prefix) into its port and whether it targets the
// in-cluster SpiceDB service. A missing port yields the SpiceDB default
// 50051.
func ParseSpiceDBEndpoint(endpoint string) (port int32, inCluster bool, err error) {
	if endpoint == "" {
		return 0, false, fmt.Errorf("empty SpiceDB endpoint")
	}
	hostport := endpoint
	if i := strings.Index(hostport, "://"); i >= 0 {
		hostport = hostport[i+3:]
	}
	host, portStr, splitErr := net.SplitHostPort(hostport)
	if splitErr != nil {
		// No port in the endpoint: the whole string is the host.
		host, portStr = hostport, "50051"
	}
	n, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return 0, false, fmt.Errorf("parse SpiceDB endpoint port %q: %w", portStr, err)
	}
	firstLabel, _, _ := strings.Cut(host, ".")
	return int32(n), firstLabel == spicedbInClusterService, nil
}

// NetpolConfig carries the operator-level inputs for per-session
// NetworkPolicy stamping. Zero value = disabled (tests and the
// in-process e2e harness construct Reconcilers without it; the operator
// wires it from flags/env at startup).
type NetpolConfig struct {
	// Enabled gates all stamping. Wired from --session-network-policies
	// (default true in the operator binary).
	Enabled bool
	// OperatorNamespace is where the control-plane Services (NATS,
	// SpiceDB, operator) live; egress peer rules are scoped to it.
	OperatorNamespace string
	// NATSPort is derived from --nats-url.
	NATSPort int32
	// SpiceDBPort is derived from SPICEDB_ENDPOINT.
	SpiceDBPort int32
	// SpiceDBInCluster is true when SPICEDB_ENDPOINT targets the
	// in-cluster service; false suppresses the SpiceDB peer rule
	// (external endpoints ride the external-443 rule).
	SpiceDBInCluster bool

	// NamespaceDefaultDeny stamps a namespace-wide default-deny into each
	// session's namespace. Wired from --session-namespace-default-deny,
	// DEFAULT OFF, and deliberately independent of Enabled.
	//
	// The whole allowlist model rests on a default-deny, and the one shipped in
	// config/networkpolicy covers the PLATFORM namespace only. Sessions do not
	// live there — each is created in its Channel's namespace, and the built-in
	// webchat uses `default`. NetworkPolicy is an allow-union with no implicit
	// default, so a pod matching no selector is unrestricted in BOTH directions
	// even on a fully enforcing CNI, and the per-session builders all select on
	// session labels. A pod created in that namespace by anything else —
	// including a Job an agent was able to create — therefore rides no rule.
	//
	// Off by default because this is the one policy AP cannot decide alone: an
	// empty podSelector covers EVERY pod in a namespace AP does not own, so in
	// a shared namespace it would sever workloads that have nothing to do with
	// any agent. Turning it on is the cluster operator's call, and it is the
	// only control that covers a pod whose labels its creator chose.
	NamespaceDefaultDeny bool
}

// RunnerNetworkPolicyName returns the runner policy name for a session.
func RunnerNetworkPolicyName(sess *spiceboxv1alpha1.AgentSession) string {
	return sess.Name + "-runner-netpol"
}

// SandboxNetworkPolicyName returns the sandbox policy name for one
// bundle SpiceboxSession.
func SandboxNetworkPolicyName(bundleSessionName string) string {
	return bundleSessionName + "-sandbox-netpol"
}

// spicedbPeer selects the operator-managed SpiceDB pods in the operator
// namespace. The spicedb-operator relabels pods (app.kubernetes.io/name becomes
// spicebox-spicedb-spicedb); the stable, semantic selector is the cluster
// component label.
func spicedbPeer(operatorNamespace string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"kubernetes.io/metadata.name": operatorNamespace},
		},
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"authzed.com/cluster-component": "spicedb"},
		},
	}
}

func sessionOwnerRef(sess *spiceboxv1alpha1.AgentSession) []metav1.OwnerReference {
	return cosidecar.OwnerRef(sess)
}

// modeEgressRules maps a merged network mode to its egress rules: none
// (or empty — fail-closed) yields nil; allowlist yields DNS + coarse
// TCP 443/80 anywhere (hostnames are not expressible in vanilla
// NetworkPolicy; allowed-hosts lists on status remain the source of
// truth for DNS-aware CNIs to narrow further).
// The canonical implementation lives in the cosidecar package.
func modeEgressRules(mode spiceboxv1alpha1.NetworkMode) []networkingv1.NetworkPolicyEgressRule {
	return cosidecar.AllowlistEgressRules(mode)
}

// BuildRunnerNetworkPolicy isolates the session's runner pod: all
// ingress denied (probes and pods/exec are kubelet/apiserver-mediated
// and unaffected), egress restricted to DNS, the control-plane
// services, separate-pod sidecar MCP servers, and external TCP
// 443/6443 (LLM / remote MCP / apiserver — no portable apiserver
// selector exists; see config/networkpolicy/egress-external.yaml for
// the same rationale). In-pod sidecar containers share the runner
// pod's network namespace, so their upstream HTTPS calls ride the
// external rule.
//
// Separate-pod sidecar toolbox pods carry the same
// agentprimitives.authzed.com/agentsession label as the runner pod
// (plus agentprimitives.authzed.com/sidecartoolbox). The policy's
// PodSelector excludes them via a DoesNotExist expression on the
// sidecartoolbox label so that this policy does not deny their ingress.
// A dedicated egress rule allows runner→sidecar traffic on all ports
// (port numbers are allocated dynamically by AllocatePorts and are not
// statically known at policy-build time).
func BuildRunnerNetworkPolicy(sess *spiceboxv1alpha1.AgentSession, cfg NetpolConfig) *networkingv1.NetworkPolicy {
	egress := []networkingv1.NetworkPolicyEgressRule{
		netpolrule.DNSEgress(),
		{
			To:    []networkingv1.NetworkPolicyPeer{netpolrule.ControlPlanePeer(cfg.OperatorNamespace, "spicebox-nats")},
			Ports: []networkingv1.NetworkPolicyPort{netpolrule.Port(corev1.ProtocolTCP, cfg.NATSPort)},
		},
		{
			To: []networkingv1.NetworkPolicyPeer{netpolrule.ControlPlanePeer(cfg.OperatorNamespace, "spicebox-operator")},
			Ports: []networkingv1.NetworkPolicyPort{
				netpolrule.Port(corev1.ProtocolTCP, 8082),
				netpolrule.Port(corev1.ProtocolTCP, 8443),
			},
		},
	}
	if cfg.SpiceDBInCluster {
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{spicedbPeer(cfg.OperatorNamespace)},
			Ports: []networkingv1.NetworkPolicyPort{netpolrule.Port(corev1.ProtocolTCP, cfg.SpiceDBPort)},
		})
	}
	// This session's separate-pod sidecar MCP servers, same namespace
	// (no namespaceSelector). Ports are allocated dynamically
	// (AllocatePorts), so no port list is expressible — all ports, to
	// exactly these pods. Selects nothing when the session has none.
	egress = append(egress, networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"agentprimitives.authzed.com/agentsession": sess.Name},
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key: labelSidecarToolbox, Operator: metav1.LabelSelectorOpExists,
				}},
			},
		}},
	})
	egress = append(egress, networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}},
		Ports: []networkingv1.NetworkPolicyPort{
			netpolrule.Port(corev1.ProtocolTCP, 443),
			netpolrule.Port(corev1.ProtocolTCP, 6443),
		},
	})

	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      RunnerNetworkPolicyName(sess),
			Namespace: sess.Namespace,
			Labels: map[string]string{
				"agentprimitives.authzed.com/agentsession": sess.Name,
			},
			OwnerReferences: sessionOwnerRef(sess),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"agentprimitives.authzed.com/agentsession": sess.Name},
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key: labelSidecarToolbox, Operator: metav1.LabelSelectorOpDoesNotExist,
				}},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Egress:      egress,
		},
	}
}

// netpolFieldOwner is the SSA field manager for stamped policies.
const netpolFieldOwner = "agentsession-netpol"

// ensureRunnerNetworkPolicy stamps (SSA) the runner pod's
// NetworkPolicy. Called before the runner pod and bundle sessions are
// created so an enforcing CNI never sees an unconstrained pod.
func (r *Reconciler) ensureRunnerNetworkPolicy(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error {
	if !r.Netpol.Enabled {
		return nil
	}
	np := BuildRunnerNetworkPolicy(sess, r.Netpol)
	if err := r.Client.Patch(ctx, np,
		client.Apply, client.ForceOwnership, client.FieldOwner(netpolFieldOwner),
	); err != nil {
		return fmt.Errorf("apply runner NetworkPolicy %q: %w", np.Name, err)
	}
	return nil
}

// ensureSandboxNetworkPolicy stamps (SSA) one bundle sandbox pod's
// NetworkPolicy. The mode comes from the bundle's SpiceboxClass
// network; a missing class falls back to none (fail-closed — the
// bundle apply path surfaces the missing class separately).
func (r *Reconciler) ensureSandboxNetworkPolicy(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, bundle spiceboxv1alpha1.ToolBundle) error {
	if !r.Netpol.Enabled {
		return nil
	}
	mode := spiceboxv1alpha1.NetworkModeNone
	var cls spiceboxv1alpha1.SpiceboxClass
	err := r.Client.Get(ctx, types.NamespacedName{Name: bundle.Class}, &cls)
	switch {
	case err == nil:
		if cls.Spec.Network.Mode != "" {
			mode = cls.Spec.Network.Mode
		}
	case apierrors.IsNotFound(err):
		// fall through with mode none
	default:
		return fmt.Errorf("get SpiceboxClass %q for sandbox NetworkPolicy: %w", bundle.Class, err)
	}
	np := BuildSandboxNetworkPolicy(sess, BundleSessionName(sess, bundle), mode)
	if err := r.Client.Patch(ctx, np,
		client.Apply, client.ForceOwnership, client.FieldOwner(netpolFieldOwner),
	); err != nil {
		return fmt.Errorf("apply sandbox NetworkPolicy %q: %w", np.Name, err)
	}
	return nil
}

// BuildSandboxNetworkPolicy isolates one bundle's sandbox pod. Mode
// none (or empty — fail-closed) yields full isolation with zero egress
// rules; tool execution is unaffected because pods/exec does not
// traverse the pod network. Mode allowlist yields DNS + coarse TCP
// 443/80 anywhere: hostnames are not expressible in vanilla
// NetworkPolicy, so effectiveAllowedHosts (recorded on session status)
// remains the source of truth for DNS-aware CNIs to narrow further.
func BuildSandboxNetworkPolicy(sess *spiceboxv1alpha1.AgentSession, bundleSessionName string, mode spiceboxv1alpha1.NetworkMode) *networkingv1.NetworkPolicy {
	egress := modeEgressRules(mode)
	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      SandboxNetworkPolicyName(bundleSessionName),
			Namespace: sess.Namespace,
			Labels: map[string]string{
				"agentprimitives.authzed.com/agentsession": sess.Name,
			},
			OwnerReferences: sessionOwnerRef(sess),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"agentprimitives.authzed.com/session": bundleSessionName},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Egress:      egress,
		},
	}
}

// SidecarNetworkPolicyName returns the policy name for one separate-pod
// sidecar toolbox: "<session>-sidecar-<ref>-netpol".
func SidecarNetworkPolicyName(sess *spiceboxv1alpha1.AgentSession, ref string) string {
	return SidecarPodName(sess, ref) + "-netpol"
}

// BuildSidecarNetworkPolicy isolates one separate-pod sidecar toolbox
// pod. Precondition: rt.Port is the operator-allocated MCP port
// (recorded on AgentSession status by AllocatePorts — it is not
// statically known at admission time).
//
// Ingress is restricted to this session's runner pod (the
// same-namespace pod carrying the agentsession label WITHOUT the
// sidecartoolbox label) on exactly the sidecar's MCP port — without
// this policy any pod in the namespace could exercise the sidecar's
// credentialed upstream access. Kubelet startup probes are
// apiserver/kubelet-mediated and unaffected.
//
// Egress mirrors BuildSandboxNetworkPolicy, driven by the merged
// EffectiveNetworkMode recorded on status: none (or empty —
// fail-closed) yields zero egress rules; allowlist yields DNS + coarse
// TCP 443/80 anywhere. Hostname-level narrowing of
// EffectiveAllowedHosts remains delegated to DNS-aware CNIs.
//
// Delegates the base policy to cosidecar.BuildNetworkPolicy — the single
// canonical netpol builder for all co-sidecar pods — then, when workshop is
// true, appends two more egress rules UNCONDITIONALLY (regardless of
// EffectiveNetworkMode): the operator's control-plane peer on 8082+8443, and
// external TCP 6443 (the apiserver). This is the ONE separate-pod sidecar
// that needs either: the ap-workshop binary reaches the operator's HTTP
// routes with its bearer (OPERATOR_MEMORY_URL) and the Kubernetes apiserver
// with its projected ServiceAccount token
// (/var/run/secrets/workshop/serviceaccount/token) — see
// internal/cmd/workshop/main.go. workshop is true only for the ONE sidecar a
// Ready, session-owned Workshop names (the same signal BuildSidecarPod's
// identity branch uses to inject the projected token); every other sidecar's
// policy is unaffected by this parameter — byte-identical to the
// workshop==false path, which was this function's entire behavior before the
// agent-builder workshop sidecar existed. Mirrors
// BuildRunnerNetworkPolicy's operator + external-6443 peers (netpol.go:183,
// 210) — the runner gets both unconditionally too, for the same reason: an
// apiserver/operator client is not subject to a SpiceboxClass network mode.
func BuildSidecarNetworkPolicy(sess *spiceboxv1alpha1.AgentSession, rt spiceboxv1alpha1.ResolvedSidecarToolbox, cfg NetpolConfig, workshop bool) *networkingv1.NetworkPolicy {
	np := cosidecar.BuildNetworkPolicy(sess, cosidecar.Spec{
		Ref:  rt.Ref,
		Name: SidecarNetworkPolicyName(sess, rt.Ref),
		Port: rt.Port,
		Egress: cosidecar.EgressPolicy{
			AllowlistMode: rt.EffectiveNetworkMode,
			AllowedHosts:  rt.EffectiveAllowedHosts,
		},
	})
	if workshop {
		np.Spec.Egress = append(np.Spec.Egress,
			networkingv1.NetworkPolicyEgressRule{
				To: []networkingv1.NetworkPolicyPeer{netpolrule.ControlPlanePeer(cfg.OperatorNamespace, "spicebox-operator")},
				Ports: []networkingv1.NetworkPolicyPort{
					netpolrule.Port(corev1.ProtocolTCP, 8082),
					netpolrule.Port(corev1.ProtocolTCP, 8443),
				},
			},
			networkingv1.NetworkPolicyEgressRule{
				To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}},
				Ports: []networkingv1.NetworkPolicyPort{netpolrule.Port(corev1.ProtocolTCP, 6443)},
			},
		)
	}
	return np
}

// NamespaceDefaultDenyName is the fixed name of the per-namespace floor. It
// carries no session in it on purpose: every session in a namespace converges
// on ONE object, so N sessions do not stamp N identical policies and the floor
// does not disappear when whichever session happened to create it is deleted.
const NamespaceDefaultDenyName = "ap-session-namespace-default-deny"

// BuildNamespaceDefaultDenyNetworkPolicy returns the namespace-wide floor for
// sess's namespace: an empty podSelector with both policy types and no rules,
// which denies everything not opened by some other policy.
//
// Deliberately NOT owner-referenced to sess. A namespace holds many sessions,
// and an ownerRef would have the first one's deletion garbage-collect the floor
// out from under every other session still running there — a security control
// that comes and goes with unrelated lifecycles is worse than none, because it
// tests green. It is instead a namespace-lifetime object: reaped with the
// namespace, converged by every session that starts in it.
func BuildNamespaceDefaultDenyNetworkPolicy(sess *spiceboxv1alpha1.AgentSession) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NamespaceDefaultDenyName,
			Namespace: sess.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/component": "session-namespace-default-deny",
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
}

// WorkspaceJobNetworkPolicyName is the fixed name of the allow rule that keeps
// this project's own workspace Jobs working under the namespace floor.
//
// Per-namespace like the floor itself, and for the same reason: the Jobs are not
// per-session objects and their policy must outlive whichever session happened
// to stamp it.
const WorkspaceJobNetworkPolicyName = "ap-workspace-job-egress"

// BuildWorkspaceJobNetworkPolicy returns the egress allowance for this
// project's own workspace Job pods (snapshot, restore, garbage-collect,
// overlay-cut, reconcile), selected by the label their pod templates carry.
//
// It exists because the namespace floor would otherwise BREAK them. The floor's
// empty podSelector covers every pod in the namespace, these pods included, and
// with no allow rule naming them the reconcile Job loses DNS and its git remote
// outright. Shipping a security control that silently breaks the platform's own
// workloads the moment it is switched on is worse than not shipping it: the
// operator turns it off again and learns to distrust it.
//
// The allowance is DNS plus outbound TCP 443/22/80, which is what a git remote
// needs over HTTPS or SSH. Ingress is deliberately empty: nothing calls these
// pods, and under the floor an empty ingress rule set means nothing can.
func BuildWorkspaceJobNetworkPolicy(sess *spiceboxv1alpha1.AgentSession) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      WorkspaceJobNetworkPolicyName,
			Namespace: sess.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/component": "workspace-job-egress",
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key: workspace.LabelWorkspaceJob, Operator: metav1.LabelSelectorOpExists,
				}},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				netpolrule.DNSEgress(),
				{
					To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}},
					Ports: []networkingv1.NetworkPolicyPort{
						netpolrule.Port(corev1.ProtocolTCP, 443),
						netpolrule.Port(corev1.ProtocolTCP, 22),
						netpolrule.Port(corev1.ProtocolTCP, 80),
					},
				},
			},
		},
	}
}

// ensureNamespaceDefaultDeny stamps (SSA) the namespace-wide floor when the
// operator asked for it, together with the allow rule this project's own
// workspace Jobs need under it. Gated on NamespaceDefaultDeny alone, not on
// Enabled: the two answer different questions, and an operator who wants
// per-session containment has not thereby asked AP to blanket-deny a namespace
// full of their own workloads.
//
// The two are stamped TOGETHER and the allow goes FIRST, because the failure
// order matters: a floor with no companion allow severs the platform's own
// workspace Jobs, while an allow with no floor is inert (NetworkPolicy is an
// allow-union, so a rule that opens nothing new changes nothing).
func (r *Reconciler) ensureNamespaceDefaultDeny(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error {
	if !r.Netpol.NamespaceDefaultDeny {
		return nil
	}
	allow := BuildWorkspaceJobNetworkPolicy(sess)
	if err := r.Client.Patch(ctx, allow,
		client.Apply, client.ForceOwnership, client.FieldOwner(netpolFieldOwner),
	); err != nil {
		return fmt.Errorf("apply workspace-job egress NetworkPolicy %q in %s: %w", allow.Name, sess.Namespace, err)
	}
	np := BuildNamespaceDefaultDenyNetworkPolicy(sess)
	if err := r.Client.Patch(ctx, np,
		client.Apply, client.ForceOwnership, client.FieldOwner(netpolFieldOwner),
	); err != nil {
		return fmt.Errorf("apply namespace default-deny NetworkPolicy %q in %s: %w", np.Name, sess.Namespace, err)
	}
	return nil
}

// ensureDetectorNetworkPolicy stamps (SSA) one content-guard detector pod's
// deny-all-egress NetworkPolicy. Called by ensureDetectorPod BEFORE the
// detector pod is created, so an enforcing CNI never sees an unconstrained
// detector.
//
// Unlike its siblings this does NOT honour r.Netpol.Enabled. Zero egress is
// the security property of the content-guard feature itself — the detector
// ingests potentially hostile tool text and must not be able to exfiltrate it
// — not an instance of the optional per-session network-policy feature, so an
// install running --session-network-policies=false must still get it. The
// policy is emitted unconditionally, exactly as the plain Create it replaces
// was.
func (r *Reconciler) ensureDetectorNetworkPolicy(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, spec cosidecar.Spec) error {
	np := cosidecar.BuildNetworkPolicy(sess, spec)
	if err := r.Client.Patch(ctx, np,
		client.Apply, client.ForceOwnership, client.FieldOwner(netpolFieldOwner),
	); err != nil {
		return fmt.Errorf("apply detector NetworkPolicy %q: %w", np.Name, err)
	}
	return nil
}

// ensureSidecarNetworkPolicy stamps (SSA) one separate-pod sidecar
// toolbox pod's NetworkPolicy. Called before the sidecar pod is created
// so an enforcing CNI never sees an unconstrained pod.
//
// workshop is true only for the ONE sidecar a Ready, session-owned Workshop
// names — see BuildSidecarNetworkPolicy's doc comment for what it adds.
func (r *Reconciler) ensureSidecarNetworkPolicy(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, rt spiceboxv1alpha1.ResolvedSidecarToolbox, workshop bool) error {
	if !r.Netpol.Enabled {
		return nil
	}
	np := BuildSidecarNetworkPolicy(sess, rt, r.Netpol, workshop)
	if err := r.Client.Patch(ctx, np,
		client.Apply, client.ForceOwnership, client.FieldOwner(netpolFieldOwner),
	); err != nil {
		return fmt.Errorf("apply sidecar NetworkPolicy %q: %w", np.Name, err)
	}
	return nil
}
