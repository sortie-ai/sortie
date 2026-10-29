# Releasing

_Requires write access to `sortie-ai/sortie` and `sortie-ai/docs`, approver rights on the SignPath signing policy, write access to the Sortie Roadmap board and permission to post announcements in Discord._

1. In `CHANGELOG.md`, add a `[X.Y.Z] - YYYY-MM-DD` heading below `[Unreleased]`, add its compare link to the footer, and point the `[Unreleased]` link at `vX.Y.Z...HEAD`.
2. Shift the Supported Versions table in `SECURITY.md`.
3. Run `make price-table`, or merge the open price-table refresh PR.
4. Check that `README.md` matches the release.
5. Commit to `main` and wait for CI to pass.
6. Run the `Release` workflow with version `X.Y.Z` and `dry_run` checked.
7. Copy the `[X.Y.Z]` section of `CHANGELOG.md` into `sortie-ai/docs/changelog.md` with a `{ #X.Y.Z }` heading anchor, and add its compare link to the footer.
8. Draft the release announcement and add it to `sortie-ai/docs`.
9. Run the `Release` workflow with `dry_run` unchecked.
10. Approve the signing request in SignPath within an hour, while the workflow waits on it.
11. Commit and push `sortie-ai/docs`, then deploy the docs site.
12. Post the announcement in Discord.
13. Close the `X.Y.Z` milestone.
14. Set the next milestone as the default on the Sortie Roadmap board.
