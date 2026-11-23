package httpsrv

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// AuditPath is the administrative, complete session-audit export route.
const AuditPath = "/audit/"
const auditExportLimit = 100000

// NewAuditHandler exports append-only evidence, including platform-only kinds.
// Its bearer is the operator's administrative debug credential, never a session
// or another component's credential. Possession is gated by Kubernetes Secret
// access, as for administrative artifact reads. It accepts no query filters.
func NewAuditHandler(mem memory.Memory, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !bearer || token == "" || provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			http.Error(w, "administrative audit credential required", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		path, ok := strings.CutPrefix(r.URL.Path, AuditPath)
		ns, name, hasName := strings.Cut(path, "/")
		if !ok || !hasName || len(validation.IsDNS1123Label(ns)) != 0 || len(validation.IsDNS1123Subdomain(name)) != 0 || r.URL.RawQuery != "" {
			http.Error(w, "expected /audit/{namespace}/{session} without filters", http.StatusBadRequest)
			return
		}
		scope := memory.Scope{Kind: "session", ID: ns + "/" + name}
		ctx := memory.WithCaller(memory.WithSystemApproval(memory.WithoutTokenSession(r.Context()), "operator:audit-export"), "system:operator")
		result, err := mem.Query(ctx, memory.Query{Scope: scope, Limit: auditExportLimit})
		if err != nil {
			log.FromContext(ctx).Info("audit export read failed", "scope", scope.ID, "error", err)
			http.Error(w, "audit export read failed", http.StatusInternalServerError)
			return
		}
		entries := make([]memory.Entry, 0, len(result.Entries))
		for _, entry := range result.Entries {
			if entry.Provenance != nil || memory.KindAppendOnly(entry.Kind) {
				entries = append(entries, entry)
			}
		}
		result.Entries = entries
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(result); err != nil {
			log.FromContext(ctx).Info("audit export response failed", "scope", scope.ID, "error", err)
		}
	})
}
