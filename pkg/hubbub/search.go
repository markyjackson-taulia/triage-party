package hubbub

import (
	"github.com/google/triage-party/pkg/constants"
	"github.com/google/triage-party/pkg/interfaces"
	"github.com/google/triage-party/pkg/models"
	"strings"
	"sync"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/hokaccha/go-prettyjson"

	"github.com/google/triage-party/pkg/logu"
	"github.com/google/triage-party/pkg/tag"
	"k8s.io/klog/v2"
)

// Search for GitHub issues or PR's
func (h *Engine) SearchAny(sp models.SearchParams) ([]*Conversation, time.Time, error) {
	cs, ts, err := h.SearchIssues(sp)
	if err != nil {
		return cs, ts, err
	}

	pcs, pts, err := h.SearchPullRequests(sp)
	if err != nil {
		return cs, ts, err
	}

	if pts.After(ts) {
		ts = pts
	}

	return append(cs, pcs...), ts, nil
}

// Search for GitHub issues or PR's
func (h *Engine) SearchIssues(sp models.SearchParams) ([]*Conversation, time.Time, error) {
	sp.Filters = openByDefault(sp.Filters)
	klog.V(1).Infof(
		"Gathering raw data for %s/%s search %s - newer than %s",
		sp.Repo.Organization,
		sp.Repo.Project,
		sp.Filters,
		logu.STime(sp.NewerThan),
	)
	var wg sync.WaitGroup

	var open []*models.Issue
	var closed []*models.Issue
	var err error

	age := time.Now()

	wg.Add(1)
	go func() {
		defer wg.Done()

		sp.State = constants.OpenState

		oi, ots, err := h.cachedIssues(sp)
		if err != nil {
			klog.Errorf("open issues: %v", err)
			return
		}
		if ots.Before(age) {
			age = ots
		}
		open = oi
		klog.V(1).Infof("%s/%s open issue count: %d", sp.Repo.Organization, sp.Repo.Project, len(open))
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if !NeedsClosed(sp.Filters) {
			return
		}

		sp.State = constants.ClosedState
		sp.UpdateAge = h.MaxClosedUpdateAge

		ci, cts, err := h.cachedIssues(sp)
		if err != nil {
			klog.Errorf("closed issues: %v", err)
		}

		if cts.Before(age) {
			age = cts
		}
		closed = ci

		klog.V(1).Infof("%s/%s closed issue count: %d", sp.Repo.Organization, sp.Repo.Project, len(closed))
	}()

	wg.Wait()

	var is []*models.Issue
	seen := map[string]bool{}

	for _, i := range append(open, closed...) {
		if len(h.debug) > 0 {
			klog.Infof("DEBUG FILTER: %s", h.debug)
			if h.debug[i.GetNumber()] {
				klog.Errorf("*** Found debug issue #%d:\n%s", i.GetNumber(), formatStruct(i))
			} else {
				continue
			}
		}

		if seen[i.GetURL()] {
			klog.Errorf("unusual: I already saw #%d", i.GetURL())
			continue
		}
		seen[i.GetURL()] = true
		is = append(is, i)
	}

	var filtered []*Conversation
	klog.V(1).Infof("%s/%s aggregate issue count: %d, filtering for:\n%s", sp.Repo.Organization, sp.Repo.Project, len(is), sp.Filters)

	// Avoids updating PR references on a quiet repository
	mostRecentUpdate := time.Time{}
	for _, i := range is {
		if i.GetUpdatedAt().After(mostRecentUpdate) {
			mostRecentUpdate = i.GetUpdatedAt()
		}
	}

	for _, i := range is {
		// Inconsistency warning: issues use a list of labels, prs a list of label pointers
		labels := []*models.Label{}
		for _, l := range i.Labels {
			l := l
			labels = append(labels, l)
		}

		if !preFetchMatch(i, labels, sp.Filters) {
			klog.V(1).Infof("#%d - %q did not match item filter: %s", i.GetNumber(), i.GetTitle(), sp.Filters)
			continue
		}

		klog.V(1).Infof("#%d - %q made it past pre-fetch: %s", i.GetNumber(), i.GetTitle(), sp.Filters)

		comments := []*models.IssueComment{}

		if needComments(i, sp.Filters) && i.GetComments() > 0 {
			klog.V(1).Infof("#%d - %q: need comments for final filtering", i.GetNumber(), i.GetTitle())

			sp.IssueNumber = i.GetNumber()
			sp.NewerThan = h.mtime(i)
			sp.Fetch = !sp.NewerThan.IsZero()

			comments, _, err = h.cachedIssueComments(sp)
			if err != nil {
				klog.Errorf("comments: %v", err)
			}
		}

		co := h.IssueSummary(i, comments, age)
		co.Labels = labels

		co.Similar = h.FindSimilar(co)
		if len(co.Similar) > 0 {
			co.Tags = append(co.Tags, tag.Similar)
		}

		if !postFetchMatch(co, sp.Filters) {
			klog.V(1).Infof("#%d - %q did not match post-fetch filter: %s", i.GetNumber(), i.GetTitle(), sp.Filters)
			continue
		}
		klog.V(1).Infof("#%d - %q made it past post-fetch: %s", i.GetNumber(), i.GetTitle(), sp.Filters)

		sp.UpdateAt = h.mtime(i)
		var timeline []*models.Timeline
		if needTimeline(i, sp.Filters, false, sp.Hidden) {

			sp.IssueNumber = i.GetNumber()
			sp.Fetch = !sp.NewerThan.IsZero()

			timeline, err = h.cachedTimeline(sp)
			if err != nil {
				klog.Errorf("timeline: %v", err)
				continue
			}
		}

		sp.Fetch = !sp.NewerThan.IsZero()

		h.addEvents(sp, co, timeline)

		// Some labels are judged by linked PR state. Ensure that they are updated to the same timestamp.
		if needReviews(i, sp.Filters, sp.Hidden) && len(co.PullRequestRefs) > 0 {

			sp.NewerThan = mostRecentUpdate
			sp.Fetch = !sp.NewerThan.IsZero()

			co.PullRequestRefs = h.updateLinkedPRs(sp, co)
		}

		if !postEventsMatch(co, sp.Filters) {
			klog.V(1).Infof("#%d - %q did not match post-events filter: %s", i.GetNumber(), i.GetTitle(), sp.Filters)
			continue
		}
		klog.V(1).Infof("#%d - %q made it past post-events: %s", i.GetNumber(), i.GetTitle(), sp.Filters)

		filtered = append(filtered, co)
	}

	return filtered, age, nil
}

