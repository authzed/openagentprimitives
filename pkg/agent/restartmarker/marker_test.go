package restartmarker_test

import (
	"crypto/ed25519"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/authzed/openagentprimitives/pkg/agent/restartmarker"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/x/keyid"
)

// keyLookup is a minimal (publisher,keyID)→key map. Keying on BOTH — not keyID
// alone — mirrors the production registry, so a key registered for one
// publisher cannot verify another's marker.
type keyLookup map[[2]string]ed25519.PublicKey

func (k keyLookup) PublisherKey(publisher, keyID string) (ed25519.PublicKey, bool) {
	pub, ok := k[[2]string{publisher, keyID}]
	return pub, ok
}

// newSigner builds a deterministic Signer plus a lookup that trusts it.
func newSigner(t *testing.T, seedByte byte, publisher string) (*restartmarker.Signer, keyLookup) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = seedByte
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	return restartmarker.NewSigner(priv, publisher),
		keyLookup{{publisher, keyid.For(pub)}: pub}
}

func newParent(t *testing.T, ns, name string, uid types.UID) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: uid},
	}
}

func newMarker(t *testing.T) *spiceboxv1alpha1.PendingRestart {
	t.Helper()
	return &spiceboxv1alpha1.PendingRestart{
		Mode:               spiceboxv1alpha1.PendingRestartModeTakeover,
		NewUserText:        "carry this on",
		TriggeredBy:        "user:victim",
		TargetSessionName:  "p-tk1",
		CutTurnIndex:       3,
		NewOwnerExternalID: "U123",
		InheritHistory:     true,
		RequestedAt:        metav1.NewTime(time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)),
	}
}

