package clikit

import (
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newCmd builds a throwaway command with one string, one duration, one int,
// and one bool flag, plus the env-override PreRunE for the given bindings.
func newCmd(t *testing.T, bindings map[string]string) (*cobra.Command, *struct {
	str string
	dur time.Duration
	num int
	flg bool
}) {
	t.Helper()
	var cfg struct {
		str string
		dur time.Duration
		num int
		flg bool
	}
	cmd := &cobra.Command{Use: "test", RunE: func(*cobra.Command, []string) error { return nil }}
	fs := cmd.Flags()
	fs.StringVar(&cfg.str, "str", "default-str", "")
	fs.DurationVar(&cfg.dur, "dur", 5*time.Minute, "")
	fs.IntVar(&cfg.num, "num", 7, "")
	fs.BoolVar(&cfg.flg, "flg", false, "")
	cmd.PreRunE = EnvOverridePreRunE(bindings)
	return cmd, &cfg
}

func TestEnvOverridePreRunE(t *testing.T) {
	t.Run("unset env: declared defaults stand", func(t *testing.T) {
		cmd, cfg := newCmd(t, map[string]string{"str": "T_STR", "dur": "T_DUR", "num": "T_NUM", "flg": "T_FLG"})
		require.NoError(t, cmd.Execute())
		assert.Equal(t, "default-str", cfg.str)
		assert.Equal(t, 5*time.Minute, cfg.dur)
		assert.Equal(t, 7, cfg.num)
		assert.False(t, cfg.flg)
	})

	t.Run("valid env: typed values parsed from the environment", func(t *testing.T) {
		t.Setenv("T_STR", "from-env")
		t.Setenv("T_DUR", "90s")
		t.Setenv("T_NUM", "42")
		t.Setenv("T_FLG", "true")
		cmd, cfg := newCmd(t, map[string]string{"str": "T_STR", "dur": "T_DUR", "num": "T_NUM", "flg": "T_FLG"})
		require.NoError(t, cmd.Execute())
		assert.Equal(t, "from-env", cfg.str)
		assert.Equal(t, 90*time.Second, cfg.dur)
		assert.Equal(t, 42, cfg.num)
		assert.True(t, cfg.flg)
	})

	t.Run("empty env is treated as unset: default stands", func(t *testing.T) {
		t.Setenv("T_DUR", "")
		cmd, cfg := newCmd(t, map[string]string{"dur": "T_DUR"})
		require.NoError(t, cmd.Execute())
		assert.Equal(t, 5*time.Minute, cfg.dur)
	})

	t.Run("command-line flag wins over env", func(t *testing.T) {
		t.Setenv("T_DUR", "90s")
		cmd, cfg := newCmd(t, map[string]string{"dur": "T_DUR"})
		cmd.SetArgs([]string{"--dur=10s"})
		require.NoError(t, cmd.Execute())
		assert.Equal(t, 10*time.Second, cfg.dur, "explicit --dur must override the env var")
	})

	cases := []struct {
		name, envVar, bad, wantErrSubstr string
	}{
		{"malformed duration fails closed", "T_DUR", "xyz", `T_DUR="xyz"`},
		{"malformed int fails closed", "T_NUM", "not-a-number", `T_NUM="not-a-number"`},
		{"malformed bool fails closed", "T_FLG", "maybe", `T_FLG="maybe"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.envVar, tc.bad)
			flagName := map[string]string{"T_DUR": "dur", "T_NUM": "num", "T_FLG": "flg"}[tc.envVar]
			cmd, _ := newCmd(t, map[string]string{flagName: tc.envVar})
			err := cmd.Execute()
			require.Error(t, err, "a malformed env value must fail closed")
			assert.Contains(t, err.Error(), tc.wantErrSubstr)
		})
	}

	t.Run("binding to an unregistered flag is reported", func(t *testing.T) {
		cmd, _ := newCmd(t, map[string]string{"nonexistent": "T_X"})
		err := cmd.Execute()
		require.Error(t, err)
		assert.Contains(t, err.Error(), `flag "nonexistent"`)
	})

	t.Run("multiple malformed values are all reported", func(t *testing.T) {
		t.Setenv("T_DUR", "xyz")
		t.Setenv("T_NUM", "nan")
		cmd, _ := newCmd(t, map[string]string{"dur": "T_DUR", "num": "T_NUM"})
		err := cmd.Execute()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "T_DUR")
		assert.Contains(t, err.Error(), "T_NUM")
	})
}

func TestChainPreRunE(t *testing.T) {
	t.Run("runs in order, stops at first error", func(t *testing.T) {
		var order []string
		boom := func(string) func(*cobra.Command, []string) error {
			return func(*cobra.Command, []string) error { order = append(order, "boom"); return assert.AnError }
		}
		mark := func(s string) func(*cobra.Command, []string) error {
			return func(*cobra.Command, []string) error { order = append(order, s); return nil }
		}
		cmd := &cobra.Command{Use: "t", RunE: func(*cobra.Command, []string) error { return nil }}
		cmd.PreRunE = ChainPreRunE(mark("a"), boom("b"), mark("c"))
		require.Error(t, cmd.Execute())
		assert.Equal(t, []string{"a", "boom"}, order, "must stop before c")
	})

	t.Run("nil functions are skipped", func(t *testing.T) {
		cmd := &cobra.Command{Use: "t", RunE: func(*cobra.Command, []string) error { return nil }}
		ran := false
		cmd.PreRunE = ChainPreRunE(nil, func(*cobra.Command, []string) error { ran = true; return nil }, nil)
		require.NoError(t, cmd.Execute())
		assert.True(t, ran)
	})
}
