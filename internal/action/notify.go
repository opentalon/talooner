package action

import "fmt"

// notifySpec is `do notify <target> <text>`: a dispatch through an OpenTalon
// channel. It is the one action with no GitHub effect and no undo — a sent Slack
// message stays sent — so retraction does nothing and re-running must not
// re-send blindly. D6 owns that, and needs a configured channel to own it with.
//
// The plugin returns notify and validate_ruleset accepts it, so an action can
// arrive here before the executor exists. That is what the registry's
// completeness check is for: it fails at construction rather than on the run
// where somebody's rule finally fires.
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
