package facts

import (
	"path"
	"sort"
	"strings"

	"github.com/opentalon/talooner/internal/config"
	"github.com/opentalon/talooner/internal/github"
)

// CodeUnit is one touched service/model/controller entity — the granularity
// expert-review-system.md's per-unit LLM review binds to (its "Key decisions"
// #2). Not wired anywhere yet: Phase 2 adds the proto field that carries these
// to the cluster (`docs/expert-review-system.md`, "Phase 2"). Phase 1 only
// builds and tests the extraction that will feed it.
type CodeUnit struct {
	Kind      string // "model", "controller" or "service"
	Path      string // the unit's representative path
	Important bool
	DocRef    string
	DiffSlice string
}

// layer is one built-in per-language convention: a path prefix, the kind of
// code_unit it produces, and whether files under it fold into one unit per
// directory (Go: a package is the unit) or one unit per file (Rails: a model,
// controller or service class is its own file).
type layer struct {
	prefix    string
	kind      string
	directory bool
}

// builtinLayers are the conventions architecture.yaml can override or extend
// (expert-review-system.md, Phase 1 "Conventions + override"). They are
// checked against every PR regardless of which language the repo is actually
// written in: Talooner has no repo-tree listing to detect that from, only the
// changed files a run already fetched, and a Rails prefix (app/models/) can
// never collide with a Go one (internal/, cmd/), so running both tables
// unconditionally costs nothing and needs no extra API call.
var builtinLayers = []layer{
	{prefix: "app/models/", kind: "model", directory: false},
	{prefix: "app/controllers/", kind: "controller", directory: false},
	{prefix: "app/services/", kind: "service", directory: false},
	{prefix: "internal/", kind: "service", directory: true},
	{prefix: "cmd/", kind: "service", directory: true},
}

// docSuffixes strips a kind's conventional file-name suffix before turning it
// into a doc name — "orders_service.rb" documents "orders", not
// "orders_service" (expert-review-system.md's own example: docs/services/orders.md).
// A model carries no such suffix, so "model" has none to strip.
var docSuffixes = map[string]string{
	"controller": "_controller",
	"service":    "_service",
}

// architectureFacts asserts the code.* roll-up namespace (facts.md, "code.*")
// into s and returns one CodeUnit per touched entity, grouped per the layer it
// matched. Files matching no known layer — override or built-in — contribute
// to no unit and no roll-up; they are simply outside the gate.
//
// Every unit's Important is true: Phase 1 only ever creates a unit for a file
// under a recognised layer, and a touched file under a layer this system
// knows about is exactly what the LLM-review gate exists to catch. A cheaper
// token-economy filter (e.g. an unimportant subtree within a layer) is a
// later refinement, not a Phase 1 concern.
func architectureFacts(s Set, files []github.FileStat, diff string, arch []config.ArchitectureRule) []CodeUnit {
	slices := diffSlicesByPath(diff)

	// unitFiles groups every changed file under its resolved unit path, in the
	// order units are first seen, so diff_slice concatenation and roll-up
	// lists are deterministic on a re-run.
	type unit struct {
		kind   string
		docRef string
		files  []string
	}
	order := make([]string, 0, len(files))
	units := make(map[string]*unit, len(files))

	for _, f := range files {
		kind, unitPath, docRef, ok := resolveUnit(f.Path, arch)
		if !ok {
			continue
		}
		u, seen := units[unitPath]
		if !seen {
			u = &unit{kind: kind, docRef: docRef}
			units[unitPath] = u
			order = append(order, unitPath)
		}
		u.files = append(u.files, f.Path)
	}

	byKind := map[string][]string{"model": nil, "controller": nil, "service": nil}
	result := make([]CodeUnit, 0, len(order))
	for _, unitPath := range order {
		u := units[unitPath]
		sort.Strings(u.files)
		var parts []string
		for _, p := range u.files {
			if body, ok := slices[p]; ok {
				parts = append(parts, body)
			}
		}
		result = append(result, CodeUnit{
			Kind:      u.kind,
			Path:      unitPath,
			Important: true,
			DocRef:    u.docRef,
			DiffSlice: strings.Join(parts, "\n"),
		})
		byKind[u.kind] = append(byKind[u.kind], unitPath)
	}

	for _, kind := range []string{"model", "controller", "service"} {
		paths := byKind[kind]
		sort.Strings(paths)
		s.Strings("code."+kind+"s_changed", paths)
		s.Bool("code.touches_"+kind, len(paths) > 0)
	}

	return result
}

// resolveUnit decides which unit, if any, a changed file belongs to: an
// architecture.yaml rule first (longest matching prefix wins, so a narrower
// override beats a broader one), then the built-in layer table, also by
// longest prefix. It reports ok=false for a file under neither.
func resolveUnit(filePath string, arch []config.ArchitectureRule) (kind, unitPath, docRef string, ok bool) {
	bestLen := -1
	for _, r := range arch {
		if !moduleOwns(r.Path, filePath) {
			continue
		}
		if len(r.Path) > bestLen {
			bestLen = len(r.Path)
			kind, unitPath, docRef, ok = r.Kind, strings.TrimSuffix(r.Path, "/"), r.DocRef, true
		}
	}
	if ok {
		return kind, unitPath, docRef, ok
	}

	bestLen = -1
	var matched layer
	for _, l := range builtinLayers {
		if !strings.HasPrefix(filePath, l.prefix) {
			continue
		}
		if len(l.prefix) > bestLen {
			bestLen = len(l.prefix)
			matched, ok = l, true
		}
	}
	if !ok {
		return "", "", "", false
	}

	if matched.directory {
		rest := strings.TrimPrefix(filePath, matched.prefix)
		seg, _, found := strings.Cut(rest, "/")
		if seg == "" || !found {
			// A file directly under the prefix, with no package subdirectory,
			// belongs to no unit — "package dirs" means a subdirectory.
			return "", "", "", false
		}
		unitPath = matched.prefix + seg
		return matched.kind, unitPath, "docs/services/" + seg + ".md", true
	}

	return matched.kind, filePath, "docs/" + matched.kind + "s/" + docName(matched.kind, filePath), true
}

// docName turns a Rails-style unit file into the co-located doc's base name:
// strip the extension, then the kind's conventional suffix if the kind has
// one — "app/services/orders_service.rb" documents "orders.md", not
// "orders_service.md".
func docName(kind, filePath string) string {
	base := path.Base(filePath)
	base = strings.TrimSuffix(base, path.Ext(base))
	if suffix, ok := docSuffixes[kind]; ok {
		base = strings.TrimSuffix(base, suffix)
	}
	return base + ".md"
}

// diffSlicesByPath splits the PR's concatenated diff by file (dependencies.go
// already does this for manifest parsing) into a path → hunk-body lookup, so
// architectureFacts can hand each unit only its own files' hunks rather than
// the whole pr.diff.
func diffSlicesByPath(diff string) map[string]string {
	out := make(map[string]string)
	for _, f := range splitDiffFiles(diff) {
		out[f.name] = f.body
	}
	return out
}