// NeedsClosed returns whether or not the filters require closed items
func NeedsClosed(fs []Filter) bool {
	// First-pass filter: do any filters require closed data?
	for _, f := range fs {
		if f.ClosedCommenters != "" {
			klog.V(1).Infof("will need closed items due to ClosedCommenters=%s", f.ClosedCommenters)
			return true
		}
		if f.ClosedComments != "" {
			klog.V(1).Infof("will need closed items due to ClosedComments=%s", f.ClosedComments)
			return true
		}
		if f.State != "" && f.State != "open" {
			klog.V(1).Infof("will need closed items due to State=%s", f.State)
			return true
		}
	}
	return false
}

func (h *Engine) SearchPullRequests(sp models.SearchParams) ([]*Conversation, time.Time, error) {
	sp.Filters = openByDefault(sp.Filters)

	klog.V(1).Infof("Searching %s/%s for PR's matching: %s - newer than %s",
		sp.Repo.Organization, sp.Repo.Project, sp.Filters, logu.STime(sp.NewerThan))
	filtered := []*Conversation{}

	var wg sync.WaitGroup

	var open []*models.PullRequest
	var closed []*models.PullRequest
	var err error
	age := time.Now()

	wg.Add(1)
	go func() {
		defer wg.Done()

		sp.State = constants.OpenState
		sp.UpdateAge = 0

		op, ots, err := h.cachedPRs(sp)
		if err != nil {
			klog.Errorf("open prs: %v", err)
			return
		}
		if ots.Before(age) {
			klog.Infof("setting age to %s (open PR count)", ots)
			age = ots
		}
		open = op
		klog.V(1).Infof("open PR count: %d", len(open))
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if !NeedsClosed(sp.Filters) {
			return
		}

		sp.UpdateAge = h.MaxClosedUpdateAge
		sp.State = constants.ClosedState

		cp, cts, err := h.cachedPRs(sp)
		if err != nil {
			klog.Errorf("closed prs: %v", err)
			return
		}

		if cts.Before(age) {
			klog.Infof("setting age to %s (open PR count)", cts)
			age = cts
		}

		closed = cp

		klog.V(1).Infof("closed PR count: %d", len(closed))
	}()

	wg.Wait()

	prs := []*models.PullRequest{}
	for _, pr := range append(open, closed...) {
		if len(h.debug) > 0 {
			if h.debug[pr.GetNumber()] {
				klog.Errorf("*** Found debug PR #%d:\n%s", pr.GetNumber(), formatStruct(*pr))
			} else {
				klog.V(2).Infof("Ignoring #%s - does not match debug filter: %v", pr.GetHTMLURL(), h.debug)
				continue
			}
		}
		prs = append(prs, pr)
	}

	for _, pr := range prs {
		if !preFetchMatch(pr, pr.Labels, sp.Filters) {
			continue
		}

		var timeline []*models.Timeline
		var reviews []*models.PullRequestReview
		var comments []*models.Comment

		if needComments(pr, sp.Filters) {

			sp.IssueNumber = pr.GetNumber()
			sp.NewerThan = h.mtime(pr)
			sp.Fetch = !sp.NewerThan.IsZero()

			comments, _, err = h.prComments(sp)
			if err != nil {
				klog.Errorf("comments: %v", err)
			}
		}

		if needTimeline(pr, sp.Filters, true, sp.Hidden) {

			sp.IssueNumber = pr.GetNumber()
			sp.NewerThan = h.mtime(pr)
			sp.Fetch = !sp.NewerThan.IsZero()

			timeline, err = h.cachedTimeline(sp)
			if err != nil {
				klog.Errorf("timeline: %v", err)
				continue
			}
		}

		if needReviews(pr, sp.Filters, sp.Hidden) {

			sp.IssueNumber = pr.GetNumber()
			sp.NewerThan = h.mtime(pr)
			sp.Fetch = !sp.NewerThan.IsZero()

			reviews, _, err = h.cachedReviews(sp)
			if err != nil {
				klog.Errorf("reviews: %v", err)
				continue
			}
		}

		if h.debug[pr.GetNumber()] {
			klog.Errorf("*** Debug PR timeline #%d:\n%s", pr.GetNumber(), formatStruct(timeline))
		}

		sp.Fetch = !sp.NewerThan.IsZero()
		sp.Age = age

		co := h.PRSummary(sp, pr, comments, timeline, reviews)
		co.Labels = pr.Labels
		co.Similar = h.FindSimilar(co)
		if len(co.Similar) > 0 {
			co.Tags = append(co.Tags, tag.Similar)
		}

		if !postFetchMatch(co, sp.Filters) {
			klog.V(4).Infof("PR #%d did not pass postFetchMatch with filter: %v", pr.GetNumber(), sp.Filters)
			continue
		}

		if !postEventsMatch(co, sp.Filters) {
			klog.V(1).Infof("#%d - %q did not match post-events filter: %s", pr.GetNumber(), pr.GetTitle(), sp.Filters)
			continue
		}

		filtered = append(filtered, co)
	}

	return filtered, age, nil
}

