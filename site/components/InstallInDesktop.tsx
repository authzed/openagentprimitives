// The oap:// link format is parsed by cmd/oap/internal/desktop/links; the Go
// test there parses the exact href this component produces for a fixture ref.
export function InstallInDesktop({ ref }: { ref: string }) {
  const href = `oap://install/agent?v=1&ref=${encodeURIComponent(ref)}`;
  return (
    <div className="install-in-desktop">
      <p>
        <a href={href}>Install in OAP Desktop</a>
      </p>
      <p>
        Or from the CLI: <code>{`oap agent install ${ref}`}</code>
      </p>
    </div>
  );
}
