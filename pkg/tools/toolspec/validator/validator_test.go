package validator

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/parser"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/parser/builtin"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

func simpleToolkit() *toolkit.Toolkit {
	return &toolkit.Toolkit{
		Name: "t", Version: "1", ToolkitRevision: "r1",
		Target:      toolkit.Target{Binary: "t", VersionRange: ">=1.0.0 <2.0.0"},
		Parser:      toolkit.ParserConfig{Kind: "declarative"},
		Env:         toolkit.Env{Allowed: []toolkit.EnvVar{}},
		Subcommands: []toolkit.Subcommand{{Path: []string{"say"}, Effects: emptyEff()}},
	}
}

func emptyEff() toolkit.Effects {
	return toolkit.Effects{
		Reads: []string{}, Writes: []string{},
		Network:    toolkit.NetworkEffect{Destinations: []string{}},
		Filesystem: toolkit.FilesystemEffect{Paths: []string{}},
		Creds:      toolkit.CredsEffect{Required: []string{}, Writes: []string{}},
	}
}

func simpleSpec() *spec.Spec {
	return &spec.Spec{
		Name: "s", Version: "1",
		Toolkit:          spec.ToolkitRef{Name: "t", Revision: "r1"},
		AllowSubcommands: []string{"say"},
	}
}

func mutatingToolkit() *toolkit.Toolkit {
	tk := simpleToolkit()
	tk.Subcommands = append(tk.Subcommands, toolkit.Subcommand{
		Path: []string{"merge"},
		Effects: toolkit.Effects{
			Destructive: true,
			Reads:       []string{"network"},
			Writes:      []string{"network"},
			Network:     toolkit.NetworkEffect{Destinations: []string{"api.github.com", "evil.com"}},
			Filesystem:  toolkit.FilesystemEffect{Paths: []string{}},
			Creds:       toolkit.CredsEffect{Required: []string{}, Writes: []string{}},
		},
	})
	return tk
}

// TestCheck_DenyCases groups every case that should yield Allow=false with a
// specific FailedOn.Path. Each case sets up its own toolkit + spec variation.
func TestCheck_DenyCases(t *testing.T) {
	cases := []struct {
		name         string
		build        func() (*toolkit.Toolkit, *spec.Spec)
		inv          Invocation
		wantFailedOn string
	}{
		{
			name: "unknown subcommand: FailedOn=parse",
			build: func() (*toolkit.Toolkit, *spec.Spec) {
				return simpleToolkit(), simpleSpec()
			},
			inv:          Invocation{Command: "t", Argv: []string{"bogus"}},
			wantFailedOn: "parse",
		},
		{
			name: "revision mismatch: FailedOn=revision",
			build: func() (*toolkit.Toolkit, *spec.Spec) {
				tk := simpleToolkit()
				tk.ToolkitRevision = "r2"
				return tk, simpleSpec()
			},
			inv:          Invocation{Command: "t", Argv: []string{"say"}},
			wantFailedOn: "revision",
		},
		{
			name: "binary version required but missing: FailedOn=binaryVersion",
			build: func() (*toolkit.Toolkit, *spec.Spec) {
				sp := simpleSpec()
				sp.Require.VerifiedBinaryVersion = true
				return simpleToolkit(), sp
			},
			inv:          Invocation{Command: "t", Argv: []string{"say"}},
			wantFailedOn: "binaryVersion",
		},
		{
			name: "binary version out of range: FailedOn=binaryVersion",
			build: func() (*toolkit.Toolkit, *spec.Spec) {
				return simpleToolkit(), simpleSpec()
			},
			inv:          Invocation{Command: "t", Argv: []string{"say"}, BinaryVersion: "3.0.0"},
			wantFailedOn: "binaryVersion",
		},
		{
			name: "subcommand not in allow set: FailedOn=allowSubcommands",
			build: func() (*toolkit.Toolkit, *spec.Spec) {
				tk := simpleToolkit()
				tk.Subcommands = append(tk.Subcommands, toolkit.Subcommand{Path: []string{"shout"}, Effects: emptyEff()})
				return tk, simpleSpec() // only "say" allowed
			},
			inv:          Invocation{Command: "t", Argv: []string{"shout"}, BinaryVersion: "1.5.0"},
			wantFailedOn: "allowSubcommands",
		},
		{
			name: "destructive effect denied: FailedOn=deny.effects.destructive",
			build: func() (*toolkit.Toolkit, *spec.Spec) {
				sp := simpleSpec()
				sp.AllowSubcommands = []string{"merge"}
				sp.Deny.Effects.Destructive = true
				return mutatingToolkit(), sp
			},
			inv:          Invocation{Command: "t", Argv: []string{"merge"}, BinaryVersion: "1.5.0"},
			wantFailedOn: "deny.effects.destructive",
		},
		{
			name: "writes intersect denied set: FailedOn=deny.effects.writes",
			build: func() (*toolkit.Toolkit, *spec.Spec) {
				sp := simpleSpec()
				sp.AllowSubcommands = []string{"merge"}
				sp.Deny.Effects.Writes = []string{"network"}
				return mutatingToolkit(), sp
			},
			inv:          Invocation{Command: "t", Argv: []string{"merge"}, BinaryVersion: "1.5.0"},
			wantFailedOn: "deny.effects.writes",
		},
		{
			name: "network destination not in allowed subset: FailedOn=allow.network.destinations",
			build: func() (*toolkit.Toolkit, *spec.Spec) {
				sp := simpleSpec()
				sp.AllowSubcommands = []string{"merge"}
				sp.Allow.Network.Set = true
				sp.Allow.Network.Destinations = []string{"api.github.com"}
				return mutatingToolkit(), sp
			},
			inv:          Invocation{Command: "t", Argv: []string{"merge"}, BinaryVersion: "1.5.0"},
			wantFailedOn: "allow.network.destinations",
		},
		{
			name: "constraint fails: FailedOn=constraints[0]",
			build: func() (*toolkit.Toolkit, *spec.Spec) {
				sp := simpleSpec()
				sp.Constraints = []spec.Constraint{{
					CEL: `call.subcommand == "not-say"`, Message: "wrong subcommand",
				}}
				return simpleToolkit(), sp
			},
			inv:          Invocation{Command: "t", Argv: []string{"say"}, BinaryVersion: "1.5.0"},
			wantFailedOn: "constraints[0]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tk, sp := tc.build()
			d, err := Check(tk, sp, tc.inv)
			require.NoError(t, err, "Check")
			assert.False(t, d.Allow, "expected deny")
			require.NotNil(t, d.FailedOn, "FailedOn must be set on deny")
			assert.Equal(t, tc.wantFailedOn, d.FailedOn.Path, "FailedOn.Path")
		})
	}
}

