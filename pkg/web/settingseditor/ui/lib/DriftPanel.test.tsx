import "@testing-library/jest-dom/vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import { describe, expect, it, vi, afterEach } from "vitest";
import { DriftPanel } from "./DriftPanel";

afterEach(cleanup);

describe("DriftPanel", () => {
  it("lists the drift paths and offers a take-ownership button that invokes the callback", () => {
    const onTakeOwnership = vi.fn();
    render(<DriftPanel drift={["limits.budget.maxTurns", "defaults.model.provider"]} onTakeOwnership={onTakeOwnership} />);

    expect(screen.getByText("limits.budget.maxTurns")).toBeInTheDocument();
    expect(screen.getByText("defaults.model.provider")).toBeInTheDocument();

    const button = screen.getByRole("button", { name: /take full ownership and re-apply/i });
    fireEvent.click(button);
    expect(onTakeOwnership).toHaveBeenCalledTimes(1);
  });

  it("disables the take-ownership button while busy", () => {
    render(<DriftPanel drift={["a.b"]} onTakeOwnership={() => {}} busy />);
    expect(screen.getByRole("button", { name: /take full ownership and re-apply/i })).toBeDisabled();
  });

  it("renders a residual-drift warning with no retry button when afterTakeOwnership and drift remains", () => {
    render(<DriftPanel drift={["defaults.model.provider"]} onTakeOwnership={() => {}} afterTakeOwnership />);
    expect(screen.getByText(/drift remains even after taking full ownership/i)).toBeInTheDocument();
    expect(screen.getByText("defaults.model.provider")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /take full ownership and re-apply/i })).not.toBeInTheDocument();
  });
});
