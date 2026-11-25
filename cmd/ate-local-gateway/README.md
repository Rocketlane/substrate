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

- Port-forwards to `ate-api-server` and `atenet-router` in `ate-system` (with
  `--in-cluster`, dials their Services directly instead).
- Mints a token for the `ate-client` service account, which is the bootstrap owner
  of actors, and calls the API with it. It mints a new one before the old one
  expires.
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

## Running inside the cluster

With `--in-cluster` the gateway runs as a pod: it uses its ServiceAccount, dials
`ate-api-server` and `atenet-router` directly, and answers `GET /healthz` on the
control port. `manifests/ate-local-gateway` deploys it in the `ate-gateway`
namespace (labelled `ate.dev/api-access=true`, which the `ate-system`
NetworkPolicies admit) with a NodePort Service: 30700 for the control API and
30701 for the proxy.

```sh
kubectl apply -k manifests/ate-local-gateway
```

Set the image with a kustomize `images` entry (the manifests name it
`ate-local-gateway`), and replace `egress-policy.json` with the policy every actor
the gateway creates should get.

Clients outside the cluster reach it through the nodes' NodePorts, for example
behind an internal load balancer. Keep that path private: as above, anything that
can reach the control port manages every actor `ate-client` owns.

Run one replica. Idle suspend is tracked in the gateway's memory, so a second
replica could suspend an actor whose tunnels go through the first.

## Running on EKS as a sidecar

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
