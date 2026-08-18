package assignment

import (
	"strings"
	"testing"
)

func TestLedgerRoundTrips(t *testing.T) {
	want := Ledger{
		Assignees: []string{"alice"},
		Users:     []string{"bob"},
		Teams:     []string{"security", "design"},
	}

	got, err := ParseLedger(LedgerBody(want))
	if err != nil {
		t.Fatalf("ParseLedger: %v", err)
	}
	if !got.equal(want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

// The ledger line is an HTML comment inside another comment's body. A name
// carrying `-->` would end it early and leave the rest of the ledger rendered
// in somebody's pull request — so no such name gets this far.
func TestNoNameCanCloseTheLedgerComment(t *testing.T) {
	for _, raw := range []string{"alice-->", "a --> b", "<!--", "\n"} {
		if _, err := ResolveAssignee(raw); err == nil {
			t.Errorf("ResolveAssignee(%q) = nil, want it refused", raw)
		}
	}
	if body := LedgerBody(Ledger{Assignees: []string{"alice"}}); strings.Count(body, "-->") != 1 {
		t.Errorf("body has more than one comment close:\n%s", body)
	}
}

// A comment with no ledger line is a pull request Talooner has added nothing
// to, which is a state, not a failure.
func TestParseLedgerOnABodyWithNoLedger(t *testing.T) {
	got, err := ParseLedger("### Talooner review\n\nnothing to report\n")
	if err != nil {
		t.Fatalf("ParseLedger: %v", err)
	}
	if !got.isEmpty() {
		t.Errorf("ledger = %+v, want empty", got)
	}
}

func TestParseLedgerRejectsWhatItCannotRead(t *testing.T) {
	for _, body := range []string{
		"<!-- talooner-ledger {\"assignees\":[\"alice\"] -->",
		"<!-- talooner-ledger {\"assignees\":\"alice\"} -->", // a string where a list belongs
		"<!-- talooner-ledger {\"assignees\":[\"alice\"]}",   // never closed
	} {
		if _, err := ParseLedger(body); err == nil {
			t.Errorf("ParseLedger(%q) = nil, want an error the caller turns into owning nothing", body)
		}
	}
}

// The prose half is what a maintainer reads when a reviewer appears out of
// nowhere, so it has to name who and say that Talooner will take them back.
func TestLedgerBodySaysWhatItIsHolding(t *testing.T) {
	body := LedgerBody(Ledger{Assignees: []string{"alice"}, Teams: []string{"security"}})
	for _, want := range []string{"@alice", "security", "removes them again"} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not mention %q:\n%s", want, body)
		}
	}

	empty := LedgerBody(Ledger{})
	if !strings.Contains(empty, "not holding any") {
		t.Errorf("empty body does not say so:\n%s", empty)
	}
}
