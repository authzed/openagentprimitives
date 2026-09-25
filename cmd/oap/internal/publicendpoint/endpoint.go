// Package publicendpoint is the ONE door through which `oap` creates the
// cluster's PublicEndpoint.
//
// It is a package of its own rather than a helper inside one command, because
// three command families need it and none of them is the others' parent:
// `oap install` creates the endpoint at install time on a kind whose policy
// says so, `oap agent install` creates one on demand when a bundle declares a
// channel that receives webhooks, and `oap desktop` creates one on boot when
// such a channel is already wired. A door living in installcmd would make
// `oap agent install` import the whole platform installer to reach one
// function.
//
// EVERY creation path goes through EnsureWebd, whose first statement is
// cloud.CheckPublicEndpointAllowed — and
// TestEveryPublicEndpointCreationGoesThroughTheCheckedHelper enforces that
// structurally, by refusing to let any other file under cmd/ or internal/ so
// much as NAME the type.
package publicendpoint

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliout"
	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

const (
	// WebdName is the cluster's one PublicEndpoint for webd. Fixed rather than
	// generated: the CR is cluster-scoped and describes the cluster's own front
	// door, so a second one is a mistake to be overwritten on re-install, not a
	// new endpoint.
	WebdName = "webd"

	// NgrokAuthTokenSecret / -Key name where the ngrok provider's credential
	// lives. The operator reads it directly; it is never plumbed into a second
	// workload.
	NgrokAuthTokenSecret = "ngrok-authtoken"
	NgrokAuthTokenKey    = "token"

	// ngrokAuthTokenEnv is the env var the ngrok agent SDK conventionally
	// reads. The creation path forwards it into the Secret when it is set, so
	// an operator who already exports it needs no separate step to get a
	// tunnel.
	ngrokAuthTokenEnv = "NGROK_AUTHTOKEN"

	// tunnelProviderNgrok is the registered localtunnel provider named on the
	// CR. It is a registry key, resolved by the operator
	// (pkg/web/localtunnel/registry) — nothing here dispatches on it.
	tunnelProviderNgrok = "ngrok"
)

// TokenPrompter asks an operator for the tunnel provider's credential. An
// empty answer means they declined; nil means there is nobody to ask — the
// desktop boots with no terminal, and `oap install` runs inside a progress
// checklist that owns the screen. Both fall back to the environment.
//
// A function rather than a flag so this package stays a leaf and never imports
// the terminal stack.
type TokenPrompter func(ctx context.Context) (string, error)

// WebdOnDemandPlan is the read-only decision for a later on-demand endpoint
// execution. The credential stays private and diagnostic formatting never
// includes it.
type WebdOnDemandPlan struct {
	ensure           bool
	token            string
	replacesLocalURL bool
}

type createdPrerequisite struct {
	kind            string
	name, namespace string
	uid             types.UID
	resourceVersion string
}

// AppliedWebdOnDemand is the recoverable receipt for Kubernetes prerequisites
// created while resolving graph channels. It retains only object identity and
// observed preconditions, never the tunnel credential itself.
type AppliedWebdOnDemand struct {
	created []createdPrerequisite
}

func (a AppliedWebdOnDemand) String() string {
	return fmt.Sprintf("AppliedWebdOnDemand{created:%d}", len(a.created))
}

func (a AppliedWebdOnDemand) GoString() string { return a.String() }

func (a *AppliedWebdOnDemand) recordObserved(ctx context.Context, c client.Client, kind string, created client.Object) error {
	if a == nil || created == nil {
		return nil
	}
	observed, err := prerequisiteObject(kind, created.GetNamespace(), created.GetName())
	if err != nil {
		return err
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(created), observed); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		// Retain the identity Create returned. Its resourceVersion may be stale,
		// but that only makes the guarded rollback refuse to delete; it can
		// never widen cleanup to a replacement object.
		a.record(kind, created)
		return fmt.Errorf("observe created %s %s/%s for rollback: %w",
			kind, created.GetNamespace(), created.GetName(), err)
	}
	if created.GetUID() != "" && observed.GetUID() != created.GetUID() {
		return fmt.Errorf("observe created %s %s/%s for rollback: object was replaced before it could be recorded",
			kind, created.GetNamespace(), created.GetName())
	}
	a.record(kind, observed)
	return nil
}

