package facts

import (
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/opentalon/talooner/internal/config"
	"github.com/opentalon/talooner/internal/github"
)

// teamNamePattern is the shape a fact name segment built from an unvalidated
// teams.yaml key or a GitHub team slug must have: a key with a "." or spaces
// in it cannot become a safe "review.<name>.approved" attr, so it is dropped
// rather than asserted mangled.
var teamNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,99}$`)

// reviewDecision is one login's current standing review, folded from its full
// history.
type reviewDecision struct {
	bot      bool
	approved bool // false means CHANGES_REQUESTED
	commitID string
}

// currentDecisions folds a review history down to one standing decision per
// login, the same way GitHub's own merge box does. Every review event is kept
// forever; dismissing one flips its own State to DISMISSED in place rather
// than adding a new event, and a COMMENTED review never overrides a person's
// last decision — only a fresh APPROVED or CHANGES_REQUESTED does. Approvals
// are not assumed dismissed on push (facts.md, "review.*"), so a stale
// commit_id is still a decision, just an old one; the caller decides what to
// do with that.
func currentDecisions(reviews []github.ReviewReport) map[string]reviewDecision {
	latest := make(map[string]github.ReviewReport)
	for _, r := range reviews {
		if r.Login == "" {
			continue
		}
		switch r.State {
		case github.StateApproved, github.StateChangesRequested, "DISMISSED":
		default:
			continue // COMMENTED, PENDING: never overrides a standing decision
		}
		if prev, ok := latest[r.Login]; !ok || r.ID > prev.ID {
			latest[r.Login] = r
		}
	}

	out := make(map[string]reviewDecision, len(latest))
	for login, r := range latest {
		if r.State == "DISMISSED" {
			continue // nothing standing for this login
		}
		out[login] = reviewDecision{
			bot:      r.Bot,
			approved: r.State == github.StateApproved,
			commitID: r.CommitID,
		}
	}
	return out
}

// reviewFacts asserts the review.* namespace (facts.md, "review.*"). reviews
// is every review ever submitted, folded here to current state; changed and
// codeowners are what user.owner already reads, reused to derive team
// approval without an org membership call GITHUB_TOKEN cannot make (Evgeny's
// call, facts.md "review.<team>.approved": a CODEOWNERS proxy, not a real
// membership lookup).
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
		// stale only says something when there is no current approval: an old
		// approval sitting next to a fresh one is not news to a rule.
		s.Bool("review."+name+".stale", stale && !approved)
	}
}

// reviewTeamNames is every logical team name review.* asserts facts for: every
// teams.yaml key, plus every directly requested team slug not already covered
// by one. The same physical team never gets two fact names — a requested slug
// that resolves to a teams.yaml entry's own target is skipped in favour of the
// logical name a ruleset actually wrote.
func reviewTeamNames(teams config.Teams, requestedTeams []string, owner string) []string {
	seenTarget := make(map[string]bool)
	var names []string
	add := func(name string) {
		if !teamNamePattern.MatchString(name) {
			return // cannot become a safe fact name; see teamNamePattern
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
	sort.Strings(logical) // teams is a map; iteration order must not leak in
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

// resolveTeamTarget mirrors assignment.ResolveReviewer's team resolution: a
// name mapped by teams.yaml is trusted repo config, taken as-is (and may name
// a team in another organisation); an unmapped name falls back to the
// path-derived slug in the repo's own organisation.
func resolveTeamTarget(name string, teams config.Teams, owner string) string {
	if teams != nil {
		if handle, ok := teams[name]; ok && handle != "" {
			return strings.TrimPrefix(handle, "@")
		}
	}
	return owner + "/" + name
}

// teamSlug is the bare slug GitHub's requested_teams reports — no
// organisation prefix, unlike a CODEOWNERS entry or a teams.yaml value.
func teamSlug(target string) string {
	if i := strings.LastIndex(target, "/"); i >= 0 {
		return target[i+1:]
	}
	return target
}

// teamProxyMembers is the CODEOWNERS-derived membership proxy for target
// (facts.md, "review.<team>.approved"): a login stands in for a team when
// CODEOWNERS lists it on the same rule as the team, for a path this PR
// touches. Wrong for a team CODEOWNERS never mentions alongside anyone — the
// documented gap, not a silent guess at full coverage.
func teamProxyMembers(rules []codeownerRule, paths []string, target string) map[string]bool {
	want := "@" + target
	members := make(map[string]bool)
	for _, path := range paths {
		// Last matching rule wins, same as resolveOwners.
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
