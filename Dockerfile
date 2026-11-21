# syntax=docker/dockerfile:1

# The build context is the root of the repository, so build it with "docker build -t mgm-inventaris-portal-backend ." from there.

# The build runs on the machine that builds the image and cross-compiles for the target, so an arm64 image needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:eb61c3873c97416fff0116553fff65d67a3549f72e11d9758849a689cdd8b523 AS build

# The Go version of go.mod has to be the one in this image. With local, a build on an older image fails and never downloads a toolchain,
# which would make the build depend on the network.
ENV GOTOOLCHAIN=local

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
# A repeatable build. CGO is off, so the binary is static. The paths are trimmed, the build id is empty, and the version control
# stamp is off, so two builds of one commit give the same file. The only input that changes the file is VERSION.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -buildvcs=false -ldflags="-s -w -buildid= -X main.version=${VERSION}" -o /out/api ./cmd/api

# The runtime image has no shell, no package manager and nothing else than the binary and the files of the base image.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3

ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="mgm-inventaris-portal-backend" \
      org.opencontainers.image.description="The backend of the LabMGM inventory portal" \
      org.opencontainers.image.source="https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend" \
      org.opencontainers.image.licenses="GPL-3.0-only" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"

COPY --from=build /out/api /api

# The API listens on 8080. The health listener has its own port that stays inside the container and is never exposed. A check that
# runs in the container reaches it on the loopback address, which is why the address below is the default of the service.
ENV HTTP_ADDR=:8080 \
    HEALTH_ADDR=127.0.0.1:8081

EXPOSE 8080

USER 65532:65532

# The image has no shell and no curl, so the binary checks itself with GET /healthz of the health listener. It needs no database setting.
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 CMD ["/api", "healthcheck"]

ENTRYPOINT ["/api"]
