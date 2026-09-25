package settingsui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"

	"golang.org/x/crypto/bcrypt"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/passwordkind"
)

// minSettingsPassword is the floor for a new admin password submitted
// through the settings UI's password-change form — checked BEFORE the
// current-password verify, so a too-short new password is rejected the same
// way regardless of whether the caller also got the current password wrong.
// Mirrors passwordkind's own minPasswordLength floor (unexported there, and
// this handler has no other reason to import that package), not a
// coincidence: both are the same single-user local-admin password policy.
const minSettingsPassword = 8

// configResponse is the GET /api/config wire shape: a redacted projection of
// the stored desktop.Config. It never carries the API key, the ngrok token,
// or the password hash — only masked/boolean projections of them (see
// maskKey) — so a caller can render the settings form without the secret
// ever reaching the browser.
type configResponse struct {
	Model struct {
		Provider     string `json:"provider"`
		APIKeyMasked string `json:"apiKeyMasked"` // "••••" + last 4, "" when unset
		Name         string `json:"name"`
	} `json:"model"`
	Providers           []string `json:"providers"`
	NgrokConfigured     bool     `json:"ngrokConfigured"`
	HealthNotifications bool     `json:"healthNotifications"`
	PasswordSet         bool     `json:"passwordSet"`
	ConfigPath          string   `json:"configPath"`
}

// configUpdateRequest is the PUT /api/config wire shape. An empty
// Model.APIKey or NgrokAuthToken means "keep the value already on disk" —
// there is no other way to signal "leave this field alone" without echoing
// the secret back, which is exactly what GET never does. Password is
// optional: present only when the user is actually changing it.
type configUpdateRequest struct {
	Model struct {
		Provider string `json:"provider"`
		APIKey   string `json:"apiKey"` // "" = keep existing
		Name     string `json:"name"`
	} `json:"model"`
	NgrokAuthToken      string `json:"ngrokAuthToken"` // "" = keep existing
	ClearNgrok          bool   `json:"clearNgrok"`
	HealthNotifications bool   `json:"healthNotifications"`
	Password            *struct {
		Current string `json:"current"`
		New     string `json:"new"`
	} `json:"password,omitempty"`
}

// fieldOutcome reports what happened to one changed field group as a result
// of a PUT: "applied" (took effect immediately), "next-start" (saved, takes
// effect the next time the cluster comes up), or "failed" (saved, but the
// immediate apply attempt errored — see Message).
type fieldOutcome struct {
	Field   string `json:"field"`   // "model" | "password" | "ngrok" | "healthNotifications"
	Outcome string `json:"outcome"` // "applied" | "next-start" | "failed"
	Message string `json:"message,omitempty"`
}

// configUpdateResponse is the PUT /api/config wire shape: one fieldOutcome
// per field GROUP that actually changed. A PUT that changes nothing gets an
// empty (never nil) Outcomes list back.
type configUpdateResponse struct {
	Outcomes []fieldOutcome `json:"outcomes"`
}

// maskKey redacts an API key for display: "" for unset, "••••" alone for a
// key too short to safely reveal a suffix of (<=4 chars — revealing a 4-char
// suffix of a 4-char key is revealing the whole key), otherwise "••••" plus
// the last 4 characters.
func maskKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 4 {
		return "••••"
	}
	return "••••" + k[len(k)-4:]
}

// handleConfigGet serves the redacted projection of the stored
// desktop.Config. See configResponse's doc for the redaction contract.
func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg, err := desktop.Load(s.deps.SupportDir)
	if err != nil {
		s.deps.Logf("settingsui: load config: %v", err)
		http.Error(w, "settings: failed to load stored config", http.StatusInternalServerError)
		return
	}

	var resp configResponse
	resp.Model.Provider = cfg.Model.Provider
	resp.Model.APIKeyMasked = maskKey(cfg.Model.APIKey)
	resp.Model.Name = cfg.Model.Name
	resp.Providers = desktop.ModelProviderNames()
	resp.NgrokConfigured = cfg.Ngrok != nil && cfg.Ngrok.AuthToken != ""
	resp.HealthNotifications = cfg.HealthNotificationsEnabled()
	resp.PasswordSet = cfg.AdminPasswordHash != ""
	resp.ConfigPath = filepath.Join(s.deps.SupportDir, "config.json")

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.deps.Logf("settingsui: encode config response: %v", err)
	}
}

// ngrokEqual reports whether two (possibly nil) NgrokConfig values carry the
// same auth token — the only field that participates in change detection.
func ngrokEqual(a, b *desktop.NgrokConfig) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.AuthToken == b.AuthToken
}

