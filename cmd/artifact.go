package cmd

import (
	"context"
	"fmt"

	"github.com/rixner/gh-cls/config"
)

// Feedback artifact identity: shared by assign (which creates it), status
// (which reports its state), and feedback (which posts to it).
const (
	feedbackBranch    = "feedback"
	feedbackTitle     = "Feedback"
	feedbackPRBody    = "This pull request is where the course staff leaves feedback on your work. Please do not close it. Your whole project appears in the diff here, so staff can comment on any line, and it updates automatically as you push to the default branch."
	feedbackIssueBody = "This issue is where the course staff leaves feedback on your work. Please do not close it."
)

// artifactFinder is the narrow lookup both status and feedback need to locate
// a repo's feedback artifact; statusClient and feedbackClient both satisfy it.
type artifactFinder interface {
	FindIssueByTitle(ctx context.Context, owner, repo, title string) (int, string, bool, error)
	FindPRByBase(ctx context.Context, owner, repo, base string) (int, string, bool, error)
}

// artifact is the feedback artifact a repository actually carries.
type artifact struct {
	mode   string // config.FeedbackPR or config.FeedbackIssue
	number int
	state  string // "open" or "closed"
}

// findExisting reports which feedback artifact a repository actually carries, by
// asking for both kinds rather than trusting the configured mode.
//
// The config declares what assign should create; it is not a record of what was
// created. The two diverge as soon as an assignment's feedback setting is changed
// after its repos exist, and a mode taken on faith then points every command at
// an artifact that is not there while the real one goes unread. Two lookups per
// repo is the price of answering from the repository instead.
//
// A repo carrying both is an error rather than a guess: picking one would send
// half a class's grades somewhere the students are not reading.
func findExisting(ctx context.Context, client artifactFinder, org, repo string) (artifact, bool, error) {
	prNum, prState, hasPR, err := client.FindPRByBase(ctx, org, repo, feedbackBranch)
	if err != nil {
		return artifact{}, false, err
	}
	issueNum, issueState, hasIssue, err := client.FindIssueByTitle(ctx, org, repo, feedbackTitle)
	if err != nil {
		return artifact{}, false, err
	}
	switch {
	case hasPR && hasIssue:
		return artifact{}, false, fmt.Errorf("%s carries both a feedback pull request (#%d) and a feedback issue (#%d), so there is no telling which one the students are reading; close or delete whichever is not in use", repo, prNum, issueNum)
	case hasPR:
		return artifact{mode: config.FeedbackPR, number: prNum, state: prState}, true, nil
	case hasIssue:
		return artifact{mode: config.FeedbackIssue, number: issueNum, state: issueState}, true, nil
	}
	return artifact{}, false, nil
}

// findArtifact returns the number of the repo's feedback artifact for the
// mode. The artifact state is unused here (posting a comment works on any
// state).
func findArtifact(ctx context.Context, client artifactFinder, org, repo, mode string) (int, bool, error) {
	switch mode {
	case config.FeedbackIssue:
		n, _, found, err := client.FindIssueByTitle(ctx, org, repo, feedbackTitle)
		return n, found, err
	case config.FeedbackPR:
		n, _, found, err := client.FindPRByBase(ctx, org, repo, feedbackBranch)
		return n, found, err
	default:
		return 0, false, fmt.Errorf("unknown feedback mode %q", mode)
	}
}

// feedbackArtifactState reports the state of a repo's feedback artifact for
// the assignment's mode: open/closed, "missing" if the artifact is absent, or
// "none" when the assignment configures no feedback.
func feedbackArtifactState(ctx context.Context, client artifactFinder, org, repo, mode string) (string, error) {
	switch mode {
	case config.FeedbackIssue:
		_, state, found, err := client.FindIssueByTitle(ctx, org, repo, feedbackTitle)
		if err != nil {
			return "", err
		}
		if !found {
			return "missing", nil
		}
		return state, nil
	case config.FeedbackPR:
		_, state, found, err := client.FindPRByBase(ctx, org, repo, feedbackBranch)
		if err != nil {
			return "", err
		}
		if !found {
			return "missing", nil
		}
		return state, nil
	default:
		return "none", nil
	}
}

// artifactNoun names the feedback artifact for the mode, for messages.
func artifactNoun(mode string) string {
	if mode == config.FeedbackPR {
		return "pull request"
	}
	return "issue"
}
