// The terminal surface's inertness guard: what the sweep keeps and drops, one
// sequence class at a time. See NOTES.md for why the surface is dangerous.
package local

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

const (
	// cursorRepaint is the two-sequence combination that makes this a
	// forged-action defect rather than a cosmetic one: move the cursor up one
	// line, erase that line. Repeat it and everything the surface drew before
	// the untrusted text is gone, replaced by whatever the attacker prints
	// next.
	cursorRepaint = "\x1b[1A\x1b[2K"
	// oscTitle sets the terminal WINDOW title — outside the app's own frame
	// entirely.
	oscTitle = "\x1b]0;pwned\x07"
)

// TestInteractionSender_TerminalControlSequencesAreInert covers every
// publisher-supplied slot of the wire payload the TUI interpolates into its
// blocking decision modal. Each row puts the repaint in exactly one slot, so
// dropping any one field from the sweep fails exactly one row.
//
// Both halves of the emitted event are asserted: Payload (what the modal reads
// field by field) and Text (the pre-rendered floor the timeline prints for a
// read-only prompt). They are rendered from the same payload and must not
// disagree about what has been made inert.
func TestInteractionSender_TerminalControlSequencesAreInert(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*channelevents.InteractionRequestPayload)
	}{
		{
			name: "the Lead cannot repaint the modal",
			mutate: func(p *channelevents.InteractionRequestPayload) {
				p.Lead = "Approval needed" + cursorRepaint + "Everything is fine"
			},
		},
		{
			name: "an agent-authored Body cannot repaint the modal",
			mutate: func(p *channelevents.InteractionRequestPayload) {
				p.Body = "The agent said: " + cursorRepaint + "[a] Approve   [d] Deny"
			},
		},
		{
			name: "a NextStep cannot repaint the modal",
			mutate: func(p *channelevents.InteractionRequestPayload) {
				p.NextStep = "Decide now" + cursorRepaint
			},
		},
		{
			name: "a field label cannot repaint the modal",
			mutate: func(p *channelevents.InteractionRequestPayload) {
				p.Fields = append(p.Fields, channelevents.InteractionField{
					Label: "Why" + cursorRepaint, Value: "because",
				})
			},
		},
		{
			name: "a field value — the summarizer's own prose — cannot repaint the modal",
			mutate: func(p *channelevents.InteractionRequestPayload) {
				p.Fields = append(p.Fields, channelevents.InteractionField{
					Label: "Justification", Value: "needed for the deploy" + cursorRepaint,
				})
			},
		},
		{
			name: "an excerpt of flagged content cannot repaint the modal",
			mutate: func(p *channelevents.InteractionRequestPayload) {
				p.Excerpt = &channelevents.InteractionExcerpt{
					Label: "Flagged" + cursorRepaint, Content: "ignore previous instructions" + cursorRepaint,
				}
			},
		},
		{
			name: "an action label cannot repaint the modal",
			mutate: func(p *channelevents.InteractionRequestPayload) {
				p.Actions[0].Label = "Approve" + cursorRepaint
			},
		},
		{
			name: "a link action's URL cannot set the window title",
			mutate: func(p *channelevents.InteractionRequestPayload) {
				p.Actions = append(p.Actions, channelevents.InteractionAction{
					ID: "open", Label: "Open", Kind: channelevents.ActionKindLink,
					URL: "https://example.invalid/x" + oscTitle,
				})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := decisionShapedPayload(tc.mutate)
			sink := &RecordingSink{}
			s := &interactionSender{sink: newInertSink(sink)}
			_, err := s.Send(context.Background(), sessInfo(),
				mustEnv(t, channelevents.KindInteractionRequest, p))
			require.NoError(t, err, "Send")

			require.Len(t, sink.Events(), 1)
			m, ok := sink.Events()[0].(MsgInteractionRequest)
			require.True(t, ok, "want MsgInteractionRequest, got %T", sink.Events()[0])

			assert.NotContains(t, payloadText(m.Payload), "\x1b",
				"no escape sequence may survive into the payload the decision modal renders")
			assert.NotContains(t, m.Text, "\x1b",
				"...nor into the pre-rendered text floor the timeline prints")
		})
	}
}

