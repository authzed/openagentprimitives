package relsource

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Two different sources claiming the same relation is ambiguous ownership,
// a bug in the claims. Last-registration-wins would pick a winner nobody
// chose, and the loser's refusals would look like a guard malfunction rather
// than a duplicate claim.
func TestBuildIndex_PanicsWhenTwoSourcesClaimOneRelation(t *testing.T) {
	sources := []Source{
		{Name: "firstclaimer", Claims: []string{"shared_resource#owner"}},
		{Name: "secondclaimer", Claims: []string{"shared_resource#owner"}},
	}

	// Assert that it panics, and capture the message to verify it names both
	// sources and the disputed relation.
	var panicMsg string
	assert.Panics(t, func() {
		defer func() {
			if r := recover(); r != nil {
				panicMsg = r.(string)
				panic(r)
			}
		}()
		buildIndex(sources)
	})

	// Verify the panic message names both sources and the relation.
	assert.Contains(t, panicMsg, "firstclaimer", "panic message must name the first source")
	assert.Contains(t, panicMsg, "secondclaimer", "panic message must name the second source")
	assert.Contains(t, panicMsg, "shared_resource#owner", "panic message must name the disputed relation")
}

// The same relation claimed twice by the SAME source does not conflict.
func TestBuildIndex_AllowsSameRelationClaimedTwiceByTheSameSource(t *testing.T) {
	sources := []Source{
		{Name: "onesource", Claims: []string{"resource#relation", "resource#relation"}},
	}

	// Should not panic; the second identical claim just overwrites the first
	// in the owner map.
	assert.NotPanics(t, func() {
		buildIndex(sources)
	})
}

// A malformed claim with no '#' separator is a programmer error and must be
// caught at declaration.
func TestBuildIndex_PanicsOnMalformedClaimWithNoHashSeparator(t *testing.T) {
	sources := []Source{
		{Name: "badsource", Claims: []string{"malformed_no_hash"}},
	}

	// Assert that it panics, and capture the message to verify it names the
	// source and the malformed claim.
	var panicMsg string
	assert.Panics(t, func() {
		defer func() {
			if r := recover(); r != nil {
				panicMsg = r.(string)
				panic(r)
			}
		}()
		buildIndex(sources)
	})

	// Verify the panic message names both the source and the malformed claim.
	assert.Contains(t, panicMsg, "badsource", "panic message must name the source")
	assert.Contains(t, panicMsg, "malformed_no_hash", "panic message must name the malformed claim")
}

// Register panics on a duplicate Name, even when the Claims are identical.
// This test registers into the global registry and must run last in this file,
// so its fixture name stays unique to this test and doesn't conflict with
// other test registrations that might accumulate in the registry.
func TestRegister_PanicsOnADuplicateNameEvenWithIdenticalClaims(t *testing.T) {
	src := Source{Name: "unique_duplicate_test_source", Claims: []string{"widget#status"}}

	// Register once
	Register(src)

	// Attempt to register the exact same source again; must panic
	assert.Panics(t, func() {
		Register(src)
	})
}
