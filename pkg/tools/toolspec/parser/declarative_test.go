package parser

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

func tkWith(subs ...toolkit.Subcommand) *toolkit.Toolkit {
	return &toolkit.Toolkit{
		Name:            "t",
		Version:         "1",
		ToolkitRevision: "r",
		Target:          toolkit.Target{Binary: "t"},
		Parser:          toolkit.ParserConfig{Kind: "declarative"},
		Subcommands:     subs,
	}
}

func emptyEffects() toolkit.Effects {
	return toolkit.Effects{
		Reads: []string{}, Writes: []string{},
		Network:    toolkit.NetworkEffect{Destinations: []string{}},
		Filesystem: toolkit.FilesystemEffect{Paths: []string{}},
		Creds:      toolkit.CredsEffect{Required: []string{}, Writes: []string{}},
	}
}

// TestDeclarative_Subcommand_Routing covers subcommand resolution: flat,
// nested, and the empty-path fallback.
func TestDeclarative_Subcommand_Routing(t *testing.T) {
	cases := []struct {
		name        string
		tk          *toolkit.Toolkit
		argv        []string
		wantSubcmd  string
		wantPathLen int
	}{
		{
			name:        "flat subcommand routes by single token",
			tk:          tkWith(toolkit.Subcommand{Path: []string{"say"}, Effects: emptyEffects()}),
			argv:        []string{"say"},
			wantSubcmd:  "say",
			wantPathLen: 1,
		},
		{
			name:        "nested subcommand routes by two tokens",
			tk:          tkWith(toolkit.Subcommand{Path: []string{"pr", "view"}, Effects: emptyEffects()}),
			argv:        []string{"pr", "view"},
			wantSubcmd:  "pr view",
			wantPathLen: 2,
		},
	}
	p := &Declarative{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call, err := p.Parse(tc.tk, tc.argv)
			require.NoError(t, err, "Parse")
			assert.Equal(t, tc.wantSubcmd, call.Subcommand, "Subcommand")
			assert.Len(t, call.SubcommandPath, tc.wantPathLen, "SubcommandPath length")
		})
	}
}

// TestDeclarative_ParseErrors covers all failure modes that should return a
// *ParseError with a specific Kind.
func TestDeclarative_ParseErrors(t *testing.T) {
	cases := []struct {
		name     string
		tk       *toolkit.Toolkit
		argv     []string
		wantKind ErrorKind
	}{
		{
			name:     "unknown subcommand: KindUnknownSubcommand",
			tk:       tkWith(toolkit.Subcommand{Path: []string{"view"}, Effects: emptyEffects()}),
			argv:     []string{"foo"},
			wantKind: KindUnknownSubcommand,
		},
		{
			name:     "unknown flag: KindUnknownFlag",
			tk:       tkWith(toolkit.Subcommand{Path: []string{"x"}, Effects: emptyEffects()}),
			argv:     []string{"x", "--nope"},
			wantKind: KindUnknownFlag,
		},
		{
			name: "missing flag value: KindMissingFlagValue",
			tk: tkWith(toolkit.Subcommand{
				Path:    []string{"x"},
				Flags:   []toolkit.Flag{{Long: "name", Type: "string"}},
				Effects: emptyEffects(),
			}),
			argv:     []string{"x", "--name"},
			wantKind: KindMissingFlagValue,
		},
		{
			name: "int flag with non-numeric value: KindFlagTypeMismatch",
			tk: tkWith(toolkit.Subcommand{
				Path:    []string{"x"},
				Flags:   []toolkit.Flag{{Long: "count", Type: "int"}},
				Effects: emptyEffects(),
			}),
			argv:     []string{"x", "--count", "foo"},
			wantKind: KindFlagTypeMismatch,
		},
		{
			name: "enum flag with out-of-set value: KindEnumValueInvalid",
			tk: tkWith(toolkit.Subcommand{
				Path:    []string{"x"},
				Flags:   []toolkit.Flag{{Long: "mode", Type: "enum", Values: []string{"a", "b"}}},
				Effects: emptyEffects(),
			}),
			argv:     []string{"x", "--mode", "c"},
			wantKind: KindEnumValueInvalid,
		},
		{
			name: "missing required positional: KindArgCountMismatch",
			tk: tkWith(toolkit.Subcommand{
				Path:       []string{"pr", "view"},
				Positional: []toolkit.Positional{{Name: "idOrUrl", Type: "string", Required: true}},
				Effects:    emptyEffects(),
			}),
			argv:     []string{"pr", "view"},
			wantKind: KindArgCountMismatch,
		},
	}
	p := &Declarative{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.Parse(tc.tk, tc.argv)
			require.Error(t, err, "Parse should fail")
			var pe *ParseError
			require.True(t, errors.As(err, &pe), "err should be *ParseError; got %T: %v", err, err)
			assert.Equal(t, tc.wantKind, pe.Kind, "ParseError.Kind")
		})
	}
}

