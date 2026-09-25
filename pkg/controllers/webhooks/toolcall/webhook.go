// Package toolcall holds the ToolCall admission webhook. A ToolCall is the
// instruction the privileged operator follows to exec a tool in the sandbox
// (and to inject resolved Secret values). The runner creates one AFTER its
// SpiceDB authz + approval check. This webhook pins that decision so it
// cannot be tampered with between creation and execution:
//
//   - CREATE: the ToolCall must be owned by an AgentSession (so a free-
//     floating / forged ToolCall is rejected), and every credential source
//     must reference a Secret in the ToolCall's own namespace (no
//     cross-namespace secret theft).
//   - UPDATE: the spec is immutable — no principal may rewrite the tool,
//     args, credentials, stdin, etc. of an already-admitted call. The
//     operator only writes the status subresource, so this breaks no
//     legitimate flow.
package toolcall

import (
	"context"
	"net/http"
	"reflect"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// Path is the webhook route; must match the ValidatingWebhookConfiguration.
const Path = "/validate-toolcall"

// Webhook validates ToolCall create/update admission requests.
type Webhook struct{ decoder admission.Decoder }

// New constructs the webhook with the scheme decoder.
func New(d admission.Decoder) *Webhook { return &Webhook{decoder: d} }

func (w *Webhook) Handle(_ context.Context, req admission.Request) admission.Response {
	var tc spiceboxv1alpha1.ToolCall
	if err := w.decoder.Decode(req, &tc); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	switch req.Operation {
	case admissionv1.Create:
		if !hasAgentSessionOwner(&tc) {
			return admission.Denied("ToolCall must be owned by an AgentSession (ownerReference Kind=AgentSession); an unpinned ToolCall is refused")
		}
		if resp, bound := denyIfNotOwnSession(req, &tc); !bound {
			return resp
		}
		if err := tc.ValidateCredentialSourceNamespaces(); err != nil {
			return admission.Denied(err.Error())
		}
		if err := tc.ValidateCredentialSourceOwnership(); err != nil {
			return admission.Denied(err.Error())
		}
		return admission.Allowed("")

	case admissionv1.Update:
		var old spiceboxv1alpha1.ToolCall
		if err := w.decoder.DecodeRaw(req.OldObject, &old); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		// The ToolCall spec is the authorized instruction; it is frozen after
		// creation. Status (a subresource) does not flow through this path.
		if !reflect.DeepEqual(old.Spec, tc.Spec) {
			return admission.Denied("ToolCall.spec is immutable once created (the runner's authorized tool/args/credentials cannot be rewritten before execution)")
		}
		return admission.Allowed("")

	default:
		return admission.Allowed("")
	}
}

// hasAgentSessionOwner reports whether tc carries an AgentSession
// ownerReference in this API group (ownerRefs are always same-namespace for
// a namespaced resource, so this pins the ToolCall to a real session).
func hasAgentSessionOwner(tc *spiceboxv1alpha1.ToolCall) bool {
	return AgentSessionOwnerName(tc) != ""
}

// AgentSessionOwnerName returns the name of tc's AgentSession ownerReference,
// or "" when there is none. Exported so the toolcall controller can re-check
// the same binding this webhook enforces.
func AgentSessionOwnerName(tc *spiceboxv1alpha1.ToolCall) string {
	group := spiceboxv1alpha1.SchemeBuilder.GroupVersion.Group
	for _, o := range tc.OwnerReferences {
		if o.Kind == "AgentSession" && strings.HasPrefix(o.APIVersion, group+"/") {
			return o.Name
		}
	}
	return ""
}

// saUsernamePrefix and runnerSASuffix bracket a per-session runner's
// authenticated ServiceAccount username:
// system:serviceaccount:<ns>:<session>-runner-sa.
const (
	saUsernamePrefix = "system:serviceaccount:"
	runnerSASuffix   = "-runner-sa"
)

// denyIfNotOwnSession binds the CREATOR to the session the ToolCall names.
//
// hasAgentSessionOwner proves only that the creator was willing to type an
// ownerReference — name and UID are plain fields it writes. That mattered
// because the runner Role grants toolcalls create/get/list with no
// resourceNames, sessions share a namespace, and the controller resolves both
// the sandbox to exec into and the AgentSession whose use_token grant is
// consulted from the creator-supplied spec.session. Naming a sibling made the
// SIBLING's own grant authorize the call, which then ran in the sibling's
// sandbox with the sibling's credentials injected.
//
// Non-runner principals (the operator, a human applying a manifest) have no
// session to be bound to and pass through; the other CREATE checks still apply
// to them.
//
// Returns ok=true when the request may proceed. This is the admission half;
// the toolcall controller re-checks the same binding before Broker.Resolve, so
// a webhook that is down or unregistered is not a bypass.
func denyIfNotOwnSession(req admission.Request, tc *spiceboxv1alpha1.ToolCall) (admission.Response, bool) {
	ns, session, isRunner := runnerSession(req.UserInfo.Username)
	if !isRunner {
		return admission.Allowed(""), true
	}
	if ns == "" || session == "" {
		// Runner-shaped by both checks, but it does not parse into a session.
		// Fail closed: a guard whose unrecognized direction is permissive is
		// not a guard.
		return admission.Denied("the requesting ServiceAccount " + req.UserInfo.Username +
			" cannot be attributed to a session, so its ToolCall's ownership claim cannot be verified"), false
	}
	if tc.Namespace != ns {
		return admission.Denied("ToolCall in namespace " + tc.Namespace +
			" was created by the runner of " + ns + "/" + session +
			": a session may only issue tool calls in its own namespace"), false
	}
	if owner := AgentSessionOwnerName(tc); owner != session {
		return admission.Denied("ToolCall claims AgentSession " + tc.Namespace + "/" + owner +
			" as its owner but the request is from the runner of " + ns + "/" + session +
			": a session may only issue tool calls as itself"), false
	}
	return admission.Allowed(""), true
}

// runnerSession splits a per-session runner ServiceAccount's authenticated
// username into the namespace and session it speaks for.
//
// The prefix and suffix are both checked against the username as a WHOLE
// before any split on the colon, matching what a matchConditions filter would
// use to decide whether to dial this webhook: a username like
// "system:serviceaccount:foo-runner-sa" matches both and would still arrive
// here, so it must reach the fail-closed branch rather than the permissive
// "not a runner at all" one. This mirrors the identical function in
// pkg/controllers/webhooks/subagentrequest, whose doc comment explains the
// same reasoning at length.
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
		return "", "", true
	}
	return nsPart, strings.TrimSuffix(name, runnerSASuffix), true
}
