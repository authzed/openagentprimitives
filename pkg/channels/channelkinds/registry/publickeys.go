package registry

import (
	"errors"
	"fmt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// errNilChannel is returned rather than nil keys alone so a caller that lost
// its Channel hears about it. Both outcomes are fail-closed; only one is
// diagnosable.
var errNilChannel = errors.New("channelkinds: PublicSecretKeysFor called with a nil Channel")

// PublicSecretKeysFor answers "which data keys of this Channel's credentials
// Secret are PUBLIC identifiers rather than secret material", dispatching on
// the Channel's own kind.
//
// It is the channel-side twin of
// credkind/registry.PublicSecretKeysFor, and the two exist together because
// ONE Secret is named by both registries: a github Channel's credentials Secret
// is the same object a type=githubApp credential points at. Either reference
// may exist without the other, so closing only one seam leaves the other
// configuration treating a published installation id as a leaked credential.
//
// # Fail closed
//
// An UNREGISTERED kind returns an error and NO keys, and a caller must treat
// that exactly as it treats nil — "all of it is secret". Deliberately an error
// rather than IsBrowserSurface's silent false: that helper answers a question
// with a useful either-way answer, whereas an empty list here is
// indistinguishable from a kind that was never asked, and reading it as
// "nothing is secret" is the one mistake this function must not enable.
func PublicSecretKeysFor(ch *spiceboxv1alpha1.Channel) ([]string, error) {
	if ch == nil {
		return nil, errNilChannel
	}
	k, ok := Get(ch.Spec.Kind)
	if !ok {
		// Names(), not a literal list: a refusal must never name a kind this
		// binary did not link.
		return nil, fmt.Errorf("channelkinds: unknown kind %q (registered: %v)", ch.Spec.Kind, Names())
	}
	return k.PublicSecretKeys(ch), nil
}
