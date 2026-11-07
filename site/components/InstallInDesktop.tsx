// The oap:// link format is parsed by cmd/oap/internal/desktop/links; the Go
// test there parses the exact href this component produces for a fixture ref.
// The prop is `image`, not `ref`: `ref` is React's reserved element-ref
// attribute, so it must not double as data.
export function InstallInDesktop({ image }: { image: string }) {
  const href = `oap://install/agent?v=1&ref=${encodeURIComponent(image)}`;
  return (
    <div className="install-in-desktop">
      <p>
        <a href={href}>Install in OAP Desktop</a>
      </p>
      <p>
        Or from the CLI: <code>{`oap agent install ${image}`}</code>
      </p>
    </div>
  );
}
