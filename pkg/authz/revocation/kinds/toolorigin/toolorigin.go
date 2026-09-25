// Package toolorigin is the tool-origin revocation Invalidator. Revoking an
// Origin (mcpserver/<name>, sidecartoolbox/<ref>, toolkit/<name>) adds it to a
// live set the runner's PreToolCall revocation guard consults; the next call to
// any tool with that Origin() is denied.
package toolorigin

import (
	"sync"

	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
)

// Set is the live revoked-origin set + the tool-origin Invalidator.
type Set struct {
	mu sync.RWMutex
	m  map[string]struct{}
}

// New returns an empty Set.
func New() *Set { return &Set{m: map[string]struct{}{}} }

// Kind implements revocation.Invalidator.
func (s *Set) Kind() string { return "tool-origin" }

// Noun implements revocation.Invalidator.
//
// "A set of tools" rather than "an origin" or "a toolbox": an Origin groups
// every tool that came from one MCPServer, SidecarToolbox or toolkit, and what
// the reader loses is that group of abilities. The origin string itself
// (mcpserver/<name>) is operator vocabulary and stays out of the copy.
func (s *Set) Noun() string { return "a set of tools" }

// Invalidate marks origin revoked (idempotent).
func (s *Set) Invalidate(origin string) error {
	s.mu.Lock()
	s.m[origin] = struct{}{}
	s.mu.Unlock()
	return nil
}

// IsRevoked reports whether origin has been revoked.
func (s *Set) IsRevoked(origin string) bool {
	s.mu.RLock()
	_, ok := s.m[origin]
	s.mu.RUnlock()
	return ok
}

var _ revocation.Invalidator = (*Set)(nil)
