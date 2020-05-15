package bugzilla

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/golang/glog"
	"k8s.io/test-infra/prow/bugzilla"
	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/plugins"
)

// Verifier takes a list of bugzilla bugs and uses the Bugzilla client to
// retrieve the associated GitHub PR via the bugzilla bug's external bug links.
// It then uses the github client to read the comments of the associated PR to
// determine whether the bug's QA Contact reviewed the GitHub PR. If yes, the bug
// gets marked as VERIFIED in Bugzilla.
type Verifier struct {
	// bzClient is used to retrieve external bug links and mark QA reviewed bugs as VERIFIED
	bzClient bugzilla.Client
	// ghClient is used to retrieve comments on a bug's PR
	ghClient github.Client
	// pluginConfig is used to check whether a repository allows approving reviews as LGTM
	pluginConfig *plugins.Configuration
}

// NewVerifier returns a Verifier configured with the provided github and bugzilla clients and the provided pluginConfig
func NewVerifier(bzClient bugzilla.Client, ghClient github.Client, pluginConfig *plugins.Configuration) *Verifier {
	return &Verifier{
		bzClient:     bzClient,
		ghClient:     ghClient,
		pluginConfig: pluginConfig,
	}
}

// pr contains a bugzilla bug ID and the associated GitHub pr that resolves the bug
type pr struct {
	bugID int
	org   string
	repo  string
	prNum int
}

var (
	// bzAssignRegex matches the QA assignment comment made by the openshift-ci-robot
	bzAssignRegex = regexp.MustCompile(`Requesting review from QA contact:[[:space:]]+/cc @[[:alnum:]]+`)
	// from prow lgtm plugin
	lgtmRe       = regexp.MustCompile(`(?mi)^/lgtm(?: no-issue)?\s*$`)
	lgtmCancelRe = regexp.MustCompile(`(?mi)^/lgtm cancel\s*$`)
)

// VerifyBugs takes a list of bugzilla bug IDs and for each bug changes the bug status to VERIFIED if bug was reviewed and
// lgtm'd by the bug's QA Contect
func (c *Verifier) VerifyBugs(bugs []string) []error {
	bzPRs, errs := getPRs(bugs, c.bzClient)
	for _, bzp := range bzPRs {
		bug, err := c.bzClient.GetBug(bzp.bugID)
		if err != nil {
			errs = append(errs, fmt.Errorf("Unable to get bugzilla number %d: %v", bzp.bugID, err))
			continue
		}
		comments, err := c.ghClient.ListIssueComments(bzp.org, bzp.repo, bzp.prNum)
		if err != nil {
			errs = append(errs, fmt.Errorf("Unable to get comments for github pull %s/%s#%d: %v", bzp.org, bzp.repo, bzp.prNum, err))
			continue
		}
		var reviews []github.Review
		if c.pluginConfig.LgtmFor(bzp.org, bzp.repo).ReviewActsAsLgtm {
			reviews, err = c.ghClient.ListReviews(bzp.org, bzp.repo, bzp.prNum)
			if err != nil {
				errs = append(errs, fmt.Errorf("Unable to get reviews for github pull %s/%s#%d: %v", bzp.org, bzp.repo, bzp.prNum, err))
				continue
			}
		}
		approved := prReviewedByQA(comments, reviews)
		if approved {
			glog.V(4).Infof("Bug %d (current status %s) should be moved to VERIFIED state", bug.ID, bug.Status)
			// once this is proven to work correctly in-cluster, add code to update bugzilla bug state to VERIFIED
		} else {
			glog.V(4).Infof("Bug %d (current status %s) not approved by QA contact", bug.ID, bug.Status)
		}
	}
	return errs
}

// getPRs identifies bugzilla bugs and the associated github PRs fixed in a release from
// a given buglist generated by `oc adm release info --bugs=git-cache-path --ouptut=name from-tag to-tag`
func getPRs(input []string, bzClient bugzilla.Client) ([]pr, []error) {
	var bzPRs []pr
	var errs []error
	for _, bzID := range input {
		bzInt, err := strconv.Atoi(bzID)
		if err != nil {
			errs = append(errs, fmt.Errorf("Failed to convert bugzilla ID %s to integer: %v", bzID, err))
			continue
		}
		extBugs, err := bzClient.GetExternalBugPRsOnBug(bzInt)
		if err != nil {
			errs = append(errs, fmt.Errorf("Failed to get external bugs for bugzilla bug %d: %v", bzInt, err))
			continue
		}
		foundPR := false
		for _, extBug := range extBugs {
			if extBug.Type.URL == "https://github.com/" {
				bzPRs = append(bzPRs, pr{
					bugID: bzInt,
					org:   extBug.Org,
					repo:  extBug.Repo,
					prNum: extBug.Num,
				})
				foundPR = true
				break
			}
		}
		if !foundPR {
			errs = append(errs, fmt.Errorf("failed to identify associated GitHub PR for bugzilla bug %d", bzInt))
		}
	}
	return bzPRs, errs
}

// prReviewedByQA looks through PR comments and identifies if an assigned
// QA contact lgtm'd the PR
func prReviewedByQA(comments []github.IssueComment, reviews []github.Review) bool {
	var lgtms, qaContacts []string
	for _, comment := range comments {
		if lgtmRe.MatchString(comment.Body) {
			lgtms = append(lgtms, comment.User.Login)
			continue
		}
		if lgtmCancelRe.MatchString(comment.Body) {
			for index, name := range lgtms {
				if name == comment.User.Login {
					lgtms = append(lgtms[:index], lgtms[index+1:]...)
					break
				}
			}
			continue
		}
		bz := bzAssignRegex.FindString(comment.Body)
		if bz != "" {
			splitbz := strings.Split(bz, "@")
			if len(splitbz) == 2 {
				qaContacts = append(qaContacts, splitbz[1])
			}
		}
	}
	for _, review := range reviews {
		if review.State == github.ReviewStateApproved || lgtmRe.MatchString(review.Body) {
			lgtms = append(lgtms, review.User.Login)
			continue
		}
		if review.State == github.ReviewStateChangesRequested || lgtmCancelRe.MatchString(review.Body) {
			for index, name := range lgtms {
				if name == review.User.Login {
					lgtms = append(lgtms[:index], lgtms[index+1:]...)
					break
				}
			}
			continue
		}
	}
	for _, contact := range qaContacts {
		for _, lgtm := range lgtms {
			if contact == lgtm {
				glog.V(4).Infof("QA Contact %s lgtm'd this PR", contact)
				return true
			}
		}
	}
	return false
}
