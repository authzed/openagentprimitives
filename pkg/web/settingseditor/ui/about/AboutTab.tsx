import * as React from "react";
import {
  Alert,
  AlertDescription,
  Badge,
  Table,
  TableHeader,
  TableBody,
  TableRow,
  TableHead,
  TableCell,
} from "@ap/design";
import { getInstallInfo, type InstallInfoResponse } from "../lib/api";

export interface AboutTabProps {
  apiBase: string;
}

// AboutTab is the settings app's About tab: a read-only projection of
// installInfoResponse (GET /api/cluster/install-info) — cluster kind,
// component Deployments, the allowlisted operator env, the first node, and
// the running oap version. There is nothing here to edit or save, so unlike
// the other tabs this one has no form state, only the load.
export function AboutTab({ apiBase }: AboutTabProps) {
  const [resp, setResp] = React.useState<InstallInfoResponse | null>(null);
  const [loadError, setLoadError] = React.useState<string | null>(null);

  React.useEffect(() => {
    let closed = false;
    getInstallInfo(apiBase)
      .then((r) => {
        if (closed) return;
        setResp(r);
      })
      .catch((e: Error) => {
        if (!closed) setLoadError(e.message);
      });
    return () => {
      closed = true;
    };
  }, [apiBase]);

  return (
    <div className="flex flex-col gap-6">
      <h2 className="text-base font-semibold">About</h2>

      {loadError && (
        <Alert variant="destructive">
          <AlertDescription>{loadError}</AlertDescription>
        </Alert>
      )}

      {resp?.clusterDown && (
        <Alert variant="destructive">
          <AlertDescription>
            The desktop cluster isn&apos;t running. Start it from the General tab to see install info.
          </AlertDescription>
        </Alert>
      )}

      {resp && !resp.clusterDown && (
        <>
          <section className="flex items-center gap-2" data-testid="about-cluster-kind">
            <h3 className="text-sm font-medium">Cluster kind</h3>
            <Badge variant="secondary">{resp.clusterKind}</Badge>
          </section>

          <section className="flex flex-col gap-2">
            <h3 className="text-sm font-medium">Components</h3>
            <Table data-testid="about-components-table">
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>Image</TableHead>
                  <TableHead>Ready</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {resp.components.length === 0 && (
                  <TableRow>
                    <TableCell colSpan={3} className="text-muted-foreground">
                      No components reported.
                    </TableCell>
                  </TableRow>
                )}
                {resp.components.map((c) => (
                  <TableRow key={c.name}>
                    <TableCell>{c.name}</TableCell>
                    <TableCell className="font-mono text-xs">{c.image}</TableCell>
                    <TableCell>{c.ready}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </section>

          <section className="flex flex-col gap-2">
            <h3 className="text-sm font-medium">Operator environment</h3>
            <Table data-testid="about-env-table">
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>Value</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {resp.operatorEnv.length === 0 && (
                  <TableRow>
                    <TableCell colSpan={2} className="text-muted-foreground">
                      No environment variables reported.
                    </TableCell>
                  </TableRow>
                )}
                {resp.operatorEnv.map((e) => (
                  <TableRow key={e.name}>
                    <TableCell className="font-mono text-xs">{e.name}</TableCell>
                    <TableCell className="font-mono text-xs">{e.value}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </section>

          <section className="flex flex-col gap-1" data-testid="about-node">
            <h3 className="text-sm font-medium">Node</h3>
            {resp.node ? (
              <p className="text-sm text-muted-foreground">
                {resp.node.name} — kubelet {resp.node.kubeletVersion} —{" "}
                {resp.node.ready ? "Ready" : "Not ready"}
              </p>
            ) : (
              <p className="text-sm text-muted-foreground">No node reported.</p>
            )}
          </section>

          <footer className="text-xs text-muted-foreground" data-testid="about-app-version">
            oap {resp.appVersion}
          </footer>
        </>
      )}
    </div>
  );
}
