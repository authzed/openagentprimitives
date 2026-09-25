package httpclient

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

// captured records what the mock server saw on a single request so the
// table cases can assert request shape after the call returns.
type captured struct {
	method string
	path   string // includes query string
	auth   string
	body   []byte
}

// run exercises one Memory method against an httptest.Server whose
// handler is supplied by the case. The handler both validates the
// request (recording into *captured) and writes the canned response.
func run(t *testing.T, handler func(*captured, http.ResponseWriter, *http.Request), call func(*Client) error) *captured {
	t.Helper()
	var cap captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.method = r.Method
		// Capture path + query string for assertions like /memory/_preferences/ns/n?turn=3
		cap.path = r.URL.Path
		if r.URL.RawQuery != "" {
			cap.path += "?" + r.URL.RawQuery
		}
		cap.auth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		cap.body = body
		// Restore the body so handlers can re-decode it after capture.
		r.Body = io.NopCloser(bytes.NewReader(body))
		handler(&cap, w, r)
	}))
	t.Cleanup(srv.Close)
	require.NoError(t, call(New(srv.URL, "tok")))
	return &cap
}

func TestClientMethods(t *testing.T) {
	ctx := context.Background()

	t.Run("Put: POST /memory/<kind>/<ns>/<n> with Entry body, returns decoded Entry", func(t *testing.T) {
		in := memory.Entry{
			Scope:   memory.Scope{Kind: "session", ID: "ns/n"},
			Kind:    "turn",
			ID:      "turn-000000-user",
			Content: json.RawMessage(`{"role":"user"}`),
		}
		var got memory.Entry
		cap := run(t, func(_ *captured, w http.ResponseWriter, r *http.Request) {
			var e memory.Entry
			require.NoError(t, json.NewDecoder(r.Body).Decode(&e))
			require.NoError(t, json.NewEncoder(w).Encode(e))
		}, func(c *Client) error {
			var err error
			got, err = c.Put(ctx, in)
			return err
		})
		assert.Equal(t, http.MethodPost, cap.method)
		assert.Equal(t, "/memory/turn/ns/n", cap.path)
		assert.Equal(t, "Bearer tok", cap.auth)
		var sent memory.Entry
		require.NoError(t, json.Unmarshal(cap.body, &sent))
		assert.Equal(t, in, sent)
		assert.Equal(t, in.ID, got.ID)
		assert.Equal(t, in.Kind, got.Kind)
	})

	t.Run("Query simple: single Kind, scope listing uses GET /memory/<kind>/<ns>/<n>", func(t *testing.T) {
		var got memory.QueryResult
		cap := run(t, func(_ *captured, w http.ResponseWriter, _ *http.Request) {
			require.NoError(t, json.NewEncoder(w).Encode(memory.QueryResult{
				Entries: []memory.Entry{{Kind: "turn", ID: "turn-000000-user"}},
			}))
		}, func(c *Client) error {
			var err error
			got, err = c.Query(ctx, memory.Query{
				Scope: memory.Scope{Kind: "session", ID: "ns/n"},
				Kinds: []string{"turn"},
			})
			return err
		})
		assert.Equal(t, http.MethodGet, cap.method)
		assert.Equal(t, "/memory/turn/ns/n", cap.path)
		assert.Equal(t, "Bearer tok", cap.auth)
		assert.Empty(t, cap.body)
		require.Len(t, got.Entries, 1)
		assert.Equal(t, "turn-000000-user", got.Entries[0].ID)
	})

	t.Run("Query rich: tag filter POSTs to /memory/_query/<ns>/<n> with Query body", func(t *testing.T) {
		q := memory.Query{
			Scope: memory.Scope{Kind: "session", ID: "ns/n"},
			Kinds: []string{"turn"},
			Tags:  []string{"important"},
		}
		var got memory.QueryResult
		cap := run(t, func(_ *captured, w http.ResponseWriter, _ *http.Request) {
			require.NoError(t, json.NewEncoder(w).Encode(memory.QueryResult{}))
		}, func(c *Client) error {
			var err error
			got, err = c.Query(ctx, q)
			return err
		})
		assert.Equal(t, http.MethodPost, cap.method)
		assert.Equal(t, "/memory/_query/ns/n", cap.path)
		assert.Equal(t, "Bearer tok", cap.auth)
		var sent memory.Query
		require.NoError(t, json.Unmarshal(cap.body, &sent))
		assert.Equal(t, q.Tags, sent.Tags)
		assert.Equal(t, q.Scope, sent.Scope)
		assert.Empty(t, got.Entries)
	})

	t.Run("SendSignal: POST /memory/_signal/<ns>/<n> with Signal body", func(t *testing.T) {
		sig := memory.Signal{
			Kind:  "session/closed",
			Scope: memory.Scope{Kind: "session", ID: "ns/n"},
		}
		cap := run(t, func(_ *captured, w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}, func(c *Client) error {
			return c.SendSignal(ctx, sig)
		})
		assert.Equal(t, http.MethodPost, cap.method)
		assert.Equal(t, "/memory/_signal/ns/n", cap.path)
		assert.Equal(t, "Bearer tok", cap.auth)
		var sent memory.Signal
		require.NoError(t, json.Unmarshal(cap.body, &sent))
		assert.Equal(t, sig.Kind, sent.Kind)
		assert.Equal(t, sig.Scope, sent.Scope)
	})

	t.Run("RegisterPublisherKey: POST /memory/_publisher_key with {keyId,pubKey} body, 204 succeeds", func(t *testing.T) {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		keyID := "deadbeef"
		cap := run(t, func(_ *captured, w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}, func(c *Client) error {
			return c.RegisterPublisherKey(ctx, keyID, pub)
		})
		assert.Equal(t, http.MethodPost, cap.method)
		assert.Equal(t, "/memory/_publisher_key", cap.path)
		assert.Equal(t, "Bearer tok", cap.auth)
		var sent struct {
			KeyID  string `json:"keyId"`
			PubKey string `json:"pubKey"`
		}
		require.NoError(t, json.Unmarshal(cap.body, &sent))
		assert.Equal(t, keyID, sent.KeyID)
		assert.Equal(t, base64.StdEncoding.EncodeToString(pub), sent.PubKey)
	})

	t.Run("GetPreferences: GET /memory/_preferences/<ns>/<n>, no turnIndex query when turnIndex < 0", func(t *testing.T) {
		cap := run(t, func(_ *captured, w http.ResponseWriter, _ *http.Request) {
			require.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{
				"subject":        "user@example.com",
				"classNamespace": "default",
				"className":      "test-agent",
				"snapshot":       map[string]interface{}{},
			}))
		}, func(c *Client) error {
			_, err := c.GetPreferences(ctx, "ns", "n", -1)
			return err
		})
		assert.Equal(t, http.MethodGet, cap.method)
		assert.Equal(t, "/memory/_preferences/ns/n", cap.path)
		assert.Equal(t, "Bearer tok", cap.auth)
		assert.Empty(t, cap.body)
	})

	t.Run("GetPreferences: GET /memory/_preferences/<ns>/<n>?turn=N when turnIndex >= 0", func(t *testing.T) {
		cap := run(t, func(_ *captured, w http.ResponseWriter, _ *http.Request) {
			require.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{
				"subject":        "user@example.com",
				"classNamespace": "default",
				"className":      "test-agent",
				"snapshot":       map[string]interface{}{},
			}))
		}, func(c *Client) error {
			_, err := c.GetPreferences(ctx, "ns", "n", 3)
			return err
		})
		assert.Equal(t, http.MethodGet, cap.method)
		assert.Equal(t, "/memory/_preferences/ns/n?turn=3", cap.path)
		assert.Equal(t, "Bearer tok", cap.auth)
	})

	t.Run("GetPreferencesForUserRef: GET /memory/_preferences/<ns>/<n>?user-ref=<escaped ref>", func(t *testing.T) {
		cap := run(t, func(_ *captured, w http.ResponseWriter, _ *http.Request) {
			require.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{
				"subject":        "canon-abc",
				"classNamespace": "default",
				"className":      "test-agent",
				"snapshot":       map[string]interface{}{},
				"note":           "",
			}))
		}, func(c *Client) error {
			got, err := c.GetPreferencesForUserRef(ctx, "ns", "n", "email:alice+bob@example.com")
			if err == nil {
				assert.Equal(t, "canon-abc", got.Subject)
			}
			return err
		})
		assert.Equal(t, http.MethodGet, cap.method)
		assert.Equal(t, "/memory/_preferences/ns/n?user-ref=email%3Aalice%2Bbob%40example.com", cap.path)
		assert.Equal(t, "Bearer tok", cap.auth)
		assert.Empty(t, cap.body)
	})

	t.Run("CommitPreference: POST /memory/_preferences_commit/<ns>/<n> with CommitRequest body, 204 succeeds", func(t *testing.T) {
		cap := run(t, func(_ *captured, w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}, func(c *Client) error {
			commitReq := preferences.CommitRequest{
				Key:     "theme",
				Value:   &apiextv1.JSON{Raw: []byte(`"dark"`)},
				Subject: "user@example.com",
			}
			return c.CommitPreference(ctx, "ns", "n", commitReq)
		})
		assert.Equal(t, http.MethodPost, cap.method)
		assert.Equal(t, "/memory/_preferences_commit/ns/n", cap.path)
		assert.Equal(t, "Bearer tok", cap.auth)
		var sent preferences.CommitRequest
		require.NoError(t, json.Unmarshal(cap.body, &sent))
		assert.Equal(t, "theme", sent.Key)
		assert.Equal(t, "user@example.com", sent.Subject)
	})

	t.Run("GetPreferencesFirstParty: GET /memory/_preferences_firstparty/<ns>/<className>?subject=<escaped>", func(t *testing.T) {
		cap := run(t, func(_ *captured, w http.ResponseWriter, _ *http.Request) {
			require.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{
				"subject":        "slack:U123ABC",
				"classNamespace": "default",
				"className":      "test-agent",
				"snapshot":       map[string]interface{}{},
				"note":           "",
			}))
		}, func(c *Client) error {
			got, err := c.GetPreferencesFirstParty(ctx, "ns", "test-agent", "slack:U123ABC+demo")
			if err == nil {
				assert.Equal(t, "slack:U123ABC", got.Subject)
			}
			return err
		})
		assert.Equal(t, http.MethodGet, cap.method)
		assert.Equal(t, "/memory/_preferences_firstparty/ns/test-agent?subject=slack%3AU123ABC%2Bdemo", cap.path)
		assert.Equal(t, "Bearer tok", cap.auth)
		assert.Empty(t, cap.body)
	})

	t.Run("CommitPreferenceFirstParty: POST /memory/_preferences_firstparty_commit/<ns>/<className> with CommitRequest body, 204 succeeds", func(t *testing.T) {
		cap := run(t, func(_ *captured, w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}, func(c *Client) error {
			commitReq := preferences.CommitRequest{
				Key:     "theme",
				Value:   &apiextv1.JSON{Raw: []byte(`"light"`)},
				Subject: "slack:U123ABC",
			}
			return c.CommitPreferenceFirstParty(ctx, "ns", "test-agent", commitReq)
		})
		assert.Equal(t, http.MethodPost, cap.method)
		assert.Equal(t, "/memory/_preferences_firstparty_commit/ns/test-agent", cap.path)
		assert.Equal(t, "Bearer tok", cap.auth)
		var sent preferences.CommitRequest
		require.NoError(t, json.Unmarshal(cap.body, &sent))
		assert.Equal(t, "theme", sent.Key)
		assert.Equal(t, "slack:U123ABC", sent.Subject)
	})
}