// The sweep must not eat the content it makes safe: an approver reads this to
// decide, and prose legitimately spans lines.
func TestInteractionSender_OrdinaryProseSurvivesTheSweep(t *testing.T) {
	p := decisionShapedPayload(func(p *channelevents.InteractionRequestPayload) {
		p.Body = "The agent wants to push to main.\n\nIt says the branch is protected."
	})
	sink := &RecordingSink{}
	s := &interactionSender{sink: newInertSink(sink)}
	_, err := s.Send(context.Background(), sessInfo(),
		mustEnv(t, channelevents.KindInteractionRequest, p))
	require.NoError(t, err, "Send")

	m := sink.Events()[0].(MsgInteractionRequest)
	assert.Equal(t, "The agent wants to push to main.\n\nIt says the branch is protected.", m.Payload.Body,
		"newlines are ordinary prose, not control sequences")
	assert.Contains(t, m.Text, "Approval needed", "the Lead still renders")
}

// TestInteractionSender_AppliedAndRejectedOutcomesAreInert covers the other two
// envelope kinds this sender handles. Their strings land in the same timeline
// as everything above, so leaving one live beside the inert request would just
// move the hole.
func TestInteractionSender_AppliedAndRejectedOutcomesAreInert(t *testing.T) {
	t.Run("applied: OutcomeText cannot repaint the timeline", func(t *testing.T) {
		sink := &RecordingSink{}
		s := &interactionSender{sink: newInertSink(sink)}
		_, err := s.Send(context.Background(), sessInfo(),
			mustEnv(t, channelevents.KindInteractionApplied, channelevents.InteractionAppliedPayload{
				Category: "tool_approval", RequestRef: "r-1", Outcome: "approve",
				OutcomeText: "approved" + cursorRepaint,
			}))
		require.NoError(t, err, "Send")
		m := sink.Events()[0].(MsgInteractionApplied)
		assert.NotContains(t, m.OutcomeText, "\x1b")
	})

	t.Run("rejected: Reason cannot repaint the timeline", func(t *testing.T) {
		sink := &RecordingSink{}
		s := &interactionSender{sink: newInertSink(sink)}
		_, err := s.Send(context.Background(), sessInfo(),
			mustEnv(t, channelevents.KindInteractionDecisionRejected, channelevents.InteractionDecisionRejectedPayload{
				RequestRef: "r-1", Class: "handler_error",
				Reason:          "the grant write failed" + cursorRepaint,
				OriginalOutcome: "approve" + cursorRepaint,
			}))
		require.NoError(t, err, "Send")
		m := sink.Events()[0].(MsgInteractionRejected)
		assert.NotContains(t, m.Reason, "\x1b")
		assert.NotContains(t, m.OriginalOutcome, "\x1b")
	})
}

// TestToolSessionSender_DeltaKeepsColourDropsCursorControl is the tool-output
// half, and the one place the sweep is deliberately NOT a blanket strip:
// ToolSessionDeltaPayload.Data is "the raw bytes the tool emitted"
// (channelevents), so colour is content the user asked to see, while cursor
// movement, erase and OSC are the surface's own controls and are not the
// tool's to use. That is the same line channelsd's browser surface draws — it
// parses these identical bytes through Anser into colour spans and keeps no
// cursor control at all.
func TestToolSessionSender_DeltaKeepsColourDropsCursorControl(t *testing.T) {
	// Shaped like a real tool: a coloured status line, then a repaint that
	// would erase the block header and outcome trailer the TUI drew, then a
	// window-title set, then indented output.
	raw := "\x1b[32mPASS\x1b[0m ok\n" + cursorRepaint + "  ✓ done · $0.0000\n" +
		oscTitle + "\tindented\r\n"

	sink := &RecordingSink{}
	s := &toolSessionSender{sink: newInertSink(sink)}
	_, err := s.Send(context.Background(), sessInfo(),
		mustEnv(t, channelevents.KindToolSessionDelta, channelevents.ToolSessionDeltaPayload{
			ToolCallRef: "tc-1", Stream: "stdout", Data: []byte(raw),
		}))
	require.NoError(t, err, "Send")

	m, ok := sink.Events()[0].(MsgToolSessionDelta)
	require.True(t, ok, "want MsgToolSessionDelta, got %T", sink.Events()[0])
	got := string(m.Payload.Data)

	assert.Contains(t, got, "\x1b[32m", "SGR colour is content the user asked to see")
	assert.NotContains(t, got, "\x1b[1A", "cursor movement is the surface's, not the tool's")
	assert.NotContains(t, got, "\x1b[2K", "neither is erase-line")
	assert.NotContains(t, got, "\x1b]0;", "a tool may not set the window title")
	assert.NotContains(t, got, "\r", "carriage return overwrites the line the surface prefixed")

	// The output itself must arrive intact — a strip that mangles ordinary
	// multi-line tool output is a regression, not a fix.
	assert.Contains(t, got, "PASS", "text survives")
	assert.Contains(t, got, "\n  ✓ done · $0.0000\n", "newlines and ordinary punctuation survive")
	assert.Contains(t, got, "\tindented", "tabs survive")
	// ansi.ResetStyle is the parameterless SGR reset ("\x1b[m"), equivalent to
	// "\x1b[0m" and what the sweep appends.
	assert.True(t, strings.HasSuffix(got, ansi.ResetStyle),
		"a surviving colour run is reset at the chunk's end so it cannot bleed onto the surface's own lines")
}

