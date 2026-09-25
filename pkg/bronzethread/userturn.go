package bronzethread

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

// BundleAttachment is one file riding on an authored user turn.
//
// File is a path RELATIVE to the bundle directory. The driver reads it and
// serves the bytes through the fake kind's AttachmentFetcher, so the whole
// ingestion path — gate, fetch, upload, extract, manifest, notice — runs for
// real rather than being simulated at the seam.
type BundleAttachment struct {
	// Filename is what the user appears to have attached. Untrusted-shaped on
	// purpose: a bundle may name a file the way a hostile upload would, since
	// sanitization is one of the things worth covering.
	Filename string `json:"filename"`
	// MIME is what the transport reports. A HINT, exactly as in production —
	// the pipeline must not trust it over the bytes.
	MIME string `json:"mime,omitempty"`
	// File is the fixture path, relative to the bundle directory. Mutually
	// exclusive with ZipFrom.
	File string `json:"file,omitempty"`

	// ZipFrom names a DIRECTORY, relative to the bundle directory, that the
	// driver zips at load time to produce this attachment's bytes.
	//
	// Fixtures are built rather than committed because a binary archive in
	// testdata cannot be reviewed: nobody can tell whether it contains what
	// its filename claims, and a zip bomb committed by accident is a landmine
	// in the repo. The source files stay plain text a reader can open.
	ZipFrom string `json:"zipFrom,omitempty"`
}

// UserTurn is one authored human message.
//
// It unmarshals from EITHER a bare JSON string or an object. Every bundle
// written before attachments existed uses the string form, and none of them
// should have to change: migrating every fixture to add an optional field
// would be all risk and no benefit, and a fixture edited only to satisfy a
// schema is a fixture nobody re-reads.
type UserTurn struct {
	Text        string             `json:"text"`
	Attachments []BundleAttachment `json:"attachments,omitempty"`
}

// UnmarshalJSON accepts the bare-string and object forms.
func (u *UserTurn) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if strings.HasPrefix(trimmed, `"`) {
		return json.Unmarshal(b, &u.Text)
	}
	// A named alias, so unmarshalling the object form does not recurse back
	// into this method.
	type plain UserTurn
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	for i, a := range p.Attachments {
		if a.Filename == "" {
			return fmt.Errorf("bronzethread: userTurns[].attachments[%d]: filename is required", i)
		}
		switch {
		case a.File == "" && a.ZipFrom == "":
			return fmt.Errorf("bronzethread: userTurns[].attachments[%d] (%q): one of file or zipFrom is required — there are no bytes to serve without it", i, a.Filename)
		case a.File != "" && a.ZipFrom != "":
			return fmt.Errorf("bronzethread: userTurns[].attachments[%d] (%q): file and zipFrom are mutually exclusive", i, a.Filename)
		case a.File != "" && !safeFixturePath(a.File):
			return fmt.Errorf("bronzethread: userTurns[].attachments[%d] (%q): file %q must stay inside the bundle directory", i, a.Filename, a.File)
		case a.ZipFrom != "" && !safeFixturePath(a.ZipFrom):
			return fmt.Errorf("bronzethread: userTurns[].attachments[%d] (%q): zipFrom %q must stay inside the bundle directory", i, a.Filename, a.ZipFrom)
		}
	}
	*u = UserTurn(p)
	return nil
}

// safeFixturePath reports whether p stays inside the bundle directory. A
// fixture is not a place to reach into the host filesystem, and a bundle is
// authored content that a reviewer should be able to trust at a glance.
//
// Rejects rather than cleans: a rewritten path disagrees with what the bundle
// says it reads, which is worse than a loud refusal.
func safeFixturePath(p string) bool {
	if p == "" || path.IsAbs(p) || strings.ContainsRune(p, 0) {
		return false
	}
	for _, seg := range strings.Split(path.Clean(p), "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}
