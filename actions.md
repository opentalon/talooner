# Talooner — actions

What `do <action>` does on the GitHub side.

Talooner has no merge rights and no `contents: write`. Every action below is
advisory or informational. The repo owner decides, via branch protection,
whether any of it gates a merge.

## v1 — native actions

A rule declares these with `do <verb> <args>`
([`talon-language/docs/actions.md`](https://github.com/opentalon/talon-language/blob/main/docs/actions.md)).
The engine resolves each argument against the PR's facts and returns the action
as data; the bot performs it.

| Talon | GitHub effect |
|---|---|
| `do approve "pr"` | Review with event `APPROVE`, plus check run `talooner` → `success` |
| `do block "pr.merge"` | Check run `talooner` → `failure`, plus review `REQUEST_CHANGES` |
| `do comment "pr" <text>` | Sticky comment, edited in place |
| `do assign "pr" <team-or-user>` | Assignee via Issues API |
| `do require <review.target>` | Review request to the mapped team/user |
| `do notify <target> <text>` | Dispatch through an OpenTalon channel (Slack, etc.) |
| `do emit <name>` | Asserts fact `event.<name> = true`; no GitHub effect |

Arguments may be literals (`"pr"`), fact references (`attr "user.owner"`), or
strings carrying interpolation (`"owned by {attr.user.owner}"`). Both are
resolved cluster-side before the action reaches the bot, so the executor never
looks a fact up.

**Talon does not validate verb names** — the vocabulary belongs to the host. A
misspelled `do aprove "pr"` parses cleanly and would otherwise vanish, so
`validate_ruleset` rejects anything outside this table
(`talooner-plugin/engine.md`, "The verb list is ours to enforce"). The bot
enforces it again at execution: an action whose verb has no executor is a hard
error, never a no-op.

### `approve` is advisory, and that's the point

A bot approval does not satisfy "require N approvals" in most branch-protection
configurations, and shouldn't. The intended flow is:

```
open PR → talooner: REQUEST_CHANGES + specific comments
       → developer fixes
       → talooner: APPROVE  (mechanical checks clean)
       → human reviews, relying on talooner's comments as the pre-pass
```

Talooner is an intelligent linter with an opinion, positioned before human
review, not instead of it. The check run is the machine-readable half; the
review and comments are the human-readable half.

### Check run

One check run named `talooner` per head sha, updated in place, never duplicated:

| Engine outcome | Conclusion |
|---|---|
| a `block` fired | `failure` |
| an `approve` fired, no `block` | `success` |
| rules fired, none decisive | `neutral` |
| ruleset invalid / extraction failed | `neutral` + annotation (**not** `failure`) |

Infrastructure failure resolving to `neutral` rather than `failure` is
deliberate: if Talooner is broken, it must not become a merge blocker for a repo
that marked its check required. Fail open on *Talooner's own* faults; fail
closed on *policy* outcomes.

Ruleset syntax errors surface as check-run annotations pinned to the offending
line in the `.tln` file, plus one summary comment.

### Sticky comments

One comment per logical topic, identified by an HTML marker:

```html
<!-- talooner:v1:review -->
```

Re-running edits that comment rather than posting a new one. A PR with 30 pushes
gets one comment with current state, not 30 comments. Comments whose triggering
condition no longer holds are edited to a resolved state, not deleted — the
history is the audit trail.

Template interpolation (`"screenshots at {screenshots.gallery_url}"`) uses
Talon's existing `{ident.field}` label interpolation (`grammar.ebnf:601`).
Confirm it's available in action-argument position, not only in labels.

### Reversibility

Facts retract; GitHub side effects mostly don't. Explicit per action:

| Action | Reversible? | On retraction |
|---|---|---|
| `approve` | yes | dismiss the review, check run → `neutral` |
| `block` | yes | check run → `success`/`neutral`, dismiss `REQUEST_CHANGES` |
| `comment` | partly | edit to resolved state; never delete |
| `assign` | yes | remove assignee |
| `require` | yes | withdraw review request |
| `notify` | **no** | one-way; a sent Slack message stays sent |
| `emit` | n/a | fact retraction only |

Retracting `assign` and `require` needs one thing the others do not: knowing
which of them are Talooner's. GitHub reports an assignee the bot added and one a
maintainer added identically, so Talooner keeps a **ledger** of what it added, in
a sticky comment of its own (`<!-- talooner:v1:state -->`), and takes back only
what the ledger claims. A deleted or unreadable ledger therefore means Talooner
owns nothing and removes nothing: the failure direction is a stale assignee,
never one taken away from the person who put it there.

Until `teams.yaml` lands (E1), a `require` target maps to GitHub by its own last
segment — `review.security` is the team `security` in the repository's
organisation, `review.@alice` is the user alice — and a target that resolves to
neither fails the run by name rather than being skipped.

So: a PR that was approved and then grows past 500 lines has its approval
dismissed on the next run. This is why the bot re-derives all facts and
re-evaluates from scratch on every event rather than applying deltas.

## Conflict resolution happens plugin-side

`approve` and `block` can both fire. They are resolved by Talon's defeasible
machinery inside the plugin, not by an ad-hoc "block wins" in the bot —
`strict` > `overrides` > priority, plus a `strict` base ruleset Talooner always
loads. Full rules in
[`talooner-plugin/engine.md`](https://github.com/opentalon/talooner-plugin/blob/main/engine.md),
"Conflict resolution".

What lands on the bot: an unresolved tie returns **both** actions plus a warning.
Since `block` produces a `failure` check run and `approve` a `success` on the
same check, the check-run writer applies block-wins as a **last-resort** tiebreak
and surfaces the plugin's warning as a comment telling the maintainer to
disambiguate with `overrides` or `priority`. The warning is the real product; the
tiebreak is just so the check run has one value.

## Not implemented: `deploy_preview`, `screenshot`, `scan_dependencies`

Each is a product on its own — build-and-host infrastructure, a headless browser
fleet, a vulnerability database. Talooner builds none of them, and there is no
dispatch mechanism to call out to something that does. No plans for one.

They're handled by the **facts API** instead. The tenant's own CI does the work
and reports the result; the engine reacts:

```
tenant's CI builds a preview (their workflow, their infra, their choice of tool)
  └─ POST https://<your-cluster>/api/v1/facts
       {"preview.status": "deployed", "preview.url": "..."}
     └─ fact lands in talon-db, scoped to the PR
        └─ next evaluation: `when attr "preview.status" == "deployed"` fires
           └─ Talooner comments, requires design review, whatever the rules say
```

The facts API is the **cluster's** endpoint, not the bot's — there is no bot to
POST to (decision 1). Your CI needs the cluster URL and API key, which it
already has as secrets if it's the same repo.

"Next evaluation" is doing real work in that diagram: in v1 nothing wakes on an
externally asserted fact. Someone comments `@talooner /review` and the fact is
picked up then (decision 20, `architecture.md`, "No reactive wake in v1"). A
tenant who wants it prompt can have their CI POST the fact and then trigger the
workflow themselves — but v1 ships neither the trigger nor a recipe for it.

This is strictly better than a dispatch action for the same outcome: one
mechanism instead of two, no webhook registry, no timeout semantics, no
`dispatch.*.failed` fact vocabulary, and the tenant keeps their existing CI
rather than reimplementing it behind a Talooner action.

Consequences:

- The brief's ruleset **parses and runs as written**. Rules gated on
  `preview.status` or `screenshots.status` simply never fire until something
  asserts those facts. No error, no special-casing, no stub actions.
- `do deploy_preview "pr"` is not a verb Talooner serves. It *parses* — Talon
  accepts any verb — so this is enforced by `validate_ruleset` rejecting it by
  name, with a pointer to the facts API. Better than accepting it and doing
  nothing, which is exactly what would happen if nobody checked.
- Talooner's half of the flow — reacting to preview and screenshot facts,
  requiring design review, posting the gallery link — works from phase 2 with no
  new machinery, at the latency of the next `/review`.

Mapping of the brief's ruleset to phases:

| Rule group | Phase |
|---|---|
| Auto-approve safe changes | v1 |
| Block incomplete PRs | v1 |
| Require human review for critical paths / large PRs | v1 |
| Security review for new dependencies (the `require` half) | v1 |
| LLM documentation review | v1.5 |
| Block/approve on `llm_review.result` | v1.5 |
| Reacting to preview / screenshot / scan facts pushed by tenant CI | v2 |
| `do deploy_preview` / `do screenshot` / `do scan_dependencies` as verbs | never |

## Workflow permissions

Declared in the tenant's own workflow, not in an App registration — so they sit
in a diff, reviewable by the people they affect:

```yaml
permissions:
  pull-requests: write   # reviews, comments, assignees, review requests
  checks: write          # the `talooner` check run
  contents: read         # ruleset, config files, diffs
```

`GITHUB_TOKEN` starts from the repo's default permission set; this block narrows
it for the job. Everything not listed is unavailable to the run — including
`contents: write`, `actions`, and `administration`. Talooner cannot push, cannot
merge, cannot change settings, cannot edit CI, and this is enforced by a token
GitHub mints rather than by Talooner declining to call an endpoint.

If a future feature needs a wider permission, the tenant edits the workflow. That
is more honest than an App re-consent dialog: a diff, reviewed by the repo's own
maintainers, in the repo it affects.

Team membership for `review.<team>.approved` is the one thing `GITHUB_TOKEN`
cannot read — org membership is out of a repo-scoped token's reach. Options,
resolved when `review.*` facts land in phase 2: derive team approval from
CODEOWNERS review requests (no extra permission, covers the common case), or take
an optional PAT secret for orgs that need real team resolution. The default must
work with no extra secret.

## Triggers

| Event | Used for |
|---|---|
| `issue_comment` (created) | **The v1 entry point** — `@talooner /review [--force]`, `/stop`, `/why`, `/plan` |
| `pull_request` (synchronize, reopened, closed) | Re-evaluate a subscribed PR; unsubscribe on close |
| `pull_request_review` (submitted) | `review.*` facts |
| `check_suite` (completed) | `pr.tests_passing`, `pr.lint_passing` |

`pull_request opened` deliberately isn't in the list — Talooner waits to be asked
(`architecture.md`, "Invocation"). Auto-review on open, opt-in per repo, is a
later phase; it adds a trigger and reuses the same subscription path.

Not used: `pull_request_target`. It runs with secrets against a fork's code and
is the standard way to get this wrong. See `auth.md`, "Secrets and fork PRs".

Every trigger except `issue_comment` runs only to serve an already-subscribed PR,
and exits 0 otherwise — a skipped job, not a red X.

Commands are honoured only from users with write access to the repo, checked
against the installation's permission API on every command. Without that gate,
any GitHub account could comment `@talooner /review` on a public repo and spend
the maintainer's LLM budget at will.
