# Contributing to the LabMGM inventory portal backend

Thanks for taking the time to contribute. This repository is the backend of the LabMGM inventory portal, written in Go with PostgreSQL. The service is at its first skeleton, so read `README.md` for what exists today.

## Before you change code

- Keep one pull request to one concern. A discrete change is easier to review than a bundle.
- Add tests with new logic. A bug fix comes with a test that fails without it. When a change cannot be tested, say why in the pull request.
- A change to a route updates `openapi.yaml` in the same pull request.
- A test that depends on how PostgreSQL or its driver really behaves must run against a real database and not against a mock.
- Heavy work runs in CI. Push the branch and read the results there. Run a single narrow check locally only when CI cannot explain a failure.
- Do not commit a secret, a token or a real `.env` file. Only `.env.example` files are tracked.

## Workflow

There is no `dev` branch. Every change to `main` goes through a pull request.

1. Fork the repository, or branch directly if you are a collaborator.
2. Make your change.
3. Open a pull request against `main`. Keep it focused.
4. Wait for the checks below. The required ones have to be green, every review thread has to be resolved and the code owner has to approve the pull request. The maintainer team may bypass that approval, so a member of the team can merge a pull request after the required checks are green.
5. Only a member of the maintainer team merges, with a merge commit and never a squash or a rebase. Nobody can push to `main` directly, and every commit has to be signed.

## Checks

Every pull request runs the checks below. The ruleset of the repository requires **Conventional commits**, **PR format**, **Config gate**, **Security gate** and **CI gate**. `docs/ci-cd.md` lists the current state.

- **Conventional commits** reads every commit message and the title of the pull request.
- **PR format** reads the branch name, the label, the assignee and the body.
- **Config gate** runs actionlint, Zizmor, ShellCheck, dotenv-linter for `.env.example`, ls-lint for file names, the Renovate validator and the Redocly lint of `openapi.yaml`.
- **CI gate** summarizes the Go jobs, which are `Go build and vet`, `Go lint` and `Go test`.
- **Security gate** summarizes CodeQL for Go, dependency review, gitleaks and a Trivy filesystem scan. Dependency review fails on a finding of severity high or above, and gitleaks fails on any finding. Trivy fails on a CRITICAL or HIGH finding and also sends its findings to code scanning. For a pull request from a fork, the Trivy findings are not uploaded to code scanning, because a fork token cannot write there. They are kept for 7 days as the artifact `trivy-fs-results` of the run, and the check still fails on a CRITICAL or HIGH finding.
- **pre-commit.ci** is not required by the ruleset, but its report should be clean. It runs repository hygiene checks on every pull request and does not push a fix.

A commit message or a title that does not follow Conventional Commits blocks the merge. To revert a change, write the title as `revert: <summary>`, because the default title that the GitHub revert button makes is rejected. A lint warning or a deprecation notice in the output of a check is still something to fix before the pull request is done, even when it does not turn the check red.

## Go checks

The workflow `CI` runs the Go checks on every pull request, and `CodeQL` runs in the workflow `Security`.

- `Go build and vet` runs `go build ./...` and `go vet ./...`.
- `Go lint` checks `gofmt`, checks that `go.mod` and `go.sum` are tidy and runs golangci-lint with `.golangci.yml`.
- `Go test` runs `go test ./... -race` against a PostgreSQL 17 service and sends the coverage to Codecov.

The database tests read `DATABASE_URL`. They make a database of their own on that server and drop it at the end, so point it at a throwaway server only. Without `DATABASE_URL` they skip, and in CI a missing value fails them, so a run can never pass with every database test skipped. Name Go files in lowercase with no separator, such as `errormap.go`, because ls-lint refuses an underscore there. Only `_test` may follow the name. A migration is a pair of files named `NNNN_title.up.sql` and `NNNN_title.down.sql`, and a change to the schema updates `docs/data-model.md` in the same pull request.

Run the checks through CI. When a failure needs a local run to explain it, use `go test ./path/to/package` for a single package and not the whole suite.

## Commit messages

Use Conventional Commits (`type(scope): summary`) in English, with one of feat, fix, docs, style, refactor, perf, test, build, ci, chore or revert. Sign every commit so that GitHub shows it as verified.

## Pull requests

- Name the branch `type/short-description`, for example `fix/loan-lookup`. The type is the same as the type of the commits.
- Write the title as a Conventional Commit, for example `fix(loans): reject a loan with no items`.
- The body is only the list of commits, one bullet for each commit. A merge commit of `main` into your branch does not need a bullet.
- Add one label, `bug` for a fix, `enhancement` for a feature and `documentation` for docs only, and assign the pull request to yourself. A contributor without write access cannot assign it, so ask a maintainer to do it.
- The check Conventional commits reads every commit message and the title. The check PR format reads the branch, the label, the assignee and the body. A bad commit message needs a rebase that rewords it and a force push to your own branch. A missing label or assignee is added by a maintainer, because it needs write access, and the maintainer then reruns the failed run, because a later passing run does not replace it. After a push or a rebase, update the body so that the bullets match the commits.
- The maintainer merges with a merge commit, never a squash or a rebase.