func (a *AppliedWebdOnDemand) record(kind string, obj client.Object) {
	if a == nil || obj == nil {
		return
	}
	a.created = append(a.created, createdPrerequisite{
		kind: kind, name: obj.GetName(), namespace: obj.GetNamespace(),
		uid: obj.GetUID(), resourceVersion: obj.GetResourceVersion(),
	})
}

func prerequisiteObject(kind, namespace, name string) (client.Object, error) {
	switch kind {
	case "PublicEndpoint":
		return &v1alpha1.PublicEndpoint{ObjectMeta: metav1.ObjectMeta{Name: name}}, nil
	case "Secret":
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}, nil
	default:
		return nil, fmt.Errorf("endpoint prerequisite: unsupported kind %q", kind)
	}
}

// Rollback deletes only prerequisites created by the Apply that produced this
// receipt. UID and resourceVersion preconditions prevent cleanup from deleting
// an object another actor replaced or changed after it was observed.
func (a *AppliedWebdOnDemand) Rollback(ctx context.Context, c client.Client) error {
	if a == nil {
		return nil
	}
	var errs []error
	for i := len(a.created) - 1; i >= 0; i-- {
		created := a.created[i]
		obj, err := prerequisiteObject(created.kind, created.namespace, created.name)
		if err != nil {
			errs = append(errs, fmt.Errorf("rollback endpoint prerequisite: %w", err))
			continue
		}
		uid, rv := created.uid, created.resourceVersion
		err = c.Delete(ctx, obj, client.Preconditions{UID: &uid, ResourceVersion: &rv})
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("rollback endpoint prerequisite: delete %s %s/%s: %w",
				created.kind, created.namespace, created.name, err))
		}
	}
	return errors.Join(errs...)
}

func (p WebdOnDemandPlan) String() string {
	return fmt.Sprintf("WebdOnDemandPlan{ensure:%t, credential:%t}", p.ensure, p.token != "")
}

func (p WebdOnDemandPlan) GoString() string { return p.String() }

// NeedsReadyURL reports whether execution will ensure and wait for the
// endpoint. A false result means the operator declined a missing credential
// or webd already has another public-address owner.
func (p WebdOnDemandPlan) NeedsReadyURL() bool { return p.ensure }

// ReplacesLocalURL reports that the URL observed during planning is only the
// machine-local address. A caller must either defer that wizard answer until
// Apply publishes the tunnel URL, or ask the operator for a public URL when
// they declined the tunnel credential.
func (p WebdOnDemandPlan) ReplacesLocalURL() bool { return p.replacesLocalURL }

// SensitiveValues returns execution-only values for registration with the
// caller's redactor. Callers must not render or serialize them.
func (p WebdOnDemandPlan) SensitiveValues() []string {
	if p.token == "" {
		return nil
	}
	return []string{p.token}
}

// PrepareWebdOnDemand performs only reads and collects the credential choice
// needed by a later Apply. It never creates or updates Kubernetes objects.
func PrepareWebdOnDemand(ctx context.Context, c client.Client, strat cloud.Strategy, ask TokenPrompter) (WebdOnDemandPlan, error) {
	if err := cloud.CheckPublicEndpointAllowed(strat); err != nil {
		return WebdOnDemandPlan{}, err
	}

	var existing v1alpha1.PublicEndpoint
	err := c.Get(ctx, types.NamespacedName{Name: WebdName}, &existing)
	switch {
	case err == nil && existing.Status.Phase == v1alpha1.PublicEndpointPhaseReady && strings.TrimSpace(existing.Status.URL) != "":
		return WebdOnDemandPlan{ensure: true}, nil
	case err == nil:
		// A pending endpoint can be completed by a credential collected below.
	case apierrors.IsNotFound(err):
		var cm corev1.ConfigMap
		key := types.NamespacedName{Namespace: cloud.WebdServiceNamespace, Name: v1alpha1.WebdExternalURLConfigMap}
		if err := c.Get(ctx, key, &cm); err != nil {
			if apierrors.IsNotFound(err) {
				return WebdOnDemandPlan{}, fmt.Errorf("ConfigMap %s/%s not found, so there is no local address to give PublicEndpoint %s — run `oap install` first",
					cloud.WebdServiceNamespace, v1alpha1.WebdExternalURLConfigMap, WebdName)
			}
			return WebdOnDemandPlan{}, fmt.Errorf("get configmap %s/%s: %w", cloud.WebdServiceNamespace, v1alpha1.WebdExternalURLConfigMap, err)
		}
		if !isLoopbackURL(cm.Data[v1alpha1.WebdTrustedURLKey]) {
			return WebdOnDemandPlan{}, nil
		}
	default:
		return WebdOnDemandPlan{}, fmt.Errorf("get PublicEndpoint %s: %w", WebdName, err)
	}

	token, available, err := planNgrokAuthToken(ctx, c, ask)
	if err != nil {
		return WebdOnDemandPlan{}, err
	}
	if !available {
		return WebdOnDemandPlan{replacesLocalURL: true}, nil
	}
	return WebdOnDemandPlan{ensure: true, token: token, replacesLocalURL: true}, nil
}

