package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	apnats "github.com/authzed/openagentprimitives/pkg/platform/nats"
)

// The principal name minted into each creds file. Named once because the name
// is also the input to apnats.InboxSubjectFor, and the client rederives the
// matching prefix from the same name in the JWT — writing it twice invites the
// two to drift, which breaks request/reply rather than merely widening a grant.
const (
	channelsdNATSUser = "channelsd"
	cliNATSUser       = "cli"
	webdNATSUser      = "webd"
)

// WebdNATSGrant is webd's minted NATS user grant. webd SUBSCRIBES to session
// out-subjects (chat + live-view mirror) AND does NATS request-reply: the
// builtin web-chat listener and the live-view submit view_message via
// RequestIn, so webd both PUBLISHES the request and must SUBSCRIBE to its
// reply inbox. The original "never publishes, subscribe-only" assumption
// predated those surfaces and was the bug — a missing inbox entry made every
// view-originated send time out even though the message was delivered.
//
// PubAllow is enumerated because an OMITTED one is publish-ANYWHERE, not
// deny-all (see apnats.UserGrant). This grant used to omit it, which left the
// browser-facing process holding publish rights on the entire control bus:
// ap.revocation, every session's out-subjects (forging the agent's own voice),
// and every inbound kind — including in.user_message, whose channelsd handler
// is the audit-signing inbound writer webd is deliberately kept away from
// (pkg/channels/channelkinds/clienthosted.SubmitUserMessage spells out why: webd holds
// no memory-write credential, so it must round-trip through view_message).
//
// The five subjects below are webd's complete publish surface, each a verified
// call site: view_message (chat SubmitUserMessage and the /interact
// user_message | annotation_batch | mcp_ui_action kinds, all via
// channelkinds.RequestViewMessage), interrupt_request and interaction_decision
// (clienthosted.Listener), resurface_request (channelkinds.
// PublishResurfaceRequest) and app_tool_call (webui/interact). webd links
// pkg/channels/channelsd/pipeline but only for its Authz interface type — it constructs
// no Pipeline, so none of that package's publishes can fire here.
//
// webd needs no publish grant on any inbox: it is never a NATS responder
// (nothing in its import graph calls msg.Respond), and a requester publishes to
// the target subject, not to its own reply inbox.
//
// TestWebdNATSGrantPublishSurface asserts every line of this against a real
// server; add a subject there and here together.
var WebdNATSGrant = apnats.UserGrant{
	Name: webdNATSUser,
	PubAllow: []string{
		"ap.session.*.*.in.view_message",
		"ap.session.*.*.in.interrupt_request",
		"ap.session.*.*.in.interaction_decision",
		"ap.session.*.*.in.resurface_request",
		"ap.session.*.*.in.app_tool_call",
		// ui_data_binding is the agent-UI's own data path: pkg/web/webui/agentui's
		// bindingsHandler resolves every `source: tool` binding by asking the
		// session's runner, via pkg/web/uibindings/tool.
		//
		// Its absence is why that feature had never worked on a real install.
		// The failure gave nothing away: a publish the server refuses does not
		// fail the caller's request — NATS reports the violation as an ASYNC
		// error on the connection, while the request itself simply waits for a
		// reply that can now never come and dies on its own timeout. So every
		// binding surfaced as "the runner did not answer", which reads as a
		// missing or busy runner and is why the first diagnosis of it chased a
		// sleeping session instead of a permission.
		"ap.session.*.*.in.ui_data_binding",
		// ui_presence is the liveness heartbeat webd republishes while a
		// viewer has an agent-UI open and focused, so the runner answering
		// that UI's bindings does not idle out underneath them.
		//
		// Added in the SAME change as the kind and as its row in
		// TestWebdNATSGrantPublishSurface, which is the discipline the line
		// above exists to teach: ui_data_binding shipped without its grant and
		// the whole agent-UI data path silently never worked.
		"ap.session.*.*.in.ui_presence",
		// ui_action is the agent-UI's INVOKE path: pkg/web/webui/agentui's
		// actions handler sends the declared action to the session's runner and
		// relays the synchronous reply. It shipped without this entry, so every
		// agent-UI action failed exactly the way ui_data_binding did — see above.
		"ap.session.*.*.in.ui_action",
		// channelevents.WebhookInboundSubject — not a *.* session template
		// (there is no session yet at delivery time, see that constant's own
		// doc) but the same failure shape as ui_data_binding above: without
		// this entry, pkg/web/webui/channelwebhook's Publish call is silently
		// refused by the server, its returned error is nil either way (NATS
		// reports permission violations asynchronously on the connection, not
		// to the caller), and the handler answers the webhook sender with 202
		// for a delivery that was actually dropped — the one status the
		// route's whole retry contract promises will never happen. Referenced
		// via the exported constant, not a copied literal, so this and the
		// route's own Publish call cannot drift apart.
		channelevents.WebhookInboundSubject,
	},
	SubAllow: []string{"ap.session.*.*.out.>", apnats.InboxSubjectFor(webdNATSUser)},
}

