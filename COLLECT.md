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
```

`--out` is required on purpose, so repositories are never cloned into a surprise
location.

## The model: one shallow clone per student, tagged each time

Each `<out>/<key>` is a real git clone, but a **shallow** one: it contains the
files at the collected commit, not the student's entire history. That keeps disk
use small even when students have committed large binaries over the term.

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
regardless of anything pushed afterward. `gh cls activity -p` writes that file
from GitHub's own record of when each push landed:

```sh
gh cls activity hw1 -p -u 2026-03-01T23:59:59-06:00 -o deadline.yml
```

The file is just a mapping, so you can also write it by hand or edit one to give
a student a later commit:

```yaml
# deadline.yml
ada:        9f3a2b1c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a
alan:       1c4d77e0...
group-alpha: a0b1c2d3...
```

```sh
gh cls collect hw1 --roster roster.csv --out ./hw1-final --commits deadline.yml --label final
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

**2. `--commits` from push events: consistent deadline, silent cutoff.** Take
each repo's SHA from the push events at the deadline instant and every student is
cut at exactly the same moment, with no window at all. Nothing tells the student:
a late push succeeds, they watch it land, and they learn only when grades come
back that it was not the commit you graded.

**3. Unpinned collect, no freeze: neither.** Each repo's target is its
default-branch tip as of the moment collect reaches that repo, and collect works
through repos concurrently (`-j`), so the cut is smeared across the run. The
student gets no signal either: their push succeeds whether or not it was
collected, and whether it counted comes down to the order collect walked the
class. That is the smear of (1) with the silence of (2).

`gh cls activity -p` produces option (2)'s input for you:

```sh
gh cls activity hw1 -p -u 2026-03-01T23:59:59-06:00 -o deadline.yml
gh cls collect hw1 --roster roster.csv --out ./hw1-final --commits deadline.yml --label final
```

It reads GitHub's own record of ref changes, takes each repo's commit as of
`--until`, and writes the pin file. The timestamps are GitHub's server-side
record of when each push landed, so they are neither commit dates (which the
pusher controls and can backdate) nor webhook receipt times (which trail the
push, and by far more when GitHub retries a failed delivery).

That record is [`/repos/{owner}/{repo}/activity`](https://docs.github.com/en/rest/repos/repos#list-repository-activities),
for which GitHub documents no retention, completeness or latency guarantee. So
`-p` verifies rather than assumes. It refuses to write a pin file if GitHub's
record has not yet caught up with a branch's current tip, which is how a lagging
record is caught instead of silently yielding an earlier commit, and it refuses
to pin any commit that is no longer retrievable. Both fail the run rather than
handing back an artifact that would break on collection day.

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

## Force-pushes are safe, and you are warned

A student may rewrite history with a force-push (unless you used branch
protection). Collect handles this without losing anything: it never tries a
fast-forward merge, it just takes the target commit and tags it. When an update's
upstream history was rewritten since your last collect, collect prints a warning
naming the repo, then proceeds. Your earlier collected commit is still tagged.

## Your local edits are protected

If a clone has uncommitted changes in its working tree, collect refuses to touch
it and reports it as skipped. This is deliberate: if your grading scripts patch a
submission, your edits are never silently discarded. Undo the edits yourself when
you are ready (`git restore .`, below) and re-collect.

## Reconciliation against the class

Collect collects every `<name>-*` repository that exists, and uses your roster (or
groups file) to tell you whether that set matches the class:

- **missing:** a student or group with no repository. Reported, since there is
  nothing to clone.
- **unexpected:** a repository that matches no roster or groups entry, perhaps a
  typo or a dropped student. It is still collected, but reported so you notice.

## The manifest

`<out>/collected.csv` records every collected commit (`label, key, repo, sha, ref,
time`). It is the quick answer to "what SHA did I grade for this student," without
opening each clone. It is appended to, never overwritten.

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
- **Get the full history** of one repo if a shallow copy is not enough:
  ```sh
  git fetch --unshallow      # all history
  git fetch --depth=50       # or just deepen by N commits
  ```
- **What commit am I on:**
  ```sh
  git rev-parse HEAD
  ```

Because each clone is a normal (if shallow) git repository, any other git command
works too. Collect just gives you the starting point and never gets in your way.
