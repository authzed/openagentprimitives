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
)

// kindWord matches any spelling of a transport kind's name, including
// slack_channel. Group goals are kind-neutral: what differs per kind lives
// behind channelkinds.GroupGoalHost, so a kind name in these packages means
// kind-specific behavior has leaked into the generic layer.
var kindWord = regexp.MustCompile(`(?i)slack`)

// TestGenericityGuard walks every group-goal package, including its tests, and
// fails on a kind name. The roots cover pkg/agent/goals (and so its manage.go),
// pkg/web/goals (and so its manage.go), pkg/controllers/goals,
// pkg/memory/goals, the pipeline's goal_*.go files, and the agent_goal blocks
// of the SpiceDB schema.
//
// Each root must yield files: a walk that finds nothing would pass while
// guarding nothing, as after a package move.
func TestGenericityGuard(t *testing.T) {
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
	pipe, err := filepath.Glob(filepath.Join(repo, "pkg/channels/channelsd/pipeline/goal_*.go"))
	require.NoError(t, err)
	require.NotEmpty(t, pipe, "no pipeline goal_*.go files: the guard would guard nothing")
	files = append(files, pipe...)

	for _, f := range files {
		b, err := os.ReadFile(f)
		require.NoError(t, err, f)
		for i, line := range strings.Split(string(b), "\n") {
			assert.False(t, kindWord.MatchString(line), "%s:%d names a transport kind; move it behind channelkinds.GroupGoalHost: %s", f, i+1, strings.TrimSpace(line))
		}
	}

	t.Run("schema agent_goal blocks", func(t *testing.T) {
		b, err := os.ReadFile(filepath.Join(repo, "pkg/authz/spicedb/schema/schema.zed"))
		require.NoError(t, err)
		blocks := regexp.MustCompile(`(?ms)^definition agent_goal\w*\s*\{.*?^\}`).FindAllString(string(b), -1)
		require.GreaterOrEqual(t, len(blocks), 3, "agent_goal, agent_goal_domain and agent_goal_execution must all be found")
		for _, blk := range blocks {
			assert.False(t, kindWord.MatchString(blk), "an agent_goal block names a transport kind:\n%s", blk)
		}
	})
}
