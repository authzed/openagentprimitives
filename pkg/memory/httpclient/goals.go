package httpclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	webgoals "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalconsent"
)

func (c *Client) Goals(ctx context.Context, ns, name string, req webgoals.Request) (webgoals.Response, error) {
	var out webgoals.Response
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/goals/%s/%s", url.PathEscape(ns), url.PathEscape(name)), req, &out)
	return out, err
}

// GoalConflict rebuilds the goal sentinel a 409 from the goal server stands
// for, wrapping err; any other err is returned as it is. The server answers
// every goals.ErrConflict with 409 and the refusal's own message, so a
// conflict that wraps a narrower sentinel still carries that sentinel's text:
// a second reply after a hand-off comes back as goals.ErrReplyHandedOff, and
// every other conflict (a run no longer current, a consent no longer bound, a
// stale revision) as plain goals.ErrConflict.
func GoalConflict(err error) error {
	var se *statusError
	if !errors.As(err, &se) || se.code != http.StatusConflict {
		return err
	}
	if strings.Contains(se.msg, webgoals.ErrReplyHandedOff.Error()) {
		return fmt.Errorf("%w: %v", webgoals.ErrReplyHandedOff, err)
	}
	return fmt.Errorf("%w: %v", webgoals.ErrConflict, err)
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

// ManageGoals calls the goal server's management API (channelsd's token
// only). A refusal is returned as the goal sentinel its status names —
// goals.ErrDenied (403), ErrNotFound (404), ErrInvalid (400), ErrConflict
// (409) — wrapped with the server's reason.
func (c *Client) ManageGoals(ctx context.Context, req webgoals.ManageRequest) (webgoals.ManageResponse, error) {
	var out webgoals.ManageResponse
	err := c.do(ctx, http.MethodPost, "/goals/manage", req, &out)
	if err == nil {
		return out, nil
	}
	code, ok := StatusCode(err)
	if !ok {
		return webgoals.ManageResponse{}, err
	}
	var sentinel error
	switch code {
	case http.StatusForbidden:
		sentinel = webgoals.ErrDenied
	case http.StatusNotFound:
		sentinel = webgoals.ErrNotFound
	case http.StatusBadRequest:
		sentinel = webgoals.ErrInvalid
	case http.StatusConflict:
		sentinel = webgoals.ErrConflict
	case http.StatusPreconditionFailed:
		sentinel = webgoals.ErrPlanChanged
	default:
		return webgoals.ManageResponse{}, err
	}
	var se *statusError
	reason := err.Error()
	if errors.As(err, &se) {
		reason = strings.TrimSpace(se.msg)
	}
	return webgoals.ManageResponse{}, fmt.Errorf("%w: %s", sentinel, reason)
}
