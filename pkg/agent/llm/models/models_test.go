package models_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/models"
)

func TestAnthropic_PricingAndCapabilities(t *testing.T) {
	opus, ok := models.Anthropic["claude-opus-4-8"]
	require.True(t, ok, "claude-opus-4-8 must be registered")
	assert.Equal(t, 5.0, opus.Pricing.InputPerMTok)
	assert.True(t, opus.Capabilities.Has(llm.CapNativeFileOut), "opus 4-8 supports native file output")
	assert.True(t, opus.Capabilities.Has(llm.CapNativeFileIn), "opus 4-8 supports native file input")

	haiku, ok := models.Anthropic["claude-haiku-4-5"]
	require.True(t, ok, "claude-haiku-4-5 must be registered")
	assert.Empty(t, haiku.Capabilities, "haiku has no native file capabilities")
}

func TestAnthropic_CapabilityMatrix(t *testing.T) {
	cases := []struct {
		name       string
		wantNative bool
	}{
		{name: "claude-opus-4-8", wantNative: true},
		{name: "claude-opus-4-7", wantNative: true},
		{name: "claude-opus-4-6", wantNative: true},
		{name: "claude-sonnet-4-6", wantNative: true},
		{name: "claude-sonnet-5", wantNative: true},
		{name: "claude-fable-5", wantNative: true},
		{name: "claude-haiku-4-5", wantNative: false},
		{name: "claude-haiku-4-5-20251001", wantNative: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info, ok := models.Anthropic[tc.name]
			require.True(t, ok, "%s must be registered", tc.name)
			assert.Equal(t, tc.wantNative, info.Capabilities.Has(llm.CapNativeFileOut))
			assert.Equal(t, tc.wantNative, info.Capabilities.Has(llm.CapNativeFileIn))
		})
	}
}

func TestOpenAI_NoNativeFileCapabilities(t *testing.T) {
	require.NotEmpty(t, models.OpenAI, "OpenAI table must be populated")
	for name, info := range models.OpenAI {
		assert.Empty(t, info.Capabilities, "%s must have no capabilities set", name)
	}
}

func TestOpenRouter_AutoModelHasNoFixedPrice(t *testing.T) {
	auto, ok := models.OpenRouter["openrouter/auto"]
	require.True(t, ok, "openrouter/auto must be registered")
	assert.Zero(t, auto.Pricing.InputPerMTok, "the auto-router has no fixed price of its own — the reported cost per response is authoritative")
}

// Every Anthropic row must declare its native input set. A row added without
// it silently loses native passthrough for that model — the file would fall
// back to extracted text with nothing logged, which is indistinguishable from
// "this format has no native support" at the call site.
func TestAnthropicRowsDeclareNativeInput(t *testing.T) {
	for id, info := range models.Anthropic {
		assert.True(t, info.NativeInputMIMEs.Has("application/pdf"),
			"model %q must declare native PDF input", id)
		assert.True(t, info.NativeInputMIMEs.Has("image/png"),
			"model %q must declare native PNG input", id)
	}
}

// A model row that declares a MIME its provider's adapter cannot emit is a
// seam defect: hydration builds the native block, the adapter rejects it, and
// the turn fails. Only the Anthropic adapter emits native blocks today, so
// only Anthropic rows may declare a native set. This test is what makes
// "moving a MIME is a one-row edit" safe — it fails the moment a row outruns
// its adapter.
func TestOnlyAdaptersThatEmitNativeBlocksDeclareThem(t *testing.T) {
	for id, info := range models.OpenAI {
		assert.Empty(t, info.NativeInputMIMEs,
			"OpenAI row %q declares native MIMEs but openaicompat cannot emit native blocks", id)
	}
	for id, info := range models.OpenRouter {
		assert.Empty(t, info.NativeInputMIMEs,
			"OpenRouter row %q declares native MIMEs but openaicompat cannot emit native blocks", id)
	}
}

