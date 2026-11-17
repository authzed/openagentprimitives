package httpclient

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	webgoals "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalconsent"
)

func (c *Client) Goals(ctx context.Context, ns, name string, req webgoals.Request) (webgoals.Response, error) {
	var out webgoals.Response
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/goals/%s/%s", url.PathEscape(ns), url.PathEscape(name)), req, &out)
	return out, err
}

func (c *Client) CommitGoalConsent(ctx context.Context, entry memory.Entry) error {
	return c.do(ctx, http.MethodPost, "/goals/decision", entry, nil)
}

// CheckGoalConsent asks the goal server whether decider may decide the signed
// request, before any decision is recorded. A refusal carries its reason.
func (c *Client) CheckGoalConsent(ctx context.Context, request memory.Entry, decider string, approved bool) error {
	return c.do(ctx, http.MethodPost, "/goals/decision/check", goalconsent.Check{Request: request, Decider: decider, Approved: approved}, nil)
}

// GroupGoalRun asks the goal server whether ns/name is the live run of a
// group-owned goal, so a decision on it may arrive by channel ingress. Any
// refusal (a private run, a concluded or unbound one) is an error.
func (c *Client) GroupGoalRun(ctx context.Context, ns, name string) (bool, error) {
	var out webgoals.RunDecisionIngress
	if err := c.do(ctx, http.MethodPost, "/goals/decision/ingress", webgoals.RunDecisionIngressCheck{Namespace: ns, Name: name}, &out); err != nil {
		return false, err
	}
	return out.Group, nil
}
