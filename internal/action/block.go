package action

import "fmt"

// blockSpec is `do block "pr.merge"`: the talooner check run at failure, plus a
// REQUEST_CHANGES review (actions.md). Talooner has no merge rights — whether
// the check gates the merge is the repo's branch protection to decide.
//
// block and approve can both fire; the tie is resolved by the plugin's
// defeasible machinery, not here. What reaches the bot is both actions and a
// warning, and block-wins is the check-run writer's last-resort tiebreak (D2).
var blockSpec = spec{
	verb: VerbBlock,
	validate: func(a Action) error {
		return required(VerbBlock, "a target", a.Target)
	},
	describe: func(a Action) string {
		return fmt.Sprintf("block %s", a.Target)
	},
}
