FROM --platform=$BUILDPLATFORM golang:1.27-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS TARGETARCH TARGETVARIANT VERSION=dev COMMIT=unknown BUILD_DATE=unknown
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} go build -trimpath -o /dockpaum \
    -ldflags "-s -w -X github.com/nicolaeser/dockpaum/internal/buildinfo.Version=$VERSION -X github.com/nicolaeser/dockpaum/internal/buildinfo.Commit=$COMMIT -X github.com/nicolaeser/dockpaum/internal/buildinfo.Date=$BUILD_DATE" \
    ./cmd/dockpaum

FROM busybox:1.37
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /dockpaum /dockpaum
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/dockpaum", "--healthcheck"]
ENTRYPOINT ["/dockpaum"]
