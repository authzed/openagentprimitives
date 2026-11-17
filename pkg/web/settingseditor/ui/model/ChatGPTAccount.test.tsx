import * as React from "react";
import { render, screen, waitFor, fireEvent, cleanup } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { ChatGPTAccount } from "./ChatGPTAccount";

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

it("signs in, lists account models and clears the selected account on sign-out", async () => {
  let signedIn = false;
  const fetcher = vi.fn(async (input: string, init?: RequestInit) => {
    let value: unknown;
    if (input === "/accounts/login") {
      expect(JSON.parse(String(init?.body))).toEqual({ account: "" });
      signedIn = true; value = { account: "oaiapp_test" };
    } else if (input === "/accounts/logout") {
      expect(JSON.parse(String(init?.body))).toEqual({ account: "oaiapp_test" });
      signedIn = false; value = { status: "signed out" };
    } else if (input.startsWith("/accounts/models")) {
      expect(input).toContain("account=oaiapp_test");
      value = [{ slug: "available-model", display_name: "Available model" }];
    } else value = signedIn ? [{ id: "oaiapp_test", label: "person@example.test", signedIn }] : [];
    return new Response(JSON.stringify(value), { status: 200 });
  });
  vi.stubGlobal("fetch", fetcher);
  function Fixture() {
    const [account, setAccount] = React.useState(""); const [model, setModel] = React.useState("");
    return <ChatGPTAccount account={account} onAccount={setAccount} model={model} onModel={setModel} />;
  }
  render(<Fixture />);
  fireEvent.click(screen.getByText("Continue with ChatGPT"));
  await waitFor(() => expect(screen.getByRole("option", { name: "Available model" })).toBeTruthy());
  expect((screen.getByLabelText("ChatGPT account") as HTMLSelectElement).value).toBe("oaiapp_test");
  fireEvent.change(screen.getByLabelText("Available model"), { target: { value: "available-model" } });
  fireEvent.click(screen.getByText("Sign out of selected account"));
  await waitFor(() => expect((screen.getByLabelText("ChatGPT account") as HTMLSelectElement).value).toBe(""));
  expect((screen.getByLabelText("Available model") as HTMLSelectElement).value).toBe("");
});

it("shows declined sign-in without selecting an account", async () => {
  const select = vi.fn();
  vi.stubGlobal("fetch", vi.fn(async (path: string) => new Response(
    path === "/accounts" ? "[]" : "Sign-in was declined", { status: path === "/accounts" ? 200 : 502 },
  )));
  render(<ChatGPTAccount account="" onAccount={select} model="" onModel={vi.fn()} />);
  fireEvent.click(screen.getByText("Continue with ChatGPT"));
  await waitFor(() => expect(screen.getByText("Sign-in was declined")).toBeTruthy());
  expect(select).not.toHaveBeenCalled();
});