func planNgrokAuthToken(ctx context.Context, c client.Client, ask TokenPrompter) (string, bool, error) {
	var existing corev1.Secret
	key := types.NamespacedName{Namespace: cloud.WebdServiceNamespace, Name: NgrokAuthTokenSecret}
	err := c.Get(ctx, key, &existing)
	switch {
	case err == nil && len(existing.Data[NgrokAuthTokenKey]) > 0:
		return "", true, nil
	case err == nil, apierrors.IsNotFound(err):
		// Collect below without writing the present or absent Secret.
	default:
		return "", false, fmt.Errorf("get secret %s/%s: %w", cloud.WebdServiceNamespace, NgrokAuthTokenSecret, err)
	}
	token := strings.TrimSpace(os.Getenv(ngrokAuthTokenEnv))
	if token == "" && ask != nil {
		var askErr error
		token, askErr = ask(ctx)
		if askErr != nil {
			return "", false, fmt.Errorf("ask for the %s: %w", ngrokAuthTokenEnv, askErr)
		}
		token = strings.TrimSpace(token)
	}
	if token == "" {
		if ask == nil {
			// An unattended run made no decision to stay private. Preserve the
			// existing behavior: execution creates the pending endpoint and its
			// bounded wait reports the missing credential instead of silently
			// treating absence of a terminal as a decline.
			return "", true, nil
		}
		return "", false, nil
	}
	return token, true, nil
}

// Apply materializes the prepared credential, creates the endpoint when
// needed, and waits for its machine-derived public URL. It asks no questions.
func (p WebdOnDemandPlan) Apply(ctx context.Context, out io.Writer, c client.Client, strat cloud.Strategy, timeout time.Duration) (*AppliedWebdOnDemand, error) {
	receipt := &AppliedWebdOnDemand{}
	if !p.ensure {
		return receipt, nil
	}
	if p.token != "" {
		fixed := func(context.Context) (string, error) { return p.token, nil }
		created, err := ensureNgrokAuthTokenSecretTracked(ctx, out, c, fixed)
		var observeErr error
		if created != nil {
			observeErr = receipt.recordObserved(ctx, c, "Secret", created)
		}
		if err != nil || observeErr != nil {
			return receipt, errors.Join(err, observeErr)
		}
	}
	created, err := ensureWebdOnDemandReadyTracked(ctx, out, c, strat, timeout, nil)
	var observeErr error
	if created != nil {
		observeErr = receipt.recordObserved(ctx, c, "PublicEndpoint", created)
	}
	return receipt, errors.Join(err, observeErr)
}

// EnsureWebd creates the cluster's one PublicEndpoint for webd, pointing
// spec.localURL at localURL — the address webd answers on from the host while
// no tunnel is up.
//
// THIS IS THE ONLY DOOR. Every path that creates a PublicEndpoint goes through
// here, so that cloud.CheckPublicEndpointAllowed is asked before anything is
// written, on every path, without each caller having to remember. `oap install`
// is one caller (a kind whose policy says CreatedAtInstall); the on-demand
// paths below are the others, and they need a localURL only they know — the
// desktop app binds 127.0.0.1 on a port it picks at runtime, where install
// binds localhost:8080. webd dispatches on an exact bare-host match, so there
// is no defaulting either of those; the caller supplies it.
//
// It also materializes the provider credential the endpoint references. That
// belongs INSIDE this function, after the check, rather than beside its call
// site: a Secret written before the refusal drops an ngrok token into a cluster
// that then refuses to open any tunnel with it, and no guard would catch that —
// the AST guard watches for PublicEndpoint construction, not for Secrets.
// Behind the check, the credential cannot outlive the refusal.
//
// Idempotent by create-if-absent: an existing endpoint is left exactly as it
// is, including a reservedDomain or a provider an operator edited by hand. A
// re-install must not stomp a live tunnel's spec.
func EnsureWebd(ctx context.Context, out io.Writer, c client.Client, strat cloud.Strategy, localURL string, ask TokenPrompter) error {
	_, err := ensureWebdTracked(ctx, out, c, strat, localURL, ask)
	return err
}