// The Anthropic adapter's "document" case can emit exactly one wire shape:
// sdk.Base64PDFSourceParam (Anthropic's document source union has no
// generic "base64 + arbitrary media_type" variant — see anthropic.go's
// comment on the document case). So application/pdf is the only MIME any
// model row may map to llm.NativeBlockDocument today. A row that maps a
// second MIME to NativeBlockDocument would not be rejected anywhere: the
// adapter has no MIME-driven branch or error path left (that was
// deliberately removed in favor of this registry-level guard), so the new
// MIME would be silently mislabeled "application/pdf" on the wire. Adding
// one requires a matching SDK source variant in the adapter's document
// case FIRST, not just a registry row here.
func TestOnlyPDFMapsToNativeBlockDocument(t *testing.T) {
	for _, table := range models.Tables() {
		for id, info := range table {
			for mime, blockType := range info.NativeInputMIMEs {
				if blockType != llm.NativeBlockDocument {
					continue
				}
				assert.Equal(t, "application/pdf", mime,
					"model %q maps %q to NativeBlockDocument, but the Anthropic adapter's document case can only emit application/pdf's wire shape (Base64PDFSourceParam) — adding a second document MIME needs a matching SDK source variant in anthropic.go's document case before it can be a registry row", id, mime)
			}
		}
	}
}

func TestTables_EnumeratesEveryProvider(t *testing.T) {
	seen := map[string]models.ModelInfo{}
	for _, table := range models.Tables() {
		for id, info := range table {
			seen[id] = info
		}
	}
	assert.Contains(t, seen, "claude-opus-4-8")
	assert.Contains(t, seen, "gpt-5.3-codex")
	assert.Contains(t, seen, "openrouter/auto")
	assert.Len(t, seen, len(models.Anthropic)+len(models.OpenAI)+len(models.OpenRouter), "Tables must expose every entry with no id collisions")
}

// TestProviders_ListsEveryTabledProvider pins the list callers use to tell a
// real upstream (its own API key, its own model ids) from a provider name that
// fronts none — the settings wizard exempts the latter from having to name a
// token env var.
func TestProviders_ListsEveryTabledProvider(t *testing.T) {
	got := models.Providers()
	assert.Equal(t, []string{"anthropic", "openai", "openrouter"}, got)
	assert.Len(t, got, len(models.Tables()), "Providers and Tables must project the same list")
	assert.NotContains(t, got, "test", "the harness provider fronts no upstream and has no built-in table")
}

func TestProviderFor_AttributesAKnownID(t *testing.T) {
	cases := []struct {
		name string
		id   string
		want string
	}{
		{name: "anthropic id: attributed to anthropic", id: "claude-sonnet-5", want: "anthropic"},
		{name: "openai id: attributed to openai", id: "gpt-5.5", want: "openai"},
		{name: "openai codex id: attributed to openai", id: "gpt-5.3-codex", want: "openai"},
		{name: "openrouter virtual model: attributed to openrouter", id: "openrouter/auto", want: "openrouter"},
		{name: "namespaced openrouter id naming another vendor: still attributed to openrouter", id: "anthropic/claude-3.5-sonnet", want: "openrouter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, known := models.ProviderFor(tc.id)
			require.True(t, known, "%s must be attributable", tc.id)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestProviderFor_UnknownIDIsNotAttributed pins the half of the contract that
// keeps callers from refusing legitimate configs: the tables trail new
// releases and models.OpenRouter is sparse by design, so an id none of them
// lists must come back unattributed rather than guessed at.
func TestProviderFor_UnknownIDIsNotAttributed(t *testing.T) {
	for _, id := range []string{
		"",
		"gpt-6-not-yet-tabled",
		"claude-sonnet-99",
		"acme/some-new-model",
		"Claude-Sonnet-5", // attribution is exact, not case-insensitive
	} {
		t.Run(id+": unattributed", func(t *testing.T) {
			got, known := models.ProviderFor(id)
			assert.False(t, known)
			assert.Empty(t, got)
		})
	}
}

// TestProviderFor_EveryBuiltInIDIsUnambiguous walks the real tables: every id
// they carry must attribute to exactly one provider. This is the live-data
// half of ProviderFor's "more than one table lists it" arm — that arm returns
// unattributed by design, and this proves no built-in id reaches it today.
func TestProviderFor_EveryBuiltInIDIsUnambiguous(t *testing.T) {
	for _, table := range models.Tables() {
		for id := range table {
			got, known := models.ProviderFor(id)
			assert.True(t, known, "%s appears in a built-in table but is not attributable", id)
			assert.NotEmpty(t, got)
		}
	}
}
