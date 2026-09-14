package assignment

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	ledgerPrefix = "<!-- talooner-ledger "
	ledgerSuffix = " -->"
)

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

func ledgerLine(l Ledger) string {
	raw, err := json.Marshal(l)
	if err != nil {
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
