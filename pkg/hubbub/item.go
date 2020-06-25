// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hubbub

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v31/github"
	"k8s.io/klog/v2"

	"github.com/google/triage-party/pkg/tag"
)

var (
	// wordRelRefRe parses relative issue references, like "fixes #3402"
	wordRelRefRe = regexp.MustCompile(`\s#(\d+)\b`)

	// puncRelRefRe parses relative issue references, like "fixes #3402."
	puncRelRefRe = regexp.MustCompile(`\s\#(\d+)[\.\!:\?]`)

	// absRefRe parses absolute issue references, like "fixes http://github.com/minikube/issues/432"
	absRefRe = regexp.MustCompile(`https*://github.com/(\w+)/(\w+)/[ip][us]\w+/(\d+)`)

	// codeRe matches code
	codeRe    = regexp.MustCompile("(?s)```.*?```")
	detailsRe = regexp.MustCompile(`(?s)<details>.*</details>`)
)

// GitHubItem is an interface that matches both GitHub Issues and PullRequests
type GitHubItem interface {
	GetAssignee() *github.User
	GetAuthorAssociation() string
	GetBody() string
	GetComments() int
	GetHTMLURL() string
	GetCreatedAt() time.Time
	GetID() int64
	GetMilestone() *github.Milestone
	GetNumber() int
	GetClosedAt() time.Time
	GetState() string
	GetTitle() string
	GetURL() string
	GetUpdatedAt() time.Time
	GetUser() *github.User
	String() string
}

// conversation creates a conversation from an issue-like
func (h *Engine) conversation(i GitHubItem, cs []*Comment, age time.Time) *Conversation {
	authorIsMember := false
	if h.isMember(i.GetUser().GetLogin(), i.GetAuthorAssociation()) {
		authorIsMember = true
	}

	co := &Conversation{
		ID:                   i.GetNumber(),
		URL:                  i.GetHTMLURL(),
		Author:               i.GetUser(),
		Title:                i.GetTitle(),
		State:                i.GetState(),
		Type:                 Issue,
		Seen:                 age,
		Created:              i.GetCreatedAt(),
		CommentsTotal:        i.GetComments(),
		ClosedAt:             i.GetClosedAt(),
		SelfInflicted:        authorIsMember,
		LatestAuthorResponse: i.GetCreatedAt(),
		Milestone:            i.GetMilestone(),
		Reactions:            map[string]int{},
		LastCommentAuthor:    i.GetUser(),
		LastCommentBody:      i.GetBody(),
	}

	// "https://github.com/kubernetes/minikube/issues/7179",
	urlParts := strings.Split(i.GetHTMLURL(), "/")
	co.Organization = urlParts[3]
	co.Project = urlParts[4]
	h.parseRefs(i.GetBody(), co, i.GetUpdatedAt())

	if i.GetAssignee() != nil {
		co.Assignees = append(co.Assignees, i.GetAssignee())
		co.Tags = append(co.Tags, tag.Assigned)
	}

	if !authorIsMember {
		co.LatestMemberResponse = i.GetCreatedAt()
	}

	lastQuestion := time.Time{}
	seenCommenters := map[string]bool{}
	seenClosedCommenters := map[string]bool{}
	seenMemberComment := false

	if h.debug[co.ID] {
		klog.Errorf("debug conversation: %s", formatStruct(co))
	}

	for _, c := range cs {
		h.parseRefs(c.Body, co, c.Updated)
		if h.debug[co.ID] {
			klog.Errorf("debug conversation comment: %s", formatStruct(c))
		}

		// We don't like their kind around here
		if isBot(c.User) {
			continue
		}

		co.LastCommentBody = c.Body
		co.LastCommentAuthor = c.User

		r := c.Reactions
		if r.GetTotalCount() > 0 {
			co.ReactionsTotal += r.GetTotalCount()
			for k, v := range reactions(r) {
				co.Reactions[k] += v
			}
		}

		if !i.GetClosedAt().IsZero() && c.Created.After(i.GetClosedAt().Add(30*time.Second)) {
			klog.V(1).Infof("#%d: comment after closed on %s: %+v", co.ID, i.GetClosedAt(), c)
			co.ClosedCommentsTotal++
			seenClosedCommenters[*c.User.Login] = true
		}

		if c.User.GetLogin() == i.GetUser().GetLogin() {
			co.LatestAuthorResponse = c.Created
		}

		if c.User.GetLogin() == i.GetAssignee().GetLogin() {
			co.LatestAssigneeResponse = c.Created
		}

		if h.isMember(c.User.GetLogin(), c.AuthorAssoc) && !isBot(c.User) {
			if !co.LatestMemberResponse.After(co.LatestAuthorResponse) && !authorIsMember {
				co.AccumulatedHoldTime += c.Created.Sub(co.LatestAuthorResponse)
			}
			co.LatestMemberResponse = c.Created
			if !seenMemberComment {
				co.Tags = append(co.Tags, tag.Commented)
				seenMemberComment = true
			}
		}

		if strings.Contains(c.Body, "?") {
			for _, line := range strings.Split(c.Body, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, ">") {
					continue
				}
				if strings.Contains(line, "?") {
					klog.V(2).Infof("question at %s: %s", c.Created, line)
					lastQuestion = c.Created
				}
			}
		}

		if !seenCommenters[*c.User.Login] {
			co.Commenters = append(co.Commenters, c.User)
			seenCommenters[*c.User.Login] = true
		}
	}

	if co.LatestMemberResponse.After(co.LatestAuthorResponse) {
		klog.V(2).Infof("marking as send: latest member response (%s) is after latest author response (%s)", co.LatestMemberResponse, co.LatestAuthorResponse)
		co.Tags = append(co.Tags, tag.Send)
		co.CurrentHoldTime = 0
	} else if !authorIsMember {
		klog.V(2).Infof("marking as recv: author is not member, latest member response (%s) is before latest author response (%s)", co.LatestMemberResponse, co.LatestAuthorResponse)
		co.Tags = append(co.Tags, tag.Recv)
		co.CurrentHoldTime += time.Since(co.LatestAuthorResponse)
		co.AccumulatedHoldTime += time.Since(co.LatestAuthorResponse)
	}

	if lastQuestion.After(co.LatestMemberResponse) {
		klog.V(2).Infof("marking as recv-q: last question (%s) comes after last member response (%s)", lastQuestion, co.LatestMemberResponse)
		co.Tags = append(co.Tags, tag.RecvQ)
	}

	if co.Milestone != nil && co.Milestone.GetState() == "open" {
		co.Tags = append(co.Tags, tag.OpenMilestone)
	}

	if !co.LatestAssigneeResponse.IsZero() {
		co.Tags = append(co.Tags, tag.AssigneeUpdated)
	}

	if len(cs) > 0 {
		last := cs[len(cs)-1]
		assoc := strings.ToLower(last.AuthorAssoc)
		if assoc == "none" {
			if last.User.GetLogin() == i.GetUser().GetLogin() {
				co.Tags = append(co.Tags, tag.AuthorLast)
			}
		} else {
			co.Tags = append(co.Tags, tag.RoleLast(assoc))
		}
		co.Updated = last.Updated
	}

	if co.State == "closed" {
		co.Tags = append(co.Tags, tag.Closed)
	}

	co.CommentersTotal = len(seenCommenters)
	co.ClosedCommentersTotal = len(seenClosedCommenters)

	if co.AccumulatedHoldTime > time.Since(co.Created) {
		panic(fmt.Sprintf("accumulated %s is more than age %s", co.AccumulatedHoldTime, time.Since(co.Created)))
	}

	// Loose, but good enough
	months := time.Since(co.Created).Hours() / 24 / 30
	co.CommentersPerMonth = float64(co.CommentersTotal) / months
	co.ReactionsPerMonth = float64(co.ReactionsTotal) / months
	return co
}