func ensureWebdTracked(ctx context.Context, out io.Writer, c client.Client, strat cloud.Strategy, localURL string, ask TokenPrompter) (*v1alpha1.PublicEndpoint, error) {
	if err := cloud.CheckPublicEndpointAllowed(strat); err != nil {
		return nil, err
	}
	if localURL == "" {
		return nil, fmt.Errorf("cannot create PublicEndpoint %q: localURL is empty, and it is not defaulted — "+
			"webd dispatches on an exact host match, so a guessed address 404s every route",
			WebdName)
	}
	if err := ensureNgrokAuthTokenSecret(ctx, out, c, ask); err != nil {
		return nil, err
	}

	var existing v1alpha1.PublicEndpoint
	err := c.Get(ctx, types.NamespacedName{Name: WebdName}, &existing)
	if err == nil {
		cliout.Info(out, "  PublicEndpoint %s exists, leaving its spec alone", WebdName)
		return nil, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get PublicEndpoint %s: %w", WebdName, err)
	}

	pe := &v1alpha1.PublicEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: WebdName},
		Spec: v1alpha1.PublicEndpointSpec{
			Target: v1alpha1.PublicEndpointTarget{
				Namespace: cloud.WebdServiceNamespace,
				Service:   cloud.WebdServiceName,
				Port:      cloud.WebdServicePort,
			},
			Provider: tunnelProviderNgrok,
			AuthTokenRef: v1alpha1.ClusterSecretKeyRef{
				Namespace: cloud.WebdServiceNamespace,
				Name:      NgrokAuthTokenSecret,
				Key:       NgrokAuthTokenKey,
			},
			LocalURL: localURL,
		},
	}
	if err := c.Create(ctx, pe); err != nil {
		return nil, fmt.Errorf("create PublicEndpoint %s: %w", WebdName, err)
	}
	cliout.Info(out, "  created PublicEndpoint %s (target %s/%s, provider %s, local URL %s)",
		WebdName, cloud.WebdServiceNamespace, cloud.WebdServiceName,
		tunnelProviderNgrok, localURL)
	return pe, nil
}

