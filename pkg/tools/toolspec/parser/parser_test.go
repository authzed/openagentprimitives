package parser

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCallZeroValue(t *testing.T) {
	var c Call
	assert.Empty(t, c.Subcommand, "zero Subcommand should be empty")
	// Flags map is initialized by the parser; zero-value nil is fine here.
}

func TestParseError_FormatAndIs(t *testing.T) {
	e := &ParseError{Kind: KindUnknownSubcommand, Detail: "foo"}
	assert.NotEmpty(t, e.Error(), "ParseError.Error() should not be empty")
	assert.True(t, errors.Is(e, ErrParse), "errors.Is(&ParseError{}, ErrParse)")
}
