package facts

import (
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/opentalon/talooner/internal/config"
	"github.com/opentalon/talooner/internal/github"
)

var teamNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,99}$`)

type reviewDecision struct {
	bot      bool
	approved bool
	commitID string
}

func currentDecisions(reviews []github.ReviewReport) map[string]reviewDecision {
	latest := make(map[string]github.ReviewReport)
	for _, r := range reviews {
		if r.Login == "" {
			continue
		}
		switch r.State {
		case github.StateApproved, github.StateChangesRequested, "DISMISSED":
		default:
			continue
		}
		if prev, ok := latest[r.Login]; !ok || r.ID > prev.ID {
			latest[r.Login] = r
		}
	}

	out := make(map[string]reviewDecision, len(latest))
	for login, r := range latest {
		if r.State == "DISMISSED" {
			continue
		}
		out[login] = reviewDecision{
			bot:      r.Bot,
			approved: r.State == github.StateApproved,
			commitID: r.CommitID,
		}
	}
	return out
}

func reviewFacts(s Set, headSHA string, reviews []github.ReviewReport, changed []string, codeowners []byte, teams config.Teams, requestedTeams []string, owner string) {
	decisions := currentDecisions(reviews)

	humanApproved, changesRequested := false, false
	for _, d := range decisions {
		if !d.approved {
			changesRequested = true
			continue
		}
		if !d.bot && d.commitID == headSHA {
			humanApproved = true
		}
	}
	s.Bool("review.human.approved", humanApproved)
	s.Bool("review.changes_requested", changesRequested)

	rules := parseCodeowners(codeowners)
	for _, name := range reviewTeamNames(teams, requestedTeams, owner) {
		target := resolveTeamTarget(name, teams, owner)
		members := teamProxyMembers(rules, changed, target)

		approved, stale := false, false
		for login, d := range decisions {
			if !d.approved || !members[strings.ToLower(login)] {
				continue
			}
			if d.commitID == headSHA {
				approved = true
			} else {
				stale = true
			}
		}

		s.Bool("review."+name+".requested", slices.Contains(requestedTeams, teamSlug(target)))
		s.Bool("review."+name+".approved", approved)
		s.Bool("review."+name+".stale", stale && !approved)
	}
}

func reviewTeamNames(teams config.Teams, requestedTeams []string, owner string) []string {
	seenTarget := make(map[string]bool)
	var names []string
	add := func(name string) {
		if !teamNamePattern.MatchString(name) {
			return
		}
		target := resolveTeamTarget(name, teams, owner)
		if seenTarget[target] {
			return
		}
		seenTarget[target] = true
		names = append(names, name)
	}

	logical := make([]string, 0, len(teams))
	for name := range teams {
		logical = append(logical, name)
	}
	sort.Strings(logical)
	for _, name := range logical {
		add(name)
	}

	slugs := slices.Clone(requestedTeams)
	sort.Strings(slugs)
	for _, slug := range slugs {
		add(slug)
	}
	return names
}

func resolveTeamTarget(name string, teams config.Teams, owner string) string {
	if teams != nil {
		if handle, ok := teams[name]; ok && handle != "" {
			return strings.TrimPrefix(handle, "@")
		}
	}
	return owner + "/" + name
}

func teamSlug(target string) string {
	if i := strings.LastIndex(target, "/"); i >= 0 {
		return target[i+1:]
	}
	return target
}

func teamProxyMembers(rules []codeownerRule, paths []string, target string) map[string]bool {
	want := "@" + target
	members := make(map[string]bool)
	for _, path := range paths {
		for i := len(rules) - 1; i >= 0; i-- {
			if !codeownersMatch(rules[i].pattern, path) {
				continue
			}
			owners := rules[i].owners
			if !slices.ContainsFunc(owners, func(o string) bool { return strings.EqualFold(o, want) }) {
				break
			}
			for _, o := range owners {
				login, isUser := strings.CutPrefix(o, "@")
				if isUser && !strings.Contains(login, "/") {
					members[strings.ToLower(login)] = true
				}
			}
			break
		}
	}
	return members
}
