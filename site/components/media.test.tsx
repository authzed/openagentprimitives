// @vitest-environment jsdom
import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Clip } from "./media";

describe("Clip", () => {
  it("lets a viewer hear a narrated clip with its controls", () => {
    render(<Clip name="codebot-plangate" />);
    const video = screen.getByText(/codebot proposes a plan/i)
      .previousElementSibling as HTMLVideoElement;
    expect(video.tagName).toBe("VIDEO");
    expect(video.controls).toBe(true);
    expect(video.muted).toBe(false);
  });
});
