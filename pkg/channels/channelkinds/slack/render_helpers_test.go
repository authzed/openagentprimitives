// pkg/channels/channelkinds/slack/render_helpers_test.go
//
// Shared Block-Kit render / response_url test helpers, used by
// interaction_details_test.go, interaction_excerpt_test.go, interaction_test.go,
// sender_chunk_test.go and show_settings_test.go.
package slack

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/require"
)

// blocksJSON marshals a Block Kit slice to JSON for substring assertions.
func blocksJSON(t *testing.T, blocks []slackapi.Block) string {
	t.Helper()
	raw, err := json.Marshal(blocks)
	require.NoError(t, err, "marshal blocks")
	return string(raw)
}

// concatBlockText flattens the human-readable text of a Block Kit slice
// (section / context / header blocks) into one string for assertions.
func concatBlockText(blocks []slackapi.Block) string {
	var sb strings.Builder
	for _, b := range blocks {
		switch v := b.(type) {
		case containerBlock:
			// Every interaction renders as a container. The lead lives in the
			// rich_text_title and the rest one level down — both are text the
			// reader sees, so both belong in the concatenation.
			sb.WriteString(richTextPlain(v.RichTextTitle))
			sb.WriteByte('\n')
			sb.WriteString(concatBlockText(v.ChildBlocks))
		case *slackapi.SectionBlock:
			if v.Text != nil {
				sb.WriteString(v.Text.Text)
				sb.WriteByte('\n')
			}
		case *slackapi.ContextBlock:
			for _, e := range v.ContextElements.Elements {
				if t, ok := e.(*slackapi.TextBlockObject); ok {
					sb.WriteString(t.Text)
					sb.WriteByte('\n')
				}
			}
		case *slackapi.HeaderBlock:
			if v.Text != nil {
				sb.WriteString(v.Text.Text)
				sb.WriteByte('\n')
			}
		}
	}
	return sb.String()
}

// renderTextFromOpts applies MsgOptions the way slack-go's client would and
// returns the resolved "text" form value, so tests can assert on the fallback
// notification text of a Block Kit message.
func renderTextFromOpts(t *testing.T, opts []slackapi.MsgOption) string {
	t.Helper()
	_, values, err := slackapi.UnsafeApplyMsgOptions("xoxb-test", "C123", "https://slack.example/", opts...)
	require.NoError(t, err, "apply msg options")
	return values.Get("text")
}

// capturedBodies is a thread-safe recorder of response_url POST bodies for the
// httptest server newResponseURLServer stands up.
type capturedBodies struct {
	mu sync.Mutex
	bs []string
}

func (c *capturedBodies) add(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bs = append(c.bs, s)
}

func (c *capturedBodies) values() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.bs))
	copy(out, c.bs)
	return out
}

// newResponseURLServer stands up an httptest server that acks every POST and
// records the request body, so tests can assert on the payload a sender edits
// a clicker's ephemeral with via response_url.
func newResponseURLServer(t *testing.T) (*capturedBodies, *httptest.Server) {
	t.Helper()
	bodies := &capturedBodies{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies.add(string(b))
		w.WriteHeader(http.StatusOK)
	}))
	return bodies, srv
}

// testResponseURL is a URL shaped like a real Slack response_url. It is the
// ONLY shape postToResponseURL will send to (validateResponseURL), so every
// test that wants a successful POST must use it — an httptest server's own
// http://127.0.0.1:PORT URL is refused, exactly as a forged wire ResponseRef
// is.
const testResponseURL = "https://hooks.slack.com/actions/T00000000/1234567890/abcdefghijklmnop"

// responseURLClient returns an *http.Client that sends every request to srv
// whatever the URL says, so a test can post to testResponseURL without
// reaching the network.
//
// This is deliberately a transport-level redirect rather than a hook that
// relaxes the host allowlist for tests. The allowlist is the security
// property; a test seam that switched it off would leave the production check
// unexercised by every test that goes through this door.
func responseURLClient(srv *httptest.Server) *http.Client {
	target, err := url.Parse(srv.URL)
	if err != nil {
		panic("responseURLClient: httptest server URL unparseable: " + err.Error())
	}
	cli := srv.Client()
	base := cli.Transport
	return &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		return base.RoundTrip(r)
	})}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// richTextPlain flattens a rich_text block to its visible text, so a test can
// assert on a container's title without knowing the element structure.
func richTextPlain(rt *slackapi.RichTextBlock) string {
	if rt == nil {
		return ""
	}
	var sb strings.Builder
	for _, el := range rt.Elements {
		sec, ok := el.(*slackapi.RichTextSection)
		if !ok {
			continue
		}
		for _, e := range sec.Elements {
			switch v := e.(type) {
			case *slackapi.RichTextSectionTextElement:
				sb.WriteString(v.Text)
			case *slackapi.RichTextSectionEmojiElement:
				sb.WriteString(":" + v.Name + ":")
			}
		}
	}
	return sb.String()
}
