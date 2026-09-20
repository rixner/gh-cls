# Collecting submissions with `gh cls collect`

`gh cls collect` pulls every student's repository to your machine so you can grade
the code by hand. It is the one command in the tool that uses git, and it keeps a
small, durable local copy of each submission. This guide explains the model and
the few git commands you may want once the code is local. You do not need to be a
git expert to use it.

## Prerequisites

- `git` on your PATH. You almost certainly already have it.
- `gh` authenticated (you already use it for the other commands). One-time, run
  `gh auth setup-git` so git can authenticate to GitHub on your behalf.

Collect never reads or stores a token; cloning goes through `gh`, and updates use
git with the credentials `gh` already manages.

## Quick start

Individual assignment (keys are GitHub usernames, from the roster):

```sh
gh cls collect hw1 --roster roster.csv --out ./hw1
```

Group assignment (keys are group names, from the groups file; no roster needed):

```sh
gh cls collect project --groups groups.yml --out ./project
```

You get one directory per student or group:

```
hw1/
  ada/          a git clone of hw1-ada at the collected commit
  alan/
  grace/
  collected.csv a record of what was collected (label, key, repo, sha, ref, time)
  .gh-cls/      collect's own state for this directory: the history setting, the
                lock a run holds while it works, and the staging area new clones
                are assembled in
```

`--out` is required on purpose, so repositories are never cloned into a surprise
location.

To see what a run would do before it does anything, add `-n`/`--dry-run`. It
makes every read-only check a real run makes, the local git reads and the tip
lookups and the snapshot file, then prints the plan: the label and history
setting, how many repositories would be cloned, updated or left alone, which ones
a check would refuse and why, and how many paced git operations the run would
spend and roughly how long that takes at the current spacing. It makes no network
git requests and writes nothing, not even the lock.

## The model: one clone per student, tagged each time

Each `<out>/<key>` is a real git clone. By default it is a **shallow** one: it
contains the files at the collected commit, not the student's entire history.
That keeps disk use small even when students have committed large binaries over
the term.

If you want the whole history and every branch instead, say so once:

```sh
gh cls collect hw1 --roster roster.csv --out ./hw1 --history full
```

The setting is recorded for that `--out` directory, so later runs keep it even
if you forget the flag, and a colleague running the plain command into the same
directory gets clones that match the ones already there. `--history snapshot`
switches back: history already in the clones stays, new collections have none,
and the other branches stop being updated. Every run's header says which setting
is in force and where it came from.

Every time you collect, the commit you took is **tagged** inside that clone, under
`gh-cls/collect/<label>`. Because each collection is tagged, **no collected state
is ever lost**: re-collecting later moves the working copy forward but leaves the
earlier commit reachable through its tag.

The `--label` names the collection. Without it, collect uses a timestamp:

```sh
gh cls collect hw1 --roster roster.csv --out ./hw1 --label midterm
```

tags each repo's collected commit `gh-cls/collect/midterm`.

## Re-collecting

- **Same label again:** repos already collected under that label are left alone
  (reported `up-to-date`); only students who were missing before (a late accept)
  are collected. This makes it safe to re-run to pick up stragglers.
- **A new label:** the clones are updated to the new target commit and the new
  state is tagged, while every prior label's tag stays put. So `--label final`
  after `--label midterm` advances the working copy to the latest code and keeps
  the midterm commit available as `gh-cls/collect/midterm`.

## Grading exactly the deadline commit

Give collect a YAML file of `key: sha` and it checks out exactly those commits,
regardless of anything pushed afterward. `gh cls activity --snapshot` writes
that file from GitHub's own record of when each push landed:

```sh
gh cls activity hw1 -s --to 2026-03-01T23:59:59-06:00 -o deadline.yml
```

The file is just a mapping, so you can also write it by hand, or edit one to give
a student a later commit. Every SHA must be a whole commit name, 40 characters;
an abbreviation is refused rather than guessed at, since collect names the commit
to GitHub when it fetches it.

```yaml
# deadline.yml
ada:         9f3a2b1c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a
alan:        1c4d77e0b5a9382f6e1d04c7a3b28e5f9d60c1a4
group-alpha: a0b1c2d3e4f5061728394a5b6c7d8e9f0a1b2c3d
```

Hand that file to collect and every clone is checked out at its pinned commit:

```sh
gh cls collect hw1 --roster roster.csv --out ./hw1-final --snapshot deadline.yml --label final
```

**An edited snapshot needs a new label.** A label names one commit per repository,
for good: that is what makes a collection permanent. If you have already collected
`--label final` and then edit the file to give a student a later commit, re-running
`final` is refused for that student, naming both commits, rather than quietly
moving what `final` means. Collect the extension under its own label instead:

```sh
gh cls collect hw1 --roster roster.csv --out ./hw1-final --snapshot deadline.yml --label final-ada
```

A student with no SHA in the file is skipped and reported, so you grade exactly
the pinned set. This pairs naturally with `gh cls freeze`: once a repo is frozen
at the deadline its tip is read-only, so the deadline commit stays available.

