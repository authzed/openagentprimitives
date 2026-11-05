package skillsource

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscover(t *testing.T) {
	files := map[string][]byte{
		"skills/skillone/SKILL.md":     []byte("---\nname: skillone\ndescription: Use for one.\n---\nBody one."),
		"skills/skillone/scripts/a.sh": []byte("echo a"),
		"skills/skilltwo/SKILL.md":     []byte("---\nname: skilltwo\ndescription: Use for two.\n---\nBody two."),
		"docs/README.md":               []byte("not a skill"),
		// invalid: frontmatter name mismatches its directory → skipped with a problem
		"skills/bad/SKILL.md": []byte("---\nname: wrong\ndescription: x\n---\nbody"),
	}

	got, _, problems := Discover("github.com/someorg/somerepo", "skills", "v1.2.0", files, DiscoverOptions{})

	require.Len(t, got, 2)
	byName := map[string]DiscoveredSkill{}
	for _, d := range got {
		byName[d.CanonicalName] = d
	}

	one, ok := byName["github.com/someorg/somerepo//skills/skillone@v1.2.0"]
	require.True(t, ok)
	assert.Equal(t, "Use for one.", one.Description)
	assert.Equal(t, "Body one.", one.Body)
	assert.NotEmpty(t, one.BundleDigest, "skillone has scripts/ → a bundle")
	assert.NotEmpty(t, one.BundleTarGz)

	two := byName["github.com/someorg/somerepo//skills/skilltwo@v1.2.0"]
	assert.Empty(t, two.BundleDigest, "skilltwo is instruction-only → no bundle")

	assert.NotEmpty(t, problems, "the name-mismatch skill is reported as a problem")
}

func TestDiscoverProblemsAreSorted(t *testing.T) {
	// problems is accumulated under a map range and lands verbatim in
	// status.discoveryProblems, which the reconciler now recomputes on every
	// pass. An unsorted order would look like a status change and re-enqueue the
	// reconcile — and its git fetch — indefinitely.
	files := map[string][]byte{
		"skills/aaa/SKILL.md": []byte("---\nname: wrong\ndescription: x\n---\nbody"),
		"skills/bbb/SKILL.md": []byte("---\nname: wrong\ndescription: x\n---\nbody"),
		"skills/ccc/SKILL.md": []byte("---\nname: wrong\ndescription: x\n---\nbody"),
		"skills/ddd/SKILL.md": []byte("---\nname: wrong\ndescription: x\n---\nbody"),
	}
	_, _, problems := Discover("github.com/someorg/somerepo", "skills", "v1.2.0", files, DiscoverOptions{})
	require.Len(t, problems, 4)
	assert.True(t, sort.StringsAreSorted(problems), "problems must be deterministically ordered: %v", problems)
}

func TestDiscoverNoRef(t *testing.T) {
	files := map[string][]byte{
		"a/SKILL.md": []byte("---\nname: a\ndescription: d\n---\nbody"),
	}
	got, _, _ := Discover("github.com/o/r", "", "", files, DiscoverOptions{})
	require.Len(t, got, 1)
	assert.Equal(t, "github.com/o/r//a", got[0].CanonicalName, "no ref → unpinned canonical name")
}

func TestFindRepoInstructions(t *testing.T) {
	cases := []struct {
		name      string
		files     map[string][]byte
		wantFile  string
		wantBody  string
		wantFound bool
	}{
		{
			name:     "AGENTS.md present: returned",
			files:    map[string][]byte{"AGENTS.md": []byte("agents body"), "skills/a/SKILL.md": []byte("x")},
			wantFile: "AGENTS.md", wantBody: "agents body", wantFound: true,
		},
		{
			name:     "both present: AGENTS.md wins over CLAUDE.md",
			files:    map[string][]byte{"CLAUDE.md": []byte("claude body"), "AGENTS.md": []byte("agents body")},
			wantFile: "AGENTS.md", wantBody: "agents body", wantFound: true,
		},
		{
			name:     "only CLAUDE.md: CLAUDE.md returned",
			files:    map[string][]byte{"CLAUDE.md": []byte("claude body")},
			wantFile: "CLAUDE.md", wantBody: "claude body", wantFound: true,
		},
		{
			name:      "non-root AGENTS.md ignored: root-only lookup",
			files:     map[string][]byte{"sub/dir/AGENTS.md": []byte("nested")},
			wantFound: false,
		},
		{
			name:      "none present: not found",
			files:     map[string][]byte{"skills/a/SKILL.md": []byte("x")},
			wantFound: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file, content, found := findRepoInstructions(tc.files)
			assert.Equal(t, tc.wantFound, found)
			assert.Equal(t, tc.wantFile, file)
			assert.Equal(t, tc.wantBody, string(content))
		})
	}
}