func TestSignThenVerify_RoundTripsAndRejectsTampering(t *testing.T) {
	signer, keys := newSigner(t, 0x01, restartmarker.Publisher)
	parent := newParent(t, "ns", "p", "uid-1")

	cases := []struct {
		name string
		// mutate runs AFTER signing. nil means "leave the signed marker alone".
		mutate  func(parent *spiceboxv1alpha1.AgentSession, pr *spiceboxv1alpha1.PendingRestart)
		wantErr error
	}{
		{name: "untouched signed marker: verifies", mutate: nil},
		{
			name: "triggeredBy swapped to another user: ErrBadSignature",
			mutate: func(_ *spiceboxv1alpha1.AgentSession, pr *spiceboxv1alpha1.PendingRestart) {
				pr.TriggeredBy = "user:attacker"
			},
			wantErr: restartmarker.ErrBadSignature,
		},
		{
			name: "mode rewritten from takeover to inherit: ErrBadSignature",
			mutate: func(_ *spiceboxv1alpha1.AgentSession, pr *spiceboxv1alpha1.PendingRestart) {
				pr.Mode = spiceboxv1alpha1.PendingRestartModeInherit
			},
			wantErr: restartmarker.ErrBadSignature,
		},
		{
			name: "target child renamed: ErrBadSignature",
			mutate: func(_ *spiceboxv1alpha1.AgentSession, pr *spiceboxv1alpha1.PendingRestart) {
				pr.TargetSessionName = "p-tk-other"
			},
			wantErr: restartmarker.ErrBadSignature,
		},
		{
			name: "inheritHistory flipped: ErrBadSignature",
			mutate: func(_ *spiceboxv1alpha1.AgentSession, pr *spiceboxv1alpha1.PendingRestart) {
				pr.InheritHistory = !pr.InheritHistory
			},
			wantErr: restartmarker.ErrBadSignature,
		},
		{
			name: "newOwnerExternalID rewritten: ErrBadSignature",
			mutate: func(_ *spiceboxv1alpha1.AgentSession, pr *spiceboxv1alpha1.PendingRestart) {
				pr.NewOwnerExternalID = "U999"
			},
			wantErr: restartmarker.ErrBadSignature,
		},
		{
			name: "replayed onto a different session: ErrBadSignature",
			mutate: func(parent *spiceboxv1alpha1.AgentSession, _ *spiceboxv1alpha1.PendingRestart) {
				parent.Name = "other"
			},
			wantErr: restartmarker.ErrBadSignature,
		},
		{
			name: "replayed onto a same-named session in another namespace: ErrBadSignature",
			mutate: func(parent *spiceboxv1alpha1.AgentSession, _ *spiceboxv1alpha1.PendingRestart) {
				parent.Namespace = "other-ns"
			},
			wantErr: restartmarker.ErrBadSignature,
		},
		{
			name: "replayed onto a recreated session of the same name: ErrBadSignature",
			mutate: func(parent *spiceboxv1alpha1.AgentSession, _ *spiceboxv1alpha1.PendingRestart) {
				parent.UID = "uid-2"
			},
			wantErr: restartmarker.ErrBadSignature,
		},
		{
			// Refused on WHO it claims to be, before the key lookup is consulted
			// at all — so the refusal does not depend on the operator's registry
			// happening not to hold system:operator (it does hold it).
			name: "signature re-attributed to another publisher: ErrWrongPublisher",
			mutate: func(_ *spiceboxv1alpha1.AgentSession, pr *spiceboxv1alpha1.PendingRestart) {
				pr.Signature.Publisher = "system:operator"
			},
			wantErr: restartmarker.ErrWrongPublisher,
		},
		{
			name: "signature stripped: ErrUnsigned",
			mutate: func(_ *spiceboxv1alpha1.AgentSession, pr *spiceboxv1alpha1.PendingRestart) {
				pr.Signature = nil
			},
			wantErr: restartmarker.ErrUnsigned,
		},
		{
			// NOT ErrUnsigned, which the operator now reads as "an older
			// connector wrote this, mid-upgrade" and answers with a re-send
			// prompt. No writer produces this shape — the old one had no
			// envelope, the new one always fills it — so a hand-built marker
			// must not reach that gentler treatment by emptying one field.
			name: "signature emptied but envelope kept: ErrBadSignature, not ErrUnsigned",
			mutate: func(_ *spiceboxv1alpha1.AgentSession, pr *spiceboxv1alpha1.PendingRestart) {
				pr.Signature.Sig = nil
			},
			wantErr: restartmarker.ErrBadSignature,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := parent.DeepCopy()
			pr := newMarker(t)
			require.NoError(t, signer.Sign(p, pr), "signing the marker must succeed")
			require.NotNil(t, pr.Signature, "Sign must populate the signature envelope")

			if tc.mutate != nil {
				tc.mutate(p, pr)
			}

			err := restartmarker.Verify(keys, p, pr)
			if tc.wantErr == nil {
				assert.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, tc.wantErr)
		})
	}
}

func TestVerify_FailsClosedOnMissingTrustInputs(t *testing.T) {
	signer, keys := newSigner(t, 0x01, restartmarker.Publisher)
	parent := newParent(t, "ns", "p", "uid-1")
	pr := newMarker(t)
	require.NoError(t, signer.Sign(parent, pr))

	t.Run("nil key lookup: refused rather than accepted unchecked", func(t *testing.T) {
		assert.Error(t, restartmarker.Verify(nil, parent, pr))
	})

	t.Run("nil marker: refused", func(t *testing.T) {
		assert.Error(t, restartmarker.Verify(keys, parent, nil))
	})

	t.Run("nil parent: refused", func(t *testing.T) {
		assert.Error(t, restartmarker.Verify(keys, nil, pr))
	})

	t.Run("malformed trusted key: ErrUnknownKey, no panic", func(t *testing.T) {
		// ed25519.Verify panics on a wrong-sized key and the registry is
		// ConfigMap-sourced, so a corrupt entry must yield a clean refusal.
		bad := keyLookup{{pr.Signature.Publisher, pr.Signature.KeyID}: ed25519.PublicKey("short")}
		assert.ErrorIs(t, restartmarker.Verify(bad, parent, pr), restartmarker.ErrUnknownKey)
	})

	t.Run("another component's key: ErrBadSignature", func(t *testing.T) {
		// Same publisher and keyID claimed, different key material behind it.
		other, _ := newSigner(t, 0x02, restartmarker.Publisher)
		otherPr := newMarker(t)
		require.NoError(t, other.Sign(parent, otherPr))
		// Present the foreign signature under the trusted keyID.
		forged := newMarker(t)
		require.NoError(t, signer.Sign(parent, forged))
		forged.Signature.Sig = otherPr.Signature.Sig
		assert.ErrorIs(t, restartmarker.Verify(keys, parent, forged), restartmarker.ErrBadSignature)
	})
}