// RefuseWebdEndpointWhenURLIsClaimed refuses an install whose routing flags
// have already spoken for webd's external URL while an endpoint that owns that
// URL is still on the cluster. claimedBy names the flag doing the claiming, so
// the refusal reads as an answer to "who owns this address".
//
// WHY THIS IS A SEPARATE QUESTION FROM "SHALL I CREATE ONE". `oap install`'s
// creation decision can only see its own flags — it asks whether install
// creates an endpoint, not whether one is already there. So
// `oap install --cluster-kind=local` followed later by
// `oap install --cluster-kind=local --trusted-hostname=h` provisions a Gateway
// and a certificate, writes the real https:// hosts into the two ConfigMap
// keys, reports success — and the first install's PublicEndpoint controller
// re-applies its own value over them under ForceOwnership on its next
// reconcile. Nothing errors. That is exactly the outcome the create decision
// argues at length to prevent, arriving by the door it cannot see.
//
// A REFUSAL, NOT A DELETE. Deleting would tear down a live tunnel — and, with
// the finalizer's ConfigMap cleanup, blank the very keys the Gateway path is
// about to fill — as a side effect of an install flag, silently. Which of the
// two owns the address is the operator's call, so this says what is in the way
// and what removes it.
//
// It asks about the TARGET, not the name: the controller writes webd's external
// URL for any endpoint targeting webd, so a hand-made one under another name
// claims the keys just as hard as the one this package creates.
func RefuseWebdEndpointWhenURLIsClaimed(ctx context.Context, out io.Writer, c client.Client, claimedBy string) error {
	var endpoints v1alpha1.PublicEndpointList
	if err := c.List(ctx, &endpoints); err != nil {
		if meta.IsNoMatchError(err) {
			// No PublicEndpoint CRD on this cluster — an install predating the
			// type. Nothing can own the URL, so there is nothing to refuse.
			return nil
		}
		return fmt.Errorf("list PublicEndpoints to find out who owns webd's external URL: %w", err)
	}
	var owners []string
	for i := range endpoints.Items {
		e := &endpoints.Items[i]
		if cloud.IsWebdTarget(e.Spec.Target.Namespace, e.Spec.Target.Service) {
			owners = append(owners, e.Name)
		}
	}
	if len(owners) == 0 {
		cliout.Info(out, "  no PublicEndpoint targets webd, so %s owns its external URL", claimedBy)
		return nil
	}
	return fmt.Errorf(
		"%s claims webd's external URL, but PublicEndpoint %s already owns it: that endpoint's controller "+
			"re-applies the tunnel's address into ConfigMap %s/%s on every reconcile, so this install would "+
			"provision a Gateway, write the real hostnames, report success, and be overwritten minutes later "+
			"with nobody told. Exactly one of the two may own the address. To keep the tunnel, re-run without "+
			"%s; to move to the hostname, delete the endpoint first (`kubectl delete publicendpoint %s`) and "+
			"re-run — deleting it here would tear down a live tunnel as a side effect of a flag",
		claimedBy, strings.Join(owners, ", "),
		cloud.WebdServiceNamespace, v1alpha1.WebdExternalURLConfigMap,
		claimedBy, strings.Join(owners, " "))
}

// EnsureWebdOnDemand creates the endpoint for a cluster kind whose policy says
// a tunnel is opened only when something needs one — `desktop` today. It
// reports whether an endpoint is now in place for webd, so a caller that goes
// on to wait for a public URL knows whether there is anything to wait for.
//
// It answers on its own the two questions the install-time path answers from
// its flags, because neither flag is in reach here: `oap agent install` and the
// desktop's own boot run long after `oap install` returned.
//
//  1. WHERE IS WEBD REACHABLE LOCALLY? spec.localURL is required and
//     deliberately not defaulted, and neither caller can know it — the desktop
//     binds 127.0.0.1 on a port it picks at runtime. The answer is already on
//     the cluster: webd's external-URL ConfigMap holds the address webd is
//     reachable at right now, which is precisely what spec.localURL means while
//     no tunnel is up.
//
//  2. DOES ANYTHING ELSE ALREADY OWN THAT URL? This is
//     webdExternalURLHasAnotherOwner's question, asked of the cluster rather
//     than of install's flags. A desktop whose cluster was installed with
//     --trusted-hostname has a Gateway, a certificate and a real https:// host
//     in those keys; an endpoint created here would hand them to a controller
//     that re-applies them under ForceOwnership every reconcile, rewriting a
//     working public hostname to a loopback address. A non-loopback value is
//     that ownership, and it is also good news for the caller: a cluster with a
//     real external host already has an address a webhook provider can deliver
//     to, so there is nothing to open.
//
// An EXISTING endpoint short-circuits both questions. By then the controller
// owns the ConfigMap and the value in it is the tunnel's own public URL, which
// would read as "another owner" to question 2 and stand this path down against
// its own endpoint.
func EnsureWebdOnDemand(ctx context.Context, out io.Writer, c client.Client, strat cloud.Strategy, ask TokenPrompter) (bool, error) {
	ensured, _, err := ensureWebdOnDemandTracked(ctx, out, c, strat, ask)
	return ensured, err
}

