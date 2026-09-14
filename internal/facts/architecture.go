package facts

import (
	"path"
	"sort"
	"strings"

	"github.com/opentalon/talooner/internal/config"
	"github.com/opentalon/talooner/internal/github"
)

type CodeUnit struct {
	Kind          string
	Path          string
	Important     bool
	DocRef        string
	DiffSlice     string
	TestDiffSlice string
}

type layer struct {
	prefix    string
	kind      string
	directory bool
}

var builtinLayers = []layer{
	{prefix: "app/models/", kind: "model", directory: false},
	{prefix: "app/controllers/", kind: "controller", directory: false},
	{prefix: "app/services/", kind: "service", directory: false},
	{prefix: "internal/", kind: "service", directory: true},
	{prefix: "cmd/", kind: "service", directory: true},
}

var docSuffixes = map[string]string{
	"controller": "_controller",
	"service":    "_service",
}

func architectureFacts(s Set, files []github.FileStat, diff string, arch []config.ArchitectureRule) []CodeUnit {
	slices := diffSlicesByPath(diff)

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
			Kind:          u.kind,
			Path:          unitPath,
			Important:     true,
			DocRef:        u.docRef,
			DiffSlice:     strings.Join(parts, "\n"),
			TestDiffSlice: testAssertionDiff(u.files, slices),
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
			return "", "", "", false
		}
		unitPath = matched.prefix + seg
		return matched.kind, unitPath, "docs/services/" + seg + ".md", true
	}

	return matched.kind, filePath, "docs/" + matched.kind + "s/" + docName(matched.kind, filePath), true
}

func docName(kind, filePath string) string {
	base := path.Base(filePath)
	base = strings.TrimSuffix(base, path.Ext(base))
	if suffix, ok := docSuffixes[kind]; ok {
		base = strings.TrimSuffix(base, suffix)
	}
	return base + ".md"
}

func diffSlicesByPath(diff string) map[string]string {
	out := make(map[string]string)
	for _, f := range splitDiffFiles(diff) {
		out[f.name] = f.body
	}
	return out
}
