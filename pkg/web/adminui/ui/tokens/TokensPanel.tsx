import * as React from "react";
import {
  Alert, AlertDescription, Badge, Button,
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@ap/design";
import { getTokens, revokeToken, type AccessTokenRow } from "../lib/api";
import { decodeSubject } from "../lib/decodeSubject";
import { fmtRelative } from "../lib/fmt";

// roleBadgeVariant picks the Badge treatment for a row's role: "unknown"
// (the SpiceDB grant could not be read — see pkg/web/admind/tokens.go) is
// visually distinct from a resolved role, which is distinct again from a
// revoked row (handled separately below, not through this).
function roleBadgeVariant(role: string): "secondary" | "outline" {
  return role === "unknown" ? "outline" : "secondary";
}

// RevokeButton is the confirm-then-POST affordance for one row. A revoked row
// never reaches this — TokensPanel renders a "Revoked" badge instead — so
// there is no double-revoke path to guard against here.
function RevokeButton({
  apiBase, row, onDone,
}: {
  apiBase: string;
  row: AccessTokenRow;
  onDone: () => void;
}) {
  const [revoking, setRevoking] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  const revoke = async () => {
    const label = row.clientName || row.name;
    if (!window.confirm(`Revoke access token "${label}"? This cannot be undone.`)) return;
    setError(null);
    setRevoking(true);
    try {
      await revokeToken(apiBase, row.name);
      onDone();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setRevoking(false);
    }
  };

  return (
    <span className="flex flex-col items-end gap-1">
      <Button variant="destructive" size="sm" onClick={revoke} disabled={revoking}>
        {revoking ? "Revoking…" : "Revoke"}
      </Button>
      {error && <span className="text-[11px] text-destructive">{error}</span>}
    </span>
  );
}

// TokensPanel lists every delegated OAuth access token (AccessToken CRs in
// the operator's configured namespace), joined with each one's SpiceDB
// grant for its role and scope. Revoke deletes the CR — its finalizer tears
// down the SpiceDB tuples — and the list refetches so the row's revoked
// state reflects the outcome immediately.
export function TokensPanel({ apiBase }: { apiBase: string }) {
  const [rows, setRows] = React.useState<AccessTokenRow[]>([]);
  const [error, setError] = React.useState<string | null>(null);
  const [loaded, setLoaded] = React.useState(false);

  const load = React.useCallback(() => {
    getTokens(apiBase)
      .then((r) => {
        setRows(r);
        setError(null);
        setLoaded(true);
      })
      .catch((e: Error) => {
        setError(e.message);
        setLoaded(true);
      });
  }, [apiBase]);

  React.useEffect(() => {
    load();
  }, [load]);

  if (error) {
    return (
      <Alert variant="destructive">
        <AlertDescription>Could not load access tokens: {error}</AlertDescription>
      </Alert>
    );
  }

  return (
    <div className="space-y-3">
      {loaded && rows.length === 0 && <p className="text-sm text-muted-foreground">No access tokens yet.</p>}
      {rows.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Owner</TableHead>
              <TableHead>Client</TableHead>
              <TableHead>Role</TableHead>
              <TableHead>Scope</TableHead>
              <TableHead>Created</TableHead>
              <TableHead>Expires</TableHead>
              <TableHead>Last used</TableHead>
              <TableHead className="text-right">Revoke</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((row) => (
              <TableRow key={row.name}>
                <TableCell className="font-mono text-xs text-foreground" title={row.owner}>
                  {decodeSubject(`user:${row.owner}`)}
                </TableCell>
                <TableCell className="text-sm text-foreground">{row.clientName || "—"}</TableCell>
                <TableCell>
                  <Badge variant={roleBadgeVariant(row.role)}>{row.role}</Badge>
                </TableCell>
                <TableCell className="text-xs text-muted-foreground">
                  {row.unfiltered ? "all classes" : row.scopeClasses?.length ? row.scopeClasses.join(", ") : "—"}
                </TableCell>
                <TableCell className="text-xs text-muted-foreground">{fmtRelative(row.createdAt)}</TableCell>
                <TableCell className="text-xs text-muted-foreground">{fmtRelative(row.expiresAt)}</TableCell>
                <TableCell className="text-xs text-muted-foreground">
                  {row.lastUsedAt ? fmtRelative(row.lastUsedAt) : "never"}
                </TableCell>
                <TableCell className="text-right">
                  {row.revoked ? (
                    <Badge variant="destructive">Revoked</Badge>
                  ) : (
                    <RevokeButton apiBase={apiBase} row={row} onDone={load} />
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
      <p className="text-[11px] text-muted-foreground">
        Delegated OAuth access tokens minted via MCP consent · revoking deletes the AccessToken CR — its finalizer
        removes the underlying SpiceDB grant.
      </p>
    </div>
  );
}
