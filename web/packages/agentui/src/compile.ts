// compile.ts is the template compiler: a page written as JSX in the platform's
// elements becomes the node grammar the AgentUI CR carries ({component, props,
// bindings, children}). It is the inverse of pkg/web/uicomponents/jsx.go's
// printer, and the two are pinned as inverses over one fixture
// (compile.roundtrip.test.ts / jsx_roundtrip_test.go).
//
// It is a TEMPLATE compiler, deliberately: elements → component, attributes →
// props (literals only), nested elements → children. Anything else — an
// identifier, a call, state, an effect, an import — is a CompileError that
// names the construct. That restriction is what keeps the runtime trust
// boundary: the CR only ever carries nodes, validated by the Go registry at
// admission. This file therefore validates SHAPE only; component names and
// referenced actions are checked in Go (uicomponents.Validate), once.
//
// It uses the `typescript` package's PARSER only (no type checker, no emit),
// and imports nothing that renders, so the same module runs under plain Node
// for the CLI (tools/compile-page.ts) and under vitest.
import ts from "typescript";

export interface CompiledNode {
  component: string;
  props?: Record<string, unknown>;
  bindings?: Record<string, unknown>;
  children?: CompiledNode[];
}

// CompileError names the construct the page used that the grammar does not
// admit, at its 1-based line:column. The message carries the position first so
// a CLI can print `file:line:col: text` and an editor can jump to it.
export class CompileError extends Error {
  // Plain fields, not TS parameter properties: the CLI (tools/compile-page.ts)
  // runs this module under `node --experimental-strip-types`, whose
  // strip-only mode erases type annotations but cannot lower a parameter
  // property to an assignment — it does not run the JSX/JS this file compiles
  // through any code-generating transform.
  readonly line: number;
  readonly column: number;
  constructor(text: string, line: number, column: number) {
    super(`${line}:${column}: ${text}`);
    this.name = "CompileError";
    this.line = line;
    this.column = column;
  }
}

// The element namespaces of the vocabulary. `ap:*` are the components; the
// `oap:` namespace holds the two structural elements — `oap:generative` (a
// hook) and `oap:page` (the root). Names are checked in Go at admission.
const ELEMENT_NAMESPACES = new Set(["ap", "oap"]);

// compilePage compiles one page: exactly `export default <element>` and
// nothing else at the top level.
export function compilePage(
  source: string,
  fileName = "page.tsx",
): CompiledNode {
  const sf = ts.createSourceFile(
    fileName,
    source,
    ts.ScriptTarget.Latest,
    true,
    ts.ScriptKind.TSX,
  );
  const fail = (node: ts.Node, text: string): never => {
    const { line, character } = sf.getLineAndCharacterOfPosition(
      node.getStart(sf),
    );
    throw new CompileError(text, line + 1, character + 1);
  };
  // `parseDiagnostics` is an internal TypeScript field, not part of the
  // public `typescript.d.ts` surface — hence the cast through `unknown`. If a
  // future TypeScript upgrade renames or moves it, this silently degrades to
  // "no syntax errors reported" rather than a compile error; the "a syntax
  // error" refusal row in compile.test.ts is the tripwire that would catch
  // that regression.
  const diags =
    (sf as unknown as { parseDiagnostics?: ts.DiagnosticWithLocation[] })
      .parseDiagnostics ?? [];
  if (diags.length > 0) {
    const d = diags[0];
    const { line, character } = sf.getLineAndCharacterOfPosition(d.start);
    throw new CompileError(
      `syntax error: ${ts.flattenDiagnosticMessageText(d.messageText, " ")}`,
      line + 1,
      character + 1,
    );
  }

  let exported: ts.ExportAssignment | undefined;
  for (const st of sf.statements) {
    if (ts.isImportDeclaration(st) || ts.isImportEqualsDeclaration(st)) {
      fail(
        st,
        "imports are not allowed: the page is written in the platform's elements and nothing else",
      );
    }
    if (ts.isExportAssignment(st) && !st.isExportEquals && !exported) {
      exported = st;
      continue;
    }
    fail(
      st,
      `only \`export default <element>\` is allowed at the top level; found ${ts.SyntaxKind[st.kind]}`,
    );
  }
  if (!exported) {
    throw new CompileError(
      "only `export default <element>` is allowed at the top level; the page has no default export",
      1,
      1,
    );
  }

  const root = unparen(exported.expression);
  if (ts.isJsxFragment(root))
    fail(root, "fragments are not allowed: the page has one root element");
  if (!ts.isJsxElement(root) && !ts.isJsxSelfClosingElement(root)) {
    fail(
      root,
      `the default export must be a single element; found ${ts.SyntaxKind[root.kind]}`,
    );
  }
  // The two checks above have already ruled out anything but these two kinds
  // (or thrown); `Expression` is an interface, not a TS union, so the
  // compiler cannot narrow it the way it narrows a real union alias.
  return compileElement(
    root as ts.JsxElement | ts.JsxSelfClosingElement,
    sf,
    fail,
  );
}

