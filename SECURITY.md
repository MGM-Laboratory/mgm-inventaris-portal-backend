# Security Policy

## Supported scope

`MGM-Laboratory/mgm-inventaris-portal-backend` is the backend of the LabMGM inventory portal, written in Go with PostgreSQL. It has no service code yet, so there is no running service to attack today. A report about the repository itself, such as a workflow that leaks a secret or a dependency with a known flaw, is in scope. A report about the service is in scope from the first release.

## Reporting a vulnerability

If you find a security issue, such as a way to read or change data that you should not reach, an injection vector, or anything that could harm a future production deployment, please **do not open a public issue**.

Instead, email **hi@labmgm.org**, or use "Report a vulnerability" in the Security tab of the repository, with a description of the issue and its impact, the steps to reproduce it, and any relevant logs or screenshots.

You can expect an initial response within 7 days. We keep you updated while the issue is triaged and fixed, and we credit reporters who want it once a fix has shipped.

## How we respond

1. The maintainer reads the report and answers the reporter within 7 days.
2. The maintainer reproduces the problem, rates its impact, and tells the reporter the result.
3. The maintainer makes the fix in a private fork of a GitHub security advisory, or in a normal pull request when the change reveals little. It passes the same checks as any other change.
4. The fix ships in a release. The release notes say that a vulnerability was fixed, without detail that helps an attacker before the users can update.
5. The maintainer asks the reporter whether they want credit, and names them in the release notes after the fix has shipped if they do.

## Scope notes

- Automated scanning that could degrade the availability of a live deployment is not authorized. Run the code locally instead.
- Do not test with the data of real people. Use sample values.

## How this repository is checked

Findings from the automated tooling in this repository (secret scanning, dependency review, Trivy and OSSF Scorecard) are triaged the same way as external reports. The default branch needs a pull request and signed commits. A pull request also needs the approval of a code owner, and the maintainer team can bypass that approval, so the single owner can merge their own pull request after the checks are green. Details of the checks are in `docs/ci-cd.md`.
