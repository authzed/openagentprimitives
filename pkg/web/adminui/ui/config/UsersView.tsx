import { ResourceSection } from "./ResourceSection";
import { COLUMNS, type ColumnSpec } from "./columns";
import { Chip, badgeVal } from "./detail/shared";
import { decodeSubject } from "../lib/decodeSubject";

// The Subject column decodes the canonical SpiceDB subject (user:<base64url of
// the email>) back to the human email; non-encoded subjects pass through raw.
const userColumns: ColumnSpec[] = COLUMNS.users.map((c) =>
  c.key === "subject"
    ? {
        ...c,
        render: (row) => {
          const subject = badgeVal(row, "subject");
          if (!subject) return <span className="text-muted-foreground">—</span>;
          return <Chip>{decodeSubject(subject)}</Chip>;
        },
      }
    : c,
);

export function UsersView({ apiBase }: { apiBase: string }) {
  return (
    <ResourceSection
      apiBase={apiBase}
      resource="users"
      entity="user"
      columns={userColumns}
      note="UserIdentity — people linked to the platform · managed via oap user-identity"
      emptyText="No users linked yet."
    />
  );
}
