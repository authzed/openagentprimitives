package desktop

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/llm/models"
)

// modelProvider is what the desktop first-run flow needs to know about one
// LLM provider (the llm.Provider seam): the env var its API key is passed as,
// and the model registered as the cluster default when the setup form's
// optional model field is left blank.
//
// defaultModel is a real id in that provider's pkg/agent/llm/models table
// (asserted by TestConfig_EffectiveModel_DefaultIsServedBySelectedProvider) —
// registering a cluster default the chosen provider cannot serve would leave
// an install that looks configured and dies on its first turn. OpenRouter gets
// its "auto" virtual model, which routes per request; OpenRouter ids are
// namespaced vendor/model, so nothing handling a model name here may assume a
// bare one.
type modelProvider struct {
	keyEnvVar    string
	defaultModel string
}

// knownModelProviders maps a provider to those facts. Its keys are the LLM
// providers registered in pkg/agent/llm/providers — the runner resolves a
// session's Model.Provider through that registry at turn time, and
// ModelCatalogEntry.Provider's CRD enum accepts exactly these names.
//
// knownExternalChannelKinds are OPTIONAL external channels (channelkinds
// registry) — Slack is deliberately absent for personal use. The built-in
// local chat is NOT listed here: it's the default when Channel is nil (no
// config). Both maps are the fail-closed gate for Config.Validate() — an
// entry is added only once a real backend exists for it (e.g. a future
// "whatsapp" channel kind, or a fourth registered llm.Provider); until then,
// Validate() must reject it rather than silently accept config-plumbing for
// something that can't actually be configured.
var knownModelProviders = map[string]modelProvider{
	"anthropic":  {keyEnvVar: "ANTHROPIC_API_KEY", defaultModel: "claude-sonnet-5"},
	"openai":     {keyEnvVar: "OPENAI_API_KEY", defaultModel: "gpt-5.5"},
	"openrouter": {keyEnvVar: "OPENROUTER_API_KEY", defaultModel: "openrouter/auto"},
}
var knownExternalChannelKinds = map[string]struct{}{}

// ModelProviderNames returns the accepted model providers, sorted. It is what
// the setup form's provider <select> must offer (asserted by setupui's
// TestServer_IndexOffersEveryKnownModelProvider) and what a rejection names,
// so the user is told the valid set rather than only that theirs was wrong.
func ModelProviderNames() []string {
	names := slices.Collect(maps.Keys(knownModelProviders))
	slices.Sort(names)
	return names
}

// lookupProvider is the single fail-closed provider gate: every path needing
// a provider's facts goes through it, so an unknown provider is rejected in
// one place with one message naming both the bad value and the valid set.
func lookupProvider(name string) (modelProvider, error) {
	p, ok := knownModelProviders[name]
	if !ok {
		return modelProvider{}, fmt.Errorf("desktop config: unknown model provider %q (known: %s)", name, strings.Join(ModelProviderNames(), ", "))
	}
	return p, nil
}

