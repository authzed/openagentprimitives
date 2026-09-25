package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSpiceDBPermission_CarriesATitle(t *testing.T) {
	p := SpiceDBPermission{Name: "push", Expr: "owner", Title: "Push commits to the repository"}
	assert.Equal(t, "Push commits to the repository", p.Title,
		"a permission declares the phrase a human reads instead of its handle")
}

func TestSpiceDBPermission_TitleIsOptional(t *testing.T) {
	// Absent is the ordinary case until someone writes one; the card
	// detokenizes the handle rather than showing nothing.
	p := SpiceDBPermission{Name: "read", Expr: "owner"}
	assert.Empty(t, p.Title)
}
