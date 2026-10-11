package browsergate

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/authzed/openagentprimitives/pkg/tools/browserpolicy"
)

// policyStore holds the current PolicyDoc and hot-reloads it from disk when the
// file's mtime changes. A parse (or validation) failure on reload keeps the
// last good policy in force and logs — the gate never fails open to "no
// policy". All access to the cached doc and mtime is guarded by mu so the
// reload racing with concurrent decisions is race-detector clean.
type policyStore struct {
	path   string
	logger *slog.Logger

	mu      sync.RWMutex
	doc     browserpolicy.PolicyDoc
	modTime time.Time
}

// newPolicyStore reads and validates the initial policy. It fails closed: an
// unreadable file, malformed JSON, or an unrecognized mode is an error, so a
// gate never starts serving against a policy it could not load.
func newPolicyStore(path string, logger *slog.Logger) (*policyStore, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("browsergate: stat policy %q: %w", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("browsergate: read policy %q: %w", path, err)
	}
	doc, err := parsePolicy(data)
	if err != nil {
		return nil, err
	}
	return &policyStore{
		path:    path,
		logger:  logger,
		doc:     doc,
		modTime: info.ModTime(),
	}, nil
}

// parsePolicy unmarshals a PolicyDoc and rejects any mode the gate cannot
// enforce. An unknown mode is treated as a parse failure so it fails closed at
// startup (New errors) and is ignored on reload (the old policy stays).
func parsePolicy(data []byte) (browserpolicy.PolicyDoc, error) {
	var doc browserpolicy.PolicyDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, fmt.Errorf("browsergate: parse policy: %w", err)
	}
	if doc.Mode != browserpolicy.ModeAllowlist && doc.Mode != browserpolicy.ModeBlocklist {
		return doc, fmt.Errorf("browsergate: unknown policy mode %q", doc.Mode)
	}
	return doc, nil
}

// current returns the active policy, re-reading the file first if its mtime
// changed since the last load.
func (s *policyStore) current() browserpolicy.PolicyDoc {
	s.maybeReload()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.doc
}

// maybeReload stats the policy file and, when the mtime differs from the cached
// one, re-reads and re-parses it. Any failure along the way is logged and the
// previous policy is retained — reload never widens access to "allow all" or
// narrows to "deny all" on a transient read/parse error.
func (s *policyStore) maybeReload() {
	info, err := os.Stat(s.path)
	if err != nil {
		s.logger.Warn("browsergate: stat policy for reload failed; keeping current policy",
			"path", s.path, "err", err.Error())
		return
	}

	s.mu.RLock()
	unchanged := info.ModTime().Equal(s.modTime)
	s.mu.RUnlock()
	if unchanged {
		return
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		s.logger.Warn("browsergate: re-read policy failed; keeping current policy",
			"path", s.path, "err", err.Error())
		return
	}
	doc, err := parsePolicy(data)
	if err != nil {
		s.logger.Warn("browsergate: re-parse policy failed; keeping current policy",
			"path", s.path, "err", err.Error())
		return
	}

	s.mu.Lock()
	s.doc = doc
	s.modTime = info.ModTime()
	s.mu.Unlock()
	s.logger.Info("browsergate: policy reloaded", "path", s.path, "mode", doc.Mode, "sites", len(doc.Sites))
}
