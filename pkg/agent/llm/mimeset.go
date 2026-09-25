package llm

// Native block types. A MIME's block type is registry data, carried in
// MIMESet alongside the MIME itself, so no consumer ever has to infer it by
// pattern-matching a MIME string.
const (
	NativeBlockImage    = "image"
	NativeBlockDocument = "document"
)

// MIMESet maps each MIME type a model accepts as native message content to the
// ContentBlock type it must be carried as. The zero value is usable and empty;
// an unknown model yields it rather than an error, because native support is
// advisory — the caller falls back to the extracted-text path.
//
// It maps to a block type rather than being a plain set so the runner can build
// a block WITHOUT knowing MIME families. A strings.HasPrefix(mime, "image/") in
// the runner would be a second opinion the registry could not override, and
// would break the property that adding a format is a one-row edit here.
type MIMESet map[string]string

// NewMIMESet builds a MIMESet from a MIME → native block type mapping.
func NewMIMESet(byMIME map[string]string) MIMESet {
	set := make(MIMESet, len(byMIME))
	for m, blockType := range byMIME {
		set[m] = blockType
	}
	return set
}

// Has reports whether the model accepts this MIME as native content.
func (s MIMESet) Has(mime string) bool {
	_, ok := s[mime]
	return ok
}

// BlockType returns the ContentBlock type this MIME is carried as, or "" if
// the MIME is not natively accepted.
func (s MIMESet) BlockType(mime string) string { return s[mime] }
