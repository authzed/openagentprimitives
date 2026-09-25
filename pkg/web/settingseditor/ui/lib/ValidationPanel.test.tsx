import "@testing-library/jest-dom/vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { describe, expect, it, afterEach } from "vitest";
import { ValidationPanel } from "./ValidationPanel";
import type { ValidationResult } from "./api";

afterEach(cleanup);

describe("ValidationPanel", () => {
  it("renders errors as blocking alerts", () => {
    const result: ValidationResult = {
      errors: ["limits.budget.maxTurns: must be >= 1"],
      violations: [],
    };
    render(<ValidationPanel result={result} />);
    const alert = screen.getByText(/must be >= 1/i).closest('[role="alert"]');
    expect(alert).not.toBeNull();
  });

  it("renders violations with a reason badge and message, not blocking", () => {
    const result: ValidationResult = {
      errors: [],
      violations: [{ reason: "AuthzClamped", message: "approval timeout clamped to cluster ceiling", fatal: false }],
    };
    render(<ValidationPanel result={result} />);
    expect(screen.getByText("AuthzClamped")).toBeInTheDocument();
    expect(screen.getByText(/clamped to cluster ceiling/i)).toBeInTheDocument();
  });

  it("marks a fatal violation's badge distinctly from a non-fatal one", () => {
    const result: ValidationResult = {
      errors: [],
      violations: [
        { reason: "FatalThing", message: "blocks admission", fatal: true },
        { reason: "AdvisoryThing", message: "advisory only", fatal: false },
      ],
    };
    render(<ValidationPanel result={result} />);
    expect(screen.getByText("FatalThing")).toHaveClass("bg-destructive");
    expect(screen.getByText("AdvisoryThing")).not.toHaveClass("bg-destructive");
  });

  it("renders the effective-settings preview only when present", () => {
    const withEffective: ValidationResult = {
      errors: [],
      violations: [],
      effective: { limits: { minPlanGateMode: "logging" } },
    };
    const { rerender } = render(<ValidationPanel result={withEffective} />);
    expect(screen.getByText(/effective settings \(preview\)/i)).toBeInTheDocument();
    expect(screen.getByText(/"minPlanGateMode": "logging"/)).toBeInTheDocument();

    rerender(<ValidationPanel result={{ errors: [], violations: [] }} />);
    expect(screen.queryByText(/effective settings \(preview\)/i)).not.toBeInTheDocument();
  });
});
