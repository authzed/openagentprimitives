export default (
  <oap:page layout="rail">
    <ap:stack direction="vertical" gap="sm">
      <ap:heading level={2} text="Round trip" />
      <ap:table columns={[{"key":"name","header":"Name"}]} empty="Nothing yet." bindings={{"rows":{"source":"memory","ref":"demo_list","args":{"limit":5},"select":"results[]"}}} />
      <ap:button action="demo_go" disabled={true} label="Go" />
      <ap:agentlink agentClass="demo-agent" label="Try it as yourself" namespace="ws-demo" prompt="linear issues for PR #1234" />
      <ap:collapsible collapsed={true} title="Details">
        <ap:notice body="Test session started." buttons={[{"label":"Done","action":"demo_go"}]} tone="info" />
      </ap:collapsible>
      <oap:generative name="phase" intent="The stage timeline." allowedComponents={["ap:steps"]}>
        <ap:steps pinned={true} steps={[{"id":"intake","label":"Intake","state":"active","summary":"in progress"},{"id":"tools","label":"Tools","state":"upcoming"}]} />
      </oap:generative>
      <ap:card title="Your agent so far">
        <oap:generative name="brief" intent="The running summary." allowedComponents={["*"]} step="intake">
          {/* empty */}
        </oap:generative>
      </ap:card>
      <oap:generative name="intake" intent="Where the person first describes the agent." allowedComponents={["ap:markdown","ap:form"]}>
        <ap:card title="Describe it">
          <ap:markdown body="Tell me what it should do — in a sentence or two." />
        </ap:card>
      </oap:generative>
    </ap:stack>
  </oap:page>
);
