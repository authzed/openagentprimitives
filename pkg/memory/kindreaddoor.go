package memory

import "context"

// kindReadDoorKey marks a caller the per-kind READ door applies to.
type kindReadDoorKey struct{}

// WithKindReadDoor marks ctx as a credential the per-kind read door governs,
// naming who for the refusal log.
//
// It exists because the door was deciding "may this credential see this Kind"
// by asking a DIFFERENT question: whether the context carried a token SESSION.
// That mark is provenance — it binds an append-only entry's publisher to its
// writer — and the webd token, which acts on ANY session and belongs to none,
// never gets one. So the door skipped the one browser-facing component token
// entirely, and it could read pt_tag_content, and every other platform-only
// Kind, for any session in the cluster.
//
// A caller is now TOLD it is behind the door rather than inferred to be. That
// is the property worth having: a component token added later inherits webd's
// exemption by default under the old rule, simply by not carrying an unrelated
// mark, and under this one it inherits nothing — someone has to decide.
//
// TokenSessionFrom remains a SECOND trigger (see kindReadDoorSubject). Every
// per-session bearer is behind the door, and replacing the old trigger rather
// than adding to it would have reopened the door for every runner at once.
func WithKindReadDoor(ctx context.Context, who string) context.Context {
	return context.WithValue(ctx, kindReadDoorKey{}, who)
}

// kindReadDoorSubject reports whether the per-kind read door applies to this
// caller, and a name for the log. In-process platform callers carry neither
// mark and are unaffected — the operator reading its own platform-only records
// is the case the door must never touch.
func kindReadDoorSubject(ctx context.Context) (string, bool) {
	if who, ok := ctx.Value(kindReadDoorKey{}).(string); ok {
		return who, true
	}
	if sess, ok := TokenSessionFrom(ctx); ok {
		return sess.Namespace + "/" + sess.Name, true
	}
	return "", false
}
