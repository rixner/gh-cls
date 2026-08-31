# gh-cls

A GitHub CLI extension that replaces the parts of GitHub Classroom a course
actually needs: one-time organization hardening, per-assignment squashed
templates, bulk creation of student/group repositories, optional branch
protection and feedback artifacts, and a hard deadline freeze.

Written because GitHub Classroom is being decommissioned, and because a CLI fits
the workflow better than a web interface.

## Compared to GitHub Classroom and Classroom 50

GitHub Classroom is being retired, with its role moving to partner tools. The
GitHub-recommended open-source successor is
[Classroom 50](https://github.com/foundation50/classroom50): a full platform with
a web interface, autograding through GitHub Actions, a gradebook, and student
self-service. If you want those, you should use it instead of this tool.

gh-cls is deliberately narrower and built on a different philosophy. It is not a
Classroom 50 competitor, just the GitHub organization-and-repository plumbing one
course needs, with the rest left out on purpose.

It intentionally does **not** provide (reach for Classroom 50 instead):

- autograding, correctness tests, scores, or a gradebook,
- a web interface (it is CLI-only),
- student-facing tooling (students just use git and GitHub as normal).

Where it does overlap, it operates differently:

- **Instructor-driven, not student-accept.** `assign` creates every repository up
  front; there is no "accept the assignment" step. Classroom 50 has students run
  `gh student accept`.
- **A hard deadline.** `freeze` downgrades write to read at the deadline, instead
  of recording an advisory due date.
- **One local config file the tool only reads.** No state lives in a config
  repository in your org, and the roster/groups files stay local and off GitHub.
- **Idempotent and fail-fast.** Commands re-assert state, verify their own pre-
  and post-conditions, and abort rather than leave anything half-done.
- **Local-first grading.** Feedback is posted as issue or PR comments, and
  `collect` pulls submissions into local shallow clones for hand grading, rather
  than running an autograder.

## Install

```sh
gh extension install rixner/gh-cls
```

Requires the [`gh`](https://cli.github.com) CLI (authenticated via `gh auth
login`). The extension inherits your existing `gh` authentication and never
handles tokens itself. Every command except `collect` runs purely against the
GitHub API and needs no `git` binary; `collect` clones repositories, so it alone
also needs `git` on your PATH (see [COLLECT.md](COLLECT.md)).

`gh auth login`'s default scopes omit two the tool needs: `admin:org` (org
settings and team management, used by `setup` and `staff`) and `delete_repo`
(rolling back a repo `assign`/`template` created, e.g. when `template --force`
overwrites one or a freshly generated repo comes out with the wrong
visibility). Grant both up front so a command doesn't fail mid-semester on a
403:
```sh
gh auth refresh -s admin:org -s delete_repo
```

## Student Information

Mappings between students and GitHub usernames live only in your local
**roster** and **groups** files, which the tool reads at runtime and never
writes into any repository. Keep these files off version control.

## Configuration

Reusable, no-PII course structure that **you author**; the tool only reads it,
never writes it. Point every command at it with `-c/--config <file>` or by
setting `$GH_CLS_CONFIG`; there is no search path or hidden config directory. The
file must set `org` and `staff_team` (the team may have no members yet; `setup`
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
each student/group repo. A bare name (`hw1-template`) is taken to live in the
configured `org`; qualify it with an owner (`other-org/base`) to clone a template
from another org. Build one with `gh cls template` (below), or point at any
existing GitHub *template repository*. `gh cls template` is not required.

`feedback` is optional. Omit the key (or leave it empty) and no feedback artifact
is created. Use that if you return grades outside GitHub. Only `issue` and `pr`
create one, so a value you are unsure about is better left off than guessed at:
`assign` opens the issue or PR on every student repo the first time it runs.

The key says what `assign` should create; it is not a record of what exists.
Changing it after an assignment's repos were made does not convert them, and
`assign` will not add a second artifact to a repo that already has one: it fails
that repo and names both kinds. `feedback` and `status` read what each repository
carries, so a mixed assignment still reports and grades correctly.

### Assignment names

Every command scopes an assignment to the repositories named `<name>-*`, so **no
assignment name may be another one followed by a `-`**. `hw1` and `hw1-makeup`
are rejected at config load, because `hw1`'s prefix (`hw1-`) also matches every
`hw1-makeup-*` repo, which would quietly mix the makeup repos into `hw1`'s
freezes, audits and counts.

This bites the common case of an exercise paired with a variant. Separate the
variant with anything but a dash and both names work:

```yaml
assignments:
  lab3:                           # in-class, group
    type: group
    template: lab3-template
  lab3_makeup:                    # asynchronous individual makeup
    type: individual
    template: lab3-template
```

### Templates in the namespace

Naming `hw1`'s template `hw1-template` is natural and supported, even though it
matches `hw1`'s own `<name>-*` prefix. Every command that lists the namespace
(`freeze`, `status`, `collect`, `activity`) excludes templates two ways, and
either alone is enough:

- **By GitHub's *template repository* flag.** This covers any template in the
  namespace, including a leftover or one no assignment names.
- **By name, against the templates the config names.** The flag is remote and
  mutable: cleared in the web UI, a template is student-repo-shaped to a listing,
  and `freeze` would downgrade its collaborators while `collect` cloned it as a
  submission. Matching the configured `template` values keeps the exclusion from
  depending on remote state. A template in another org is in no namespace here and
  needs neither check.

`audit` consults neither, since it works from the repos the roster and groups say
should exist. `assign` refuses outright to clone a repo that is not a template
repository, so a cleared flag stops it with a message rather than silently, and
`--mark-template` sets the flag again.

One collision the exclusions cannot fix: **a student or group key can complete a
template's name.** A group named `template` under `hw1` wants the repo
`hw1-template`, which is the template itself, and `assign` adopts a repo that
already exists rather than creating one, so that group would be handed push on the
starter code. `assign` checks this before it creates or grants anything and
refuses the run, naming the unit and the template. A template's name is arbitrary,
so the check covers every assignment's template, not just the one being run:
`assignments.hw1.template: hw2-starter` is a legal thing to write, and it collides
with a `starter` key under `hw2`.

The **roster** is a local CSV mapping student identifier → GitHub username:

```csv
identifier,username
student-001,ada-lovelace
student-002,alan-turing
```

A **groups** file (group assignments) maps group name → student identifiers:

```yaml
group-alpha: [student-001, student-003]
group-beta:  [student-002]
```

A **TA** file (for `gh cls staff`) is a CSV in the same `identifier,username`
format as the roster, listing the staff team's GitHub usernames.

## Commands

Every command reads the org and staff team from the config (`-c/--config` or
`$GH_CLS_CONFIG`); neither is a command-line flag. Every mutating command
requires you to be an organization **owner** and accepts `-n/--dry-run`.
Persistent flags: `-c/--config`, `--log-requests`. The examples below assume
`export GH_CLS_CONFIG=gh-cls.yml` (otherwise add `-c gh-cls.yml` to each).

```sh
# 1. Per-semester: harden the org named in the config.
gh cls setup

# 1b. Whenever the TA staff changes: add (and optionally prune) the staff team.
gh cls staff --tas tas.csv             # add-only; warns about unlisted members
gh cls staff --tas tas.csv --prune     # also remove members not in the file

# 2. Optional: build a squashed, single-commit template repo from a source.
gh cls template hw1-template --source cs101-staff/hw1-dev

# 3. Create one repo per student (or group) from the assignment's template repo.
gh cls assign hw1 --roster roster.csv
gh cls assign project --roster roster.csv --groups groups.yml --branch-protection

# Anytime: a read-only overview of the staff team and each assignment's repos.
gh cls status
gh cls status hw1
gh cls status hw1 --detail   # per-repo freeze/feedback scan, also writes a CSV

# Anytime: GitHub's record of who moved which branch when.
gh cls activity hw1                       # per-repo summary
gh cls activity hw1 --all                 # every change, by who made it
gh cls activity hw1 -w                    # force pushes and branch deletions

# 4. Anytime: reconcile who should be on each repo against who actually is.
gh cls audit hw1 --roster roster.csv
gh cls audit project --roster roster.csv --groups groups.yml   # group: --groups too
gh cls audit hw1 --roster roster.csv --renew   # re-issue expired/missing access

# 5. At the deadline: downgrade students from write to read (reverse with -u).
gh cls freeze hw1
gh cls freeze hw1 --undo
gh cls freeze hw1 alice --undo   # extension: unfreeze just one student/group repo
gh cls freeze hw1 alice          # re-freeze it when the extension expires

# 6. Collect submissions locally to grade by hand (one shallow clone per student,
#    tagged each collect; see COLLECT.md for the model and the git you need).
gh cls collect hw1 --roster roster.csv --out ./hw1

# 6b. Or pin the deadline commit for every repo first, then collect exactly
#     those, so every student is cut at the same instant (see COLLECT.md).
gh cls activity hw1 -s --to 2026-03-01T23:59:59-06:00 -o deadline.yml
gh cls collect hw1 --roster roster.csv --out ./hw1-final --snapshot deadline.yml

# 7. After grading: post one feedback file per student/group as a comment on the
#    repo's feedback issue or PR. Files are named <username>.md / <group>.md.
gh cls feedback hw1 --dir ./hw1-feedback --roster roster.csv
```

- **setup** sets base permission to none, disables member repo/Pages creation,
  forking of private repositories, and Actions org-wide, reports Copilot status,
  ensures the staff team exists, and declares the `gh-cls-frozen` organization
  property that `freeze` records deadline state in (see **The freeze record**
  below). All actions are idempotent and report changed vs already-in-desired-state. It also
  prints an optional-hardening checklist for member-privilege toggles that exist
  only in the web UI (installing apps, changing repository visibility, deleting or
  transferring repositories, creating teams). These are the instructor's to
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
  source must already be a template repository. `--mark-source` opts into
  marking it rather than failing; `-F` overwrites an existing `<repo>`. A bare
  `<repo>` is created in the org; `--source` is always `owner/name`.
- **assign** runs preflight checks (type/inputs; the assignment's template repo
  exists and is a template repository; all-branches single-commit; roster/groups
  consistency; every roster username is a real GitHub account; no student or
  group would be given a repo that is really a template, see **Templates in the
  namespace** below), then generates each repo from that template concurrently. The
  template must be a template repository. `--mark-template` opts into marking it.
  Repos are private unless `-p/--public` is given, and only the template's default
  branch is generated unless `-a/--all-branches` copies them all. The template
  must be fully squashed (each branch a single commit); `-U/--allow-unsquashed`
  overrides that preflight and clones the history as-is.
  Grants on an existing repo follow that repo's freeze record, so re-running
  assign after a deadline (to add a late student, say) re-asserts read on the
  frozen repos instead of handing write back to the class.
  For a group assignment, an enrolled student in no group, or a student in more
  than one group, aborts the whole run before any repo is created, listing every
  problem so the groups file can be fixed in one pass; `--force` (`-F`) downgrades
  those to warnings and proceeds (e.g. a student intentionally excused from the
  group work). `-b` applies an all-branches ruleset blocking force-push and
  deletion, which only org admins bypass (staff get push but cannot force-push or
  delete protected branches). `--feedback pr|issue` overrides the assignment's
  feedback setting for one run, which is how a single repo is given the other
  artifact without editing the config. Idempotent: existing repos are skipped but
  access grants are re-asserted.
- **activity** reports GitHub's own record of who moved which branch when. With
  no mode flag it prints a per-repo summary; `--all` summarizes every recorded
  change by who made it, answering "who has been pushing to this repo", and with
  `-o` writes every individual change as CSV; `-w/--rewrites` lists force pushes
  and branch deletions together. Those matter most on a **free organization**, where
  the `--branch-protection` ruleset cannot apply to private repositories and so
  cannot block either: this is how you see what you are unable to prevent. A
  deletion's `before` commit is the tip that was removed, which is often still
  fetchable, so the report doubles as a route back to deleted work. `-s/--snapshot`
  writes a
  snapshot file of each repo's commit as of `--to`, ready for
  `collect --snapshot`, which is how you get a deadline that is identical for
  every student (see **The freeze record** and COLLECT.md). Before writing one it
  checks that GitHub's record has caught up with each branch's current tip and
  that every recorded commit is still retrievable, so it never hands back a snapshot
  file that cannot be collected. `--from`/`--to` bound the window (`--to` defaults
  to now), `--branch` picks a branch, and `-o` writes the artifact to a file. Reads only,
  so it needs no org-owner role.
- **audit** reconciles the students who should be on the `<name>-*` repos against
  the actual state, reporting each as *on repo*, *invited (pending)*, *invited
  (EXPIRED)*, *MISSING*, or *NO REPO*, and flagging access that is present but not
  expected. `--roster` is always required, and a **group assignment also needs
  `--groups`**. Audit resolves the expected members of each group repo the same way
  `assign` does, so it takes the same two files (an individual assignment rejects
  `--groups`). Because students join as outside collaborators, a grant becomes an
  invitation they must accept within seven days, so `--renew` re-issues access for
  everyone whose invitation expired or who is missing entirely (it never removes
  access). A student already on a frozen repo reads as *on repo (frozen)*, which
  is a settled state: it is not listed by default and `--renew` leaves it alone.
  For students `--renew` does act on, the access it restores follows the repo's
  freeze record, so renewing after a deadline hands back read, not write.
  `--all` lists everyone, not just those needing attention. It also
  warns (never aborts) when the groups file leaves a student in no group or in more
  than one, the same inconsistencies assign refuses to create repos for.
- **freeze** operates purely on each repo's current direct collaborators and
  pending invitations, never the roster, so a drifted roster cannot let anyone
  escape the freeze. **Pending invitations are downgraded too**: a student who has
  not accepted yet is not a collaborator, but their invitation still carries the
  write access it was issued with, so leaving it alone would let them accept after
  the deadline and push. Expired invitations are left as they are, since they can
  no longer be accepted, and `audit --renew` is how you re-issue one. Invitations
  are downgraded *before* collaborators, which closes the race where a student
  accepts mid-run: a student sits in the invitation list until they accept and in
  the collaborator list afterwards, so doing collaborators first would leave a
  window in which they appear in neither and keep write. Each repo's
  state is also recorded (see **The freeze record** below), so `freeze` requires
  `setup` to have run and refuses to start otherwise. It skips
  template repositories, so a `<name>-template` that matches the `<name>-*` prefix
  is never frozen. Naming one or more student/group keys (`freeze hw1 alice`)
  scopes it to just those `<name>-<key>` repos, for granting or ending an
  individual extension; an unknown key aborts the run before any change.
  `--undo` grants push to every non-admin direct collaborator, restores every live
  invitation to write, and records the repo as thawed, including any collaborator
  who was deliberately read-only before the freeze.
- **feedback** posts one feedback file per student (or group) as a comment on that
  repo's feedback issue or PR: whichever artifact the repository actually carries,
  found by looking rather than by trusting the assignment's `feedback` policy, so
  a policy changed after the repos were made cannot send grades to an artifact
  that is not there. A repo whose kind differs from the policy is named in the
  run, and one carrying both is an error. Each file in `--dir` is `<key>.md` or
  `<key>.txt`, where `<key>` is the GitHub username (individual) or group name
  (group), resolved from `--roster` (plus `--groups` for a group assignment);
  contents are rendered as Markdown. The directory must hold exactly one
  file per student/group. A missing file or a file matching no one is named and
  aborts, unless `--force` posts the matching subset and reports the rest.
  Idempotent: a re-run only posts feedback not already present (so a partial or
  `--force` run is finished by re-running), and editing a file posts a new comment
  rather than changing the old one.
- **collect** clones each student or group repository locally for hand grading,
  one shallow clone per repo under `--out`, taking each to its target commit and
  tagging it (`gh-cls/collect/<label>`) so every collection is preserved. The
  default target is the default-branch tip; `--snapshot <yml>` pins exact SHAs
  (for grading the deadline state), and `gh cls activity --snapshot` is what produces
  that file. Each collection's tag is named by `--label`
  (default: a timestamp). Re-running a label tops up only repos not yet collected
  under it; a new label advances the clones and tags the new state, leaving prior
  tags in place. It is roster-aware (`--roster` for individual,
  `--groups` for group), reporting any missing or unexpected repositories, and
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
  feedback artifact. The artifact is read from the repository rather than from
  the config, so the report shows which kind each repo actually carries (a
  `feedback_kind` column, and a note naming any repo whose kind is not the one
  the assignment configures) as well as its state (open, closed, or missing). It
  prints per-assignment counts and writes a per-repo CSV. The freeze state counts a pending invitation
  as the access it will confer on acceptance, so a repo whose collaborators are
  all read-only but which still has a write invitation outstanding reads as
  unfrozen. Each repo's actual access is compared against its freeze record and
  any disagreement is reported as `DRIFT`, which catches a freeze that did not
  fully take or an extension that was never actually granted. The CSV is a
  timestamped file in the current directory, or `--out <path>`, and is never
  overwritten: the timestamped name rolls to a new one on a same-second re-run,
  so a run, fix, re-run loop leaves both files to compare, while an explicit
  `--out` that already exists is refused instead.
  `--detail` costs two to three API calls per repo, plus one org-wide call for the
  freeze record; the default summary costs neither. status reads only, so it needs
  no org-owner role, and it still works on an org that has not run `setup`,
  reporting every repo as *not recorded*.

## The freeze record

`freeze` records each repository's deadline state in a `gh-cls-frozen`
organization custom property (`true` when frozen, `false` after an `--undo`,
absent if never frozen). `setup` declares the property, and `assign`, `freeze`
and `audit --renew` all refuse to run until it exists.

Every command that grants student access consults it, which is the point: a
freeze that only `freeze` knows about is one that `assign` or `audit --renew`
silently undoes the next time either re-asserts a grant.

The record exists because freeze state cannot be reliably inferred from
permissions. `audit --renew` restores access to students who have **none**, so
their own repository holds no permission to read the state from, and on an
individual assignment that student is the repo's only collaborator. Widening the
question to the whole assignment does not help either: `freeze hw1 alice --undo`
grants one extension, so a partly-frozen assignment is a normal state rather than
an anomaly. Without a per-repository record, a renew after the deadline hands
push back.

A custom property was chosen over the alternatives on four counts:

- **It is not a git ref**, so no push can remove it. A tag or git note can be
  deleted by anyone with write access.
- **Students cannot change it.** The property is declared `values_editable_by:
  org_actors`, so not even a repository admin can set a value, and students join
  as outside collaborators with push. `setup` re-asserts this on every run and
  warns if it has been widened. Repository topics, by contrast, are editable by
  the Maintain role.
- **It is free.** Custom properties work on GitHub Free organizations, so the
  deadline lock does not depend on a paid plan. That rules out doing this with a
  ruleset, which needs Team or higher for private repositories.
- **Updates are atomic.** Setting one property leaves a repository's other
  property values untouched. The topics API replaces the whole set, so it would
  need a read-modify-write that can lose a concurrent change.

Reading it is cheap: one org-wide call returns every repository's value, so it
does not scale with class size.

## Running commands concurrently

> [!WARNING]
> **Run only one `gh cls` command against an organization at a time.** The tool
> has no way to lock an org, and GitHub offers no atomic update across a
> repository's freeze record and its access grants, so two runs against the same
> org can interleave badly. In particular, a `freeze` running at the same time as
> an `assign` or an `audit --renew` can leave an assignment writable past its
> deadline.

This is not theoretical, and it is not fully preventable. What the tool does
instead is refuse to hide it:

- `assign` and `audit --renew` re-read the freeze record **after** their grants.
  If a repository they granted write to has since been recorded frozen, the run
  fails, names those repositories, and tells you to re-run `gh cls freeze <name>`.
- `freeze` verifies every repository after changing it, so an access grant that
  landed underneath it fails that repository loudly rather than reporting a lock
  that did not hold.

Either way you get a non-zero exit and a named repair, never a clean-looking run.
A `freeze` is also not instantaneous: it works through repositories concurrently,
so on a large class there are seconds to minutes between the first repository
being locked and the last. Treat the deadline as "when freeze finishes", and
prefer `gh cls activity --snapshot` to record exact SHAs, then `collect --snapshot`, if you
need a precise cut-off.

### What students do concurrently is safe

Student activity is not a hazard for these commands, with one exception:

- **Pushing** does not affect `freeze`, `audit`, `status`, or `feedback` at all;
  those read and write access and metadata, never refs. `assign` reshapes the
  default branch only when it first creates a feedback PR, which happens before
  anyone is granted access.
- **Accepting an invitation** is handled: `freeze` downgrades invitations before
  collaborators precisely so a student accepting mid-run cannot slip between the
  two.
- **`collect`** is the one command a push can race, by design: the default target
  is the default-branch tip, so a push during collection is simply collected.
  Pin exact commits with `gh cls activity --snapshot` and `collect --snapshot` when that
  matters, and note that `collect` reports a forced (history-rewriting) update
  rather than silently accepting it.

## Rate limits

A bulk command issues many API calls, so GitHub may rate-limit a run. Two things
keep that from ending the run early.

Requests are paced run-wide, each kind at its own rate: creating content (a
repository, a commit, a pull request) against GitHub's 80 per minute, changing
who can reach something against the 180 writes a minute an endpoint allows, and
reads well under their own far looser ceiling. The rate is enforced where the
request is made, so it holds however much of the run is happening at once.

The client also caps how many requests it has outstanding at once, well under
the 100 concurrent requests GitHub allows. The rates make that unreachable in
normal running, so it engages only if responses hang.

The separation matters most for `freeze`, which is made entirely of access
changes and is a deadline: it runs at the faster rate, rather than being held to
the rate that governs creating repositories.

`collect`'s clones are paced separately, because git operations are not governed
by the API's limits and nothing GitHub publishes covers cloning a class of
repositories back to back. They run one at a time, three seconds apart, which is
a spacing a full class has been observed to complete reliably at rather than a
measured threshold, so it is deliberately conservative: a 70-repository
collection takes around three and a half minutes.

When GitHub does refuse a request, the whole run pauses and then continues,
rather than failing the repository that was refused. That covers the primary
limit, the secondary one ("You have exceeded a secondary rate limit"), and the
422 that answers a burst of repository generation ("was submitted too quickly"),
honoring GitHub's own `Retry-After` and reset headers wherever they are sent.
Since a limit applies to the whole run, every request waits, not just the one
that was refused.

If a run stays limited through every retry it fails naming the request that was
refused; re-running is safe, since every command skips what already exists.

`--log-requests <file>` appends one JSON line per API call: the time, the method
and path, the attempt, the status, how long the request took, how long the run
held it back, and any rate-limit headers the response carried. GitHub does not
document what the 422 counts or where its threshold is, so a log of a real run
is how to find out; it also shows whether a slow run was waiting on GitHub or on
the run's own pacing. The file is appended to, never truncated, and it records
nothing that a repository name does not already say.

Because the rates are known, `assign` says what a run will cost before it
changes anything: how many repositories it will create against how many it will
only re-assert, and how long that takes at those rates. The figure is a floor (it
counts the pacing, not GitHub's own response time, so a real run takes somewhat
longer), and it is there so a long run is a decision rather than a surprise.

It also warns where a run would meet one of GitHub's hourly ceilings. The one a
large class meets first is the primary limit of 5,000 requests an hour, which
counts reads too: a repository costs around a dozen of them, over half of which
are polls waiting for GitHub to reflect its own writes. A class of about 250
provisioned in one run exceeds it, pausing until the hour resets before
finishing.

## Before a real run

Preview any command with `--dry-run` first. A dry run changes nothing, but it
does run the command's preflight checks against the real organization and
reports what each repository would get, so it also catches a missing staff team,
an unsquashed or unmarked template, a roster username that does not exist, and
repositories that already exist or are recorded frozen.

The `--branch-protection` ruleset requires the organization to be on GitHub's
Team plan or higher; confirm under **Billing & plans** that the org shows
"Team". The freeze record needs no paid plan.

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
