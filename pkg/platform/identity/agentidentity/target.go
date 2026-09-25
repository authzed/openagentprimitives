// target.go — resolving WHOSE credential a credential-update link covers, and
// where that credential's value lives.
//
// Shared by the two components on either side of the browser boundary:
//
//   - identityd (in webd) resolves it to ROUTE the click: an agent-owned
//     credential takes a different gate and write path than a person's own, and
//     its menu row must render a paste form rather than an OAuth button that
//     would link the visitor's own account.
//   - admind (in the operator) resolves it AGAIN, independently, to decide what
//     it is being asked to authorize and write.
//
// The operator's resolution is authoritative. webd asserts only WHICH SUBJECT is
// signed in and WHICH LINK was clicked (both riding inside the HMAC-signed
// deep-link); it never names the AgentIdentity, the Secret, or a verdict. One
// shared function rather than two implementations is what keeps the operator's
// decision about the same object the browser was shown.
package agentidentity

import (
	"context"
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// ErrAmbiguousOwner is the fail-closed verdict when a credential-update link
// cannot be resolved to exactly one credential owner.
//
// Refusing beats guessing, because the two candidate destinations are a shared
// bot credential and a person's private one. "I could not tell whose this is"
// and "it is the clicker's own" are not the same statement, and treating them
// alike is how a pasted bot token lands in an admin's personal identity.
var ErrAmbiguousOwner = errors.New("agentidentity: credential-update request does not resolve to a single credential owner")

// RequestTarget names the AgentIdentity-owned credential a credential-update
// request covers.
type RequestTarget struct {
	// Namespace + Name identify the AgentIdentity CR.
	Namespace string
	Name      string
	// Credential is the credential name within that identity.
	Credential string
	// SecretRef is the backing Secret the operator recorded when the card
	// opened (status.credentialSecretRef), or nil when the request records
	// none. Passed to PutToken as the expected destination — the reconciler
	// marks the request Fulfilled by watching THIS Secret's content, so a write
	// anywhere else reproduces the "nobody updated it" lie the flow exists to
	// remove.
	SecretRef *spiceboxv1alpha1.NamespacedRef
}

// String renders the target for logs (no secret material).
func (t *RequestTarget) String() string {
	return t.Namespace + "/" + t.Name + "#" + t.Credential
}

// ResolveRequestTarget reports which AgentIdentity-owned credential the
// session's credential-update request(s) cover, or (nil, nil) when they cover a
// user-owned credential (or when the session has no matching request at all).
//
// The authority is the CredentialUpdateRequest's own status.resolvedCredential,
// written by the operator's reconciler after it independently re-verified the
// credential — NOT anything the caller supplies beyond the session and the
// credential names, both of which ride inside the signed deep-link. That is
// what stops a caller naming an identity the request never referenced.
//
// A non-nil error means "refuse". Callers must not fall back to a user-owned
// write on error.
func ResolveRequestTarget(ctx context.Context, c client.Reader, sessionNS, sessionName string, credentials []string) (*RequestTarget, error) {
	if sessionNS == "" || sessionName == "" {
		return nil, fmt.Errorf("%w: malformed session ref %q/%q", ErrAmbiguousOwner, sessionNS, sessionName)
	}
	if len(credentials) == 0 {
		return nil, fmt.Errorf("%w: no credential names to resolve", ErrAmbiguousOwner)
	}

	var list spiceboxv1alpha1.CredentialUpdateRequestList
	if err := c.List(ctx, &list, client.InNamespace(sessionNS)); err != nil {
		return nil, fmt.Errorf("list CredentialUpdateRequests in %s: %w", sessionNS, err)
	}

	// Collect every request in this session that resolved to one of the named
	// credentials. Phase is deliberately NOT filtered: a request that has
	// already expired still tells the truth about WHOSE credential it named,
	// and that is the only fact being read here. The permission check, not the
	// request's phase, is what authorizes a write.
	var found *RequestTarget
	userOwned := false
	for i := range list.Items {
		cur := &list.Items[i]
		if cur.Spec.SessionRef.Namespace != sessionNS || cur.Spec.SessionRef.Name != sessionName {
			continue
		}
		rc := cur.Status.ResolvedCredential
		if rc == nil || !containsString(credentials, rc.Credential) {
			continue
		}
		if !rc.AgentOwned() {
			userOwned = true
			continue
		}
		cand := &RequestTarget{
			Namespace:  rc.Namespace,
			Name:       rc.Name,
			Credential: rc.Credential,
			SecretRef:  cur.Status.CredentialSecretRef,
		}
		if found == nil {
			found = cand
			continue
		}
		if found.Namespace != cand.Namespace || found.Name != cand.Name || found.Credential != cand.Credential {
			return nil, fmt.Errorf("%w: session %s/%s has requests naming both %s and %s",
				ErrAmbiguousOwner, sessionNS, sessionName, found, cand)
		}
		// Same identity + credential asked twice (the ask budget permits a
		// repeat). Prefer whichever record actually carries a backing-Secret
		// baseline, so the write is checked against a real destination.
		if found.SecretRef == nil {
			found.SecretRef = cand.SecretRef
		}
	}
	if found == nil {
		return nil, nil
	}
	if userOwned {
		// One link covering both a shared bot credential and a person's own is
		// not a shape anything mints, and there is no safe way to serve it: the
		// same pasted value would have to go to two different destinations.
		return nil, fmt.Errorf("%w: session %s/%s names both an agent-owned and a user-owned credential for %v",
			ErrAmbiguousOwner, sessionNS, sessionName, credentials)
	}
	// The write keys on ONE credential; a multi-credential request set would
	// make "which one did the form mean" ambiguous at submit. channelsd mints
	// exactly one credential per credential-update link, so this only fires on a
	// shape nothing produces.
	if len(credentials) != 1 {
		return nil, fmt.Errorf("%w: an agent-owned credential-update link must cover exactly one credential, got %v",
			ErrAmbiguousOwner, credentials)
	}
	return found, nil
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
