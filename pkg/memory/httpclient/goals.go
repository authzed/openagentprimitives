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
