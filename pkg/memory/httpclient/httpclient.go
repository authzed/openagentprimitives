// Package httpclient is the HTTP client for the operator's memory API.
// httpclient.Client implements memory.Memory; the runner, oap, and
// channelsd use it instead of an in-process Backend.
package httpclient

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/sentinel"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("httpclient: status=%d body=%s", e.code, e.msg)
}

// sentinelError is a memory sentinel rebuilt from a response the server
// identified with sentinel.Header — the far half of the contract httpsrv holds
// up, and the reason errors.Is(err, memory.ErrInvalidQuery) means the same
// thing whether the caller's memory is in-process or an HTTP client.
//
// It renders the SERVER's message, not the sentinel's own — the reason this is
// a type rather than a fmt.Errorf("%w: …") wrap. A sentinel's value is largely
// its detail (ErrInvalidQuery exists to carry the "did you mean %q?" hint that
// lets a model fix its next call in one turn), and the server message already
// contains the sentinel's own text, so re-prefixing would print it twice.
//
// ON TRUST: the recall meta tools render this message with Trusted=true, so a
// content guard cannot withhold it and it must never become a channel for bytes
// this codebase did not author. It cannot be: reconstruction happens only for a
// response carrying sentinel.Header, which only httpsrv sets, and every
// sentinel-wrapping construction here composes fixed platform text with
// request-derived values. The errors that interpolate up to 1KiB of an upstream
// body are NOT in sentinel.Table, are answered 500, and arrive as a plain
// untrusted statusError.
type sentinelError struct {
	sentinel error
	msg      string
}

func (e *sentinelError) Error() string {
	if e.msg == "" {
		// A refusal with no body still has to say something; the sentinel's own
		// text is the platform-authored floor.
		return e.sentinel.Error()
	}
	return e.msg
}

func (e *sentinelError) Unwrap() error { return e.sentinel }

type Client struct {
	baseURL string
	token   string
	backend string
	http    *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{baseURL: baseURL, token: token, http: &http.Client{Timeout: 30 * time.Second}}
}

// WithBackend returns a copy of the Client that passes ?backend= on
// read requests, overriding the shadow backend's read source.
func (c *Client) WithBackend(backend string) *Client {
	cp := *c
	cp.backend = backend
	return &cp
}

var _ memory.Memory = (*Client)(nil)
var _ memory.PoolWriter = (*Client)(nil)
var _ memory.PoolReader = (*Client)(nil)

// scopePath splits Scope.ID ("<ns>/<name>") into the two URL segments.
func scopePath(s memory.Scope) (ns, name string) {
	for i := 0; i < len(s.ID); i++ {
		if s.ID[i] == '/' {
			return s.ID[:i], s.ID[i+1:]
		}
	}
	return s.ID, ""
}

func (c *Client) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	ns, name := scopePath(e.Scope)
	var out memory.Entry
	err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/memory/%s/%s/%s", url.PathEscape(e.Kind), ns, name), e, &out)
	return out, err
}

// PutToPool writes an entry into a RESOURCE pool rather than the session's
// own scope. The URL still names the session, because that is what the
// bearer token authorizes; the pool is named separately and the server
// proves the session's write grant on it before honouring it.
//
// It is NOT part of memory.Memory, and deliberately so: Memory.Put routes by
// e.Scope, and a pool write cannot, since the server refuses to read a
// destination out of the body at all. A caller that wants a pool has to say so
// at the call site.
//
// poolScope must be a memory.ResourceScope — its ID is the "<type>:<id>" ref
// the server re-validates and proves. A session scope passed here would be
// refused by the server (a '/' in the ref), not silently written to.
func (c *Client) PutToPool(ctx context.Context, sessionScope, poolScope memory.Scope, e memory.Entry) (memory.Entry, error) {
	if !memory.IsResourceScope(poolScope) {
		// Refused here rather than sent: the server would refuse it too, but a
		// caller that reached for the pool path with a session scope has a bug
		// worth naming at the call site instead of reading out of a 400.
		return memory.Entry{}, fmt.Errorf("httpclient: PutToPool needs a resource scope, got %q", poolScope.Kind)
	}
	ns, name := scopePath(sessionScope)
	var out memory.Entry
	err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/memory/%s/%s/%s?pool=%s", url.PathEscape(e.Kind), ns, name, url.QueryEscape(poolScope.ID)), e, &out)
	return out, err
}

