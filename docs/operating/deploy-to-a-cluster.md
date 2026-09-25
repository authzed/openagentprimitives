# Deploying to a cluster

This guide covers running `oap install` against a real Kubernetes cluster on
EKS, GKE, or AKS (plus Docker Desktop / kind for local development).

---

## What this does

`oap install` installs the agentprimitives stack and configures **webd** — the
credential portal, OAuth callback, and artifact-viewer UI — so it is reachable
from a browser on a real cluster with TLS.

webd uses a **two-origin model**:

| Origin | Purpose |
|--------|---------|
| **Trusted** (`--trusted-hostname`) | Auth callbacks, the portal UI, sensitive browser state |
| **Sandbox** (`--sandbox-hostname`) | Untrusted artifact-viewer content rendered in an iframe |

The two hostnames **must be different** (e.g. `webd.example.com` and
`artifacts.example.com`). Cross-origin isolation between them is a security
requirement: sandbox content cannot read cookies or storage from the trusted
origin.

---

## Prerequisites

- `kubeconfig` pointing at the target cluster with sufficient admin access.
- **Two DNS hostnames** you control — one for the trusted origin, one for the
  sandbox origin. Both will need to point at the cloud load-balancer address
  that `oap install` prints (see [DNS step](#dns-the-one-manual-step) below).
- An email address for the Let's Encrypt ACME account.

---

## The install command

```bash
oap install \
  --trusted-hostname=webd.example.com \
  --sandbox-hostname=artifacts.example.com \
  --acme-email=ops@example.com
```

`--trusted-hostname` and `--sandbox-hostname` take **bare hostnames** — no
scheme, port, or path. For example, `webd.example.com` not
`https://webd.example.com/`.

---

## What happens, step by step

1. **Cloud detection** — `oap` reads the `providerID` from cluster nodes to
   identify whether you are on GKE, EKS, AKS, or bare metal.

2. **GatewayClass resolution**

   - **GKE**: uses the built-in GKE managed Gateway controller; no controller
     is installed.
   - **EKS / AKS / bare**: `oap` offers to install
     [Envoy Gateway](https://gateway.envoyproxy.io/) (a maintained Gateway API
     implementation) and then creates an `eg` GatewayClass. See
     [Install prompts](#install-prompts) below.

3. **cert-manager** — `oap` offers to install cert-manager if it is not already
   present. See [Install prompts](#install-prompts) below.

4. **Let's Encrypt ClusterIssuer** — `oap` creates a Let's Encrypt HTTP-01
   ClusterIssuer using `--acme-email`.

5. **Gateway and HTTPRoutes** — `oap` synthesizes:
   - A `Gateway` resource with one HTTPS listener per hostname and one HTTP `:80`
     listener for the ACME challenge.
   - One `HTTPRoute` per hostname routing to the webd Service.

6. **Wait for load-balancer address** — `oap` watches the Gateway until the
   cloud assigns an address, then **prints the two DNS records** you need to
   create.

7. **ConfigMap patch** — `oap` patches the `spicebox-webd-external-url`
   ConfigMap with the resolved hostnames so webd picks them up automatically.

---

## Install prompts

When `oap` needs to install cert-manager or Envoy Gateway it **explains what
each component is and what it will create**, then asks:

```
Install cert-manager? [y/N]
```

- **Interactive runs**: type `y` to accept or press Enter (default `N`) to
  decline.
- **CI / non-interactive runs**: pass `--assume-yes` (or `-y`) to accept all
  prompts automatically.
- **Declining**: `oap` prints the exact `kubectl apply -f <pinned-url>` command
  to run manually. Install the component, then re-run `oap install` — it
  skips steps that are already satisfied.

---

## DNS — the one manual step

After `oap install` completes it prints the load-balancer address, for example:

```
DNS records to create:
  webd.example.com       A/CNAME -> 203.0.113.42
  artifacts.example.com  A/CNAME -> 203.0.113.42
```

Create those two records with your DNS provider. Once they resolve:

- cert-manager completes the Let's Encrypt HTTP-01 challenge automatically.
- Certificates are issued and stored in cluster Secrets.
- webd picks up the new ConfigMap values and begins serving on HTTPS — **no
  restart required**.

> **Note**: the HTTP-01 challenge requires the load-balancer to be reachable
> from the internet on port 80 and DNS to be pointed at it. Let's Encrypt
> validates both hostnames independently.

---

## Opt-outs and overrides

### `--disable-artifact-viewer`

Install with only the trusted origin; skip the sandbox hostname and the
artifact-viewer iframe entirely. This is the explicit opt-out when you do not
want artifact rendering. Omitting **both** `--sandbox-hostname` and
`--disable-artifact-viewer` is an error — `oap install` will not silently skip
the sandbox setup.

```bash
oap install \
  --trusted-hostname=webd.example.com \
  --acme-email=ops@example.com \
  --disable-artifact-viewer
```

### `--manual-webd-routing`

Skip all Gateway / HTTPRoute / TLS setup. Use this when you bring your own
Gateway or Ingress, run webd internal-only, or use a Slack-only deployment
where browser access is not required. Running `oap install` on a non-local
cluster **without** either `--trusted-hostname` or `--manual-webd-routing` is
an error — `oap` will not leave webd silently unreachable.

```bash
oap install --manual-webd-routing
```

### `--gateway-class=<name>`

Force a specific GatewayClass name instead of letting `oap` auto-resolve one.
Useful if the cluster has multiple Gateway implementations.

```bash
oap install \
  --trusted-hostname=webd.example.com \
  --sandbox-hostname=artifacts.example.com \
  --acme-email=ops@example.com \
  --gateway-class=my-gateway-class
```

### `--tls-issuer=<name>`

Use an existing cert-manager ClusterIssuer instead of creating the Let's
Encrypt one. Skips `--acme-email` (it is not used when a ClusterIssuer is
supplied).

```bash
oap install \
  --trusted-hostname=webd.example.com \
  --sandbox-hostname=artifacts.example.com \
  --tls-issuer=my-cluster-issuer
```

---

## Per-cloud notes

| Cloud | Gateway controller | TLS |
|-------|--------------------|-----|
| **GKE** | Built-in GKE managed Gateway — no install step | cert-manager + Let's Encrypt HTTP-01 |
| **EKS** | `oap` offers to install Envoy Gateway (`eg` GatewayClass) | cert-manager + Let's Encrypt HTTP-01 |
| **AKS** | `oap` offers to install Envoy Gateway (`eg` GatewayClass) | cert-manager + Let's Encrypt HTTP-01 |
| **Bare metal** | `oap` offers to install Envoy Gateway (`eg` GatewayClass) | cert-manager + Let's Encrypt HTTP-01 |

TLS uses Let's Encrypt HTTP-01 on all clouds — no cloud-provider credentials
are needed for certificate issuance. The only requirement is that the
load-balancer be internet-reachable on port 80 and that DNS be pointed at it.

---

## Docker Desktop / kind (local development)

For **local development** use `oap init --local` (ngrok), not this flow.
`oap init --local` provides a public ngrok tunnel with TLS and does **not** use
the Gateway path described in this guide.

If you run the cluster install path against kind:

- You need [`cloud-provider-kind`](https://github.com/kubernetes-sigs/cloud-provider-kind)
  to obtain a load-balancer address.
- Let's Encrypt cannot validate a non-public host, so TLS via the
  HTTP-01 challenge will not work. Use `oap init --local` for local TLS.

---

## Verifying the install

Check that the Gateway has been assigned an address:

```bash
kubectl get gateway spicebox-webd -n agentprimitives-system -o wide
```

Check that the ClusterIssuer is ready:

```bash
kubectl get clusterissuer
```

Check that certificates have been issued:

```bash
kubectl get certificate -n agentprimitives-system
```

The `READY` column should show `True` for each certificate once DNS resolves
and the HTTP-01 challenge completes.

Finally, browse to `https://webd.example.com` and confirm the portal loads.
