package validator

import (
	"errors"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/authz/validator/core"
	"github.com/authzed/openagentprimitives/pkg/tools/redact"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/effect"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/parser"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/parser/builtin"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// Invocation is the concrete thing being validated.
type Invocation struct {
	Command       string
	Argv          []string
	Env           map[string]string
	Cwd           string
	BinaryVersion string
	// Config is the installer-bound AgentClass.spec.config exposed to constraint
	// (and exception) CEL as the `config` root. Nil is treated as empty — a
	// constraint that reads a `config.X` the caller did not supply then fails
	// closed (a missing key is a CEL error, which denies the call).
	Config map[string]any
}

// Check runs the validator pipeline and returns a Decision.
// The error return is reserved for internal failures (currently:
// unknown parser, malformed CEL/version-range syntax inside the
// toolkit/spec themselves — i.e. authoring bugs, not policy denies).
//
// Phases (each helper lives in phases.go):
//
//	parse → revision → binaryVersion → allowSubcommands
//	→ structured (rules + exceptions) → CEL constraints → allow.
//
// Each phase short-circuits the pipeline by populating Decision.FailedOn;
// sensitive values registered up-front are scrubbed from every
// user-visible string by finalize.
func Check(tk *toolkit.Toolkit, sp *spec.Spec, inv Invocation) (*Decision, error) {
	d := &Decision{Redactions: map[string]redact.Descriptor{}}
	r := redact.New()

	// Register sensitive env values up-front; flag and positional values are
	// registered after parse below (their names live on the parsed Call).
	registerSensitiveValues(r, tk, sp, inv)

	// Parse phase.
	p, err := pickParser(tk)
	if err != nil {
		return nil, err
	}
	call, parseErr := p.Parse(tk, inv.Argv)
	if parseErr != nil {
		addTrace(d, "parse", StatusFail, parseErr.Error(), "")
		failedOn(d, "parse", parseErr.Error())
		return finalize(d, r), nil
	}
	registerSensitiveFlagValues(r, tk, sp, call)
	registerSensitivePositionals(r, sp, call)
	d.Parsed = &ParsedCall{
		Subcommand:     call.Subcommand,
		SubcommandPath: append([]string(nil), call.SubcommandPath...),
		Flags:          core.CopyAnyMap(call.Flags),
		Positional:     core.CopyAnyMap(call.Positional),
		Tail:           append([]string(nil), call.Tail...),
		Argv:           append([]string(nil), call.Argv...),
	}
	addTrace(d, "parse", StatusPass, fmt.Sprintf("matched [%s]", call.Subcommand), "")

	// Sequential phases. Each returns false to short-circuit the pipeline.
	if ok := checkRevision(d, tk, sp); !ok {
		return finalize(d, r), nil
	}
	if ok, err := checkBinaryVersion(d, tk, sp, inv); err != nil {
		return nil, err
	} else if !ok {
		return finalize(d, r), nil
	}
	if ok := checkAllowSubcommands(d, sp, call); !ok {
		return finalize(d, r), nil
	}

	// Resolve effects for structured + CEL phases.
	sc := findSubcommand(tk, call.SubcommandPath)
	if sc == nil {
		// findSubcommand returns nil when no toolkit subcommand declares this
		// exact path. A well-formed parser only produces paths present in
		// tk.Subcommands, so reaching here means the toolkit/parser disagree
		// (an authoring bug) — fail closed rather than nil-deref sc.Effects.
		return nil, fmt.Errorf("no toolkit subcommand for path %v", call.SubcommandPath)
	}
	resolved := effect.Resolve(sc.Effects, call, inv.Env, inv.Cwd)
	// resourceID is the canonical instance id this call names (or "" for a
	// local/sentinel op); exposed as call.resourceId so a constraint can gate on
	// WHICH resource a call reaches, resolved through the same machinery the
	// authz permission path uses.
	resourceID := resolveResourceID(tk, call)
	callMap := callAsMap(call, inv, resolved, resourceID)

	if ok, err := checkStructured(d, sp, resolved, callMap, inv.Config); err != nil {
		return nil, err
	} else if !ok {
		return finalize(d, r), nil
	}
	if ok, err := checkConstraints(d, sp, callMap, inv.Config); err != nil {
		return nil, err
	} else if !ok {
		return finalize(d, r), nil
	}

	d.Allow = true
	d.Reason = "allowed"
	return finalize(d, r), nil
}

// pickParser selects the parser implementation declared by the toolkit.
func pickParser(tk *toolkit.Toolkit) (parser.Parser, error) {
	switch tk.Parser.Kind {
	case "declarative":
		return &parser.Declarative{}, nil
	case "builtin":
		p, ok := builtin.Get(tk.Parser.Name)
		if !ok {
			return nil, fmt.Errorf("builtin parser %q not registered", tk.Parser.Name)
		}
		return p, nil
	default:
		return nil, errors.New(`toolkit parser.kind must be "declarative" or "builtin"`)
	}
}
