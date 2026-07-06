#!/bin/sh
# generate-certs.sh — generate a self-signed CA + serving certificate for
# docker-proxy-webhook and install it, as an alternative to cert-manager.
# See docker-proxy-webhook/README.md, "TLS certificate management".
#
# Idempotent: safe to re-run for renewal. controller-runtime hot-reloads the
# updated Secret's tls.crt/tls.key without a pod restart.
#
# Usage: generate-certs.sh [-n namespace] [-s service] [-d days] [-c ca-days] [--dry-run]

set -eu

NAMESPACE="docker-proxy"
SERVICE="docker-proxy-webhook"
SECRET_NAME="docker-proxy-webhook-certificate"
DAYS=365     # server certificate validity, matches cert-manager's duration: 8760h
CA_DAYS=1825 # CA certificate validity (~5 years)
DRY_RUN=0

usage() {
  cat <<EOF
Usage: $(basename "$0") [-n namespace] [-s service] [-d days] [-c ca-days] [--dry-run]

  -n namespace   Namespace the webhook runs in (default: $NAMESPACE)
  -s service     Service name the webhook is exposed as; also the name of
                 the Mutating/ValidatingWebhookConfiguration objects
                 (default: $SERVICE)
  -d days        Server certificate validity in days (default: $DAYS)
  -c ca-days     CA certificate validity in days (default: $CA_DAYS)
  --dry-run      Generate the certificate but print what would be applied
                 instead of touching the cluster
  -h, --help     Show this help
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
  -n)
    NAMESPACE="$2"
    shift 2
    ;;
  -s)
    SERVICE="$2"
    shift 2
    ;;
  -d)
    DAYS="$2"
    shift 2
    ;;
  -c)
    CA_DAYS="$2"
    shift 2
    ;;
  --dry-run)
    DRY_RUN=1
    shift
    ;;
  -h | --help)
    usage
    exit 0
    ;;
  *)
    echo "Unknown argument: $1" >&2
    usage >&2
    exit 1
    ;;
  esac
done

for bin in openssl base64; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "error: $bin is required but was not found in PATH" >&2
    exit 1
  fi
done
if [ "$DRY_RUN" -eq 0 ] && ! command -v kubectl >/dev/null 2>&1; then
  echo "error: kubectl is required (or pass --dry-run) but was not found in PATH" >&2
  exit 1
fi

WORKDIR=$(mktemp -d)
trap 'rm -rf "$WORKDIR"' EXIT

CA_KEY="$WORKDIR/ca.key"
CA_CRT="$WORKDIR/ca.crt"
SERVER_KEY="$WORKDIR/tls.key"
SERVER_CSR="$WORKDIR/server.csr"
SERVER_CRT="$WORKDIR/tls.crt"
SAN_CONF="$WORKDIR/san.cnf"

DNS1="${SERVICE}.${NAMESPACE}.svc"
DNS2="${SERVICE}.${NAMESPACE}.svc.cluster.local"

echo "Generating self-signed CA (valid ${CA_DAYS} days)..." >&2
openssl req -x509 -nodes -newkey rsa:2048 \
  -keyout "$CA_KEY" -out "$CA_CRT" \
  -days "$CA_DAYS" \
  -subj "/CN=docker-proxy-webhook-ca"

cat >"$SAN_CONF" <<EOF
[req]
distinguished_name = req_distinguished_name
req_extensions = v3_req
prompt = no
[req_distinguished_name]
CN = ${DNS1}
[v3_req]
keyUsage = digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth, clientAuth
subjectAltName = @alt_names
[alt_names]
DNS.1 = ${DNS1}
DNS.2 = ${DNS2}
EOF

echo "Generating server key + CSR (SANs: ${DNS1}, ${DNS2})..." >&2
openssl req -new -nodes -newkey rsa:2048 \
  -keyout "$SERVER_KEY" -out "$SERVER_CSR" \
  -config "$SAN_CONF"

echo "Signing server certificate with the CA (valid ${DAYS} days)..." >&2
openssl x509 -req \
  -in "$SERVER_CSR" \
  -CA "$CA_CRT" -CAkey "$CA_KEY" -CAcreateserial \
  -out "$SERVER_CRT" \
  -days "$DAYS" \
  -extensions v3_req -extfile "$SAN_CONF"

CA_BUNDLE=$(base64 <"$CA_CRT" | tr -d '\n')

if [ "$DRY_RUN" -eq 1 ]; then
  echo >&2
  echo "--dry-run: no changes applied to the cluster. Generated certificate:" >&2
  openssl x509 -in "$SERVER_CRT" -noout -text | grep "DNS:"
  openssl verify -CAfile "$CA_CRT" "$SERVER_CRT"
  openssl x509 -in "$SERVER_CRT" -noout -enddate
  echo >&2
  echo "Would run:" >&2
  echo "  kubectl create secret tls ${SECRET_NAME} --cert=<tls.crt> --key=<tls.key> -n ${NAMESPACE} --dry-run=client -o yaml | kubectl apply -f -" >&2
  echo "  kubectl patch mutatingwebhookconfiguration ${SERVICE} --type=json -p='[{\"op\":\"replace\",\"path\":\"/webhooks/<i>/clientConfig/caBundle\",\"value\":\"<base64 CA>\"}]' (one op per entry in .webhooks)" >&2
  echo "  kubectl patch validatingwebhookconfiguration ${SERVICE} --type=json -p='[...]' (same, skipped if the resource doesn't exist)" >&2
  exit 0
fi

echo "Creating/updating Secret ${SECRET_NAME} in namespace ${NAMESPACE}..." >&2
kubectl create secret tls "$SECRET_NAME" \
  --cert="$SERVER_CRT" --key="$SERVER_KEY" \
  -n "$NAMESPACE" \
  --dry-run=client -o yaml | kubectl apply -f -

# patch_webhook_cabundle patches every entry of .webhooks[].clientConfig.caBundle
# on the named MutatingWebhookConfiguration/ValidatingWebhookConfiguration.
# Must not fail if the resource type/name doesn't exist (e.g. the
# ValidatingWebhookConfiguration from feature 01 isn't applied yet).
patch_webhook_cabundle() {
  resource_type="$1"

  if ! kubectl get "$resource_type" "$SERVICE" >/dev/null 2>&1; then
    echo "Note: ${resource_type}/${SERVICE} not found, skipping." >&2
    return 0
  fi

  echo "Patching caBundle into ${resource_type}/${SERVICE}..." >&2
  webhook_count=$(kubectl get "$resource_type" "$SERVICE" -o jsonpath='{.webhooks[*].name}' | wc -w | tr -d ' ')

  patch="["
  i=0
  while [ "$i" -lt "$webhook_count" ]; do
    [ "$i" -gt 0 ] && patch="${patch},"
    patch="${patch}{\"op\":\"replace\",\"path\":\"/webhooks/${i}/clientConfig/caBundle\",\"value\":\"${CA_BUNDLE}\"}"
    i=$((i + 1))
  done
  patch="${patch}]"

  kubectl patch "$resource_type" "$SERVICE" --type=json -p="$patch"
}

patch_webhook_cabundle mutatingwebhookconfiguration
patch_webhook_cabundle validatingwebhookconfiguration

echo >&2
echo "Done. Certificate expiry:" >&2
openssl x509 -in "$SERVER_CRT" -noout -enddate
echo >&2
echo "Reminder: schedule renewal before expiry. Re-running this script renews" >&2
echo "in place (idempotent); controller-runtime hot-reloads the new cert" >&2
echo "without a pod restart." >&2
