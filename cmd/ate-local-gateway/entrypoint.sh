#!/bin/sh
# Container entrypoint for ate-local-gateway on EKS (e.g. as an ECS sidecar next to the agent).
#
# Required env:
#   ATE_GATEWAY_EKS_CLUSTER    EKS cluster name
#   ATE_GATEWAY_EKS_ENDPOINT   Kubernetes API endpoint (https://...)
#   ATE_GATEWAY_EKS_CA         cluster CA, base64 (certificate_authority_data)
#   AWS_REGION                 cluster region
# Optional env:
#   ATE_GATEWAY_EGRESS_POLICY  protojson EgressPolicy applied to every actor created via the control API
#   ATE_GATEWAY_MAX_IDLE       cap on actors' idle timeout before suspend (e.g. 2m); default: no cap
#   ATE_GATEWAY_CONTROL        control API listen address (default 127.0.0.1:18700)
#   ATE_GATEWAY_PROXY          data-plane proxy listen address (default 127.0.0.1:18701)
#
# Credentials: the kubeconfig authenticates with `aws eks get-token`, i.e. with whatever AWS credentials the container
# has (the ECS task role). That role needs an EKS access entry; its Kubernetes permissions come from RBAC (see
# README.md). No eks:DescribeCluster is needed because endpoint and CA are passed in.
set -eu

: "${ATE_GATEWAY_EKS_CLUSTER:?}" "${ATE_GATEWAY_EKS_ENDPOINT:?}" "${ATE_GATEWAY_EKS_CA:?}" "${AWS_REGION:?}"

KUBECONFIG_PATH=/tmp/kubeconfig
cat > "$KUBECONFIG_PATH" <<KCFG
apiVersion: v1
kind: Config
clusters:
- name: eks
  cluster:
    server: ${ATE_GATEWAY_EKS_ENDPOINT}
    certificate-authority-data: ${ATE_GATEWAY_EKS_CA}
users:
- name: gateway
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: aws
      args: ["eks", "get-token", "--cluster-name", "${ATE_GATEWAY_EKS_CLUSTER}", "--region", "${AWS_REGION}", "--output", "json"]
      interactiveMode: Never
contexts:
- name: eks
  context:
    cluster: eks
    user: gateway
current-context: eks
KCFG

set -- --kubeconfig "$KUBECONFIG_PATH" --context eks \
  --control "${ATE_GATEWAY_CONTROL:-127.0.0.1:18700}" \
  --proxy "${ATE_GATEWAY_PROXY:-127.0.0.1:18701}"
if [ -n "${ATE_GATEWAY_EGRESS_POLICY:-}" ]; then
  printf '%s' "$ATE_GATEWAY_EGRESS_POLICY" > /tmp/egress-policy.json
  set -- "$@" --egress-policy /tmp/egress-policy.json
fi
if [ -n "${ATE_GATEWAY_MAX_IDLE:-}" ]; then
  set -- "$@" --max-idle "$ATE_GATEWAY_MAX_IDLE"
fi
exec /usr/local/bin/ate-local-gateway "$@"
