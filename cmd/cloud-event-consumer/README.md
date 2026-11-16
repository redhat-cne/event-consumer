# Cloud Event Consumer with Authentication

This directory contains examples of how to use the standalone cloud-event-consumer with authentication features including mTLS and OAuth.

## Files

- `main.go` - Main consumer application with authentication support
- `auth-config-example.json` - Example authentication configuration file
- `README.md` - This documentation file

For authentication usage examples, see [the sample client](../../examples/auth-examples/main.go).

## Authentication Features

The consumer supports two types of authentication:

### 1. mTLS (Mutual TLS)
- Client certificate authentication
- CA certificate validation
- Transport layer security
- Uses Service CA for serving certificates and a separate issuer for clientAuth certificates

### 2. OAuth
- ServiceAccount bearer-token authentication
- Tokens validated in-process against the Kubernetes TokenReview API (issuer, signature, expiry checked by the API server)
- Optional audience restriction via `requiredAudiences`

## Configuration

Authentication is configured using a JSON configuration file. The configuration supports:

```json
{
  "enableMTLS": true,
  "useServiceCA": true,
  "clientCertPath": "/etc/cloud-event-consumer/client-certs/tls.crt",
  "clientKeyPath": "/etc/cloud-event-consumer/client-certs/tls.key",
  "serverCertPath": "/etc/cloud-event-consumer/server-certs/tls.crt",
  "serverKeyPath": "/etc/cloud-event-consumer/server-certs/tls.key",
  "caCertPath": "/etc/cloud-event-consumer/ca-bundle/service-ca.crt",
  "enableOAuth": true,
  "requiredAudiences": ["https://kubernetes.default.svc"],
  "serviceAccountName": "consumer-sa",
  "serviceAccountToken": "/var/run/event-api-token/token"
}
```

### Configuration Options

#### mTLS Configuration
- `enableMTLS`: Enable/disable mTLS authentication
- `useServiceCA`: Compatibility hint; certificate files must still be provisioned and mounted
- `clientCertPath`: Path to client certificate file
- `clientKeyPath`: Path to client private key file
- `serverCertPath`: Path to the callback server certificate file
- `serverKeyPath`: Path to the callback server private key file
- `caCertPath`: Path to CA certificate file

#### OAuth Configuration
- `enableOAuth`: Enable/disable OAuth (ServiceAccount bearer-token) authentication
- `requiredAudiences`: Accepted audiences; TokenReview must return at least one matching audience. Empty uses the API server's default audience policy.
- `serviceAccountName`: ServiceAccount name for authentication
- `serviceAccountToken`: Path to the ServiceAccount token file presented as the bearer token

> Tokens are validated in-process against the Kubernetes TokenReview API. There
> is no client-configured issuer or JWKS URL — the API server verifies the
> token's issuer, signature, and expiry.

## Usage

### Basic Usage

```bash
# Run without authentication
./consumer --local-api-addr=localhost:8989 --http-event-publishers=localhost:9043

# Run with authentication
./consumer --local-api-addr=localhost:8989 --http-event-publishers=localhost:9043 --auth-config=/path/to/auth-config.json
```

### Command Line Options

- `--local-api-addr`: Local API address (default: localhost:8989)
- `--api-path`: REST API path (default: /api/ocloudNotifications/v2/)
- `--http-event-publishers`: Comma-separated list of publisher addresses
- `--auth-config`: Path to authentication configuration file (JSON format)

### Environment Variables

- `NODE_NAME`: Node name for resource addressing
- `NODE_IP`: Node IP address
- `CONSUMER_TYPE`: Consumer type (PTP, HW, MOCK)
- `ENABLE_STATUS_CHECK`: Enable periodic status checks

## Examples

### Example 1: mTLS Only

```json
{
  "enableMTLS": true,
  "useServiceCA": true,
  "clientCertPath": "/etc/cloud-event-consumer/client-certs/tls.crt",
  "clientKeyPath": "/etc/cloud-event-consumer/client-certs/tls.key",
  "serverCertPath": "/etc/cloud-event-consumer/server-certs/tls.crt",
  "serverKeyPath": "/etc/cloud-event-consumer/server-certs/tls.key",
  "caCertPath": "/etc/cloud-event-consumer/ca-bundle/service-ca.crt",
  "enableOAuth": false
}
```

### Example 2: OAuth Only

```json
{
  "enableMTLS": false,
  "enableOAuth": true,
  "caCertPath": "/etc/cloud-event-consumer/ca-bundle/service-ca.crt",
  "serverCertPath": "/etc/cloud-event-consumer/server-certs/tls.crt",
  "serverKeyPath": "/etc/cloud-event-consumer/server-certs/tls.key",
  "requiredAudiences": ["https://kubernetes.default.svc"],
  "serviceAccountName": "consumer-sa",
  "serviceAccountToken": "/var/run/event-api-token/token"
}
```

### Example 3: Combined mTLS + OAuth

```json
{
  "enableMTLS": true,
  "useServiceCA": true,
  "clientCertPath": "/etc/cloud-event-consumer/client-certs/tls.crt",
  "clientKeyPath": "/etc/cloud-event-consumer/client-certs/tls.key",
  "serverCertPath": "/etc/cloud-event-consumer/server-certs/tls.crt",
  "serverKeyPath": "/etc/cloud-event-consumer/server-certs/tls.key",
  "caCertPath": "/etc/cloud-event-consumer/ca-bundle/service-ca.crt",
  "enableOAuth": true,
  "requiredAudiences": ["https://kubernetes.default.svc"],
  "serviceAccountName": "consumer-sa",
  "serviceAccountToken": "/var/run/event-api-token/token"
}
```