// A delta carrying no escape sequence at all must come through byte-identical,
// reset included — the sweep adds nothing to output that needed nothing.
func TestToolSessionSender_PlainDeltaIsUnchanged(t *testing.T) {
	sink := &RecordingSink{}
	s := &toolSessionSender{sink: newInertSink(sink)}
	_, err := s.Send(context.Background(), sessInfo(),
		mustEnv(t, channelevents.KindToolSessionDelta, channelevents.ToolSessionDeltaPayload{
			ToolCallRef: "tc-1", Stream: "stdout", Data: []byte("building…\n  step 1\n"),
		}))
	require.NoError(t, err, "Send")
	m := sink.Events()[0].(MsgToolSessionDelta)
	assert.Equal(t, []byte("building…\n  step 1\n"), m.Payload.Data)
}

// TestToolSessionSender_EventTextIsInert closes the parsed-event leg of the
// same stream. A dispatched tool in stream-json mode reaches the timeline as
// ToolSessionEventPayload.Text instead of raw Data, through the same
// appendTail path — inerting one leg and not the other would leave the defect
// reachable by changing the tool's output mode.
func TestToolSessionSender_EventTextIsInert(t *testing.T) {
	sink := &RecordingSink{}
	s := &toolSessionSender{sink: newInertSink(sink)}
	_, err := s.Send(context.Background(), sessInfo(),
		mustEnv(t, channelevents.KindToolSessionEvent, channelevents.ToolSessionEventPayload{
			ToolCallRef: "tc-1", EventType: "text_delta",
			Text:      "\x1b[32mworking\x1b[0m\n" + cursorRepaint + "  ✓ done",
			ToolName:  "Write" + cursorRepaint,
			OuterTool: "claude",
			Reason:    "write the README" + cursorRepaint,
			Summary:   "wrote it" + oscTitle,
		}))
	require.NoError(t, err, "Send")

	m := sink.Events()[0].(MsgToolSessionEvent)
	assert.Contains(t, m.Payload.Text, "\x1b[32m", "the streamed text keeps colour, like a raw delta")
	assert.NotContains(t, m.Payload.Text, "\x1b[1A", "...but not cursor movement")
	for name, got := range map[string]string{
		"toolName": m.Payload.ToolName, "reason": m.Payload.Reason, "summary": m.Payload.Summary,
	} {
		assert.NotContains(t, got, "\x1b",
			"%s is a label the surface composes into its own header — no escape sequence may survive", name)
	}
}