func needComments(i interfaces.IItem, fs []Filter) bool {
	for _, f := range fs {
		if f.TagRegex() != nil {
			if ok, t := matchTag(tag.Tags, f.TagRegex(), f.TagNegate()); ok {
				if t.NeedsComments {
					klog.V(1).Infof("#%d - need comments due to tag %s (negate=%v)", i.GetNumber(), f.TagRegex(), f.TagNegate())
					return true
				}
			}
		}

		if f.ClosedCommenters != "" || f.ClosedComments != "" {
			klog.V(1).Infof("#%d - need comments due to closed comments", i.GetNumber())
			return true
		}

		if f.Responded != "" || f.Commenters != "" {
			klog.V(1).Infof("#%d - need comments due to responded/commenters filter", i.GetNumber())
			return true
		}
	}

	return i.GetState() == "open"
}

func needTimeline(i interfaces.IItem, fs []Filter, pr bool, hidden bool) bool {
	if i.GetMilestone() != nil {
		return true
	}

	if i.GetState() != "open" {
		return false
	}

	if i.GetUpdatedAt() == i.GetCreatedAt() {
		return false
	}

	if pr {
		return true
	}

	for _, f := range fs {
		if f.TagRegex() != nil {
			if ok, t := matchTag(tag.Tags, f.TagRegex(), f.TagNegate()); ok {
				if t.NeedsTimeline {
					return true
				}
			}
		}
		if f.Prioritized != "" {
			return true
		}
	}

	return !hidden
}

func needReviews(i interfaces.IItem, fs []Filter, hidden bool) bool {
	if i.GetState() != "open" {
		return false
	}

	if i.GetUpdatedAt() == i.GetCreatedAt() {
		return false
	}

	if hidden {
		return false
	}

	for _, f := range fs {
		if f.TagRegex() != nil {
			if ok, t := matchTag(tag.Tags, f.TagRegex(), f.TagNegate()); ok {
				if t.NeedsReviews {
					klog.V(1).Infof("#%d - need reviews due to tag %s (negate=%v)", i.GetNumber(), f.TagRegex(), f.TagNegate())
					return true
				}
			}
		}
	}

	return true
}

func formatStruct(x interface{}) string {
	s, err := prettyjson.Marshal(x)
	if err == nil {
		return string(s)
	}
	y := strings.Replace(spew.Sdump(x), "\n", "\n|", -1)
	y = strings.Replace(y, ", ", ",\n - ", -1)
	y = strings.Replace(y, "}, ", "},\n", -1)
	return strings.Replace(y, "},\n - ", "},\n", -1)
}
