FROM --platform=${BUILDPLATFORM} golang:1.21 AS builder

## docker buildx build injected build-args:
#BUILDPLATFORM — matches the current machine. (e.g. linux/amd64)
#BUILDOS — os component of BUILDPLATFORM, e.g. linux
#BUILDARCH — e.g. amd64, arm64, riscv64
#BUILDVARIANT — used to set ARM variant, e.g. v7
#TARGETPLATFORM — The value set with --platform flag on build
#TARGETOS - OS component from --platform, e.g. linux
#TARGETARCH - Architecture from --platform, e.g. arm64
#TARGETVARIANT

ARG TARGETOS
ARG TARGETARCH

ARG GOPROXY=https://proxy.golang.org,direct

WORKDIR /src

# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN --mount=type=bind,target=. \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download -x

RUN --mount=type=bind,target=. \
    --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go env && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} OUTPUT_DIR=/out make build

FROM alpine:3.18
LABEL maintainers="ApeCloud Authors"
LABEL description="open-local is a local disk management system"
RUN apk add --no-cache util-linux coreutils e2fsprogs e2fsprogs-extra xfsprogs xfsprogs-extra blkid file open-iscsi jq quota-tools
COPY --from=builder /out/open-local /bin/open-local
ENTRYPOINT ["open-local"]
