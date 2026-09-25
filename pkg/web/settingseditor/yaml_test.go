package settingseditor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestSpecYAML_RoundTrip(t *testing.T) {
	in := &v1alpha1.SettingsSpec{
		Limits: &v1alpha1.SettingsLimits{DeniedModels: []string{"gpt-5.5"}},
	}
	b, err := SpecToYAML(in)
	require.NoError(t, err)
	out, err := SpecFromYAML(b)
	require.NoError(t, err)
	assert.Equal(t, in, out)
}

func TestSpecFromYAML_RejectsMultiDocument(t *testing.T) {
	// sigs.k8s.io/yaml silently reads only the first document; a user pasting
	// two docs into the raw editor must get an error, not silent truncation.
	_, err := SpecFromYAML([]byte("limits:\n  deniedModels: [a]\n---\nlimits:\n  deniedModels: [b]\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multi-document")
}

func TestSpecFromYAML_RejectsUnknownFields(t *testing.T) {
	_, err := SpecFromYAML([]byte("limits:\n  notARealField: true\n"))
	require.Error(t, err, "unknown fields must fail, not vanish on apply")
}

func TestSpecFromYAML_AcceptsLeadingCommentThenSeparator(t *testing.T) {
	// A comment (or blank line) before the opening "---" is still ONE document;
	// the scanner must not misread the separator as starting a second doc.
	out, err := SpecFromYAML([]byte("# leading comment\n---\nlimits:\n  deniedModels: [a]\n"))
	require.NoError(t, err)
	require.NotNil(t, out.Limits)
	assert.Equal(t, []string{"a"}, out.Limits.DeniedModels)
}

func TestSpecFromYAML_AcceptsPlainLeadingSeparator(t *testing.T) {
	out, err := SpecFromYAML([]byte("---\nlimits:\n  deniedModels: [a]\n"))
	require.NoError(t, err)
	require.NotNil(t, out.Limits)
	assert.Equal(t, []string{"a"}, out.Limits.DeniedModels)
}

func TestSpecFromYAML_RejectsEmptyFirstDocThenContent(t *testing.T) {
	// "---\n---\ncontent" is an empty first document followed by a second one
	// carrying the content; sigs.k8s.io/yaml would silently truncate to the
	// empty doc, so it must be rejected as multi-document.
	_, err := SpecFromYAML([]byte("---\n---\nlimits:\n  deniedModels: [a]\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multi-document")
}
