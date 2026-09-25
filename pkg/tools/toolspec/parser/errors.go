package parser

import (
	"errors"
	"fmt"
)

// ErrParse is the sentinel error for every parse failure.
// Use errors.Is(err, ErrParse) to detect any parse failure;
// type-assert to *ParseError for the specific Kind.
var ErrParse = errors.New("parse error")

type ErrorKind string

const (
	KindUnknownSubcommand ErrorKind = "UnknownSubcommand"
	KindUnknownFlag       ErrorKind = "UnknownFlag"
	KindArgCountMismatch  ErrorKind = "ArgCountMismatch"
	KindFlagTypeMismatch  ErrorKind = "FlagTypeMismatch"
	KindMissingFlagValue  ErrorKind = "MissingFlagValue"
	KindEnumValueInvalid  ErrorKind = "EnumValueInvalid"
)

type ParseError struct {
	Kind   ErrorKind
	Detail string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("%s: %s", e.Kind, e.Detail)
}

func (e *ParseError) Unwrap() error { return ErrParse }
