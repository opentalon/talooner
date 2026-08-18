package action

import "fmt"

// requireSpec is `do require <review.target>`: a review request to the mapped
// team or user, withdrawn when the rule stops matching (actions.md). D5 owns
// both halves.
//
// The target is the review namespace — review.security, review.design — already
// resolved to whatever the rule named. Mapping it to a GitHub team or login is
// the executor's, and team membership is the one thing GITHUB_TOKEN cannot read,
// so that mapping is where the CODEOWNERS fallback lives.
var requireSpec = spec{
	verb: VerbRequire,
	validate: func(a Action) error {
		return required(VerbRequire, "a target", a.Target)
	},
	describe: func(a Action) string {
		return fmt.Sprintf("require %s", a.Target)
	},
}
