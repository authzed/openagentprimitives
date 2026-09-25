package imagemode

import "strings"

// RegistryRootOf returns the registry root of a container image reference —
// the portion before the image name — for a registry-qualified ref with OR
// without an "@sha256:…" digest. ok is false when the ref names no registry we
// could push to: a bare local tag ("spicebox-operator:dev"), or a Docker Hub
// org path ("acme-infra/spicedb:latest") whose first segment is not a registry
// host.
//
// This is the digest-agnostic sibling of the drift check's parsePinnedRef.
// That one exists to compare a DEPLOYED digest against the registry's current
// one, so it rightly demands a digest; this answers "which registry does this
// cluster pull from", which an unpinned ref (--no-digest-pin) answers just as
// well.
func RegistryRootOf(ref string) (string, bool) {
	if at := strings.LastIndex(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	slash := strings.LastIndex(ref, "/")
	if slash < 0 {
		return "", false
	}
	root := ref[:slash]
	host := root
	if i := strings.Index(root, "/"); i >= 0 {
		host = root[:i]
	}
	// Docker's own rule for telling a registry host from a Hub org path: a host
	// carries a dot or a port, or is literally localhost.
	if host != "localhost" && !strings.ContainsAny(host, ".:") {
		return "", false
	}
	return root, true
}
