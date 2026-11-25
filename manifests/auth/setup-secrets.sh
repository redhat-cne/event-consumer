#!/bin/bash

# setup-secrets.sh - provision the authentication material for the secured
# cloud-event-consumer (mTLS + OAuth on the O-RAN ocloudNotifications v2 API).
#
# It creates, in the consumer namespace (cloud-events):
#   - consumer-client-certs : the consumer's clientAuth keypair used for mTLS TO
#                             the publisher. Minted out-of-band because OpenShift
#                             Service CA serving certs are serverAuth-only and are
#                             NOT valid as client certs.
#   - server-ca-bundle      : the Service CA cert, so the consumer trusts the
#                             publisher's server certificate, plus the client CA
#                             so its callback server trusts the publisher client.
#   - consumer-auth-config  : config.json consumed via --auth-config.
# The consumer's own callback server cert (consumer-server-certs) is minted by
# the Service CA via the serving-cert annotation the secured overlay adds to the
# consumer-events-subscription-service Service.
#
# It also publishes the client CA into the publisher namespace (openshift-ptp) as
# the ConfigMap ptp-event-publisher-client-ca, which the ptp-operator folds into
# the publisher's trust bundle (ca-bundle.crt) so the publisher trusts this
# consumer's client certificate. This replaces the old manual "overwrite the
# publisher CA bundle + scale the operator to 0" workaround.
# It creates ptp-event-publisher-client-tls in openshift-ptp from the same client
# CA so outbound push callbacks use mTLS as well as a bearer token.
#
# Client-cert provisioning method (CERT_MANAGER):
#   - default (CERT_MANAGER unset/false): openssl. Zero cluster dependencies;
#     a client CA + client cert are generated into ${DIR_CERTS} (default
#     /tmp/certs) and loaded into the consumer-client-certs Secret.
#   - CERT_MANAGER=true: cert-manager. A self-signed CA Issuer is bootstrapped
#     and auth/client-cert-service.yaml mints consumer-client-certs.

set -euo pipefail

NAMESPACE="${NAMESPACE:-cloud-events}"
PUBLISHER_NAMESPACE="${PUBLISHER_NAMESPACE:-openshift-ptp}"
CLIENT_CA_CONFIGMAP="${CLIENT_CA_CONFIGMAP:-ptp-event-publisher-client-ca}"
PUBLISHER_CLIENT_SECRET="${PUBLISHER_CLIENT_SECRET:-ptp-event-publisher-client-tls}"
PUBLISHER_DAEMONSET="${PUBLISHER_DAEMONSET:-linuxptp-daemon}"
CONSUMER_DEPLOYMENT="${CONSUMER_DEPLOYMENT:-cloud-consumer-deployment}"
DIR_CERTS="${DIR_CERTS:-/tmp/certs}"
CERT_MANAGER="${CERT_MANAGER:-false}"
CEP_VERSION="${CEP_VERSION:-2}"
TIMEOUT="${TIMEOUT:-60}"
ROLLOUT_TIMEOUT="${ROLLOUT_TIMEOUT:-180s}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

case "$CEP_VERSION" in
    1|2) ;;
    *) echo "Error: CEP_VERSION must be 1 or 2" >&2; exit 1 ;;
esac

echo "Setting up authentication for cloud-event-consumer"
echo "  consumer namespace : $NAMESPACE"
echo "  publisher namespace: $PUBLISHER_NAMESPACE"
echo "  client-cert method : $([ "$CERT_MANAGER" = "true" ] && echo cert-manager || echo "openssl (DIR_CERTS=$DIR_CERTS)")"

if ! oc get project openshift-service-ca >/dev/null 2>&1 && ! oc get ns openshift-service-ca-operator >/dev/null 2>&1; then
    echo "Error: this script requires OpenShift with the Service CA operator"
    echo "For generic Kubernetes see auth/certificate-example.md"
    exit 1
fi

# wait_for <description> <command...> : polls until the command succeeds.
wait_for() {
    local desc="$1"; shift
    local count=0
    while [ "$count" -lt "$TIMEOUT" ]; do
        if "$@" >/dev/null 2>&1; then
            echo "✓ $desc"
            return 0
        fi
        echo "  waiting for $desc ... ($count/$TIMEOUT)"
        sleep 2
        count=$((count + 1))
    done
    echo "Error: timed out waiting for $desc"
    return 1
}

