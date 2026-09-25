export interface LinkMenuRow {
  credentialName: string;
  label: string;
  why: string;
  iconUrl: string;
  status: "linked" | "missing";
  kind: "oauth" | "pat";
  oauthUrl: string;
  instructions?: string;
  docsUrl?: string;
}

export interface LinkPageProps {
  sessionRef: string;
  agentName: string;
  what: string[];
  signedLink: string;
  rows: LinkMenuRow[];
}