func TestDiscoverReturnsRepoInstructions(t *testing.T) {
	files := map[string][]byte{
		"AGENTS.md":           []byte("repo conventions"),
		"skills/foo/SKILL.md": []byte("---\nname: foo\ndescription: A foo skill that does foo things well.\n---\nbody"),
	}
	_, instr, _ := Discover("github.com/org/repo", "skills", "v1", files, DiscoverOptions{})
	assert.Equal(t, "AGENTS.md", instr.SourceFile)
	assert.Equal(t, "repo conventions", instr.Content)
}

func TestDiscoverSubpathMatchingNothingIsAProblem(t *testing.T) {
	// spec.subpath is the operator's own claim about where the skill lives. When
	// it drifts by one segment from the repo's real layout — a directory renamed
	// upstream, a typo at authoring time, a ref that predates the skill — the
	// filter simply matches nothing. Zero skills and zero problems is also
	// exactly what an empty repo produces, so without this the pass is
	// indistinguishable from a clean sync of a repo that has no skills, and the
	// only thing that looks wrong is the AgentClass parked at SkillMissing.
	files := map[string][]byte{
		"plugins/review/skills/review-pr/SKILL.md": []byte("---\nname: review-pr\ndescription: Use when reviewing.\n---\nBody."),
		"plugins/review/README.md":                 []byte("not a skill"),
	}

	// One segment short of the real directory.
	got, _, problems := Discover("github.com/someorg/somerepo", "plugins/review/skills/review", "main", files, DiscoverOptions{})

	assert.Empty(t, got, "the drifted subpath matches no skill")
	require.Len(t, problems, 1, "a subpath that matched no SKILL.md must be reported")
	assert.Contains(t, problems[0], "plugins/review/skills/review",
		"the problem must name the subpath that matched nothing")
}

func TestDiscoverSubpathThatMatchedIsNotAProblem(t *testing.T) {
	// The converse guard: a subpath that DID match must not gain a problem, or
	// every healthy source starts reporting one.
	files := map[string][]byte{
		"plugins/review/skills/review-pr/SKILL.md": []byte("---\nname: review-pr\ndescription: Use when reviewing.\n---\nBody."),
	}

	got, _, problems := Discover("github.com/someorg/somerepo", "plugins/review/skills/review-pr", "main", files, DiscoverOptions{})

	require.Len(t, got, 1)
	assert.Equal(t, "github.com/someorg/somerepo//plugins/review/skills/review-pr@main", got[0].CanonicalName)
	assert.Empty(t, problems)
}

func TestDiscoverWholeTreeWithNoSkillMDIsNotASubpathProblem(t *testing.T) {
	// An empty spec.subpath makes no claim about layout, so "found nothing"
	// there is not a subpath problem — RecordSync is what reports the empty
	// outcome. Guards against the check firing on every source that scans a
	// whole repo.
	_, _, problems := Discover("github.com/someorg/somerepo", "", "main",
		map[string][]byte{"README.md": []byte("no skills here")}, DiscoverOptions{})

	assert.Empty(t, problems)
}

// pluginTree is a repo laid out the way Claude Code loads plugins: a manifest,
// one skill under skills/, and agents beside it. root is the plugin's
// directory ("" for a repo that IS the plugin).
func pluginTree(root string) map[string][]byte {
	p := func(rel string) string {
		if root == "" {
			return rel
		}
		return root + "/" + rel
	}
	return map[string][]byte{
		p(".claude-plugin/plugin.json"):        []byte(`{"name":"demo-plugin","version":"1.0.0"}`),
		p("skills/demo-review/SKILL.md"):       []byte("---\nname: demo-review\ndescription: Use for review.\n---\nLaunch the bug-hunter agent."),
		p("skills/demo-review/methodology.md"): []byte("how"),
		p("agents/bug-hunter.md"):              []byte("---\nname: bug-hunter\ndescription: hunts bugs\n---\nprompt"),
		p("agents/security-auditor.md"):        []byte("---\nname: security-auditor\ndescription: audits\n---\nprompt"),
		p("agents/README.txt"):                 []byte("not an agent"),
		p("agents/nested/deep.md"):             []byte("not a direct child"),
		p("hooks/hooks.json"):                  []byte(`{"hooks":{}}`),
		p(".mcp.json"):                         []byte(`{"mcpServers":{}}`),
	}
}

