package settingsui

import (
	"fmt"
	"net/http"
)

// handleKubectlUse switches the current kubectl context to the desktop
// cluster via Deps.KubectlUse. A nil seam is a 501 — this platform/build
// never wired kubectl integration — rather than a silent no-op; a seam
// error is a 500 with the error surfaced verbatim (see AGENTS.md's
// no-silent-errors rule); success is 204 with no body.
func (s *Server) handleKubectlUse(w http.ResponseWriter, r *http.Request) {
	if s.deps.KubectlUse == nil {
		http.Error(w, "settings: kubectl integration is not available", http.StatusNotImplemented)
		return
	}
	if err := s.deps.KubectlUse(); err != nil {
		s.deps.Logf("settingsui: KubectlUse: %v", err)
		http.Error(w, fmt.Sprintf("settings: switch kubectl context: %v", err), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRevealConfig opens the platform file browser on the config file via
// Deps.RevealConfig. A nil seam is a 501, mirroring handleKubectlUse — the
// reveal itself has no failure mode worth reporting back (it's a
// fire-and-forget OS call), so success is always 204 once the seam runs.
func (s *Server) handleRevealConfig(w http.ResponseWriter, r *http.Request) {
	if s.deps.RevealConfig == nil {
		http.Error(w, "settings: reveal is not available", http.StatusNotImplemented)
		return
	}
	s.deps.RevealConfig()
	w.WriteHeader(http.StatusNoContent)
}

// handleRevealLogs opens the platform file browser on the log directory via
// Deps.RevealLogs. See handleRevealConfig's doc — same contract.
func (s *Server) handleRevealLogs(w http.ResponseWriter, r *http.Request) {
	if s.deps.RevealLogs == nil {
		http.Error(w, "settings: reveal is not available", http.StatusNotImplemented)
		return
	}
	s.deps.RevealLogs()
	w.WriteHeader(http.StatusNoContent)
}
