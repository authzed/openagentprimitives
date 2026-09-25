package sensitive

// SensitiveValue wraps secret bytes so a raw string/[]byte token can't be
// accidentally logged, formatted, or serialized. The bytes are reachable ONLY
// via UnderlyingValue(); every other rendering path yields "[REDACTED]".
type SensitiveValue struct {
	v []byte // unexported: no direct field access from other packages
}

// NewSensitiveValue wraps b. The caller must not retain/mutate b afterward.
func NewSensitiveValue(b []byte) SensitiveValue { return SensitiveValue{v: b} }

// UnderlyingValue returns the wrapped bytes. This is the ONLY exit for the
// secret material — call it as late as possible, at the actual injection site.
func (s SensitiveValue) UnderlyingValue() []byte { return s.v }

// IsEmpty reports whether the wrapped value has zero length.
func (s SensitiveValue) IsEmpty() bool { return len(s.v) == 0 }

func (s SensitiveValue) String() string               { return "[REDACTED]" }
func (s SensitiveValue) GoString() string             { return "[REDACTED]" }
func (s SensitiveValue) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }
