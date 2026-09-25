package anthropic_test

import (
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/models"
)

// sdkImageMediaTypes is every value sdk.Base64ImageSourceMediaType is
// permitted to carry. The SDK declares these four and nothing else; the API
// rejects any other media_type with a 400.
//
// Built from the SDK's own constants, never from literals: a future SDK
// release that adds a fifth image format widens this set the moment the
// dependency is bumped, with no edit here, and one that renames a constant
// fails to compile rather than silently narrowing the check.
var sdkImageMediaTypes = map[string]bool{
	string(sdk.Base64ImageSourceMediaTypeImageJPEG): true,
	string(sdk.Base64ImageSourceMediaTypeImagePNG):  true,
	string(sdk.Base64ImageSourceMediaTypeImageGIF):  true,
	string(sdk.Base64ImageSourceMediaTypeImageWebP): true,
}

// TestEveryNativeImageMIMEIsAnSDKMediaType is the image-side mirror of
// TestOnlyPDFMapsToNativeBlockDocument (pkg/agent/llm/models), and it lives
// here — in the package that owns the constraint — because the constraint is
// the SDK's, not the registry's.
//
// BuildParams' image case casts the block's MIME straight to
// sdk.Base64ImageSourceMediaType. That cast cannot fail: Go converts any
// string, and the SDK's four constants are a convention the type does not
// enforce. So a model row adding, say, an HEIC or BMP image type is a green
// build, a green unit suite, a green e2e run (the fake provider declares
// whatever a test hands it), and a hard 400 on the first real image a user
// sends — silent in exactly the "adding a format is a one-row edit" workflow
// this feature advertises. Four of the five MIMEs the registry declares today
// are images, and nothing else checked them.
//
// The fix when this fails is NOT to relax it. The adapter's image case must
// be able to emit the new format's wire shape first — which, for a format the
// SDK's enum does not carry, means an SDK release, not a registry row.
func TestEveryNativeImageMIMEIsAnSDKMediaType(t *testing.T) {
	require.NotEmpty(t, models.Anthropic, "the Anthropic model table is empty — this guard would pass by checking nothing")

	checked := 0
	for id, info := range models.Anthropic {
		for mime, blockType := range info.NativeInputMIMEs {
			if blockType != llm.NativeBlockImage {
				continue
			}
			checked++
			assert.True(t, sdkImageMediaTypes[mime],
				"model %q declares %q as a native image, but sdk.Base64ImageSourceMediaType has no such value — the adapter casts the MIME straight into that field, so the API answers 400 on the first real image. The adapter's image case needs an SDK media type for this format BEFORE it can be a registry row.", id, mime)
		}
	}
	require.NotZero(t, checked,
		"no model row declares a native image MIME — either the registry lost its image support or this guard is scanning the wrong thing; a guard that checks nothing reads as passing forever")
}