// TestAgentAuthoredTextIsInert covers the timeline's other three writers. The
// agent's reply is the least filtered text this kind carries — everything else
// is composed by the runner AROUND the model's output, this IS the model's
// output — and it lands in the same terminal as the tool blocks above. Leaving
// it live while the tool bytes beside it are swept would just move the hole to
// the easier target.
func TestAgentAuthoredTextIsInert(t *testing.T) {
	t.Run("user_message: the agent's reply cannot repaint the timeline", func(t *testing.T) {
		sink := &RecordingSink{}
		s := &localSender{sink: newInertSink(sink)}
		_, err := s.Send(context.Background(), sessInfo(),
			mustEnv(t, channelevents.KindUserMessage, channelevents.OutboundUserMessagePayload{
				Text: "Done." + cursorRepaint + "  ✓ approved by you",
			}))
		require.NoError(t, err, "Send")
		m := sink.Events()[0].(MsgUserMessage)
		assert.NotContains(t, m.Text, "\x1b")
		assert.Contains(t, m.Text, "Done.", "the reply itself still arrives")
	})

	t.Run("notification: an update_status caption cannot repaint the status line", func(t *testing.T) {
		sink := &RecordingSink{}
		s := &localSender{sink: newInertSink(sink)}
		_, err := s.Send(context.Background(), sessInfo(),
			mustEnv(t, channelevents.KindNotification, channelevents.NotificationPayload{
				Text: "Working" + cursorRepaint, Short: "Working" + oscTitle,
			}))
		require.NoError(t, err, "Send")
		m := sink.Events()[0].(MsgNotification)
		assert.NotContains(t, m.Text, "\x1b")
		assert.NotContains(t, m.Short, "\x1b")
	})

	t.Run("stream delta: a streamed token run cannot repaint the timeline", func(t *testing.T) {
		sink := &RecordingSink{}
		s := &streamDeltaSink{sink: newInertSink(sink)}
		require.NoError(t, s.OnDelta(context.Background(), sessInfo(),
			mustEnv(t, channelevents.KindAssistantStreamDelta, channelevents.AssistantStreamDeltaPayload{
				EventType: "text_delta", Text: "thinking…" + cursorRepaint,
			})), "OnDelta")
		m := sink.Events()[0].(MsgStreamDelta)
		assert.NotContains(t, m.Payload.Text, "\x1b")
	})

	// The chunking case the streamed path uniquely has: the surface
	// concatenates deltas, so a sequence split across two of them must not be
	// reassembled into a live one.
	t.Run("stream delta: a sequence split across two chunks is not reassembled", func(t *testing.T) {
		sink := &RecordingSink{}
		s := &streamDeltaSink{sink: newInertSink(sink)}
		for _, chunk := range []string{"ok\x1b[1", "A\x1b[2Kforged"} {
			require.NoError(t, s.OnDelta(context.Background(), sessInfo(),
				mustEnv(t, channelevents.KindAssistantStreamDelta, channelevents.AssistantStreamDeltaPayload{
					EventType: "text_delta", Text: chunk,
				})), "OnDelta")
		}
		var joined string
		for _, ev := range sink.Events() {
			joined += ev.(MsgStreamDelta).Payload.Text
		}
		assert.NotContains(t, joined, "\x1b",
			"the halves must not rejoin into a live escape sequence in the timeline")
	})
}

// TestInertToolOutput_SequenceClasses pins the keep/drop line one sequence
// class at a time — the cases a sender-level test cannot reach, and the ones a
// hand-rolled scanner gets wrong. inertText is asserted alongside on the rows
// where the two must differ, since "colour survives here and not there" is the
// whole judgement call.
func TestInertToolOutput_SequenceClasses(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "24-bit colour survives: it is content the user ran a tool to see",
			in:   "\x1b[38;2;255;0;0mred\x1b[0m",
			want: "\x1b[38;2;255;0;0mred\x1b[0m" + ansi.ResetStyle,
		},
		{
			name: "the alternate-screen private mode is dropped: a tool may not take over the display",
			in:   "\x1b[?1049hgone",
			want: "gone",
		},
		{
			name: "a full terminal reset (ESC c) is dropped",
			in:   "before\x1bcafter",
			want: "beforeafter",
		},
		{
			name: "an OSC that never terminates is dropped whole, not left half-printed",
			in:   "ok\x1b]0;no terminator here",
			want: "ok",
		},
		{
			name: "a CSI left unterminated by a chunk boundary is dropped, not reassembled",
			in:   "ok\n\x1b[1",
			want: "ok\n",
		},
		{
			name: "a C1 CSI written as UTF-8 loses its introducer, leaving the repaint as inert text",
			in:   "ok1A2Kforged",
			want: "ok1A2Kforged",
		},
		{
			name: "multi-byte characters and tabs pass through untouched",
			in:   "\tビルド中… ✓\n",
			want: "\tビルド中… ✓\n",
		},
		{
			name: "backspace and vertical tab are dropped; they move the cursor too",
			in:   "do\bne\vhere",
			want: "donehere",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, string(inertToolOutput([]byte(tc.in))))
		})
	}
}

