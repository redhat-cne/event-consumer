# Cloud event consumer

Standalone consumer ported from cloud-event-proxy. It subscribes to the O-RAN
v2 event API, receives CloudEvent callbacks, and pulls CurrentState from CEPv1
or native CEPv2 in linuxptp-daemon. It has no legacy proxy or REST API dependency.

See the [consumer guide](cmd/cloud-event-consumer/README.md),
[deployment guide](manifests/README.md), and [credentials](manifests/auth/README.md).

```sh
make test build
make docker-build docker-push CONSUMER_IMG=quay.io/your-account/cloud-event-consumer:dev
make deploy-consumer SECURED=true CONSUMER_IMG=quay.io/your-account/cloud-event-consumer:dev
```

## Subscription behavior

The consumer subscribes at startup and retries pending subscriptions every 30 seconds.
Pass `--subscribe=false` to run only the callback server with its configured authentication.
This mode skips automatic subscriptions and CurrentState polling.

Deployments initially default to plaintext for compatibility. Set SECURED=true
for full mTLS and OAuth. Use the same JSON options as CEPv1:
enableMTLS, enableOAuth, requiredAudiences, certificate paths, service account
token path, tlsMinVersion and tlsCipherSuites. Both enabled mechanisms must pass.
Client CA material is published to ptp-event-publisher-client-ca; the operator
merges trust without overwriting the injected Service CA or stopping the operator.

For CEPv2 use /api/ocloudNotifications/v2/ and full node-scoped resource addresses.
The publisher and callback serving certificates and separate clientAuth
certificates must be provisioned before secured traffic can succeed.

The CEPv2 secured overlay uses the node-specific publisher Service and its
matching serving certificate. `NODE_NAME` defaults to the consumer pod's node;
schedule the consumer on the target PTP node or override that environment variable.
This keeps subscriptions and cached state on the correct publisher. To use CEPv1,
set `--http-event-publishers` to its certificate-bearing shared Service instead.
