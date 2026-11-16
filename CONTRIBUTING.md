# Contributing to the LabMGM inventory portal backend

Thanks for taking the time to contribute. This repository is the backend of the LabMGM inventory portal, written in Go with PostgreSQL. It has no service code yet. Read `README.md` for what exists today.

## Before you change code

- Keep one pull request to one concern. A discrete change is easier to review than a bundle.
- Add tests with new logic. A bug fix comes with a test that fails without it. When a change cannot be tested, say why in the pull request.
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

Every pull request runs the checks below. The ruleset of the repository requires only **Conventional commits** today. The other checks join the ruleset after each of them has run once, and `docs/ci-cd.md` lists the current state.

- **Conventional commits** reads every commit message and the title of the pull request.
- **PR format** reads the branch name, the label, the assignee and the body.
- **Config gate** runs actionlint, Zizmor, ShellCheck, ls-lint for file names and the Renovate validator.
- **Security gate** summarizes dependency review, gitleaks and a Trivy filesystem scan. Dependency review fails on a finding of severity high or above, and gitleaks fails on any finding. Trivy fails on a CRITICAL or HIGH finding and also sends its findings to code scanning. For a pull request from a fork, the Trivy findings are not uploaded to code scanning, because a fork token cannot write there. They are kept for 7 days as the artifact `trivy-fs-results` of the run, and the check still fails on a CRITICAL or HIGH finding.
- **pre-commit.ci** is not required by the ruleset, but its report should be clean. It runs repository hygiene checks on every pull request and does not push a fix.

A commit message or a title that does not follow Conventional Commits blocks the merge. To revert a change, write the title as `revert: <summary>`, because the default title that the GitHub revert button makes is rejected. A lint warning or a deprecation notice in the output of a check is still something to fix before the pull request is done, even when it does not turn the check red.

## Go checks

The Go code is not written yet, so no Go check runs today. The Go checks join CI in the same pull request that adds the Go code, and this page is updated then. Run them through CI. When a failure needs a local run to explain it, use `go test ./path/to/package` for a single package and not the whole suite.

## Commit messages

Use Conventional Commits (`type(scope): summary`) in English, with one of feat, fix, docs, style, refactor, perf, test, build, ci, chore or revert. Sign every commit so that GitHub shows it as verified.

## Pull requests

- Name the branch `type/short-description`, for example `fix/loan-lookup`. The type is the same as the type of the commits.
- Write the title as a Conventional Commit, for example `fix(loans): reject a loan with no items`.
- The body is only the list of commits, one bullet for each commit. A merge commit of `main` into your branch does not need a bullet.
- Add one label, `bug` for a fix, `enhancement` for a feature and `documentation` for docs only, and assign the pull request to yourself. A contributor without write access cannot assign it, so ask a maintainer to do it and then rerun the failed check.
- The check Conventional commits reads every commit message and the title. The check PR format reads the branch, the label, the assignee and the body. A bad commit message needs a rebase that rewords it and a force push to your own branch. A missing label or assignee is added by a maintainer, because it needs write access, and the check runs again by itself. After a push or a rebase, update the body so that the bullets match the commits.
- The maintainer merges with a merge commit, never a squash or a rebase.
