# syntax=docker/dockerfile:1

# The image of the API docs site. The build context is the root of the repository, because the page is built from openapi.yaml there.
# Build it with "docker build -f docs-site/Dockerfile -t mgm-inventaris-portal-docs ." from the root. The file
# docs-site/Dockerfile.dockerignore decides what the context holds, and the root .dockerignore is for the API image only.

# The page is plain files and does not depend on the platform, so the build runs on the machine that builds the image.
FROM --platform=$BUILDPLATFORM node:24.22.3-alpine3.24@sha256:116b800c3754b440ee0aa3ed2f20f5bf3d99a554bfd9adeea09ea8017f6e85f5 AS build

WORKDIR /src/docs-site

# The lock file pins the Redocly CLI by version and by hash, and the install runs no script of any package.
COPY docs-site/package.json docs-site/package-lock.json ./
RUN npm ci --ignore-scripts --no-audit --no-fund

COPY redocly.yaml openapi.yaml /src/
COPY docs-site/finish-page.mjs ./

# Builds dist/index.html with the viewer inside it, then writes the gzip copy and the Content-Security-Policy snippet. The build
# fails when the page needs a file from another host, see finish-page.mjs.
RUN npm run build

# nginx as an unprivileged user on port 8080. The image has no other server and no content than the files copied below.
FROM nginxinc/nginx-unprivileged:1.30.6-alpine3.24@sha256:f5116ed4606f3eb015dd4a8e28cb9b351476dc0ad4bc6e9f93b19cec130cdf03

ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="mgm-inventaris-portal-docs" \
      org.opencontainers.image.description="The API docs of the LabMGM inventory portal, built from openapi.yaml" \
      org.opencontainers.image.source="https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend" \
      org.opencontainers.image.licenses="GPL-3.0-only" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"

COPY --chmod=0644 docs-site/nginx.conf /etc/nginx/nginx.conf
COPY --chmod=0644 docs-site/headers.conf /etc/nginx/headers.conf
COPY --from=build --chmod=0644 /src/docs-site/dist/csp.conf /etc/nginx/csp.conf
COPY --from=build --chmod=0644 /src/docs-site/dist/index.html /src/docs-site/dist/index.html.gz /src/openapi.yaml /usr/share/nginx/html/

EXPOSE 8080

USER 101:101

# The image has no curl, so the check uses the wget of Alpine. /healthz answers 200 with no body and writes no log line.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8080/healthz"]

# The start scripts of the base image try to edit /etc/nginx/conf.d, which a read-only root file system refuses. They have nothing
# to do here, because nginx.conf is complete, so nginx starts directly.
ENTRYPOINT []
CMD ["nginx", "-g", "daemon off;"]