# apply_tls_secret preserves the immutable type of an existing Secret while
# updating its tls.crt and tls.key data. Older deployments may already have an
# Opaque placeholder; Secret volumes and crypto/tls use the data keys, not the
# Kubernetes type. New Secrets use kubernetes.io/tls.
apply_tls_secret() {
    local name="$1" namespace="$2" cert_file="$3" key_file="$4"
    local existing_type
    existing_type="$(oc get secret "$name" -n "$namespace" -o jsonpath='{.type}' 2>/dev/null || true)"

    case "$existing_type" in
        ""|kubernetes.io/tls)
            oc create secret tls "$name" \
                --cert="$cert_file" --key="$key_file" \
                --namespace="$namespace" --dry-run=client -o yaml | oc apply -f -
            ;;
        Opaque)
            oc create secret generic "$name" \
                --from-file=tls.crt="$cert_file" --from-file=tls.key="$key_file" \
                --namespace="$namespace" --dry-run=client -o yaml | oc apply -f -
            ;;
        *)
            echo "Error: refusing to replace unsupported Secret type '$existing_type' for $namespace/$name" >&2
            return 1
            ;;
    esac
}

# The CEPv1 publisher and standalone consumer load TLS material at startup.
restart_and_wait() {
    local resource="$1" namespace="$2"
    if ! oc get "$resource" -n "$namespace" >/dev/null 2>&1; then
        echo "Warning: $namespace/$resource not found; skipping TLS reload."
        return 0
    fi
    oc rollout restart "$resource" -n "$namespace"
    oc rollout status "$resource" -n "$namespace" --timeout="$ROLLOUT_TIMEOUT"
}

# Probe with the newly provisioned credentials. The Secret update can reach the
# running consumer before or after the publisher reloads its client CA.
publisher_auth_ready() {
    oc --request-timeout=10s exec "deployment/$CONSUMER_DEPLOYMENT" -n "$NAMESPACE" \
        -c cloud-event-consumer -- sh -ec '
        auth_dir=/etc/cloud-event-consumer
        cert="$auth_dir/client-certs/tls.crt"
        fingerprint=$(sha256sum "$cert")
        [ "${fingerprint%% *}" = "$1" ] || exit 1

        url=$2
        if [ -z "$url" ]; then
            node=${NODE_NAME%%.*}
            url="https://ptp-event-publisher-service-$node.$3.svc.cluster.local:9043/api/ocloudNotifications/v2/subscriptions"
        fi
        token=$(cat /var/run/event-api-token/token)
        # Feed the bearer header through stdin to keep it out of process arguments.
        status=$(printf "Authorization: Bearer %s\n" "$token" |
            curl --silent --show-error --max-time 5 --proto "=https" \
                --cacert "$auth_dir/ca-bundle/service-ca.crt" \
                --cert "$cert" --key "$auth_dir/client-certs/tls.key" \
                --header @- --output /dev/null --write-out "%{http_code}" "$url")
        [ "$status" = 200 ]
    ' sh "$1" "${PUBLISHER_AUTH_URL:-}" "$PUBLISHER_NAMESPACE"
}

########################################
# 1. consumer client certificate (mTLS to the publisher)
########################################
CLIENT_CA_PEM=""
if [ "$CERT_MANAGER" = "true" ]; then
    echo "Bootstrapping a cert-manager CA issuer and client certificate..."
    cat <<EOF | oc apply -f -
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: selfsigned-issuer
  namespace: $NAMESPACE
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: cloud-events-ca
  namespace: $NAMESPACE
spec:
  isCA: true
  commonName: cloud-events-ca
  secretName: cloud-events-ca
  privateKey:
    algorithm: RSA
    size: 4096
  issuerRef:
    name: selfsigned-issuer
    kind: Issuer
    group: cert-manager.io
---
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: cloud-events-ca-issuer
  namespace: $NAMESPACE
spec:
  ca:
    secretName: cloud-events-ca
