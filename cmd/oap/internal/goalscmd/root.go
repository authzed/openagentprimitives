// Package goalscmd implements session-authenticated management of private goals.
package goalscmd

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/memclient"
	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/spf13/cobra"
)

// NewCmd builds the oap goals subtree. Mutation request IDs are explicit so a
// retry after a connection failure can return the original accepted revision.
func NewCmd(g *apcmd.Globals) *cobra.Command {
	root := &cobra.Command{Use: "goals", Short: "Manage private goals for a session's owner and agent class", Long: "Manage durable goals using an authenticated, non-delegated session. Execution requires separate bounded human consent. Responses are JSON."}
	for _, op := range []string{"list", "get", "runs", "create", "update"} {
		root.AddCommand(newOperation(g, op))
	}
	return root
}

func newOperation(g *apcmd.Globals, op string) *cobra.Command {
	var req goals.Request
	req.Operation = op
	var due, timezone, title, outcome string
	use := op + " <session>"
	argc := 1
	if op == "get" || op == "runs" || op == "update" {
		use += " <goal-id>"
		argc = 2
	}
	short := op + " goals"
	if op == "runs" {
		short = "List execution history for a goal"
	}
	cmd := &cobra.Command{Use: use, Short: short, Args: cobra.ExactArgs(argc)}
	f := cmd.Flags()
	switch op {
	case "list", "runs":
		if op == "list" {
			f.StringVar((*string)(&req.List.State), "state", "", "Filter by state")
		}
		f.StringVar(&req.List.After, "after", "", "Continue from the previous response's next cursor")
		f.IntVar(&req.List.Limit, "limit", 50, "Page size (1–100)")
	case "create", "update":
		f.StringVar(&title, "title", "", "Short goal title")
		f.StringVar(&outcome, "outcome", "", "Desired outcome")
		f.StringVar(&due, "due-at", "", "Intent only: RFC3339 due timestamp")
		f.StringVar(&timezone, "timezone", "", "IANA timezone")
		if op == "create" {
			f.StringVar(&req.Create.RequestID, "request-id", "", "Stable idempotency key; reuse for identical retries")
		} else {
			f.StringVar(&req.Change.RequestID, "request-id", "", "Stable idempotency key; reuse for identical retries")
			f.Int64Var(&req.Change.Revision, "revision", 0, "Expected current revision (required)")
			f.StringVar(&req.Change.Action, "action", "revise", "revise, activate, pause, resume, cancel, or complete")
			f.BoolVar(&req.Change.ClearDue, "clear-due", false, "Remove due timestamp")
			req.Change.Result = &goals.Result{}
			f.StringVar(&req.Change.Result.Summary, "summary", "", "Reported completion summary")
			f.StringSliceVar(&req.Change.Result.Evidence, "evidence", nil, "Completion evidence references")
		}
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if argc == 2 {
			req.ID = args[1]
			req.Change.ID = args[1]
		}
		var dueAt *time.Time
		if due != "" {
			t, err := time.Parse(time.RFC3339, due)
			if err != nil {
				return fmt.Errorf("--due-at: %w", err)
			}
			dueAt = &t
		}
		if op == "create" {
			if req.Create.RequestID == "" || title == "" || outcome == "" {
				return fmt.Errorf("--request-id, --title, and --outcome are required")
			}
			req.Create.Title = title
			req.Create.Outcome = outcome
			req.Create.DueAt = dueAt
			req.Create.Timezone = timezone
		}
		if op == "update" {
			if req.Change.RequestID == "" || req.Change.Revision < 1 {
				return fmt.Errorf("--request-id and --revision are required")
			}
			if f.Changed("title") {
				req.Change.Title = &title
			}
			if f.Changed("outcome") {
				req.Change.Outcome = &outcome
			}
			if f.Changed("timezone") {
				req.Change.Timezone = &timezone
			}
			req.Change.DueAt = dueAt
			if !f.Changed("summary") && !f.Changed("evidence") {
				req.Change.Result = nil
			}
		}
		b, err := g.Bundle()
		if err != nil {
			return err
		}
		conn, err := memclient.Connect(cmd.Context(), b, args[0], "")
		if err != nil {
			return err
		}
		defer conn.Close()
		if op == "create" || op == "update" {
			// Resolve the domain through the server; local flags never select an owner.
			res, err := conn.Client.Goals(cmd.Context(), b.Namespace, args[0], goals.Request{Operation: "list", List: goals.ListRequest{Limit: 1}})
			if err != nil {
				return err
			}
			req.Resource = res.Resource
		}
		res, err := conn.Client.Goals(cmd.Context(), b.Namespace, args[0], req)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	return cmd
}
