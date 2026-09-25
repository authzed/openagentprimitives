package meta_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

// fakePreferenceSaver is a scriptable meta.PreferenceSaver: it records the
// exact key/value/display it was called with (so tests can assert the
// confirm card's wording) and returns a fixed outcome or error.
type fakePreferenceSaver struct {
	outcome meta.SaveOutcome
	err     error
	calls   int

	gotKey     string
	gotValue   *apiextv1.JSON
	gotDisplay string
}

func (f *fakePreferenceSaver) Save(_ context.Context, key string, value *apiextv1.JSON, display string) (meta.SaveOutcome, error) {
	f.calls++
	f.gotKey, f.gotValue, f.gotDisplay = key, value, display
	if f.err != nil {
		return meta.SaveOutcome{}, f.err
	}
	return f.outcome, nil
}

func TestSetPreference_Name(t *testing.T) {
	st := meta.NewSetPreference(&fakePreferencesReader{}, &fakePreferenceSaver{})
	assert.Equal(t, "set_preference", st.Name())
}

// TestSetPreference_InputSchema_NoUserArgument pins that a save stays
// current-user-only: unlike get_preferences, set_preference must NEVER grow
// a `user` argument — a save is a confirm round-trip addressed to the
// CURRENT turn's author (see PreferenceSaver's doc), and a `user` argument
// would let one person's tool call save a value on someone else's behalf
// with no confirm ever reaching them.
func TestSetPreference_InputSchema_NoUserArgument(t *testing.T) {
	st := meta.NewSetPreference(&fakePreferencesReader{}, &fakePreferenceSaver{})

	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(st.InputSchema(), &schema))
	_, hasUser := schema.Properties["user"]
	assert.False(t, hasUser, "set_preference must have no `user` property in its InputSchema")
}

func TestSetPreference_UnknownKey_NamesKnownKeys_SaverNeverCalled(t *testing.T) {
	reader := &fakePreferencesReader{snap: snapshotWithLanguage(preferences.SourceUser, false)}
	saver := &fakePreferenceSaver{}
	st := meta.NewSetPreference(reader, saver)

	res, err := st.Execute(context.Background(), json.RawMessage(`{"key":"timezone","value":"UTC"}`), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content, `unknown preference "timezone"`)
	assert.Contains(t, res.Content, "language", "the error must name the known keys")
	assert.Equal(t, 0, saver.calls)
}

func TestSetPreference_Locked_ExactSubstring_SaverNeverCalled(t *testing.T) {
	reader := &fakePreferencesReader{snap: snapshotWithLanguage(preferences.SourceLocked, true)}
	saver := &fakePreferenceSaver{}
	st := meta.NewSetPreference(reader, saver)

	res, err := st.Execute(context.Background(), json.RawMessage(`{"key":"language","value":"de"}`), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	// Cross-task contract (Task 15's bronze bundles assert on this exact
	// substring plus the key name) — do not reword.
	assert.Contains(t, res.Content, `is locked by admin policy`)
	assert.Contains(t, res.Content, `"language"`)
	assert.Equal(t, 0, saver.calls)
}

func TestSetPreference_EnumMismatch_LocalError_SaverNeverCalled(t *testing.T) {
	snap := preferences.SnapshotResponse{
		Snapshot: preferences.Snapshot{
			Keys: []preferences.Resolved{
				{
					Name: "theme",
					Type: "enum",
					Enum: []spiceboxv1alpha1.PreferenceEnumValue{
						{Value: "dark"},
						{Value: "light"},
					},
				},
			},
		},
	}
	reader := &fakePreferencesReader{snap: snap}
	saver := &fakePreferenceSaver{}
	st := meta.NewSetPreference(reader, saver)

	res, err := st.Execute(context.Background(), json.RawMessage(`{"key":"theme","value":"purple"}`), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content, "purple")
	assert.Equal(t, 0, saver.calls, "an invalid value must never reach the human confirm")
}

func TestSetPreference_Approve_SaverCalledOnceWithDisplay_ResultContainsSaved(t *testing.T) {
	reader := &fakePreferencesReader{snap: snapshotWithLanguage(preferences.SourceUser, false)}
	saver := &fakePreferenceSaver{outcome: meta.SaveOutcome{Approved: true, DecidedBy: "user:alice"}}
	st := meta.NewSetPreference(reader, saver)

	res, err := st.Execute(context.Background(), json.RawMessage(`{"key":"language","value":"de"}`), nil)
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content, "saved: language")
	assert.Contains(t, res.Content, "confirmed by user:alice")

	assert.Equal(t, 1, saver.calls)
	assert.Equal(t, "language", saver.gotKey)
	require.NotNil(t, saver.gotValue)
	assert.Equal(t, `"de"`, string(saver.gotValue.Raw))
	assert.Equal(t, `Save "language: de" as your default for this agent?`, saver.gotDisplay)

	// The saved value is re-read, not assumed from the request.
	assert.Equal(t, 2, reader.calls)
}

