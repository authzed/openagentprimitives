package httpsrv

import "net/http"

// maxJSONBody bounds every JSON request body this server decodes.
//
// Eight routes decoded straight from r.Body with no cap while a sibling in this
// same package (inbound_asset.go's MaxInboundAssetBytes) bounded its own. The
// server's hardening deliberately leaves ReadTimeout at 0 for streaming, so
// nothing else stopped a slow, large body — and putEntry decodes a memory.Entry
// whose Content is a json.RawMessage, buffered WHOLE before any Kind,
// provenance or authorization logic runs.
//
// The reach is not a boundary crossing: a runner's own per-session token
// authorizes writes to its own session, so a ~400 MB body against a 256Mi
// operator is an OOM with nothing anomalous in the request at all.
//
// Sized for the largest legitimate entry — a transcript turn with inline
// content — with room to spare. An entry approaching this is a bug or an
// attack; either way the caller learns at the door instead of after the
// operator's memory is gone.
const maxJSONBody = 8 << 20 // 8 MiB

// limitJSONBody caps r.Body in place, so the caller's existing
// json.NewDecoder(r.Body) picks it up with no other change.
//
// MaxBytesReader rather than io.LimitReader: it makes the overrun an ERROR the
// decoder surfaces, where a LimitReader would silently truncate and hand the
// decoder a body that looks merely malformed.
func limitJSONBody(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
}
