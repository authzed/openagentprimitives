package main

import (
	"context"
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/authz/plangate/hold"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagedecision"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Signals carry the sender's context. This decision lookup is the operator's
// own read, not a read authorized for or returned to that sender.
func newClosureDenialStamper(k8s client.Client, mem memory.Memory) *hold.ClosureDenialStamper {
	return hold.NewClosureDenialStamper(hold.ClosureDenialStamperDeps{
		Client: k8s,
		Denied: func(ctx context.Context, scope memory.Scope) (bool, error) {
			ctx = memory.WithCaller(memory.WithSystemApproval(memory.WithoutTokenSession(ctx), "operator:closure-denial-stamp"), "system:operator")
			recs, err := infoleakagedecision.List(ctx, mem, scope)
			if err != nil {
				return false, err
			}
			for _, r := range recs {
				if r.Decision == infoleakagedecision.DecisionDenied {
					return true, nil
				}
			}
			return false, nil
		},
		Logger: slog.Default(),
	})
}
