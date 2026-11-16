# CI/CD

The workflows below run on pull requests and on merges to `main` of `github.com/MGM-Laboratory/mgm-inventaris-portal-backend`, as the Trigger column of each one says. `main` is protected. Every change goes through a branch, a pull request and the checks, and it is merged with a merge commit, never squash or rebase.

The service code is not written yet. There is no Go build, no test run, no Dockerfile and no image build. Those parts join when the Go code does.

## GitHub Actions workflows

| Workflow            | Triggers                                                | What it does                                                                                                                                                                                                                                                                                                                        |
| ------------------- | ------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Config lint         | Pull request, push to `main`, manual                    | Actionlint, Zizmor, ShellCheck, ls-lint for file names and the Renovate validator. The job `Config gate` summarizes the run.                                                                                                                                                                                                        |
| Security            | Pull request, push to `main`, Mondays 03:00 UTC, manual | Dependency review (pull requests only), gitleaks and a Trivy filesystem scan. Dependency review fails on a finding of severity high or above, and gitleaks fails on any finding. Trivy fails on a CRITICAL or HIGH finding and also sends its findings to code scanning. For a pull request from a fork, the Trivy findings are not uploaded to code scanning, because a fork token cannot write there. They are kept for 7 days as the artifact `trivy-fs-results` of the run, and the check still fails on a CRITICAL or HIGH finding. OSSF Scorecard runs on the schedule and by hand. The job `Security gate` summarizes the run. CodeQL joins when the Go code does. |
| Commit messages     | Pull request                                            | Runs commitlint on every commit and checks that the title follows Conventional Commits. The job is named `Conventional commits`.                                                                                                                                                                                                    |
| Pull request format | Pull request                                            | Checks the branch name, the label, the assignee and that the body holds one bullet for each commit. It runs again when a label or an assignee changes. Removing the last label or the last assignee fails the check, and a rerun of the failed run clears it after the label or the assignee is back. The job is named `PR format`.                                                                                                                                                                                                                 |
| Release Please      | Push to `main`, manual                                  | Opens and updates the release pull request, and creates the tag `vX.Y.Z` and the GitHub release when that pull request is merged. The job `Release PR and tag` builds a token from the release GitHub App `syafa-bot`.                                                                                                              |

The lint tools that run as Docker images (actionlint, Zizmor, ShellCheck and gitleaks) are pinned by digest in `.github/tools/Dockerfile`, which nothing builds. Renovate reads that file, so a bump there is the bump the jobs use. The action `.github/actions/pull-images` sends image pulls through `mirror.gcr.io` and logs in to Docker Hub only when the secret `DOCKERHUB_PULL_TOKEN` exists. The username comes from the variable `DOCKERHUB_USERNAME`.

## Required checks

The ruleset `main` on the default branch requires a pull request with a merge commit, resolved review threads, signed commits, up to date branches, and no deletion or force push. It has no bypass actor. It requires four status checks, each pinned to GitHub Actions.

- Conventional commits
- PR format
- Config gate
- Security gate

A check joins the ruleset after it has run once. The pre-commit.ci report and the code scanning results are not required.

A second ruleset, `main-merge-and-approval`, requires one approving review and the approval of a code owner, and it allows only merge commits. It also has the rule `update`, so only the bypass team can merge into `main`. `.github/CODEOWNERS` names the owner. The team `mgm-inventaris-portal-maintainer` can bypass this second ruleset through a pull request, and it cannot bypass the first one. The bypass covers only the approval, the code owner review and the `update` rule. Signed commits, resolved threads, up to date branches and the required checks still apply to the team. A pull request from someone outside the team needs the approval of the owner. A pull request that changes only `go.mod`, `go.sum`, `CHANGELOG.md` or `.release-please-manifest.json` has no code owner, so it needs any one approval.

## Releases

Release Please reads the Conventional Commits on `main` and keeps one release pull request open. Merging it writes `CHANGELOG.md`, creates the tag and publishes the GitHub release. `.release-please-manifest.json` holds the current version, and `release-please-config.json` starts the changelog at the commit named in `bootstrap-sha`.

The private key of the release GitHub App is the secret `RELEASE_APP_PRIVATE_KEY` of the environment `release`, which only `main` can use. The client id is the variable `RELEASE_APP_CLIENT_ID`. Two tag rulesets protect the tags `v*`. `release-tags` blocks deletion, force pushes and updates for everyone, and `release-tags-creation` lets only the release App create a tag.

The release pull request is opened by the App, so the owner can approve it, or a member of the bypass team can merge it like any other pull request. Its title is `chore(main): release X.Y.Z`. Commits of the types `ci`, `docs`, `chore` and `build` do not start a release, and the first `fix` or `feat` after the commit in `bootstrap-sha` does. The first release is 0.1.0, because of `initial-version`. While the version is below 1.0.0, a breaking change raises the minor version and not the major one, because of `bump-minor-pre-major`. The release to 1.0.0 is a decision of the owner.

## pre-commit.ci

The pre-commit.ci GitHub app runs the hooks of `.pre-commit-config.yaml` on every pull request. They check for large files, case conflicts, broken JSON, TOML and YAML, merge conflict markers, symbolic links that do not resolve, private keys, a final newline, mixed line endings and trailing whitespace. The run is not a required check. `autofix_prs` is off, because the ruleset only accepts signed commits and the app would push an unsigned fix. A failing run needs a fix by hand. The app has no setting to turn its update pull requests off, so `autoupdate_schedule` is `quarterly`, the longest interval that it allows. Its update pull request carries unsigned commits and fails the checks `Conventional commits` and `PR format`, so close it. Renovate moves the hook revisions through its `pre-commit` manager, with the commit type `ci`.

## Naming rules

`.ls-lint.yml` sets the naming of files and folders. Folders are lowercase words joined by hyphens. Files are kebab case, and Markdown files may also use capitals such as `README.md`. A Go file is lowercase words with no separator, and only `_test` may follow them. Workflows end with `.yaml` and every other YAML file ends with `.yml`, also when the name has more dots such as `compose.production.yml`, except `.pre-commit-config.yaml`, which pre-commit reads by that name. ls-lint does not merge the rules of a folder with the rules above it, so `.ls-lint.yml` repeats the shared rules in every folder entry through a merge key.

## Dependency updates

Renovate opens the pull requests through the Mend Renovate GitHub app, which an owner of the organization has to install on the repository. `renovate.json` sets a weekly run on Monday before 07:00 Asia/Jakarta, a minimum release age of 7 days, grouped minor and patch updates for Go modules and GitHub Actions, and `go mod tidy` after a Go module update. A major update of a tool version in a workflow waits for approval on the dependency dashboard. Dependabot alerts are on and Dependabot security updates are off, so only Renovate opens version pull requests.

## Not set up yet

- The Go build, tests with coverage and Codecov, golangci-lint and CodeQL. They join with the Go code.
- A Dockerfile and the image build, scan and signing.
