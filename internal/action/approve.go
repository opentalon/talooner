package action

import "fmt"

// approveSpec is `do approve "pr"`: a review with event APPROVE, plus the
// talooner check run at success (actions.md). The approval is advisory — it does
// not satisfy branch protection, and is not meant to.
//
// The GitHub half is review.Writer, which serves this verb and block from one
// decided verdict: both firing is one review, not an approval submitted and
// dismissed moments later. Here the target is only checked to exist.
var approveSpec = spec{
	verb: VerbApprove,
	validate: func(a Action) error {
		return required(VerbApprove, "a target", a.Target)
	},
	describe: func(a Action) string {
		return fmt.Sprintf("approve %s", a.Target)
	},
}
