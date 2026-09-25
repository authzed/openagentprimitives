package skillsource

import (
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

	got, _, problems := Discover("github.com/someorg/somerepo", "skills", "v1.2.0", files)

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
	_, _, problems := Discover("github.com/someorg/somerepo", "skills", "v1.2.0", files)
	require.Len(t, problems, 4)
	assert.True(t, sort.StringsAreSorted(problems), "problems must be deterministically ordered: %v", problems)
}

func TestDiscoverNoRef(t *testing.T) {
	files := map[string][]byte{
		"a/SKILL.md": []byte("---\nname: a\ndescription: d\n---\nbody"),
	}
	got, _, _ := Discover("github.com/o/r", "", "", files)
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
	_, instr, _ := Discover("github.com/org/repo", "skills", "v1", files)
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
	got, _, problems := Discover("github.com/someorg/somerepo", "plugins/review/skills/review", "main", files)

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

	got, _, problems := Discover("github.com/someorg/somerepo", "plugins/review/skills/review-pr", "main", files)

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
		map[string][]byte{"README.md": []byte("no skills here")})

	assert.Empty(t, problems)
}
