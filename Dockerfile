# The image exists to be copied from, e.g.
#   COPY --from=ghcr.io/ruakij/fuse-sandbox:vX.Y.Z /fuse-sandbox /usr/local/bin/
# Cross-compiling from the build platform, so a multi-arch build needs no
# emulated toolchain.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETARCH
ARG version=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${version}" -o /out/fuse-sandbox ./cmd/fuse-sandbox

FROM scratch
COPY --from=build /out/fuse-sandbox /fuse-sandbox
ENTRYPOINT ["/fuse-sandbox"]