type NgrokConfig struct {
	AuthToken string `json:"authToken"`
}
type ModelConfig struct {
	Provider string `json:"provider"` // one of ModelProviderNames()
	APIKey   string `json:"apiKey"`

	// Name is the model to register as the cluster default, e.g.
	// "claude-sonnet-5" or the namespaced "anthropic/claude-3.5-sonnet" an
	// OpenRouter user would give. Blank — the setup form's model field left
	// empty, and what a config.json written before this field existed
	// carries — means "whatever the selected provider's recommended model
	// is", re-resolved at every bring-up. See EffectiveModel.
	Name string `json:"name,omitempty"`
}
type ChannelConfig struct {
	Kind        string            `json:"kind"` // an external channel (e.g. future "whatsapp")
	Credentials map[string]string `json:"credentials"`
}
type Config struct {
	Model   ModelConfig    `json:"model"`             // REQUIRED
	Channel *ChannelConfig `json:"channel,omitempty"` // nil = built-in chat only
	Ngrok   *NgrokConfig   `json:"ngrok,omitempty"`   // optional; only for external channel/OAuth

	// AdminPasswordHash is a bcrypt hash (see
	// pkg/platform/identity/idp/passwordkind.HashPassword) of the local admin
	// password captured at first-run setup. This is the ONLY form the
	// password is ever persisted in — the plaintext submitted via the
	// setup form's POST /config is hashed by onConfigSubmit before Config
	// ever reaches Save, and never round-trips through this struct or
	// config.json. Empty means no local password IdP has been provisioned
	// yet (e.g. a config.json predating this field, or a re-run that
	// didn't touch the password field — see setupui's keep-on-blank
	// handling).
	AdminPasswordHash string `json:"adminPasswordHash,omitempty"`

	// HealthNotifications toggles macOS notifications on cluster-health
	// transitions (a key pod going unhealthy after bring-up, and recovery).
	// nil — the default for an unset/pre-existing config.json — means ENABLED;
	// an explicit false is the user having unchecked "Notify on health issues"
	// in the menubar. See HealthNotificationsEnabled.
	HealthNotifications *bool `json:"healthNotifications,omitempty"`
}

func (c Config) Validate() error {
	if _, err := lookupProvider(c.Model.Provider); err != nil {
		return err
	}
	if c.Model.APIKey == "" {
		return fmt.Errorf("desktop config: model apiKey required")
	}
	model, err := c.EffectiveModel()
	if err != nil {
		return err
	}
	// A hand-edited config.json ("Edit configuration…" reveals it in Finder)
	// is how a provider switch leaves the previous provider's model behind;
	// catch it here rather than at the agent's first turn. models.ProviderFor
	// stays quiet for an id no built-in table attributes, so a model newer
	// than those tables is still accepted as typed.
	if owner, known := models.ProviderFor(model); known && owner != c.Model.Provider {
		return fmt.Errorf("desktop config: model %q is served by %q, not the selected provider %q — name a %s model or leave model.name empty to use that provider's default", model, owner, c.Model.Provider, c.Model.Provider)
	}
	// Built-in chat is the default (Channel == nil) and needs nothing.
	if c.Channel != nil {
		if _, ok := knownExternalChannelKinds[c.Channel.Kind]; !ok {
			return fmt.Errorf("desktop config: unknown external channel kind %q", c.Channel.Kind)
		}
	}
	// Ngrok is optional — no error when unset.
	return nil
}

// EffectiveModel is the model name to register as the cluster default: the
// explicit Model.Name when the setup form's optional model field was filled
// in, otherwise the selected provider's recommended model. An unknown
// provider is an error rather than a silent empty string — the caller
// registers this value as the cluster default, and a blank one would register
// nothing at all while reporting success.
func (c Config) EffectiveModel() (string, error) {
	p, err := lookupProvider(c.Model.Provider)
	if err != nil {
		return "", err
	}
	if name := strings.TrimSpace(c.Model.Name); name != "" {
		return name, nil
	}
	return p.defaultModel, nil
}

// EnvForInstall maps config to env vars the install path consumes: the model
// API key under the selected provider's key var always; NGROK_AUTHTOKEN only
// when a tunnel is configured. An unknown provider contributes no key var —
// Validate is the gate that rejects one, and it runs before any bring-up.
func (c Config) EnvForInstall() map[string]string {
	env := map[string]string{}
	if p, ok := knownModelProviders[c.Model.Provider]; ok {
		env[p.keyEnvVar] = c.Model.APIKey
	}
	if c.Ngrok != nil && c.Ngrok.AuthToken != "" {
		env["NGROK_AUTHTOKEN"] = c.Ngrok.AuthToken
	}
	return env
}

func Load(dir string) (Config, error) {
	var c Config
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

func (c Config) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600)
}

// HealthNotificationsEnabled reports whether health-transition notifications
// should fire. Unset (nil) defaults to enabled.
func (c Config) HealthNotificationsEnabled() bool {
	return c.HealthNotifications == nil || *c.HealthNotifications
}
