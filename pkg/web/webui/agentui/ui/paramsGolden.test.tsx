import { expect, it } from "vitest";
import { collectDefaultParams } from "@ap/agentui";
import type { Declaration } from "@ap/agentui";
import golden from "./testdata/params.golden.json";

// testdata/params.golden.json is the ONE artifact both halves of the
// current-parameter mirror (J5) read: pkg/web/webui/agentui/params_golden_internal_test.go
// asserts the REAL uicomponents.ParamStates output against it; this file
// re-derives the parameter map through @ap/agentui's collectDefaultParams,
// applied to the golden's OWN declaration — not by echoing golden.parameters
// back at itself. That's what makes this a pin: a coordinated change (edit
// ParamStates' expansion rule, regenerate this file to match) still fails
// here, because collectDefaultParams computes its half independently from
// the shared declaration.
//
// The J5 join: this file is produced and asserted by
// params_golden_internal_test.go. Re-deriving it HERE through the browser's
// own seeding function is what makes a change on either side fail the other
// side's suite.
it("seeds the same parameter keys and values read_view reports", () => {
  const expected = Object.fromEntries(golden.parameters.map((p) => [p.key, p.value]));
  expect(collectDefaultParams(golden.declaration as Declaration)).toEqual(expected);
});
