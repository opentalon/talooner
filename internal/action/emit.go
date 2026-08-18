package action

import "fmt"

// emitSpec is `do emit <name>`: asserts event.<name> in the PR's scope and does
// nothing to GitHub (actions.md). It is how one rule chains into another.
//
// The assertion happens cluster-side, so the bot's executor has nothing to do —
// which is exactly the case where a missing name would vanish without trace, and
// why an empty one is rejected here.
var emitSpec = spec{
	verb: VerbEmit,
	validate: func(a Action) error {
		return required(VerbEmit, "a name", a.Name)
	},
	describe: func(a Action) string {
		return fmt.Sprintf("emit event.%s", a.Name)
	},
}