// QueryPool reads a RESOURCE pool's entries rather than the session's own
// scope. Like PutToPool, the URL names the session (that is what the bearer
// token authorizes) and the pool rides the request line as a pool parameter;
// the server proves the session's grant on it before serving — httpsrv's
// destinationFor runs for every method on the keyed route, not only POST.
//
// This is the read direction of the addressing PutToPool already had, and its
// absence was not a wart. provenance.SigningMemory seeds a pool's signing chain
// by querying it back, so with no route to a pool's entries the FIRST pool
// write over an httpclient inner failed outright — "seed chain for dossier:d-1:
// … status=404 body=not a memory path" — in every mode.
//
// It refuses a query the keyed route cannot express rather than silently
// re-aiming it: a pool destination is honoured only on
// /memory/{kind}/{ns}/{name} (poolDestinationAllowed refuses it on every
// _-prefixed route), so a rich query — several Kinds, ids, tags, a time window
// — would have to go to _query, which would then serve the SESSION's scope.
// Answering a pool question with session data is the silent downgrade the whole
// destination gate exists to prevent, so the mismatch is named here instead.
func (c *Client) QueryPool(ctx context.Context, sessionScope, poolScope memory.Scope, q memory.Query) (memory.QueryResult, error) {
	if !memory.IsResourceScope(poolScope) {
		return memory.QueryResult{}, fmt.Errorf("httpclient: QueryPool needs a resource scope, got %q", poolScope.Kind)
	}
	if isRich(q) {
		return memory.QueryResult{}, fmt.Errorf(
			"httpclient: QueryPool serves a single-Kind listing only; %q cannot be addressed by a rich query (the rich route cannot name a pool destination)",
			poolScope.ID)
	}
	ns, name := scopePath(sessionScope)
	vals := url.Values{"pool": []string{poolScope.ID}}
	if q.Limit > 0 {
		vals.Set("limit", strconv.Itoa(q.Limit))
	}
	path := fmt.Sprintf("/memory/%s/%s/%s?%s", url.PathEscape(q.Kinds[0]), ns, name, vals.Encode())
	var out memory.QueryResult
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// isRich reports whether q needs the POST /_query route (anything beyond
// a single-Kind scope listing).
func isRich(q memory.Query) bool {
	return len(q.Kinds) != 1 || len(q.IDs) > 0 || len(q.LinkedTo) > 0 ||
		len(q.LinkedFrom) > 0 || len(q.Tags) > 0 || len(q.FieldEquals) > 0 ||
		q.Since != nil || q.Until != nil
}

func (c *Client) Query(ctx context.Context, q memory.Query) (memory.QueryResult, error) {
	ns, name := scopePath(q.Scope)
	var out memory.QueryResult
	if isRich(q) {
		err := c.do(ctx, http.MethodPost, fmt.Sprintf("/memory/_query/%s/%s", ns, name), q, &out)
		return out, err
	}
	path := fmt.Sprintf("/memory/%s/%s/%s", url.PathEscape(q.Kinds[0]), ns, name)
	if q.Limit > 0 {
		path += fmt.Sprintf("?limit=%d", q.Limit)
	}
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

func (c *Client) SendSignal(ctx context.Context, sig memory.Signal) error {
	ns, name := scopePath(sig.Scope)
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/memory/_signal/%s/%s", ns, name), sig, nil)
}

func (c *Client) Search(ctx context.Context, req memory.SearchRequest) (memory.MergedSearchResult, error) {
	if len(req.Scopes) == 0 {
		return memory.MergedSearchResult{}, fmt.Errorf("httpclient: Search requires at least one scope")
	}
	ns, name := scopePath(req.Scopes[0])
	var out memory.MergedSearchResult
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/memory/_search/%s/%s", ns, name), req, &out)
	if err != nil {
		// Compatibility bridge for a server too old to stamp sentinel.Header,
		// where this route's bare 404 is the only sentinel a caller gets. Kept
		// as narrow as possible — this one route, this one status — so a
		// mixed-version cluster does not regress mid-upgrade. A current server
		// never reaches it: do() already rebuilt the sentinel from the header.
		var se *statusError
		if errors.As(err, &se) && se.code == http.StatusNotFound {
			return memory.MergedSearchResult{}, &sentinelError{
				sentinel: memory.ErrNoSearchProviders,
				msg:      strings.TrimSpace(se.msg),
			}
		}
		return memory.MergedSearchResult{}, err
	}
	return out, nil
}

// ReindexResult holds the result of a server-side reindex operation.
type ReindexResult struct {
	// Count is how many entries reached EVERY search provider; entries that
	// failed on one provider are excluded.
	Count int `json:"count"`
}

// Reindex triggers a server-side reindex of every entry in scope. Not part of
// the Memory interface; a standalone method for CLI use.
func (c *Client) Reindex(ctx context.Context, scope memory.Scope) (ReindexResult, error) {
	ns, name := scopePath(scope)
	var out ReindexResult
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/memory/_reindex/%s/%s", ns, name), nil, &out)
	return out, err
}

// Delete removes a single entry. Not part of the Memory interface, where Delete
// is operator-only; a standalone method for CLI use.
func (c *Client) Delete(ctx context.Context, scope memory.Scope, kind, id string) error {
	ns, name := scopePath(scope)
	return c.do(ctx, http.MethodDelete,
		fmt.Sprintf("/memory/_entry/%s/%s?kind=%s&id=%s",
			ns, name, url.QueryEscape(kind), url.QueryEscape(id)),
		nil, nil)
}

// RegisterPublisherKey registers this client's component signing key with the
// operator. The publisher identity is derived server-side from the BEARER TOKEN,
// never from the body; keyID must equal provenance.KeyID(pub).
func (c *Client) RegisterPublisherKey(ctx context.Context, keyID string, pub ed25519.PublicKey) error {
	body := struct {
		KeyID  string `json:"keyId"`
		PubKey string `json:"pubKey"`
	}{
		KeyID:  keyID,
		PubKey: base64.StdEncoding.EncodeToString(pub),
	}
	return c.do(ctx, http.MethodPost, "/memory/_publisher_key", body, nil)
}

// KGSearchFacts queries the knowledge graph for facts matching query.
func (c *Client) KGSearchFacts(ctx context.Context, scope memory.Scope, query string, limit int) ([]memory.KGFact, error) {
	ns, name := scopePath(scope)
	var out struct {
		Facts []memory.KGFact `json:"facts"`
	}
	err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/memory/_kg/%s/%s?action=search&q=%s&limit=%d",
			ns, name, url.QueryEscape(query), limit), nil, &out)
	return out.Facts, err
}

