package cloud

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNopReporter_SatisfiesReporterAndIsNonInteractive(t *testing.T) {
	var r Reporter = NopReporter{}
	r.Step("x")
	r.OK("x")
	r.Info("x")
	r.Warn("x") // must not panic
	assert.False(t, r.Interactive())
}
