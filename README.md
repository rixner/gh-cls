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
**roster** and **teams** files, which the tool reads at runtime and never writes
into any repository. Keep these files off version control.

## Configuration

Reusable, no-PII course structure, found (first match wins) at `$GH_CLS_CONFIG`,
`./.gh-cls.yml`, or `$XDG_CONFIG_HOME/gh-cls/config.yml`:

```yaml
# `org` is written by `gh cls setup --org`, not by hand.
org: cs101-spring26
staff_team: staff

assignments:
  hw1:
    type: individual
    template: cs101-templates/hw1-starter
    feedback: issue
  project:
    type: group
    template: cs101-templates/project-starter
    branch_protection: true
    feedback: pr
```

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

## Commands

Every mutating command requires you to be an organization **owner** and accepts
`-n/--dry-run`. Persistent flags: `-o/--org` (override the config org),
`-s/--staff-team`, `-j/--concurrency`.

```sh
# 1. Per-semester: harden the org and record it in config (requires --org).
gh cls setup --org cs101-spring26 --staff-team staff

# 2. Per-assignment: generate a single-commit <name>-template in the org.
gh cls template hw1

# 3. Create one repo per student (or team), granting push + staff access.
gh cls assign hw1 --roster roster.csv
gh cls assign project --roster roster.csv --teams teams.yml --branch-protection

# 4. At the deadline: downgrade students from write to read (reverse with -u).
gh cls freeze hw1
gh cls freeze hw1 --undo
```

- **setup** sets base permission to none, disables member repo/Pages creation
  and Actions org-wide, reports Copilot status, and ensures the staff team. All
  actions are idempotent and report changed vs already-in-desired-state.
- **template** generates `<name>-template` from the maintained source template
  via GitHub's template generation, so the source's history is never exposed (the
  derived repo is one fresh commit). It marks the source as a template repository
  if needed, since generation requires it. `-F` replaces an existing one.
- **assign** runs preflight checks (type/inputs, in-org template, all-branches
  single-commit, roster/teams consistency), then generates repos concurrently.
  `-b` applies an all-branches ruleset blocking force-push and deletion; `-f
  pr|issue` adds a feedback artifact. Idempotent: existing repos are skipped but
  access grants are re-asserted.
- **freeze** operates purely on each repo's current direct collaborators, never
  the roster, so a drifted roster cannot let anyone escape the freeze.

## First run against a live org

Nothing here was exercised against the live GitHub API while it was written.
Before a real run, preview with `--dry-run`, and verify in Billing & plans that
the **org** shows "Team" (required for `--branch-protection`).

## Development

```sh
go build ./...   # builds the gh-cls binary
go test ./...    # all tests run locally against fakes (no network)
```
