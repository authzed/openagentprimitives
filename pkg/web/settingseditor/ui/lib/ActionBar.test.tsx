import "@testing-library/jest-dom/vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import { describe, expect, it, vi, afterEach } from "vitest";
import { ActionBar } from "./ActionBar";

afterEach(() => {
  cleanup();
});

describe("ActionBar", () => {
  it("shows the 'Unsaved changes' indicator when dirty, and hides it when clean", () => {
    const { rerender } = render(
      <ActionBar dirty={true} onValidate={() => {}} onSave={() => {}} />,
    );
    expect(screen.getByRole("status")).toHaveTextContent(/unsaved changes/i);

    rerender(<ActionBar dirty={false} onValidate={() => {}} onSave={() => {}} />);
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("disables both Validate and Save when busy", () => {
    render(<ActionBar dirty={false} busy onValidate={() => {}} onSave={() => {}} />);
    expect(screen.getByRole("button", { name: /^validate$/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: /^save$/i })).toBeDisabled();
  });

  it("disables both Validate and Save when disabled", () => {
    render(<ActionBar dirty={false} disabled onValidate={() => {}} onSave={() => {}} />);
    expect(screen.getByRole("button", { name: /^validate$/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: /^save$/i })).toBeDisabled();
  });

  it("fires onValidate and onSave when their buttons are clicked", () => {
    const onValidate = vi.fn();
    const onSave = vi.fn();
    render(<ActionBar dirty={false} onValidate={onValidate} onSave={onSave} />);

    fireEvent.click(screen.getByRole("button", { name: /^validate$/i }));
    expect(onValidate).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));
    expect(onSave).toHaveBeenCalledTimes(1);
  });

  it("renders the note text when provided", () => {
    render(
      <ActionBar
        dirty={false}
        note="changes apply immediately on save"
        onValidate={() => {}}
        onSave={() => {}}
      />,
    );
    expect(screen.getByText(/changes apply immediately on save/i)).toBeInTheDocument();
  });

  it("renders a custom saveLabel in place of the default Save text", () => {
    render(
      <ActionBar dirty={false} saveLabel="Apply without validating?" onValidate={() => {}} onSave={() => {}} />,
    );
    expect(screen.getByRole("button", { name: /apply without validating\?/i })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^save$/i })).not.toBeInTheDocument();
  });
});
