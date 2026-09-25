package sessioncmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	apspicedb "github.com/authzed/openagentprimitives/cmd/oap/internal/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// newSessionSlotsCmd lists the instances a session currently holds through its
// slots. Revocation is only meaningful if an operator can first SEE what is
// held: a grant names a hashed object id for a value slot, so there is no other
// way to discover what a session may reach.
func newSessionSlotsCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "slots <session>",
		Short: "List the instances bound into this session's slots",
		Long: `List every instance this AgentSession currently holds through a slot grant.

Each row is "<resourceType>:<id>#<permission>" — the object, and the permission
that grant confers. The permission is part of the identity, not decoration:
grants are per-permission, so a read-scoped grant and a write-scoped grant on the
same instance are different objects. For a
VALUE slot (a URL, a path) the id is the hash the tool's own permission check
computes, not the original value — the value goes to the tool, SpiceDB only ever
sees the hash. That is by design, and it is why this listing exists: without it
the held set is not discoverable at all.

Grants also expire on their own (bounded by the session's wall-clock lifetime
cap), so an empty list may mean "expired" rather than "never granted".`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionName := args[0]
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			cl, err := apspicedb.NewClientFromEnv()
			if err != nil {
				return err
			}
			defer cl.Close()

			held, err := cl.ListSlotGrants(cmd.Context(), b.Namespace, sessionName)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(held) == 0 {
				fmt.Fprintf(out, "no slot grants on agentsession:%s/%s\n", b.Namespace, sessionName)
				return nil
			}
			for _, h := range held {
				fmt.Fprintf(out, "%s:%s#%s\n", h.ResourceType, h.ResourceID, h.Permission)
			}
			return nil
		},
	}
}

// newSessionRevokeSlotCmd withdraws one instance from a session.
//
// Surgical rather than all-or-nothing: ending the session is already possible,
// and an operator who has to choose between "kill the whole session" and "leave
// it holding something it should not" will usually pick neither.
func newSessionRevokeSlotCmd(g *apcmd.Globals) *cobra.Command {
	var asUser string
	cmd := &cobra.Command{
		Use:   "revoke-slot <session> <resourceType>:<id>#<permission>",
		Short: "Withdraw one instance from a session's slots",
		Long: `Remove a single slot grant, so the session can no longer reach that instance.

The next authorization check denies — a slot grant is a SpiceDB tuple, and its
removal is visible immediately to any later check. Work already dispatched is
not clawed back; the revocation applies from the next call onward.

Standing is symmetric with APPROVAL, plus platform admin: whoever could have
granted the instance can take it back. Pass --as to name the canonical identity
the check runs against; without it the command refuses rather than acting
unattributed.

Use 'oap session slots <session>' to see what is held. For a value slot the id is
the hash the tool computes, which is what this command expects.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionName, ref := args[0], args[1]

			// The PERMISSION is required, not optional: grants are per-permission,
			// so a reference without one names no tuple. A read-scoped grant and a
			// write-scoped grant on the same instance are different objects, and
			// guessing which was meant could revoke the wrong one — or nothing.
			resourceType, rest, ok := strings.Cut(ref, ":")
			if !ok {
				return fmt.Errorf("instance %q: must be \"<resourceType>:<id>#<permission>\" — see 'oap session slots %s'", ref, sessionName)
			}
			resourceID, permission, ok := strings.Cut(rest, "#")
			if !ok || resourceType == "" || resourceID == "" || permission == "" {
				return fmt.Errorf("instance %q: must be \"<resourceType>:<id>#<permission>\" — see 'oap session slots %s'", ref, sessionName)
			}
			if asUser == "" {
				// Refusing beats acting unattributed: revocation is an
				// authorization decision, and one taken by nobody in particular
				// cannot be audited or gated.
				return fmt.Errorf("--as is required: revocation is gated on approver standing, so it must name who is asking " +
					"(use 'oap identity canonical-id <email>')")
			}

			b, err := g.Bundle()
			if err != nil {
				return err
			}
			cl, err := apspicedb.NewClientFromEnv()
			if err != nil {
				return err
			}
			defer cl.Close()

			return revokeSlot(cmd.Context(), cl, b.Namespace, sessionName,
				identity.CanonicalFromTrusted(asUser,
					"--as value typed on the oap CLI (unverified; see G5/G7)"), resourceType, resourceID, permission, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&asUser, "as", "",
		"canonical identity the revoke-standing check runs against (required)")
	return cmd
}

// slotRevokeClient is the SpiceDB surface revokeSlot needs, narrowed so a fake
// can stand in for tests without a live SpiceDB connection — see
// slots_test.go. *spicedb.Client satisfies it.
type slotRevokeClient interface {
	authz.SlotRevokeChecker
	ListSlotGrants(ctx context.Context, ns, name string) ([]authz.SlotBinding, error)
	Relations() authz.RelWriter
}

// revokeSlot removes one slot grant, after confirming both standing AND that
// the grant actually exists.
//
// The existence check matters because DeleteRelationships against an absent
// tuple is a silent no-op — the RPC returns success either way. session-only
// (the default standing model this branch shipped) makes escaped/hashed
// object ids the norm on the approval and class-default paths, so an operator
// who pastes the raw value they saw (a URL, a path) rather than the escaped
// form is now a likely mistake, not an edge case. Without this check that
// mistake is reported as "revoked" even though nothing was removed.
func revokeSlot(ctx context.Context, cl slotRevokeClient, ns, sessionName string,
	asUser identity.CanonicalUserID, resourceType, resourceID, permission string, out io.Writer,
) error {
	sess := authz.SessionRef{Namespace: ns, Name: sessionName}
	allowed, err := authz.CheckMayRevokeSlot(ctx, cl, sess, asUser)
	if err != nil {
		return fmt.Errorf("checking revoke standing: %w", err)
	}
	if !allowed {
		return fmt.Errorf("%s may not revoke slots on agentsession:%s/%s: "+
			"revoking requires the standing to approve on this session, or platform kill_session",
			asUser, ns, sessionName)
	}

	// TrustedObjectID: resourceID was copy-pasted by the operator from
	// 'oap session slots' output, which itself reads grants back out of
	// SpiceDB (already canonical) — see pkg/authz/spicedb/slot_grants.go's
	// ListSlotGrants. The existence check just below is what catches it when
	// that assumption does not hold — a value that was never actually copied
	// from that listing.
	binding := authz.SlotBinding{ResourceType: resourceType, ResourceID: authz.TrustedObjectID(resourceID), Permission: permission}

	held, err := cl.ListSlotGrants(ctx, ns, sessionName)
	if err != nil {
		return fmt.Errorf("checking current grants: %w", err)
	}
	found := false
	for _, h := range held {
		if h.ResourceType == binding.ResourceType && h.ResourceID == binding.ResourceID && h.Permission == binding.Permission {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no matching grant found for %s:%s#%s on agentsession:%s/%s — "+
			"see 'oap session slots %s' for what is held",
			resourceType, resourceID, permission, ns, sessionName, sessionName)
	}

	if err := authz.RevokeSlots(ctx, cl.Relations(), sess, []authz.SlotBinding{binding}); err != nil {
		return err
	}
	fmt.Fprintf(out, "revoked %s:%s#%s from agentsession:%s/%s\n",
		resourceType, resourceID, permission, ns, sessionName)
	return nil
}