// TestCheck_DenyWithoutFailedOnPath covers deny cases where we don't assert a
// specific FailedOn.Path (multiple paths could plausibly fire).
func TestCheck_DenyWithoutFailedOnPath(t *testing.T) {
	cases := []struct {
		name  string
		build func() (*toolkit.Toolkit, *spec.Spec)
		inv   Invocation
	}{
		{
			name: "empty allow.network set denies any destination",
			build: func() (*toolkit.Toolkit, *spec.Spec) {
				sp := simpleSpec()
				sp.AllowSubcommands = []string{"merge"}
				sp.Allow.Network.Set = true
				sp.Allow.Network.Destinations = []string{}
				return mutatingToolkit(), sp
			},
			inv: Invocation{Command: "t", Argv: []string{"merge"}, BinaryVersion: "1.5.0"},
		},
		{
			name: "exception with when=false: deny stands",
			build: func() (*toolkit.Toolkit, *spec.Spec) {
				sp := simpleSpec()
				sp.AllowSubcommands = []string{"merge"}
				sp.Deny.Effects.Destructive = true
				sp.Exceptions = []spec.Exception{{
					Overrides: []string{"deny.effects.destructive"},
					When:      `call.subcommand == "nope"`,
				}}
				return mutatingToolkit(), sp
			},
			inv: Invocation{Command: "t", Argv: []string{"merge"}, BinaryVersion: "1.5.0"},
		},
		{
			name: "exception with wrong override path: does not lift firing rule",
			build: func() (*toolkit.Toolkit, *spec.Spec) {
				sp := simpleSpec()
				sp.AllowSubcommands = []string{"merge"}
				sp.Deny.Effects.Destructive = true
				sp.Exceptions = []spec.Exception{{
					Overrides: []string{"deny.effects.writes"}, // wrong path
					When:      `true`,
				}}
				return mutatingToolkit(), sp
			},
			inv: Invocation{Command: "t", Argv: []string{"merge"}, BinaryVersion: "1.5.0"},
		},
		{
			name: "constraint sees effects: evil.com fails CEL",
			build: func() (*toolkit.Toolkit, *spec.Spec) {
				sp := simpleSpec()
				sp.AllowSubcommands = []string{"merge"}
				sp.Constraints = []spec.Constraint{{
					CEL: `call.effects.network.destinations.all(h, h in ["api.github.com"])`,
				}}
				return mutatingToolkit(), sp
			},
			inv: Invocation{Command: "t", Argv: []string{"merge"}, BinaryVersion: "1.5.0"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tk, sp := tc.build()
			d, err := Check(tk, sp, tc.inv)
			require.NoError(t, err, "Check")
			assert.False(t, d.Allow, "expected deny")
		})
	}
}

func TestCheck_AllowSubcommands_Allow(t *testing.T) {
	d, err := Check(simpleToolkit(), simpleSpec(), Invocation{
		Command: "t", Argv: []string{"say"}, BinaryVersion: "1.5.0",
	})
	require.NoError(t, err, "Check")
	assert.True(t, d.Allow, "expected allow; decision=%+v", d)
}