func TestDeclarative_ErrorsWithoutSpecificKind(t *testing.T) {
	cases := []struct {
		name string
		tk   *toolkit.Toolkit
		argv []string
	}{
		{
			name: "empty argv against non-empty-path toolkit errors",
			tk:   tkWith(toolkit.Subcommand{Path: []string{"view"}, Effects: emptyEffects()}),
			argv: []string{},
		},
		{
			name: "extra positional beyond declared schema errors",
			tk: tkWith(toolkit.Subcommand{
				Path:       []string{"x"},
				Positional: []toolkit.Positional{{Name: "only", Type: "string", Required: true}},
				Effects:    emptyEffects(),
			}),
			argv: []string{"x", "a", "b"},
		},
	}
	p := &Declarative{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.Parse(tc.tk, tc.argv)
			require.Error(t, err, "Parse should fail")
		})
	}
}

// TestDeclarative_Flags covers flag parsing across long/short/equals/bool/list
// shapes — same toolkit subcommand body, single field assertion per case.
func TestDeclarative_Flags(t *testing.T) {
	prViewWithRepo := toolkit.Subcommand{
		Path:    []string{"pr", "view"},
		Flags:   []toolkit.Flag{{Long: "repo", Short: "R", Type: "string"}},
		Effects: emptyEffects(),
	}
	cases := []struct {
		name      string
		sc        toolkit.Subcommand
		argv      []string
		flagName  string
		wantValue interface{}
	}{
		{
			name:      "long string flag with space separator binds value",
			sc:        prViewWithRepo,
			argv:      []string{"pr", "view", "--repo", "authzed/ap"},
			flagName:  "repo",
			wantValue: "authzed/ap",
		},
		{
			name:      "long string flag with equals separator binds value",
			sc:        prViewWithRepo,
			argv:      []string{"pr", "view", "--repo=authzed/ap"},
			flagName:  "repo",
			wantValue: "authzed/ap",
		},
		{
			name:      "short flag alias binds value",
			sc:        prViewWithRepo,
			argv:      []string{"pr", "view", "-R", "authzed/ap"},
			flagName:  "repo",
			wantValue: "authzed/ap",
		},
		{
			name: "bool flag with no value sets true",
			sc: toolkit.Subcommand{
				Path: []string{"pr", "view"},
				Flags: []toolkit.Flag{
					{Long: "web", Short: "w", Type: "bool"},
					{Long: "repo", Type: "string"},
				},
				Effects: emptyEffects(),
			},
			argv:      []string{"pr", "view", "--web", "--repo", "x"},
			flagName:  "web",
			wantValue: true,
		},
		{
			name: "int flag binds int64",
			sc: toolkit.Subcommand{
				Path:    []string{"x"},
				Flags:   []toolkit.Flag{{Long: "count", Type: "int"}},
				Effects: emptyEffects(),
			},
			argv:      []string{"x", "--count", "5"},
			flagName:  "count",
			wantValue: int64(5),
		},
		{
			name: "enum flag binds value when in set",
			sc: toolkit.Subcommand{
				Path:    []string{"x"},
				Flags:   []toolkit.Flag{{Long: "mode", Type: "enum", Values: []string{"a", "b"}}},
				Effects: emptyEffects(),
			},
			argv:      []string{"x", "--mode", "a"},
			flagName:  "mode",
			wantValue: "a",
		},
	}
	p := &Declarative{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call, err := p.Parse(tkWith(tc.sc), tc.argv)
			require.NoError(t, err, "Parse")
			assert.Equal(t, tc.wantValue, call.Flags[tc.flagName], "Flags[%s]", tc.flagName)
		})
	}
}

