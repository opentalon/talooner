package action

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
