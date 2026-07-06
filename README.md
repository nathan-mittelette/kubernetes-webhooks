# Kubernetes Webhooks

This repository contains Kubernetes admission webhooks for managing container images. It is a fork of the original [NextDeveloperTeam/kubernetes-webhooks](https://github.com/NextDeveloperTeam/kubernetes-webhooks) repository.

## Overview

This project provides Kubernetes admission webhooks that automatically modify pod specifications to enhance image management and availability. The webhooks intercept pod creation requests and apply transformations to ensure reliable container image access.

## Webhooks

### docker-proxy-webhook

A mutating admission webhook that rewrites container image URLs in pod specifications to point to a caching docker proxy server (such as Artifactory), complemented by an optional **validating** webhook that enforces a registry domain whitelist on the (post-rewrite) image. This provides several benefits:

- **Image Availability**: Archives images in case they are deleted from their canonical location
- **Rate Limit Avoidance**: Prevents issues with public registry rate limiting
- **Disaster Recovery**: Ensures images remain accessible even if original registries are unavailable
- **Performance**: Reduces image pull times through caching
- **Enforcement**: Optionally rejects (or warns on) pods whose images aren't served from an approved registry

The webhook automatically:
- Rewrites `image` fields in pod specs to use your configured proxy domains, including multi-hostname and per-domain path-prefix layouts
- Adds global and/or per-domain pull secrets (appended, never replacing existing ones) when images are rewritten, and can replicate those secrets into every namespace automatically
- Validates that (post-rewrite) image domains are on an allowed list, in `enforce` or `warn` mode
- Provides metrics via Prometheus for monitoring
- Respects ignore lists for private registries (e.g., AWS ECR)

## Getting Started

See the [docker-proxy-webhook README](./docker-proxy-webhook/README.md) for detailed deployment instructions and configuration options.

TLS for the admission webhooks is provided by [cert-manager](https://github.com/jetstack/cert-manager) by default, but a fully supported manual alternative (no cert-manager required) is also available — see that README's "TLS Certificate Management" section.

## Security

Please submit security vulnerabilities to `mittelette.nathan@gmail.com`

## License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.

## Attribution

This is a fork of the original work by [NextDeveloperTeam](https://github.com/NextDeveloperTeam/kubernetes-webhooks). All credit for the original implementation goes to the original authors.
