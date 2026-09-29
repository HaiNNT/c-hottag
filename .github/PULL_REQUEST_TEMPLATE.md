<!--
c-hottag takes pull requests by invitation only:
https://github.com/HaiNNT/c-hottag/blob/main/CONTRIBUTING.md
A pull request from anyone without write access to this repository is
closed automatically. If you were not invited, please open an issue instead:
https://github.com/HaiNNT/c-hottag/issues/new/choose
-->

## What and why

<!-- The change, and the issue it comes from (Fixes #123). -->

## Checklist

- [ ] The maintainer invited this pull request.
- [ ] The gate passes locally (the four lines in CONTRIBUTING.md).
- [ ] New behaviour has a test, and tests take `HOME` and `CHOTTAG_HOME` from `t.TempDir()`.
- [ ] It holds no personal data or tokens: fixtures use `alice@example.com`, `Acme` and `/Users/alice`.
- [ ] A user-visible change updates the docs and adds a line to the next notes in `docs/release-notes/`.
- [ ] If it touches the proxy, the local CA or token handling, the description above says how.
