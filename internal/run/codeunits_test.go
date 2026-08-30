package run

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/opentalon/talooner/internal/config"
	"github.com/opentalon/talooner/internal/facts"
)

// codeUnitArch is a minimal opt-in architecture.yaml: resolveCodeUnits refuses
// to do anything at all without one (TestCodeUnitsOmittedWithoutArchitectureYaml,
// run_test.go), regardless of what it actually overrides.
var codeUnitArch = []config.ArchitectureRule{{Path: "unrelated/", Kind: "service"}}

// Two units sharing one doc_ref must fetch it once, not twice — the shared-doc
// case facts.md calls out ("several units can share one doc").
func TestResolveCodeUnitsFetchesASharedDocOnce(t *testing.T) {
	gh := &fakeGitHub{docs: map[string]string{"docs/services/shared.md": "the shared contract"}}
	r := Runner{GitHub: gh.client(t), Log: slog.New(slog.DiscardHandler)}

	units := []facts.CodeUnit{
		{Kind: "service", Path: "internal/a", Important: true, DocRef: "docs/services/shared.md", DiffSlice: "diff-a"},
		{Kind: "service", Path: "internal/b", Important: true, DocRef: "docs/services/shared.md", DiffSlice: "diff-b"},
	}
	got, warnings, err := r.resolveCodeUnits(t.Context(), "opentalon", "talooner", "master", units, codeUnitArch)
	if err != nil {
		t.Fatalf("resolveCodeUnits: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none — the doc was found", warnings)
	}
	if len(got) != 2 {
		t.Fatalf("code units = %v, want 2", got)
	}
	for _, u := range got {
		if u.DocContent != "the shared contract" {
			t.Errorf("unit %s doc_content = %q", u.Name, u.DocContent)
		}
	}

	fetches := 0
	for _, p := range gh.paths {
		if strings.HasSuffix(p, "/contents/docs/services/shared.md") {
			fetches++
		}
	}
	if fetches != 1 {
		t.Errorf("doc fetched %d times, want exactly 1", fetches)
	}
}

// A doc over 1 MiB is not something FileContent can even hand back — GitHub's
// Contents API itself refuses to inline anything that big, and FileContent
// turns that into a hard error (internal/github/contents.go's maxFileBytes).
// resolveCodeUnits treats it the same as a missing doc: drop the unit, warn
// once per doc_ref even when two units share it, and never fail the run.
func TestResolveCodeUnitsWarnsOnceForASharedOversizedDoc(t *testing.T) {
	big := strings.Repeat("x", (1<<20)+10) // over github's maxFileBytes
	gh := &fakeGitHub{docs: map[string]string{"docs/services/shared.md": big}}
	r := Runner{GitHub: gh.client(t), Log: slog.New(slog.DiscardHandler)}

	units := []facts.CodeUnit{
		{Kind: "service", Path: "internal/a", Important: true, DocRef: "docs/services/shared.md", DiffSlice: "diff-a"},
		{Kind: "service", Path: "internal/b", Important: true, DocRef: "docs/services/shared.md", DiffSlice: "diff-b"},
	}
	got, warnings, err := r.resolveCodeUnits(t.Context(), "opentalon", "talooner", "master", units, codeUnitArch)
	if err != nil {
		t.Fatalf("resolveCodeUnits: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("code units = %v, want none — neither unit's doc could load", got)
	}

	unavailable := 0
	for _, w := range warnings {
		if w.Code == "code_unit_doc_unavailable" {
			unavailable++
		}
	}
	if unavailable != 1 {
		t.Errorf("code_unit_doc_unavailable warnings = %d, want exactly 1 (one per doc_ref, not per unit)", unavailable)
	}
}

// An architecture.yaml override that names no doc_ref at all — "the unit still
// exists, it simply carries no doc to review against" (facts.md) — is dropped
// with no fetch and no warning: this is a declared answer, not a problem.
func TestResolveCodeUnitsSkipsAUnitWithNoDocRef(t *testing.T) {
	gh := &fakeGitHub{}
	r := Runner{GitHub: gh.client(t), Log: slog.New(slog.DiscardHandler)}

	units := []facts.CodeUnit{
		{Kind: "model", Path: "legacy/thing.rb", Important: true, DocRef: "", DiffSlice: "diff"},
	}
	got, warnings, err := r.resolveCodeUnits(t.Context(), "opentalon", "talooner", "master", units, codeUnitArch)
	if err != nil {
		t.Fatalf("resolveCodeUnits: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("code units = %v, want none", got)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	if len(gh.paths) != 0 {
		t.Errorf("GitHub was called %v, want no calls for a unit with no doc_ref", gh.paths)
	}
}

// No architecture.yaml at all is the opt-in gate itself (run_test.go's
// TestCodeUnitsOmittedWithoutArchitectureYaml covers it end to end); this is
// the same behavior directly against resolveCodeUnits, confirming it makes no
// GitHub calls at all rather than merely returning nothing.
func TestResolveCodeUnitsNoopsWithoutArchitectureYaml(t *testing.T) {
	gh := &fakeGitHub{docs: map[string]string{"docs/services/auth.md": "would be found if asked"}}
	r := Runner{GitHub: gh.client(t), Log: slog.New(slog.DiscardHandler)}

	units := []facts.CodeUnit{
		{Kind: "service", Path: "internal/auth", Important: true, DocRef: "docs/services/auth.md", DiffSlice: "diff"},
	}
	got, warnings, err := r.resolveCodeUnits(t.Context(), "opentalon", "talooner", "master", units, nil)
	if err != nil {
		t.Fatalf("resolveCodeUnits: %v", err)
	}
	if got != nil || warnings != nil {
		t.Errorf("got = %v, warnings = %v, want nil, nil with no architecture.yaml", got, warnings)
	}
	if len(gh.paths) != 0 {
		t.Errorf("GitHub was called %v, want no calls at all", gh.paths)
	}
}
