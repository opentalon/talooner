// Package facts extracts everything a rule can match on from the PR being
// reviewed. It is the extraction half of facts.md; what the cluster does with
// the result — scoping, lifetime, namespace enforcement — is the plugin's.
//
// One rule governs every extractor here: assert explicitly, negative cases
// included. Unset means "the extractor never ran or died", never "we determined
// it is false". The engine is two-valued with negation-as-failure, so a
// not-shaped rule over an unset fact *fires* — a PR whose extraction half
// failed would be approved with a review reporting no problems (facts.md,
// "Unset is false, and that asymmetry is load-bearing"). An extractor that
// cannot produce its whole set returns an error instead of a partial one.
package facts

// Set is a fact name to its value, and is what evaluate_pr takes as its `facts`
// JSON argument (talooner-plugin/protocol.md). Values are bool, int, string or
// []string; nothing else survives the round trip.
type Set map[string]any

// New returns an empty Set.
func New() Set {
	return make(Set)
}

// Bool asserts a boolean fact.
func (s Set) Bool(name string, v bool) {
	s[name] = v
}

// Int asserts an integer fact.
func (s Set) Int(name string, v int) {
	s[name] = v
}

// String asserts a string fact, empty string included.
func (s Set) String(name string, v string) {
	s[name] = v
}

// Strings asserts a list fact. A nil slice becomes an empty list rather than
// JSON null: an empty list matches no predicate, which is the answer, while
// null reads as unset, which is the absence of one.
func (s Set) Strings(name string, v []string) {
	if v == nil {
		v = []string{}
	}
	s[name] = v
}