func ensureWebdOnDemandTracked(ctx context.Context, out io.Writer, c client.Client, strat cloud.Strategy, ask TokenPrompter) (bool, *v1alpha1.PublicEndpoint, error) {
	// Asked here as well as inside EnsureWebd, and not because a write could
	// otherwise slip past: nothing below writes before that call. It is for the
	// MESSAGE. A kind that may never carry a tunnel would otherwise be told its
	// external-URL ConfigMap is missing, or that webd is already reachable —
	// true sentences about the wrong question.
	if err := cloud.CheckPublicEndpointAllowed(strat); err != nil {
		return false, nil, err
	}

	var existing v1alpha1.PublicEndpoint
	err := c.Get(ctx, types.NamespacedName{Name: WebdName}, &existing)
	if err == nil {
		cliout.Info(out, "  PublicEndpoint %s already exists", WebdName)
		return true, nil, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, nil, fmt.Errorf("get PublicEndpoint %s: %w", WebdName, err)
	}

	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: cloud.WebdServiceNamespace, Name: v1alpha1.WebdExternalURLConfigMap}
	if err := c.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil, fmt.Errorf("ConfigMap %s/%s not found, so there is no local address to give PublicEndpoint %s — run `oap install` first",
				cloud.WebdServiceNamespace, v1alpha1.WebdExternalURLConfigMap, WebdName)
		}
		return false, nil, fmt.Errorf("get configmap %s/%s: %w", cloud.WebdServiceNamespace, v1alpha1.WebdExternalURLConfigMap, err)
	}

	localURL := cm.Data[v1alpha1.WebdTrustedURLKey]
	if !isLoopbackURL(localURL) {
		// Both non-loopback shapes are somebody else's, and they are worth
		// telling apart: a real host is an address that already works, while an
		// empty value is an install that provisioned a Gateway whose URL has
		// not landed yet — and a webhook registered against either of those is
		// this pass's business, not this function's.
		if localURL == "" {
			cliout.Info(out, "  %s/%s carries no %s yet, so webd's external URL is spoken for by the Gateway `oap install --trusted-hostname` provisioned; no tunnel opened",
				cloud.WebdServiceNamespace, v1alpha1.WebdExternalURLConfigMap, v1alpha1.WebdTrustedURLKey)
		} else {
			cliout.Info(out, "  webd is already reachable at %s, so no tunnel is opened", localURL)
		}
		return false, nil, nil
	}

	created, err := ensureWebdTracked(ctx, out, c, strat, localURL, ask)
	if err != nil {
		return false, created, err
	}
	return true, created, nil
}

// EnsureWebdOnDemandReady is EnsureWebdOnDemand plus a BOUNDED wait for the
// endpoint to publish a public URL, for the caller that cannot go on without
// one: a channel wizard registers its webhook with a third party, and an
// address that is not live yet is an address that gets registered wrong.
//
// The bound is the point. A cluster with no ngrok credential never reaches
// Ready — the endpoint sits at Pending/AuthTokenSecretMissing by design, since
// a token that has not arrived is not a failure — so an unbounded wait would
// hang `oap agent install` with nothing on screen. The refusal says which
// endpoint, how long it waited, what phase it was left in, and what to do.
func EnsureWebdOnDemandReady(ctx context.Context, out io.Writer, c client.Client, strat cloud.Strategy, timeout time.Duration, ask TokenPrompter) error {
	_, err := ensureWebdOnDemandReadyTracked(ctx, out, c, strat, timeout, ask)
	return err
}

func ensureWebdOnDemandReadyTracked(ctx context.Context, out io.Writer, c client.Client, strat cloud.Strategy, timeout time.Duration, ask TokenPrompter) (*v1alpha1.PublicEndpoint, error) {
	ensured, created, err := ensureWebdOnDemandTracked(ctx, out, c, strat, ask)
	if err != nil || !ensured {
		return created, err
	}
	return created, awaitWebdReady(ctx, out, c, timeout)
}

// publicEndpointPollInterval is how often the wait re-reads the endpoint.
// The controller writes status once per reconcile, so a tighter loop only
// re-reads the same object.
const publicEndpointPollInterval = 2 * time.Second

