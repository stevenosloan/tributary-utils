---
name: release
description: Version and tag the utils module with semantic versioning. Use whenever a change to utils (any package, exported API, behavior, or go.mod) is merged or about to be, when the user says "release", "tag", "bump the version", or asks what version a change should be.
---

# Releasing utils

The version of this module **is its git tag** (`vMAJOR.MINOR.PATCH`). Consumers
pin it in their `go.mod`. Every change that reaches `main` and that a
consumer could pick up needs a new tag; never leave `main` ahead of the latest
tag for long.

## 1. Find the current version and what changed

```sh
git fetch --tags
last=$(git describe --tags --abbrev=0 --match 'v*')
git log --oneline "$last"..main
git diff --stat "$last"..main
```

If there are no commits since `$last`, stop: nothing to release.

## 2. Choose the bump

While on `v0.x`, `0` is treated as the major version: `v0.MINOR.PATCH` works
like `MAJOR.MINOR` in semver, so a new feature takes a minor. This differs
from the common `v0` convention (breaking changes take a minor, additions a
patch), which this module used through `v0.2.0`.

| Change since last tag | Bump while `v0.x` | At `v1.0.0`+ |
| --- | --- | --- |
| New exported API or package, backward-compatible | **minor** (`0.3.0` -> `0.4.0`) | minor |
| Bug fix, docs, CI, internal refactor, no API change | **patch** (`0.3.0` -> `0.3.1`) | patch |
| Breaking: removed/renamed/re-typed exported identifier, changed behavior callers rely on, new required arg, raised `go` directive | **minor**, and say it's breaking in the tag message | major |

A breaking change has no bump of its own below `1.0`, so the tag message is
what tells consumers. Start it with `BREAKING:` and name what changed, for
example `v0.4.0: BREAKING: <identifier> <what changed>`.

Rules of thumb:
- Look at the **exported API diff**, not the commit messages. Check with
  `git diff "$last"..main -- '*.go'` for changed exported signatures.
- Doc-only/CI-only changes may skip a release; say so rather than tagging.
- Mixed changes take the highest bump.
- Moving to `v1.0.0` is a deliberate decision for the user. Never do it
  unprompted. `v2+` would also need a `/v2` module path; flag it, don't do it.
- Propose the version and the reasoning to the user and get confirmation
  before tagging.

## 3. Tag (only after confirmation)

Tag the commit on `main` (after the PR is merged), not a feature branch.

```sh
git switch main && git pull --ff-only
GOWORK=off go vet ./... && GOWORK=off go test ./...
test -z "$(gofmt -l .)"
git tag -a vX.Y.Z -m "vX.Y.Z: <one-line summary>"
```

Pushing a tag is public and effectively permanent (the Go module proxy caches
it; a retagged version breaks checksums). **Ask before** `git push origin vX.Y.Z`,
and never move or delete a pushed tag; ship a new patch instead.
