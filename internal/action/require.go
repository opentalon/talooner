package action

import "fmt"

var requireSpec = spec{
	verb: VerbRequire,
	validate: func(a Action) error {
		return required(VerbRequire, "a target", a.Target)
	},
	describe: func(a Action) string {
		return fmt.Sprintf("require %s", a.Target)
	},
}
