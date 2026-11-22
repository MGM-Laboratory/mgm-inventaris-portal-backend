# Deployment

The portal runs from two Docker images on one host. The backend image comes from this repository and the frontend image comes from [`mgm-inventaris-portal-frontend`](https://github.com/MGM-Laboratory/mgm-inventaris-portal-frontend). CI builds, scans, signs and publishes the backend image, as [`ci-cd.md`](ci-cd.md) explains. This page covers how to run both images on a host and how to update them without downtime.

## The images

| Image | Source | Registry name | Port in the container |
| --- | --- | --- | --- |
| Backend | The `Dockerfile` in the root of this repository | `ghcr.io/mgm-laboratory/mgm-inventaris-portal-backend` | 8080 |
| Frontend | The frontend repository | `ghcr.io/mgm-laboratory/mgm-inventaris-portal-frontend` | 3000 |

The backend image holds one static Go binary at `/api` on a distroless base, and it runs as the numeric user 65532. It has no shell and no package manager. The image has the tags `latest` and `sha-<commit>` for `main`, `pr-<number>` for a pull request of this repository, and `1.2.3` for a release. A release also has the tag `1.2`, and a release of version 1.0.0 or later also has the tag `1`. The signed copy on Docker Hub is `<namespace>/mgm-inventaris-portal-backend`.

## The files of `deploy/`

- `compose.production.example.yml` runs two copies of the backend and two copies of the frontend, named `api-1`, `api-2`, `web-1` and `web-2`.
- `rolling-update.sh` updates the copies one at a time.
- `.env.example` lists the settings of the compose file and of the backend.

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

`ENVIRONMENT=production` and `SECURE_COOKIE=true` stay as they are. Production refuses an `http` `PUBLIC_BASE_URL` and a `SECURE_COOKIE` of `false`. `MIGRATE_ON_START=true` makes each copy apply the embedded migrations at start under an advisory lock. With two copies, one applies them while the other waits and then finds nothing to do. A copy that cannot migrate never becomes healthy, so `rolling-update.sh` stops at it.

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

## How a release becomes a running copy

1. Release Please keeps a release pull request open on `main`. Merging it creates the tag `vX.Y.Z` and the GitHub release.
2. The workflow `Docker` runs for `main` and for the tag. It builds the amd64 and arm64 images, pushes them to GitHub Container Registry, scans each one with Trivy, attaches a CycloneDX SBOM attestation and signs the image. The tag run adds the version tags.
3. The workflow `Docker Hub mirror` starts when `Docker` ends. It checks the signature and copies the image to Docker Hub.
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

## Updating to a new version

Run the update in `tmux` or `screen`, so a dropped connection does not stop it half way.

1. Read the changelog of the release, and the settings that are new or changed. A new version may add migrations.
2. Run the script with a dry run first, for example `./rolling-update.sh --dry-run --api 0.2.0` (the setting `DRY_RUN=1` does the same). It prints the steps and changes nothing.
3. Run it without the dry run. A service that you do not name keeps its copies and its tag.
4. Read the output. Each copy has to report healthy twice in a row before the script goes on to the next one.

The script pulls the images before it changes anything, so a tag that does not exist stops it with nothing changed. It keeps the old `.env` as `.env.bak`, recreates the backend copies first and then the frontend copies, one at a time, and stops at the first copy that does not become healthy. It checks each copy on the address of `API_BIND` or `WEB_BIND` in `.env`, or on the loopback address when the bind is `0.0.0.0`. It does not roll back by itself. It prints which copies run the new version, which still run the old one and the command that goes back.

Going back to an older backend version needs `ROLLBACK_OK` set to that version, because a newer version may have applied a migration that the older one cannot run on. A migration is not undone by going back.

When a compose file or an env file is edited by hand, keep the old one next to it as `compose.yml.bak-<reason>` or `.env.bak`.

## What is not verified yet

- The four free ports on the host. The template has no value, and nobody has checked the host.
- The name of the database container, the host name that `DATABASE_URL` needs on the network `DB_NETWORK`, and the name of the database and its user on the host.
- The address that a backend copy sees as the peer of a request that comes through the proxy, which decides the right value of `TRUSTED_PROXIES`. The section on checking the proxy address says how to read it.
- Whether the package of each image on GitHub Container Registry is public. A package starts private, and the host needs a login with a token that can read it until the visibility is changed.
- The arm64 build, the run of `Docker` on `main`, the version tag path, the workflow `Docker Hub mirror` and the manual `Re-sign image`. The amd64 build, the scan, the signature and `Repeatable build` ran on a pull request.
- The frontend side. The compose file assumes that the image listens on 3000 with the route `/healthz`, that it needs no run time setting and that it writes only to `/tmp` and `/app/.next/cache`.
