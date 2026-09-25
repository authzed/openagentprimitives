package apiadapter_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/apiadapter"
)

const goodConfig = `
baseURL: https://api.example.test
auth: {type: bearer, envVar: API_TOKEN}
operations:
  - name: get_account
    description: Fetch one account by id.
    method: GET
    path: /accounts/{id}
    params:
      - {name: id, in: path, required: true, type: string, description: Account id.}
      - {name: verbose, in: query, type: boolean, description: Include detail.}
  - name: create_note
    description: Attach a note to an account.
    method: POST
    path: /accounts/{id}/notes
    params:
      - {name: id, in: path, required: true, type: string}
      - {name: body, in: body, required: true, type: string}
`

func TestParse_Valid(t *testing.T) {
	c, err := apiadapter.Parse([]byte(goodConfig))
	require.NoError(t, err)
	assert.Equal(t, "https://api.example.test", c.BaseURL)
	assert.Equal(t, []string{"create_note", "get_account"}, c.OperationNames())
	op, ok := c.Operation("get_account")
	require.True(t, ok)
	assert.Equal(t, "GET", op.Method)
}

// TestParse_HTTPAllowedWithNoAuth pins the row MAJOR 7's fix must keep
// passing: plain http is fine when there is no credential to leak (the
// e2e/test-stub path some SidecarToolbox configs use).
func TestParse_HTTPAllowedWithNoAuth(t *testing.T) {
	c, err := apiadapter.Parse([]byte("baseURL: http://a.test\nauth: {type: none}\noperations: [{name: a, method: GET, path: /x}]"))
	require.NoError(t, err)
	assert.Equal(t, "http://a.test", c.BaseURL)
}

func TestParse_Rejects(t *testing.T) {
	cases := []struct{ name, cfg, wantErr string }{
		{"baseURL not absolute", "baseURL: /nope\nauth: {type: none}\noperations: [{name: a, method: GET, path: /x}]", "absolute URL"},
		{"baseURL loopback refused by the SSRF guard", "baseURL: http://127.0.0.1:8080\nauth: {type: none}\noperations: [{name: a, method: GET, path: /x}]", "baseURL host"},
		{"unknown auth type", "baseURL: https://a.test\nauth: {type: magic}\noperations: [{name: a, method: GET, path: /x}]", "auth.type"},
		{"auth needs envVar", "baseURL: https://a.test\nauth: {type: bearer}\noperations: [{name: a, method: GET, path: /x}]", "auth.envVar is required"},
		{"header auth needs name", "baseURL: https://a.test\nauth: {type: header, envVar: T}\noperations: [{name: a, method: GET, path: /x}]", "auth.name is required"},
		{"no operations", "baseURL: https://a.test\nauth: {type: none}\noperations: []", "at least one operation"},
		{"bad method", "baseURL: https://a.test\nauth: {type: none}\noperations: [{name: a, method: FETCH, path: /x}]", "method"},
		{"path must start with slash", "baseURL: https://a.test\nauth: {type: none}\noperations: [{name: a, method: GET, path: x}]", "must start with /"},
		{"placeholder without param", "baseURL: https://a.test\nauth: {type: none}\noperations: [{name: a, method: GET, path: \"/x/{id}\"}]", "no matching param"},
		{"duplicate operation names", "baseURL: https://a.test\nauth: {type: none}\noperations: [{name: a, method: GET, path: /x}, {name: a, method: GET, path: /y}]", "duplicate operation"},
		{"unknown field is rejected", "baseURL: https://a.test\nauth: {type: none}\noperations: [{name: a, method: GET, path: /x}]\nbogus: 1", "parse config"},
		{"empty param name", "baseURL: https://a.test\nauth: {type: none}\noperations: [{name: a, method: GET, path: /x, params: [{in: query, type: string}]}]", "param name is required"},
		{"invalid param in", "baseURL: https://a.test\nauth: {type: none}\noperations: [{name: a, method: GET, path: /x, params: [{name: q, in: bogus, type: string}]}]", "must be one of path, query, header, body"},
		{"invalid param type", "baseURL: https://a.test\nauth: {type: none}\noperations: [{name: a, method: GET, path: /x, params: [{name: q, in: query, type: bogus}]}]", "is not a supported type"},
		{"object type outside body", "baseURL: https://a.test\nauth: {type: none}\noperations: [{name: a, method: GET, path: /x, params: [{name: q, in: query, type: object}]}]", "is only valid for in: body"},
		{"duplicate param name", "baseURL: https://a.test\nauth: {type: none}\noperations: [{name: a, method: GET, path: /x, params: [{name: q, in: query, type: string}, {name: q, in: header, type: string}]}]", "duplicate param"},
		{"path param not required", "baseURL: https://a.test\nauth: {type: none}\noperations: [{name: a, method: GET, path: \"/x/{id}\", params: [{name: id, in: path, type: string}]}]", "must be required"},
		{"in: path param whose placeholder is not in the path", "baseURL: https://a.test\nauth: {type: none}\noperations: [{name: a, method: GET, path: /x, params: [{name: id, in: path, required: true, type: string}]}]", "is not in the path"},
		{"baseURL with a query is rejected (it would silently mask/override an operation's own query)", "baseURL: https://a.test?apiVersion=2\nauth: {type: none}\noperations: [{name: a, method: GET, path: /x}]", "must carry no query or fragment"},
		{"baseURL with a fragment is rejected", "baseURL: https://a.test#frag\nauth: {type: none}\noperations: [{name: a, method: GET, path: /x}]", "must carry no query or fragment"},
		{"baseURL in opaque form has no host, so the pre-existing absolute-URL check refuses it before the new query/fragment check ever runs", "baseURL: https:opaque-form\nauth: {type: none}\noperations: [{name: a, method: GET, path: /x}]", "absolute URL"},
		{"baseURL userinfo is an inline credential channel and is rejected even with auth: none", "baseURL: https://key:secret@api.example.test\nauth: {type: none}\noperations: [{name: a, method: GET, path: /x}]", "must carry no userinfo"},
		{"http baseURL with configured auth would send the credential in cleartext", "baseURL: http://a.test\nauth: {type: bearer, envVar: T}\noperations: [{name: a, method: GET, path: /x}]", "cleartext"},
		{"operation name over 48 chars is rejected (permsurface's 64-char handle has no room)", "baseURL: https://a.test\nauth: {type: none}\noperations: [{name: " + strings.Repeat("a", 49) + ", method: GET, path: /x}]", "must match"},
		{"operation name with an illegal character is rejected", "baseURL: https://a.test\nauth: {type: none}\noperations: [{name: \"a.b\", method: GET, path: /x}]", "must match"},
		{"header param name with a space is not a valid HTTP header field name", "baseURL: https://a.test\nauth: {type: none}\noperations: [{name: a, method: GET, path: /x, params: [{name: \"X Bad Key\", in: header, type: string}]}]", "not a valid HTTP header field name"},
		{"header auth name with a space is not a valid HTTP header field name", "baseURL: https://a.test\nauth: {type: header, envVar: T, name: \"X Bad Key\"}\noperations: [{name: a, method: GET, path: /x}]", "not a valid HTTP header field name"},
		{"path with an unbalanced '{' is rejected", "baseURL: https://a.test\nauth: {type: none}\noperations: [{name: a, method: GET, path: \"/x/{id\"}]", "unbalanced '{'"},
		{"config document over the size cap is rejected", "baseURL: https://a.test\nauth: {type: none}\noperations: [{name: a, method: GET, path: /x, description: \"" + strings.Repeat("a", 70000) + "\"}]", "65536"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := apiadapter.Parse([]byte(tc.cfg))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
