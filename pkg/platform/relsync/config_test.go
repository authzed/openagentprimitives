package relsync_test

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// noAskKind is a minimal relsync.Configurable fixture for a kind that has
// nothing to ask beyond credential and endpoint — e.g. Slack, which syncs
// the whole workspace its token reaches.
type noAskKind struct{}

func (noAskKind) ConfigScreens(prior relsync.ExistingConfig) []relsync.ConfigScreen { return nil }

func (noAskKind) BuildConfig(answers map[string]string) (json.RawMessage, error) {
	return nil, nil
}

func (noAskKind) NeedsEndpoint() bool { return false }

// listAskKind is a minimal relsync.Configurable fixture for a kind that
// asks one comma-separated-list question ("orgs") and reflects it straight
// into spec.config.
type listAskKind struct{}

func (listAskKind) ConfigScreens(prior relsync.ExistingConfig) []relsync.ConfigScreen {
	def := ""
	if len(prior.Config) > 0 {
		var parsed struct {
			Orgs []string `json:"orgs"`
		}
		if err := json.Unmarshal(prior.Config, &parsed); err == nil {
			def = strings.Join(parsed.Orgs, ",")
		}
	}
	return []relsync.ConfigScreen{
		{
			Key:     "orgs",
			Prompt:  "Organizations to sync",
			Help:    "Comma-separated list of org logins.",
			Default: def,
		},
	}
}

// BuildConfig assembles every key present in answers into a JSON array,
// sorted by key first. The array shape (not an object) is deliberate: a Go
// map marshaled directly would come out key-sorted for free, which would
// make this fixture incapable of exposing an unsorted-range bug — the same
// blind spot TestConfigurable_BuildConfigIsDeterministic exists to catch.
// An array preserves whatever order its elements were appended in, so
// forgetting the sort.Strings below genuinely changes the marshaled bytes
// between two calls on identical input.
func (listAskKind) BuildConfig(answers map[string]string) (json.RawMessage, error) {
	keys := make([]string, 0, len(answers))
	for k := range answers {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	type field struct {
		Key    string   `json:"key"`
		Values []string `json:"values"`
	}
	fields := make([]field, 0, len(keys))
	for _, k := range keys {
		var values []string
		if v := answers[k]; v != "" {
			values = strings.Split(v, ",")
		}
		fields = append(fields, field{Key: k, Values: values})
	}
	return json.Marshal(fields)
}

func (listAskKind) NeedsEndpoint() bool { return true }

// A kind that needs no configuration is a first-class case, not a gap: Slack
// syncs the whole workspace its token reaches, so it has nothing to ask.
func TestConfigurable_AKindMayAskNothing(t *testing.T) {
	var k relsync.Configurable = noAskKind{}
	assert.Empty(t, k.ConfigScreens(relsync.ExistingConfig{}))
	cfg, err := k.BuildConfig(nil)
	require.NoError(t, err)
	assert.Nil(t, cfg, "no questions means no spec.config, not an empty object")
}

// A re-run must be able to show what is already set. Prior values reach the
// screens as defaults; nothing else in the seam carries them.
func TestConfigurable_ScreensArePrefilledFromPrior(t *testing.T) {
	k := listAskKind{}
	screens := k.ConfigScreens(relsync.ExistingConfig{Config: json.RawMessage(`{"orgs":["acme","widgets"]}`)})
	require.Len(t, screens, 1)
	assert.Equal(t, "acme,widgets", screens[0].Default,
		"a re-run shows the configured value so enter-through keeps it")
}

// The applied spec must be a pure function of its inputs, so a byte-identical
// re-apply is a genuine SSA no-op rather than a field-ownership churn.
//
// answers deliberately carries two keys, not one: a single-key map can
// never expose an unsorted-range bug, because ranging it always visits its
// one entry — order can't vary regardless of whether BuildConfig sorts.
// With two-plus keys, Go's map iteration order (randomized independently
// per range, not fixed per process — verified empirically before writing
// this fixture, see the task report) means a version of BuildConfig that
// forgot to sort produces genuinely different byte output across these two
// calls on a nontrivial fraction of runs. This test only stays green
// because listAskKind.BuildConfig sorts before marshaling.
func TestConfigurable_BuildConfigIsDeterministic(t *testing.T) {
	k := listAskKind{}
	answers := map[string]string{"orgs": "acme,widgets", "teams": "platform,core"}

	a, err := k.BuildConfig(answers)
	require.NoError(t, err)
	b, err := k.BuildConfig(answers)
	require.NoError(t, err)
	assert.Equal(t, string(a), string(b))
}
