package settingseditor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestDrift_NoDifference_Empty(t *testing.T) {
	a := &v1alpha1.SettingsSpec{Limits: &v1alpha1.SettingsLimits{DeniedModels: []string{"m"}}}
	b := &v1alpha1.SettingsSpec{Limits: &v1alpha1.SettingsLimits{DeniedModels: []string{"m"}}}
	got, err := Drift(a, b)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestDrift_ReportsPathKeptAliveByAnotherManager(t *testing.T) {
	intended := &v1alpha1.SettingsSpec{}
	readback := &v1alpha1.SettingsSpec{
		Limits: &v1alpha1.SettingsLimits{NativeFileHandling: ptr(true)},
	}
	got, err := Drift(intended, readback)
	require.NoError(t, err)
	assert.Equal(t, []string{"limits.nativeFileHandling"}, got)
}

func TestDrift_ReportsChangedScalarInsideStruct(t *testing.T) {
	intended := &v1alpha1.SettingsSpec{Limits: &v1alpha1.SettingsLimits{Budget: &v1alpha1.SettingsBudgetCeiling{MaxTurns: 10}}}
	readback := &v1alpha1.SettingsSpec{Limits: &v1alpha1.SettingsLimits{Budget: &v1alpha1.SettingsBudgetCeiling{MaxTurns: 20}}}
	got, err := Drift(intended, readback)
	require.NoError(t, err)
	assert.Equal(t, []string{"limits.budget.maxTurns"}, got)
}

func TestDrift_ReportsChangedArrayValue(t *testing.T) {
	intended := &v1alpha1.SettingsSpec{Limits: &v1alpha1.SettingsLimits{DeniedModels: []string{"a"}}}
	readback := &v1alpha1.SettingsSpec{Limits: &v1alpha1.SettingsLimits{DeniedModels: []string{"a", "b"}}}
	got, err := Drift(intended, readback)
	require.NoError(t, err)
	assert.Equal(t, []string{"limits.deniedModels"}, got)
}

func ptr[T any](v T) *T { return &v }
