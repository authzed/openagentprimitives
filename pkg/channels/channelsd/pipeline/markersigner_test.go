package pipeline

import (
	"bytes"
	"crypto/ed25519"

	"github.com/authzed/openagentprimitives/pkg/agent/restartmarker"
)

// testMarkerSigner is the connector identity the pipeline signs restart markers
// as in tests. The operator verifies status.pendingRestart before acting on it,
// and the pipeline refuses to write an unsigned marker, so every test that
// drives a fork trigger needs one wired.
var testMarkerSigner = restartmarker.NewSigner(
	ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x3d}, ed25519.SeedSize)),
	restartmarker.Publisher,
)
