package admind

import (
	"net/http"
	"strings"
)

// accessAdmin is one platform-admin grant as projected for the Access panel.
type accessAdmin struct {
	// Subject is the raw SpiceDB subject ("user:<canonical>" or
	// "group:<id>#member"), shown verbatim.
	Subject string `json:"subject"`
	// Kind classifies Subject so the UI can icon it.
	Kind string `json:"kind"` // "user" | "group"
}

// accessStats is the DEGRADED SpiceDB stats panel. SpiceDB exposes no cheap
// schema-definition or relationship counts through the permissions API, so
// Available is false and the counts are nil — a "stats unavailable" panel,
// never an error.
type accessStats struct {
	// SchemaDefs is the schema-definition count; nil means unavailable.
	SchemaDefs *int `json:"schemaDefs"`
	// Relationships is the stored relationship count; nil means unavailable.
	Relationships *int `json:"relationships"`
	// Available reports whether the counts above mean anything.
	Available bool `json:"available"`
}

// accessResponse is the GET /admin/v1/access payload.
type accessResponse struct {
	// Admins is every platform-admin grant; empty means nobody holds it.
	Admins []accessAdmin `json:"admins"`
	// GrantCmd is the CLI an operator runs to add one — the console is
	// read-only, so this is shown rather than wired.
	GrantCmd string `json:"grantCmd"`
	// Stats is the SpiceDB counts block, currently always unavailable.
	Stats accessStats `json:"stats"`
}

// platformGrantAdminCmd is the (read-only) command an operator runs to add a
// platform admin. The admin UI is read-only, so it is shown, not wired.
const platformGrantAdminCmd = "oap platform grant-admin <email>"

// handleAccess serves the platform Access panel: who holds platform admin
// (from SpiceDB) plus a degraded schema/relationship stats block.
func (a *Admind) handleAccess(w http.ResponseWriter, r *http.Request) {
	subs, err := a.cfg.Checker.ListPlatformAdmins(r.Context())
	if err != nil {
		a.cfg.Logger.Info("admind: list platform admins failed", "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "list platform admins failed: "+err.Error())
		return
	}
	admins := make([]accessAdmin, 0, len(subs))
	for _, s := range subs {
		kind := "user"
		if strings.HasPrefix(s, "group:") {
			kind = "group"
		}
		admins = append(admins, accessAdmin{Subject: s, Kind: kind})
	}
	writeJSON(w, http.StatusOK, accessResponse{
		Admins:   admins,
		GrantCmd: platformGrantAdminCmd,
		Stats:    accessStats{Available: false},
	})
}
