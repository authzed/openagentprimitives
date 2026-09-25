package capability

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

// stubPreferencesReader/stubPreferenceSaver are the minimal fakes needed to
// prove wiring here — behavior of the tools themselves is covered in
// pkg/agent/tool/meta.
type stubPreferencesReader struct{}

func (stubPreferencesReader) Current(context.Context) (preferences.SnapshotResponse, error) {
	return preferences.SnapshotResponse{}, nil
}

func (stubPreferencesReader) ForRef(context.Context, string) (preferences.SnapshotResponse, error) {
	return preferences.SnapshotResponse{}, nil
}

type stubPreferenceSaver struct{}

func (stubPreferenceSaver) Save(context.Context, string, *apiextv1.JSON, string) (meta.SaveOutcome, error) {
	return meta.SaveOutcome{}, nil
}

func onePreferenceSchema() []spiceboxv1alpha1.UserPreferenceSchema {
	return []spiceboxv1alpha1.UserPreferenceSchema{{Name: "language", Type: "string"}}
}

func TestPreferencesDefaultOn(t *testing.T) {
	c, ok := Lookup("preferences")
	require.True(t, ok)
	assert.True(t, c.DefaultOn(), "preferences must be default-on: nothing to opt into beyond the class's own schema")
	assert.False(t, c.Infrastructural())
}

func TestPreferencesOffer_EmptySchema_NilNil(t *testing.T) {
	c, _ := Lookup("preferences")
	tools, skip := c.Offer(OfferContext{
		Ctx: context.Background(), Granted: true, Enabled: true,
		Env: RunnerEnv{
			UserPreferences:   nil,
			PreferencesReader: stubPreferencesReader{},
			PreferenceSaver:   stubPreferenceSaver{},
		},
	})
	assert.Nil(t, skip, "empty schema is feature-absent, never a skip")
	assert.Empty(t, tools)
}

func TestPreferencesOffer_NoReaderOrSaver_SkipReason(t *testing.T) {
	c, _ := Lookup("preferences")
	cases := []struct {
		name string
		env  RunnerEnv
	}{
		{name: "nil reader", env: RunnerEnv{UserPreferences: onePreferenceSchema(), PreferenceSaver: stubPreferenceSaver{}}},
		{name: "nil saver", env: RunnerEnv{UserPreferences: onePreferenceSchema(), PreferencesReader: stubPreferencesReader{}}},
		{name: "both nil", env: RunnerEnv{UserPreferences: onePreferenceSchema()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tools, skip := c.Offer(OfferContext{Ctx: context.Background(), Granted: true, Enabled: true, Env: tc.env})
			require.NotNil(t, skip)
			assert.Equal(t, "preferences", skip.Capability)
			assert.Empty(t, tools)
		})
	}
}

func TestPreferencesOffer_BothToolsAndPromptSection(t *testing.T) {
	pc, ok := Lookup("preferences")
	require.True(t, ok)
	sectionOfferer, ok := pc.(SectionOfferer)
	require.True(t, ok, "preferences must implement SectionOfferer to carry its guidance text")

	o := OfferContext{
		Ctx: context.Background(), Granted: true, Enabled: true,
		Env: RunnerEnv{
			UserPreferences:   onePreferenceSchema(),
			PreferencesReader: stubPreferencesReader{},
			PreferenceSaver:   stubPreferenceSaver{},
		},
	}
	tools, sections, skip := sectionOfferer.OfferWithSections(o)
	assert.Nil(t, skip)
	assert.Equal(t, []string{"get_preferences", "set_preference"}, toolNames(tools))
	require.Len(t, sections, 1)
	assert.Equal(t, preferencesSectionTitle, sections[0].Title)
	assert.Contains(t, sections[0].Body, "set_preference")
	assert.Contains(t, sections[0].Body, "get_preferences")

	// Offer (the plain-Capability view) must agree on the tools.
	plainTools, plainSkip := pc.Offer(o)
	assert.Nil(t, plainSkip)
	assert.Equal(t, toolNames(tools), toolNames(plainTools))
}

func TestPreferencesAssembleAll_ContributesSectionOnlyWhenActive(t *testing.T) {
	withoutSchema := AssembleAll(context.Background(), AssembleDeps{
		Class:   classGranting(t),
		Session: sessionFixture(t),
		Env:     RunnerEnv{},
		Logger:  testLogger(t),
	})
	assert.NotContains(t, toolNames(withoutSchema.Tools), "get_preferences")
	for _, s := range withoutSchema.Sections {
		assert.NotEqual(t, preferencesSectionTitle, s.Title)
	}

	withSchema := AssembleAll(context.Background(), AssembleDeps{
		Class:   classGranting(t),
		Session: sessionFixture(t),
		Env: RunnerEnv{
			UserPreferences:   onePreferenceSchema(),
			PreferencesReader: stubPreferencesReader{},
			PreferenceSaver:   stubPreferenceSaver{},
		},
		Logger: testLogger(t),
	})
	assert.Contains(t, toolNames(withSchema.Tools), "get_preferences")
	assert.Contains(t, toolNames(withSchema.Tools), "set_preference")
	found := false
	for _, s := range withSchema.Sections {
		if s.Title == preferencesSectionTitle {
			found = true
		}
	}
	assert.True(t, found, "the preferences prompt section must ride the same Assemble pass as its tools")
}
