package facts

import "testing"

func TestCodeownersMatch(t *testing.T) {
	for _, tt := range []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{"star matches anything", "*", "a/b/c.go", true},
		{"star matches a bare file", "*", "README.md", true},
		{"extension anywhere", "*.go", "internal/auth/token.go", true},
		{"extension rejects other", "*.go", "internal/auth/token.py", false},
		{"anchored dir with trailing slash", "/build/logs/", "build/logs", true},
		{"anchored dir matches children", "/build/logs/", "build/logs/x/y", true},
		{"anchored dir rejects parent", "/build/logs/", "build/log", false},
		{"anchored file", "/src/main.go", "src/main.go", true},
		{"anchored file rejects nested", "/src/main.go", "a/src/main.go", false},
		{"any-depth prefix", "**/foo", "foo", true},
		{"any-depth prefix nested", "**/foo", "a/b/foo", true},
		{"any-depth prefix rejects suffix", "**/foo", "foobar", false},
		{"slash pattern matches one level", "docs/*", "docs/guide.md", true},
		{"slash pattern matches deeper", "docs/*", "docs/a/b.md", true},
		{"subdir dir match", "internal/auth/", "internal/auth/x.go", true},
		{"subdir dir match bare", "internal/auth/", "internal/auth", true},
		{"empty pattern matches nothing", "", "anything", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := codeownersMatch(tt.pattern, tt.path); got != tt.want {
				t.Errorf("codeownersMatch(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
			}
		})
	}
}

func TestParseCodeownersSkipsJunk(t *testing.T) {
	const data = `
# a comment
*       @global-owner

docs/*  @docs
`
	rules := parseCodeowners([]byte(data))
	if len(rules) != 2 {
		t.Fatalf("parsed %d rules, want 2", len(rules))
	}
	if rules[0].pattern != "*" || len(rules[0].owners) != 1 || rules[0].owners[0] != "@global-owner" {
		t.Errorf("rule 0 = %+v, want pattern * owner @global-owner", rules[0])
	}
	if rules[1].pattern != "docs/*" || rules[1].owners[0] != "@docs" {
		t.Errorf("rule 1 = %+v, want pattern docs/* owner @docs", rules[1])
	}
}

// Last matching rule wins, per GitHub; the union is sorted and de-duplicated.
func TestResolveOwnersLastMatchWins(t *testing.T) {
	rules := parseCodeowners([]byte("* @everyone\ndocs/* @docs\ndocs/README.md @readme"))
	primary, owners := resolveOwners(rules, []string{"docs/README.md", "src/app.go"})
	if primary != "@readme" {
		t.Errorf("primary = %q, want @readme", primary)
	}
	if len(owners) != 2 || owners[0] != "@everyone" || owners[1] != "@readme" {
		t.Errorf("owners = %v, want [@everyone @readme]", owners)
	}
}

func TestResolveOwnersDeduplicatesAcrossPaths(t *testing.T) {
	rules := parseCodeowners([]byte("a.go @alice\nb.go @alice\na.go @bob"))
	_, owners := resolveOwners(rules, []string{"a.go", "b.go"})
	if len(owners) != 2 {
		t.Fatalf("owners = %v, want 2 distinct", owners)
	}
}

func TestResolveOwnersUnsetWhenNothingMatches(t *testing.T) {
	rules := parseCodeowners([]byte("/internal/secret/* @alice"))
	primary, owners := resolveOwners(rules, []string{"README.md"})
	if primary != "" || owners != nil {
		t.Errorf("primary = %q, owners = %v, want both empty (no CODEOWNERS match)", primary, owners)
	}
}
