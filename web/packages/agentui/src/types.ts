// TS mirrors of pkg/web/uicomponents' wire types. These are structural mirrors, not
// generated: the Go side is authoritative and validates every declaration
// before it reaches the browser, so the renderer's job is to render what it is
// given, never to re-validate it.

export interface Binding {
  source: "tool" | "memory" | "artifact" | "action";
  ref: string;
  args?: unknown;
  /**
   * select is SERVER-SIDE ONLY and is deliberately never read here. It rides
   * the wire because the whole declaration crosses to the browser (the
   * renderer needs `bindings` to know which props are bound), but the value
   * that arrives in a BindingState has ALREADY been extracted by
   * pkg/web/webui/agentui's resolveOneBinding. Applying it again in the browser
   * would extract from an already-extracted value and render nothing, on a
   * clean 200 — which is exactly what testdata/bindings.golden.json's `data`
   * hook exists to catch.
   */
  select?: string;
}

export interface Node {
  component: string;
  props?: Record<string, unknown>;
  bindings?: Record<string, Binding>;
  children?: Node[];
}

// Action mirrors pkg/web/uicomponents.Action / spiceboxv1alpha1.AgentUIAction
// field-for-field, INCLUDING tool and args: the whole table ships to the
// browser in the bootstrap props (see
// pkg/web/webui/agentui/ui/testdata/props.actions.golden.json). What stays
// server-side is not the data but the AUTHORITY — a control only ever sends a
// NAME plus an inputs map (actions.tsx's ActionsContextValue), and
// actions.go's handler reads the tool and the args template out of its own
// declaration on every request, so a browser that rewrote these fields in
// memory would change nothing about what runs.
//
// This shape exists so the declaration wire's `actions` table is readable by
// name from the type system rather than staying invisible to it.
export interface Action {
  name: string;
  // Exactly one of tool and prompt is set (pkg/web/uicomponents validates it), so
  // both are optional here rather than tool being required: a prompt action
  // has no tool, and typing one as mandatory would make every such action a
  // type error at the one place the distinction is read.
  tool?: string;
  // prompt marks an action that ASKS THE AGENT rather than calling a tool. The
  // browser routes on it — see useActionLifecycle's invoke — because asking
  // the agent something is sending it a message, which goes to the transcript
  // rather than the actions route.
  prompt?: string;
  args?: unknown;
  inputs?: string[];
}

export interface Declaration {
  actions?: Action[];
  // view is the page: ONE node tree. The agent's writable regions are
  // oap:generative nodes ("hooks") anywhere inside it, named by their `name`
  // prop — the same name update_view targets and the same name this package
  // renders as data-hook.
  view: Node;
  /**
   * agentComposed is SERVER-COMPUTED and arrives on the wire only: the names
   * of hooks whose content (or whose emptiness) is the agent's. Not a node
   * field — the Go validator rejects any structural key it does not know on a
   * node — so an agent cannot mark its own work.
   */
  agentComposed?: string[];
  /**
   * answered is SERVER-COMPUTED, wire only: hooks whose question the viewer
   * already replied to — derived from the fill's write time and the
   * transcript (pkg/web/webui/agentui's answeredHooks), never stored and
   * never something an agent's own fragment could set. It is what keeps an
   * ap:question from asking twice across a reload.
   */
  answered?: string[];
}

// ActionRequestBody mirrors pkg/web/webui/agentui/actions.go's actionRequestBody
// field-for-field: the ONLY shape a browser may POST to
// /agent-ui/{ns}/{name}/actions to invoke a declared action. There is
// deliberately no tool/args/scope/session field — see actions.go's own doc
// comment for why: the tool and the args template always come from the
// server-side declaration, never the request.
export interface ActionRequestBody {
  action: string;
  params?: Record<string, string>;
  inputs?: Record<string, string>;
}

// ActionResponseBody mirrors actions.go's actionResponseBody: the
// synchronous answer to an action invocation. requestId is an opaque
// correlation handle for matching a later live update, never rendered as
// prose; state is a uiaction.State value ("submitted", "denied", "failed",
// ...) rendered ON the control that fired the action, never as an HTTP
// error — a denial is a 200 carrying state "denied".
export interface ActionResponseBody {
  requestId: string;
  state: string;
  message?: string;
}