type Fail = (node: ts.Node, text: string) => never;

function unparen(e: ts.Expression): ts.Expression {
  while (ts.isParenthesizedExpression(e)) e = e.expression;
  return e;
}

function compileElement(
  el: ts.JsxElement | ts.JsxSelfClosingElement,
  sf: ts.SourceFile,
  fail: Fail,
): CompiledNode {
  const opening = ts.isJsxElement(el) ? el.openingElement : el;
  // TS admits type arguments on a JSX tag (`<ap:x<T> />`) — a generic React
  // component's, which nothing here is. The node grammar has nowhere to put
  // them, so naming them beats compiling the tag with the construct dropped.
  if (opening.typeArguments)
    fail(opening, "type arguments are not allowed on an element");
  const component = elementName(opening.tagName, fail);
  const node: CompiledNode = { component };

  const props: Record<string, unknown> = {};
  let bindings: Record<string, unknown> | undefined;
  // Attribute names come from user-authored pages, so `props` (a plain
  // object) cannot be trusted as a seen-set: `name in props` is true for
  // "toString", "constructor", etc. via the prototype chain even though
  // that attribute was never given. Track seen names in a real Set instead.
  const seen = new Set<string>();
  for (const attr of opening.attributes.properties) {
    if (ts.isJsxSpreadAttribute(attr))
      fail(attr, "spread attributes are not allowed");
    const name = attr.name.getText(sf);
    if (seen.has(name)) {
      fail(attr, `attribute \`${name}\` is given twice`);
    }
    seen.add(name);
    // Same reason the object literal refuses the key: `props` is a plain
    // object, so `props["__proto__"] = v` sets its prototype instead of
    // adding a prop and the attribute vanishes from the compiled node.
    if (name === "__proto__")
      fail(attr, "`__proto__` is not allowed as an attribute name");
    const value = attributeValue(attr, sf, fail);
    if (name === "bindings") {
      if (value === null || typeof value !== "object" || Array.isArray(value)) {
        fail(
          attr,
          "`bindings` must be an object literal: {prop: {source, ref, args?, select?}}",
        );
      }
      bindings = value as Record<string, unknown>;
      continue;
    }
    props[name] = value;
  }
  if (Object.keys(props).length > 0) node.props = props;
  // An empty bindings object is treated as absent, the same as empty props:
  // the Go printer never emits an empty `bindings` attribute, so this keeps
  // the round trip symmetric.
  if (bindings && Object.keys(bindings).length > 0) node.bindings = bindings;

  if (ts.isJsxElement(el)) {
    const children: CompiledNode[] = [];
    for (const child of el.children) {
      if (ts.isJsxText(child)) {
        if (child.text.trim() !== "") {
          fail(
            child,
            "text children are not supported: text is a prop (ap:text text=…, ap:markdown body=…)",
          );
        }
        continue;
      }
      if (ts.isJsxExpression(child)) {
        if (child.dotDotDotToken)
          fail(child, "spread children are not allowed");
        if (child.expression === undefined) continue; // {/* a comment */}
        fail(
          child,
          "expression children are not allowed: a child is an element",
        );
      }
      if (ts.isJsxFragment(child)) fail(child, "fragments are not allowed");
      if (ts.isJsxElement(child) || ts.isJsxSelfClosingElement(child)) {
        children.push(compileElement(child, sf, fail));
        continue;
      }
      // JsxChild is a real union and every member is handled above, so the
      // compiler narrows child to `never` here; it still has a `.kind` as a
      // plain ts.Node, this line just outlives any future JsxChild variant.
      fail(
        child,
        `${ts.SyntaxKind[(child as ts.Node).kind]} is not allowed as a child`,
      );
    }
    if (children.length > 0) node.children = children;
  }
  return node;
}

// elementName admits only namespaced tags in the vocabulary's namespaces. A
// bare identifier (`<div>`, `<Card>`) is either HTML or a React component —
// neither exists here, and saying so beats a registry error at admission.
function elementName(tag: ts.JsxTagNameExpression, fail: Fail): string {
  if (ts.isJsxNamespacedName(tag)) {
    const ns = tag.namespace.text;
    if (!ELEMENT_NAMESPACES.has(ns)) {
      fail(
        tag,
        `\`${ns}:${tag.name.text}\` is not a platform element: elements are ap:*, the hook oap:generative and the root oap:page`,
      );
    }
    return `${ns}:${tag.name.text}`;
  }
  const text = ts.isIdentifier(tag) ? tag.text : tag.getText();
  return fail(
    tag,
    `\`${text}\` is not a platform element: elements are ap:*, the hook oap:generative and the root oap:page`,
  );
}

