package facts

import (
	"sort"
	"strings"

	"github.com/opentalon/talooner/internal/config"
	"github.com/opentalon/talooner/internal/github"
)

func moduleFacts(s Set, files []github.FileStat, modules []config.Module) {
	lines := make(map[string]int, len(modules))
	touched := 0
	for _, m := range modules {
		n := 0
		for _, f := range files {
			if moduleOwns(m.Path, f.Path) {
				n += f.Additions + f.Deletions
			}
		}
		if n > 0 {
			lines[m.Path] = n
			touched++
		}
	}

	s.Int("module.touched_count", touched)
	if touched == 0 {
		return
	}

	primary := ""
	best := -1
	for _, m := range sortedByPath(modules) {
		if lines[m.Path] > best {
			best = lines[m.Path]
			primary = m.Path
		}
	}

	var docURLs []string
	seen := make(map[string]bool)
	for _, m := range modules {
		if lines[m.Path] == 0 || m.DocumentationURL == "" || seen[m.DocumentationURL] {
			continue
		}
		seen[m.DocumentationURL] = true
		docURLs = append(docURLs, m.DocumentationURL)
	}
	sort.Strings(docURLs)

	if p := moduleByPath(modules, primary); p != nil {
		if p.DocumentationURL != "" {
			s.String("module.documentation_url", p.DocumentationURL)
		}
		if p.Owner != "" {
			s.String("module.owner", p.Owner)
		}
	}
	if len(docURLs) > 0 {
		s.Strings("module.documentation_urls", docURLs)
	}
}

func moduleOwns(prefix, path string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return false
	}
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func sortedByPath(modules []config.Module) []config.Module {
	out := make([]config.Module, len(modules))
	copy(out, modules)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func moduleByPath(modules []config.Module, path string) *config.Module {
	for i := range modules {
		if modules[i].Path == path {
			return &modules[i]
		}
	}
	return nil
}