EOF
    # auth/client-cert-service.yaml mints consumer-client-certs from the CA issuer.
    oc apply -f "$SCRIPT_DIR/client-cert-service.yaml"
    wait_for "consumer-client-certs (cert-manager)" oc get secret consumer-client-certs -n "$NAMESPACE"
    wait_for "publisher-client-certs (cert-manager)" oc get secret publisher-client-certs -n "$NAMESPACE"
    wait_for "cloud-events-ca secret (cert-manager)" oc get secret cloud-events-ca -n "$NAMESPACE"
    CLIENT_CA_PEM="$(oc get secret cloud-events-ca -n "$NAMESPACE" -o jsonpath='{.data.tls\.crt}' | base64 -d)"
else
    echo "Generating a client CA and client certificate with openssl into $DIR_CERTS..."
    umask 077
    if [ -L "$DIR_CERTS" ]; then
        echo "ERROR: DIR_CERTS must not be a symbolic link: $DIR_CERTS" >&2
        exit 1
    fi
    if [ -e "$DIR_CERTS" ] && [ ! -d "$DIR_CERTS" ]; then
        echo "ERROR: DIR_CERTS exists but is not a directory: $DIR_CERTS" >&2
        exit 1
    fi
    mkdir -p -m 700 "$DIR_CERTS"
    chmod 700 "$DIR_CERTS"
    ext="$DIR_CERTS/client-ext.cnf"
    printf 'extendedKeyUsage=clientAuth\nkeyUsage=digitalSignature,keyEncipherment\n' > "$ext"
    # Client CA (only regenerated if missing, so re-runs keep the published CA stable).
    if [ ! -f "$DIR_CERTS/client-ca.crt" ] || [ ! -f "$DIR_CERTS/client-ca.key" ]; then
        openssl req -x509 -newkey rsa:4096 -nodes \
            -keyout "$DIR_CERTS/client-ca.key" -out "$DIR_CERTS/client-ca.crt" \
            -days 365 -subj "/CN=cloud-event-consumer-client-ca" >/dev/null 2>&1
    fi
    # Consumer client cert (clientAuth), signed by the client CA.
    openssl req -newkey rsa:4096 -nodes \
        -keyout "$DIR_CERTS/client.key" -out "$DIR_CERTS/client.csr" \
        -subj "/O=cloud-event-consumer/CN=cloud-event-consumer" >/dev/null 2>&1
    openssl x509 -req -in "$DIR_CERTS/client.csr" \
        -CA "$DIR_CERTS/client-ca.crt" -CAkey "$DIR_CERTS/client-ca.key" -CAcreateserial \
        -out "$DIR_CERTS/client.crt" -days 365 -extfile "$ext" >/dev/null 2>&1
    apply_tls_secret consumer-client-certs "$NAMESPACE" \
        "$DIR_CERTS/client.crt" "$DIR_CERTS/client.key"
    CLIENT_CA_PEM="$(cat "$DIR_CERTS/client-ca.crt")"
    echo "✓ consumer-client-certs secret created (openssl)"

    openssl req -newkey rsa:4096 -nodes \
        -keyout "$DIR_CERTS/publisher-client.key" -out "$DIR_CERTS/publisher-client.csr" \
        -subj "/O=ptp-event-publisher/CN=ptp-event-publisher" >/dev/null 2>&1
    openssl x509 -req -in "$DIR_CERTS/publisher-client.csr" \
        -CA "$DIR_CERTS/client-ca.crt" -CAkey "$DIR_CERTS/client-ca.key" -CAcreateserial \
        -out "$DIR_CERTS/publisher-client.crt" -days 365 -extfile "$ext" >/dev/null 2>&1
fi

