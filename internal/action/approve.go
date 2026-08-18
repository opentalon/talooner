package action

import "fmt"

// approveSpec is `do approve "pr"`: a review with event APPROVE, plus the
// talooner check run at success (actions.md). The approval is advisory — it does
// not satisfy branch protection, and is not meant to.
//
// Retraction dismisses the review, so the GitHub half has to be able to find
// what it wrote. That is D4's problem; here the target is only checked to exist.
var approveSpec = spec{
	verb: VerbApprove,
	validate: func(a Action) error {
		return required(VerbApprove, "a target", a.Target)
	},
	describe: func(a Action) string {
		return fmt.Sprintf("approve %s", a.Target)
	},
}
