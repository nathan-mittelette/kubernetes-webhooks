# Docker Proxy Admission Webhook

This webhook intercepts pod resources and rewrites container `image` URLs to point to a "pull-through" caching docker proxy. This ensures that images remain available even if they disappear from their original location (e.g., due to Docker Hub's retention policy) or during outages at the original provider.

Using a webhook avoids having to:
1. Update all existing deployments to reference the caching proxy
2. Remember to update all future deployments to reference the caching proxy

## Prerequisites

### cert-manager
This webhook requires [cert-manager](https://github.com/jetstack/cert-manager) to be installed in your cluster for automatic TLS certificate management. The webhook uses cert-manager to generate and manage the TLS certificates required for the admission webhook to communicate securely with the Kubernetes API server.

**Important**: cert-manager must be configured to pull images from your docker cache proxy to avoid circular dependencies. The cert-manager namespace is intentionally excluded from the webhook to prevent a bootstrap problem where:
- The webhook depends on cert-manager for TLS certificates
- cert-manager depends on the webhook for image rewriting

### Docker Registry Credentials
In each namespace where the webhook will operate, create a docker config secret named `docker-proxy-credentials` for pulling images from your private proxy. See the [Kubernetes documentation](https://kubernetes.io/docs/tasks/configure-pod-container/pull-image-private-registry/) for details.

You can use [registry-creds](https://github.com/alexellis/registry-creds) to automate the creation of these secrets across namespaces.

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

#### Certificate Configuration
Update the certificate section if you're using a custom domain:

```yaml
spec:
  secretName: docker-proxy-webhook-certs
  issuerRef:
    name: your-cluster-issuer
    kind: ClusterIssuer
  dnsNames:
  - docker-proxy-webhook.docker-proxy.svc
  - docker-proxy-webhook.docker-proxy.svc.cluster.local
```

### 3. Deploy the Webhook

Apply the configuration:

```bash
kubectl apply -f manifests/k8s.yaml
```

### 4. Verify Installation

Check that the webhook is running:

```bash
kubectl get pods -n docker-proxy
kubectl get mutatingwebhookconfiguration docker-proxy-webhook
```

## Monitoring

The webhook exposes Prometheus metrics on port 8080:
- `docker_proxy_mutating_webhook_result_total` - Total webhook invocations
- `docker_proxy_mutating_webhook_failures_total` - Total webhook failures
- `docker_proxy_container_rewrite_total` - Total container image rewrites

Configure alerts for webhook failures and unmapped image references to ensure proper operation.