package action

import "fmt"

var approveSpec = spec{
	verb: VerbApprove,
	validate: func(a Action) error {
		return required(VerbApprove, "a target", a.Target)
	},
	describe: func(a Action) string {
		return fmt.Sprintf("approve %s", a.Target)
	},
}
