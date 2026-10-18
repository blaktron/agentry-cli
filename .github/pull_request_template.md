<!-- Pull requests go into `dev`; `main` takes only promotions from `dev`.
One concern per pull request. Conventional-commit subject. -->

## What and why

Closes #

## Verification

<!-- The tests, scripts or live runs you did, and what you did not verify. -->

## Checklist

- [ ] `make check` passes
- [ ] every commit is signed off (DCO, CONTRIBUTING.md)
- [ ] a new capability or grant has a test that an undeclared use is refused
- [ ] a change to a built-in QuickAgent's files updates `builtins.lock`
- [ ] no tokens, keys or secret values in the diff, the tests or the logs
