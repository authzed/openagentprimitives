package toolcheck_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
)

func TestZedTokenCache_GetAfterSet(t *testing.T) {
	c := toolcheck.NewZedTokenCache()
	c.Set("github_repo", "authzed/spicedb", "tok-1")
	assert.Equal(t, "tok-1", c.Get("github_repo", "authzed/spicedb"))
}

func TestZedTokenCache_GetMissingReturnsEmpty(t *testing.T) {
	c := toolcheck.NewZedTokenCache()
	assert.Equal(t, "", c.Get("github_repo", "nope"), "missing key should return empty string")
}

func TestZedTokenCache_LatestReturnsMostRecent(t *testing.T) {
	c := toolcheck.NewZedTokenCache()
	c.Set("a", "1", "tok-a")
	c.Set("b", "2", "tok-b")
	c.Set("a", "1", "tok-a-2")
	assert.Equal(t, "tok-a-2", c.Latest(), "Latest should return most-recent Set")
}

func TestZedTokenCache_ConcurrentSetIsSafe(t *testing.T) {
	c := toolcheck.NewZedTokenCache()
	done := make(chan struct{}, 4)
	for i := 0; i < 4; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 1000; j++ {
				c.Set("t", "i", "tok")
				_ = c.Get("t", "i")
				_ = c.Latest()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
}
