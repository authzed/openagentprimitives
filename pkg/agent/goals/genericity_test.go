package goals_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"

	// Every channel kind a binary links, so the guard is built from the real
	// registry rather than from a list kept here.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// ordinaryWordKinds are registered kind names that are also ordinary words in
// this code — "agent" is in every package path here, "github" in every import
// — so they cannot be matched wherever they appear. For these the guard flags
// only the name as a whole quoted string ("github"), which is how a
// comparison or a switch on a kind names it, and only outside test files:
// tests need some kind to run a group on and use these as fixtures.
//
// A kind not listed here gets the strict rule, so a newly registered kind is
// guarded in full without anyone editing this file.
var ordinaryWordKinds = map[string]bool{"agent": true, "browser": true, "fake": true, "github": true, "local": true}

// kindMatcher reports whether a line of file names one of the transport
// kinds in names. Group goals are kind-neutral: what differs per kind lives
// behind channelkinds.GroupGoalHost, so a kind name in these packages means
// kind-specific behavior has leaked into the generic layer.
//
// A kind's name is matched in any spelling and position (slack_channel,
// Slack), in every file, unless it is an ordinary word (ordinaryWordKinds).
func kindMatcher(names []string) func(file, line string) bool {
	var anywhere, quoted []string
	for _, n := range names {
		if ordinaryWordKinds[n] {
			quoted = append(quoted, regexp.QuoteMeta(n))
			continue
		}
		anywhere = append(anywhere, regexp.QuoteMeta(n))
	}
	// A pattern with no alternatives would match every line.
	never := regexp.MustCompile(`a^`)
	strict, literal := never, never
	if len(anywhere) > 0 {
		strict = regexp.MustCompile(`(?i)(` + strings.Join(anywhere, "|") + `)`)
	}
	if len(quoted) > 0 {
		literal = regexp.MustCompile("[\"`](" + strings.Join(quoted, "|") + ")[\"`]")
	}
	return func(file, line string) bool {
		if strict.MatchString(line) {
			return true
		}
		return !strings.HasSuffix(file, "_test.go") && literal.MatchString(line)
	}
}

// TestGenericityGuard walks every group-goal package, including its tests, and
// fails on a kind name. The roots cover pkg/agent/goals (and so its manage.go),
// pkg/web/goals (and so its manage.go), pkg/controllers/goals,
// pkg/memory/goals, the pipeline's goal_*.go files, the binaries' goal*.go
// files under internal/cmd, and the agent_goal blocks of the SpiceDB schema.
//
// Each root must yield files: a walk that finds nothing would pass while
// guarding nothing, as after a package move.
func TestGenericityGuard(t *testing.T) {
	names := registry.Names()
	require.Contains(t, names, "slack", "the kind packages are not registered: the guard would guard nothing")
	for n := range ordinaryWordKinds {
		require.Contains(t, names, n, "ordinaryWordKinds lists %q, which is not a registered kind; remove it", n)
	}
	namesKind := kindMatcher(names)

	t.Run("the matcher flags a kind name, and only a kind name", func(t *testing.T) {
		for _, tc := range []struct {
			name, file, line string
			want             bool
		}{
			{name: "a comparison with a kind in generic code: flagged", file: "dispatcher.go", line: `if ch.Spec.Kind == "github" {`, want: true},
			{name: "a case on an ordinary-word kind in generic code: flagged", file: "group.go", line: "case `browser`:", want: true},
			{name: "a kind-specific identifier, any spelling: flagged", file: "service.go", line: `owner := "slack_channel:" + id`, want: true},
			{name: "a capitalized kind in a comment: flagged", file: "service.go", line: `// Slack posts this in the thread.`, want: true},
			{name: "a kind-specific identifier in a test: flagged", file: "service_test.go", line: `Group: "slack_channel:C1#member"`, want: true},
			{name: "a strictly matched kind as a test fixture: flagged", file: "service_test.go", line: `Spec: v1.ChannelSpec{Kind: "bento"}`, want: true},
			{name: "an ordinary-word kind as a test fixture: allowed", file: "reply_test.go", line: `Spec: v1.ChannelSpec{Kind: "fake"}`},
			{name: "an import path: allowed", file: "service.go", line: `"github.com/authzed/openagentprimitives/pkg/agent/goals"`},
			{name: "an ordinary word that is also a kind: allowed", file: "service.go", line: `// The agent's local clock.`},
			{name: "a line naming no kind: allowed", file: "service.go", line: `return s.commit(ctx, a, next, r.Revision, prior, r.RequestID, h, r.Action)`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				assert.Equal(t, tc.want, namesKind(tc.file, tc.line))
			})
		}
	})

	repo := filepath.Join("..", "..", "..")
	dirs := []string{"pkg/agent/goals", "pkg/web/goals", "pkg/controllers/goals", "pkg/memory/goals"}
	self := "genericity_test.go"

	var files []string
	for _, d := range dirs {
		n := 0
		err := filepath.WalkDir(filepath.Join(repo, d), func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() || filepath.Ext(p) == "" || strings.HasSuffix(p, string(filepath.Separator)+self) {
				return nil
			}
			files = append(files, p)
			n++
			return nil
		})
		require.NoError(t, err, d)
		require.NotZero(t, n, "root %s has no files: the guard would guard nothing", d)
	}
	// The pipeline and the binaries hold goal code beside everything else
	// they do, so only their goal files are walked.
	for _, pattern := range []string{"pkg/channels/channelsd/pipeline/goal_*.go", "internal/cmd/*/goal*.go"} {
		found, err := filepath.Glob(filepath.Join(repo, pattern))
		require.NoError(t, err)
		require.NotEmpty(t, found, "no files match %s: the guard would guard nothing", pattern)
		files = append(files, found...)
	}

	for _, f := range files {
		b, err := os.ReadFile(f)
		require.NoError(t, err, f)
		for i, line := range strings.Split(string(b), "\n") {
			assert.False(t, namesKind(f, line), "%s:%d names a transport kind; move it behind channelkinds.GroupGoalHost: %s", f, i+1, strings.TrimSpace(line))
		}
	}

	t.Run("schema agent_goal blocks", func(t *testing.T) {
		b, err := os.ReadFile(filepath.Join(repo, "pkg/authz/spicedb/schema/schema.zed"))
		require.NoError(t, err)
		blocks := regexp.MustCompile(`(?ms)^definition agent_goal\w*\s*\{.*?^\}`).FindAllString(string(b), -1)
		require.GreaterOrEqual(t, len(blocks), 3, "agent_goal, agent_goal_domain and agent_goal_execution must all be found")
		for _, blk := range blocks {
			for _, line := range strings.Split(blk, "\n") {
				assert.False(t, namesKind("schema.zed", line), "an agent_goal block names a transport kind: %s", strings.TrimSpace(line))
			}
		}
	})
}