// Return if a user or role should be considered a member
func (h *Engine) isMember(user string, role string) bool {
	if h.members[user] {
		klog.V(3).Infof("%q (%s) is in membership list", user, role)
		return true
	}

	if h.memberRoles[strings.ToLower(role)] {
		klog.V(3).Infof("%q (%s) is in membership role list", user, role)
		return true
	}

	return false
}

// parse any references and update mention time
func (h *Engine) parseRefs(text string, co *Conversation, t time.Time) {

	// remove code samples which mention unrelated issues
	text = codeRe.ReplaceAllString(text, "<code></code>")
	text = detailsRe.ReplaceAllString(text, "<details></details>")

	var ms [][]string
	ms = append(ms, wordRelRefRe.FindAllStringSubmatch(text, -1)...)
	ms = append(ms, puncRelRefRe.FindAllStringSubmatch(text, -1)...)

	seen := map[string]bool{}

	for _, m := range ms {
		i, err := strconv.Atoi(m[1])
		if err != nil {
			klog.Errorf("unable to parse int from %s: %v", err)
			continue
		}

		if i == co.ID {
			continue
		}

		rc := &RelatedConversation{
			Organization: co.Organization,
			Project:      co.Project,
			ID:           i,
			Seen:         t,
		}

		if t.After(h.mtimeRef(rc)) {
			klog.Infof("%s later referenced #%d at %s: %s", co.URL, i, t, text)
			h.updateMtimeLong(co.Organization, co.Project, i, t)
		}

		if !seen[fmt.Sprintf("%s/%d", rc.Project, rc.ID)] {
			co.IssueRefs = append(co.IssueRefs, rc)
		}
		seen[fmt.Sprintf("%s/%d", rc.Project, rc.ID)] = true
	}

	for _, m := range absRefRe.FindAllStringSubmatch(text, -1) {
		org := m[1]
		project := m[2]
		i, err := strconv.Atoi(m[3])
		if err != nil {
			klog.Errorf("unable to parse int from %s: %v", err)
			continue
		}

		if i == co.ID && org == co.Organization && project == co.Project {
			continue
		}

		rc := &RelatedConversation{
			Organization: org,
			Project:      project,
			ID:           i,
			Seen:         t,
		}

		if t.After(h.mtimeRef(rc)) {
			klog.Infof("%s later referenced %s/%s #%d at %s: %s", co.URL, org, project, i, t, text)
			h.updateMtimeLong(org, project, i, t)
		}

		if !seen[fmt.Sprintf("%s/%d", rc.Project, rc.ID)] {
			co.IssueRefs = append(co.IssueRefs, rc)
		}
		seen[fmt.Sprintf("%s/%d", rc.Project, rc.ID)] = true
	}
}
