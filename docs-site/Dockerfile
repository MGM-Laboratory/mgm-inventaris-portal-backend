# syntax=docker/dockerfile:1

# The image of the API docs site. The build context is the root of the repository, because the page is built from openapi.yaml there and
# the server is the Go code in cmd/docs-server. Build it with "docker build -f docs-site/Dockerfile -t mgm-inventaris-portal-docs ." from
# the root. The file docs-site/Dockerfile.dockerignore decides what the context holds, and the root .dockerignore is for the API image only.

# The page is plain files and does not depend on the platform, so the build runs on the machine that builds the image.
FROM --platform=$BUILDPLATFORM node:24.22.3-alpine3.24@sha256:116b800c3754b440ee0aa3ed2f20f5bf3d99a554bfd9adeea09ea8017f6e85f5 AS page

WORKDIR /src/docs-site

# The lock file pins the Redocly CLI by version and by hash, and the install runs no script of any package.
COPY docs-site/package.json docs-site/package-lock.json ./
RUN npm ci --ignore-scripts --no-audit --no-fund

# The mode is set because the files come from the checkout, and a mode of 0600 there would stop the server user from reading the spec.
COPY --chmod=0644 redocly.yaml openapi.yaml /src/
COPY docs-site/finish-page.mjs ./

# Builds dist/index.html with the viewer inside it, then writes the gzip copy and the Content-Security-Policy file. The build fails when
# a tag of the page points at another host, see finish-page.mjs.
RUN npm run build

# The server is the same Go image as the one of the API image, so the two have one Go version to keep current.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:eb61c3873c97416fff0116553fff65d67a3549f72e11d9758849a689cdd8b523 AS server

# The Go version of go.mod has to be the one in this image. With local, a build on an older image fails and never downloads a toolchain.
ENV GOTOOLCHAIN=local

WORKDIR /src

# The server uses only the standard library, so nothing is downloaded. The module files come along so that the build sees the same Go
# version line as the API image.
COPY go.mod go.sum ./
COPY cmd/docs-server ./cmd/docs-server

ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
# A repeatable build, as in the Dockerfile of the API. CGO is off, so the binary is static. The paths are trimmed, the build id is empty
# and the version control stamp is off, so two builds of one commit give the same file.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -buildvcs=false -ldflags="-s -w -buildid= -X main.version=${VERSION}" -o /out/docs-server ./cmd/docs-server

# The runtime image has no shell, no package manager and nothing else than the server and the files of the site.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3

ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="mgm-inventaris-portal-docs" \
      org.opencontainers.image.description="The API docs of the LabMGM inventory portal, built from openapi.yaml" \
      org.opencontainers.image.source="https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend" \
      org.opencontainers.image.licenses="GPL-3.0-only" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"

COPY --from=server --chmod=0755 /out/docs-server /docs-server
# No --chmod here. It would also apply to the folder that this line creates, and a folder without the execute bit cannot be entered by
# the exporter of the repeatable build. The files are read only for the server user (0644 by the umask of the build stages).
COPY --from=page /src/docs-site/dist/index.html /src/docs-site/dist/index.html.gz /src/docs-site/dist/csp.txt /src/openapi.yaml /site/

# The server reads these two settings. The files are read once at start, and the folder is the only place that it reads from.
ENV DOCS_ADDR=:8080 \
    DOCS_DIR=/site

EXPOSE 8080

USER 65532:65532

# The image has no shell and no curl, so the binary checks itself with GET /healthz on the loopback address. It needs no other setting.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/docs-server", "healthcheck"]

ENTRYPOINT ["/docs-server"]