// KGGetEntity retrieves a single knowledge graph entity by UUID.
func (c *Client) KGGetEntity(ctx context.Context, scope memory.Scope, uuid string) (*memory.KGEntity, error) {
	ns, name := scopePath(scope)
	var out memory.KGEntity
	err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/memory/_kg/%s/%s?action=entity&uuid=%s",
			ns, name, url.QueryEscape(uuid)), nil, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// KGEntityFacts returns all facts associated with the given entity UUID.
func (c *Client) KGEntityFacts(ctx context.Context, scope memory.Scope, uuid string) ([]memory.KGFact, error) {
	ns, name := scopePath(scope)
	var out struct {
		Facts []memory.KGFact `json:"facts"`
	}
	err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/memory/_kg/%s/%s?action=entity-facts&uuid=%s",
			ns, name, url.QueryEscape(uuid)), nil, &out)
	return out.Facts, err
}

// KGRelatedEntities returns entities related to the given entity UUID.
func (c *Client) KGRelatedEntities(ctx context.Context, scope memory.Scope, uuid string, limit int) ([]memory.KGEntity, error) {
	ns, name := scopePath(scope)
	var out struct {
		Entities []memory.KGEntity `json:"entities"`
	}
	err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/memory/_kg/%s/%s?action=related&uuid=%s&limit=%d",
			ns, name, url.QueryEscape(uuid), limit), nil, &out)
	return out.Entities, err
}

