import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import { RevisionList } from "./RevisionList";
import type { LiveRevision } from "./types";

afterEach(cleanup);

const revs: LiveRevision[] = [
  { seq: 1, revisionId: "rev-1", changeDescription: "first", createdAt: "", tags: [] },
  { seq: 2, revisionId: "rev-2", changeDescription: "second", createdAt: "", tags: ["latest"] },
];

describe("RevisionList – per-revision download", () => {
  it("renders a download icon per revision linking to that revision", () => {
    render(
      <RevisionList
        revisions={revs}
        currentRevId="rev-2"
        onSelect={() => {}}
        downloadHref={(id) => `/artifact-download?artifactId=a&sessionRef=n%2Fs&rev=${id}`}
      />,
    );
    const hrefs = screen.getAllByRole("link").map((l) => l.getAttribute("href"));
    expect(hrefs).toContain("/artifact-download?artifactId=a&sessionRef=n%2Fs&rev=rev-1");
    expect(hrefs).toContain("/artifact-download?artifactId=a&sessionRef=n%2Fs&rev=rev-2");
  });

  it("clicking the download icon does not pin the revision (stops propagation)", () => {
    const onSelect = vi.fn();
    render(
      <RevisionList revisions={revs} currentRevId={null} onSelect={onSelect} downloadHref={(id) => `/d?rev=${id}`} />,
    );
    fireEvent.click(screen.getAllByRole("link")[0]);
    expect(onSelect).not.toHaveBeenCalled();
  });

  it("omits download icons when no downloadHref is provided", () => {
    render(<RevisionList revisions={revs} currentRevId={null} onSelect={() => {}} />);
    expect(screen.queryAllByRole("link")).toHaveLength(0);
  });
});
