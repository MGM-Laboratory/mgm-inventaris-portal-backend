# LabMGM inventory portal, backend

The backend of the LabMGM inventory portal, written in Go with PostgreSQL. The frontend lives in [`mgm-inventaris-portal-frontend`](https://github.com/MGM-Laboratory/mgm-inventaris-portal-frontend).

[![Security](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/actions/workflows/security.yaml/badge.svg)](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/actions/workflows/security.yaml)
[![Config lint](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/actions/workflows/config.yaml/badge.svg)](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/actions/workflows/config.yaml)
[![Release Please](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/actions/workflows/release-please.yaml/badge.svg)](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/actions/workflows/release-please.yaml)

[![License](https://img.shields.io/github/license/MGM-Laboratory/mgm-inventaris-portal-backend)](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/blob/main/LICENSE)
[![Go](https://img.shields.io/badge/Go-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-4169E1?logo=postgresql&logoColor=white)](https://www.postgresql.org)

[![Last commit](https://img.shields.io/github/last-commit/MGM-Laboratory/mgm-inventaris-portal-backend)](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commits/main)
[![Conventional Commits](https://img.shields.io/badge/Conventional%20Commits-1.0.0-FE5196?logo=conventionalcommits&logoColor=white)](https://www.conventionalcommits.org)

## Stack

- Go and PostgreSQL.
- CI, release automation and dependency updates on GitHub Actions, Release Please and Renovate. See [`docs/ci-cd.md`](docs/ci-cd.md).

## Quick start

The service code is not written yet, so there is nothing to build or run. This section is filled in together with the first Go code.

## Configuration

The service code is not written yet, so it reads no settings. This section gets a table of every setting, with its default and its purpose, together with the first Go code.

## Documentation

- [`docs/ci-cd.md`](docs/ci-cd.md) lists the workflows, the required checks and the release flow.
- [`CONTRIBUTING.md`](CONTRIBUTING.md) and [`SECURITY.md`](SECURITY.md) hold the rules for contributions and for reporting a vulnerability.

## Project layout

```text
docs/             CI/CD documentation
.github/          Workflows, the lint tool images, issue forms and the pull request template
```

## Contributing and security

Read [`CONTRIBUTING.md`](CONTRIBUTING.md) before you open a pull request. Report a security issue privately as [`SECURITY.md`](SECURITY.md) says, and do not open a public issue. The rules of behavior are in [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md).

## License

GNU General Public License v3.0. See [`LICENSE`](LICENSE).
