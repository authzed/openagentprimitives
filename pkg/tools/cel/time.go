package cel

import (
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

func init() {
	Register(Helper{
		Name:      "now",
		Signature: "now() -> timestamp",
		Doc: "Current wall-clock time, evaluated fresh each call. Use for 'within the last N days/hours' " +
			"intents; combine with CEL's built-in timestamp() and duration() to compare. CEL duration() " +
			"does NOT accept 'd' — express days in hours: 24h = 1 day, 168h = 7 days, 720h = 30 days.",
		Example:    `now() - timestamp(args.created_at) < duration("168h")  // last 7 days`,
		Scope:      ScopeToolspec | ScopeMCP,
		EnvOptions: []cel.EnvOption{nowFunc()},
	})
}

func nowFunc() cel.EnvOption {
	return cel.Function("now",
		cel.Overload("now",
			[]*cel.Type{},
			cel.TimestampType,
			cel.FunctionBinding(func(_ ...ref.Val) ref.Val {
				return types.Timestamp{Time: time.Now()}
			}),
		),
	)
}