// TestVerify_PinsPublisherToTheChannelsdConstant pins WHO may author a marker,
// not merely that the author is someone the registry knows. The operator passes
// the component publisher registry, which holds system:operator and
// system:authzd alongside system:channelsd — so without this pin, compromise of
// ANY component publisher (or a publisher added to that registry later) mints a
// marker that verifies, and a verified takeover marker names its own victim and
// gets the child stamped with that user's identity. Only channelsd ever writes
// status.pendingRestart; the exported Publisher constant says so, and the
// verifier has to be the thing that enforces it.
func TestVerify_PinsPublisherToTheChannelsdConstant(t *testing.T) {
	parent := newParent(t, "ns", "p", "uid-1")

	cases := []struct {
		name      string
		publisher string
	}{
		{name: "operator's own registered key: refused", publisher: "system:operator"},
		{name: "authzd's registered key: refused", publisher: "system:authzd"},
		{name: "a session's own audit key: refused", publisher: "session:ns/p"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The foreign publisher is REGISTERED and signs correctly — the only
			// thing wrong with this marker is who wrote it.
			foreign, foreignKeys := newSigner(t, 0x02, tc.publisher)
			channelsd, channelsdKeys := newSigner(t, 0x01, restartmarker.Publisher)
			keys := keyLookup{}
			for k, v := range foreignKeys {
				keys[k] = v
			}
			for k, v := range channelsdKeys {
				keys[k] = v
			}

			pr := newMarker(t)
			require.NoError(t, foreign.Sign(parent, pr), "the foreign publisher signs a well-formed marker")
			require.NotNil(t, pr.Signature)
			require.Equal(t, tc.publisher, pr.Signature.Publisher, "precondition: signed as the foreign publisher")

			assert.ErrorIs(t, restartmarker.Verify(keys, parent, pr), restartmarker.ErrWrongPublisher,
				"only %s may author a restart marker", restartmarker.Publisher)

			// The same lookup must still accept a genuine channelsd marker, so the
			// pin is a publisher check and not a blanket refusal.
			good := newMarker(t)
			require.NoError(t, channelsd.Sign(parent, good))
			assert.NoError(t, restartmarker.Verify(keys, parent, good),
				"channelsd's own marker still verifies against the same registry")
		})
	}
}

// TestDigest_SurvivesMetav1TimeRoundTrip pins the precision contract. metav1.Time
// marshals as RFC3339 with no fractional part, so a digest taken over the
// signer's nanosecond clock reading would never recompute after the marker
// round-trips through the API server — every legitimate restart would be
// refused, and only in a real cluster.
func TestDigest_SurvivesMetav1TimeRoundTrip(t *testing.T) {
	signer, keys := newSigner(t, 0x01, restartmarker.Publisher)
	parent := newParent(t, "ns", "p", "uid-1")

	pr := newMarker(t)
	pr.RequestedAt = metav1.NewTime(time.Date(2026, 8, 9, 12, 0, 0, 123456789, time.UTC))
	require.NoError(t, signer.Sign(parent, pr))

	// What the API server gives back: sub-second precision dropped.
	roundTripped := pr.DeepCopy()
	roundTripped.RequestedAt = metav1.NewTime(pr.RequestedAt.Rfc3339Copy().Time)

	assert.NoError(t, restartmarker.Verify(keys, parent, roundTripped),
		"a marker must still verify after metav1.Time drops sub-second precision")
}

