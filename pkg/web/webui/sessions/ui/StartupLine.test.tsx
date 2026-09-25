import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { INITIAL_SESSION_SIGNALS, SessionSignalsContext, type SessionSignals } from "../../chat/ui/sessionSignals";
import { StartupLine } from "./StartupLine";

afterEach(cleanup);

function withSignals(startup: SessionSignals["startup"]) {
  render(
    <SessionSignalsContext.Provider value={{ ...INITIAL_SESSION_SIGNALS, startup }}>
      <StartupLine />
    </SessionSignalsContext.Provider>,
  );
}

describe("StartupLine", () => {
  it("renders nothing once the session has started (startup is null)", () => {
    withSignals(null);
    expect(screen.queryByTestId("session-shell-startup")).toBeNull();
  });

  it("shows the watcher's caption as a status line", () => {
    withSignals({ text: "Can't start yet — the agent was not allowed the resources it needs to start: exceeded quota", short: null, stillTrying: false });
    const line = screen.getByTestId("session-shell-startup");
    expect(line).toHaveAttribute("role", "status");
    expect(line).toHaveTextContent("exceeded quota");
    expect(screen.queryByTestId("session-shell-startup-still-trying")).toBeNull();
  });

  it("adds still trying once the wait has outlived the grace", () => {
    withSignals({ text: "Starting up — your request will run once everything's ready…", short: null, stillTrying: true });
    expect(screen.getByTestId("session-shell-startup-still-trying")).toHaveTextContent("still trying");
  });

  it("renders both the long and short caption when short is set — the responsive class picks which one shows", () => {
    withSignals({ text: "Can't start yet — waiting for capacity to run your request", short: "Waiting for capacity", stillTrying: false });
    expect(screen.getByText("Can't start yet — waiting for capacity to run your request")).toBeTruthy();
    expect(screen.getByText("Waiting for capacity")).toBeTruthy();
  });

  it("renders only the long caption when short is null", () => {
    withSignals({ text: "Can't start yet — waiting for capacity to run your request", short: null, stillTrying: false });
    expect(screen.getByText("Can't start yet — waiting for capacity to run your request")).toBeTruthy();
    expect(screen.queryByText("Waiting for capacity")).toBeNull();
  });
});
