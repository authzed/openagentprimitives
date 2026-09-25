// channelsd is the transport-AGNOSTIC relay: it resolves a Channel's kind
// through pkg/channels/channelkinds/registry and hands work to that kind. Every wire
// protocol — Block Kit, mrkdwn, chat.postEphemeral, the External map keys a
// kind stamps on a binding — belongs behind that seam.
package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// transportSDKs are the vendored client libraries that speak one channel
// kind's wire protocol. A binary that imports one has, by construction, taken
// on knowledge of that transport.
var transportSDKs = []string{
	"github.com/slack-go/slack",
}

// TestChannelsdImportsNoTransportSDK is the structural guard behind the
// metaagent_scope_approval / metaagent_notice handlers.
//
// Those two handlers built Slack Block Kit inline, read
// binding.External["channel_id"] / ["thread_ts"] (keys only
// pkg/channels/channelkinds/slack ever writes), and resolved a Slack bot client
// directly. The consequence was not only a layering complaint: a session on
// any other kind had its scope approval dropped with an Info log, while the
// runner blocked for the whole approval window and then halted "refusing to
// run unscoped". A generic handler that silently only works for one kind is
// exactly what AGENTS.md §Pluggability means by "the abstraction is missing a
// method".
//
// Non-test files only: a test may legitimately construct a kind's payloads to
// assert what the generic path produced.
func TestChannelsdImportsNoTransportSDK(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err, "read internal/cmd/channelsd")

	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		require.NoError(t, err, "parse %s", name)

		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err, "unquote import in %s", name)
			for _, sdk := range transportSDKs {
				assert.NotEqual(t, sdk, path,
					"%s imports the %s SDK: channelsd must reach a transport only through "+
						"channelkinds.Kind (SubChannelSender for a bespoke surface), never by "+
						"speaking its wire protocol — otherwise every other kind silently gets nothing",
					name, sdk)
			}
		}
	}
}
