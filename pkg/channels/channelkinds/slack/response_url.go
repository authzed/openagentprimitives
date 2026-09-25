// pkg/channels/channelkinds/slack/response_url.go
//
// Shared helper for posting to Slack's interactive response_url endpoint.
// Both approval senders (tool_approval, info_leakage_approval) use this
// to edit ephemerals + decision messages in place. The package-level
// function accepts an injected *http.Client so tests can stub it without
// spinning up real Slack infrastructure.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// responseURLHost is the only host a response_url may name. Slack issues
// interactive response_urls under https://hooks.slack.com/actions/… (and
// /commands/… for slash commands) on every workspace, Enterprise Grid
// included; there is no per-workspace variant to accommodate.
const responseURLHost = "hooks.slack.com"

// defaultResponseURLClient is the client used when a caller injects none. It
// is SSRF-guarded (safehttp refuses to dial loopback/private/link-local, and
// re-validates every redirect hop) and carries a timeout, which
// http.DefaultClient does not — a hung POST to a response_url would otherwise
// block a sender goroutine indefinitely.
//
// It is defence in depth BEHIND validateResponseURL, not a substitute for it:
// safehttp guards the address family, not the identity of the host, so a
// public attacker-chosen host passes it cleanly. The allowlist is what makes
// the destination non-negotiable; safehttp is what stops a poisoned or rebound
// hooks.slack.com resolving into the cluster.
var defaultResponseURLClient = sync.OnceValue(safehttp.Client)

// validateResponseURL refuses any destination that is not Slack's own
// response_url endpoint over TLS.
//
// This is a fail-closed choke point, and it exists because the URL is NOT
// ours. On the sender path it is InteractionAppliedPayload.ResponseRef /
// InteractionDecisionRejectedPayload.ResponseRef — publisher-controlled JSON
// that arrived over NATS. A runner publishing out.interaction_applied on its
// OWN session subject passes every subject-authority check on the path, so
// without this the publisher chooses an arbitrary host and channelsd makes the
// request on its behalf: an unauthenticated POST of attacker-chosen JSON to
// the operator's debug/memory server, SpiceDB, the API server, or a node
// metadata endpoint. It is equally an egress bypass — a session pinned to
// effectiveNetworkMode: none has no egress of its own; channelsd does.
//
// The check is on the exact host, not a suffix: a suffix test would accept
// "hooks.slack.com.attacker.example". https is required because the body is a
// decision record.
func validateResponseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid response_url (want https://%s/…): %w", responseURLHost, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("refusing response_url with scheme %q: only https://%s/… is allowed", u.Scheme, responseURLHost)
	}
	if u.Hostname() != responseURLHost {
		return fmt.Errorf("refusing response_url host %q: only https://%s/… is allowed", u.Hostname(), responseURLHost)
	}
	return nil
}

// postToResponseURL POSTs a JSON body to a Slack interactive response_url.
// body is marshalled to JSON; callers typically pass a map[string]any with
// "replace_original": true plus either "text" or "blocks". An SSRF-guarded
// client is used when client is nil; tests inject an httptest.Server-backed
// client via the sender's httpClient field.
//
// The destination is validated before anything is dialled — see
// validateResponseURL. The refusal is returned, never swallowed: both callers
// either log it (sendDecisionApplied, whose other surfaces still get edited)
// or return it (sendRejected, which has no other surface).
func postToResponseURL(ctx context.Context, client *http.Client, rawURL string, body any) error {
	if rawURL == "" {
		return fmt.Errorf("postToResponseURL: empty response_url")
	}
	if err := validateResponseURL(rawURL); err != nil {
		return fmt.Errorf("postToResponseURL: %w", err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal response_url body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("build response_url request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	cli := client
	if cli == nil {
		cli = defaultResponseURLClient()
	}
	resp, err := cli.Do(req)
	if err != nil {
		return fmt.Errorf("response_url POST: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("response_url HTTP %d", resp.StatusCode)
	}
	return nil
}
