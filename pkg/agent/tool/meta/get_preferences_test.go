package meta_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/subjectresolve"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

// fakePreferencesReader is a scriptable meta.PreferencesReader: it either
// returns a fixed snapshot or a fixed error, and counts calls so tests can
// assert a saver was (or was not) reached only after a read, and how many
// reads a single set_preference call performed. refCalls records every ref
// ForRef was called with, in order, so a test can assert get_preferences
// routed a `user` argument to ForRef (and never to Current) without a second
// fake type.
type fakePreferencesReader struct {
	snap     preferences.SnapshotResponse
	err      error
	calls    int
	refCalls []string
}

func (f *fakePreferencesReader) Current(context.Context) (preferences.SnapshotResponse, error) {
	f.calls++
	if f.err != nil {
		return preferences.SnapshotResponse{}, f.err
	}
	return f.snap, nil
}

func (f *fakePreferencesReader) ForRef(_ context.Context, ref string) (preferences.SnapshotResponse, error) {
	f.refCalls = append(f.refCalls, ref)
	if f.err != nil {
		return preferences.SnapshotResponse{}, f.err
	}
	return f.snap, nil
}

func languageValue(s string) *apiextv1.JSON {
	raw, _ := json.Marshal(s)
	return &apiextv1.JSON{Raw: raw}
}

func snapshotWithLanguage(source preferences.Source, locked bool) preferences.SnapshotResponse {
	return preferences.SnapshotResponse{
		Subject:        "user:alice",
		ClassNamespace: "default",
		ClassName:      "demo-class",
		Snapshot: preferences.Snapshot{
			Keys: []preferences.Resolved{
				{
					Name:        "language",
					Type:        "string",
					Description: "Reply language",
					Value:       languageValue("en"),
					Source:      source,
					Locked:      locked,
				},
			},
		},
	}
}

func TestGetPreferences_Name(t *testing.T) {
	gt := meta.NewGetPreferences(&fakePreferencesReader{})
	assert.Equal(t, "get_preferences", gt.Name())
}

func TestGetPreferences_Success(t *testing.T) {
	reader := &fakePreferencesReader{snap: snapshotWithLanguage(preferences.SourceUser, false)}
	gt := meta.NewGetPreferences(reader)

	res, err := gt.Execute(context.Background(), json.RawMessage(`{}`), nil)
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.False(t, res.Trusted, "preference values are user/admin-authored content, not framework text")
	assert.Contains(t, res.Content, `"language"`)
	assert.Contains(t, res.Content, `"en"`)
	assert.Equal(t, 1, reader.calls)
	assert.Empty(t, reader.refCalls, "no `user` argument must route to Current, never ForRef")
}

func TestGetPreferences_ReaderError(t *testing.T) {
	reader := &fakePreferencesReader{err: errors.New("boom")}
	gt := meta.NewGetPreferences(reader)

	res, err := gt.Execute(context.Background(), json.RawMessage(`{}`), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.True(t, res.Trusted)
	assert.Equal(t, "get_preferences: boom", res.Content)
}

// TestGetPreferences_UserArg_RoutesToForRef proves a `user` argument routes
// to ForRef with the exact ref given, and never touches Current — the two
// reads are mutually exclusive on the server side (GetPreferencesForUserRef's
// doc), so the tool must pick exactly one.
func TestGetPreferences_UserArg_RoutesToForRef(t *testing.T) {
	reader := &fakePreferencesReader{snap: snapshotWithLanguage(preferences.SourceUser, false)}
	gt := meta.NewGetPreferences(reader)

	res, err := gt.Execute(context.Background(), json.RawMessage(`{"user":"trigger-author"}`), nil)
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Equal(t, []string{"trigger-author"}, reader.refCalls)
	assert.Equal(t, 0, reader.calls, "a `user` argument must not also read Current")
}

// TestGetPreferences_InputSchema_DocumentsEveryResolverForm is the doc-drift
// guard: the `user` argument's description must be BUILT from
// subjectresolve.Usages(), not a hand-copied list, so a future resolver
// registered there and forgotten here fails red instead of silently
// documenting a stale set of forms.
func TestGetPreferences_InputSchema_DocumentsEveryResolverForm(t *testing.T) {
	gt := meta.NewGetPreferences(&fakePreferencesReader{})
	schema := string(gt.InputSchema())

	usages := subjectresolve.Usages()
	require.NotEmpty(t, usages, "subjectresolve must have at least one registered resolver")
	for _, u := range usages {
		assert.Contains(t, schema, u.Form, "InputSchema must document resolver form %q verbatim", u.Form)
	}
}
