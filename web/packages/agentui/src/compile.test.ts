import { describe, expect, it } from "vitest";
import { CompileError, compilePage } from "./compile";

// The spec's §9 example, verbatim in spirit: heading, a timeline hook with an
// author default, a card holding an empty hook, and an intake hook whose
// default uses an element the AGENT may not (ap:card under markdown+form).
const examplePage = `export default (
  <ap:stack direction="vertical" gap="sm">
    <ap:heading text="Agent Builder" level={2} />
    <oap:generative
      name="phase"
      intent="The stage timeline. Advance the active step as you progress."
      allowedComponents={["ap:steps"]}
    >
      <ap:steps steps={[{ label: "Intake", state: "active" }, { label: "Tools", state: "upcoming" }]} />
    </oap:generative>
    <ap:card title="Your agent so far">
      <oap:generative name="brief" intent="The running summary." allowedComponents={["*"]}>
        {/* empty */}
      </oap:generative>
    </ap:card>
    {/* The initial text field; the agent clears it once underway. */}
    <oap:generative
      name="intake"
      intent="Where the person first describes the agent."
      allowedComponents={["ap:markdown", "ap:form"]}
    >
      <ap:card title="Describe the agent you want to build">
        <ap:markdown body="Tell me what this agent should do." />
        <ap:form
          action="describe_agent"
          submitLabel="Start building"
          fields={[{ name: "description", kind: "textarea", label: "What should this agent do?" }]}
        />
      </ap:card>
    </oap:generative>
  </ap:stack>
);
`;

