package desktop_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/models"
)

// providerCases enumerates every model provider the desktop setup form
// offers, with the provider-shaped facts the rest of the bring-up depends on:
// the env var the API key is passed as, the model registered as the cluster
// default when the form's model field is left blank, and the built-in catalog
// that default must appear in. TestModelProviderNames_CoversEveryProvider
// fails if a provider is added to desktop without a row here.
var providerCases = []struct {
	provider    string
	wantKeyVar  string
	wantDefault string
	catalog     map[string]models.ModelInfo
}{
	{provider: "anthropic", wantKeyVar: "ANTHROPIC_API_KEY", wantDefault: "claude-sonnet-5", catalog: models.Anthropic},
	{provider: "openai", wantKeyVar: "OPENAI_API_KEY", wantDefault: "gpt-5.5", catalog: models.OpenAI},
	{provider: "openrouter", wantKeyVar: "OPENROUTER_API_KEY", wantDefault: "openrouter/auto", catalog: models.OpenRouter},
}

// TestModelProviderNames_CoversEveryProvider ties the table above to the
// package's own list, so a fourth provider cannot be registered without the
// key-var / default-model / catalog assertions below being extended to it.
func TestModelProviderNames_CoversEveryProvider(t *testing.T) {
	want := make([]string, 0, len(providerCases))
	for _, tc := range providerCases {
		want = append(want, tc.provider)
	}
	slices.Sort(want)
	assert.Equal(t, want, desktop.ModelProviderNames(), "every desktop model provider needs a row in providerCases")
}

func TestConfig_Validate_AcceptsEveryKnownProvider(t *testing.T) {
	// Personal-use default: just the provider + key. Built-in chat needs
	// nothing; channel, ngrok and the model name are optional.
	for _, tc := range providerCases {
		t.Run(tc.provider+": accepted with only a key", func(t *testing.T) {
			c := desktop.Config{Model: desktop.ModelConfig{Provider: tc.provider, APIKey: "sk-x"}}
			assert.NoError(t, c.Validate())
		})
	}
}

func TestConfig_Validate_Errors(t *testing.T) {
	cases := map[string]desktop.Config{
		"missing api key":          {Model: desktop.ModelConfig{Provider: "anthropic"}},
		"unknown model provider":   {Model: desktop.ModelConfig{Provider: "bogus", APIKey: "x"}},
		"empty model provider":     {Model: desktop.ModelConfig{APIKey: "x"}},
		"unknown external channel": {Model: desktop.ModelConfig{Provider: "anthropic", APIKey: "x"}, Channel: &desktop.ChannelConfig{Kind: "bogus"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) { assert.Error(t, c.Validate()) })
	}
}

