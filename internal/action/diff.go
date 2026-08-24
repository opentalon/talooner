package action

// Diff compares a planned action set against the one that actually governs
// writes and reports what the planned set would add and remove. It exists for
// E2 (#21): a fork PR's head-branch ruleset is evaluated for comparison only,
// and this is how that comparison is rendered without ever executing what it
// found.
//
// Comparison is by value, with multiplicity: an action appearing twice in one
// set and once in the other counts as one added or removed, not a match, so a
// ruleset that fires the same `do block "pr"` rule twice is not silently
// folded into a ruleset that fires it once.
func Diff(base, planned []Action) (added, removed []Action) {
	remaining := make(map[Action]int, len(base))
	for _, a := range base {
		remaining[a]++
	}
	for _, a := range planned {
		if remaining[a] > 0 {
			remaining[a]--
		} else {
			added = append(added, a)
		}
	}
	taken := make(map[Action]int, len(remaining))
	for _, a := range base {
		if taken[a] < remaining[a] {
			removed = append(removed, a)
			taken[a]++
		}
	}
	return added, removed
}
