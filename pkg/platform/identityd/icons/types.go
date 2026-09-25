package icons

import "time"

// Entry is one cached icon: a real fetched favicon, or the deterministic SVG
// fallback when discovery found none.
type Entry struct {
	// Bytes is the icon body served verbatim.
	Bytes []byte
	// ContentType is the response Content-Type, taken from upstream or "image/svg+xml".
	ContentType string
	// ETag is the quoted validator for If-None-Match; empty means no 304 is possible.
	ETag string
	// FetchedAt is when Cache.Put admitted the entry; expiry is measured from it.
	FetchedAt time.Time
	// Negative marks the fallback SVG, which gets the shorter TTL and max-age.
	Negative bool
}
