---
name: release
description: Cut an OpenRiak operator + Helm chart release — bump operator/chart versions and every reference, add release notes, merge via PR, then tag v<operator> and chart-v<chart>. Use whenever the user asks to cut, ship, publish or tag a release, or to bump the operator/chart version.
---

# Cutting an OpenRiak release

Two independent version numbers, released together:

| What | Version source | Tag | Published by |
|------|---------------|-----|--------------|
| Operator image | `Chart.yaml` `appVersion` (e.g. `0.0.9`) | `v0.0.9` | `build-operator.yml` → `ghcr.io/marthydavid/openriak-operator:0.0.9` (+ `latest`) |
| Helm chart | `Chart.yaml` `version` (e.g. `0.1.7`) | `chart-v0.1.7` | `release-chart.yml` → `oci://ghcr.io/marthydavid/charts` (fails if tag ≠ `Chart.yaml` version) |

Riak operand images (`riak-*` tags, `images/riak/**`) are a separate pipeline; do not touch them for an operator release.

Do the whole thing yourself, end to end; only stop for the user on the cases under "When to stop".

## 1. Decide the versions

- `git fetch --tags origin`; `git tag --sort=-v:refname | head` for the latest `v*` and `chart-v*`.
- Operator: patch bump by default (`0.0.8` → `0.0.9`). Chart: bump its own minor/patch (`0.1.6` → `0.1.7`). Use what the user named if they gave versions.
- Refuse to reuse a tag that already exists locally or on origin.

## 2. Work from an up-to-date base

- Releases are cut from `main`, never tagged from a feature branch.
- Branch for the release: `release/v<operator>` from `origin/main`. If the fix being released lives on an unmerged branch/PR, get it merged first (or include it via its PR); check `gh pr list` and `git log origin/main..<branch>`.
- Do not rewrite history of shared branches (no rebase+force-push without the user's say-so). Prefer a new branch from `origin/main` plus merge/cherry-pick, or merging the PR.

## 3. Bump every reference

Find every occurrence of the previous versions rather than trusting a list:

```bash
grep -rIn "<old-operator>\|<old-chart>" . --exclude-dir=.git --exclude=go.sum
```

Known places (the previous release commit touched all of these): `charts/openriak-operator/Chart.yaml` (`version`, `appVersion`), `README.md`, `docs/examples.md`, `docs/getting-started/kubernetes.md`, `examples/*.yaml`, any `make deploy IMG=…:<ver>` snippet. Do not edit old entries in `docs/release-notes.md` or `go.sum`.

## 4. Release notes

Add a `## Operator <op> / chart <chart>` section at the **top** of `docs/release-notes.md` (newest first), matching the existing tone: one-sentence summary, `### Fixes`, `### Changes you may notice`. Take content from `git log <last-tag>..HEAD` and merged PRs; describe user-visible behaviour, link the issue/PR, say whether it needs a CRD/chart upgrade. Make sure `mkdocs.yml` nav lists release notes (it does).

## 5. Verify, then PR

```bash
go build ./... && go test ./internal/... -timeout 180s
GOTOOLCHAIN=go1.22.12 golangci-lint run
helm lint charts/openriak-operator   # if helm is available
```

Commit as `release: operator v<op> / chart <chart>` (with the required Co-Authored-By trailer), push the branch, open a PR to `main`. Read CI with `gh pr checks` / the ci-workflow-guardian agent and fix failures; do not poll with sleep loops.

## 6. Merge and tag

After CI is green and the PR is merged (merge it yourself only if the user asked for the release to be cut, which they did by invoking this):

```bash
git checkout main && git pull --ff-only
git tag v<op>          && git push origin v<op>
git tag chart-v<chart> && git push origin chart-v<chart>
```

Tag the merge commit on `main`. Push each tag individually, never `--tags`.

## 7. Confirm it shipped

- `gh run list --limit 6`: `Build Operator Image` for `v<op>` and `Helm Chart` for `chart-v<chart>` must succeed; fix and re-run on failure (never move a published tag without asking).
- Optionally `gh release create v<op> --generate-notes` if the repo uses GitHub Releases (check `gh release list` first and follow the existing pattern).
- Report: versions, PR link, tag links, image/chart locations.

## When to stop and ask

- The requested fix is not merged and merging it needs a history rewrite or force-push.
- A tag already exists, or CI fails for a reason outside the release.
- The user's "don't interfere with release X" constraint: never touch tag X or its branch; releases go forward only.