func TestDeclarative_StringListFlag_Repeatable(t *testing.T) {
	sc := toolkit.Subcommand{
		Path:    []string{"x"},
		Flags:   []toolkit.Flag{{Long: "tag", Type: "stringList"}},
		Effects: emptyEffects(),
	}
	p := &Declarative{}
	call, err := p.Parse(tkWith(sc), []string{"x", "--tag", "a", "--tag", "b"})
	require.NoError(t, err, "Parse")
	got, ok := call.Flags["tag"].([]string)
	require.True(t, ok, "Flags[tag] should be []string; got %T", call.Flags["tag"])
	assert.Equal(t, []string{"a", "b"}, got, "Flags[tag]")
}

func TestDeclarative_StringListFlag_SplitOn(t *testing.T) {
	sc := toolkit.Subcommand{
		Path:    []string{"x"},
		Flags:   []toolkit.Flag{{Long: "tags", Type: "stringList", SplitOn: ","}},
		Effects: emptyEffects(),
	}
	p := &Declarative{}
	call, err := p.Parse(tkWith(sc), []string{"x", "--tags", "a,b,c"})
	require.NoError(t, err, "Parse")
	got, ok := call.Flags["tags"].([]string)
	require.True(t, ok, "Flags[tags] should be []string; got %T", call.Flags["tags"])
	assert.Equal(t, []string{"a", "b", "c"}, got, "Flags[tags]")
}

func TestDeclarative_PositionalRequired_Bound(t *testing.T) {
	sc := toolkit.Subcommand{
		Path:       []string{"pr", "view"},
		Positional: []toolkit.Positional{{Name: "idOrUrl", Type: "string", Required: true}},
		Effects:    emptyEffects(),
	}
	p := &Declarative{}
	call, err := p.Parse(tkWith(sc), []string{"pr", "view", "123"})
	require.NoError(t, err, "Parse")
	assert.Equal(t, "123", call.Positional["idOrUrl"], "Positional[idOrUrl]")
}

func TestDeclarative_PositionalMixedWithFlags(t *testing.T) {
	sc := toolkit.Subcommand{
		Path:       []string{"pr", "view"},
		Positional: []toolkit.Positional{{Name: "idOrUrl", Type: "string", Required: true}},
		Flags:      []toolkit.Flag{{Long: "repo", Short: "R", Type: "string"}},
		Effects:    emptyEffects(),
	}
	p := &Declarative{}
	call, err := p.Parse(tkWith(sc), []string{"pr", "view", "123", "-R", "x/y"})
	require.NoError(t, err, "Parse")
	assert.Equal(t, "123", call.Positional["idOrUrl"], "Positional[idOrUrl]")
	assert.Equal(t, "x/y", call.Flags["repo"], "Flags[repo]")
}

func TestDeclarative_GlobalFlags(t *testing.T) {
	tk := &toolkit.Toolkit{
		Name: "t", Version: "1", ToolkitRevision: "r",
		Target:      toolkit.Target{Binary: "t"},
		Parser:      toolkit.ParserConfig{Kind: "declarative"},
		GlobalFlags: []toolkit.Flag{{Long: "help", Short: "h", Type: "bool"}},
		Subcommands: []toolkit.Subcommand{{Path: []string{"x"}, Effects: emptyEffects()}},
	}
	p := &Declarative{}
	call, err := p.Parse(tk, []string{"x", "--help"})
	require.NoError(t, err, "Parse")
	assert.Equal(t, true, call.Flags["help"], "Flags[help]")
}

