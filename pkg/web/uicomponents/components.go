package uicomponents

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents/registry"
)

// The v1 vocabulary. Every type here is a thin declarative surface over an
// existing @ap/design (shadcn) component — nothing is redesigned and nothing is
// forked, so every agent UI inherits the product's look and its
// CSP-compatible styling for free.
//
// Adding a type is a deliberate, reviewed act: this set is the entire space an
// agent-written view-model can express, and that boundedness is what makes
// LLM-authored UI safe.
//
// # The jsonschema tags TELL the agent; they do not ENFORCE
//
// The `jsonschema:"enum=…"` / `minimum=` / `maximum=` tags below are read by
// component.Schema()'s reflector and land in the schema published to the agent.
// A bare `string` tells the agent "gap is a string" and nothing more, so it
// writes gap:"large" and the renderer silently falls back to "md"; naming the
// legal values turns that guess into a closed choice.
//
// validateProps does NOT check these values — it decodes into the struct,
// which is a TYPE check, not a VALUE check, so gap:"large" validates clean.
// That is deliberate: every renderer already falls back for an unrecognized
// value, so an out-of-vocabulary value degrades to the default rather than
// blanking anything, and rejecting a declaration over one cosmetic string
// would cost the agent a turn for no user-visible gain. The opposite of the
// NAME case, where a wrong prop name means the prop is silently absent and the
// agent is told success — hence names enforced, values not.
//
// An enum tag therefore has teeth only as far as the model complies. If a
// value ever becomes security-relevant — a variant changing what a control
// DOES rather than how it looks — it needs a real check in validateProps.

type StackProps struct {
	Direction string `json:"direction,omitempty" jsonschema:"enum=vertical,enum=horizontal"`            // "vertical" is the default
	Gap       string `json:"gap,omitempty" jsonschema:"enum=none,enum=sm,enum=md,enum=lg"`              // "md" is the default
	Align     string `json:"align,omitempty" jsonschema:"enum=start,enum=center,enum=end,enum=stretch"` // "start" is the default
}

// StatusProps is ap:status, one line that is always true about something the
// page tracks — a test run, a build — drawn as a dot in the state's colour and
// the text. The agent repaints it at every transition; it is the page's source
// of the state, so the conversation never has to ask "is it running?".
type StatusProps struct {
	// State picks the dot: idle (grey), running (pulsing), paused (amber),
	// ended (grey), unwatched (amber — still running, no longer watched).
	State string `json:"state" jsonschema:"enum=idle,enum=running,enum=paused,enum=ended,enum=unwatched"`
	// Text is the line itself, carrying NO clock: "Running since", "Paused
	// at", "Ended at". The moment goes in Since — the agent is not the one
	// who knows which clock the person reads.
	Text string `json:"text"`
	// Since is the moment the line refers to, as an RFC3339 time. The browser
	// draws it after the text in the VIEWER's own time zone and keeps the raw
	// value on the element; a line about no particular moment omits it.
	Since string `json:"since,omitempty"`
}

// checkStatus is StatusProps's Component.Check: a known state, a non-empty
// line, and a since that is a moment. An unknown state would render as a dot
// with no meaning, and a since the browser cannot parse as a time is one it
// draws nothing for — which reads as the agent having forgotten to say when.
func checkStatus(props any) error {
	p, ok := props.(*StatusProps)
	if !ok {
		return fmt.Errorf("ap:status: props decoded to %T", props)
	}
	switch p.State {
	case "idle", "running", "paused", "ended", "unwatched":
	default:
		return fmt.Errorf("ap:status: state %q is not one of idle, running, paused, ended, unwatched", p.State)
	}
	if strings.TrimSpace(p.Text) == "" {
		return fmt.Errorf("ap:status: text is required")
	}
	if p.Since != "" {
		if _, err := time.Parse(time.RFC3339, p.Since); err != nil {
			return fmt.Errorf("ap:status: since %q is not an RFC3339 time", p.Since)
		}
	}
	return nil
}

type GridProps struct {
	Columns int    `json:"columns,omitempty" jsonschema:"minimum=1,maximum=12"` // omit for auto-fit
	Gap     string `json:"gap,omitempty" jsonschema:"enum=none,enum=sm,enum=md,enum=lg"`
}

