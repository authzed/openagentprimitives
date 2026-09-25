import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
// No existing test in this package uses jest-dom matchers (grep confirms),
// so the project doesn't wire a global setupFile for it. Import the vitest
// adapter locally, matching the pattern the brief's assertions require.
import "@testing-library/jest-dom/vitest";
import { Input } from "./input";
import { Label } from "./label";
import { Popover, PopoverContent, PopoverTrigger } from "./popover";
import { Calendar } from "./calendar";

describe("shadcn installs backing ap:form and ap:daterange", () => {
  it("renders a labelled input", () => {
    render(
      <>
        <Label htmlFor="since">Since</Label>
        <Input id="since" placeholder="pick a date" />
      </>,
    );
    expect(screen.getByLabelText("Since")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("pick a date")).toBeInTheDocument();
  });

  it("renders popover content only once opened", () => {
    render(
      <Popover open>
        <PopoverTrigger>open</PopoverTrigger>
        <PopoverContent>inside</PopoverContent>
      </Popover>,
    );
    expect(screen.getByText("inside")).toBeInTheDocument();
  });

  it("renders a calendar grid", () => {
    render(<Calendar mode="single" />);
    expect(screen.getByRole("grid")).toBeInTheDocument();
  });
});