func TestDiscoverPlugin(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string][]byte
		subpath string
		opts    DiscoverOptions
		check   func(t *testing.T, d DiscoveredSkill, problems []string)
	}{
		{
			name:    "skill inside a plugin: name and direct agents/*.md captured, nothing else",
			files:   pluginTree("plugins/demo"),
			subpath: "plugins/demo/skills/demo-review",
			check: func(t *testing.T, d DiscoveredSkill, problems []string) {
				require.NotNil(t, d.Plugin)
				assert.Equal(t, "demo-plugin", d.Plugin.Name)
				require.Len(t, d.Plugin.Agents, 2, "only direct *.md children of agents/ are agents")
				assert.Equal(t, "bug-hunter", d.Plugin.Agents[0].Name, "agents are sorted by name")
				assert.Equal(t, "security-auditor", d.Plugin.Agents[1].Name)
				assert.Contains(t, d.Plugin.Agents[0].Content, "name: bug-hunter", "content is verbatim, frontmatter included")
				assert.Empty(t, problems)
			},
		},
		{
			name:    "repo root is the plugin: captured",
			files:   pluginTree(""),
			subpath: "skills/demo-review",
			check: func(t *testing.T, d DiscoveredSkill, _ []string) {
				require.NotNil(t, d.Plugin)
				assert.Equal(t, "demo-plugin", d.Plugin.Name)
			},
		},
		{
			name:    "source opted out: standalone, as before plugins were captured",
			files:   pluginTree("plugins/demo"),
			subpath: "plugins/demo/skills/demo-review",
			opts:    DiscoverOptions{SkipPlugins: true},
			check: func(t *testing.T, d DiscoveredSkill, problems []string) {
				assert.Nil(t, d.Plugin)
				assert.Empty(t, problems)
			},
		},
		{
			name: "no manifest: standalone, no problem",
			files: map[string][]byte{
				"skills/demo-review/SKILL.md": []byte("---\nname: demo-review\ndescription: d\n---\nbody"),
				"agents/bug-hunter.md":        []byte("prompt"),
			},
			subpath: "skills/demo-review",
			check: func(t *testing.T, d DiscoveredSkill, problems []string) {
				assert.Nil(t, d.Plugin)
				assert.Empty(t, problems)
			},
		},
		{
			name: "skill not under a skills/ dir: standalone even beside a manifest",
			files: map[string][]byte{
				".claude-plugin/plugin.json": []byte(`{"name":"demo-plugin"}`),
				"tools/demo-review/SKILL.md": []byte("---\nname: demo-review\ndescription: d\n---\nbody"),
				"agents/bug-hunter.md":       []byte("prompt"),
			},
			subpath: "tools/demo-review",
			check: func(t *testing.T, d DiscoveredSkill, _ []string) {
				assert.Nil(t, d.Plugin)
			},
		},
		{
			name: "invalid manifest name: standalone, problem reported",
			files: func() map[string][]byte {
				f := pluginTree("")
				f[".claude-plugin/plugin.json"] = []byte(`{"name":"Not Kebab"}`)
				return f
			}(),
			subpath: "skills/demo-review",
			check: func(t *testing.T, d DiscoveredSkill, problems []string) {
				assert.Nil(t, d.Plugin)
				require.Len(t, problems, 1)
				assert.Contains(t, problems[0], "kebab-case")
			},
		},
		{
			name: "malformed manifest JSON: standalone, problem reported",
			files: func() map[string][]byte {
				f := pluginTree("")
				f[".claude-plugin/plugin.json"] = []byte(`{`)
				return f
			}(),
			subpath: "skills/demo-review",
			check: func(t *testing.T, d DiscoveredSkill, problems []string) {
				assert.Nil(t, d.Plugin)
				require.Len(t, problems, 1)
				assert.Contains(t, problems[0], "not valid JSON")
			},
		},
		{
			name: "manifest declares custom agent paths: default dir still captured, problem reported",
			files: func() map[string][]byte {
				f := pluginTree("")
				f[".claude-plugin/plugin.json"] = []byte(`{"name":"demo-plugin","agents":["./custom/x.md"]}`)
				return f
			}(),
			subpath: "skills/demo-review",
			check: func(t *testing.T, d DiscoveredSkill, problems []string) {
				require.NotNil(t, d.Plugin)
				assert.Len(t, d.Plugin.Agents, 2)
				require.Len(t, problems, 1)
				assert.Contains(t, problems[0], "custom agent paths")
			},
		},
		{
			name: "agent with an unusable file name: skipped, problem reported, rest captured",
			files: func() map[string][]byte {
				f := pluginTree("")
				f["agents/.hidden.md"] = []byte("prompt")
				return f
			}(),
			subpath: "skills/demo-review",
			check: func(t *testing.T, d DiscoveredSkill, problems []string) {
				require.NotNil(t, d.Plugin)
				assert.Len(t, d.Plugin.Agents, 2)
				require.Len(t, problems, 1)
				assert.Contains(t, problems[0], ".hidden")
			},
		},
		{
			name: "oversized agent: skipped, problem reported",
			files: func() map[string][]byte {
				f := pluginTree("")
				f["agents/huge.md"] = bytes.Repeat([]byte("x"), maxPluginAgentBytes+1)
				return f
			}(),
			subpath: "skills/demo-review",
			check: func(t *testing.T, d DiscoveredSkill, problems []string) {
				require.NotNil(t, d.Plugin)
				assert.Len(t, d.Plugin.Agents, 2)
				require.Len(t, problems, 1)
				assert.Contains(t, problems[0], "huge.md")
			},
		},
		{
			name: "agents over the total ceiling: alphabetical prefix kept, problem reported",
			files: func() map[string][]byte {
				f := pluginTree("")
				chunk := bytes.Repeat([]byte("x"), maxPluginAgentBytes)
				for _, n := range []string{"a1", "a2", "a3", "a4", "a5"} {
					f["agents/"+n+".md"] = chunk
				}
				return f
			}(),
			subpath: "skills/demo-review",
			check: func(t *testing.T, d DiscoveredSkill, problems []string) {
				require.NotNil(t, d.Plugin)
				require.Len(t, d.Plugin.Agents, 4, "four 64 KiB agents fill the 256 KiB ceiling exactly")
				assert.Equal(t, "a4", d.Plugin.Agents[3].Name)
				require.Len(t, problems, 1)
				assert.Contains(t, problems[0], "capturing 4 of 7")
			},
		},
		{
			name: "more agents than the CRD's MaxItems: alphabetical prefix kept, problem reported",
			files: func() map[string][]byte {
				f := pluginTree("")
				for i := range maxPluginAgents {
					f[fmt.Sprintf("agents/z%03d.md", i)] = []byte("prompt")
				}
				return f
			}(),
			subpath: "skills/demo-review",
			check: func(t *testing.T, d DiscoveredSkill, problems []string) {
				require.NotNil(t, d.Plugin)
				require.Len(t, d.Plugin.Agents, maxPluginAgents, "one more and the apiserver rejects the Skill")
				assert.Equal(t, "bug-hunter", d.Plugin.Agents[0].Name)
				assert.Equal(t, fmt.Sprintf("z%03d", maxPluginAgents-3), d.Plugin.Agents[maxPluginAgents-1].Name)
				require.Len(t, problems, 1)
				assert.Contains(t, problems[0], fmt.Sprintf("capturing %d of %d", maxPluginAgents, maxPluginAgents+2))
			},
		},
		{
			name: "manifest agents key empty or null: no problem reported",
			files: func() map[string][]byte {
				f := pluginTree("")
				f[".claude-plugin/plugin.json"] = []byte(`{"name":"demo-plugin","agents":null}`)
				return f
			}(),
			subpath: "skills/demo-review",
			check: func(t *testing.T, d DiscoveredSkill, problems []string) {
				require.NotNil(t, d.Plugin)
				assert.Len(t, d.Plugin.Agents, 2)
				assert.Empty(t, problems)
			},
		},
		{
			name: "skill ships its own reserved plugin dir: dropped from the bundle, problem reported",
			files: func() map[string][]byte {
				f := pluginTree("")
				f["skills/demo-review/.oap-plugin/agents/planted.md"] = []byte("planted")
				return f
			}(),
			subpath: "skills/demo-review",
			check: func(t *testing.T, d DiscoveredSkill, problems []string) {
				files := untarBundle(t, d.BundleTarGz)
				assert.Contains(t, files, "methodology.md")
				assert.NotContains(t, files, ".oap-plugin/agents/planted.md")
				require.Len(t, problems, 1)
				assert.Contains(t, problems[0], "reserved")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, problems := Discover("github.com/demo-org/demo-repo", tc.subpath, "v1", tc.files, tc.opts)
			require.Len(t, got, 1)
			tc.check(t, got[0], problems)
		})
	}
}

// untarBundle unpacks a discovered bundle archive into its files by path.
func untarBundle(t *testing.T, archive []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	require.NoError(t, err)
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		require.NoError(t, err)
		b, err := io.ReadAll(tr)
		require.NoError(t, err)
		files[hdr.Name] = b
	}
}
