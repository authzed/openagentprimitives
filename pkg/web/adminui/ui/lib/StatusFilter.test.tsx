import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { StatusFilter } from "./StatusFilter";

afterEach(() => cleanup());

const statuses = ["Valid", "Failed"];

describe("StatusFilter", () => {
  it("renders an All chip plus one chip per status", () => {
    render(<StatusFilter statuses={statuses} value="" onChange={() => {}} />);
    expect(screen.getByRole("button", { name: "All" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Valid" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Failed" })).toBeTruthy();
  });

  it("calls onChange with the status when an inactive chip is clicked", () => {
    const onChange = vi.fn();
    render(<StatusFilter statuses={statuses} value="" onChange={onChange} />);
    fireEvent.click(screen.getByRole("button", { name: "Valid" }));
    expect(onChange).toHaveBeenCalledWith("Valid");
  });

  it("toggles back to All (\"\") when the active chip is clicked", () => {
    const onChange = vi.fn();
    render(<StatusFilter statuses={statuses} value="Valid" onChange={onChange} />);
    fireEvent.click(screen.getByRole("button", { name: "Valid" }));
    expect(onChange).toHaveBeenCalledWith("");
  });

  it("selects All when the All chip is clicked", () => {
    const onChange = vi.fn();
    render(<StatusFilter statuses={statuses} value="Valid" onChange={onChange} />);
    fireEvent.click(screen.getByRole("button", { name: "All" }));
    expect(onChange).toHaveBeenCalledWith("");
  });

  it("tints the active chip with its status tone and leaves inactive chips neutral", () => {
    render(<StatusFilter statuses={statuses} value="Valid" onChange={() => {}} />);
    // Valid → success tone
    expect(screen.getByRole("button", { name: "Valid" }).className).toContain("text-success");
    // Failed is not selected → muted/neutral
    expect(screen.getByRole("button", { name: "Failed" }).className).toContain("text-muted-foreground");
  });

  it("highlights All when nothing is selected", () => {
    render(<StatusFilter statuses={statuses} value="" onChange={() => {}} />);
    const all = screen.getByRole("button", { name: "All" });
    expect(all.getAttribute("aria-pressed")).toBe("true");
    expect(all.className).toContain("text-primary");
  });
});
