package hooks

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// PoolDestination is a resource's memory pool as an egress destination.
//
// Its audience is whoever holds view_memory on the resource — derived live,
// never stored — which is the whole premise of resource-scoped memory: the
// pool's readers are a property of the resource's permissions at read time,
// not of anything recorded when the entry was written.
//
// Deliberately NOT the slot permission the writing session holds. A session
// may hold (dossier, write_memory) while the pool's readers are whoever has
// view_memory; gating on the slot would compare against the wrong set and
// nothing downstream would notice.
func PoolDestination(lookup func(ctx context.Context, resource, permission string) ([]string, error), objType, objID string) Destination {
	return &poolDestination{lookup: lookup, ref: objType + ":" + objID}
}

type poolDestination struct {
	lookup func(ctx context.Context, resource, permission string) ([]string, error)
	ref    string
}

func (d *poolDestination) Audience(ctx context.Context) ([]string, error) {
	if d.lookup == nil {
		return nil, fmt.Errorf("info-leakage: no subject lookup wired; the audience of %s is unknown", d.ref)
	}
	subs, err := d.lookup(ctx, d.ref, memory.PermissionViewMemory)
	if err != nil {
		return nil, fmt.Errorf("info-leakage: expanding %s#%s: %w", d.ref, memory.PermissionViewMemory, err)
	}
	return subs, nil
}

// A permission expansion always enumerates its recipients, so neither bypass
// branch — both written for channel transports that cannot — applies.
func (d *poolDestination) Capability() channelkinds.Capability { return channelkinds.CapabilityFull }

func (d *poolDestination) Describe() string { return d.ref }

// A cross-pool write is refused, fail-closed, with no prompt — the design
// spec's ruling, taken over asking an approver with both on the table.
//
// The mechanism is what makes the ruling more than a preference. Approving a
// leakage_share writes a grant over the taint's SOURCE resources, so a yes
// here would not permit this one write; it would widen who may read the
// resource the session read FROM. Nobody chose that, and a card whose copy
// says "would share with…" does not tell the approver it is what they are
// agreeing to.
func (d *poolDestination) MayAskAnApprover() bool { return poolMayAskAnApprover }

// poolMayAskAnApprover is that answer as a package fact, because one caller
// has a tool name and no destination object to ask. The per-datum legs resolve
// a destination's audience through a closure rather than a Destination, so
// InfoLeakAudience.toolCallMayAskAnApprover reads this — restating `false`
// there is how the two paths would drift, and a card raised on the path that
// drifted is the widening the ruling exists to prevent.
const poolMayAskAnApprover = false
