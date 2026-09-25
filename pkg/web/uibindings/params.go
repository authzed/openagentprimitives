package uibindings

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
)

// ErrUnknownParam is returned when an args template references a parameter
// the request did not supply a value for.
var ErrUnknownParam = errors.New("uibindings: binding args reference an unknown parameter")

// ErrTemplateTooDeep is returned when an args template nests deeper than
// SubstituteParams tolerates. The server-side declaration is the only source
// of template shape (see SubstituteParams), so in practice this guards
// against a malformed or pathological declaration rather than a browser
// input — but substitute recurses once per nesting level, so an unbounded
// template would still be a stack-exhaustion path worth cutting off.
var ErrTemplateTooDeep = errors.New("uibindings: binding args template nests too deeply")

// paramKey is uicomponents' own placeholder key, not a second copy: the
// declaration package owns the args-template vocabulary, and its validator
// rejects a placeholder naming a parameter no control drives. Two independent
// spellings would mean an object one side treats as a placeholder and the
// other treats as ordinary data — validated as data and then never
// substituted, or vice versa.
const paramKey = uicomponents.ParamRefKey

// maxSubstituteDepth bounds substitute's recursion. Real declarations nest a
// handful of levels (an object with an array of filter clauses is deep by
// this codebase's standards); 64 is generous headroom above that while
// stopping well short of exhausting the goroutine stack.
const maxSubstituteDepth = 64

// SubstituteParams replaces every {"$param":"<name>"} object anywhere in args
// with the viewer's current string value for <name>, returning the rewritten
// JSON. Substitution is SERVER-side and the ONLY thing a browser may vary
// about a binding: the source, the ref, and the shape of this template all
// come from the server-side declaration, so a viewer can change WHICH time
// span a granted readonly tool is asked about but never WHICH tool is called
// or what else it is asked.
//
// A placeholder object must have exactly one key, "$param", with a string
// value; anything else is left untouched (it is ordinary data that happens to
// contain the key). A referenced-but-absent parameter returns ErrUnknownParam
// rather than substituting an empty string, because a silently-empty filter
// is a query that quietly means something different from what was declared.
//
// The substituted value is always the viewer's param value re-encoded as a
// JSON string leaf — never parsed or spliced as JSON — so no parameter value
// can introduce a key, replace an object or array, or otherwise change the
// template's structure; it can only ever fill a scalar slot the template
// itself marked as substitutable.
func SubstituteParams(args json.RawMessage, params map[string]string) (json.RawMessage, error) {
	if len(args) == 0 {
		return args, nil
	}
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		return nil, fmt.Errorf("uibindings: parse binding args: %w", err)
	}
	out, err := substitute(v, params, 0)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("uibindings: re-encode binding args: %w", err)
	}
	return b, nil
}

func substitute(v any, params map[string]string, depth int) (any, error) {
	if depth > maxSubstituteDepth {
		return nil, fmt.Errorf("%w: exceeds %d levels", ErrTemplateTooDeep, maxSubstituteDepth)
	}
	switch t := v.(type) {
	case map[string]any:
		// A placeholder is an object with EXACTLY one key, "$param", whose
		// value is a string. Anything else is ordinary data that happens to
		// contain the key, and is walked rather than rewritten.
		if len(t) == 1 {
			if name, ok := t[paramKey].(string); ok {
				val, present := params[name]
				if !present {
					return nil, fmt.Errorf("%w: %q", ErrUnknownParam, name)
				}
				return val, nil
			}
		}
		out := make(map[string]any, len(t))
		for k, child := range t {
			sub, err := substitute(child, params, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = sub
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, child := range t {
			sub, err := substitute(child, params, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = sub
		}
		return out, nil
	default:
		return v, nil
	}
}

// ErrRunnerUnreachable reports that a tool binding could not be resolved
// because the session's RUNNER did not answer — it is not subscribed, or it
// did not reply in time.
//
// It exists so the caller can tell this apart from every other resolve
// failure, because this one has a remedy nothing else has: an idle session has
// had its pods reaped, and the platform can wake it. Without a typed sentinel
// the handler would be matching on message text to decide whether to act,
// which is how a copy edit silently disables a recovery path.
var ErrRunnerUnreachable = errors.New("uibindings: the session runner did not answer")