// TestSetPreference_Approve_MultilineValue_DisplayCollapsesNewlines_SavedValueRaw
// proves the confirm card's rendered value collapses newlines to single
// spaces (renderHumanValue's sanitization choke point), while the value
// handed to the saver — what actually gets committed — remains the raw,
// unsanitized original.
func TestSetPreference_Approve_MultilineValue_DisplayCollapsesNewlines_SavedValueRaw(t *testing.T) {
	reader := &fakePreferencesReader{snap: snapshotWithLanguage(preferences.SourceUser, false)}
	saver := &fakePreferenceSaver{outcome: meta.SaveOutcome{Approved: true, DecidedBy: "user:alice"}}
	st := meta.NewSetPreference(reader, saver)

	rawValue := "line one\nline two\nline three"
	args, err := json.Marshal(map[string]any{"key": "language", "value": rawValue})
	require.NoError(t, err)

	res, err := st.Execute(context.Background(), args, nil)
	require.NoError(t, err)
	assert.False(t, res.IsError)

	assert.Equal(t, 1, saver.calls)
	assert.Equal(t, `Save "language: line one line two line three" as your default for this agent?`, saver.gotDisplay)
	assert.NotContains(t, saver.gotDisplay, "\n")

	require.NotNil(t, saver.gotValue, "the saved value must remain the raw, untruncated original")
	var gotRaw string
	require.NoError(t, json.Unmarshal(saver.gotValue.Raw, &gotRaw))
	assert.Equal(t, rawValue, gotRaw)
}

// TestSetPreference_Approve_LongValue_DisplayTruncated_SavedValueRaw proves
// the confirm card caps a long model-chosen value at 120 runes with a "…"
// suffix, while the saved value — what actually gets committed — remains
// the raw, untruncated original.
func TestSetPreference_Approve_LongValue_DisplayTruncated_SavedValueRaw(t *testing.T) {
	reader := &fakePreferencesReader{snap: snapshotWithLanguage(preferences.SourceUser, false)}
	saver := &fakePreferenceSaver{outcome: meta.SaveOutcome{Approved: true, DecidedBy: "user:alice"}}
	st := meta.NewSetPreference(reader, saver)

	rawValue := strings.Repeat("a", 200)
	args, err := json.Marshal(map[string]any{"key": "language", "value": rawValue})
	require.NoError(t, err)

	res, err := st.Execute(context.Background(), args, nil)
	require.NoError(t, err)
	assert.False(t, res.IsError)

	assert.Equal(t, 1, saver.calls)
	wantDisplay := `Save "language: ` + strings.Repeat("a", 120) + `…" as your default for this agent?`
	assert.Equal(t, wantDisplay, saver.gotDisplay)

	require.NotNil(t, saver.gotValue, "the saved value must remain the raw, untruncated original")
	var gotRaw string
	require.NoError(t, json.Unmarshal(saver.gotValue.Raw, &gotRaw))
	assert.Equal(t, rawValue, gotRaw)
	assert.Len(t, gotRaw, 200)
}

func TestSetPreference_Clear_DisplayNamesTheKey(t *testing.T) {
	reader := &fakePreferencesReader{snap: snapshotWithLanguage(preferences.SourceUser, false)}
	saver := &fakePreferenceSaver{outcome: meta.SaveOutcome{Approved: true, DecidedBy: "user:alice"}}
	st := meta.NewSetPreference(reader, saver)

	res, err := st.Execute(context.Background(), json.RawMessage(`{"key":"language"}`), nil)
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Nil(t, saver.gotValue, "omitted value means clear, never a JSON null value")
	assert.Equal(t, `Clear your saved "language"?`, saver.gotDisplay)
}

func TestSetPreference_Denied_PlainNotSavedText_IsErrorFalse(t *testing.T) {
	reader := &fakePreferencesReader{snap: snapshotWithLanguage(preferences.SourceUser, false)}
	saver := &fakePreferenceSaver{outcome: meta.SaveOutcome{Denied: true, DecidedBy: "user:alice"}}
	st := meta.NewSetPreference(reader, saver)

	res, err := st.Execute(context.Background(), json.RawMessage(`{"key":"language","value":"de"}`), nil)
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Equal(t, "not saved: the user declined.", res.Content)
}

func TestSetPreference_TimedOut_PlainNotSavedText_IsErrorFalse(t *testing.T) {
	reader := &fakePreferencesReader{snap: snapshotWithLanguage(preferences.SourceUser, false)}
	saver := &fakePreferenceSaver{outcome: meta.SaveOutcome{TimedOut: true}}
	st := meta.NewSetPreference(reader, saver)

	res, err := st.Execute(context.Background(), json.RawMessage(`{"key":"language","value":"de"}`), nil)
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Equal(t, "not saved: confirmation timed out.", res.Content)
}

func TestSetPreference_ReaderError_IsError(t *testing.T) {
	reader := &fakePreferencesReader{err: errors.New("read boom")}
	saver := &fakePreferenceSaver{}
	st := meta.NewSetPreference(reader, saver)

	res, err := st.Execute(context.Background(), json.RawMessage(`{"key":"language","value":"de"}`), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Equal(t, 0, saver.calls)
}

func TestSetPreference_SaverTransportError_IsError(t *testing.T) {
	reader := &fakePreferencesReader{snap: snapshotWithLanguage(preferences.SourceUser, false)}
	saver := &fakePreferenceSaver{err: errors.New("publish boom")}
	st := meta.NewSetPreference(reader, saver)

	res, err := st.Execute(context.Background(), json.RawMessage(`{"key":"language","value":"de"}`), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Equal(t, 1, saver.calls)
}

func TestSetPreference_MalformedArgs_IsError(t *testing.T) {
	st := meta.NewSetPreference(&fakePreferencesReader{}, &fakePreferenceSaver{})

	res, err := st.Execute(context.Background(), json.RawMessage(`{not valid json`), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.True(t, res.Trusted)
}
