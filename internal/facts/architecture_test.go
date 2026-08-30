package facts

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/opentalon/talooner/internal/config"
	"github.com/opentalon/talooner/internal/github"
)

// architectureFacts asserts every code.* roll-up, always — a PR touching
// nothing under a known layer reads empty lists and false bools, the honest
// answer, not a dead extractor (facts.md, "Unset is false").
func TestPRArchitectureFacts(t *testing.T) {
	for _, tt := range []struct {
		name  string
		files []github.FileStat
		arch  []config.ArchitectureRule
		want  map[string]any
	}{
		{
			name:  "nothing under a known layer",
			files: []github.FileStat{{Path: "README.md"}},
			want: map[string]any{
				"code.models_changed":      []string{},
				"code.controllers_changed": []string{},
				"code.services_changed":    []string{},
				"code.touches_model":       false,
				"code.touches_controller":  false,
				"code.touches_service":     false,
			},
		},
		{
			name: "Rails model, controller and service are file-granularity units",
			files: []github.FileStat{
				{Path: "app/models/user.rb"},
				{Path: "app/controllers/orders_controller.rb"},
				{Path: "app/services/orders_service.rb"},
			},
			want: map[string]any{
				"code.models_changed":      []string{"app/models/user.rb"},
				"code.controllers_changed": []string{"app/controllers/orders_controller.rb"},
				"code.services_changed":    []string{"app/services/orders_service.rb"},
				"code.touches_model":       true,
				"code.touches_controller":  true,
				"code.touches_service":     true,
			},
		},
		{
			name: "Go internal/ and cmd/ fold into one unit per top-level package dir",
			files: []github.FileStat{
				{Path: "internal/auth/token.go"},
				{Path: "internal/auth/session.go"},
				{Path: "cmd/tln/main.go"},
			},
			want: map[string]any{
				"code.models_changed":      []string{},
				"code.controllers_changed": []string{},
				"code.services_changed":    []string{"cmd/tln", "internal/auth"},
				"code.touches_model":       false,
				"code.touches_controller":  false,
				"code.touches_service":     true,
			},
		},
		{
			name: "a file directly under internal/ with no package dir forms no unit",
			files: []github.FileStat{
				{Path: "internal/doc.go"},
			},
			want: map[string]any{
				"code.models_changed":      []string{},
				"code.controllers_changed": []string{},
				"code.services_changed":    []string{},
				"code.touches_service":     false,
			},
		},
		{
			name:  "architecture.yaml extends the built-ins with a new prefix",
			files: []github.FileStat{{Path: "legacy/order.rb"}},
			arch:  []config.ArchitectureRule{{Path: "legacy/", Kind: "model"}},
			want: map[string]any{
				"code.models_changed": []string{"legacy"},
				"code.touches_model":  true,
			},
		},
		{
			name: "architecture.yaml overrides the built-in kind for a narrower prefix",
			files: []github.FileStat{
				{Path: "app/services/orders_service.rb"},
			},
			arch: []config.ArchitectureRule{
				{Path: "app/services/orders_service.rb", Kind: "controller", DocRef: "docs/orders.md"},
			},
			want: map[string]any{
				"code.services_changed":    []string{},
				"code.controllers_changed": []string{"app/services/orders_service.rb"},
				"code.touches_service":     false,
				"code.touches_controller":  true,
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := New()
			architectureFacts(s, tt.files, "", tt.arch)
			for name, v := range tt.want {
				if !reflect.DeepEqual(s[name], v) {
					t.Errorf("%s = %v (%T), want %v", name, s[name], s[name], v)
				}
			}
		})
	}
}

// The co-located doc convention strips a kind's file-name suffix before
// naming the doc — "orders_service.rb" documents "orders.md", per
// expert-review-system.md's own example.
func TestPRArchitectureUnitsCarryConventionalDocRefs(t *testing.T) {
	files := []github.FileStat{
		{Path: "app/models/user.rb"},
		{Path: "app/controllers/orders_controller.rb"},
		{Path: "app/services/orders_service.rb"},
		{Path: "internal/auth/token.go"},
	}
	s := New()
	units := architectureFacts(s, files, "", nil)

	want := map[string]string{
		"app/models/user.rb":                   "docs/models/user.md",
		"app/controllers/orders_controller.rb": "docs/controllers/orders.md",
		"app/services/orders_service.rb":       "docs/services/orders.md",
		"internal/auth":                        "docs/services/auth.md",
	}
	got := map[string]string{}
	for _, u := range units {
		got[u.Path] = u.DocRef
		if !u.Important {
			t.Errorf("unit %s Important = false, want true", u.Path)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("doc refs = %+v, want %+v", got, want)
	}
}

// An architecture.yaml rule with no doc_ref leaves the unit's DocRef empty —
// an override without a stated doc is not defaulted to the built-in
// convention, it simply has no doc to review against.
func TestPRArchitectureOverrideWithNoDocRefStaysUnset(t *testing.T) {
	files := []github.FileStat{{Path: "legacy/order.rb"}}
	arch := []config.ArchitectureRule{{Path: "legacy/", Kind: "model"}}
	s := New()
	units := architectureFacts(s, files, "", arch)
	if len(units) != 1 || units[0].DocRef != "" {
		t.Errorf("units = %+v, want one unit with an empty DocRef", units)
	}
}

// diff_slice carries only the touched unit's own hunks, not the whole PR
// diff — the accuracy and token-economy point of per-unit granularity
// (expert-review-system.md, "Key decisions" #2).
func TestPRArchitectureDiffSlicePerUnit(t *testing.T) {
	diff := "diff --git a/app/models/user.rb b/app/models/user.rb\n" +
		"--- a/app/models/user.rb\n+++ b/app/models/user.rb\n@@ -1 +1 @@\n+user change\n" +
		"diff --git a/app/models/order.rb b/app/models/order.rb\n" +
		"--- a/app/models/order.rb\n+++ b/app/models/order.rb\n@@ -1 +1 @@\n+order change\n"
	files := []github.FileStat{{Path: "app/models/user.rb"}, {Path: "app/models/order.rb"}}
	s := New()
	units := architectureFacts(s, files, diff, nil)

	byPath := map[string]CodeUnit{}
	for _, u := range units {
		byPath[u.Path] = u
	}
	if got := byPath["app/models/user.rb"].DiffSlice; got == "" || !strings.Contains(got, "user change") || strings.Contains(got, "order change") {
		t.Errorf("user.rb diff_slice = %q, want only its own hunk", got)
	}
	if got := byPath["app/models/order.rb"].DiffSlice; got == "" || !strings.Contains(got, "order change") || strings.Contains(got, "user change") {
		t.Errorf("order.rb diff_slice = %q, want only its own hunk", got)
	}
}

// PR() wires architectureFacts in next to moduleFacts: the code.* facts are
// visible on the Set the public extractor returns, not just internally.
func TestPRWiresArchitectureFacts(t *testing.T) {
	src := fakeSource{pr: samplePR(), files: []string{"app/models/user.rb"}}
	got, _, err := PR(context.Background(), src, "opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("PR: %v", err)
	}
	models, ok := got["code.models_changed"].([]string)
	if !ok || len(models) != 1 || models[0] != "app/models/user.rb" {
		t.Errorf("code.models_changed = %v, want [app/models/user.rb]", got["code.models_changed"])
	}
}