// EnsureNATSIdentity creates the spicebox-nats-identity Secret if it does not
// already exist. Idempotent: a re-run must not rotate the identity.
func EnsureNATSIdentity(ctx context.Context, c client.Client) error {
	var existing corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Namespace: TokenSecretNamespace, Name: "spicebox-nats-identity"}, &existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get nats identity secret: %w", err)
	}

	id, err := apnats.GenerateIdentity()
	if err != nil {
		return fmt.Errorf("generate nats identity: %w", err)
	}

	// The interpolated values (OperatorJWT, AccountPublicKey, AccountJWT) are
	// base32/base64url-encoded and contain no whitespace, quotes, or braces, so
	// they are safe to inline into the NATS config format without escaping.
	authConf := fmt.Sprintf(`operator: %s
resolver: MEMORY
resolver_preload: {
  %s: %s
}
`, id.OperatorJWT, id.AccountPublicKey, id.AccountJWT)

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "spicebox-nats-identity", Namespace: TokenSecretNamespace},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"operator-jwt":               []byte(id.OperatorJWT),
			"account-jwt":                []byte(id.AccountJWT),
			"account-public-key":         []byte(id.AccountPublicKey),
			"account-signing-seed":       id.AccountSigningSeed,
			"account-signing-public-key": []byte(id.AccountSigningPublicKey),
			"auth.conf":                  []byte(authConf),
		},
	}
	if err := c.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create nats identity secret: %w", err)
	}
	return nil
}

// EnsureNATSTLS creates the spicebox-nats-tls Secret if it does not already
// exist. Idempotent: a re-run must not rotate the TLS material.
func EnsureNATSTLS(ctx context.Context, c client.Client) error {
	var existing corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Namespace: TokenSecretNamespace, Name: "spicebox-nats-tls"}, &existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get nats tls secret: %w", err)
	}

	m, err := apnats.GenerateTLS([]string{
		"spicebox-nats",
		"spicebox-nats.agentprimitives-system",
		"spicebox-nats.agentprimitives-system.svc",
		"spicebox-nats.agentprimitives-system.svc.cluster.local",
	})
	if err != nil {
		return fmt.Errorf("generate nats tls: %w", err)
	}

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "spicebox-nats-tls", Namespace: TokenSecretNamespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"ca.crt": m.CACertPEM, "tls.crt": m.ServerCertPEM, "tls.key": m.ServerKeyPEM},
	}
	if err := c.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create nats tls secret: %w", err)
	}
	return nil
}

// natsClientCred pairs one creds Secret with the grant its user JWT must carry.
type natsClientCred struct {
	secretName string
	grant      apnats.UserGrant
}

// natsClientCreds is every principal a bus credential is minted for. A function
// rather than a var because two of the three grants call apnats.InboxSubjectFor.
func natsClientCreds() []natsClientCred {
	return []natsClientCred{
		{
			secretName: "spicebox-channelsd-nats-creds",
			grant: apnats.UserGrant{
				Name:     channelsdNATSUser,
				PubAllow: []string{"ap.>", "_INBOX.>"},
				SubAllow: []string{"ap.>", apnats.InboxSubjectFor(channelsdNATSUser)},
			},
		},
		{
			secretName: "spicebox-cli-nats-creds",
			grant: apnats.UserGrant{
				Name:     cliNATSUser,
				PubAllow: []string{"ap.>", "_INBOX.>"},
				SubAllow: []string{"ap.>", apnats.InboxSubjectFor(cliNATSUser)},
			},
		},
		{secretName: "spicebox-webd-nats-creds", grant: WebdNATSGrant},
	}
}

// EnsureNATSClientCreds mints the per-principal NATS creds Secrets. It requires
// spicebox-nats-identity + spicebox-nats-tls to already exist (EnsureNATSIdentity
// + EnsureNATSTLS run first). Each creds Secret is minted only when absent or
// when the stored JWT no longer carries the shipped grant (see ensureNATSCredsSecret).
func EnsureNATSClientCreds(ctx context.Context, c client.Client, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var identitySec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: TokenSecretNamespace, Name: "spicebox-nats-identity"}, &identitySec); err != nil {
		return fmt.Errorf("get nats identity secret (run EnsureNATSIdentity first): %w", err)
	}
	var tlsSec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: TokenSecretNamespace, Name: "spicebox-nats-tls"}, &tlsSec); err != nil {
		return fmt.Errorf("get nats tls secret (run EnsureNATSTLS first): %w", err)
	}
	caCrt := tlsSec.Data["ca.crt"]
	if len(caCrt) == 0 {
		return fmt.Errorf("nats tls secret is missing ca.crt")
	}

	// MintUser only consults AccountPublicKey + AccountSigningSeed.
	id := &apnats.Identity{
		AccountPublicKey:   string(identitySec.Data["account-public-key"]),
		AccountSigningSeed: identitySec.Data["account-signing-seed"],
	}
	if id.AccountPublicKey == "" || len(id.AccountSigningSeed) == 0 {
		return fmt.Errorf("nats identity secret is missing account-public-key or account-signing-seed")
	}

	for _, cred := range natsClientCreds() {
		if err := ensureNATSCredsSecret(ctx, c, cred, id, caCrt, logf); err != nil {
			return err
		}
	}
	return nil
}