// awaitWebdReady polls until the endpoint publishes a URL, or until timeout.
//
// A non-positive timeout is refused rather than treated as "forever" or as "do
// not wait": both are silent, and one of them is the hang this bound exists to
// prevent.
func awaitWebdReady(ctx context.Context, out io.Writer, c client.Client, timeout time.Duration) error {
	if timeout <= 0 {
		return fmt.Errorf("cannot wait for the public endpoint %q: the wait was given no bound (%s)", WebdName, timeout)
	}
	cliout.Info(out, "  waiting up to %s for PublicEndpoint %s to publish a public URL",
		timeout.Round(time.Second), WebdName)

	// What the last poll saw, so the refusal can say what did not happen rather
	// than only that it did not happen in time.
	lastSeen := "the endpoint was never read"
	pollErr := wait.PollUntilContextTimeout(ctx, publicEndpointPollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			var pe v1alpha1.PublicEndpoint
			if err := c.Get(ctx, types.NamespacedName{Name: WebdName}, &pe); err != nil {
				if apierrors.IsNotFound(err) {
					// Deleted from under us between the create and this read.
					// Keep polling and let the bound speak: something else is
					// writing this cluster, and a hard failure here would name
					// the wrong cause.
					lastSeen = "the endpoint is no longer on the cluster"
					return false, nil
				}
				return false, fmt.Errorf("get PublicEndpoint %s: %w", WebdName, err)
			}
			lastSeen = describePublicEndpointState(&pe)
			if pe.Status.Phase == v1alpha1.PublicEndpointPhaseReady && pe.Status.URL != "" {
				cliout.Info(out, "  PublicEndpoint %s is Ready at %s", WebdName, pe.Status.URL)
				return true, nil
			}
			return false, nil
		})
	if pollErr == nil {
		return nil
	}
	if !wait.Interrupted(pollErr) {
		return pollErr
	}
	return fmt.Errorf("the public endpoint %q did not publish a URL within %s (%s); "+
		"without one this cluster has no address a webhook provider can deliver to. "+
		"Most often the tunnel has no credential yet: put an ngrok authtoken in Secret %s/%s under key %q "+
		"(or export %s and re-run), then check `kubectl get publicendpoint %s -o yaml`",
		WebdName, timeout.Round(time.Second), lastSeen,
		cloud.WebdServiceNamespace, NgrokAuthTokenSecret, NgrokAuthTokenKey, ngrokAuthTokenEnv, WebdName)
}

// describePublicEndpointState renders what an endpoint's status says, for a
// refusal that has to explain a wait that ran out. The Ready condition's reason
// and message are where the controller puts the actionable half (no auth token,
// unknown provider, the provider refused), so a phase alone is not enough.
func describePublicEndpointState(pe *v1alpha1.PublicEndpoint) string {
	phase := pe.Status.Phase
	if phase == "" {
		phase = "not yet reconciled"
	}
	for i := range pe.Status.Conditions {
		cond := pe.Status.Conditions[i]
		if cond.Type != v1alpha1.PublicEndpointConditionReady {
			continue
		}
		if cond.Message != "" {
			return fmt.Sprintf("phase %s, %s: %s", phase, cond.Reason, cond.Message)
		}
		return fmt.Sprintf("phase %s, %s", phase, cond.Reason)
	}
	return "phase " + phase
}

// isLoopbackURL reports whether raw names a loopback host.
//
// That is the shape of every address webd's external-URL ConfigMap carries
// while nothing outside the machine owns it: `oap install` seeds
// http://localhost:8080, and `oap desktop` writes http://127.0.0.1:<port>.
// Anything else — a real https:// host, or an empty value awaiting one — is
// somebody else's answer to where this cluster is reached.
//
// url.Parse + net.ParseIP rather than a prefix test on the string: "localhost"
// and "127.0.0.1" are not the only loopback spellings (::1), and a host that
// merely starts with one of them (http://localhost.demo.test) is not loopback
// at all.
func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ensureNgrokAuthTokenSecret forwards NGROK_AUTHTOKEN from the invoking shell
// into the Secret the PublicEndpoint references, when it is set and the cluster
// does not already hold a token there.
//
// The env var is the ngrok agent SDK's own convention, so an operator who
// already exports it for other ngrok tooling needs no extra step. Absent, this
// is not an error: the endpoint reports Pending/AuthTokenSecretMissing and
// keeps publishing the local URL until someone supplies a token, which is the
// designed state for a cluster that has not chosen to be public yet.
//
// Called only from EnsureWebd, after its refusal — see there.
//
// A stored token is never overwritten — rotating one is the operator's action
// against a live tunnel, not something a re-install should undo from a stale
// shell. Filling in a Secret that holds no token is not a rotation, so that one
// is written.
func ensureNgrokAuthTokenSecret(ctx context.Context, out io.Writer, c client.Client, ask TokenPrompter) error {
	_, err := ensureNgrokAuthTokenSecretTracked(ctx, out, c, ask)
	return err
}