## A consistent deadline or precise student feedback: pick one

Two things you would like at a deadline are in tension, and no combination of
these commands gives you both. Decide which you want before the deadline, not
after a student appeals.

**1. Freeze, then collect: precise feedback, smeared deadline.** Once `freeze`
has locked a repo, a push to it is rejected, so the student finds out at once
whether their push counted. Nothing they push is silently discarded. `freeze`
locks repos concurrently, so a student whose repo is locked last had seconds or
minutes longer than one locked first; the window is bounded by the freeze's own
duration, and once the freeze finishes the tips cannot move again.

**2. `--snapshot` from push events: consistent deadline, silent cutoff.** Take
each repo's SHA from the push events at the deadline instant and every student is
cut at exactly the same moment, with no window at all. Nothing tells the student:
a late push succeeds, they watch it land, and they learn only when grades come
back that it was not the commit you graded.

**3. Unpinned collect, no freeze: neither.** Each repo's target is its
default-branch tip as of the moment collect reaches that repo, and collect paces
itself through the class, so the cut is smeared across the run. The
student gets no signal either: their push succeeds whether or not it was
collected, and whether it counted comes down to the order collect walked the
class. That is the smear of (1) with the silence of (2).

`gh cls activity --snapshot` produces option (2)'s input for you:

```sh
gh cls activity hw1 -s --to 2026-03-01T23:59:59-06:00 -o deadline.yml
gh cls collect hw1 --roster roster.csv --out ./hw1-final --snapshot deadline.yml --label final
```

It reads GitHub's own record of ref changes, takes each repo's commit as of
`--to`, and writes the snapshot file. The timestamps are GitHub's server-side
record of when each push landed, so they are neither commit dates (which the
pusher controls and can backdate) nor webhook receipt times (which trail the
push, and by far more when GitHub retries a failed delivery).

