# Deployment

The portal runs from two Docker images on one host. The backend image comes from this repository and the frontend image comes from [`mgm-inventaris-portal-frontend`](https://github.com/MGM-Laboratory/mgm-inventaris-portal-frontend). CI builds, scans, signs and publishes the backend image, as [`ci-cd.md`](ci-cd.md) explains. This page covers how to run both images on a host and how to update them without downtime. A third image, the API docs site, runs on its own, and the section "The API docs site" below covers it.

## The images

| Image | Source | Registry name | Port in the container |
| --- | --- | --- | --- |
| Backend | The `Dockerfile` in the root of this repository | `ghcr.io/mgm-laboratory/mgm-inventaris-portal-backend` | 8080 |
| Frontend | The frontend repository | `ghcr.io/mgm-laboratory/mgm-inventaris-portal-frontend` | 3000 |
| Docs site | `docs-site/Dockerfile` in this repository | `ghcr.io/mgm-laboratory/mgm-inventaris-portal-docs` | 8080 |

The backend image holds one static Go binary at `/api` on a distroless base, and it runs as the numeric user 65532. It has no shell and no package manager. The image has the tags `latest` and `sha-<commit>` for `main`, `pr-<number>` for a pull request of this repository, and `1.2.3` for a release. A release also has the tag `1.2`, and a release of version 1.0.0 or later also has the tag `1`. The signed copy on Docker Hub is `<namespace>/mgm-inventaris-portal-backend`. The docs image has the same tags, and its signed copy on Docker Hub is `<namespace>/mgm-inventaris-portal-docs`.

## The files of `deploy/`

- `compose.production.example.yml` runs two copies of the backend and two copies of the frontend, named `api-1`, `api-2`, `web-1` and `web-2`.
- `rolling-update.sh` updates the copies one at a time.
- `.env.example` lists the settings of the compose file and of the backend.
- `docs/compose.example.yml` runs one copy of the docs site, named `inventaris-docs`, and `docs/.env.example` lists its settings. The folder is separate from the stack above, because the docs site shares nothing with it.

## How the copies are laid out

- Every copy has a fixed name and its own host port. The ports listen on the address of `API_BIND` and `WEB_BIND`, which is the loopback address by default, so only a reverse proxy on the same host reaches them. A proxy on another host needs the address of the network interface that it reaches.
- The four host ports are settings with no default (`API_PORT_1`, `API_PORT_2`, `WEB_PORT_1` and `WEB_PORT_2`). Compose stops with a message until all four are set, so a copy never takes a port that another app uses by chance. The image tags are settings with no default too (`API_IMAGE_TAG` and `WEB_IMAGE_TAG`).
- The backend copies load the whole `.env` as their environment. The frontend copies get no `env_file`, so they never see the database URL or the key of the TOTP secrets.
- The database is a separate container that other apps may share. The backend copies join its Docker network, which has to exist already. Compose never creates or removes it. The name is `DB_NETWORK`, which is `postgres_postgres` by default.
- Every copy runs with a read only file system, no Linux capabilities, `no-new-privileges`, a reaper as process 1 and limits for memory and CPU. The writable paths are tmpfs mounts. The frontend needs a writable `/app/.next/cache`.
- The backend waits up to 3 seconds after a stop signal so the proxy sees the change, and then lets the requests that run finish for up to 15 seconds. The compose file gives it 30 seconds before Docker kills it.
- Docker keeps at most `LOG_MAX_FILES` log files of `LOG_MAX_SIZE` for each copy, so the logs cannot fill the disk.
- The stack has no volume. The only state is the database. The volume rule of the hosts, which puts volumes under `~/docker-volume/<name>`, has nothing to apply to here.

## Health routes

The backend has two listeners. The API listens on `HTTP_ADDR` and the health routes listen on `HEALTH_ADDR`. The image sets `HTTP_ADDR` to `:8080` and `HEALTH_ADDR` to `127.0.0.1:8081`, and it exposes only port 8080. The health port is inside the container and no published port reaches it.

- `GET /healthz` reads nothing and answers `{"status":"ok"}` while the process runs.
- `GET /readyz` pings the database and answers `503` while the pool cannot reach it or while the copy drains for a stop.

The image has no shell and no curl, so the binary checks itself. The command `api healthcheck` makes one `GET /healthz` on `HEALTH_ADDR` with a timeout of 3 seconds, and it exits 0 for the answer 200 and 1 for anything else. It reads no setting except `HEALTH_ADDR`. The `HEALTHCHECK` of the image and the `healthcheck` of the compose file both run it. The compose file and the script use only `/healthz`, and nothing in them reads `/readyz`.

A copy opens the database and applies the migrations before it listens, so a copy that Docker reports healthy has reached the database at start. The script also asks `GET /api/config` on the published port of each backend copy and `GET /healthz` on the published port of each frontend copy.

## Settings to set before the first start

Copy `.env.example` to `.env` in the folder of the stack and set these values. The `README.md` has the table with every other setting, its default and its bounds.

| Setting | What to set |
| --- | --- |
| `API_PORT_1`, `API_PORT_2`, `WEB_PORT_1`, `WEB_PORT_2` | Four ports that are free on the host |
| `API_IMAGE_TAG`, `WEB_IMAGE_TAG` | The tags to start, as a version such as `1.2.3` or a tag of a commit such as `sha-1a2b3c4`. Until the first release the images have only the commit tags. `rolling-update.sh` writes them on every update |
| `DATABASE_URL` | The URL of the database of this service, with its own user and a password from `openssl rand -hex 64` |
| `PUBLIC_BASE_URL` | The public address of the portal, with `https` and the host only |
| `TOTP_ENCRYPTION_KEY` | A key from `openssl rand -base64 32`. Keep a copy apart from the database, because a lost key makes every stored TOTP secret unreadable. The service refuses the placeholder of the template |
| `TRUSTED_PROXIES` | The addresses and ranges of the reverse proxy as the container sees them. The template lists the three private ranges, and the real peer has to be checked on the host, see the section on checking the proxy address |

`ENVIRONMENT=production` and `SECURE_COOKIE=true` stay as they are. Production refuses an `http` `PUBLIC_BASE_URL` and a `SECURE_COOKIE` of `false`. `MIGRATE_ON_START=true` makes each copy apply the embedded migrations at start under an advisory lock. One copy applies them while the others wait and then find nothing to do. A copy that cannot migrate never becomes healthy, so `rolling-update.sh` stops at it.

## First setup of a host

1. Create a database and a user for the service in the shared PostgreSQL container, with a password from `openssl rand -hex 64`. Send the statements on stdin to `psql`, so the password never appears in a command line.

   ```sql
   create database inventaris_production;
   create user inventaris_production_user with password '<password>';
   alter database inventaris_production owner to inventaris_production_user;
   ```

2. Make the folder `~/docker/inventaris` on the host. Copy `compose.production.example.yml` into it as `compose.yml`, copy `rolling-update.sh` into it, and copy `.env.example` into it as `.env`.
3. Fill in `.env` as the section above says. The password in `DATABASE_URL` and the key `TOTP_ENCRYPTION_KEY` are secrets, so they stay only in this file.
4. Start the first versions with the script, for example `./rolling-update.sh --api 0.1.0 --web 0.1.0`. It pulls the images, writes the tags into `.env` and starts the four copies one at a time. On macOS a non interactive SSH session has no `docker` in its path, so start it from a login shell, for example with `zsh -lc`.
5. Create the first admin, as the next section says.
6. Point the reverse proxy at the copies. It sends `/api` to the two backend ports and every other path to the two frontend ports, so the browser sees one hostname. It has to send the next request to the other copy when one copy is down, because that keeps the portal up during an update. It has to set `X-Real-IP` to the address of the client and overwrite any value that the client sent, because the service reads that header for its rate limits when the peer is in `TRUSTED_PROXIES`.

## The first admin

There is no sign up, so the first admin comes from the command `api create-admin`, which the [`README.md`](../README.md) describes. Run it inside a running backend copy, where the settings of the service are already in the environment. Read the password without echo, and pipe it on the standard input so it never appears in a command line.

```sh
read -rs ADMIN_INITIAL_PASSWORD
printf '%s\n' "$ADMIN_INITIAL_PASSWORD" | docker compose exec -T api-1 /api create-admin --email admin@example.org --name "Example Admin"
unset ADMIN_INITIAL_PASSWORD
```

Until the admin sets an own password, every sign in gets only a setup session of 15 minutes.

## Running copies on a second host

A second host adds two more backend copies and two more frontend copies behind the same proxy. The database stays on the first host, so the second host reaches it over the network.

1. Make the folder `~/docker/inventaris` and copy the three files into it as in the first setup. Copy `.env` from the first host straight from host to host, and not through a chat. Change only `DATABASE_URL`, the binds and the ports. Do not create a database or a user again.
2. Keep the same `DATABASE_URL` role, the same `TOTP_ENCRYPTION_KEY` and the same image tags as the first host. Every copy has to read the same key. A copy with another key cannot open a TOTP secret that another copy sealed, and it cannot confirm an enrollment that another copy began.
3. Set the host name of `DATABASE_URL` to an address of the database host that the second host can reach. The database host has to publish the PostgreSQL port on an interface that the second host reaches, and it has to allow only the address of the second host for this role and database. Keep the port closed to every other network.
4. Remove the network `database` from the `networks` list of the `x-api` block and from the `networks` section at the bottom of `compose.yml`. The second host has no database container to join, and compose stops when the external network does not exist.
5. Set `API_BIND` and `WEB_BIND` to the IPv4 address of the network interface that the proxy reaches, and give the four ports. The proxy lists all four backend copies and all four frontend copies as upstreams.
6. Check the peer address that a backend copy sees on the second host, as the section on checking the proxy address says. A proxy on another host may come from a range that is not in `TRUSTED_PROXIES`. Then every client counts as the proxy, and the rate limits are shared by all clients.
7. Start the copies with `rolling-update.sh` as on the first host. A copy that starts while another copy migrates waits for the advisory lock of the migrations for up to 120 seconds, and then finds nothing left to apply.

Four things follow from this layout.

- The URL in the example ends in `sslmode=disable`, so the traffic to the database is not encrypted. Use it only on a private encrypted network. Otherwise turn on TLS in PostgreSQL and use `sslmode=verify-full`. That needs a certificate that names the host in `DATABASE_URL` and a root certificate that the copy trusts. A private root certificate needs `sslrootcert` and a read only mount of the file into the container.
- While the database host is down, no backend copy on any host can serve a request, because all copies share one database. A second host adds capacity for the web and the API processes, and it does not add availability for the data.
- Each copy keeps a pool of `DB_MAX_CONNS` connections. Four backend copies with the default of 8 hold up to 32 connections. Keep the sum for all hosts below `max_connections` of PostgreSQL, with room for the other apps that share it.
- The two hosts run two versions at once while one is updated, so update them one after the other and read the changelog for migrations first. A backend copy that is older than the database refuses to start. An old copy that restarts after a newer copy has migrated stays down until its host is updated.

## How a release becomes a running copy

1. Release Please keeps a release pull request open on `main`. Merging it creates the tag `vX.Y.Z` and the GitHub release.
2. The workflow `Docker` runs for `main` and for the tag. It builds the amd64 and arm64 images of the backend and of the docs site, pushes them to GitHub Container Registry, scans each one with Trivy, attaches a CycloneDX SBOM attestation and signs the image. The tag run adds the version tags.
3. The workflow `Docker Hub mirror` starts when `Docker` ends. It checks the signature and copies each image to Docker Hub.
4. A person runs `rolling-update.sh` on the host with the new version. Nothing deploys by itself.

## Checking the proxy address

A copy counts the address of a client for its rate limits. It trusts the header `X-Real-IP` only when the peer of the request is in `TRUSTED_PROXIES`. Behind published ports the peer is the address of the Docker network gateway or of the proxy, and it depends on the host. To check it, follow these steps.

1. Set `LOG_LEVEL=debug` in `.env` and recreate one copy with `docker compose up -d api-1`. The other copy keeps its old settings until it is recreated. Do not run `rolling-update.sh` for this, because it recreates both copies.
2. Send a request through the proxy, for example `GET /api/config`. The proxy may send it to the other copy, so repeat it until the line appears in the logs of `api-1`.
3. Read the request line of that copy in `docker compose logs api-1`. The field `peer` is the address that the copy sees. The field `client` is the address that was counted, and for an IPv6 client it is the `/64` prefix. The field `client_source` says where it came from.
4. Act on the value of `client_source`.
   - `x-real-ip` means the peer is trusted and the header was used. Nothing needs to change.
   - `peer` means the peer is not in `TRUSTED_PROXIES`. Add the range of `peer` to the list.
   - `peer-trusted-header-missing-or-bad` means the peer is trusted but the header is missing or is not an address. Make the proxy set `X-Real-IP`.
5. Set `LOG_LEVEL` back to `info` and recreate `api-1` with `docker compose up -d api-1`, because the debug line holds client addresses.
6. When you changed `TRUSTED_PROXIES`, recreate `api-2` as well with `docker compose up -d api-2`, because it keeps the old list until then. Send a few requests and check that both copies count the client and not the proxy.

On Docker Desktop for macOS, a request through a published port usually has the address of the gateway of the virtual machine as its peer, and step 3 shows the real value. That address is inside the default private ranges. So anything that can reach the published port directly counts as a trusted proxy and can choose the address that the rate limits count. Keep `API_BIND` at its default of `127.0.0.1`. Set it to a private network address only when the proxy runs on another host, and then limit who can reach that network. A narrow range such as the gateway address alone trusts fewer other private addresses. On Docker Desktop it still does not tell the proxy from a direct caller, so the bind address is the control. It also stops working when the subnet of the virtual machine changes.

## Updating to a new version

Run the update in `tmux` or `screen`, so a dropped connection does not stop it half way.

1. Read the changelog of the release, and the settings that are new or changed. A new version may add migrations.
2. Run the script with a dry run first, for example `./rolling-update.sh --dry-run --api 0.2.0` (the setting `DRY_RUN=1` does the same). It prints the steps and changes nothing.
3. Run it without the dry run. A service that you do not name keeps its copies and its tag.
4. Read the output. Each copy has to report healthy twice in a row before the script goes on to the next one.

The script pulls the images before it changes anything, so a tag that does not exist stops it with nothing changed. It keeps the old `.env` as `.env.bak`, recreates the backend copies first and then the frontend copies, one at a time, and stops at the first copy that does not become healthy. It checks each copy on the address of `API_BIND` or `WEB_BIND` in `.env`, or on the loopback address when the bind is `0.0.0.0`. It does not roll back by itself. It prints which copies run the new version, which still run the old one and the command that goes back.

Going back to an older backend version needs `ROLLBACK_OK` set to that version, because a newer version may have applied a migration that the older one cannot run on. A migration is not undone by going back.

When a compose file or an env file is edited by hand, keep the old one next to it as `compose.yml.bak-<reason>` or `.env.bak`.

## The API docs site

The docs image serves the API docs page at `inventaris-docs.labmgm.org`. The page is built from `openapi.yaml` when the image is built, so a new version of the contract needs a new image and not a change on the host. The image runs nginx as an unprivileged user on port 8080. It answers `/` with the page, `/openapi.yaml` with the raw contract as plain text and `/healthz` with an empty `200` that is not logged. Every other path is a 404. The example for one copy is `deploy/docs/compose.example.yml`. It has a read only root file system, one tmpfs on `/tmp`, no Linux capabilities, `no-new-privileges`, limits for memory and CPU, a log limit and a health check. It has no database, no volume and no secret.

To set up a copy, follow these steps. Every host that serves the page runs its own copy in its own folder, and the reverse proxy sends requests for the hostname to all of them.

1. Make the folder `~/docker/inventaris-docs` on the host. Copy `deploy/docs/compose.example.yml` into it as `compose.yml`, and copy `deploy/docs/.env.example` into it as `.env`.
2. Set `DOCS_PORT` to a port that is free on the host and `DOCS_IMAGE_TAG` to a version such as `1.2.3` or a tag of a commit such as `sha-1a2b3c4`. Both have no default, so compose stops with a message until they are set. `DOCS_BIND` is the loopback address by default, so only a proxy on the same host reaches the copy. A proxy on another host needs the address of the network interface that it reaches. The limits and the log settings have defaults.
3. Start the copy with `docker compose up -d` in that folder, and check that `docker compose ps` reports it healthy.
4. Point the reverse proxy for `inventaris-docs.labmgm.org` at the port. The proxy ends the TLS. The image sends no `Strict-Transport-Security` header, so the proxy is the place to set it.

To update a copy, set `DOCS_IMAGE_TAG` in `.env` to the new version, run `docker compose pull` and then `docker compose up -d`. Compose recreates the container when the image changed, and the page is unavailable on that copy for a few seconds. The page keeps no state and sets no cookie. With a copy on two hosts, update one host, check that it is healthy and then update the other, so the proxy always has a copy to send requests to. Keep the old file next to the new one as `.env.bak` when the file is edited by hand.

The page makes no request to another host. Its `Content-Security-Policy` allows scripts only by hash, and the other headers are in `docs-site/headers.conf`. How the page and the policy are built is in [`ci-cd.md`](ci-cd.md) under "API docs site".

## What is not verified yet

- The layout with a second host and a remote database. The connection over the network, the TLS option and the lock across hosts were not run on a real second host by this documentation.
- The four free ports on the host. The template has no value, and nobody has checked the host.
- The name of the database container, the host name that `DATABASE_URL` needs on the network `DB_NETWORK`, and the name of the database and its user on the host.
- The address that a backend copy sees as the peer of a request that comes through the proxy, which decides the right value of `TRUSTED_PROXIES`. The section on checking the proxy address says how to read it.
- Whether the package of each image on GitHub Container Registry is public. A package starts private, and the host needs a login with a token that can read it until the visibility is changed.
- The arm64 build, the run of `Docker` on `main`, the version tag path, the workflow `Docker Hub mirror` and the manual `Re-sign image`. The amd64 build, the scan, the signature and `Repeatable build` ran on a pull request.
- The docs image. It has not been built or started in CI or on a host yet. Whether nginx starts with the read only root file system and the tmpfs on `/tmp`, whether the log links of the base image exist, the free port, the host name on the proxy and the TLS of that host name have not been checked.
- The frontend side. The compose file assumes that the image listens on 3000 with the route `/healthz`, that it needs no run time setting and that it writes only to `/tmp` and `/app/.next/cache`.
