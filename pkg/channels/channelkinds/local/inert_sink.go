// This kind's ONE Emit door, which is what makes inert.go's sweep a property of
// the kind rather than a habit of each sender file. An enumeration of sinks is
// the wrong instrument: it is correct only for the set that existed on the day
// it was written, and nothing about adding a render event, a sender, or a field
// to an existing payload makes anyone open inert.go.
//
// So the sweep sits at the boundary every render event must cross. local.Host
// is the sole construction point of every real sender this kind hands out, all
// built from one Host.sink, and NewHost wraps the caller's EventSink here once.
// The senders' sink field is typed *inertSink rather than EventSink, so
// `&localSender{sink: someRawSink}` does not COMPILE — that is what stops the
// door being routed around by a future sender, or by a test. See NOTES.md.
package local

import (
	"reflect"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// inertSink is the wrapper. It is stateless beyond its inner sink, so it
// inherits the EventSink contract's concurrency requirement for free: several
// senders push from different goroutines.
type inertSink struct{ inner EventSink }

// newInertSink wraps sink so everything emitted through it is made inert for a
// terminal. Wrapping an already-wrapped sink is harmless — the sweep is
// idempotent — but NewHost is the only production caller and wraps exactly
// once.
func newInertSink(sink EventSink) *inertSink { return &inertSink{inner: sink} }

func (s *inertSink) Emit(msg any) { s.inner.Emit(sweepRenderEvent(msg)) }

// sweepRenderEvent returns a copy of msg with every string and []byte it
// carries made inert for this kind's surface, reached through whatever nesting,
// pointers, slices and maps it happens to use.
//
// Sweeping by SHAPE rather than by a list of known message types is the point:
// the default for a field nobody has thought about is "swept", so forgetting is
// safe. The one exception, a tool's own output, is named in keepsToolSGR, where
// forgetting an entry loses colour on one field rather than opening a hole.
//
// The walk works on a copy: a sender still holding the payload it emitted sees
// its own value, not a swept one.
func sweepRenderEvent(msg any) any {
	if msg == nil {
		return nil
	}
	v := reflect.ValueOf(msg)
	out := reflect.New(v.Type()).Elem()
	out.Set(v)
	sweepValue(out, false, 0)
	return out.Interface()
}

// maxSweepDepth bounds the walk. Nothing real comes close (the deepest render
// event is four levels down), so this is only about a value pointing back at
// itself, which a shape-driven walk would follow until the stack ran out and
// took the whole TUI process with it.
//
// At the limit the value is ZEROED, not passed through: passing it through would
// hand the terminal a string the sweep never reached.
const maxSweepDepth = 24

// sweepValue rewrites v in place. keepSGR carries the tool-output exception
// down into whatever container holds the bytes.
//
// Containers are REBUILT rather than walked in place (a slice's backing array,
// a map, a pointee are all shared with the value the caller passed in), so the
// sweep is non-destructive to the emitting sender's own data.
func sweepValue(v reflect.Value, keepSGR bool, depth int) {
	if depth > maxSweepDepth {
		v.Set(reflect.Zero(v.Type()))
		return
	}
	switch v.Kind() {
	case reflect.String:
		if keepSGR {
			v.SetString(string(inertToolOutput([]byte(v.String()))))
			return
		}
		v.SetString(inertText(v.String()))

	case reflect.Pointer:
		if v.IsNil() {
			return
		}
		nv := reflect.New(v.Type().Elem())
		nv.Elem().Set(v.Elem())
		sweepValue(nv.Elem(), keepSGR, depth+1)
		v.Set(nv)

	case reflect.Interface:
		if v.IsNil() {
			return
		}
		inner := v.Elem()
		nv := reflect.New(inner.Type()).Elem()
		nv.Set(inner)
		sweepValue(nv, keepSGR, depth+1)
		v.Set(nv)

	case reflect.Slice:
		if v.IsNil() {
			return
		}
		// []byte (and any named type over it) is a LEAF, not a container of
		// numbers: it is a tool's output chunk, and splitting it per element
		// would decode nothing.
		if v.Type().Elem().Kind() == reflect.Uint8 {
			b := make([]byte, v.Len())
			reflect.Copy(reflect.ValueOf(b), v)
			if keepSGR {
				b = inertToolOutput(b)
			} else {
				b = []byte(inertText(string(b)))
			}
			v.Set(reflect.ValueOf(b).Convert(v.Type()))
			return
		}
		nv := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		reflect.Copy(nv, v)
		for i := 0; i < nv.Len(); i++ {
			sweepValue(nv.Index(i), keepSGR, depth+1)
		}
		v.Set(nv)

	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			sweepValue(v.Index(i), keepSGR, depth+1)
		}

	case reflect.Map:
		if v.IsNil() {
			return
		}
		nv := reflect.MakeMapWithSize(v.Type(), v.Len())
		for iter := v.MapRange(); iter.Next(); {
			k := reflect.New(v.Type().Key()).Elem()
			k.Set(iter.Key())
			sweepValue(k, keepSGR, depth+1)
			e := reflect.New(v.Type().Elem()).Elem()
			e.Set(iter.Value())
			sweepValue(e, keepSGR, depth+1)
			nv.SetMapIndex(k, e)
		}
		v.Set(nv)

	case reflect.Struct:
		t := v.Type()
		if !sweepableStruct(t) {
			return
		}
		for i := 0; i < t.NumField(); i++ {
			f := v.Field(i)
			if !f.CanSet() {
				// An unexported field. It cannot be READ by the consumer either
				// — cmd/oap is a different package — so a string hiding in one
				// can never reach the terminal.
				continue
			}
			sweepValue(f, keepSGR || keepsToolSGR(t, t.Field(i).Name), depth+1)
		}
	}
}

// apModulePath bounds the walk. A struct from outside this module is a leaf:
// its fields are somebody else's invariants, and nothing in a render event
// carries renderable prose inside a foreign type.
const apModulePath = "github.com/authzed/openagentprimitives/"

func sweepableStruct(t reflect.Type) bool {
	return strings.HasPrefix(t.PkgPath(), apModulePath)
}

var (
	toolSessionDeltaType = reflect.TypeOf(channelevents.ToolSessionDeltaPayload{})
	toolSessionEventType = reflect.TypeOf(channelevents.ToolSessionEventPayload{})
)

// keepsToolSGR names the ONE class of field that keeps colour: a tool's own
// output. ToolSessionEventPayload.Text is the parsed leg of the same stream
// ToolSessionDeltaPayload.Data carries raw, which is why both are here and the
// event payload's other strings (ToolName, OuterTool, Reason, Summary) are not
// — those are labels the surface composes into its own block header.
//
// A field renamed out from under this function loses colour, which is a visible
// regression and not a hole. A test still pins the names. See NOTES.md.
func keepsToolSGR(structType reflect.Type, field string) bool {
	switch structType {
	case toolSessionDeltaType:
		return field == "Data"
	case toolSessionEventType:
		return field == "Text"
	}
	return false
}
