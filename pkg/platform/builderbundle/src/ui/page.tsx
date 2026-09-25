// The agent-builder workshop page (spec §11). Compiled by `mage ui:compile`
// into page.view.json, which Bundle() splices into the AgentUI manifest's
// spec.view; the actions the form and the table name are declared in
// manifests/agentui.yaml. The page is a rail-and-stage page (`oap:page
// layout="rail"`): the timeline renders as a vertical rail down the left,
// and the stage beside it shows the hooks bound to whichever step is
// selected — the active step by default, or a finished step the builder
// clicks back to. Hooks are the regions the builder repaints with
// update_view; each hook's intent is the standing instruction the builder
// reads beside it. The page's regions are the timeline (the rail),
// `questions` (no step, so it always sits at the top of the stage), `agent`
// (bound to the assess step: the person's description in their own words,
// plus the facts settled from it), and the four phase hooks (tools,
// permissions, testRun, deliver). Only the timeline and the agent hook's
// default card exist before the builder speaks; `questions` starts empty
// and the four phase hooks have no default, so they do not exist on screen
// until the builder reaches that phase. That default card carries no title
// of its own: the agent hook's panel is already titled "Your agent", and a
// second heading over the invitation is two headings on an empty page.
//
// A hook bound to a step (`step`) shows only while that step is selected on
// the rail; a hook with no step, like `questions`, always shows. A step's
// hook simply stays where it is on the stage, repainted in place, so the
// builder never has to clear a finished region and the rail stays in view
// the whole time.
export default (
  <oap:page layout="rail">
    <ap:heading text="Agent Builder" level={2} />
    <oap:generative
      name="phase"
      intent="The stage timeline: Intake, Tools, Permissions, Build, Test, Deliver. Exactly one step is active, the ones before it done, the ones after upcoming. Repaint it with the next step active each time you move to a new phase, keeping every step's id and label exactly as they are, and keeping it pinned — change only state. A finished step stays reachable from the timeline. Every step carries a summary — a few words of fact (the agent's name and purpose, 'none', 'valid · 12:48') — keep each one current when you repaint."
      allowedComponents={["ap:steps"]}
    >
      <ap:steps
        pinned
        steps={[
          { id: "assess", label: "Intake", state: "active", summary: "tell me what to build" },
          { id: "tools", label: "Tools", state: "upcoming", summary: "what it will use" },
          { id: "permissions", label: "Permissions", state: "upcoming", summary: "what it may do" },
          { id: "build", label: "Build", state: "upcoming", summary: "not built yet" },
          { id: "test", label: "Test", state: "upcoming", summary: "not started" },
          { id: "deliver", label: "Deliver", state: "upcoming", summary: "nothing to deliver yet" },
        ]}
      />
    </oap:generative>
    <oap:generative
      name="questions"
      intent="Where you ask the person something. Put one ap:question here, then wait for the answer; clear this hook once it is answered. Keep it empty when you have nothing to ask."
      allowedComponents={["ap:question", "ap:markdown"]}
    />
    <oap:generative
      name="agent"
      intent="The agent as the person described it and as you have settled it. After the description arrives, repaint this hook as an ap:card with no title (the panel is already titled 'Your agent'): an ap:heading 'In your words' (level 4) with an ap:button labelled 'Edit' (action edit_description) beside it, the person's description verbatim as an ap:markdown blockquote, then an ap:heading 'What we've settled' (level 4) over an ap:markdown list of the facts you have confirmed so far (what it does, who it is for, where it runs, the example requests) — grow the list as you learn; a fact you are asking about now reads '— asking you now'. When they press Edit, this hook shows the describe form again with `values` carrying their current words; leave it there until they submit, then put the text back. Never clear this hook."
      allowedComponents={["*"]}
      step="assess"
      title="Your agent"
    >
      <ap:card>
        <ap:markdown body="Tell me what this agent should do in a sentence or two — the job it does, who it does it for, and where it runs. I'll ask follow-up questions one at a time, then help you connect any tools it needs, set its permissions, and test it before you install it." />
        <ap:form
          action="describe_agent"
          submitLabel="Start building"
          fields={[
            {
              name: "description",
              kind: "textarea",
              label: "What should this agent do?",
              placeholder: "e.g. Summarize my team's open GitHub pull requests every morning and post the digest to our Slack channel.",
            },
          ]}
        />
      </ap:card>
    </oap:generative>
    <oap:generative
      name="tools"
      intent="The tools the agent will use, once the Tools phase has settled them: a table with one row per tool (tool, source, status), or a short note while there are none. No row action: linking accounts is the platform's job, never a control here. Empty until then."
      allowedComponents={["*"]}
      step="tools"
    />
    <oap:generative
      name="permissions"
      intent="What the agent may do on its own, only after asking first, and never — the will/won't/will-ask-first summary the person confirmed in the Permissions phase. Empty until then."
      allowedComponents={["*"]}
      step="permissions"
    />
    <oap:generative
      name="testRun"
      intent="The Test phase. Its first line is always an ap:status that is true right now (Not started / Running since / Still running; I stopped watching at / Waiting on you since / Ended at). Its text carries no clock: the moment goes in `since`, copied from the time in parentheses on the line you were told, and the page draws it in the person's own reading of the time. Beneath it: the ap:agentlink from workshop_test_link with embed set while no test runs; the ap:chat for the running test; the example requests as plain text; the ap:notice with its buttons (test_done, thats_not_right, stop_test) while you watch; keep_watching after the watch times out; and, once it ended, what the agent actually did and a start_fresh_test button labelled 'Start a fresh test'. Empty until the agent is valid."
      allowedComponents={["*"]}
      step="test"
    />
    <oap:generative
      name="deliver"
      intent="The outcome: a heading '<name> is ready', one line saying the draft is a saved copy of what was tested, the ap:attachment for the exported draft (its artifact prop is the artifactId workshop_export_draft returned), three buttons — install_for_real 'Install for real', hand_off 'Hand the draft to someone', test_again 'Test it again' — and one closing line that nothing runs until the person approves an install. Empty until the draft exists."
      allowedComponents={["*"]}
      step="deliver"
    />
  </oap:page>
);
