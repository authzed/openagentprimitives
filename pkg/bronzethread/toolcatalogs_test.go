package bronzethread_test

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
)

// collectCatalogReports returns a check whose loud-failure reports land in the
// returned slice instead of a *testing.T, so the report text itself is
// assertable.
func collectCatalogReports(t *testing.T, cats []bt.ToolCatalog, extras []string) (*bt.ToolCatalogCheck, *[]string) {
	t.Helper()
	var (
		mu   sync.Mutex
		msgs []string
	)
	c := bt.NewToolCatalogCheck(cats, extras, func(msg string) {
		mu.Lock()
		defer mu.Unlock()
		msgs = append(msgs, msg)
	})
	return c, &msgs
}

// twoTurnCatalog is the shape this whole mechanism exists for: a small set at
// the start of a run and a larger one from a later turn, the way a session
// looks when a gate opens mid-run.
func twoTurnCatalog() []bt.ToolCatalog {
	return []bt.ToolCatalog{
		{FromTurnIndex: 1, Tools: []string{"respond_to_user", "update_status"}},
		{FromTurnIndex: 5, Tools: []string{"box_read", "box_write", "respond_to_user", "update_status"}},
	}
}

func TestToolCatalogCheck_OffersTheRecordedSetPerTurn(t *testing.T) {
	c, msgs := collectCatalogReports(t, twoTurnCatalog(), []string{"box_read", "box_write"})
	filter := c.Filter()
	require.NotNil(t, filter, "a bundle that pins catalogs must produce a filter")

	all := []string{"box_read", "box_write", "respond_to_user", "update_status"}

	t.Run("before the second change: the later tools are withheld", func(t *testing.T) {
		assert.Equal(t, []string{"respond_to_user", "update_status"}, filter(2, all))
	})
	t.Run("from the second change: every recorded tool is offered", func(t *testing.T) {
		assert.Equal(t, all, filter(5, all))
	})
	t.Run("declared extras are withheld in SILENCE", func(t *testing.T) {
		assert.Empty(t, *msgs, "an extra the bundle declared is not a finding")
	})
}

func TestToolCatalogCheck_TurnBeforeTheFirstRecordMakesNoClaim(t *testing.T) {
	c, msgs := collectCatalogReports(t, twoTurnCatalog(), nil)
	all := []string{"box_read", "respond_to_user"}

	// No record covers turn 0, so the set is UNKNOWN — not empty. Withholding
	// here would offer nothing at all on a turn the capture says nothing about.
	assert.Equal(t, all, c.Filter()(0, all))
	assert.Empty(t, *msgs)
}

func TestToolCatalogCheck_AMissingRecordedToolIsReported(t *testing.T) {
	c, msgs := collectCatalogReports(t, twoTurnCatalog(), nil)

	got := c.Filter()(5, []string{"box_read", "respond_to_user", "update_status"})

	require.Len(t, *msgs, 1, "one missing tool, one report")
	assert.Contains(t, (*msgs)[0], `"box_write"`, "the report must name the tool that vanished")
	assert.Contains(t, (*msgs)[0], "turn 5")
	assert.NotContains(t, got, "box_write", "a tool that is not there cannot be offered")
}

func TestToolCatalogCheck_AnUndeclaredExtraIsReportedAndWithheld(t *testing.T) {
	c, msgs := collectCatalogReports(t, twoTurnCatalog(), []string{"box_read", "box_write"})

	// box_read/box_write are declared. secret_tool is not: a gate that used to
	// withhold it has stopped, which is the regression this exists to catch.
	got := c.Filter()(2, []string{"respond_to_user", "secret_tool", "update_status"})

	require.Len(t, *msgs, 1)
	assert.Contains(t, (*msgs)[0], `"secret_tool"`)
	assert.NotContains(t, (*msgs)[0], `"box_read"`, "a declared extra must not appear in the finding")
	assert.Equal(t, []string{"respond_to_user", "update_status"}, got,
		"an undeclared extra is withheld too: the model must see the set the captured run saw")
}

func TestToolCatalogCheck_ARecurringDifferenceIsReportedOnce(t *testing.T) {
	c, msgs := collectCatalogReports(t, twoTurnCatalog(), nil)
	filter := c.Filter()

	// The same difference on every turn the set is in force. Reporting each
	// would bury the one finding under repetitions of itself.
	for turn := 1; turn < 5; turn++ {
		filter(turn, []string{"respond_to_user", "surprise", "update_status"})
	}

	require.Len(t, *msgs, 1, "one distinct difference, one report")
	assert.Contains(t, (*msgs)[0], "turn 1",
		"the report that gets through names the FIRST turn the difference appeared at")
}

func TestToolCatalogCheck_TwoDIFFERENTDifferencesAreBothReported(t *testing.T) {
	c, msgs := collectCatalogReports(t, twoTurnCatalog(), nil)
	filter := c.Filter()

	// Dedup is per-difference, not per-check: suppressing the second would be
	// the silence the mechanism exists to remove.
	filter(2, []string{"respond_to_user", "surprise", "update_status"})
	filter(2, []string{"another_surprise", "respond_to_user", "update_status"})

	assert.Len(t, *msgs, 2, "two distinct extras, two reports")
}