// TestConfig_Validate_UnknownProviderNamesTheValidSet keeps the rejection
// actionable: it must say which value was wrong AND what would have worked,
// so a user who typed "claude" into config.json is not left guessing.
func TestConfig_Validate_UnknownProviderNamesTheValidSet(t *testing.T) {
	c := desktop.Config{Model: desktop.ModelConfig{Provider: "bogus", APIKey: "x"}}
	err := c.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"bogus"`)
	for _, name := range desktop.ModelProviderNames() {
		assert.Contains(t, err.Error(), name)
	}
}

// TestConfig_Validate_RejectsAModelFromAnotherProvider covers the hand-edit
// path: "Edit configuration…" reveals config.json in Finder, so switching
// model.provider while leaving the previous provider's model.name behind is a
// real way to end up with a cluster default the new provider cannot serve.
// Validate must catch it, and must NOT catch a model that is merely unknown —
// the built-in tables trail new releases, and OpenRouter's is sparse by
// design.
func TestConfig_Validate_RejectsAModelFromAnotherProvider(t *testing.T) {
	cases := []struct {
		name        string
		provider    string
		model       string
		wantErr     bool
		wantMention string
	}{
		{name: "anthropic model left behind under openai: rejected, names the real owner", provider: "openai", model: "claude-sonnet-5", wantErr: true, wantMention: "anthropic"},
		{name: "openai model left behind under anthropic: rejected", provider: "anthropic", model: "gpt-5.5", wantErr: true, wantMention: "openai"},
		{name: "openrouter's virtual model under anthropic: rejected", provider: "anthropic", model: "openrouter/auto", wantErr: true, wantMention: "openrouter"},
		{name: "namespaced openrouter id under anthropic: rejected", provider: "anthropic", model: "anthropic/claude-3.5-sonnet", wantErr: true, wantMention: "openrouter"},
		{name: "model the provider does serve: accepted", provider: "openai", model: "gpt-5.4-mini", wantErr: false},
		{name: "model no built-in table knows: accepted, tables trail releases", provider: "openai", model: "gpt-6-not-yet-tabled", wantErr: false},
		{name: "unlisted namespaced openrouter id: accepted", provider: "openrouter", model: "acme/some-new-model", wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := desktop.Config{Model: desktop.ModelConfig{Provider: tc.provider, APIKey: "sk-x", Name: tc.model}}
			err := c.Validate()
			if !tc.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMention, "the rejection must name the provider that does serve the model")
			assert.Contains(t, err.Error(), tc.model)
		})
	}
}

// TestConfig_Validate_AcceptsEveryProvidersOwnDefault is the pairing of the
// two gates: the model Validate resolves for a blank name must itself survive
// Validate, or first-run setup would reject its own defaults.
func TestConfig_Validate_AcceptsEveryProvidersOwnDefault(t *testing.T) {
	for _, tc := range providerCases {
		t.Run(tc.provider+": its own default passes validation", func(t *testing.T) {
			c := desktop.Config{Model: desktop.ModelConfig{Provider: tc.provider, APIKey: "sk-x", Name: tc.wantDefault}}
			assert.NoError(t, c.Validate())
		})
	}
}

func TestConfig_EnvForInstall_EmitsTheSelectedProvidersKeyVar(t *testing.T) {
	for _, tc := range providerCases {
		t.Run(tc.provider+": only its own key var is set", func(t *testing.T) {
			c := desktop.Config{Model: desktop.ModelConfig{Provider: tc.provider, APIKey: "sk-x"}}
			env := c.EnvForInstall()
			assert.Equal(t, "sk-x", env[tc.wantKeyVar])
			for _, other := range providerCases {
				if other.wantKeyVar != tc.wantKeyVar {
					assert.NotContains(t, env, other.wantKeyVar, "another provider's key var must not be set")
				}
			}
			assert.NotContains(t, env, "NGROK_AUTHTOKEN", "no ngrok env unless a tunnel is configured")
		})
	}
}

func TestConfig_EffectiveModel_BlankNameFallsBackToTheProvidersDefault(t *testing.T) {
	for _, tc := range providerCases {
		t.Run(tc.provider+": blank name resolves to its default model", func(t *testing.T) {
			c := desktop.Config{Model: desktop.ModelConfig{Provider: tc.provider, APIKey: "sk-x"}}
			got, err := c.EffectiveModel()
			require.NoError(t, err)
			assert.Equal(t, tc.wantDefault, got)

			// Whitespace is not a model name: a form field holding only
			// spaces means the same thing as an empty one.
			c.Model.Name = "   "
			got, err = c.EffectiveModel()
			require.NoError(t, err)
			assert.Equal(t, tc.wantDefault, got)
		})
	}
}

// TestConfig_EffectiveModel_DefaultIsServedBySelectedProvider is the reason
// the default is per provider at all: registering a cluster default the
// selected provider cannot serve yields an install that looks configured and
// fails on its first turn. Each default must be an id in that provider's own
// built-in catalog. (models.OpenRouter is deliberately sparse — a missing
// entry there is not an error for pricing — so this asserts our default is
// one of the ids it does vouch for.)
func TestConfig_EffectiveModel_DefaultIsServedBySelectedProvider(t *testing.T) {
	for _, tc := range providerCases {
		t.Run(tc.provider+": default model is in its catalog", func(t *testing.T) {
			c := desktop.Config{Model: desktop.ModelConfig{Provider: tc.provider, APIKey: "sk-x"}}
			got, err := c.EffectiveModel()
			require.NoError(t, err)
			assert.Contains(t, tc.catalog, got, "%s is not a model the %s provider serves", got, tc.provider)
		})
	}
}

// TestConfig_EffectiveModel_ExplicitNameWins covers the setup form's optional
// model field, including OpenRouter's namespaced vendor/model ids — nothing
// in the resolution path may assume a bare name.
func TestConfig_EffectiveModel_ExplicitNameWins(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		model    string
		want     string
	}{
		{name: "anthropic: a bare id is used verbatim", provider: "anthropic", model: "claude-opus-4-8", want: "claude-opus-4-8"},
		{name: "openai: a bare id is used verbatim", provider: "openai", model: "gpt-5.4-mini", want: "gpt-5.4-mini"},
		{name: "openrouter: a namespaced id survives intact", provider: "openrouter", model: "anthropic/claude-3.5-sonnet", want: "anthropic/claude-3.5-sonnet"},
		{name: "openrouter: a namespaced id outside the built-in catalog is still accepted", provider: "openrouter", model: "acme/some-new-model", want: "acme/some-new-model"},
		{name: "anthropic: surrounding whitespace is trimmed", provider: "anthropic", model: "  claude-opus-4-8  ", want: "claude-opus-4-8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := desktop.Config{Model: desktop.ModelConfig{Provider: tc.provider, APIKey: "sk-x", Name: tc.model}}
			got, err := c.EffectiveModel()
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestConfig_EffectiveModel_UnknownProviderFailsClosed proves the resolution
// fails loudly rather than handing the caller "" — a blank model name would
// be registered as the cluster default while every layer reported success.
func TestConfig_EffectiveModel_UnknownProviderFailsClosed(t *testing.T) {
	for name, c := range map[string]desktop.Config{
		"unknown provider":                        {Model: desktop.ModelConfig{Provider: "bogus", APIKey: "x"}},
		"empty provider":                          {Model: desktop.ModelConfig{APIKey: "x"}},
		"unknown provider with an explicit model": {Model: desktop.ModelConfig{Provider: "bogus", APIKey: "x", Name: "some-model"}},
	} {
		t.Run(name+": errors, resolves to no model", func(t *testing.T) {
			got, err := c.EffectiveModel()
			require.Error(t, err)
			assert.Empty(t, got)
			assert.Contains(t, err.Error(), "unknown model provider")
		})
	}
}

// TestConfig_SaveLoad_ModelNameSurvivesAndBlankStaysBlank covers the re-run
// path: an explicit model round-trips, and a config.json written before the
// field existed (no "name" key at all) still resolves — to the provider's
// default — rather than loading as an empty model.
func TestConfig_SaveLoad_ModelNameSurvivesAndBlankStaysBlank(t *testing.T) {
	dir := t.TempDir()
	in := desktop.Config{Model: desktop.ModelConfig{Provider: "openrouter", APIKey: "sk-x", Name: "anthropic/claude-3.5-sonnet"}}
	require.NoError(t, in.Save(dir))
	got, err := desktop.Load(dir)
	require.NoError(t, err)
	assert.Equal(t, "anthropic/claude-3.5-sonnet", got.Model.Name)

	legacy := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(legacy), 0o700))
	require.NoError(t, os.WriteFile(legacy, []byte(`{"model":{"provider":"anthropic","apiKey":"sk-x"}}`), 0o600))
	old, err := desktop.Load(filepath.Dir(legacy))
	require.NoError(t, err)
	assert.Empty(t, old.Model.Name, "a config predating the field carries no model name")
	model, err := old.EffectiveModel()
	require.NoError(t, err)
	assert.Equal(t, "claude-sonnet-5", model, "a blank name resolves to the provider's default, not to nothing")
}

// TestConfig_SaveLoad_PersistsOnlyTheHash proves the on-disk round trip
// carries AdminPasswordHash (so a relaunch's desktop.Load sees it) and that
// the persisted JSON never contains a plaintext password field — there is
// no such field on Config to begin with, but this also guards against a
// future field rename accidentally reintroducing one under a different
// key.
func TestConfig_SaveLoad_PersistsOnlyTheHash(t *testing.T) {
	dir := t.TempDir()
	c := desktop.Config{
		Model:             desktop.ModelConfig{Provider: "anthropic", APIKey: "sk-x"},
		AdminPasswordHash: "$2a$12$abcdefghijklmnopqrstuv",
	}
	require.NoError(t, c.Save(dir))

	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), "adminPasswordHash")
	assert.NotContains(t, string(raw), "password", "config.json must never carry a plaintext password field")

	got, err := desktop.Load(dir)
	require.NoError(t, err)
	assert.Equal(t, c.AdminPasswordHash, got.AdminPasswordHash)
}

func TestConfig_HealthNotificationsEnabled_DefaultsOnWhenNil(t *testing.T) {
	c := desktop.Config{Model: desktop.ModelConfig{Provider: "anthropic", APIKey: "sk-x"}}
	assert.True(t, c.HealthNotificationsEnabled(), "nil pointer means enabled by default")

	off := false
	c.HealthNotifications = &off
	assert.False(t, c.HealthNotificationsEnabled(), "explicit false disables")

	on := true
	c.HealthNotifications = &on
	assert.True(t, c.HealthNotificationsEnabled(), "explicit true enables")
}

func TestConfig_SaveLoad_PreservesHealthNotifications(t *testing.T) {
	dir := t.TempDir()
	off := false
	in := desktop.Config{
		Model:               desktop.ModelConfig{Provider: "anthropic", APIKey: "sk-x"},
		HealthNotifications: &off,
	}
	require.NoError(t, in.Save(dir))

	got, err := desktop.Load(dir)
	require.NoError(t, err)
	require.NotNil(t, got.HealthNotifications, "pointer must survive the round trip")
	assert.False(t, *got.HealthNotifications)
	assert.False(t, got.HealthNotificationsEnabled())
}