########################################
# 2. publish the client CA to the publisher so it trusts this consumer
########################################
echo "Publishing client CA to $PUBLISHER_NAMESPACE/$CLIENT_CA_CONFIGMAP..."
if oc get namespace "$PUBLISHER_NAMESPACE" >/dev/null 2>&1; then
    oc create configmap "$CLIENT_CA_CONFIGMAP" \
        --from-literal=client-ca.crt="$CLIENT_CA_PEM" \
        --namespace="$PUBLISHER_NAMESPACE" --dry-run=client -o yaml | oc apply -f -
    echo "✓ $CLIENT_CA_CONFIGMAP published (ptp-operator folds it into the publisher trust bundle)"

    if [ "$CERT_MANAGER" = "true" ]; then
        PUBLISHER_CLIENT_CERT="$(oc get secret publisher-client-certs -n "$NAMESPACE" -o jsonpath='{.data.tls\.crt}' | base64 -d)"
        PUBLISHER_CLIENT_KEY="$(oc get secret publisher-client-certs -n "$NAMESPACE" -o jsonpath='{.data.tls\.key}' | base64 -d)"
        apply_tls_secret "$PUBLISHER_CLIENT_SECRET" "$PUBLISHER_NAMESPACE" \
            <(printf '%s' "$PUBLISHER_CLIENT_CERT") \
            <(printf '%s' "$PUBLISHER_CLIENT_KEY")
    else
        apply_tls_secret "$PUBLISHER_CLIENT_SECRET" "$PUBLISHER_NAMESPACE" \
            "$DIR_CERTS/publisher-client.crt" "$DIR_CERTS/publisher-client.key"
    fi
    echo "✓ $PUBLISHER_CLIENT_SECRET published for authenticated push callbacks"
else
    echo "Warning: namespace $PUBLISHER_NAMESPACE not found; skipping client-CA publish."
    echo "         The publisher will not trust this consumer's client cert until"
    echo "         $CLIENT_CA_CONFIGMAP exists in $PUBLISHER_NAMESPACE."
fi

########################################
# 3. server CA bundle (consumer trusts the publisher's server cert)
########################################
wait_for "Service CA injection into server-ca-bundle-configmap" \
    bash -c "oc get configmap server-ca-bundle-configmap -n $NAMESPACE -o jsonpath='{.data.service-ca\.crt}' 2>/dev/null | grep -q 'BEGIN CERTIFICATE'"
CA_CERT="$(oc get configmap server-ca-bundle-configmap -n "$NAMESPACE" -o jsonpath='{.data.service-ca\.crt}')"
TRUST_BUNDLE="${CA_CERT%$'\n'}"$'\n'"${CLIENT_CA_PEM%$'\n'}"$'\n'
oc create secret generic server-ca-bundle \
    --from-literal=service-ca.crt="$TRUST_BUNDLE" \
    --namespace="$NAMESPACE" --dry-run=client -o yaml | oc apply -f -
echo "✓ server-ca-bundle secret created"

########################################
# 4. consumer callback server cert (Service CA serving cert)
########################################
wait_for "consumer-server-certs serving cert" oc get secret consumer-server-certs -n "$NAMESPACE"

########################################
# 5. consumer auth config
########################################
echo "Creating consumer-auth-config ConfigMap..."
oc delete configmap consumer-auth-config -n "$NAMESPACE" --ignore-not-found=true
cat <<EOF | oc apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: consumer-auth-config
  namespace: $NAMESPACE
data:
  config.json: |
    {
      "enableMTLS": true,
      "useServiceCA": true,
      "clientCertPath": "/etc/cloud-event-consumer/client-certs/tls.crt",
      "clientKeyPath": "/etc/cloud-event-consumer/client-certs/tls.key",
      "serverCertPath": "/etc/cloud-event-consumer/server-certs/tls.crt",
      "serverKeyPath": "/etc/cloud-event-consumer/server-certs/tls.key",
      "caCertPath": "/etc/cloud-event-consumer/ca-bundle/service-ca.crt",
      "enableOAuth": true,
      "useOpenShiftOAuth": true,
      "requiredAudiences": ["https://kubernetes.default.svc"],
      "serviceAccountName": "consumer-sa",
      "serviceAccountToken": "/var/run/event-api-token/token"
    }
EOF
echo "✓ consumer-auth-config ConfigMap created"

if [ "$CEP_VERSION" = "1" ]; then
    restart_and_wait "daemonset/$PUBLISHER_DAEMONSET" "$PUBLISHER_NAMESPACE"
else
    consumer_cert_sha256="$(oc get secret consumer-client-certs -n "$NAMESPACE" \
        -o jsonpath='{.data.tls\.crt}' | base64 -d | openssl dgst -sha256 -r)"
    wait_for "CEPv2 publisher accepts the new consumer credentials" \
        publisher_auth_ready "${consumer_cert_sha256%% *}"
fi
restart_and_wait "deployment/$CONSUMER_DEPLOYMENT" "$NAMESPACE"

echo ""
echo "Authentication setup completed successfully."