describe("compilePage", () => {
  it("compiles the example page to the node grammar", () => {
    expect(compilePage(examplePage)).toEqual({
      component: "ap:stack",
      props: { direction: "vertical", gap: "sm" },
      children: [
        { component: "ap:heading", props: { text: "Agent Builder", level: 2 } },
        {
          component: "oap:generative",
          props: {
            name: "phase",
            intent: "The stage timeline. Advance the active step as you progress.",
            allowedComponents: ["ap:steps"],
          },
          children: [
            {
              component: "ap:steps",
              props: { steps: [{ label: "Intake", state: "active" }, { label: "Tools", state: "upcoming" }] },
            },
          ],
        },
        {
          component: "ap:card",
          props: { title: "Your agent so far" },
          children: [
            {
              component: "oap:generative",
              props: { name: "brief", intent: "The running summary.", allowedComponents: ["*"] },
            },
          ],
        },
        {
          component: "oap:generative",
          props: {
            name: "intake",
            intent: "Where the person first describes the agent.",
            allowedComponents: ["ap:markdown", "ap:form"],
          },
          children: [
            {
              component: "ap:card",
              props: { title: "Describe the agent you want to build" },
              children: [
                { component: "ap:markdown", props: { body: "Tell me what this agent should do." } },
                {
                  component: "ap:form",
                  props: {
                    action: "describe_agent",
                    submitLabel: "Start building",
                    fields: [{ name: "description", kind: "textarea", label: "What should this agent do?" }],
                  },
                },
              ],
            },
          ],
        },
      ],
    });
  });

  it("compiles oap:page as the root with its layout", () => {
    const node = compilePage(`export default (
      <oap:page layout="rail">
        <ap:heading text="Agent Builder" level={2} />
      </oap:page>
    );`);
    expect(node).toEqual({
      component: "oap:page",
      props: { layout: "rail" },
      children: [{ component: "ap:heading", props: { text: "Agent Builder", level: 2 } }],
    });
  });

  it("accepts every literal form and omits empty props/children", () => {
    const node = compilePage(
      `export default (<ap:x s="str" n={2} neg={-1} t={true} f={false} nil={null} flag arr={[1, "a", {k: "v"}]} obj={{ "quoted": [true], plain: null }} />);`,
    );
    expect(node).toEqual({
      component: "ap:x",
      props: {
        s: "str",
        n: 2,
        neg: -1,
        t: true,
        f: false,
        nil: null,
        flag: true,
        arr: [1, "a", { k: "v" }],
        obj: { quoted: [true], plain: null },
      },
    });
    expect(compilePage(`export default <ap:empty />;`)).toEqual({ component: "ap:empty" });
    expect(compilePage(`export default (<ap:stack></ap:stack>);`)).toEqual({ component: "ap:stack" });
  });

  it("does not mistake an Object.prototype member name for an already-seen attribute", () => {
    // toEqual on an object with an own `toString` string property is fine —
    // it compares own enumerable properties, not the prototype chain.
    expect(compilePage(`export default <ap:x toString="hi" constructor={1} />;`)).toEqual({
      component: "ap:x",
      props: { toString: "hi", constructor: 1 },
    });
  });

  it("keeps a plain string attribute verbatim, entities included", () => {
    expect(compilePage(`export default <ap:text text="a &amp; b" />;`)).toEqual({
      component: "ap:text",
      props: { text: "a &amp; b" },
    });
    expect(compilePage(`export default <ap:text text={"a & b"} />;`)).toEqual({
      component: "ap:text",
      props: { text: "a & b" },
    });
  });

  it("lifts the bindings attribute into the node's bindings, never its props", () => {
    expect(
      compilePage(
        `export default (<ap:table columns={["name"]} bindings={{ rows: { source: "tool", ref: "crm_list", args: { limit: 5 }, select: "results[]" } }} />);`,
      ),
    ).toEqual({
      component: "ap:table",
      props: { columns: ["name"] },
      bindings: { rows: { source: "tool", ref: "crm_list", args: { limit: 5 }, select: "results[]" } },
    });
  });

  it("treats an empty bindings object as absent, the same as empty props", () => {
    expect(compilePage(`export default <ap:x a="1" bindings={{}} />;`)).toEqual({
      component: "ap:x",
      props: { a: "1" },
    });
  });

  it("ignores comments, including the printer's markers", () => {
    const node = compilePage(`export default (
      <oap:generative name="h" intent="i" allowedComponents={["*"]}>
        {/* your fill */}
        {/* empty: you cleared this hook */}
        <ap:text text="t" />
      </oap:generative>
    );`);
    expect(node.children).toEqual([{ component: "ap:text", props: { text: "t" } }]);
    expect(compilePage(`export default (<ap:x>{/* empty */}</ap:x>);`)).toEqual({ component: "ap:x" });
  });

  const refusals: [string, string, RegExp][] = [
    ["an identifier", `export default <ap:x a={foo} />;`, /`foo` is an identifier, not a literal/],
    ["undefined", `export default <ap:x a={undefined} />;`, /`undefined` is an identifier, not a literal/],
    ["a shorthand object property", `export default <ap:x a={{ label }} />;`, /`label` is an identifier, not a literal/],
    ["a call (a hook)", `export default <ap:x a={useState(0)} />;`, /call expressions are not allowed \(`useState`\)/],
    ["member access", `export default <ap:x a={config.title} />;`, /member expressions are not allowed/],
    ["a template literal", "export default <ap:x a={`t`} />;", /template literals are not allowed/],
    ["a conditional", `export default <ap:x a={true ? 1 : 2} />;`, /conditionals are not allowed/],
    ["a function", `export default <ap:x a={() => 1} />;`, /functions are not allowed/],
    ["a spread attribute", `export default <ap:x {...props} />;`, /spread attributes are not allowed/],
    ["an array spread", `export default <ap:x a={[...rows]} />;`, /spreads are not allowed/],
    ["an object spread", `export default <ap:x a={{ ...base }} />;`, /spreads are not allowed/],
    ["a computed key", `export default <ap:x a={{ [k]: 1 }} />;`, /computed keys are not allowed/],
    ["a __proto__ key", `export default <ap:x a={{ __proto__: { b: 1 } }} />;`, /`__proto__` is not allowed as a key/],
    ["a __proto__ attribute", `export default <ap:x __proto__="1" />;`, /`__proto__` is not allowed as an attribute name/],
    ["an import", `import React from "react";\nexport default <ap:x />;`, /imports are not allowed/],
    ["a second top-level statement", `const t = 1;\nexport default <ap:x />;`, /only `export default <element>` is allowed at the top level/],
    ["no default export", `const page = <ap:x />;`, /only `export default <element>` is allowed at the top level/],
    ["a non-element default export", `export default 1;`, /the default export must be a single element/],
    ["a fragment", `export default <><ap:x /></>;`, /fragments are not allowed/],
    ["a tag outside the vocabulary's namespaces", `export default <div />;`, /`div` is not a platform element/],
    ["a React-style component tag", `export default <Card />;`, /`Card` is not a platform element/],
    ["a member-expression tag", `export default <Foo.Bar />;`, /`Foo\.Bar` is not a platform element/],
    ["type arguments", `export default <ap:x<string> />;`, /type arguments are not allowed/],
    ["a text child", `export default <ap:card>hello</ap:card>;`, /text children are not supported/],
    ["an expression child", `export default <ap:card>{title}</ap:card>;`, /expression children are not allowed/],
    ["a spread child", `export default <ap:card>{...items}</ap:card>;`, /spread children are not allowed/],
    ["a duplicate attribute", `export default <ap:x a="1" a="2" />;`, /attribute `a` is given twice/],
    ["bindings that are not an object literal", `export default <ap:x bindings="rows" />;`, /`bindings` must be an object literal/],
    ["a syntax error", `export default <ap:x>;`, /./],
  ];
  it.each(refusals)("refuses %s by name, with a position", (_name, src, want) => {
    let err: unknown;
    try {
      compilePage(src, "page.tsx");
    } catch (e) {
      err = e;
    }
    expect(err).toBeInstanceOf(CompileError);
    const ce = err as CompileError;
    expect(ce.message).toMatch(want);
    expect(ce.message).toMatch(/^\d+:\d+: /);
    expect(ce.line).toBeGreaterThan(0);
    expect(ce.column).toBeGreaterThan(0);
  });

  it("reports the position of the offending construct, not the file start", () => {
    const src = `export default (\n  <ap:stack>\n    <ap:x a={foo} />\n  </ap:stack>\n);`;
    expect(() => compilePage(src)).toThrow(/^3:\d+: /);
  });
});