function attributeValue(
  attr: ts.JsxAttribute,
  sf: ts.SourceFile,
  fail: Fail,
): unknown {
  const init = attr.initializer;
  if (init === undefined) return true; // <ap:x flag /> is flag={true}
  if (ts.isStringLiteral(init)) return init.text; // verbatim; entities are not decoded
  if (ts.isJsxExpression(init)) {
    if (init.expression === undefined)
      fail(init, "an attribute needs a value: `a={}` is empty");
    return literal(init.expression, sf, fail);
  }
  return fail(init, `${ts.SyntaxKind[init.kind]} is not a literal`);
}

// literal is the whole expressible surface: string, number, boolean, null,
// and arrays/objects of the same. Every other expression is refused by name.
function literal(expr: ts.Expression, sf: ts.SourceFile, fail: Fail): unknown {
  const e = unparen(expr);
  if (ts.isStringLiteral(e)) return e.text;
  if (ts.isNumericLiteral(e)) return Number(e.text);
  if (e.kind === ts.SyntaxKind.TrueKeyword) return true;
  if (e.kind === ts.SyntaxKind.FalseKeyword) return false;
  if (e.kind === ts.SyntaxKind.NullKeyword) return null;
  if (
    ts.isPrefixUnaryExpression(e) &&
    e.operator === ts.SyntaxKind.MinusToken &&
    ts.isNumericLiteral(e.operand)
  ) {
    return -Number(e.operand.text);
  }
  if (ts.isArrayLiteralExpression(e)) {
    return e.elements.map((el) => {
      if (ts.isSpreadElement(el)) return fail(el, "spreads are not allowed");
      if (ts.isOmittedExpression(el))
        return fail(el, "an array hole is not a literal");
      return literal(el, sf, fail);
    });
  }
  if (ts.isObjectLiteralExpression(e)) {
    const out: Record<string, unknown> = {};
    for (const p of e.properties) {
      if (ts.isShorthandPropertyAssignment(p)) {
        fail(
          p,
          `\`${p.name.text}\` is an identifier, not a literal: write ${p.name.text}: "…"`,
        );
      }
      if (ts.isSpreadAssignment(p)) fail(p, "spreads are not allowed");
      if (!ts.isPropertyAssignment(p))
        fail(p, `${ts.SyntaxKind[p.kind]} is not allowed in an object literal`);
      if (ts.isComputedPropertyName(p.name))
        fail(p.name, "computed keys are not allowed");
      const key =
        ts.isIdentifier(p.name) ||
        ts.isStringLiteral(p.name) ||
        ts.isNumericLiteral(p.name)
          ? p.name.text
          : fail(p.name, `${ts.SyntaxKind[p.name.kind]} is not a literal key`);
      // `out` is a plain object, so `out["__proto__"] = v` would set its
      // prototype instead of adding a key and JSON.stringify would drop the
      // property — a silent drop in a compiler whose charter is to refuse by
      // name. The key is refused rather than the object built prototype-less,
      // so the page's author is told the property went nowhere.
      if (key === "__proto__") fail(p, "`__proto__` is not allowed as a key");
      out[key] = literal(p.initializer, sf, fail);
    }
    return out;
  }
  if (ts.isIdentifier(e))
    return fail(e, `\`${e.text}\` is an identifier, not a literal`);
  if (ts.isCallExpression(e))
    return fail(
      e,
      `call expressions are not allowed (\`${e.expression.getText(sf)}\`)`,
    );
  if (ts.isPropertyAccessExpression(e) || ts.isElementAccessExpression(e))
    return fail(e, "member expressions are not allowed");
  if (ts.isTemplateExpression(e) || ts.isNoSubstitutionTemplateLiteral(e))
    return fail(e, "template literals are not allowed: use a string literal");
  if (ts.isConditionalExpression(e))
    return fail(e, "conditionals are not allowed");
  if (ts.isArrowFunction(e) || ts.isFunctionExpression(e))
    return fail(e, "functions are not allowed");
  if (
    ts.isJsxElement(e) ||
    ts.isJsxSelfClosingElement(e) ||
    ts.isJsxFragment(e)
  )
    return fail(e, "an element is not a prop value: nest it as a child");
  return fail(e, `${ts.SyntaxKind[e.kind]} is not a literal`);
}
