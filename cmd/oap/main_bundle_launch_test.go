package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInjectDesktopSubcommand(t *testing.T) {
	cases := []struct {
		name string
		args []string
		exe  string
		want []string
	}{
		{
			name: "bare invocation from inside a .app bundle injects desktop",
			args: []string{"oap"},
			exe:  "/Applications/oap.app/Contents/MacOS/oap",
			want: []string{"oap", "desktop"},
		},
		{
			name: "bare invocation outside a .app bundle is left untouched",
			args: []string{"oap"},
			exe:  "/usr/local/bin/oap",
			want: []string{"oap"},
		},
		{
			name: "explicit subcommand inside a .app bundle is never touched",
			args: []string{"oap", "install"},
			exe:  "/Applications/oap.app/Contents/MacOS/oap",
			want: []string{"oap", "install"},
		},
		{
			name: "flags-only invocation inside a .app bundle is never touched",
			args: []string{"oap", "--help"},
			exe:  "/Applications/oap.app/Contents/MacOS/oap",
			want: []string{"oap", "--help"},
		},
		{
			name: "a bare go-build path that happens to contain 'app' but not the bundle marker is untouched",
			args: []string{"oap"},
			exe:  "/Users/dev/agentprimitives/bin/oap",
			want: []string{"oap"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := injectDesktopSubcommand(tc.args, tc.exe)
			assert.Equal(t, tc.want, got)
		})
	}
}
