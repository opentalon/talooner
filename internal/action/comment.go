package action

import "fmt"

// commentSpec is `do comment "pr" <text>`: one sticky comment per topic, edited
// in place rather than re-posted, so a PR with 30 pushes carries one comment
// with current state (actions.md). Every comment action on a PR renders into
// that one comment, so the body is built from the whole action set in
// internal/comment rather than performed here one action at a time; this spec
// is what validates and plans them.
//
// The text arrives with {attr.x} interpolation already resolved. Empty text is
// rejected: a comment saying nothing is indistinguishable from a rule that
// matched and had nothing to say, and it still notifies everyone watching.
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
