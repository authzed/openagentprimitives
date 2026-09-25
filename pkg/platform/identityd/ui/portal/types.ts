export interface PortalCredential {
  name: string;
  label: string;
  isOAuth: boolean;
  iconUrl: string;
}
export interface PortalProps {
  subject: string;
  displayName: string;
  linked: PortalCredential[];
  suggested: PortalCredential[];
}