// KGCommunities returns all communities (topic clusters) for the scope's group.
func (c *Client) KGCommunities(ctx context.Context, scope memory.Scope) ([]memory.KGCommunity, error) {
	ns, name := scopePath(scope)
	var out struct {
		Communities []memory.KGCommunity `json:"communities"`
	}
	err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/memory/_kg/%s/%s?action=communities", ns, name), nil, &out)
	return out.Communities, err
}

// KGClient wraps Client with a bound scope for use as a tool.KGQuerier. The
// bound scope always wins: Communities IGNORES its groupID argument and answers
// for this scope's group.
type KGClient struct {
	client *Client
	scope  memory.Scope
}

func NewKGClient(c *Client, scope memory.Scope) *KGClient {
	return &KGClient{client: c, scope: scope}
}

func (k *KGClient) SearchFacts(ctx context.Context, query string, limit int) ([]memory.KGFact, error) {
	return k.client.KGSearchFacts(ctx, k.scope, query, limit)
}
func (k *KGClient) GetEntity(ctx context.Context, uuid string) (*memory.KGEntity, error) {
	return k.client.KGGetEntity(ctx, k.scope, uuid)
}
func (k *KGClient) EntityFacts(ctx context.Context, entityUUID string) ([]memory.KGFact, error) {
	return k.client.KGEntityFacts(ctx, k.scope, entityUUID)
}
func (k *KGClient) RelatedEntities(ctx context.Context, entityUUID string, limit int) ([]memory.KGEntity, error) {
	return k.client.KGRelatedEntities(ctx, k.scope, entityUUID, limit)
}
func (k *KGClient) Communities(ctx context.Context, groupID string) ([]memory.KGCommunity, error) {
	return k.client.KGCommunities(ctx, k.scope)
}

// GetPreferences retrieves a user preferences snapshot for a session at an
// optional turn index. turnIndex < 0 omits the ?turn query parameter.
func (c *Client) GetPreferences(ctx context.Context, ns, name string, turnIndex int) (preferences.SnapshotResponse, error) {
	path := fmt.Sprintf("/memory/_preferences/%s/%s", ns, name)
	if turnIndex >= 0 {
		path += fmt.Sprintf("?turn=%d", turnIndex)
	}
	var out preferences.SnapshotResponse
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// CommitPreference commits a user preference update. Expects a 204 response.
func (c *Client) CommitPreference(ctx context.Context, ns, name string, req preferences.CommitRequest) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/memory/_preferences_commit/%s/%s", ns, name), req, nil)
}

// GetPreferencesFirstParty retrieves a class-addressed first-party preferences snapshot.
// The subject parameter is the first-party identifier (e.g., a Slack user ID).
func (c *Client) GetPreferencesFirstParty(ctx context.Context, ns, className, subject string) (preferences.SnapshotResponse, error) {
	path := fmt.Sprintf("/memory/_preferences_firstparty/%s/%s?subject=%s", ns, className, url.QueryEscape(subject))
	var out preferences.SnapshotResponse
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// CommitPreferenceFirstParty commits a first-party preference update. Expects a 204 response.
func (c *Client) CommitPreferenceFirstParty(ctx context.Context, ns, className string, req preferences.CommitRequest) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/memory/_preferences_firstparty_commit/%s/%s", ns, className), req, nil)
}