// handleConfigPut applies a settings-form submission per the task brief's
// behavior contract:
//
//  1. decode the request
//  2. build the updated Config (empty apiKey/ngrokAuthToken keep the stored
//     value; the password block is optional)
//  3. verify the current password if one is being changed — 403 and nothing
//     saved on mismatch
//  4. Validate() the updated Config — 400 and nothing saved on failure
//  5. Save it — 500 and no apply attempted on failure
//  6. apply-on-save: call ReapplyConfig AT MOST ONCE, only when the cluster
//     is running and the model or password group changed; report per-field
//     outcomes for whatever changed
func (s *Server) handleConfigPut(w http.ResponseWriter, r *http.Request) {
	var req configUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("settings: decode request body: %v", err), http.StatusBadRequest)
		return
	}

	cur, err := desktop.Load(s.deps.SupportDir)
	if err != nil {
		s.deps.Logf("settingsui: load config: %v", err)
		http.Error(w, "settings: failed to load stored config", http.StatusInternalServerError)
		return
	}

	updated := cur
	updated.Model.Provider = req.Model.Provider
	updated.Model.Name = req.Model.Name
	if req.Model.APIKey != "" {
		updated.Model.APIKey = req.Model.APIKey
	}

	switch {
	case req.ClearNgrok:
		updated.Ngrok = nil
	case req.NgrokAuthToken != "":
		updated.Ngrok = &desktop.NgrokConfig{AuthToken: req.NgrokAuthToken}
	}

	healthEnabled := req.HealthNotifications
	updated.HealthNotifications = &healthEnabled

	if req.Password != nil {
		if len(req.Password.New) < minSettingsPassword {
			http.Error(w, fmt.Sprintf("settings: password must be at least %d characters", minSettingsPassword), http.StatusBadRequest)
			return
		}
		if cur.AdminPasswordHash != "" {
			if err := bcrypt.CompareHashAndPassword([]byte(cur.AdminPasswordHash), []byte(req.Password.Current)); err != nil {
				http.Error(w, "settings: current password is incorrect", http.StatusForbidden)
				return
			}
		}
		newHash, err := passwordkind.HashPassword(req.Password.New)
		if err != nil {
			s.deps.Logf("settingsui: hash new password: %v", err)
			http.Error(w, "settings: failed to hash new password", http.StatusInternalServerError)
			return
		}
		updated.AdminPasswordHash = newHash
	}

	if err := updated.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := updated.Save(s.deps.SupportDir); err != nil {
		s.deps.Logf("settingsui: save config: %v", err)
		http.Error(w, "settings: failed to save config", http.StatusInternalServerError)
		return
	}

	modelChanged := cur.Model.Provider != updated.Model.Provider ||
		cur.Model.APIKey != updated.Model.APIKey ||
		cur.Model.Name != updated.Model.Name
	passwordChanged := cur.AdminPasswordHash != updated.AdminPasswordHash
	ngrokChanged := !ngrokEqual(cur.Ngrok, updated.Ngrok)
	healthChanged := cur.HealthNotificationsEnabled() != updated.HealthNotificationsEnabled()

	outcomes := []fieldOutcome{}

	if modelChanged || passwordChanged {
		switch {
		case !s.deps.State().Running:
			if modelChanged {
				outcomes = append(outcomes, fieldOutcome{Field: "model", Outcome: "next-start"})
			}
			if passwordChanged {
				outcomes = append(outcomes, fieldOutcome{Field: "password", Outcome: "next-start"})
			}
		case s.deps.ReapplyConfig == nil:
			// Running but no seam wired: never silently claim success. Save
			// already stands (config.json holds the new values), so this is
			// reported the same way a real apply failure would be.
			msg := "settings: reapply not configured; config saved and will take effect at next start"
			s.deps.Logf("%s", msg)
			if modelChanged {
				outcomes = append(outcomes, fieldOutcome{Field: "model", Outcome: "failed", Message: msg})
			}
			if passwordChanged {
				outcomes = append(outcomes, fieldOutcome{Field: "password", Outcome: "failed", Message: msg})
			}
		default:
			scope := ReapplyScope{Model: modelChanged, Password: passwordChanged}
			if applyErr := s.deps.ReapplyConfig(r.Context(), updated, scope); applyErr != nil {
				msg := fmt.Sprintf("apply failed: %v (config saved — will take effect at next start)", applyErr)
				s.deps.Logf("settingsui: ReapplyConfig: %v", applyErr)
				if modelChanged {
					outcomes = append(outcomes, fieldOutcome{Field: "model", Outcome: "failed", Message: msg})
				}
				if passwordChanged {
					outcomes = append(outcomes, fieldOutcome{Field: "password", Outcome: "failed", Message: msg})
				}
			} else {
				if modelChanged {
					outcomes = append(outcomes, fieldOutcome{Field: "model", Outcome: "applied"})
				}
				if passwordChanged {
					outcomes = append(outcomes, fieldOutcome{Field: "password", Outcome: "applied"})
				}
			}
		}
	}

	if ngrokChanged {
		// The tunnel env is only consumed at install time (initLocalHook is a
		// stub today) — there is no live-apply path for it yet, so it is
		// always "next-start" regardless of whether the cluster is running.
		outcomes = append(outcomes, fieldOutcome{Field: "ngrok", Outcome: "next-start"})
	}

	if healthChanged {
		if s.deps.OnHealthNotificationsSaved != nil {
			s.deps.OnHealthNotificationsSaved(updated.HealthNotificationsEnabled())
			outcomes = append(outcomes, fieldOutcome{Field: "healthNotifications", Outcome: "applied"})
		} else {
			// No sync seam wired: never silently claim success. The save
			// stands (config.json holds the new value for next start), so
			// this mirrors the ReapplyConfig-nil branch above.
			msg := "settings: health notification sync not configured; config saved and will take effect at next start"
			s.deps.Logf("%s", msg)
			outcomes = append(outcomes, fieldOutcome{Field: "healthNotifications", Outcome: "failed", Message: msg})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(configUpdateResponse{Outcomes: outcomes}); err != nil {
		s.deps.Logf("settingsui: encode config update response: %v", err)
	}
}
