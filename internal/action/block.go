package action

import "fmt"

var blockSpec = spec{
	verb: VerbBlock,
	validate: func(a Action) error {
		return required(VerbBlock, "a target", a.Target)
	},
	describe: func(a Action) string {
		return fmt.Sprintf("block %s", a.Target)
	},
}
