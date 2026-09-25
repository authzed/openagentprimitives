package authz

import (
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// spicedbUserIDLib registers `spicedb_user_id(email) -> string` —
// thin wrapper over identity.EmailReference(...).Canonical(). The email→canonical
// encoding is opaque to spec authors; this exposes the helper
// without leaking the base64-lowercase details.
type spicedbUserIDLib struct{}

func (spicedbUserIDLib) CompileOptions() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Function("spicedb_user_id",
			cel.Overload("spicedb_user_id_string",
				[]*cel.Type{cel.StringType}, cel.StringType,
				cel.UnaryBinding(func(arg ref.Val) ref.Val {
					email, ok := arg.Value().(string)
					if !ok {
						return types.NewErr("spicedb_user_id: arg must be string, got %T", arg.Value())
					}
					if email == "" {
						return types.NewErr("spicedb_user_id: email must be non-empty")
					}
					canon, err := identity.EmailReference(identity.Email(email)).Canonical()
					if err != nil {
						// Unreachable: EmailReference always carries the email.
						return types.NewErr("spicedb_user_id: %v", err)
					}
					// identity boundary: the CEL runtime is string-typed; the canonical is
					// returned as a types.String value here.
					return types.String(canon.String())
				}),
			),
		),
	}
}

func (spicedbUserIDLib) ProgramOptions() []cel.ProgramOption { return nil }
