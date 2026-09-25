import { ConfigDetail } from "./ConfigDetail";
import { decodeSubject } from "../../lib/decodeSubject";

// UserPage is the UserIdentity detail. Tabs = Overview (subject / display name /
// available+resolved credential counts / last refresh) + Credentials (list).
// The projector sets the detail name to the human display (displayName, else the
// subject) and the description to the canonical subject. The Overview Manage
// block is omitted when the projector emits no manageCmd (UserIdentity CRs are
// owned by the identity setup flow, not hand-edited).
export function UserPage({ apiBase, id, tab }: { apiBase: string; id: string; tab?: string }) {
  return (
    <ConfigDetail
      apiBase={apiBase}
      resource="users"
      entity="user"
      id={id}
      tab={tab}
      backRoute={{ type: "view", view: "users" }}
      // The description is the canonical subject (user:<base64url email>);
      // surface the DECODED human email prominently as the page subtitle so an
      // admin reads "alice@example.com", not the opaque encoded subject.
      subtitle={(d) => decodeSubject(d.description ?? "")}
    />
  );
}
