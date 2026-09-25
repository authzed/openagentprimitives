package channelkinds_test

import (
	"testing"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeContributor struct {
	frag *spiceboxv1alpha1.SpiceDBSchemaFragment
}

func (f *fakeContributor) SpiceDBSchemaFragment() *spiceboxv1alpha1.SpiceDBSchemaFragment {
	return f.frag
}

func TestSchemaContributor_Interface(t *testing.T) {
	f := &spiceboxv1alpha1.SpiceDBSchemaFragment{RawZed: "definition x {}"}
	var c channelkinds.SchemaContributor = &fakeContributor{frag: f}
	got := c.SpiceDBSchemaFragment()
	require.NotNil(t, got)
	assert.Equal(t, "definition x {}", got.RawZed)
}

func TestSchemaContributor_NilFragment(t *testing.T) {
	var c channelkinds.SchemaContributor = &fakeContributor{frag: nil}
	assert.Nil(t, c.SpiceDBSchemaFragment())
}