That record is [`/repos/{owner}/{repo}/activity`](https://docs.github.com/en/rest/repos/repos#list-repository-activities),
for which GitHub documents no retention, completeness or latency guarantee. So
`--snapshot` verifies rather than assumes. It refuses to write a file if GitHub's
record has not yet caught up with a branch's current tip, which is how a lagging
record is caught instead of silently yielding an earlier commit, and it refuses
to pin any commit that is no longer retrievable. Both fail the run rather than
handing back an artifact that would break on collection day, and one repository
failing either check blocks the whole file: a snapshot missing a student looks
complete, and `collect` would take no commit at all for them.

A student with no activity in the window is different, and does not block the
file. There is simply no commit to pin, so they are named in the report, left
out of the file, and reported by `collect` as `skipped (not in the snapshot)`.

Do not build such a record on the [events
API](https://docs.github.com/en/rest/activity/events) (`/repos/{owner}/{repo}/events`)
instead. GitHub documents that one as unsuitable: it retains only 30 days,
returns at most 300 events, and states that it "is not built to serve real-time
use cases" with latency "anywhere from 30s to 6h".

Collect only reads from GitHub (it lists repos, then clones and fetches), so
unlike `assign`, `freeze` and `audit --renew` it never competes with another
`gh cls` command for the same state. Running it while a freeze is in progress
cannot corrupt the freeze; it just collects a moving target. See the concurrency
warning in [README.md](README.md) for the commands that do conflict.

Two collects into the same `--out` are the one collision that matters, since they
would race on the clones and the tags and could each append the same manifest
rows. That one is prevented rather than warned about: a run claims
`<out>/.gh-cls/lock` for its duration, and a second run into that directory stops
at once, printing the host, process and start time of the run that holds it.
Collecting into two different directories at the same time is fine. If a run was
killed or the machine restarted, the claim is left behind; the refusal says so
and names the file to delete.

## Force-pushes are safe, and you are warned

A student may rewrite history with a force-push (unless you used branch
protection). Collect handles this without losing anything: it never tries a
fast-forward merge, it just takes the target commit and tags it. When an update's
upstream history was rewritten since your last collect, collect prints a warning
naming the repo, then proceeds. Your earlier collected commit is still tagged.

## Your work in a clone is protected

Nothing you have done inside a clone is silently discarded. Which of four things
you did decides what happens, and the run names the one you hit:

- **You modified tracked files**, usually because a grading script patched the
  submission. The clone is left exactly as it is and reported `skipped (local
  changes)`. Undo the edits when you are ready (`git restore .`, below) and
  re-collect.
- **You left untracked or ignored files** behind, such as build output or a
  results file you wrote. These do not hold up a collection. The repository is
  collected and the run notes that your files are still sitting in the worktree.
- **One of your files is at a path the new commit tracks.** Checking out would
  have to overwrite it, so collect does not check out at all and reports `skipped
  (files in the way)` with the paths. Move or delete them and re-collect.
- **You committed in the clone and no branch or tag holds that commit**, which
  happens when you commit on a detached HEAD. Moving HEAD would leave your commit
  reachable only through the reflog, which expires, so collect stops and reports
  `skipped (HEAD held by no ref)` along with the `git tag` command that keeps it.
  Tag it, or commit on a branch, and the next run collects normally.

## Reconciliation against the class

Collect collects every `<name>-*` repository that exists, and uses your roster (or
groups file) to tell you whether that set matches the class:

- **missing:** a student or group with no repository. Reported, since there is
  nothing to clone.
- **unexpected:** a repository that matches no roster or groups entry, perhaps a
  typo or a dropped student. It is still collected, but reported so you notice.

## What a run reports

The header names the assignment, the output directory, the tag this run writes,
and the history setting with where it came from. Then one line per repository as
it finishes, and at the end a count of each outcome: collected, updated,
up-to-date, skipped, refused, failed.

On a class-sized run the per-repo lines have scrolled away by the time it ends,
and a repository the run passed over still has code in it from last time, so
opening the directory cannot tell you it was passed over. Two lists after the
counts say so explicitly.

**Not collected under `<label>`** names every repository this run did not put
under the label, each with its reason and the fix. Besides the four in **Your
work in a clone is protected**, you may see `skipped (not in the snapshot)`, for
a key the snapshot file has no SHA for, and `skipped (empty repository)`, for a
student who has pushed nothing: no directory is made for them, so a later run
collects cleanly once they do. It also names anything `refused`, which is
collect declining to make something
permanently wrong rather than failing to do the work: the label already holds a
different commit for that repository, the directory is a clone of some other
repo, or what is there is not a clone it can use. Refusals point at a mistake
only you can correct, so a run with any of them exits non-zero, as does one with
outright failures. A run whose repositories were merely skipped exits zero.

**Collected, with something to note** names repositories that were collected but
where something is worth knowing: the student rewrote history since an earlier
collection, your untracked files are still in the worktree, or the clone holds
more history than this directory's setting.

## The manifest

`<out>/collected.csv` records every collected commit (`label, key, repo, sha, ref,
time`). It is the quick answer to "what SHA did I grade for this student," without
opening each clone. It is appended to, never overwritten.

Tags are written per repository as a run proceeds, but the manifest is written at
the end, so a run that is interrupted can leave repositories collected and
unrecorded. Re-running under the same label fills those rows in from each repo's
tag, and never writes a row twice, so the manifest ends up complete whether or not
a run finished. A row filled in by a later run carries that run's timestamp, since
the moment of the original collection is not recoverable; the `sha` is the
collected one either way.

## Pairing with `gh cls feedback`

Collect writes working copies; `feedback` reads a separate directory of feedback
files named `<key>.md`. A typical flow:

```sh
gh cls collect hw1 --roster roster.csv --out ./submissions
# read ./submissions/<key>/, write ./feedback/<key>.md
gh cls feedback hw1 --roster roster.csv --dir ./feedback
```

Keep the two directories separate so neither command trips over the other's files.

## The git you may want (cheat-sheet)

Everything below is plain git you run yourself inside a collected clone. Collect
does not need any of it; these are for when you want more than the snapshot.

- **Undo grading-script patches** (restore tracked files to the collected commit):
  ```sh
  cd hw1/ada && git restore .
  ```
- **See an earlier collection** you took under another label:
  ```sh
  git checkout gh-cls/collect/midterm   # detached; the midterm state
  git checkout -                        # back to where you were
  ```
- **List the collections in a clone:**
  ```sh
  git tag --list 'gh-cls/collect/*'
  ```
- **Get the full history.** For the whole set, collect with `--history full`,
  which is recorded for the directory and applies to later runs too. For one
  repo, by hand:
  ```sh
  git fetch --unshallow      # all history
  git fetch --depth=50       # or just deepen by N commits
  ```
  A clone you deepen by hand keeps its history: collect notices and leaves it
  alone, mentioning that it holds more than the directory's setting.
- **Get back to a branch.** A collected clone is parked on a commit, not a
  branch, and branch-name guessing is off, so `git checkout main` reports that
  there is no such branch rather than quietly building one from `origin/main`
  and showing you code that was never collected. What to use instead:
  ```sh
  git checkout gh-cls/collect/<label>   # the state that was collected
  git switch -c main origin/main        # a real branch, in a --history full clone
  ```
  A `--history full` clone tracks every branch, so `git checkout origin/main`
  works there. A snapshot clone holds only the collected commit, so it has no
  `origin/*` to check out.

  In a full clone, `origin/*` is GitHub's branch state as of that run's fetch,
  not the state you collected: in a pinned collection it can hold pushes made
  after the deadline, and the run's header says so. Grade from
  `gh-cls/collect/<label>` and read `origin/main` as "what is on GitHub now".
- **What commit am I on:**
  ```sh
  git rev-parse HEAD
  ```

Because each clone is a normal git repository, shallow or not, any other git
command works too. Collect just gives you the starting point and never gets in
your way: it leaves a clone with modified tracked files alone, refuses rather
than overwrite a file of yours that a new commit also tracks, and will not move
off a commit you made in the clone that no branch or tag holds.