// ensureNATSCredsSecret reconciles one creds Secret against the shipped grant,
// minting a user JWT only when the stored one does not already carry it. A
// user JWT is a frozen copy of the grant it was minted from, so a cluster
// installed before a subject was added to a grant kept a JWT without it forever
// until this drift check (grant.CredsDrift) forced a re-mint; a re-mint rotates
// the user nkey, so it must be rare (set-compare, order-insensitive). ca.crt is
// refreshed on the same write.
func ensureNATSCredsSecret(ctx context.Context, c client.Client, cred natsClientCred, id *apnats.Identity, caCrt []byte, logf func(string, ...any)) error {
	key := types.NamespacedName{Namespace: TokenSecretNamespace, Name: cred.secretName}

	var existing corev1.Secret
	err := c.Get(ctx, key, &existing)
	switch {
	case err == nil:
		drift := cred.grant.CredsDrift(string(existing.Data["nats.creds"]))
		if drift == "" && bytes.Equal(existing.Data["ca.crt"], caCrt) {
			return nil
		}
		if drift == "" {
			drift = "the stored CA no longer matches the NATS server's"
		}
		logf("re-minting %s: %s", cred.secretName, drift)

		creds, mintErr := apnats.MintUser(id, cred.grant)
		if mintErr != nil {
			return fmt.Errorf("re-mint %s nats creds: %w", cred.grant.Name, mintErr)
		}
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		existing.Data["nats.creds"] = []byte(creds)
		existing.Data["ca.crt"] = caCrt
		if err := c.Update(ctx, &existing); err != nil {
			return fmt.Errorf("update %s secret: %w", cred.secretName, err)
		}
		// A running process read its creds file once, at connect time; the
		// kubelet refreshing the mounted file changes nothing for it, so a
		// re-mint must roll every consumer or the stale JWT stays on the wire.
		return restartWorkloadsMountingSecret(ctx, c, key, logf)

	case apierrors.IsNotFound(err):
		creds, mintErr := apnats.MintUser(id, cred.grant)
		if mintErr != nil {
			return fmt.Errorf("mint %s nats creds: %w", cred.grant.Name, mintErr)
		}
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: cred.secretName, Namespace: TokenSecretNamespace},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"nats.creds": []byte(creds), "ca.crt": caCrt},
		}
		if err := c.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create %s secret: %w", cred.secretName, err)
		}
		return nil

	default:
		return fmt.Errorf("get %s secret: %w", cred.secretName, err)
	}
}

// restartWorkloadsMountingSecret rolls every Deployment in the Secret's
// namespace whose pod template mounts it, so a rotated credential actually
// reaches the processes using it. Nothing to roll is the normal case on a first
// mint: nobody is running yet.
func restartWorkloadsMountingSecret(ctx context.Context, c client.Client, secret types.NamespacedName, logf func(string, ...any)) error {
	var deps appsv1.DeploymentList
	if err := c.List(ctx, &deps, client.InNamespace(secret.Namespace)); err != nil {
		return fmt.Errorf("list deployments in %s to roll after rotating %s: %w", secret.Namespace, secret.Name, err)
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	for i := range deps.Items {
		d := &deps.Items[i]
		if !podSpecMountsSecret(&d.Spec.Template.Spec, secret.Name) {
			continue
		}
		patch := []byte(fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"ap.authzed.com/restartedAt":%q}}}}}`, stamp))
		if err := c.Patch(ctx, d, client.RawPatch(types.StrategicMergePatchType, patch)); err != nil {
			return fmt.Errorf("rollout-restart %s/%s after rotating %s: %w", d.Namespace, d.Name, secret.Name, err)
		}
		logf("restarting %s to pick up the re-minted %s", d.Name, secret.Name)
	}
	return nil
}

// podSpecMountsSecret reports whether the pod template reads the named Secret —
// as a volume (the creds-file shape) or through env/envFrom.
func podSpecMountsSecret(spec *corev1.PodSpec, name string) bool {
	for _, v := range spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == name {
			return true
		}
		if v.Projected == nil {
			continue
		}
		for _, src := range v.Projected.Sources {
			if src.Secret != nil && src.Secret.Name == name {
				return true
			}
		}
	}
	containers := make([]corev1.Container, 0, len(spec.Containers)+len(spec.InitContainers))
	containers = append(containers, spec.Containers...)
	containers = append(containers, spec.InitContainers...)
	for _, ctr := range containers {
		for _, ef := range ctr.EnvFrom {
			if ef.SecretRef != nil && ef.SecretRef.Name == name {
				return true
			}
		}
		for _, e := range ctr.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name == name {
				return true
			}
		}
	}
	return false
}
