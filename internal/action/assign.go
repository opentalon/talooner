package action

import "fmt"

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
