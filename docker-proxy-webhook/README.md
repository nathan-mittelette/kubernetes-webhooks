# Docker Proxy Admission Webhook

This webhook intercepts pod resources and rewrites container `image` URLs to point to a "pull-through" caching docker proxy. This ensures that images remain available even if they disappear from their original location (e.g., due to Docker Hub's retention policy) or during outages at the original provider.

Using a webhook avoids having to:
1. Update all existing deployments to reference the caching proxy
2. Remember to update all future deployments to reference the caching proxy

## Prerequisites

### TLS certificate

The webhook needs TLS because the Kubernetes API server only calls admission
webhooks over HTTPS, and must trust the serving certificate via the
`caBundle` field of the webhook configurations. **[cert-manager](https://github.com/jetstack/cert-manager)
is the default, recommended way to provide it** — see
[TLS certificate management](#tls-certificate-management) below for the full
picture, including a fully supported alternative that doesn't require
cert-manager at all.

### Docker Registry Credentials
If your proxy requires authentication, the webhook can attach `imagePullSecrets`
to rewritten pods automatically. See
[Registry authentication (imagePullSecrets)](#registry-authentication-imagepullsecrets)
below for the full configuration reference — in short, the referenced secrets
must exist as `kubernetes.io/dockerconfigjson` secrets in **every namespace**
where rewritten pods run (secret references are namespace-local). See the
[Kubernetes documentation](https://kubernetes.io/docs/tasks/configure-pod-container/pull-image-private-registry/)
for how to create such a secret manually, or enable the built-in
[automatic pull secret replication](#automatic-pull-secret-replication) to
avoid doing it by hand in every namespace.

## Configuration

### 1. Exclude Critical Namespaces

First, label the critical namespaces to exclude them from webhook processing:

```bash
kubectl label namespace cert-manager docker-proxy-webhook=disabled
kubectl label namespace kube-system docker-proxy-webhook=disabled
```

**Important**: Update all `kube-system` deployments to pull from your docker proxies manually. Failing to do so may result in nodes unable to join the cluster, as CNI, DNS, and kube-proxy pods will fail to start if the webhook is not running.

### 2. Configure Domain Mapping

Edit the `manifests/k8s.yaml` file to configure the webhook:

#### ConfigMap Configuration
Update the `configMap` section with your proxy mappings:

```yaml
data:
  docker-proxy-config.yaml: |
    # List of domains to ignore (e.g., private registries)
    ignoreList:
    - "123456789012.dkr.ecr.us-east-1.amazonaws.com"
    - "your-private-registry.com"
    
    # Map public registries to your proxy domains
    domainMap:
      docker.io: your-proxy-docker-io.example.com
      quay.io: your-proxy-quay-io.example.com
      gcr.io: your-proxy-gcr-io.example.com
      k8s.gcr.io: your-proxy-k8s-gcr-io.example.com
      docker.elastic.co: your-proxy-docker-elastic-co.example.com
```

#### Domain mapping value syntax

Each `domainMap` value is `proxy-domain[/path/prefix]`:

- **Multiple hostnames**: any number of source domains may map to any number of
  distinct proxy hostnames, and several source domains may share the same proxy
  hostname (with different path prefixes, see below). Each entry is independent.
- **Path-prefixed targets**: if your proxy is a single host serving multiple
  upstream repositories (the typical Artifactory/Nexus/Harbor layout), give the
  target a path prefix instead of a distinct hostname per upstream:

  ```yaml
  domainMap:
    docker.io: registry.example.com/docker-hub-remote
    quay.io:   registry.example.com/quay-remote
    gcr.io:    other-registry.example.com
  ```

  With this configuration, `nginx:1.27` is rewritten to
  `registry.example.com/docker-hub-remote/library/nginx:1.27`, while
  `quay.io/coreos/kube-state-metrics:v1.8.0` becomes
  `registry.example.com/quay-remote/coreos/kube-state-metrics:v1.8.0` — both
  share the `registry.example.com` host but land under different prefixes.
- **Matching is exact**: the source domain (the `domainMap` key) is matched
  exactly against the image's registry domain. There is no wildcard or
  subdomain matching.
- **`ignoreList` wins on overlap**: if the same domain appears as both a
  `domainMap` key and an `ignoreList` entry, the domain is left untouched
  (matching the ignore behavior); a startup log line calls out the overlap.
- **Rewriting is idempotent**: once an image is already pointed at a
  configured target (host, and path prefix if any), the webhook leaves it
  unchanged and does not count it as an "unknown domain" — safe under
  `reinvocationPolicy: IfNeeded` and safe to run twice.
- **Validation at startup**: values with a scheme (`http://`, `https://`), a
  trailing slash, embedded whitespace, or that don't parse as a valid registry
  host/path are rejected at startup with an actionable error. Keys and values
  are lowercased automatically (registry names are case-insensitive/lowercase
  by convention).

## Registry Authentication (imagePullSecrets)

When any container image in a pod is rewritten towards a proxy registry, the
webhook can attach `imagePullSecrets` so the kubelet can authenticate the pull.

### How it works

- **Additive, never destructive**: configured secrets are *appended* to the
  pod's `imagePullSecrets`. Any pre-existing entries (added by the user, or by
  another controller) are always preserved, in their original position. The
  kubelet tries every listed secret plus node credentials, so appending is
  always safe.
- **Two levels**:
  - **Global** (`pullSecrets` at the top level of the config) — appended
    whenever *any* image in the pod was rewritten, regardless of which
    `domainMap` entry matched.
  - **Per-domain** (`pullSecrets` on a `domainMap` entry) — appended only when
    *that specific mapping* rewrote at least one container of the pod.
- **Nothing is added when nothing was rewritten.** An image that was already
  pointed at a configured target, or that fell through unmapped/ignored, does
  not trigger any secret to be attached (secrets follow rewrites).
- **No duplicates**: a secret already present on the pod (by name) is never
  added twice.

### Configuration reference

`domainMap` entries accept either form:

```yaml
domainMap:
  # String shorthand — exactly today's behavior, no secrets attached.
  quay.io: registry.example.com/quay-remote

  # Object form — target + secrets specific to this mapping.
  docker.io:
    target: registry.example.com/docker-hub-remote
    pullSecrets:
      - docker-hub-proxy-creds
  gcr.io:
    target: other-registry.example.com
    pullSecrets:
      - other-registry-creds

# Global secrets: appended whenever ANY image was rewritten,
# regardless of which mapping was used.
pullSecrets:
  - docker-proxy-credentials
```

Secret names must be valid Kubernetes secret names (DNS-1123 subdomains);
invalid or duplicate names fail at startup.

**Choosing a layout**: a single proxy host with one set of credentials only
needs the global `pullSecrets` list. Multiple proxy hosts with distinct
credentials should use per-domain `pullSecrets` on each `domainMap` entry
instead (or in addition — both are merged, deduplicated, per pod).

**Legacy `-pull-secret` flag**: still works, and is treated as a one-element
global list. It is deprecated in favor of the config file's `pullSecrets`. If
both the flag and any config-level secrets are set, **the config wins** and a
startup warning logs the ignored flag value. Neither set = feature off
(today's default, unchanged).

### Provisioning the secrets

This is the part most users get wrong: **the secret must exist in every
namespace** where rewritten pods run — secret references are namespace-local,
there is no cluster-wide secret in Kubernetes.

- **Recommended**: enable the built-in
  [automatic pull secret replication](#automatic-pull-secret-replication)
  controller — create the secret once, it is copied everywhere automatically.
- **Manual alternative**: run
  `kubectl create secret docker-registry <name> --docker-server=... --docker-username=... --docker-password=... -n <namespace>`
  in every namespace, or use an external tool such as
  [registry-creds](https://github.com/alexellis/registry-creds) or
  [kubernetes-replicator](https://github.com/mittwald/kubernetes-replicator).

**Failure mode if the secret is missing**: the pod is admitted (the webhook
only rewrites the image and lists the secret name; it does not verify the
secret exists), but the pull fails with `ErrImagePull`/`ImagePullBackOff`.
Diagnose with `kubectl describe pod <pod>` — the events show a 401/403
response from the proxy registry.

## Automatic Pull Secret Replication

Eliminates the biggest operational gap of the pull-secret story: the secrets
referenced by `pullSecrets` / `domainMap[].pullSecrets` (above) must exist in
**every namespace** where rewritten pods run. This optional controller
(built into the same binary and Deployment) replicates them automatically.

### Concept & flow

```
   docker-proxy namespace                     every other namespace
  ┌─────────────────────┐    replication    ┌───────────────────────┐
  │ source Secret(s)     │ ───controller───▶ │ managed copy (labeled) │
  │ (created/rotated by  │                   │ ... one per namespace  │
  │  you, once)          │                   └───────────────────────┘
  └─────────────────────┘
```

- The secrets live once, in the webhook's own namespace (`docker-proxy`).
- **On namespace creation**, the secrets are copied into the new namespace
  immediately.
- **At startup**, all existing namespaces are reconciled — no separate
  seeding script needed.
- **On rotation** (editing/re-applying a source secret), the change
  propagates to every copy automatically.
- **Targeting**: every namespace except those labeled
  `docker-proxy-webhook=disabled` — the same opt-out label the mutating
  webhook uses. This automatically excludes `kube-system`, `cert-manager`,
  and the `docker-proxy` namespace itself.

### Setup

1. Enable it in the config (`manifests/k8s.yaml`'s ConfigMap):

   ```yaml
   secretReplication:
     enabled: true
     # Optional. When empty (default), replicates every secret referenced by
     # the global pullSecrets and every domainMap[].pullSecrets. Set this to
     # replicate a fixed list instead.
     # secrets: []
   ```

2. Apply the RBAC manifest (kept separate on purpose — see Security below):

   ```bash
   kubectl apply -f manifests/secret-replication-rbac.yaml
   ```

   This also requires the `serviceAccountName: docker-proxy-webhook` and the
   `POD_NAMESPACE` downward-API env var on the Deployment, both already
   present in `manifests/k8s.yaml`.

3. Verify:

   ```bash
   kubectl get secret -A -l app.kubernetes.io/managed-by=docker-proxy-webhook
   ```

### Rotation runbook

Edit or re-`kubectl apply` the source secret in the `docker-proxy` namespace.
Propagation to every namespace is automatic — no manual step per namespace,
and no restart needed.

### Opting out

Label a namespace `docker-proxy-webhook=disabled` — the same switch used to
exclude it from image rewriting. **This stops future management only**: no
new copies, no rotation updates, no drift repair for that namespace's
existing copies. It does **not** delete secrets already copied there (pods in
that namespace may still reference them; an automatic delete triggered by a
label change would break running workloads). To remove them explicitly:

```bash
kubectl delete secret -n <namespace> -l app.kubernetes.io/managed-by=docker-proxy-webhook
```

### Security

Replicating secrets requires cluster-wide read/write access to Secrets, which
is why the RBAC lives in its own manifest (`manifests/secret-replication-rbac.yaml`)
instead of the base one — if you never enable `secretReplication`, don't
apply that file, and nothing in the base setup needs it. A pre-existing
secret with the same name that wasn't created by this feature is never
modified or deleted; it's left alone and counted as a conflict instead.

### Troubleshooting

- **A conflict is reported**: a target namespace already has its own
  unrelated secret with the same name as a replicated one. Rename one of them.
- **A "source missing" failure is reported**: the configured secret doesn't
  exist yet in the `docker-proxy` namespace — create it there.
- **Transient `ErrImagePull` right after creating a namespace**: pods created
  in a brand-new namespace can race the replication controller by
  milliseconds. The kubelet retries with backoff, so the pod recovers as soon
  as the secret lands — this is expected and self-resolving.

## Image Domain Validation

A second, **validating** admission webhook (`/validate`) complements the
mutating one: it rejects pod creation when a container image is not served
from a whitelisted registry domain. This is an enforcement guarantee —
nothing runs in the cluster unless its image comes from an approved registry.

### How it fits with the mutating webhook

Kubernetes runs **all mutating webhooks before all validating webhooks**, so
validation sees the pod **after** image rewriting. The whitelist is checked
against the domain the image will actually be pulled from. Consequently:

- `domainMap` **values** (the proxy domains) and `ignoreList` **entries**
  (the domains intentionally left untouched) are auto-allowed by default —
  see `autoAllowConfiguredDomains` below — so you don't have to duplicate
  that list.
- Matching is on the registry **domain** only (exact match, no
  wildcard/subdomain matching) — path prefixes are a mutating-webhook
  concern.

### Configuration reference

```yaml
validation:
  # Master switch. When false (or this section is absent), /validate always
  # allows — fully backward compatible with existing deployments.
  enabled: true

  # enforce: deny pod creation on violation (DEFAULT).
  # warn:    allow, but attach an admission warning, log, and count a metric.
  mode: enforce

  # When true (DEFAULT), every domainMap value and ignoreList entry is
  # implicitly whitelisted.
  autoAllowConfiguredDomains: true

  # Additional explicitly allowed registry domains.
  allowedDomains:
    - "registry.internal.example.com"
```

If `enabled: true` and the effective whitelist ends up empty (nothing in
`allowedDomains`, `autoAllowConfiguredDomains: false`, and no `domainMap` /
`ignoreList` entries), the webhook refuses to start — this is a
misconfiguration, not a "deny everything" security posture.

**Coverage**: `spec.containers`, `spec.initContainers`, **and
`spec.ephemeralContainers`** — `kubectl debug` must not be a bypass. A
container whose image fails to parse as a valid reference is treated as a
violation (an unparseable image cannot be trusted).

### Rollout procedure

1. Deploy with `mode: warn`. Existing non-conforming pods keep running;
   new/updated pods with a disallowed image are admitted but produce a
   `kubectl` warning, a log line, and increment
   `docker_proxy_validating_webhook_denied_images_total`.
2. Watch that metric for a representative period (a few days is typical) and
   fix offenders (correct the image, or extend `allowedDomains`).
3. Switch to `mode: enforce`.

### Failure policy trade-off

The `ValidatingWebhookConfiguration` ships with `failurePolicy: Fail`: **a
security control that fails open is not a control.** The trade-off is that a
webhook outage blocks pod creation in every non-excluded namespace. Mitigations
already in place:
- the same namespace opt-out label (`docker-proxy-webhook=disabled`) as the
  mutating webhook;
- pod anti-affinity so both webhook pods aren't scheduled on the same node;
- clusters that prefer availability over enforcement can edit the manifest to
  `failurePolicy: Ignore`.

### Troubleshooting: "my pod was rejected"

1. Read the deny message — it names the offending container, image, and
   domain.
2. Check that domain against `domainMap` / `ignoreList` / `allowedDomains`.
3. Either fix the image reference, add the domain to `allowedDomains`, or (as
   a last resort) label the namespace `docker-proxy-webhook=disabled`.

Recommended alert: `denied_images_total` > 0 in `enforce` mode (something is
actually being blocked); sustained `denied_images_total` in `warn` mode
(signals offenders that need fixing before the `enforce` cutover). See
[Monitoring](#monitoring) below for the full metrics list.

## TLS Certificate Management

The Secret name (`docker-proxy-webhook-certificate`) and mount path
(`/tmp/k8s-webhook-server/serving-certs`) are identical in both modes below —
the Deployment (`manifests/k8s.yaml`) is shared, and switching modes never
touches the pod spec.

| | cert-manager (Option A, default) | Manual (Option B) |
|---|---|---|
| Automation | Fully automatic issuance + renewal | You run a script (or automate it yourself, e.g. a CronJob) |
| Renewal | Automatic (`renewBefore: 360h`) | Manual — re-run `hack/generate-certs.sh` |
| Bootstrap circularity | None once the cert-manager namespace is excluded (see below) | None — no cert-manager anywhere |
| Operational burden | Low (one more controller to run) | Low complexity, but *you* own expiry monitoring |

### Option A — cert-manager (default)

```bash
kubectl apply -f manifests/k8s.yaml -f manifests/cert-manager.yaml
```

This is the setup described throughout this README. cert-manager produces
the `docker-proxy-webhook-certificate` Secret from the `Certificate` resource
in `manifests/cert-manager.yaml`, and its CA injector fills in `caBundle` on
both webhook configurations via the `cert-manager.io/inject-ca-from`
annotation already present on them in `manifests/k8s.yaml`.

**Bootstrap note**: cert-manager must itself be excluded from the webhook
(see "Exclude Critical Namespaces" above) to avoid a circular dependency —
the webhook depends on cert-manager for certificates, and cert-manager would
otherwise depend on the webhook for image rewriting.

To customize the issuer or add SANs for a custom domain, edit
`manifests/cert-manager.yaml`:

```yaml
spec:
  issuerRef:
    name: your-cluster-issuer
    kind: ClusterIssuer
  dnsNames:
  - docker-proxy-webhook.docker-proxy.svc
  - docker-proxy-webhook.docker-proxy.svc.cluster.local
```

### Option B — manual (no cert-manager)

Use this when you can't or don't want to run cert-manager.

**Prerequisites**: `openssl`, and `kubectl` with permission to create
Secrets in the `docker-proxy` namespace and to patch (cluster-scoped)
`mutatingwebhookconfigurations`/`validatingwebhookconfigurations`. If you
can't grant cluster-admin, the minimal RBAC is:
`get`/`patch` on `mutatingwebhookconfigurations.admissionregistration.k8s.io`
and `validatingwebhookconfigurations.admissionregistration.k8s.io`
(cluster-scoped), plus `get`/`create`/`update` on `secrets` in `docker-proxy`.

**Install**:

```bash
kubectl apply -f manifests/k8s.yaml   # do NOT apply manifests/cert-manager.yaml
./hack/generate-certs.sh              # or: make manual-certs
```

`generate-certs.sh` (idempotent — see `--help` for all flags):
1. generates a self-signed CA (5-year validity by default);
2. generates a server key + certificate signed by that CA, with SANs
   `docker-proxy-webhook.<namespace>.svc` and the `.svc.cluster.local` form
   (1-year validity by default, matching cert-manager's `duration: 8760h`);
3. creates/updates the `docker-proxy-webhook-certificate` Secret;
4. patches `caBundle` into both the `MutatingWebhookConfiguration` and the
   `ValidatingWebhookConfiguration` (skipping either one gracefully if it
   isn't applied yet).

Use `--dry-run` to preview without touching the cluster.

**Renewal**: re-run the same script. controller-runtime's certificate
watcher picks up the updated Secret **without a pod restart**. Monitor
expiry yourself — e.g. `openssl x509 -in <(kubectl get secret
docker-proxy-webhook-certificate -n docker-proxy -o jsonpath='{.data.tls\.crt}'
| base64 -d) -noout -enddate`, a cron job around the script, or any
cert-expiry exporter already in your cluster.

> **You own renewal in this mode.** An expired certificate means the API
> server rejects webhook calls — with `failurePolicy: Fail` on the
> validating webhook (and the mutating webhook's own effective failure mode),
> that blocks pod creation in every non-excluded namespace. Set a reminder or
> automate the re-run.

### Switching modes on a live cluster

**cert-manager → manual**: run `hack/generate-certs.sh` first (it patches
`caBundle` and updates the Secret in place using the *same* names cert-manager
was using) — the webhook never serves an untrusted cert because the Secret
is updated, not replaced, and controller-runtime hot-reloads it. Only after
confirming it's working, delete the `Certificate` resource
(`kubectl delete -f manifests/cert-manager.yaml`) so cert-manager stops
reconciling (and potentially fighting over) that Secret.

**manual → cert-manager**: apply `manifests/cert-manager.yaml`. cert-manager
takes over the existing Secret (same name) and reissues it; the CA injector
then overwrites `caBundle` with its own. No pod restart needed either way.

### 3. Deploy the Webhook

Apply the base manifest, then pick a TLS mode from
[TLS Certificate Management](#tls-certificate-management) above:

```bash
kubectl apply -f manifests/k8s.yaml

# Option A (default):
kubectl apply -f manifests/cert-manager.yaml

# Option B:
./hack/generate-certs.sh
```

If you're enabling [automatic pull secret replication](#automatic-pull-secret-replication), also apply `manifests/secret-replication-rbac.yaml`.

### 4. Verify Installation

Check that the webhook is running:

```bash
kubectl get pods -n docker-proxy
kubectl get mutatingwebhookconfiguration docker-proxy-webhook
kubectl get validatingwebhookconfiguration docker-proxy-webhook
```

## Monitoring

The webhook exposes Prometheus metrics on port 8080:
- `docker_proxy_mutating_webhook_result_total` - Total webhook invocations
- `docker_proxy_mutating_webhook_failures_total` - Total webhook failures
- `docker_proxy_mutating_webhook_container_rewrites_total` - Total container image rewrites
- `docker_proxy_mutating_webhook_unknown_domain_total` - Images whose domain matched neither `domainMap` nor `ignoreList`
- `docker_proxy_mutating_webhook_pull_secrets_added_total` - `imagePullSecrets` entries actually appended to a pod

When [image domain validation](#image-domain-validation) is enabled:
- `docker_proxy_validating_webhook_result_total` - Every admission decision (allowed/denied)
- `docker_proxy_validating_webhook_denied_images_total` - Each rejected/warned image
- `docker_proxy_validating_webhook_failures_total` - Internal errors

When [automatic pull secret replication](#automatic-pull-secret-replication) is enabled:
- `docker_proxy_secret_replication_total` - Successful replication operations
- `docker_proxy_secret_replication_failures_total` - Replication errors (e.g. source secret missing)
- `docker_proxy_secret_replication_conflicts_total` - A target namespace already had its own unrelated secret with the same name

Configure alerts for webhook failures and unmapped image references to ensure proper operation.