// pkg/authz/spicedb/objectid.go — composing SpiceDB object ids out of Kubernetes
// coordinates, and refusing the ones that cannot be composed at all.
//
// SpiceDB's object_id grammar is NARROWER than a Kubernetes object name's. An
// object id matches `^(([a-zA-Z0-9/_|\-=+]{1,1024})|\*)$`; a Kubernetes name is
// a DNS-1123 subdomain, which also permits `.`. So a perfectly legal CR name
// like "support.bot" produces a relationship write SpiceDB rejects with
// InvalidArgument — forever, for that object's whole life, since a name cannot
// be edited.
//
// That is a PERMANENT failure wearing a transient failure's clothes: the gRPC
// error looks like any other write error, so a caller that requeues on error
// retries it every backoff interval until the object is deleted. Composing ids
// through this file makes the impossibility detectable (errors.Is against
// ErrUnrepresentableObjectID) so callers can surface it instead of spinning.
package spicedb

import (
	"errors"
	"fmt"
	"regexp"
)

// ErrUnrepresentableObjectID reports that the Kubernetes coordinates handed in
// cannot be expressed as a SpiceDB object id at all.
//
// It is PERMANENT. Retrying cannot fix it, and neither can any operator action
// short of recreating the object under a different name. Callers that requeue
// on error must branch on this and surface it instead.
var ErrUnrepresentableObjectID = errors.New("spicedb: not representable as an object id")

// objectIDPattern is SpiceDB's own object_id grammar, minus the `*` wildcard
// alternative (a wildcard is never a valid *resource* id here — it would mean
// "every object of this type").
//
// The length bound is checked separately rather than as a `{1,1024}` repeat:
// Go's RE2 refuses a repeat count above 1000, so writing it inline panics at
// package init.
var objectIDPattern = regexp.MustCompile(`^[a-zA-Z0-9/_|\-=+]+$`)

// maxObjectIDLen is SpiceDB's object_id length ceiling.
const maxObjectIDLen = 1024

// AgentIdentityObjectID composes the `agentidentity` object id for a namespaced
// AgentIdentity: "<namespace>/<name>".
//
// The SINGLE site that composes it, so the relationship write
// (EnsureAgentIdentityPlatform) and the permission check
// (CheckAgentIdentityUpdateCredential) can never name different objects — and
// so the charset constraint is enforced once rather than remembered twice.
func AgentIdentityObjectID(ns, name string) (string, error) {
	// An empty half composes an id SpiceDB would happily ACCEPT ("default/",
	// "/support-bot") while meaning nothing — a permission check against it
	// silently answers about an object no reconciler ever links. Refuse instead:
	// the caller is holding an unresolved reference, and fail-closed is the only
	// safe reading of that.
	if ns == "" || name == "" {
		return "", fmt.Errorf("%w: agentidentity namespace=%q name=%q (both halves are required; an empty one "+
			"composes an id that resolves to nothing and would answer every check with a silent no)",
			ErrUnrepresentableObjectID, ns, name)
	}
	id := ns + "/" + name
	if !objectIDPattern.MatchString(id) || len(id) > maxObjectIDLen {
		return "", fmt.Errorf("%w: agentidentity %q (a SpiceDB object id admits only [a-zA-Z0-9/_|-=+] and at most "+
			"%d characters, while a Kubernetes name also admits '.'; this identity can never be linked to the platform "+
			"object, so agentidentity#update_credential is permanently unsatisfiable for it)",
			ErrUnrepresentableObjectID, id, maxObjectIDLen)
	}
	return id, nil
}

// AgentClassObjectID composes the `agentclass` object id for a namespaced
// AgentClass: "<namespace>/<name>".
//
// The SINGLE site that composes it, so the relationship write
// (EnsureAgentClassPlatform) and the lookup behind the browser's agent picker
// (LookupStartableClasses) can never name different objects — and so the
// charset constraint is enforced once rather than remembered twice.
//
// Note the asymmetry with the lookup: this validates on the WRITE side, while
// LookupStartableClasses splits ids coming back OUT of SpiceDB and reports the
// ones that do not split cleanly. Both directions are needed — an id this
// refuses is never written, but an id written by an older operator (or by
// hand) can still come back from a lookup.
func AgentClassObjectID(ns, name string) (string, error) {
	// An empty half composes an id SpiceDB would happily ACCEPT ("default/",
	// "/support-bot") while meaning nothing — a permission check against it
	// silently answers about an object no reconciler ever links. Refuse
	// instead: the caller is holding an unresolved reference, and fail-closed
	// is the only safe reading of that.
	if ns == "" || name == "" {
		return "", fmt.Errorf("%w: agentclass namespace=%q name=%q (both halves are required; an empty one "+
			"composes an id that resolves to nothing and would answer every check with a silent no)",
			ErrUnrepresentableObjectID, ns, name)
	}
	id := ns + "/" + name
	if !objectIDPattern.MatchString(id) || len(id) > maxObjectIDLen {
		return "", fmt.Errorf("%w: agentclass %q (a SpiceDB object id admits only [a-zA-Z0-9/_|-=+] and at most "+
			"%d characters, while a Kubernetes name also admits '.'; this class can never be linked to the platform "+
			"object, so agentclass#start_session is permanently unsatisfiable for it and it will never appear in "+
			"the browser's agent picker)",
			ErrUnrepresentableObjectID, id, maxObjectIDLen)
	}
	return id, nil
}
