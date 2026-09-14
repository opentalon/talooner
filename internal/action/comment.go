package action

import "fmt"

var commentSpec = spec{
	verb: VerbComment,
	validate: func(a Action) error {
		if err := required(VerbComment, "a target", a.Target); err != nil {
			return err
		}
		return required(VerbComment, "text", a.Text)
	},
	describe: func(a Action) string {
		return fmt.Sprintf("comment on %s: %s", a.Target, summarize(a.Text))
	},
}
