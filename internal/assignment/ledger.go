package assignment

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ledgerPrefix and ledgerSuffix wrap the machine-readable half of the ledger
// comment. It is a second HTML comment inside the body rather than the topic
// marker itself, because a StickyComment body may not carry its own marker.
const (
	ledgerPrefix = "<!-- talooner-ledger "
	ledgerSuffix = " -->"
)

// Ledger is what Talooner added to a pull request and may therefore take away
// again. Anything not in it belongs to somebody else, and Talooner leaves it
// alone however long it has been standing.
type Ledger struct {
	Assignees []string `json:"assignees,omitempty"`
	Users     []string `json:"reviewer_users,omitempty"`
	Teams     []string `json:"reviewer_teams,omitempty"`
}

func (l Ledger) isEmpty() bool { return len(l.Assignees)+len(l.Users)+len(l.Teams) == 0 }

func (l Ledger) equal(other Ledger) bool {
	return sameSet(l.Assignees, other.Assignees) &&
		sameSet(l.Users, other.Users) &&
		sameSet(l.Teams, other.Teams)
}

// ParseLedger reads a ledger out of a comment body. A body with no ledger line
// in it is an empty ledger, not an error: that is a pull request Talooner has
// added nothing to yet.
//
// A ledger line that will not decode *is* an error. Somebody edited it, and the
// caller's answer is to own nothing this run rather than to guess — see the
// package comment for why that direction is the safe one.
func ParseLedger(body string) (Ledger, error) {
	for line := range strings.Lines(body) {
		raw, ok := strings.CutPrefix(strings.TrimSpace(line), ledgerPrefix)
		if !ok {
			continue
		}
		raw, ok = strings.CutSuffix(raw, ledgerSuffix)
		if !ok {
			return Ledger{}, fmt.Errorf("ledger line is not closed: %q", strings.TrimSpace(line))
		}
		var l Ledger
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			return Ledger{}, fmt.Errorf("decode ledger %q: %w", raw, err)
		}
		return l, nil
	}
	return Ledger{}, nil
}

// LedgerBody renders the ledger comment: what Talooner is holding, in words,
// and the same thing in the line the next run reads back.
//
// The prose half is not decoration. This comment sits in a human's pull request
// thread, and "Talooner added these and will take them back" is exactly what
// somebody wondering why a reviewer appeared needs to read.
func LedgerBody(l Ledger) string {
	var b strings.Builder
	b.WriteString("### Talooner assignments\n\n")
	if l.isEmpty() {
		b.WriteString("Talooner is not holding any assignees or review requests on this pull request. " +
			"Anything still standing was added by a person, and Talooner will not remove it.\n\n")
	} else {
		b.WriteString("Talooner added the following, and removes them again when the rules stop asking for them. " +
			"Anything not listed here was added by a person and is left alone.\n\n")
		writeList(&b, "Assignees", l.Assignees, "@")
		writeList(&b, "Review requests", l.Users, "@")
		writeList(&b, "Team review requests", l.Teams, "")
	}
	b.WriteString(ledgerLine(l))
	b.WriteString("\n<sub>Talooner edits this comment in place; it always shows the current state.</sub>\n")
	return b.String()
}

// ledgerLine is the machine-readable half. Every name in it has been through
// namePattern, so none of them can carry the `-->` that would end the comment
// early and leave the rest of the ledger rendered in the thread.
func ledgerLine(l Ledger) string {
	raw, err := json.Marshal(l)
	if err != nil {
		// Ledger is three string slices; this cannot fail. An empty ledger line
		// is still a valid one, and reads as "Talooner holds nothing".
		raw = []byte("{}")
	}
	return ledgerPrefix + string(raw) + ledgerSuffix + "\n"
}

func writeList(b *strings.Builder, title string, names []string, sigil string) {
	if len(names) == 0 {
		return
	}
	fmt.Fprintf(b, "**%s**: ", title)
	for i, name := range names {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(sigil + name)
	}
	b.WriteString("\n\n")
}

// sameSet compares two name lists the way GitHub compares logins: without
// regard to order or case.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, name := range a {
		if !contains(b, name) {
			return false
		}
	}
	return true
}
