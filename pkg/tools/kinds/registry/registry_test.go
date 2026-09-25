package registry

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/tools/contract"
)

// stubKind is a minimal contract.Kind for registry tests. claimPrefix
// gates DecodeYAML: a doc beginning with claimPrefix is "claimed" and
// returns a metav1.PartialObjectMetadata stamped with the kind name;
// otherwise (nil, nil) is returned so the registry can try the next
// kind. errOnDecode forces DecodeYAML to return an error instead.
type stubKind struct {
	name        string
	claimPrefix string
	errOnDecode error
}

func (s stubKind) Name() string                         { return s.name }
func (s stubKind) GVR() schema.GroupVersionResource     { return schema.GroupVersionResource{} }
func (s stubKind) NewObject() client.Object             { return &metav1.PartialObjectMetadata{} }
func (s stubKind) NewList() client.ObjectList           { return &metav1.PartialObjectMetadataList{} }
func (s stubKind) Row(client.Object) contract.Row       { return contract.Row{} }
func (s stubKind) Detail(client.Object) contract.Detail { return contract.Detail{} }

func (s stubKind) DecodeYAML(doc []byte) (client.Object, error) {
	if s.errOnDecode != nil {
		return nil, s.errOnDecode
	}
	if s.claimPrefix == "" {
		return nil, nil
	}
	if strings.HasPrefix(string(doc), s.claimPrefix) {
		return &metav1.PartialObjectMetadata{
			TypeMeta: metav1.TypeMeta{Kind: s.name},
			ObjectMeta: metav1.ObjectMeta{
				Name: s.name,
			},
		}, nil
	}
	return nil, nil
}

func TestRegisterAndGet(t *testing.T) {
	Reset()
	Register(stubKind{name: "A"})
	k, ok := Get("A")
	require.True(t, ok, "Get(A) should hit")
	require.NotNil(t, k)
	assert.Equal(t, "A", k.Name())
	_, ok = Get("missing")
	assert.False(t, ok, "Get(missing) should miss")
}

func TestRegisterDuplicatePanics(t *testing.T) {
	Reset()
	Register(stubKind{name: "X"})
	require.Panics(t, func() {
		Register(stubKind{name: "X"})
	}, "expected panic on duplicate registration")
}

func TestAllSorted(t *testing.T) {
	Reset()
	// Insert in non-alphabetical order; All must return sorted by name.
	Register(stubKind{name: "C"})
	Register(stubKind{name: "A"})
	Register(stubKind{name: "B"})
	all := All()
	require.Len(t, all, 3)
	got := []string{all[0].Name(), all[1].Name(), all[2].Name()}
	assert.Equal(t, []string{"A", "B", "C"}, got)
}

func TestDecodeAnyDispatches(t *testing.T) {
	Reset()
	Register(stubKind{name: "First", claimPrefix: "kind: First"})
	Register(stubKind{name: "Second", claimPrefix: "kind: Second"})

	obj, k, err := DecodeAny([]byte("kind: Second\nname: foo"))
	require.NoError(t, err)
	require.NotNil(t, k)
	assert.Equal(t, "Second", k.Name())
	pom, ok := obj.(*metav1.PartialObjectMetadata)
	require.True(t, ok, "obj is not PartialObjectMetadata: %T", obj)
	assert.Equal(t, "Second", pom.GetName())
}

func TestDecodeAnyNoClaimantListsTried(t *testing.T) {
	Reset()
	Register(stubKind{name: "Alpha", claimPrefix: "kind: Alpha"})
	Register(stubKind{name: "Beta", claimPrefix: "kind: Beta"})

	_, _, err := DecodeAny([]byte("kind: Gamma"))
	require.Error(t, err)
	for _, sub := range []string{"Alpha", "Beta", "no registered tool kind"} {
		assert.Contains(t, err.Error(), sub)
	}
}

func TestDecodeAnyEmptyRegistry(t *testing.T) {
	Reset()
	_, _, err := DecodeAny([]byte("kind: Anything"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no tool kinds registered")
}

func TestDecodeAnyPropagatesError(t *testing.T) {
	Reset()
	want := errors.New("boom")
	Register(stubKind{name: "Broken", errOnDecode: want})
	_, _, err := DecodeAny([]byte("anything"))
	require.Error(t, err)
	assert.ErrorIs(t, err, want)
}