## Development

### Building the Consumer

```bash
# Build the consumer
make build

# Build the authentication examples
go build -o build/auth-examples ./examples/auth-examples
```

### Running Examples

```bash
# Run the main consumer
./build/cloud-event-consumer --auth-config=cmd/cloud-event-consumer/auth-config-example.json

# Run the authentication examples
./build/auth-examples
```

## Integration with Kubernetes/OpenShift

### Deployment

The consumer can be deployed in Kubernetes/OpenShift with authentication using the manifests in the `manifests/` directory:

```bash
# From the event-consumer repository root
make deploy-consumer SECURED=true

# Verify deployment
kubectl get pods -n cloud-events
kubectl logs deployment/cloud-consumer-deployment -n cloud-events
```

#### Resource addressing

The consumer builds full node-scoped resource addresses from `NODE_NAME` and
`CONSUMER_TYPE`. Schedule it on the target PTP node or set `NODE_NAME` explicitly
to match that publisher. `CLUSTER_NAME` is not consumed by
this standalone Makefile and does not configure an OAuth issuer.

#### Updating the Audience Configuration at Runtime

If you need to change the required audiences after deployment:

Edit the complete JSON configuration, retaining all certificate paths. If you
change the audience, update both peers' required audiences and their
`serviceAccountToken.audience` projections to the same value. The standard
Kubernetes API credential remains separate for TokenReview calls.

```bash
oc edit configmap consumer-auth-config -n cloud-events

# Restart the consumer deployment
oc rollout restart deployment/cloud-consumer-deployment -n cloud-events
oc rollout status deployment/cloud-consumer-deployment -n cloud-events
```

**Verification Steps**
```bash
# 1. Check the configured audiences
oc get configmap consumer-auth-config -n cloud-events -o jsonpath='{.data.config\.json}' | jq '.requiredAudiences'

# 2. Check consumer logs for authentication
oc logs deployment/cloud-consumer-deployment -n cloud-events --tail=20 | grep -i "auth\|token"

# 4. Verify consumer can connect to PTP publisher
oc logs deployment/cloud-consumer-deployment -n cloud-events | grep -i "subscription\|publisher"
```

### Certificate Management

For mTLS authentication, certificates can be managed using:

1. **OpenShift Service CA** (serving certificates)
   - Automatic serving-certificate generation and renewal
   - Does not issue the clientAuth certificates required for mTLS
   - Processes must restart to load renewed certificate and trust files

2. **cert-manager** (alternative)
   - External certificate management
   - Custom certificate policies
   - Integration with external CAs

3. **Manual certificates** (development only)
   - Self-signed certificates
   - Manual rotation
   - Not recommended for production

### ServiceAccount Configuration

For OAuth authentication, configure the ServiceAccount:

The following discovery permissions are used in both modes. Secured callbacks
also require the TokenReview binding in [auth/rbac.yaml](../../manifests/auth/rbac.yaml)
and the projected token mount in the secured overlay.

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: consumer-sa
  namespace: cloud-events
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: cloud-event-consumer
  namespace: openshift-ptp
rules:
- apiGroups: [""]
  resources: ["services"]
  verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: cloud-event-consumer
  namespace: openshift-ptp
subjects:
- kind: ServiceAccount
  name: consumer-sa
  namespace: cloud-events
roleRef:
  kind: Role
  name: cloud-event-consumer
  apiGroup: rbac.authorization.k8s.io
```

## Troubleshooting

### Common Issues

1. **Certificate not found**
   ```
   Error: client certificate file not found: /path/to/cert
   ```
   - Verify certificate paths in configuration
   - Check certificate files exist and are readable
   - Ensure proper file permissions

2. **OAuth token not found**
   ```
   Error: service account token file not found: /path/to/token
   ```
   - Verify ServiceAccount token path
   - Check ServiceAccount exists and has proper permissions
   - Ensure token file is mounted correctly

3. **Authentication failed**
   ```
   Error: 401 Unauthorized
   ```
   - Verify the ServiceAccount token is valid and not expired
   - Confirm the publisher's ServiceAccount can `create` `tokenreviews` (authentication.k8s.io)
   - Ensure the token's audience matches one of the publisher's `requiredAudiences`

### Debugging

Enable debug logging to troubleshoot authentication issues:

```bash
# Set log level to debug
export LOG_LEVEL=debug

# Run consumer with debug logging
./consumer --auth-config=auth-config.json
```

### Health Checks

The consumer includes health check endpoints:

- `GET /health` - Basic health check
- `GET /ready` - Readiness check
- `GET /metrics` - Prometheus metrics

## Security Considerations

1. **Certificate Security**
   - Store certificates in Kubernetes secrets
   - Use proper file permissions (600 for private keys)
   - Rotate certificates regularly
   - Monitor certificate expiration

2. **Token Security**
   - Use short-lived tokens when possible
   - Rotate ServiceAccount tokens regularly
   - Monitor token usage and access patterns
   - Implement proper RBAC policies

3. **Network Security**
   - Use TLS for all communications
   - Implement network policies
   - Monitor network traffic
   - Use service mesh for additional security

## Contributing

When contributing to the authentication features:

1. Follow the existing code patterns
2. Add comprehensive tests
3. Update documentation
4. Consider backward compatibility
5. Test with both mTLS and OAuth configurations

## License

This code is licensed under the Apache License 2.0. See the LICENSE file for details.
