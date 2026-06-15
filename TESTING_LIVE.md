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
(the name is arbitrary; every command needs `-c` or `GH_CLS_CONFIG` to find it):

```
export GH_CLS_CONFIG=./gh-cls-test.yml
ORG=gh-cls-sandbox
STU=<throwaway-login>
```

There is **no `gh-cls-test.yml` to copy** — you author it (the config is
per-course; the format is documented in [README.md](README.md)). The tool only
reads it, never writes it. Create it now with the org and staff team (you add the
`assignments:` entry before step 5); the `export` above points every command at
it, or pass `-c gh-cls-test.yml` to each:

```yaml
org: gh-cls-sandbox
staff_team: staff
```

Run each step **with `--dry-run` first**, then for real.

1. **setup** — `gh cls setup`. It reads the org and staff team from the config.
   Re-run; the second run should report `already` for each setting. In the UI
   verify: base permission *None*, member repo/Pages creation off, Actions
   disabled, a `staff` team exists.

   **1b. staff (optional)** — write a `tas.csv` (`identifier,username` with a TA
   login, e.g. `ta-1,<TA>`) and run `gh cls staff --tas tas.csv`. Verify `<TA>` is
   added to the `staff` team (or invited, if not yet an org member). Re-run → it
   reports `already in sync`. Replace `<TA>` with a different login → the run adds
   the new one and **warns** that `<TA>` is still on the team (not removed); re-run
   with `--prune` → `<TA>` is removed and named in the output.

2. **Seed a source.** Create a repo with at least one commit to squash from —
   e.g. a new repo initialized with a README named `hw1-src`.

3. **`gh cls template hw1-template -s $ORG/hw1-src --mark-source`** — verify
   `hw1-template` exists, is marked a *template repository*, is private, and has a
   single commit on its default branch. `--mark-source` flags the source `hw1-src`
   a template repository (the pre-req to generate from it); without the flag the
   command fails telling you to set it. Re-run without `-F` → it should error that
   `hw1-template` already exists; with `-F` → it recreates it.

4. **Add the assignment to the config** so `assign` can resolve it (`assign`
   errors with *"assignment not found in config"* otherwise). Its `template` is the
   repo assign clones — `hw1-template`, built in step 3:
   ```yaml
   org: gh-cls-sandbox
   staff_team: staff
   assignments:
     hw1:
       type: individual
       template: hw1-template
   ```
   Also create a `roster.csv` (kept out of git by `.gitignore`):
   ```csv
   identifier,username
   <STU>,<STU>
   ```

5. **`gh cls assign hw1 -r roster.csv --public --branch-protection --feedback issue`**
   — clones `hw1-template` into `hw1-<STU>`; verify `<STU>` is a direct collaborator
   with **push**, the staff team has push, a protection ruleset is present (public
   repo), and a *Feedback* issue is open. Re-run → it should report the repo
   `skipped`.

6. **`gh cls freeze hw1`** — `<STU>` drops to **read** (the `hw1-template` repo is
   skipped: freeze ignores template repositories). (Single-account fallback:
   reports `0` changed because you are admin-skipped.)

7. **`gh cls freeze hw1 --undo`** — push restored. Re-run → `0` changes.

8. **Group assignment (optional)** — build a group template
   (`gh cls template proj-template -s $ORG/hw1-src`), add a `group` assignment with
   `template: proj-template`, write a `teams.yml` (`alpha: [<STU>]`), and
   `gh cls assign proj -r roster.csv --teams teams.yml --public`. Verify
   `proj-alpha` is created with the team's members granted push.

9. **Cleanup** — delete `hw1-template`, every `hw1-*`, `hw1-src`, and any group
   repos. (Leaving the `staff` team is fine.)