// TestDeclarative_LeadingGlobalFlags covers global flags placed BEFORE the
// subcommand — git's required ordering for -C/--git-dir/--work-tree/--no-pager.
func TestDeclarative_LeadingGlobalFlags(t *testing.T) {
	// Toolkit mirroring git's relevant shape: path/bool/short-only global flags
	// plus a couple of subcommands.
	gitLike := &toolkit.Toolkit{
		Name: "git", Version: "1", ToolkitRevision: "r",
		Target: toolkit.Target{Binary: "git"},
		Parser: toolkit.ParserConfig{Kind: "declarative"},
		GlobalFlags: []toolkit.Flag{
			{Long: "git-dir", Type: "path"},
			{Long: "work-tree", Type: "path"},
			{Long: "no-pager", Type: "bool"},
			{Short: "C", Type: "path"}, // short-only, like git's -C
		},
		Subcommands: []toolkit.Subcommand{
			{Path: []string{"status"}, Effects: emptyEffects()},
			{Path: []string{"log"}, Effects: emptyEffects()},
			{
				Path:    []string{"checkout"},
				Flags:   []toolkit.Flag{{Long: "branch", Short: "b", Type: "string"}},
				Effects: emptyEffects(),
			},
			{
				Path: []string{"clone"},
				Positional: []toolkit.Positional{
					{Name: "repository", Type: "string", Required: true},
					{Name: "directory", Type: "path", Required: false},
				},
				Effects: emptyEffects(),
			},
		},
	}
	p := &Declarative{}

	t.Run("long path globals before subcommand: Subcommand=status, git-dir/work-tree bound", func(t *testing.T) {
		call, err := p.Parse(gitLike, []string{"--git-dir=/x/.git", "--work-tree=/x", "status"})
		require.NoError(t, err, "Parse")
		assert.Equal(t, "status", call.Subcommand, "Subcommand")
		assert.Equal(t, "/x/.git", call.Flags["git-dir"], "Flags[git-dir]")
		assert.Equal(t, "/x", call.Flags["work-tree"], "Flags[work-tree]")
	})

	t.Run("short-only -C before subcommand: Subcommand=checkout, -C consumed, -b parsed after", func(t *testing.T) {
		call, err := p.Parse(gitLike, []string{"-C", "/x", "checkout", "-b", "foo"})
		require.NoError(t, err, "Parse")
		assert.Equal(t, "checkout", call.Subcommand, "Subcommand")
		assert.Equal(t, "/x", call.Flags["C"], "Flags[C] (short-only global)")
		assert.Equal(t, "foo", call.Flags["branch"], "Flags[branch] (post-subcommand)")
	})

	t.Run("bool global before subcommand: Subcommand=log, no-pager consumed", func(t *testing.T) {
		call, err := p.Parse(gitLike, []string{"--no-pager", "log"})
		require.NoError(t, err, "Parse")
		assert.Equal(t, "log", call.Subcommand, "Subcommand")
		assert.Equal(t, true, call.Flags["no-pager"], "Flags[no-pager]")
	})

	t.Run("no leading globals: Subcommand=clone, positionals intact (regression)", func(t *testing.T) {
		call, err := p.Parse(gitLike, []string{"clone", "https://example.com/r.git", "/workspace/repo"})
		require.NoError(t, err, "Parse")
		assert.Equal(t, "clone", call.Subcommand, "Subcommand")
		assert.Equal(t, "https://example.com/r.git", call.Positional["repository"], "Positional[repository]")
		assert.Equal(t, "/workspace/repo", call.Positional["directory"], "Positional[directory]")
	})

	t.Run("leading globals with no subcommand after: KindUnknownSubcommand", func(t *testing.T) {
		_, err := p.Parse(gitLike, []string{"-C", "/x"})
		require.Error(t, err, "Parse should fail")
		var pe *ParseError
		require.True(t, errors.As(err, &pe), "err should be *ParseError; got %T: %v", err, err)
		assert.Equal(t, KindUnknownSubcommand, pe.Kind, "ParseError.Kind")
	})

	t.Run("leading non-global non-subcommand token: KindUnknownSubcommand", func(t *testing.T) {
		_, err := p.Parse(gitLike, []string{"--bogus", "status"})
		require.Error(t, err, "Parse should fail")
		var pe *ParseError
		require.True(t, errors.As(err, &pe), "err should be *ParseError; got %T: %v", err, err)
		assert.Equal(t, KindUnknownSubcommand, pe.Kind, "ParseError.Kind")
	})

	t.Run("value-taking global flag with no value: MissingFlagValue", func(t *testing.T) {
		_, err := p.Parse(gitLike, []string{"-C"})
		require.Error(t, err, "Parse should fail")
		var pe *ParseError
		require.True(t, errors.As(err, &pe), "err should be *ParseError; got %T: %v", err, err)
		assert.Equal(t, KindMissingFlagValue, pe.Kind, "ParseError.Kind")
	})
}

