# ate-local-gateway image for EKS (multi-arch). Build from the repository root (vendored modules):
#   docker buildx build --platform linux/arm64 -f cmd/ate-local-gateway/Dockerfile -t <repo>:<tag> .
FROM golang:1.27.4-bookworm AS build
WORKDIR /src
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -mod=vendor -trimpath -ldflags="-s -w" \
      -o /out/ate-local-gateway ./cmd/ate-local-gateway

# Amazon Linux 2023 ships AWS CLI v2, used by the kubeconfig exec plugin (`aws eks get-token`).
FROM amazonlinux:2023
RUN dnf install -y awscli-2 shadow-utils && dnf clean all && rm -rf /var/cache/dnf \
 && useradd -r -u 10001 gateway
COPY --from=build /out/ate-local-gateway /usr/local/bin/ate-local-gateway
COPY cmd/ate-local-gateway/entrypoint.sh /usr/local/bin/entrypoint.sh
USER gateway
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
