package probe

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func respWith(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body))}
}

func reqWith(ctx context.Context, method string) *http.Request {
	r, _ := http.NewRequestWithContext(ctx, method, "https://mcp.example/mcp", nil)
	return r
}

// The exchange's errStatus is the platform's own evidence that a credential
// was rejected: dispatch copies it into agenttool.Result.HTTPStatus, and
// credupdate.classify matches it against [401] to raise a
// request-credential-update card telling a person their credential no longer
// works.
//
// It must therefore come from the JSON-RPC POST and nothing else. The exchange
// type's own doc claimed that was already true — "its side requests … run on
// the connection context, carry no exchange, and can neither record into one
// nor erase one" — and it was not. The go-sdk builds its connection context
// with xcontext.Detach, which drops CANCELLATION and keeps VALUES, so the
// standalone-SSE GET the SDK issues inside Connect runs carrying the opening
// caller's exchange. That GET is on-endpoint, so it passes the origin pin
// (which defends against off-endpoint hops, not against a side request to the
// endpoint itself).
//
// A server answering 200 to initialize and 401 to the standalone-SSE GET could
// therefore make the platform attest that a working credential had been
// rejected, and carry up to 512 bytes of its own text into the surfaced error.
func TestRecordExchange_OnlyThePOSTCanSetTheErrorStatus(t *testing.T) {
	cases := []struct {
		name   string
		method string
		code   int
		body   string
		want   int
	}{
		{
			name:   "the JSON-RPC POST's 401 is the caller's failure and is recorded",
			method: http.MethodPost, code: 401, body: "token expired", want: 401,
		},
		{
			name:   "a standalone-SSE GET's 401 is a side request and is NOT",
			method: http.MethodGet, code: 401,
			body: "ATTACKER-CONTROLLED: your token was rejected, please re-enter it",
			want: 0,
		},
		{
			name:   "a session-teardown DELETE's 403 is a side request and is NOT",
			method: http.MethodDelete, code: 403, body: "forbidden", want: 0,
		},
		{
			name:   "a side request's 5xx is not the caller's server error either",
			method: http.MethodGet, code: 503, body: "unavailable", want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, ex := startExchange(context.Background())

			recordExchange(reqWith(ctx, tc.method), respWith(tc.code, tc.body))

			ex.mu.Lock()
			gotStatus, gotBody := ex.errStatus, ex.errBody
			ex.mu.Unlock()

			assert.Equal(t, tc.want, gotStatus)
			if tc.want == 0 {
				assert.Empty(t, gotBody,
					"a side request must not put its own text into the error a person is shown")
			}
		})
	}
}

// The body must still be restored on every path, side request included: the SDK
// reads it after the transport returns, and a consumed body is a protocol error
// with no relation to what actually happened.
func TestRecordExchange_LeavesTheBodyReadable(t *testing.T) {
	ctx, _ := startExchange(context.Background())
	resp := respWith(401, "some body")

	recordExchange(reqWith(ctx, http.MethodGet), resp)

	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "some body", string(b))
}
