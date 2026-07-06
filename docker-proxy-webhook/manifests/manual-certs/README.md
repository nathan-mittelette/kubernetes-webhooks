# Option B: Manual TLS certificates

There is nothing to `kubectl apply` here — manual mode has no separate
manifest, because it produces the same `docker-proxy-webhook-certificate`
Secret and patches `caBundle` directly onto the webhook configurations
already declared in `../k8s.yaml`.

Run, after applying `../k8s.yaml` (and **not** `../cert-manager.yaml`):

```bash
../../hack/generate-certs.sh
# or: make manual-certs   (from docker-proxy-webhook/)
```

See `docker-proxy-webhook/README.md`, section "TLS certificate management",
for prerequisites, the renewal procedure, and how to switch modes on a live
cluster.