type CardProps struct {
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

// CollapsibleProps is ap:collapsible — a titled container the person can fold
// closed and open again. Collapsed is the author's or agent's STARTING state;
// the person's toggle wins over it in the browser until the prop changes.
// The same disclosure is what the view draws around a hook bound to a
// finished timeline step (see GenerativeProps.Step); this component is the
// author-driven form of it.
type CollapsibleProps struct {
	Title     string `json:"title"`
	Collapsed bool   `json:"collapsed,omitempty"`
}

// checkCollapsible is CollapsibleProps's Component.Check: a fold with no
// title has no label to reopen it by.
func checkCollapsible(props any) error {
	p, ok := props.(*CollapsibleProps)
	if !ok {
		return fmt.Errorf("ap:collapsible: props decoded to %T", props)
	}
	if strings.TrimSpace(p.Title) == "" {
		return fmt.Errorf("ap:collapsible: title is required")
	}
	return nil
}

type HeadingProps struct {
	Text  string `json:"text,omitempty"`
	Level int    `json:"level,omitempty" jsonschema:"minimum=1,maximum=4"` // defaults to 2
}

type TextProps struct {
	Text    string `json:"text,omitempty"`
	Variant string `json:"variant,omitempty" jsonschema:"enum=body,enum=muted,enum=mono"` // "body" is the default
}

// MarkdownProps carries agent-authored prose. This is the CORRECT home for
// rich text: @ap/design's Markdown renders GitHub-flavored markdown with raw
// HTML disabled (no rehype-raw) and dangerous URL protocols stripped, so the
// body can never introduce markup or script. Reaching for ap:raw_html to format
// a paragraph is a smell — the vocabulary makes the safe thing the convenient
// thing.
type MarkdownProps struct {
	Body string `json:"body,omitempty"`
}

type MetricProps struct {
	Label   string `json:"label,omitempty"`
	Value   string `json:"value,omitempty"`
	Delta   string `json:"delta,omitempty"`
	Trend   string `json:"trend,omitempty" jsonschema:"enum=up,enum=down,enum=flat"`
	Caption string `json:"caption,omitempty"`
}

type TableColumn struct {
	Key    string `json:"key"`
	Header string `json:"header,omitempty"`
	Align  string `json:"align,omitempty" jsonschema:"enum=left,enum=right,enum=center"` // "left" is the default
}

type TableProps struct {
	// Columns fixes the display order; a row key with no column is not shown.
	Columns []TableColumn `json:"columns,omitempty"`
	// Rows are keyed by TableColumn.Key; usually filled by a data binding.
	Rows []map[string]any `json:"rows,omitempty"`
	// Empty is the message shown when Rows is empty, distinguishing "no
	// results" from "not loaded yet".
	Empty string `json:"empty,omitempty"`
	// RowAction puts a control on every row, firing the named action with that
	// row's own values as its inputs. It is how a table of records becomes
	// something a viewer can act on one record at a time — "tell me about this
	// one" — without the declaration naming which one in advance.
	//
	// A flat string naming an action, exactly like ButtonProps.Action, so this
	// component registers the SAME ActionProp seam every other action-firing
	// component uses (see registry.Component.ActionProp) and its reference is
	// found by the same ActionRefs walk. Nesting it under an object would have
	// meant teaching that walk to look inside props for one component — a
	// special case in the shared abstraction to save one level of YAML.
	RowAction string `json:"rowAction,omitempty"`
	// RowActionLabel is the row control's text. Defaults to "Details" in the
	// renderer: a row control with no label is a button whose effect nobody
	// can guess.
	RowActionLabel string `json:"rowActionLabel,omitempty"`
}

type ChartSeries struct {
	Key   string `json:"key"`
	Label string `json:"label,omitempty"`
}

type ChartProps struct {
	Kind string `json:"kind,omitempty" jsonschema:"enum=line,enum=bar,enum=area"` // "line" is the default
	// XKey names the Data key plotted along the x axis.
	XKey string `json:"xKey,omitempty"`
	// Series names which Data keys become plotted lines, and how they label.
	Series []ChartSeries `json:"series,omitempty"`
	// Data are the plotted points, usually filled by a data binding.
	Data []map[string]any `json:"data,omitempty"`
}

type SelectOption struct {
	Value string `json:"value"`
	Label string `json:"label,omitempty"`
}

type SelectProps struct {
	Param string `json:"param,omitempty"` // the binding parameter this control drives
	// Options are the choices offered; bindable, unlike Value.
	Options []SelectOption `json:"options,omitempty"`
	// Value is the DECLARED default for Param; empty means the viewer must
	// choose before dependent bindings can resolve.
	Value string `json:"value,omitempty"`
	// Placeholder is shown while Value is empty.
	Placeholder string `json:"placeholder,omitempty"`
}

type DateRangeProps struct {
	// Param is the declared name; it drives "<param>.from" and "<param>.to",
	// never "<param>" itself.
	Param string `json:"param,omitempty"`
	// From is the declared default for "<param>.from".
	From string `json:"from,omitempty"` // RFC3339
	// To is the declared default for "<param>.to".
	To string `json:"to,omitempty"`
}

type ButtonProps struct {
	Label string `json:"label,omitempty"`
	// Variant is the full set the design system's button cva declares, and the
	// renderer's own list is the same six. Keep them in step: a variant listed
	// here but absent from cva renders UNSTYLED (a present-but-unmatched cva
	// prop suppresses defaultVariants), and one the renderer accepts but this
	// omits is a value the agent is never told it may use.
	Variant string `json:"variant,omitempty" jsonschema:"enum=default,enum=secondary,enum=destructive,enum=outline,enum=ghost,enum=link"`
	// Action names the declared action this button fires. The declaration only
	// NAMES it; authorization happens per click, viewer-bound, in the runner.
	Action string `json:"action,omitempty"`
	// Disabled is bindable, so data can decide whether the control is live.
	Disabled bool `json:"disabled,omitempty"`
}

type FormField struct {
	// Name is the input key sent at invoke time; must appear in the target
	// action's own Inputs allowlist or the declaration is rejected.
	Name        string `json:"name"`
	Label       string `json:"label,omitempty"`
	Kind        string `json:"kind,omitempty" jsonschema:"enum=text,enum=number,enum=textarea"` // "text" is the default
	Placeholder string `json:"placeholder,omitempty"`
	// Required is enforced in the browser only; the server checks names, not
	// presence.
	Required bool `json:"required,omitempty"`
}

// MaxFormValueLen bounds one initial field value in runes — a prefilled
// description, not a document.
const MaxFormValueLen = 4096

type FormProps struct {
	// Fields declare the input NAMES this form supplies when it fires.
	Fields      []FormField `json:"fields,omitempty"`
	SubmitLabel string      `json:"submitLabel,omitempty"`
	// Action names the declared action this form fires.
	Action string `json:"action,omitempty"`
	// Values are initial field texts, by field name — what an Edit shows the
	// person before they change anything. A key that names no declared field
	// is refused, and so is a line break in a field that is not a textarea: a
	// value the form cannot show is a value nobody can see or correct.
	// Rendered as text, never as markup.
	Values map[string]string `json:"values,omitempty"`
}

// checkForm is FormProps's Component.Check: every initial value names a
// declared field, stays within MaxFormValueLen, and carries a line break only
// where the field can show one.
//
// The line-break rule is about what the person can SEE. Only a textarea
// renders more than one line; an <input> shows the first line and silently
// hides the rest, so a prefilled single-line field would submit text the
// person was never shown and could not correct — the same reason a value
// naming no declared field is refused.
func checkForm(props any) error {
	p, ok := props.(*FormProps)
	if !ok {
		return fmt.Errorf("ap:form: props decoded to %T", props)
	}
	declared := make(map[string]FormField, len(p.Fields))
	for _, f := range p.Fields {
		declared[f.Name] = f
	}
	for _, k := range slices.Sorted(maps.Keys(p.Values)) {
		f, ok := declared[k]
		if !ok {
			return fmt.Errorf("ap:form: values[%q] names no declared field", k)
		}
		if n := utf8.RuneCountInString(p.Values[k]); n > MaxFormValueLen {
			return fmt.Errorf("ap:form: values[%q] is %d characters; the limit is %d", k, n, MaxFormValueLen)
		}
		if f.Kind != "textarea" && strings.ContainsAny(p.Values[k], "\r\n") {
			return fmt.Errorf("ap:form: values[%q] contains a line break; field %q is not a textarea", k, f.Name)
		}
	}
	return nil
}

type BadgeProps struct {
	Text string `json:"text,omitempty"`
	// Four, not the button's six: badge.tsx's cva declares no ghost or link.
	Variant string `json:"variant,omitempty" jsonschema:"enum=default,enum=secondary,enum=destructive,enum=outline"`
}

type TabProps struct {
	Value string `json:"value"`
	Label string `json:"label,omitempty"`
}

type TabsProps struct {
	Tabs  []TabProps `json:"tabs,omitempty"`
	Value string     `json:"value,omitempty"`
}

type AlertProps struct {
	Title    string `json:"title,omitempty"`
	Body     string `json:"body,omitempty"`
	Severity string `json:"severity,omitempty" jsonschema:"enum=info,enum=warning,enum=error"` // "info" is the default
}

type EmptyProps struct {
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
}

// StepsProps is ap:steps, the page's stage timeline. Pinned keeps it in view
// at the top of the page while the rest scrolls — the builder page sets it,
// because the timeline is the one element that orients the person.
type StepsProps struct {
	Steps  []Step `json:"steps,omitempty"`
	Pinned bool   `json:"pinned,omitempty"`
}

// MaxStepSummaryLen bounds Step.Summary in runes: one line under a step's
// label in the rail, never a sentence.
const MaxStepSummaryLen = 60

// Step is one stage in an ap:steps timeline. State defaults to "upcoming" when
// empty so a partially-authored list still renders sensibly.
//
// ID is optional and is what a hook binds to (GenerativeProps.Step): the view
// collapses a hook whose step is done and keeps the active step's hook open.
// It is a DNS-1123 label, unique within the timeline, and it must survive
// every repaint of the timeline — an agent that drops an id breaks the hooks
// bound to it, and validateStepBindings refuses the fill saying so.
type Step struct {
	ID    string `json:"id,omitempty" jsonschema:"pattern=^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"`
	Label string `json:"label,omitempty"`
	State string `json:"state,omitempty" jsonschema:"enum=done,enum=active,enum=upcoming"`
	// Summary is the one line the rail shows under this step's label: a
	// fact about the step ("none", "valid · 12:48"), not a sentence. The
	// agent keeps it current when it repaints the timeline; the declaration's
	// own steps carry the defaults. Plain text, one line, at most
	// MaxStepSummaryLen runes (checkSteps).
	Summary string `json:"summary,omitempty"`
}

// checkSteps is StepsProps's Component.Check: every id present is a label and
// no id repeats, and every step's summary is one line within MaxStepSummaryLen
// runes. Absent ids are legal — binding is opt-in — but the summary check
// applies to every step regardless of whether it carries an id.
func checkSteps(props any) error {
	p, ok := props.(*StepsProps)
	if !ok {
		return fmt.Errorf("ap:steps: props decoded to %T", props)
	}
	seen := map[string]struct{}{}
	for i, st := range p.Steps {
		if strings.ContainsAny(st.Summary, "\r\n") {
			return fmt.Errorf("ap:steps: steps[%d].summary must be one line", i)
		}
		if n := utf8.RuneCountInString(st.Summary); n > MaxStepSummaryLen {
			return fmt.Errorf("ap:steps: steps[%d].summary is %d characters; the limit is %d", i, n, MaxStepSummaryLen)
		}
		if st.ID == "" {
			continue
		}
		if errs := validation.IsDNS1123Label(st.ID); len(errs) > 0 {
			return fmt.Errorf("ap:steps: steps[%d].id %q: %s", i, st.ID, strings.Join(errs, "; "))
		}
		if _, dup := seen[st.ID]; dup {
			return fmt.Errorf("ap:steps: steps[%d].id %q is declared twice", i, st.ID)
		}
		seen[st.ID] = struct{}{}
	}
	return nil
}

// QuestionProps is ap:question — the agent's inline ask. It is a GENERATIVE
// component: the agent puts one inside a hook instead of asking in prose, so
// the person sees the question on the page they are looking at. Submitting
// sends the answer to the session's transcript exactly as a typed reply,
// through the same route a Prompt action uses; no declared action is named,
// which is why this props struct carries no ActionProp — there is nothing to
// authorize beyond what the viewer could have typed.
type QuestionProps struct {
	// Prompt is the question, rendered as text.
	Prompt string `json:"prompt,omitempty"`
	// Kind picks the control: free text (the default) or one of Choices.
	Kind string `json:"kind,omitempty" jsonschema:"enum=text,enum=choice"`
	// Choices are the options for kind=choice; the chosen Value is the answer.
	Choices []QuestionChoice `json:"choices,omitempty"`
	// Placeholder is the empty-field hint for kind=text.
	Placeholder string `json:"placeholder,omitempty"`
	// SubmitLabel is the button text; the renderer defaults it to "Answer".
	SubmitLabel string `json:"submitLabel,omitempty"`
}

// QuestionChoice is one option of a kind=choice question.
type QuestionChoice struct {
	Label string `json:"label,omitempty"`
	Value string `json:"value"`
}

// ProgressProps is ap:progress — agent-authored status: what it is doing now,
// what it has finished, what comes next. When a hook shows one, the view's
// default progress region yields to it (the view derives that from the tree,
// never from anything the agent says).
type ProgressProps struct {
	// Now is one line: what the agent is doing right now. Bindable, so an
	// author can wire it to a live status source instead of a literal.
	Now string `json:"now,omitempty"`
	// Done lists finished steps, in order.
	Done []string `json:"done,omitempty"`
	// Next lists upcoming steps, in order.
	Next []string `json:"next,omitempty"`
	// Step optionally names the ap:steps label this progress belongs to. It
	// is free caption text, rendered as-is and never checked — unlike a
	// hook's step (GenerativeProps.Step), which is a step ID the validator
	// resolves.
	Step string `json:"step,omitempty"`
}

// SkeletonProps is ap:skeleton, the in-progress stand-in the platform
// substitutes for a node whose bound data is still resolving — a first load, a
// re-resolve after a filter change, or a session being woken to answer.
//
// Registered here rather than existing only in the browser, because the
// browser registry must mirror this list exactly: a renderer with no component
// behind it is a contract break, not merely unused. An author may also declare
// one as a Tier-0 placeholder for a slot an agent will fill.
//
// Nothing here is Bindable: a loading placeholder whose own content had to
// load could never render, and the platform writes Label when it has something
// specific to say.
type SkeletonProps struct {
	Label string `json:"label,omitempty"`
}

type ErrorProps struct {
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
}

// RawHTMLProps is the Tier-2 escape hatch. The HTML is UNTRUSTED and is served
// from the sandbox origin in an allow-scripts (never allow-same-origin) iframe
// under a server-built CSP — the same posture the mcp-ui widget path already
// uses. It exists so the vocabulary never has to be widened for a one-off, NOT
// as a general-purpose rendering path.
type RawHTMLProps struct {
	HTML string `json:"html,omitempty"`
}

// SessionViewProps embeds the existing browser session view
// (pkg/web/webui/sessionview) for a session ref, as a same-origin iframe. It
// carries NO capability of its own: the embedded page's own agentsession#interact
// check (of the VIEWER) is the sole gate, so an agent-chosen ref can only ever
// surface sessions the viewer is already entitled to. SessionRef is "ns/name".
type SessionViewProps struct {
	SessionRef string `json:"sessionRef,omitempty"`
}

// ChatProps is ap:chat: the browser chat for ONE session, inside the page, as
// a same-origin iframe of /chat-embed/{ns}/{name}. It carries no capability of
// its own: that page checks the VIEWER's own agentsession#interact, so an
// agent-chosen ref can only ever show a session the viewer may already talk
// to — the same argument ap:session_view makes. SessionRef is "ns/name".
type ChatProps struct {
	SessionRef string `json:"sessionRef"`
}

// checkChat is ChatProps's Component.Check: exactly two DNS-1123 labels
// joined by "/", so the iframe src is always a well-formed same-origin path.
func checkChat(props any) error {
	p, ok := props.(*ChatProps)
	if !ok {
		return fmt.Errorf("ap:chat: props decoded to %T", props)
	}
	segs := strings.Split(p.SessionRef, "/")
	if len(segs) != 2 {
		return fmt.Errorf("ap:chat: sessionRef must be ns/name, got %q", p.SessionRef)
	}
	for _, s := range segs {
		if errs := validation.IsDNS1123Label(s); len(errs) > 0 {
			return fmt.Errorf("ap:chat: sessionRef segment %q is not a DNS-1123 label: %s", s, strings.Join(errs, "; "))
		}
	}
	return nil
}

// AgentLinkProps is ap:agentlink: a button-styled control for ONE agent. It
// carries no href — the renderer builds the only address it can point at from
// Namespace/AgentClass/Prompt — so a fill can never send a person anywhere
// else. Namespace and AgentClass are DNS-label shaped (Check enforces it,
// below); Prompt is prefilled into the session it opens.
//
// Embed is the fifth value, and it decides what the click DOES: without it
// the control opens the browser chat's new-session dialog in a new tab, and
// with it the click starts the session as the viewer and mounts its chat in
// the control's place (see Embed's own comment below).
//
// The jsonschema "pattern" tags on Namespace/AgentClass are advisory hints
// for schema readers, matching the DNS-1123 label rule checkAgentLink
// enforces below — the Check hook is what actually gates a bad value, not
// the published pattern.
type AgentLinkProps struct {
	Namespace  string `json:"namespace" jsonschema:"pattern=^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"`
	AgentClass string `json:"agentClass" jsonschema:"pattern=^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"`
	Label      string `json:"label"`
	Prompt     string `json:"prompt,omitempty"`
	// Embed makes the click start the session AS THE VIEWER (the same
	// POST /sessions/api/start the tab flow uses) and mount the chat in the
	// link's place — an ap:chat for the returned session — instead of opening
	// a tab. It grants nothing: the same route, the same starter check.
	Embed bool `json:"embed,omitempty"`
}

// checkAgentLink is AgentLinkProps's Component.Check: the shape check
// (validateProps' decode) only proves these are strings, not that Namespace
// and AgentClass are safe to splice into a same-origin URL path/query the
// renderer builds unescaped-of-meaning (encoded, but still address-shaped).
// Both name a Kubernetes object, so the same DNS-1123 label rule the API
// server itself enforces on those names is the correct bound here, not an
// ad hoc one.
func checkAgentLink(props any) error {
	p, ok := props.(*AgentLinkProps)
	if !ok {
		return fmt.Errorf("ap:agentlink: props decoded to %T", props)
	}
	if errs := validation.IsDNS1123Label(p.Namespace); len(errs) > 0 {
		return fmt.Errorf("ap:agentlink: namespace %q: %s", p.Namespace, strings.Join(errs, "; "))
	}
	if errs := validation.IsDNS1123Label(p.AgentClass); len(errs) > 0 {
		return fmt.Errorf("ap:agentlink: agentClass %q: %s", p.AgentClass, strings.Join(errs, "; "))
	}
	if strings.TrimSpace(p.Label) == "" {
		return fmt.Errorf("ap:agentlink: label is required")
	}
	return nil
}

// AttachmentProps is ap:attachment — a file of this session, shown as a file
// row with a download control. The agent names only the artifact id; the
// shell resolves the file's name, size and kind from the artifact-view meta
// route and builds the download link itself, both gated by the viewer's own
// CheckView, so a fill can never address another session's file or point the
// person at a URL of its own. There is deliberately no href and no url.
type AttachmentProps struct {
	Artifact string `json:"artifact" jsonschema:"pattern=^artifact-[0-9a-f]{16}$"`
	Label    string `json:"label,omitempty"`
}

// artifactIDRe is the shape memory.NewID mints for the artifact kind: the
// kind's prefix and sixteen lowercase hex digits (eight random bytes).
var artifactIDRe = regexp.MustCompile(`^artifact-[0-9a-f]{16}$`)

// checkAttachment is AttachmentProps's Component.Check: the id must be
// artifact-shaped. A render handle (ar-…) or a revision id (artrev-…) is not
// what the meta and download routes key on, and refusing it here tells the
// agent which value to pass instead of a row that says "unavailable".
func checkAttachment(props any) error {
	p, ok := props.(*AttachmentProps)
	if !ok {
		return fmt.Errorf("ap:attachment: props decoded to %T", props)
	}
	if p.Artifact == "" {
		return fmt.Errorf("ap:attachment: artifact is required (the artifactId the tool returned)")
	}
	if !artifactIDRe.MatchString(p.Artifact) {
		return fmt.Errorf("ap:attachment: artifact %q is not an artifact id (expected artifact-<16 hex digits>, the artifactId the tool returned — not a handle)", p.Artifact)
	}
	return nil
}

// NoticeProps is ap:notice — something the agent TELLS the person while it
// waits for an event rather than an answer: a test session started, a draft
// is being packaged. It is the generative counterpart of ap:question for the
// case where no words are wanted back, and the view keeps its reply modal
// closed while one is on the page (the same rule an ap:question gets).
//
// Buttons are declared actions, named the way ap:button names its one action;
// the registry's ActionListProp is what lets ActionRefs find them, so an
// undeclared one fails the document instead of rendering a button that 400s.
// There is deliberately no href and no input: a notice navigates nowhere and
// collects nothing — a control that wants either is an ap:button or ap:form.
type NoticeProps struct {
	Title string `json:"title,omitempty"`
	// Body is markdown, rendered through the same renderer as ap:markdown.
	Body string `json:"body"`
	Tone string `json:"tone,omitempty" jsonschema:"enum=info,enum=success,enum=warning"`
	// Buttons are at most MaxNoticeButtons; each names a declared action.
	Buttons []NoticeButton `json:"buttons,omitempty"`
}

// NoticeButton is one button on an ap:notice.
type NoticeButton struct {
	Label  string `json:"label"`
	Action string `json:"action"`
}

// MaxNoticeButtons bounds a notice's buttons: a notice offers a next step or
// two, and a row of five is a menu, which is a different component.
const MaxNoticeButtons = 4

// noticeTones is the closed set NoticeProps.Tone admits. Unlike most enum
// tags in this file this one IS enforced: the renderer keys colour and icon
// on it, and an out-of-set tone would otherwise degrade to "info" silently
// while the agent is told success.
var noticeTones = []string{"info", "success", "warning"}

// checkNotice is NoticeProps's Component.Check: the shape decode proves the
// fields are strings and a slice; this proves the VALUES — a body, a legal
// tone, a bounded list of buttons each with a label and an action name. The
// action name's existence in the declared table is validateActions' job,
// through ActionRefs.
func checkNotice(props any) error {
	p, ok := props.(*NoticeProps)
	if !ok {
		return fmt.Errorf("ap:notice: props decoded to %T", props)
	}
	if strings.TrimSpace(p.Body) == "" {
		return fmt.Errorf("ap:notice: body is required")
	}
	if p.Tone != "" && !slices.Contains(noticeTones, p.Tone) {
		return fmt.Errorf("ap:notice: tone %q is not one of %s", p.Tone, strings.Join(noticeTones, ", "))
	}
	if len(p.Buttons) > MaxNoticeButtons {
		return fmt.Errorf("ap:notice: %d buttons; at most %d buttons", len(p.Buttons), MaxNoticeButtons)
	}
	for i, b := range p.Buttons {
		if strings.TrimSpace(b.Label) == "" {
			return fmt.Errorf("ap:notice: buttons[%d]: label is required", i)
		}
		if b.Action == "" {
			return fmt.Errorf("ap:notice: buttons[%d]: action is required", i)
		}
	}
	return nil
}

// GenerativeType is the one structural type: a named region of the page the
// agent may fill. It is a node like any other so it can sit ANYWHERE in the
// author's tree, and it is registered like any other so validateProps and
// the schema reflector need no special case for it.
const GenerativeType = "oap:generative"

// AllowAll is the allowlist entry that admits the whole registered
// vocabulary (every non-structural type).
const AllowAll = "*"

// PageType is the second structural type: the page's outermost node, which
// carries the layout the renderer draws the page in. Like oap:generative it is
// author-only — never published to the agent, never legal in a fill — and it
// is legal ONLY as the root of spec.view (validatePageRoot), because a layout
// is a property of the whole page and a page inside a page has no meaning.
const PageType = "oap:page"

// Page layouts. Column is today's single column and is what an empty layout
// means, so a page that never declares a root renders exactly as before. Rail
// draws the page's one ap:steps timeline as a vertical rail on the left and
// shows the selected step's hooks in the stage beside it.
const (
	PageLayoutColumn = "column"
	PageLayoutRail   = "rail"
)

// PageProps are oap:page's author-only props.
type PageProps struct {
	// Layout selects how the renderer arranges the page: column (default) or
	// rail. Anything else is refused by checkPage.
	Layout string `json:"layout,omitempty" jsonschema:"enum=column,enum=rail"`
}

// checkPage is PageProps's Component.Check: the layout, when set, is one the
// renderer knows. A typo'd layout must fail at admission, not render as a
// silently-wrong column.
func checkPage(props any) error {
	p, ok := props.(*PageProps)
	if !ok {
		return fmt.Errorf("oap:page: props decoded to %T", props)
	}
	switch p.Layout {
	case "", PageLayoutColumn, PageLayoutRail:
		return nil
	}
	return fmt.Errorf("oap:page: layout %q is not one of column, rail", p.Layout)
}

// GenerativeProps are an author-only contract with the agent. Intent is the
// instruction the agent is given for this region; AllowedComponents bounds
// what the AGENT may put here (validateHooks requires it non-empty on an
// author-written hook; the spec.slots shim writes ["*"]). Neither bounds the
// author's own children: those are validated against the registry like any
// other node, and an author default the agent could not regenerate is still
// a default the agent can replace.
type GenerativeProps struct {
	Name              string   `json:"name"`
	Intent            string   `json:"intent,omitempty"`
	AllowedComponents []string `json:"allowedComponents,omitempty"`
	// Step binds this hook to a step of the page's ap:steps timeline, by the
	// step's id. The view collapses the hook once that step is done and keeps
	// it open while the step is active; validateStepBindings requires the id
	// to exist on the page's one, literal-stepped timeline.
	Step string `json:"step,omitempty"`
	// Title is the fold's header when this hook is bound to a step that is
	// done, in place of the step's label — so two hooks bound to one step do
	// not fold under two identical headers. Author-only, plain text.
	Title string `json:"title,omitempty"`
}

func init() {
	for _, c := range []Component{
		{Type: "ap:stack", Props: StackProps{}, AcceptsChildren: true},
		{Type: "ap:status", Props: StatusProps{}, Check: checkStatus},
		{Type: "ap:grid", Props: GridProps{}, AcceptsChildren: true},
		{Type: "ap:card", Props: CardProps{}, AcceptsChildren: true},
		{Type: "ap:tabs", Props: TabsProps{}, AcceptsChildren: true},
		{Type: "ap:collapsible", Props: CollapsibleProps{}, AcceptsChildren: true, Check: checkCollapsible},

		{Type: "ap:heading", Props: HeadingProps{}, Bindable: []string{"text"}},
		{Type: "ap:text", Props: TextProps{}, Bindable: []string{"text"}},
		{Type: "ap:markdown", Props: MarkdownProps{}, Bindable: []string{"body"}},
		{Type: "ap:metric", Props: MetricProps{}, Bindable: []string{"value", "delta", "caption"}},
		// ActionProp "rowAction": a table with one fires a control per row.
		// Registered on the same seam as ap:button's, so ActionRefs finds a
		// row action's reference without knowing what a table is, and an
		// undeclared one fails the document rather than rendering a button
		// that 400s on every press.
		//
		// No InputNamesProp: a form declares its field names up front, but a
		// table's inputs are the ROW's own keys, which come from a binding and
		// are unknowable at authoring time. The action's own Inputs allowlist
		// bounds them, filtered server-side on invoke.
		{Type: "ap:table", Props: TableProps{}, Bindable: []string{"rows"}, ActionProp: "rowAction"},
		{Type: "ap:chart", Props: ChartProps{}, Bindable: []string{"data"}},
		// A control's OPTIONS are data the agent may supply; its VALUE is not.
		// SelectProps.Value and DateRangeProps.From/To are declared DEFAULTS
		// for the runtime parameter keys below, and the viewer owns them from
		// first interaction onward. Making them bindable would give one fact
		// two writers with no reconciliation: the control would display a
		// server-resolved value while the parameter map stayed empty, so every
		// sibling binding on that parameter would error with no signal to the
		// viewer that display and resolution had diverged.
		{Type: "ap:select", Props: SelectProps{}, Bindable: []string{"options"},
			ParamProp: "param", ParamValues: []ParamValue{{ValueProp: "value"}}},
		{Type: "ap:session_view", Props: SessionViewProps{}, Bindable: []string{"sessionRef"}},
		{Type: "ap:chat", Props: ChatProps{}, Check: checkChat},
		{Type: "ap:daterange", Props: DateRangeProps{},
			ParamProp: "param", ParamValues: []ParamValue{{Suffix: "from", ValueProp: "from"}, {Suffix: "to", ValueProp: "to"}}},
		{Type: "ap:button", Props: ButtonProps{}, Bindable: []string{"disabled"}, ActionProp: "action"},
		{Type: "ap:form", Props: FormProps{}, ActionProp: "action", InputNamesProp: "fields", Check: checkForm},
		{Type: "ap:badge", Props: BadgeProps{}, Bindable: []string{"text"}},
		{Type: "ap:alert", Props: AlertProps{}, Bindable: []string{"body"}},
		{Type: "ap:empty", Props: EmptyProps{}},
		{Type: "ap:steps", Props: StepsProps{}, Bindable: []string{"steps"}, Check: checkSteps},
		{Type: "ap:progress", Props: ProgressProps{}, Bindable: []string{"now"}},
		{Type: "ap:question", Props: QuestionProps{}},
		{Type: "ap:notice", Props: NoticeProps{}, ActionListProp: "buttons", Check: checkNotice},
		{Type: "ap:error", Props: ErrorProps{}, Bindable: []string{"body"}},
		{Type: "ap:skeleton", Props: SkeletonProps{}},
		{Type: "ap:raw_html", Props: RawHTMLProps{}, Bindable: []string{"html"}},
		{Type: "ap:agentlink", Props: AgentLinkProps{}, Check: checkAgentLink},
		{Type: "ap:attachment", Props: AttachmentProps{}, Check: checkAttachment},
		{Type: GenerativeType, Props: GenerativeProps{}, AcceptsChildren: true, Structural: true},
		{Type: PageType, Props: PageProps{}, AcceptsChildren: true, Structural: true, Check: checkPage},
	} {
		registry.Register(c)
	}
}
