package action

import "fmt"

var emitSpec = spec{
	verb: VerbEmit,
	validate: func(a Action) error {
		return required(VerbEmit, "a name", a.Name)
	},
	describe: func(a Action) string {
		return fmt.Sprintf("emit event.%s", a.Name)
	},
}