func ensureNgrokAuthTokenSecretTracked(ctx context.Context, out io.Writer, c client.Client, ask TokenPrompter) (*corev1.Secret, error) {
	// The cluster answers first: a Secret that HOLDS a token settles the
	// question, so neither the environment nor the operator is consulted.
	// Asking either one here would report a missing credential to a cluster
	// that has a working one, and interrupt an operator who already answered.
	//
	// Holding a token, not merely existing. A Secret created by hand and left
	// unfinished, an External Secrets sync that has not landed a value, a key
	// spelled differently — each of those answered nothing, and reading its
	// presence as an answer suppresses the environment AND the prompt at once.
	// Nothing fails: the endpoint sits at Pending/AuthTokenSecretMissing while
	// this run reports it left a working credential alone, and the one person
	// who could fix it is never asked.
	var existing corev1.Secret
	key := types.NamespacedName{Namespace: cloud.WebdServiceNamespace, Name: NgrokAuthTokenSecret}
	err := c.Get(ctx, key, &existing)
	switch {
	case err == nil && len(existing.Data[NgrokAuthTokenKey]) > 0:
		cliout.Info(out, "  Secret %s/%s already holds a %q, leaving it alone",
			cloud.WebdServiceNamespace, NgrokAuthTokenSecret, NgrokAuthTokenKey)
		return nil, nil
	case err == nil:
		// Present but empty-handed: fall through and ask, then fill THIS object
		// in rather than creating a second one.
	case apierrors.IsNotFound(err):
		// Nothing there: fall through and ask, then create.
	default:
		return nil, fmt.Errorf("get secret %s/%s: %w", cloud.WebdServiceNamespace, NgrokAuthTokenSecret, err)
	}
	secretExists := err == nil

	// The environment answers before the operator does: a scripted run has
	// nobody at a terminal, and one that exports the variable has said what it
	// wants without being asked.
	token := os.Getenv(ngrokAuthTokenEnv)
	source := ngrokAuthTokenEnv
	if token == "" && ask != nil {
		typed, askErr := ask(ctx)
		if askErr != nil {
			// Never read as "declined". An interrupted question and an
			// operator choosing to stay private are different answers, and
			// only one of them should leave this run quiet about it.
			return nil, fmt.Errorf("ask for the %s: %w", ngrokAuthTokenEnv, askErr)
		}
		token, source = strings.TrimSpace(typed), "the answer you gave"
	}
	if token == "" {
		cliout.Info(out, "  no tunnel credential yet; the tunnel stays Pending until Secret %s/%s has a %q key "+
			"(set %s, or re-run this and answer the question)",
			cloud.WebdServiceNamespace, NgrokAuthTokenSecret, NgrokAuthTokenKey, ngrokAuthTokenEnv)
		return nil, nil
	}

	// Data, not StringData, in both branches: StringData is a write-only
	// convenience the apiserver folds into Data, so a client that reads the
	// object back — the operator's broker, a test's fake client — sees nothing
	// there.
	if secretExists {
		// Fill in the object that is already there. Only the token key is
		// touched; whatever else the Secret carries is left alone, since
		// something put it there on purpose.
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		existing.Data[NgrokAuthTokenKey] = []byte(token)
		if err := c.Update(ctx, &existing); err != nil {
			return nil, fmt.Errorf("update secret %s/%s: %w", cloud.WebdServiceNamespace, NgrokAuthTokenSecret, err)
		}
		// The value is a credential: say that it landed, never what it is.
		cliout.Info(out, "  filled in the %q of Secret %s/%s from %s",
			NgrokAuthTokenKey, cloud.WebdServiceNamespace, NgrokAuthTokenSecret, source)
		return nil, nil
	}

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      NgrokAuthTokenSecret,
			Namespace: cloud.WebdServiceNamespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{NgrokAuthTokenKey: []byte(token)},
	}
	if err := c.Create(ctx, sec); err != nil {
		return nil, fmt.Errorf("create secret %s/%s: %w", cloud.WebdServiceNamespace, NgrokAuthTokenSecret, err)
	}
	cliout.Info(out, "  created Secret %s/%s from %s", cloud.WebdServiceNamespace, NgrokAuthTokenSecret, source)
	return sec, nil
}