// Both sweeps must be IDEMPOTENT, because the Emit door (inert_sink.go) cannot
// know whether the value it was handed has already been through them — the
// interaction sender deliberately sweeps its payload early so RenderText sees
// swept text, and the door then sees it a second time. The tool-output sweep is
// the one that could drift: it terminates a chunk that kept colour with an
// explicit reset, and appending one per pass would grow every chunk.
func TestInertSweeps_AreIdempotent(t *testing.T) {
	for _, in := range []string{
		"\x1b[32mPASS\x1b[0m ok\n" + cursorRepaint + "trailer",
		"plain prose\nwith a tab\there",
		"\x1b[31mred to the end",
	} {
		once := string(inertToolOutput([]byte(in)))
		assert.Equal(t, once, string(inertToolOutput([]byte(once))),
			"inertToolOutput must be a fixed point after one pass")

		onceText := inertText(in)
		assert.Equal(t, onceText, inertText(onceText),
			"inertText must be a fixed point after one pass")
	}
}

// TestKeepsToolSGR_NamesFieldsThatStillExist guards the one place the door
// still needs a name rather than a shape. A rename on either wire payload would
// silently drop this exception — colour lost on real tool output, which is a
// visible regression rather than a hole, but still worth catching here rather
// than by a user noticing their test output went monochrome.
func TestKeepsToolSGR_NamesFieldsThatStillExist(t *testing.T) {
	for _, tc := range []struct {
		typ   reflect.Type
		field string
	}{
		{reflect.TypeOf(channelevents.ToolSessionDeltaPayload{}), "Data"},
		{reflect.TypeOf(channelevents.ToolSessionEventPayload{}), "Text"},
	} {
		_, ok := tc.typ.FieldByName(tc.field)
		require.True(t, ok, "%s has no field %q — the tool-output exception is stale", tc.typ, tc.field)
		assert.True(t, keepsToolSGR(tc.typ, tc.field), "%s.%s must keep colour", tc.typ, tc.field)
	}
	assert.False(t, keepsToolSGR(reflect.TypeOf(channelevents.ToolSessionEventPayload{}), "Summary"),
		"a header label the surface composes is not the tool's output")
	assert.False(t, keepsToolSGR(reflect.TypeOf(channelevents.AssistantStreamDeltaPayload{}), "Text"),
		"the model's own narrative is not tool output either")
}

// The one place inertText and inertToolOutput deliberately disagree. A payload
// slot is a publisher's sentence laid out by the surface, so colour there is
// never something a reader asked for; a tool's output is the tool's own.
func TestInertText_DropsColourThatToolOutputKeeps(t *testing.T) {
	const coloured = "\x1b[31mattention\x1b[0m please"
	assert.Equal(t, "attention please", inertText(coloured),
		"a payload slot carries no colour of its own")
	assert.Equal(t, "\x1b[31mattention\x1b[0m please"+ansi.ResetStyle, string(inertToolOutput([]byte(coloured))),
		"the same bytes from a tool keep their colour")
}

// decisionShapedPayload mirrors what host_approval.go publishes for a tool
// approval: a platform Lead, label/value fields carrying LLM-supplied prose,
// and Approve/Deny decision actions — the shape that opens the TUI's BLOCKING
// modal, where the keybinding hints are drawn one line below this payload.
func decisionShapedPayload(mutate func(*channelevents.InteractionRequestPayload)) channelevents.InteractionRequestPayload {
	p := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: "default", Name: "s1"},
		Category:        "tool_approval",
		RequestRef:      "req-inert-1",
		Lead:            "Approval needed",
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision},
			{ID: "deny", Label: "Deny", Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceRequester,
			Requester: &channelevents.ExternalIdentity{Kind: "user", ExternalID: "user-1"},
		},
	}
	mutate(&p)
	return p
}

// payloadText concatenates every text slot of a payload the terminal renders,
// so one assertion covers the whole struct rather than the one field a row
// happened to set.
func payloadText(p channelevents.InteractionRequestPayload) string {
	var b strings.Builder
	b.WriteString(p.Lead + "\n" + p.Body + "\n" + p.NextStep + "\n")
	for _, f := range p.Fields {
		b.WriteString(f.Label + "\n" + f.Value + "\n")
	}
	if p.Excerpt != nil {
		b.WriteString(p.Excerpt.Label + "\n" + p.Excerpt.Content + "\n")
	}
	for _, a := range p.Actions {
		b.WriteString(a.Label + "\n" + a.URL + "\n")
	}
	return b.String()
}