func TestToolCatalogCheck_Consulted(t *testing.T) {
	c, _ := collectCatalogReports(t, twoTurnCatalog(), nil)

	// A correct bundle produces no findings, so "never asked" and "asked and
	// found nothing" are otherwise identical, and a check nobody wired in would
	// stay green forever.
	assert.False(t, c.Consulted(), "nothing has asked it anything yet")
	c.Filter()(1, []string{"respond_to_user", "update_status"})
	assert.True(t, c.Consulted())
}

func TestToolCatalogCheck_UnpinnedBundleGetsNoFilterAndHoldsNothing(t *testing.T) {
	c, _ := collectCatalogReports(t, nil, nil)

	// The compatibility path, and the honest one: a bundle that records no
	// catalog is making no claim, so the run offers what it composed.
	assert.Nil(t, c.Filter(), "no catalogs pinned means no filter wired")
	assert.Empty(t, c.HeldFromAssembly())
}

func TestToolCatalogCheck_HeldFromAssembly(t *testing.T) {
	cases := []struct {
		name   string
		cats   []bt.ToolCatalog
		extras []string
		want   []string
	}{
		{
			name:   "an extra absent from the FIRST catalog is held out of assembly",
			cats:   twoTurnCatalog(),
			extras: []string{"box_read", "box_write"},
			want:   []string{"box_read", "box_write"},
		},
		{
			name: "an extra the first catalog already had is NOT held",
			cats: []bt.ToolCatalog{
				{FromTurnIndex: 1, Tools: []string{"box_read", "respond_to_user"}},
			},
			extras: []string{"box_read"},
			want:   nil,
		},
		{
			name:   "no declared extras: nothing is held",
			cats:   twoTurnCatalog(),
			extras: nil,
			want:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := bt.NewToolCatalogCheck(tc.cats, tc.extras, func(string) {})
			assert.Equal(t, tc.want, c.HeldFromAssembly())
		})
	}
}

func TestNewToolCatalogCheck_NilReporterPanicsRatherThanDroppingTheFinding(t *testing.T) {
	c := bt.NewToolCatalogCheck(twoTurnCatalog(), nil, nil)
	assert.Panics(t, func() {
		c.Filter()(5, []string{"respond_to_user", "update_status"})
	}, "a mismatch nothing reports is the silent ride-through this replaces")
}

func TestToolCatalogCheck_ConcurrentTurnsDoNotRace(t *testing.T) {
	c := bt.NewToolCatalogCheck(twoTurnCatalog(), nil, func(string) {})
	filter := c.Filter()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			filter(i, []string{"box_read", "respond_to_user", "update_status", "zzz_extra"})
		}()
	}
	wg.Wait()
}

func TestBundle_ValidateToolCatalogs(t *testing.T) {
	// One valid bundle, doctored by exactly one field per case.
	base := func() bt.Bundle {
		return bt.Bundle{
			Name:         "demo",
			AgentDir:     "testdata/demo",
			AgentClass:   "demo-agent",
			UserTurns:    []bt.UserTurn{{Text: "hi"}},
			LLM:          []bt.LLMStep{{}},
			ToolCatalogs: twoTurnCatalog(),
		}
	}
	cases := []struct {
		name    string
		doctor  func(b *bt.Bundle)
		wantErr string
	}{
		{name: "a well-formed catalog list validates", doctor: func(*bt.Bundle) {}},
		{
			name:    "negative turn index",
			doctor:  func(b *bt.Bundle) { b.ToolCatalogs[0].FromTurnIndex = -1 },
			wantErr: "negative fromTurnIndex",
		},
		{
			name:    "turn indices out of order",
			doctor:  func(b *bt.Bundle) { b.ToolCatalogs[1].FromTurnIndex = 0 },
			wantErr: "strictly increasing turn order",
		},
		{
			name:    "the same turn index twice",
			doctor:  func(b *bt.Bundle) { b.ToolCatalogs[1].FromTurnIndex = 1 },
			wantErr: "strictly increasing turn order",
		},
		{
			name:    "an empty tool set",
			doctor:  func(b *bt.Bundle) { b.ToolCatalogs[1].Tools = nil },
			wantErr: "lists no tools",
		},
		{
			name:    "an unsorted tool set",
			doctor:  func(b *bt.Bundle) { b.ToolCatalogs[0].Tools = []string{"update_status", "respond_to_user"} },
			wantErr: "is not sorted",
		},
		{
			name:    "the same tool twice",
			doctor:  func(b *bt.Bundle) { b.ToolCatalogs[0].Tools = []string{"a", "a"} },
			wantErr: `lists "a" twice`,
		},
		{
			name: "expectedExtraTools with no catalogs to be excused from",
			doctor: func(b *bt.Bundle) {
				b.ToolCatalogs = nil
				b.ExpectedExtraTools = []string{"box_read"}
			},
			wantErr: "pins no toolCatalogs",
		},
		{
			name:    "the same expected extra twice",
			doctor:  func(b *bt.Bundle) { b.ExpectedExtraTools = []string{"box_read", "box_read"} },
			wantErr: `names "box_read" twice`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := base()
			tc.doctor(&b)
			err := b.Validate()
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