// TestDeclarative_DoubleDashTail covers the third meaning of `--`: an opaque
// argv vector for a nested program (`kubectl exec <pod> -- <cmd>…`). The
// subcommand declares a variadic AfterDashDash slot for it, so the nested
// command lands in call.Positional where a constraint can see it — and stays
// on call.Tail for specs that read it there.
func TestDeclarative_DoubleDashTail(t *testing.T) {
	sc := toolkit.Subcommand{
		Path: []string{"exec"},
		Positional: []toolkit.Positional{
			{Name: "pod", Type: "string", Required: true},
			{Name: "command", Type: "stringList", AfterDashDash: true},
		},
		Effects: emptyEffects(),
	}
	p := &Declarative{}
	call, err := p.Parse(tkWith(sc), []string{"exec", "mypod", "--", "/bin/sh", "-c", "ls"})
	require.NoError(t, err, "Parse")
	assert.Equal(t, "mypod", call.Positional["pod"], "Positional[pod]")
	assert.Equal(t, []string{"/bin/sh", "-c", "ls"}, call.Positional["command"], "Positional[command]")
	assert.Equal(t, []string{"/bin/sh", "-c", "ls"}, call.Tail, "Tail")
}

// TestDeclarative_UndeclaredTailFailsClosed proves post-`--` argv with nowhere
// to bind is rejected rather than silently dropped. A subcommand that genuinely
// accepts a tail declares a slot for it (see TestDeclarative_DoubleDashTail);
// one that doesn't must not let unvalidated argv through to the binary.
func TestDeclarative_UndeclaredTailFailsClosed(t *testing.T) {
	sc := toolkit.Subcommand{
		Path:       []string{"exec"},
		Positional: []toolkit.Positional{{Name: "pod", Type: "string", Required: true}},
		Effects:    emptyEffects(),
	}
	p := &Declarative{}
	_, err := p.Parse(tkWith(sc), []string{"exec", "mypod", "--", "/bin/sh"})
	require.Error(t, err, "Parse must reject an undeclared tail")
	var pe *ParseError
	require.True(t, errors.As(err, &pe), "err should be *ParseError; got %T", err)
	assert.Equal(t, KindArgCountMismatch, pe.Kind, "ParseError.Kind")
}

// TestDeclarative_EmptyPath_BasicMatch verifies that a subcommand with path: []
// matches argv that has no leading subcommand token (e.g. cat, echo).
func TestDeclarative_EmptyPath_BasicMatch(t *testing.T) {
	sc := toolkit.Subcommand{
		Path:       []string{},
		Positional: []toolkit.Positional{{Name: "files", Type: "stringList"}},
		Effects:    emptyEffects(),
	}
	p := &Declarative{}
	call, err := p.Parse(tkWith(sc), []string{"a.txt"})
	require.NoError(t, err, "Parse")
	assert.Empty(t, call.Subcommand, "Subcommand should be empty for path: []")
	assert.Empty(t, call.SubcommandPath, "SubcommandPath should be empty")
	got, ok := call.Positional["files"].([]string)
	require.True(t, ok, "Positional[files] should be []string; got %T", call.Positional["files"])
	assert.Equal(t, []string{"a.txt"}, got, "Positional[files]")
}

// TestDeclarative_EmptyPath_LongestWins verifies that a specific subcommand
// (e.g. path: [help]) beats the empty-path fallback when argv starts with "help",
// but argv that does not match any specific subcommand falls through to path: [].
func TestDeclarative_EmptyPath_LongestWins(t *testing.T) {
	emptySC := toolkit.Subcommand{
		Path:       []string{},
		Positional: []toolkit.Positional{{Name: "file", Type: "string"}},
		Effects:    emptyEffects(),
	}
	helpSC := toolkit.Subcommand{
		Path:    []string{"help"},
		Effects: emptyEffects(),
	}
	// Order matters: put empty-path first to confirm longest-wins beats insertion order.
	tk := &toolkit.Toolkit{
		Name:            "t",
		Version:         "1",
		ToolkitRevision: "r",
		Target:          toolkit.Target{Binary: "t"},
		Parser:          toolkit.ParserConfig{Kind: "declarative"},
		Subcommands:     []toolkit.Subcommand{emptySC, helpSC},
	}
	p := &Declarative{}

	t.Run("specific path wins over empty-path fallback", func(t *testing.T) {
		call, err := p.Parse(tk, []string{"help"})
		require.NoError(t, err, "Parse(['help'])")
		assert.Equal(t, "help", call.Subcommand, "Subcommand")
	})

	t.Run("non-matching argv falls through to empty path", func(t *testing.T) {
		call, err := p.Parse(tk, []string{"other.txt"})
		require.NoError(t, err, "Parse(['other.txt'])")
		assert.Empty(t, call.Subcommand, "Subcommand should be empty (matched path: [])")
		assert.Equal(t, "other.txt", call.Positional["file"], "Positional[file]")
	})
}