func TestCheck_Constraint_Passes(t *testing.T) {
	sp := simpleSpec()
	sp.Constraints = []spec.Constraint{{
		CEL: `call.subcommand == "say"`,
	}}
	d, err := Check(simpleToolkit(), sp, Invocation{
		Command: "t", Argv: []string{"say"}, BinaryVersion: "1.5.0",
	})
	require.NoError(t, err, "Check")
	assert.True(t, d.Allow, "expected allow; decision=%+v", d)
}

func TestCheck_BinaryVersionUnverifiedWarning_AllowsWithWarning(t *testing.T) {
	// require=false, no binary version provided → proceed with warning.
	d, err := Check(simpleToolkit(), simpleSpec(), Invocation{Command: "t", Argv: []string{"say"}})
	require.NoError(t, err, "Check")
	assert.True(t, d.Allow, "expected allow; decision=%+v", d)
	found := false
	for _, w := range d.Warnings {
		if w.Kind == "VersionUnverified" {
			found = true
			break
		}
	}
	assert.True(t, found, "expected VersionUnverified warning; got %v", d.Warnings)
}

func TestCheck_ExceptionLiftsStructuredDeny(t *testing.T) {
	tk := mutatingToolkit()
	sp := simpleSpec()
	sp.AllowSubcommands = []string{"merge"}
	sp.Deny.Effects.Destructive = true
	sp.Exceptions = []spec.Exception{{
		Overrides: []string{"deny.effects.destructive"},
		When:      `call.subcommand == "merge"`,
		Message:   "dev override",
	}}
	d, err := Check(tk, sp, Invocation{Command: "t", Argv: []string{"merge"}, BinaryVersion: "1.5.0"})
	require.NoError(t, err, "Check")
	assert.True(t, d.Allow, "expected allow via override; decision=%+v", d)
	found := false
	for _, tr := range d.Trace {
		if tr.Status == StatusOverrideApplied {
			found = true
			break
		}
	}
	assert.True(t, found, "expected override-applied trace entry; trace=%+v", d.Trace)
}

func TestCheck_RedactsSensitiveEnv(t *testing.T) {
	tk := simpleToolkit()
	tk.Env.Allowed = []toolkit.EnvVar{{Name: "TOKEN", Sensitive: true, Description: "Auth token"}}
	sp := simpleSpec()
	sp.Constraints = []spec.Constraint{{
		CEL:     `call.env["TOKEN"] == "expected"`,
		Message: `token mismatch`,
	}}
	d, err := Check(tk, sp, Invocation{
		Command: "t", Argv: []string{"say"}, BinaryVersion: "1.5.0",
		Env: map[string]string{"TOKEN": "super-secret-value"},
	})
	require.NoError(t, err, "Check")
	// The Decision may allow or deny depending on match; we only care about redaction.
	require.NotEmpty(t, d.Redactions, "expected at least one redaction; decision=%+v", d)
	for _, entry := range d.Redactions {
		assert.Equal(t, "TOKEN", entry.Name, "redaction Name")
		assert.Equal(t, "env", entry.Kind, "redaction Kind")
	}
}

// strayPathParser is a builtin parser whose Call sets a Subcommand that IS in
// the allow set (so earlier phases pass) but a SubcommandPath that is NOT
// present in tk.Subcommands — exactly the toolkit/parser disagreement that used
// to nil-deref sc.Effects in Check before the guard was added.
type strayPathParser struct{}

func (strayPathParser) Parse(_ *toolkit.Toolkit, argv []string) (*parser.Call, error) {
	return &parser.Call{
		Subcommand:     "say",
		SubcommandPath: []string{"nonexistent"},
		Flags:          map[string]any{},
		Positional:     map[string]any{},
		Argv:           append([]string(nil), argv...),
	}, nil
}

func TestCheck_StraySubcommandPath_ReturnsErrorNotPanic(t *testing.T) {
	builtin.Register("test_stray_path", strayPathParser{})

	tk := simpleToolkit()
	tk.Parser = toolkit.ParserConfig{Kind: "builtin", Name: "test_stray_path"}
	sp := simpleSpec() // allows "say"

	// Must return an internal error (authoring bug), not panic, and not allow.
	d, err := Check(tk, sp, Invocation{Command: "t", Argv: []string{"say"}, BinaryVersion: "1.5.0"})
	require.Error(t, err, "stray SubcommandPath must surface an internal error")
	assert.Contains(t, err.Error(), "no toolkit subcommand for path", "error should name the missing path")
	assert.Nil(t, d, "no Decision is returned on an internal error")
}

func TestCheck_RedactsSensitiveFlagValue(t *testing.T) {
	tk := simpleToolkit()
	tk.Subcommands[0].Flags = []toolkit.Flag{{Long: "token", Type: "string", Sensitive: true, Description: "token"}}
	sp := simpleSpec()
	sp.Constraints = []spec.Constraint{{
		CEL: `call.flag("token") == "expected"`,
	}}
	d, err := Check(tk, sp, Invocation{
		Command: "t", Argv: []string{"say", "--token", "abc-123"}, BinaryVersion: "1.5.0",
	})
	require.NoError(t, err, "Check")
	assert.NotEmpty(t, d.Redactions, "expected at least one redaction; decision=%+v", d)
}
