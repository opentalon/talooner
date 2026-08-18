package facts

import (
	"encoding/json"
	"testing"
)

func TestSetHoldsEachType(t *testing.T) {
	s := New()
	s.Int("pr.number", 42)
	s.String("pr.title", "Add a thing")
	s.Bool("pr.draft", false)
	s.Strings("pr.labels", []string{"bug", "v1"})

	if got := s["pr.number"]; got != 42 {
		t.Errorf("pr.number = %v, want 42", got)
	}
	if got := s["pr.title"]; got != "Add a thing" {
		t.Errorf("pr.title = %v, want \"Add a thing\"", got)
	}
	if got := s["pr.draft"]; got != false {
		t.Errorf("pr.draft = %v, want false", got)
	}
	if got, ok := s["pr.labels"].([]string); !ok || len(got) != 2 {
		t.Errorf("pr.labels = %v, want [bug v1]", s["pr.labels"])
	}
}

// A nil slice marshals to JSON null, and the plugin reads null as unset — which
// turns "we determined the list is empty" into "the extractor died". The
// difference decides whether a not-shaped rule fires (facts.md, "Unset is
// false").
func TestStringsAssertsEmptyListNotNull(t *testing.T) {
	s := New()
	s.Strings("pr.changed_files", nil)

	if _, ok := s["pr.changed_files"]; !ok {
		t.Fatal("pr.changed_files missing, want an empty list asserted")
	}
	body, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want := `{"pr.changed_files":[]}`; string(body) != want {
		t.Errorf("json = %s, want %s", body, want)
	}
}

func TestSetOverwritesRatherThanDuplicating(t *testing.T) {
	s := New()
	s.Bool("pr.draft", true)
	s.Bool("pr.draft", false)

	if got := s["pr.draft"]; got != false {
		t.Errorf("pr.draft = %v, want false: the later assertion wins", got)
	}
}
