package action

import "fmt"

var notifySpec = spec{
	verb: VerbNotify,
	validate: func(a Action) error {
		if err := required(VerbNotify, "a target", a.Target); err != nil {
			return err
		}
		return required(VerbNotify, "text", a.Text)
	},
	describe: func(a Action) string {
		return fmt.Sprintf("notify %s: %s", a.Target, summarize(a.Text))
	},
}
