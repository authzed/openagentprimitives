package imageload_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/imageload"
)

func TestFor(t *testing.T) {
	const img = "spicebox-operator:dev"
	cases := []struct {
		name     string
		context  string
		wantDisp imageload.Disposition
		wantArgv []string
	}{
		{
			name:     "kind context: LocalLoad via kind load docker-image",
			context:  "kind-mycluster",
			wantDisp: imageload.LocalLoad,
			wantArgv: []string{"kind", "load", "docker-image", img, "--name", "mycluster"},
		},
		{
			name:     "k3d context: LocalLoad via k3d image import",
			context:  "k3d-mycluster",
			wantDisp: imageload.LocalLoad,
			wantArgv: []string{"k3d", "image", "import", img, "-c", "mycluster"},
		},
		{
			name:     "minikube context: LocalLoad via minikube image load",
			context:  "minikube",
			wantDisp: imageload.LocalLoad,
			wantArgv: []string{"minikube", "image", "load", img},
		},
		{
			name:     "oap-desktop: VMLoad, no argv (SSH path lives in cmd/oap)",
			context:  imageload.APContextName,
			wantDisp: imageload.VMLoad,
			wantArgv: nil,
		},
		{
			name:     "legacy ap-desktop: still VMLoad until the merge renames it",
			context:  imageload.LegacyAPContextName,
			wantDisp: imageload.VMLoad,
			wantArgv: nil,
		},
		{
			name:     "docker-desktop: NotNeeded, node shares the laptop daemon",
			context:  "docker-desktop",
			wantDisp: imageload.NotNeeded,
			wantArgv: nil,
		},
		{
			name:     "GKE context: NeedsRegistry, laptop cannot reach the node runtime",
			context:  "gke_my-proj_us-east1_prod",
			wantDisp: imageload.NeedsRegistry,
			wantArgv: nil,
		},
		{
			name:     "unrecognized context: NeedsRegistry (fail closed, not carry on)",
			context:  "my-prod-eks",
			wantDisp: imageload.NeedsRegistry,
			wantArgv: nil,
		},
		{
			name:     "empty context: NeedsRegistry",
			context:  "",
			wantDisp: imageload.NeedsRegistry,
			wantArgv: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := imageload.For(tc.context, img)
			assert.Equal(t, tc.wantDisp, got.Disposition)
			assert.Equal(t, tc.wantArgv, got.Argv)
		})
	}
}

// Every consumer's exhaustive switch names the disposition in its default-case
// error, so String must render each one — and must not silently report a new,
// unhandled disposition as one of the known four.
func TestDispositionString(t *testing.T) {
	assert.Equal(t, "not-needed", imageload.NotNeeded.String())
	assert.Equal(t, "local-load", imageload.LocalLoad.String())
	assert.Equal(t, "vm-load", imageload.VMLoad.String())
	assert.Equal(t, "needs-registry", imageload.NeedsRegistry.String())
	assert.Equal(t, "unknown", imageload.Disposition(99).String(),
		"an unclassified value must read as unknown, not as a known disposition")
}

// Argv is set for exactly one disposition; anything else with an argv would be
// a caller trap (a nil-argv exec).
func TestForArgvOnlyForLocalLoad(t *testing.T) {
	for _, kctx := range []string{"docker-desktop", imageload.APContextName, "gke_my-proj_us-east1_prod", ""} {
		p := imageload.For(kctx, "img:dev")
		assert.Empty(t, p.Argv, "context %q must carry no argv", kctx)
	}
}
