package memory

// mintInternal produces an internal-tier approval. It is UNEXPORTED on purpose:
// only code inside pkg/memory (the provenance verify-on-write path in Local.Put)
// may authorize an append-only write, so the guarantee "only pkg/memory can
// authorize a tamper-evident write" is enforced by the package boundary, not a
// runtime convention. Panics if perm is not internal-tier (programmer error).
func mintInternal(perm Permission, resource, component string) Approval {
	if !perm.internal() {
		panic("memory.mintInternal: non-internal perm " + string(perm))
	}
	return Approval{perm: perm, resource: resource, source: "internal", evidence: component}
}
