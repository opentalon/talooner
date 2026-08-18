package action

import "fmt"

// assignSpec is `do assign "pr" <team-or-user>`: an assignee through the Issues
// API, removed again when the rule stops matching (actions.md). The write and
// the withdrawal are internal/assignment; this is the argument check.
//
// The assignee arrives resolved — `attr "user.owner"` reaches here as @alice,
// never as the string user.owner. An empty assignee therefore means the fact it
// was resolved from was unset, and assigning nobody would read as a rule that
// never fired.
var assignSpec = spec{
	verb: VerbAssign,
	validate: func(a Action) error {
		if err := required(VerbAssign, "a target", a.Target); err != nil {
			return err
		}
		return required(VerbAssign, "an assignee", a.Assignee)
	},
	describe: func(a Action) string {
		return fmt.Sprintf("assign %s to %s", a.Target, a.Assignee)
	},
}