// GetPreferencesForUserRef retrieves a class-visible preferences snapshot
// for a NAMED user, resolved server-side from ref (an email/resource/
// trigger-author reference — see pkg/platform/identity/subjectresolve).
// Mutually exclusive with GetPreferences' ?turn= on the server side; every
// call is audited by the operator regardless of whether ref resolves.
func (c *Client) GetPreferencesForUserRef(ctx context.Context, ns, name, ref string) (preferences.SnapshotResponse, error) {
	path := fmt.Sprintf("/memory/_preferences/%s/%s?user-ref=%s", ns, name, url.QueryEscape(ref))
	var out preferences.SnapshotResponse
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// memoryHTTPMaxAttempts / memoryHTTPBackoff bound do()'s transient-retry loop.
//
// The runner talks to the operator's memory server the instant its session pod
// starts, and a just-scheduled pod's kube-proxy rules — or the operator's own
// restart window — can make the first dial fail with "connection refused" for a
// moment. Without a retry that single blip fails the whole conversation, since
// the runner loop's memory path is fail-closed. Transient transport failures and
// 5xx therefore ride out a short bounded backoff; permanent failures (4xx,
// marshal/decode) and a cancelled caller context are NOT retried. Total budget
// ~17s, comfortably inside a turn's context.
const memoryHTTPMaxAttempts = 8

var memoryHTTPBackoff = []time.Duration{
	250 * time.Millisecond,
	500 * time.Millisecond,
	1 * time.Second,
	2 * time.Second,
	3 * time.Second,
	5 * time.Second,
	5 * time.Second,
}

// do performs one request — marshal body, set auth, decode out, map non-2xx to
// an error — retrying transient failures per memoryHTTPMaxAttempts.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("httpclient: marshal: %w", err)
		}
		bodyBytes = b
	}
	fullURL := c.baseURL + path
	if c.backend != "" {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		fullURL += sep + "backend=" + url.QueryEscape(c.backend)
	}

	var lastErr error
	for attempt := 0; attempt < memoryHTTPMaxAttempts; attempt++ {
		if attempt > 0 {
			// Back off before retrying, but abort immediately if the caller's
			// context is done — never sleep past a cancel/deadline.
			select {
			case <-ctx.Done():
				return fmt.Errorf("httpclient: %s %s: %w (gave up after %d attempts; last error: %v)",
					method, path, ctx.Err(), attempt, lastErr)
			case <-time.After(memoryHTTPBackoff[min(attempt-1, len(memoryHTTPBackoff)-1)]):
			}
		}

		var rdr io.Reader
		if bodyBytes != nil {
			rdr = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequestWithContext(ctx, method, fullURL, rdr)
		if err != nil {
			return fmt.Errorf("httpclient: request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		if bodyBytes != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.http.Do(req)
		if err != nil {
			// Transport-level failure (no HTTP response): connection refused /
			// reset / dial timeout while the operator memory endpoint is still
			// coming up. Retry unless it's the caller's context that failed.
			if ctx.Err() != nil {
				return err
			}
			lastErr = err
			continue
		}
		if resp.StatusCode >= 500 {
			// Server-side transient (operator still starting / restarting). No
			// sentinel is answered 5xx — every row in sentinel.Table is a
			// permanent 4xx verdict — so nothing is rebuilt here.
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			lastErr = &statusError{code: resp.StatusCode, msg: string(msg)}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			// 4xx is permanent (auth, not-found, bad request) — do not retry.
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			code := resp.Header.Get(sentinel.Header)
			resp.Body.Close()
			// Rebuild the sentinel when — and only when — the server named it.
			// The status alone cannot: 403 is answered by two sentinels and by
			// refusals that are no sentinel at all (the cross-session token
			// check, handleKG's approval door, any intermediary proxy), and 400
			// and 404 are ambiguous the same way. An unstamped response is
			// therefore an opaque transport error, which is the fail-toward-
			// unknown direction — it also means a server too old to stamp the
			// header degrades to exactly the behavior that shipped before.
			if s, ok := sentinel.ForCode(code); ok {
				return &sentinelError{sentinel: s, msg: strings.TrimSpace(string(msg))}
			}
			return &statusError{code: resp.StatusCode, msg: string(msg)}
		}
		if out != nil {
			derr := json.NewDecoder(resp.Body).Decode(out)
			resp.Body.Close()
			if derr != nil {
				return fmt.Errorf("httpclient: decode: %w", derr)
			}
			return nil
		}
		resp.Body.Close()
		return nil
	}
	return fmt.Errorf("httpclient: %s %s: giving up after %d attempts: %w",
		method, path, memoryHTTPMaxAttempts, lastErr)
}
