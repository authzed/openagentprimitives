package parser

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// FuzzParseDoesNotPanic seeds argv patterns from the golden corpus and fuzzes combinations.
func FuzzParseDoesNotPanic(f *testing.F) {
	tk := &toolkit.Toolkit{
		Name: "t", Version: "1", ToolkitRevision: "r",
		Target: toolkit.Target{Binary: "t"},
		Parser: toolkit.ParserConfig{Kind: "declarative"},
		Subcommands: []toolkit.Subcommand{
			{
				Path:       []string{"pr", "view"},
				Positional: []toolkit.Positional{{Name: "id", Type: "string", Required: true}},
				Flags: []toolkit.Flag{
					{Long: "repo", Short: "R", Type: "string"},
					{Long: "json", Type: "string"},
					{Long: "web", Short: "w", Type: "bool"},
				},
				Effects: toolkit.Effects{
					Network:    toolkit.NetworkEffect{Destinations: []string{}},
					Filesystem: toolkit.FilesystemEffect{Paths: []string{}},
					Creds:      toolkit.CredsEffect{Required: []string{}, Writes: []string{}},
				},
			},
		},
	}

	seeds := [][]string{
		{"pr", "view", "1"},
		{"pr", "view", "--repo=x/y", "1"},
		{"pr", "view", "1", "-R", "x/y"},
		{"pr", "view", "1", "--", "extra"},
		{"nope"},
		{"pr"},
		{"pr", "view", "--repo"}, // missing value
	}
	for _, s := range seeds {
		f.Add(joinForFuzz(s))
	}
	p := &Declarative{}
	f.Fuzz(func(t *testing.T, joined string) {
		require.NotPanics(t, func() {
			argv := splitForFuzz(joined)
			_, _ = p.Parse(tk, argv)
		}, "parser panicked on argv %q", joined)
	})
}

// splitForFuzz/joinForFuzz use a byte the fuzzer won't normally produce inside tokens as separator.
func joinForFuzz(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += "\x01"
		}
		out += x
	}
	return out
}
func splitForFuzz(s string) []string {
	if s == "" {
		return nil
	}
	out := []string{""}
	for i := 0; i < len(s); i++ {
		if s[i] == '\x01' {
			out = append(out, "")
		} else {
			out[len(out)-1] += string(s[i])
		}
	}
	return out
}
