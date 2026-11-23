# ate-local-gateway

A small HTTP front for an Agent Substrate cluster. Clients that cannot speak the
`ate-api-server` gRPC API or the atenet-router mTLS CONNECT protocol (for example a
Python agent service) talk plain HTTP to the gateway on localhost. The gateway does
the Kubernetes and mTLS work for them.

| Listener | Default | Purpose |
|---|---|---|
| control (`--control`) | `127.0.0.1:18700` | create / get / resume / suspend / delete actors |
| proxy (`--proxy`) | `127.0.0.1:18701` | `/_ate/{atespace}/{actor}/{port}/...` forwarded to the actor through atenet-router |

Both listeners bind to loopback by default. Run the gateway as a sidecar in the
same network namespace as its only client. Don't expose these ports: the gateway
has no authentication of its own.

## What it does in the cluster

- Port-forwards to `ate-api-server` and `atenet-router` in `ate-system`.
- Mints a token for the `ate-client` service account, which is the bootstrap owner
  of actors, and calls the API with it.
- Gets a client certificate for the router through a `PodCertificateRequest`
  signed as `atenet-client`, and trusts the router through its
  `ClusterTrustBundle`.

Because of the `ate-client` token, anything that can reach the control listener
can manage every actor that `ate-client` owns. Treat the gateway's identity like
an admin credential for the actors.

## Running on kind (local development)

```sh
go run ./cmd/ate-local-gateway --context kind-kind
```

## Running on EKS

Build the image from the repository root:

```sh
docker build -f cmd/ate-local-gateway/Dockerfile -t ate-local-gateway .
```

The entrypoint writes a kubeconfig that authenticates with `aws eks get-token`,
using whatever AWS credentials the container has, such as an ECS task role. It
reads these environment variables:

| Variable | Required | Meaning |
|---|---|---|
| `ATE_GATEWAY_EKS_CLUSTER` | yes | EKS cluster name |
| `ATE_GATEWAY_EKS_ENDPOINT` | yes | Kubernetes API endpoint |
| `ATE_GATEWAY_EKS_CA` | yes | cluster CA, base64 (`certificateAuthority.data`) |
| `AWS_REGION` | yes | cluster region |
| `ATE_GATEWAY_EGRESS_POLICY` | no | protojson `EgressPolicy` applied to every actor the gateway creates |
| `ATE_GATEWAY_MAX_IDLE` | no | cap on an actor's idle timeout before it is suspended, e.g. `2m` |
| `ATE_GATEWAY_CONTROL` / `ATE_GATEWAY_PROXY` | no | listen addresses |

The endpoint and CA are passed in, so the role needs no `eks:DescribeCluster`. It
does need:

1. Network access to the cluster API on port 443.
2. An EKS access entry that maps the role to the Kubernetes group
   `ate-local-gateway`, with no access policy.
3. The RBAC for that group. `ate-setup install --eks` applies it from
   `manifests/ate-install/eks/prereqs/gateway-rbac.yaml`: in `ate-system`, get
   services, get/list pods, create pods/portforward, create a token for
   `ate-client`, manage podcertificaterequests; cluster-wide, read
   clustertrustbundles.
