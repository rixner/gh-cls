# gh-cls

A GitHub CLI extension that replaces the parts of GitHub Classroom a course
actually needs: one-time organization hardening, per-assignment squashed
templates, bulk creation of student/team repositories, optional branch
protection and feedback artifacts, and a hard deadline freeze.

Written because GitHub Classroom is being decommissioned, and because a CLI fits
the workflow better than a web interface.

## Install

```sh
gh extension install rixner/gh-cls
```

Requires the [`gh`](https://cli.github.com) CLI (authenticated via `gh auth
login`). The extension inherits your existing `gh` authentication and never
handles tokens itself; every command, including `template`, runs purely against
the GitHub API, so no `git` binary or separate git credentials are needed.

## Student Information

Mappings between students and GitHub usernames live only in your local
**roster** and **teams** files, which the tool reads at runtime and never
writes into any repository. Keep these files off version control.

## Configuration

Reusable, no-PII course structure that **you author**; the tool only reads it,
never writes it. Point every command at it with `-c/--config <file>` or by
setting `$GH_CLS_CONFIG`; there is no search path or hidden config directory. The
file must set `org` and `staff_team` (the team may have no members yet — `setup`
creates it and `assign` grants it access to every repo, so a TA added later
inherits access to all existing assignments):

```yaml
org: cs101-spring26
staff_team: staff

assignments:
  hw1:
    type: individual
    template: hw1-template        # the repo assign clones; bare -> cs101-spring26/hw1-template
    feedback: issue
  project:
    type: group
    template: shared-org/proj-base
    branch_protection: true
    feedback: pr
```

An assignment's `template` is the **template repository assign clones** to create
each student/team repo. A bare name (`hw1-template`) is taken to live in the
configured `org`; qualify it with an owner (`other-org/base`) to clone a template
from another org. Build one with `gh cls template` (below), or point at any
existing GitHub *template repository* — `gh cls template` is not required.

The **roster** is a local CSV mapping student identifier → GitHub username:

```csv
identifier,username
student-001,ada-lovelace
student-002,alan-turing
```

A **teams** file (group assignments) maps team name → student identifiers:

```yaml
team-alpha: [student-001, student-003]
team-beta:  [student-002]
```

A **TA** file (for `gh cls staff`) is a CSV in the same `identifier,username`
format as the roster, listing the staff team's GitHub usernames.

## Commands

Every command reads the org and staff team from the config (`-c/--config` or
`$GH_CLS_CONFIG`); neither is a command-line flag. Every mutating command
requires you to be an organization **owner** and accepts `-n/--dry-run`.
Persistent flags: `-c/--config`, `-j/--concurrency`. The examples below assume
`export GH_CLS_CONFIG=gh-cls.yml` (otherwise add `-c gh-cls.yml` to each).

```sh
# 1. Per-semester: harden the org named in the config.
gh cls setup

# 1b. Whenever the TA staff changes: add (and optionally prune) the staff team.
gh cls staff --tas tas.csv             # add-only; warns about unlisted members
gh cls staff --tas tas.csv --prune     # also remove members not in the file

# 2. Optional: build a squashed, single-commit template repo from a source.
gh cls template hw1-template --source cs101-staff/hw1-dev

# 3. Create one repo per student (or team) from the assignment's template repo.
gh cls assign hw1 --roster roster.csv
gh cls assign project --roster roster.csv --teams teams.yml --branch-protection

# Anytime: a read-only overview of the staff team and each assignment's repos.
gh cls status
gh cls status hw1
gh cls status hw1 --detail   # per-repo freeze/feedback scan, also writes a CSV

# 4. Anytime: reconcile who should be on each repo against who actually is.
gh cls audit hw1 --roster roster.csv
gh cls audit hw1 --roster roster.csv --renew   # re-issue expired/missing access

# 5. At the deadline: downgrade students from write to read (reverse with -u).
gh cls freeze hw1
gh cls freeze hw1 --undo

# 6. Collect submissions locally to grade by hand (one shallow clone per student,
#    tagged each collect; see COLLECT.md for the model and the git you need).
gh cls collect hw1 --roster roster.csv --out ./hw1

# 7. After grading: post one feedback file per student/team as a comment on the
#    repo's feedback issue or PR. Files are named <username>.md / <team>.md.
gh cls feedback hw1 --dir ./hw1-feedback --roster roster.csv
```

- **setup** sets base permission to none, disables member repo/Pages creation
  and Actions org-wide, reports Copilot status, and ensures the staff team
  exists. All actions are idempotent and report changed vs already-in-desired-state. It also
  prints an optional-hardening checklist for member-privilege toggles that exist
  only in the web UI (installing apps, changing repository visibility, deleting or
  transferring repositories, creating teams) — these are the instructor's to
  apply or leave open, at their discretion.
- **staff** adds the GitHub usernames in a `--tas` CSV (the same
  `identifier,username` format as the roster) to the staff team. By default it
  only **adds**: members not in the file are left alone but reported with a
  warning pointing at `--prune`, so an incomplete file can never silently remove a
  TA. `--prune` also removes members not in the file, naming each removal so a
  mistake is easy to undo; `--dry-run` previews either. A TA who is not yet an org
  member is invited and joins on acceptance. The team must already exist (from
  setup), and the file must list at least one TA (an empty file is rejected).
- **template** builds `<repo>` as a single-commit, history-free copy of
  `--source` (via GitHub's template generation) and marks it a template
  repository so assign can clone it. It is optional: assign clones whatever
  template an assignment names, so any existing template repository works. The
  source must already be a template repository — `--mark-source` opts into
  marking it rather than failing; `-F` overwrites an existing `<repo>`. A bare
  `<repo>` is created in the org; `--source` is always `owner/name`.
- **assign** runs preflight checks (type/inputs; the assignment's template repo
  exists and is a template repository; all-branches single-commit; roster/teams
  consistency), then generates each repo from that template concurrently. The
  template must be a template repository — `--mark-template` opts into marking it.
  `-b` applies an all-branches ruleset blocking force-push and deletion, which
  only org admins bypass (staff get push but cannot force-push or delete
  protected branches); `-f pr|issue` adds a feedback artifact. Idempotent:
  existing repos are skipped but access grants are re-asserted.
- **audit** reconciles the students who should be on the `<name>-*` repos
  (resolved from the roster, plus the teams file for a group assignment) against
  the actual state, reporting each as *on repo*, *invited (pending)*, *invited
  (EXPIRED)*, *MISSING*, or *NO REPO*, and flagging access that is present but not
  expected. Because students join as outside collaborators — a grant becomes an
  invitation they must accept within seven days — `--renew` re-issues access for
  everyone whose invitation expired or who is missing entirely (it never removes
  access). `--all` lists everyone, not just those needing attention.
- **freeze** operates purely on each repo's current direct collaborators, never
  the roster, so a drifted roster cannot let anyone escape the freeze. It skips
  template repositories, so a `<name>-template` that matches the `<name>-*` prefix
  is never frozen.
- **feedback** posts one feedback file per student (or team) as a comment on that
  repo's feedback issue or PR — the artifact assign created, named by the
  assignment's `feedback` policy. Each file in `--dir` is `<key>.md` or
  `<key>.txt`, where `<key>` is the GitHub username (individual) or team name
  (group); contents are rendered as Markdown. The directory must hold exactly one
  file per student/team — a missing file or a file matching no one is named and
  aborts, unless `--force` posts the matching subset and reports the rest.
  Idempotent: a re-run only posts feedback not already present (so a partial or
  `--force` run is finished by re-running), and editing a file posts a new comment
  rather than changing the old one.
- **collect** clones each student or team repository locally for hand grading,
  one shallow clone per repo under `--out`, taking each to its target commit and
  tagging it (`gh-cls/collect/<label>`) so every collection is preserved. The
  default target is the default-branch tip; `--commits <yml>` pins exact SHAs
  (for grading the deadline state). Re-running a label tops up only repos not yet
  collected under it; a new label advances the clones and tags the new state,
  leaving prior tags in place. It is roster-aware (`--roster` for individual,
  `--teams` for group), reporting any missing or unexpected repositories, and
  refuses to disturb a clone with local changes so grading-script edits survive.
  Shallow keeps disk small; a clone is a normal git repo, so `git restore .`,
  `git fetch --unshallow`, and `git checkout gh-cls/collect/<label>` all work.
  It is the one command that uses git (cloning via `gh`, updates via git). **See
  [COLLECT.md](COLLECT.md) for the model and the git you may want.**
- **status** reports the current state of the org without changing anything: the
  staff team and its size, and for each assignment (or just `<name>`) how many
  student repositories exist and their visibility, flagging any that contradict
  the assignment's policy. With `--detail` it also scans each repo for its freeze
  state (write vs read for non-admins, including a "mixed" partial freeze) and its
  feedback issue/PR state (open, closed, or missing), printing per-assignment
  counts and writing a per-repo CSV. The CSV is a timestamped file in the current
  directory (or `--out <path>`) and is never overwritten: a same-second re-run
  rolls to a new name, so a run, fix, re-run loop leaves both files to compare.
  `--detail` costs one to two API calls per repo; the default summary does not.
  status reads only, so it needs no org-owner role.

## Before a real run

Preview any command with `--dry-run` first. The `--branch-protection` ruleset
requires the organization to be on GitHub's Team plan or higher; confirm under
**Billing & plans** that the org shows "Team".

## Development

```sh
go build         # builds the gh-cls binary
go vet ./...     # static checks across all packages
go test ./...    # all tests run locally against fakes (no network)
```

The tests above never touch the network. For exercising `gh cls` end to end
against a real, disposable org (by hand or via the opt-in `go test -tags live
./live/`), see [TESTING_LIVE.md](TESTING_LIVE.md).

## AI Assistance

This project was developed with assistance from AI coding tools. All code has been
reviewed, tested, and accepted by the maintainers.
