// Package subagentrequest holds the admission webhook that makes a
// SubagentRequest's parent claim provable.
//
// A delegated child's entire standing derives from SubagentRequest.spec.parent:
// the controller copies it onto the child's spec.parent, and read_transcript,
// approve and the pooled budget ceiling all resolve through that one field. The
// runner's Role grants create on subagentrequests namespace-wide and CANNOT
// pin it by resourceName, because a CR's name is not knowable before it is
// created. So the claim is unconstrained by RBAC, and the authenticated
// principal is the only thing on the request its creator cannot choose.
//
// An owner reference would not do: whoever creates an object sets its owner
// references, so an ownerRef proves only that the creator was willing to type
// one.
package subagentrequest

import (
	"context"
	"net/http"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// Path is the webhook's HTTP path. It must match config/manager/webhook.yaml.
const Path = "/validate-subagentrequest-parent"

// runnerSASuffix and saUsernamePrefix mirror
// pkg/controllers/webhooks/agentsession. They are duplicated rather than
// exported across packages deliberately: the agentsession webhook's copy is
// paired with its OWN matchConditions block, and a shared constant would
// silently couple two independently-configured admission gates. Both must stay
// in sync with BuildRunnerRBAC's `<session>-runner-sa`; the RBAC sufficiency
// harness covers that name.
const (
	runnerSASuffix   = "-runner-sa"
	saUsernamePrefix = "system:serviceaccount:"
)

// Webhook validates SubagentRequest admission requests.
type Webhook struct{ decoder admission.Decoder }

// New constructs the webhook with the scheme decoder.
func New(d admission.Decoder) *Webhook { return &Webhook{decoder: d} }

func (w *Webhook) Handle(_ context.Context, req admission.Request) admission.Response {
	// DELETE carries no object to check. Every other operation that can set or
	// change spec.parent is gated.
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return admission.Allowed("")
	}

	// A non-runner principal is governed by ordinary RBAC, which is a
	// sufficient control for it: only the per-session runner Role grants an
	// unrestricted namespace-scoped create to an untrusted principal. The
	// operator's own aggregate ClusterRole also carries the verb, cluster-wide
	// -- but only so it can grant it onto each runner Role in turn (Kubernetes'
	// RBAC escalation prevention requires a granter to already hold what it
	// grants), and it never creates one of these itself. The operator and
	// human administrators are trusted here for exactly the same reason the
	// agentsession identity webhook trusts them.
	//
	// This branch is normally unreachable in a correctly-registered install:
	// the ValidatingWebhookConfiguration is expected to narrow dispatch with a
	// matchConditions block so only `*-runner-sa` principals are ever dialed.
	// It exists anyway, because a webhook whose correctness depends on its own
	// registration staying right is a webhook that fails open the moment that
	// registration drifts.
	ns, sessionName, isRunner := runnerSession(req.UserInfo.Username)
	if !isRunner {
		return admission.Allowed("requester is not a session runner ServiceAccount")
	}
	if ns == "" || sessionName == "" {
		// Looks like a runner, does not parse into a session. Fail closed: a
		// guard whose unrecognized direction is permissive is not a guard.
		return admission.Denied("the requesting ServiceAccount " +
			req.UserInfo.Username + " cannot be attributed to a session, so its parent claim cannot be verified")
	}

	var sr spiceboxv1alpha1.SubagentRequest
	if err := w.decoder.Decode(req, &sr); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	if sr.Spec.Parent.Namespace != ns || sr.Spec.Parent.Name != sessionName {
		return admission.Denied("SubagentRequest.spec.parent claims " +
			sr.Spec.Parent.Namespace + "/" + sr.Spec.Parent.Name +
			" but the request is from the runner of " + ns + "/" + sessionName +
			": a session may only delegate as itself")
	}
	return admission.Allowed("")
}

// runnerSession splits a per-session runner ServiceAccount's authenticated
// username into the namespace and session it speaks for.
//
// The per-session ServiceAccount is named `<session>-runner-sa` in the
// session's own namespace (BuildRunnerRBAC), so the principal names the one
// session it is entitled to act as. isRunner distinguishes "not a runner at
// all" (ordinary RBAC governs it) from "a runner whose name does not parse"
// (fail closed) — collapsing the two into a single bool is how this check
// would acquire a permissive direction.
//
// The prefix and suffix are both checked against the username as a WHOLE
// before any split on the namespace/name colon, and deliberately so: those
// are the same two checks a matchConditions filter would use to decide
// whether to dial this webhook at all, so a username like
// "system:serviceaccount:foo-runner-sa" — no colon separating a namespace
// from a name — still matches both and would still be dialed. Falling
// through to isRunner=false for it would route a runner-shaped principal to
// the permissive "not a runner at all" branch instead of the fail-closed
// one, which is exactly the gap this function exists to close. This makes
// the parse stricter than pkg/controllers/webhooks/agentsession's
// isSessionRunner, which has this identical gap; the divergence is
// deliberate — do not "fix" this back to match that package.
func runnerSession(username string) (ns, session string, isRunner bool) {
	if !strings.HasPrefix(username, saUsernamePrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(username, saUsernamePrefix)
	if !strings.HasSuffix(rest, runnerSASuffix) {
		return "", "", false
	}
	nsPart, name, ok := strings.Cut(rest, ":")
	if !ok {
		// Looks like a runner SA by both checks above, but there is no colon to
		// split a namespace from a name. Fail closed.
		return "", "", true
	}
	return nsPart, strings.TrimSuffix(name, runnerSASuffix), true
}
