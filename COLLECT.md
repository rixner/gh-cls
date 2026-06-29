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

Group assignment (keys are team names, from the teams file; no roster needed):

```sh
gh cls collect project --teams teams.yml --out ./project
```

You get one directory per student or team:

```
hw1/
  ada/          a git clone of hw1-ada at the collected commit
  alan/
  grace/
  collected.csv a record of what was collected (key, repo, SHA, time)
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

If you record each repo's commit at the deadline, give collect a YAML file of
`key: sha` and it checks out exactly those commits, regardless of anything pushed
afterward:

```yaml
# deadline.yml
ada:        9f3a2b1c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a
alan:       1c4d77e0...
team-alpha: a0b1c2d3...
```

```sh
gh cls collect hw1 --roster roster.csv --out ./hw1-final --commits deadline.yml --label final
```

A student with no SHA in the file is skipped and reported, so you grade exactly
the pinned set. This pairs naturally with `gh cls freeze`: once a repo is frozen
at the deadline its tip is read-only, so the deadline commit stays available.

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
teams file) to tell you whether that set matches the class:

- **missing:** a student or team with no repository. Reported, since there is
  nothing to clone.
- **unexpected:** a repository that matches no roster or teams entry, perhaps a
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
