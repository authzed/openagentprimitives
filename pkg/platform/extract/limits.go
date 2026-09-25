package extract

import (
	"encoding/json"
	"fmt"
)

// archivesConfig is the JSON an AgentClass writes under
// `capabilities.attachments.archives`.
//
// Pointers, not values, so "omitted" is distinguishable from "zero". A plain
// int64 cannot tell the two apart, and reading an omitted field as 0 would
// silently mean UNLIMITED to every `n > lim` check downstream — the one way
// this whole type could fail open.
//
// MaxWallClock is deliberately absent: it bounds how long extractord will
// spend on one archive, which protects the POD from a slow-decompress bomb.
// That is not an AgentClass's concern and not its to relax.
type archivesConfig struct {
	MaxUncompressedTotal *int64 `json:"maxUncompressedTotal,omitempty"`
	MaxMemberBytes       *int64 `json:"maxMemberBytes,omitempty"`
	MaxMembers           *int   `json:"maxMembers,omitempty"`
	MaxRatio             *int   `json:"maxRatio,omitempty"`
}

// attachmentsConfig is the enclosing capability config. Only the archives
// stanza is read here; unknown siblings are ignored so a future capability
// option does not have to touch this parser.
type attachmentsConfig struct {
	Archives *archivesConfig `json:"archives,omitempty"`
}

// ParseLimits resolves the archive bounds an AgentClass asks for against the
// ceiling a deployment will actually serve.
//
// CLAMPS, never substitutes. A class TIGHTENING a bound is a legitimate
// operator choice; a class loosening one past the ceiling is not, and an
// unclamped path would let an AgentClass edit raise the limit on a shared
// extractord that many classes use. Every value returned is therefore
// min(requested, ceiling), and an omitted value is the ceiling itself.
//
// Called from two places on purpose: the attachments capability parses it at
// apply time so a malformed config fails where an operator sees it, and the
// operator parses it again at upload time because that is where the value is
// actually used. One parser, so the two can never disagree about what a config
// means.
func ParseLimits(raw []byte, ceiling Limits) (Limits, error) {
	if err := ceiling.validate(); err != nil {
		return Limits{}, fmt.Errorf("extract: ceiling is unusable: %w", err)
	}
	out := ceiling

	if len(raw) == 0 {
		return out, nil
	}
	var cfg attachmentsConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Limits{}, fmt.Errorf("extract: parsing archive limits: %w", err)
	}
	if cfg.Archives == nil {
		return out, nil
	}
	a := cfg.Archives

	if a.MaxUncompressedTotal != nil {
		v, err := positive64("maxUncompressedTotal", *a.MaxUncompressedTotal)
		if err != nil {
			return Limits{}, err
		}
		out.MaxUncompressedTotal = min64(v, ceiling.MaxUncompressedTotal)
	}
	if a.MaxMemberBytes != nil {
		v, err := positive64("maxMemberBytes", *a.MaxMemberBytes)
		if err != nil {
			return Limits{}, err
		}
		out.MaxMemberBytes = min64(v, ceiling.MaxMemberBytes)
	}
	if a.MaxMembers != nil {
		v, err := positiveInt("maxMembers", *a.MaxMembers)
		if err != nil {
			return Limits{}, err
		}
		out.MaxMembers = minInt(v, ceiling.MaxMembers)
	}
	if a.MaxRatio != nil {
		v, err := positiveInt("maxRatio", *a.MaxRatio)
		if err != nil {
			return Limits{}, err
		}
		out.MaxRatio = minInt(v, ceiling.MaxRatio)
	}
	return out, nil
}

// validate refuses a ceiling with a non-positive field. A zero ceiling cannot
// clamp anything — min(requested, 0) is 0, which every bound check then reads
// as unlimited — so it is refused rather than trusted.
func (l Limits) validate() error {
	switch {
	case l.MaxUncompressedTotal <= 0:
		return fmt.Errorf("maxUncompressedTotal must be positive, got %d", l.MaxUncompressedTotal)
	case l.MaxMemberBytes <= 0:
		return fmt.Errorf("maxMemberBytes must be positive, got %d", l.MaxMemberBytes)
	case l.MaxMembers <= 0:
		return fmt.Errorf("maxMembers must be positive, got %d", l.MaxMembers)
	case l.MaxRatio <= 0:
		return fmt.Errorf("maxRatio must be positive, got %d", l.MaxRatio)
	case l.MaxWallClock <= 0:
		return fmt.Errorf("maxWallClock must be positive, got %s", l.MaxWallClock)
	}
	return nil
}

// positive64 rejects a non-positive configured value rather than reading it as
// "unlimited" or quietly substituting the ceiling: the first fails open, and
// the second hides a typo in a security bound.
func positive64(field string, v int64) (int64, error) {
	if v <= 0 {
		return 0, fmt.Errorf("extract: archives.%s must be positive, got %d", field, v)
	}
	return v, nil
}

func positiveInt(field string, v int) (int, error) {
	if v <= 0 {
		return 0, fmt.Errorf("extract: archives.%s must be positive, got %d", field, v)
	}
	return v, nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