func TestClientNon2xxErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "tok")
	ctx := context.Background()

	t.Run("Put returns error containing status code", func(t *testing.T) {
		_, err := c.Put(ctx, memory.Entry{
			Scope: memory.Scope{Kind: "session", ID: "ns/n"}, Kind: "turn", ID: "turn-0",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "500")
	})

	t.Run("Query returns error containing status code", func(t *testing.T) {
		_, err := c.Query(ctx, memory.Query{
			Scope: memory.Scope{Kind: "session", ID: "ns/n"}, Kinds: []string{"turn"},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "500")
	})

	t.Run("SendSignal returns error containing status code", func(t *testing.T) {
		err := c.SendSignal(ctx, memory.Signal{
			Kind: "session/closed", Scope: memory.Scope{Kind: "session", ID: "ns/n"},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "500")
	})

	t.Run("RegisterPublisherKey returns error containing status code on non-204", func(t *testing.T) {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		err = c.RegisterPublisherKey(ctx, "deadbeef", pub)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "500")
		assert.Contains(t, err.Error(), "boom")
	})

	t.Run("CommitPreference returns error on non-204", func(t *testing.T) {
		commitReq := preferences.CommitRequest{
			Key:     "theme",
			Value:   &apiextv1.JSON{Raw: []byte(`"dark"`)},
			Subject: "user@example.com",
		}
		err := c.CommitPreference(ctx, "ns", "n", commitReq)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "500")
	})

	t.Run("GetPreferencesFirstParty returns error on 500", func(t *testing.T) {
		_, err := c.GetPreferencesFirstParty(ctx, "ns", "agent", "slack:U123")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "500")
	})

	t.Run("CommitPreferenceFirstParty returns error on non-204", func(t *testing.T) {
		commitReq := preferences.CommitRequest{
			Key:     "theme",
			Value:   &apiextv1.JSON{Raw: []byte(`"light"`)},
			Subject: "slack:U123",
		}
		err := c.CommitPreferenceFirstParty(ctx, "ns", "agent", commitReq)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "500")
	})
}
