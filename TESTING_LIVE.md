# Live testing against GitHub

The unit and command tests in this repo run entirely against fakes — nothing in
them touches GitHub. This document is the procedure for exercising `gh cls`
against a **real, disposable** organization, both by hand and via the automated
test in [`live/live_test.go`](live/live_test.go).

You do **not** need a class of students or a second machine. One free GitHub
organization is enough; a single throwaway "student" account unlocks the parts a
sole owner cannot test (see *The freeze caveat* below).

## Prerequisites

1. **A disposable org you own.** Create a free organization (e.g.
   `gh-cls-sandbox`). You must be an *owner*; every command guards on it.
2. **Auth is inherited from `gh` — there is no token to manage.** `gh cls` uses
   your existing `gh auth login`. You only need to ensure that login carries the
   scopes the test needs (the defaults omit `delete_repo`):
   ```
   gh auth refresh -s admin:org -s delete_repo
   ```
   `admin:org` is for org settings and teams; `delete_repo` is for cleanup. Every
   command — including `template` — runs purely against the GitHub API, so no
   `git` binary or credential helper is involved.
3. **A "student" login.** A second throwaway GitHub account is the recommended
   path. For the `freeze` downgrade to be observable it should **accept
   membership** in the sandbox org once (invite it, then accept from that
   account). After that, repo grants to it take effect immediately.

## Install the extension under test

Both runs below invoke `gh cls`, which resolves to whatever `cls` extension is
installed — *not* automatically your working copy. Install the local checkout so
`gh cls` runs the code you are testing rather than the published release:

```
go build                # produces the ./gh-cls binary
gh extension remove cls # only if a published copy is already installed
gh extension install .  # registers this directory as `gh cls`
```

`gh` strips the `gh-` prefix, so the `gh-cls` binary becomes the `gh cls`
command. The install is a symlink to this directory, so a later `go build ./...`
is picked up with no reinstall.

## The freeze caveat (why a second account matters)

`freeze` downgrades every *non-admin* direct collaborator from write to read and
**skips admins** ([`cmd/freeze.go`](cmd/freeze.go)). An organization owner is an
admin on every repo, so if you enroll *yourself* as the student, `freeze`
correctly skips you and reports `0` changes — you can never watch the actual
write→read transition. Only a non-owner collaborator (the throwaway account)
shows the real behavior. Everything else (`setup`, `template`, `assign`) is fully
exercisable with a single account.

## Plan limits on a free org

- **Branch protection** (`assign --branch-protection`) creates a repository
  ruleset. On a free org, rulesets apply to **public** repos only, so pair it
  with `--public`. Private-repo rulesets need GitHub Team/Education.
- **Copilot** has no public toggle; `setup` only *reports* seats (the billing
  endpoint 404s on a free org, which the tool treats as "none present").

## Automated run

The test is double-gated: a `live` build tag keeps it out of `go test ./...`
entirely, and it skips unless `GH_CLS_LIVE_ORG` is set.

```
GH_CLS_LIVE_ORG=gh-cls-sandbox \
GH_CLS_STUDENT1=<throwaway-login> \
GH_CLS_STUDENT2=<optional-second-login> \
  go test -tags live -run TestLive -timeout 20m -v ./live/
```

It runs the full arc in-process against the real API — seed a source template →
`setup` → `template` → `assign` (individual) → `freeze` → `--undo` → a group
`assign` — asserting each step via the API and re-running each command to check
idempotency. It needs no config file on disk: it writes a throwaway one into a
temp directory and points `GH_CLS_CONFIG` at it, so your real config is never
touched. It uses unique per-run repo names and deletes everything it creates in
`t.Cleanup` (the `staff` team is left behind by design — there is no delete-team
primitive and `setup` is idempotent).

If `GH_CLS_STUDENT1` is not an accepted org member, the run still passes but logs
that it skipped the freeze downgrade assertions (the pending-invite collaborator
does not appear in the repo's direct-collaborator list).

Afterward, confirm the org has no `ghclslive*` / `ghclssrc*` repos left — that
verifies cleanup ran.

## Manual run

Useful for eyeballing behavior in the GitHub UI. Work in a scratch directory and
point `GH_CLS_CONFIG` at a throwaway config so your real config is never touched
(its name is arbitrary — the `./gh-cls.yml` default only applies when
`GH_CLS_CONFIG` is unset):

```
export GH_CLS_CONFIG=./gh-cls-test.yml
ORG=gh-cls-sandbox
STU=<throwaway-login>
```

There is **no `gh-cls-test.yml` to copy** — you create it during the walkthrough
(the config is per-course, so the repo ships none; the format is documented in
[README.md](README.md)). `setup` (step 1) creates the file for you with the
`org:` line; you add the `assignments:` entries in step 4 before `assign` needs
them. The `export` above just chooses where that file will live.

Run each step **with `--dry-run` first**, then for real.

1. **setup** — `gh cls setup -o $ORG -s staff`. Re-run; the second run should
   report `already` for each setting. It writes both `org:` and `staff_team:` into
   `gh-cls-test.yml`, so later steps need neither flag. In the UI verify: base
   permission *None*, member repo/Pages creation off, Actions disabled, a `staff`
   team exists.

2. **Seed a source template.** Create a repo with at least one commit to generate
   from — e.g. a new repo initialized with a README named `hw1-src`.

3. **`gh cls template hw1 -o $ORG -t $ORG/hw1-src`** — verify `hw1-template`
   exists, is marked a *template repository*, is private, and has a single commit
   on its default branch. (`template` also marks the *source* `hw1-src` as a
   template repository, which GitHub requires in order to generate from it.)
   Re-run without `-F` → it should error that the template already exists; with
   `-F` → it recreates it.

4. **Fill in the config** so `assign` can resolve the assignment. `setup` already
   created `gh-cls-test.yml` with the `org:` and `staff_team:` lines; open it and
   add the `assignments` entries (`assign` errors with *"assignment not found in
   config"* otherwise):
   ```yaml
   org: gh-cls-sandbox
   staff_team: staff        # already written by setup
   assignments:
     hw1:
       type: individual
       template: gh-cls-sandbox/hw1-src
   ```
   Also create a `roster.csv` (kept out of git by `.gitignore`):
   ```csv
   identifier,username
   <STU>,<STU>
   ```

5. **`gh cls assign hw1 -o $ORG -s staff -r roster.csv --public --branch-protection --feedback issue`**
   — verify `hw1-<STU>` is created, `<STU>` is a direct collaborator with **push**,
   the staff team has push, a protection ruleset is present (public repo), and a
   *Feedback* issue is open. Re-run → it should report the repo `skipped`.

6. **`gh cls freeze hw1 -o $ORG`** — `<STU>` drops to **read**. (Single-account
   fallback: reports `0` changed because you are admin-skipped.)

7. **`gh cls freeze hw1 -o $ORG --undo`** — push restored. Re-run → `0` changes.

8. **Group assignment (optional)** — add a `group` assignment to the config,
   write a `teams.yml` (`alpha: [<STU>]`), and
   `gh cls assign <name> -o $ORG -s staff -r roster.csv --teams teams.yml --public`.
   Verify `<name>-alpha` is created with the team's members granted push.

9. **Cleanup** — delete `hw1-template`, every `hw1-*`, `hw1-src`, and any group
   repos. (Leaving the `staff` team is fine.)
