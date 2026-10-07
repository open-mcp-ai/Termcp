# Release preparation

Ordinary development PRs add user-facing changes under `## Unreleased` in
`CHANGELOG.md`. `server.json` keeps the latest released version.

Before tagging a new version, open a release PR from a branch named
`release/vX.Y.Z` or title it `release: vX.Y.Z`. The name/title is the signal
that makes the PR check require that exact version. Without a release signal,
CI cannot distinguish an ordinary PR from a forgotten release PR.

1. Run `make prepare-release RELEASE_VERSION=vX.Y.Z`. This moves the current
   Unreleased notes into a dated version section and updates both the version
   and OCI identifier in `server.json`.
2. Review and edit the release notes. Run
   `make check-release RELEASE_VERSION=vX.Y.Z` locally.
3. Merge only after the `Test` workflow passes. In GitHub branch protection,
   require `test (ubuntu-latest)`, `test (macos-latest)`, and
   `test (windows-latest)` on both `main` and `dev`. A workflow that merely
   runs does not prevent merging.
4. After the release PR is merged, tag that merged commit with `vX.Y.Z`.
   Committing and pushing still require the human approval described in
   `AGENTS.md`.

Both tag-triggered publishing workflows check the committed files against the
tag before building or publishing. They no longer rewrite `server.json` inside
the runner, which previously hid stale metadata in the repository.
