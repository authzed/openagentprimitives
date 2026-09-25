package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveWebDevURL(t *testing.T) {
	assert.Equal(t, "http://localhost:5173", resolveWebDevURL(""), "empty -> default")
	assert.Equal(t, "https://dev.example", resolveWebDevURL("https://dev.example"))
}
