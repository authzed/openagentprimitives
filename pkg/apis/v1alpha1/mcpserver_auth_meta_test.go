package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMCPServerAuthMetaFields(t *testing.T) {
	a := MCPServerAuth{Title: "Linear", Description: "read your Linear issues"}
	assert.Equal(t, "Linear", a.Title)
	assert.Equal(t, "read your Linear issues", a.Description)
}