// signatureEnvelope is the one PendingRestart field the digest deliberately
// does not cover — a signature cannot sign itself. Matched by TYPE rather than
// by name so renaming the field keeps the exemption, while a NEW field never
// inherits it.
var signatureEnvelope = reflect.TypeOf((*spiceboxv1alpha1.PendingRestartSignature)(nil))

// mutateField sets v to a value different from its zero value, so the caller can
// ask whether the digest noticed. It FAILS on a type it does not know how to
// change rather than skipping: a marker field of a new type must not be able to
// slip past the coverage check by being unrecognized.
func mutateField(t *testing.T, name string, v reflect.Value) {
	t.Helper()
	switch {
	case v.Type() == reflect.TypeOf(metav1.Time{}):
		v.Set(reflect.ValueOf(metav1.NewTime(time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC))))
	case v.Kind() == reflect.String: // covers identity.Subject
		v.SetString("mutated")
	case v.CanInt():
		v.SetInt(7)
	case v.Kind() == reflect.Bool:
		v.SetBool(true)
	default:
		t.Fatalf("PendingRestart.%s has type %s, which this test does not know how to mutate; "+
			"teach mutateField about it so the field's digest coverage is actually checked", name, v.Type())
	}
}

// TestDigest_CoversEveryPendingRestartField makes the marker's signed-field set
// structural instead of a promise in a doc comment.
//
// Digest serializes a hand-written `canonical` struct that enumerates
// v1alpha1.PendingRestart's fields one by one. Nothing but that comment stopped
// the next field added to the CRD from being left out — and a field outside the
// digest is MUTABLE under an otherwise-valid signature. For this marker that is
// not cosmetic: takeover mode skips the SpiceDB fork gate, the reconciler stamps
// triggeredBy onto the child as its started-by identity, and the child's runner
// SA then gets namespace-crossing RBAC on that user's OAuth Secrets. An unsigned
// field is a hole straight through the only check standing there.
//
// The property under test is the real one — "changing this field changes the
// signed message" — not a name-mapping proxy, so it needs no table to keep in
// sync with either struct.
func TestDigest_CoversEveryPendingRestartField(t *testing.T) {
	parent := newParent(t, "ns", "p", "uid-1")
	const keyID = "kid-1"

	typ := reflect.TypeOf(spiceboxv1alpha1.PendingRestart{})
	require.Positive(t, typ.NumField(), "PendingRestart must have fields for this test to mean anything")

	exempt := 0
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type == signatureEnvelope {
			exempt++
			continue
		}
		t.Run(f.Name+" changes the digest: covered by the signature", func(t *testing.T) {
			base := &spiceboxv1alpha1.PendingRestart{}
			before := restartmarker.Digest(parent, base, restartmarker.Publisher, keyID)

			mutated := base.DeepCopy()
			mutateField(t, f.Name, reflect.ValueOf(mutated).Elem().Field(i))
			require.NotEqual(t, reflect.ValueOf(base).Elem().Field(i).Interface(),
				reflect.ValueOf(mutated).Elem().Field(i).Interface(),
				"precondition: mutateField must actually change %s", f.Name)

			assert.NotEqual(t, before, restartmarker.Digest(parent, mutated, restartmarker.Publisher, keyID),
				"PendingRestart.%s is NOT folded into the digest, so it is mutable under a valid "+
					"signature — add it to restartmarker's canonical struct and bump digestVersion", f.Name)
		})
	}
	assert.Equal(t, 1, exempt,
		"exactly one PendingRestart field (the signature envelope) may be exempt from the digest; "+
			"a second one means a real field is being skipped by this test instead of checked")
}

func TestNewSigner_KeyIDMatchesTheRegistryContentAddress(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)

	s := restartmarker.NewSigner(priv, restartmarker.Publisher)
	assert.Equal(t, keyid.For(pub), s.KeyID(),
		"the signer's keyID must be the same content address publisherkeys.Add validates, or the operator can never resolve it")
}
