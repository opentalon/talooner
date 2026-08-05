# Talooner — actions

What `do <action>` does on the GitHub side.

Talooner has no merge rights and no `contents: write`. Every action below is
advisory or informational. The repo owner decides, via branch protection,
whether any of it gates a merge.

## v1 — native actions

| Talon | GitHub effect |
|---|---|
| `approve "pr"` | Review with event `APPROVE`, plus check run `talooner` → `success` |
| `block "pr.merge"` | Check run `talooner` → `failure`, plus review `REQUEST_CHANGES` |
| `comment "pr" <text>` | Sticky comment, edited in place |
| `assign "pr" <team-or-user>` | Assignee via Issues API |
| `require <review.target>` | Review request to the mapped team/user |
| `notify <target> <text>` | Dispatch through an OpenTalon channel (Slack, etc.) |
| `emit <name>` | Asserts fact `event.<name> = true`; no GitHub effect |

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
line in the `.talon` file, plus one summary comment.

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

So: a PR that was approved and then grows past 500 lines has its approval
dismissed on the next run. This is why the bot re-derives all facts and
re-evaluates from scratch on every event rather than applying deltas.

## Conflict resolution — defeasible

`approve` and `block` can both fire. Resolved by Talon's defeasible machinery
(`talon-language/docs/defeasible.md`), not by an ad-hoc "block wins" in
Talooner:

- Safety rules are declared `strict` — they always fire, never defeated.
- Priority ordering `CRITICAL > HIGH > MEDIUM > LOW`, default `MEDIUM`.
- `overrides "Rule name"` for explicit defeat, walked transitively.
- An unresolved tie fires both and warns.

Talooner ships a small **base ruleset** it always loads at low precedence,
declaring the non-negotiables as `strict`, so a tenant ruleset can't
accidentally approve something structurally unreviewable:

```talon
strict rule "Never approve a PR with unresolved conflicts" { ... }
strict rule "Never approve while required checks are still running" { ... }
```

An unresolved tie between a tenant `approve` and a tenant `block` resolves
conservatively: both fire, and since `block` produces a `failure` check run
while `approve` produces `success` on the same check, the check-run writer
applies block-wins as a **last-resort** tiebreak and emits a ruleset warning
telling the maintainer to disambiguate with `overrides` or `priority`. The
warning is the real product; the tiebreak is just so the check run has one
value.

## Not implemented: `deploy_preview`, `screenshot`, `scan_dependencies`

Each is a product on its own — build-and-host infrastructure, a headless browser
fleet, a vulnerability database. Talooner builds none of them, and there is no
dispatch mechanism to call out to something that does. No plans for one.

They're handled by the **facts API** instead. The tenant's own CI does the work
and reports the result; the engine reacts:

```
tenant's CI builds a preview (their workflow, their infra, their choice of tool)
  └─ POST /api/v1/facts {"preview.status": "deployed", "preview.url": "..."}
     └─ engine wakes, `when "preview.status" == "deployed"` fires
        └─ Talooner comments, requires design review, whatever the rules say
```

This is strictly better than a dispatch action for the same outcome: one
mechanism instead of two, no webhook registry, no timeout semantics, no
`dispatch.*.failed` fact vocabulary, and the tenant keeps their existing CI
rather than reimplementing it behind a Talooner action.

Consequences:

- The brief's ruleset **parses and runs as written**. Rules gated on
  `preview.status` or `screenshots.status` simply never fire until something
  asserts those facts. No error, no special-casing, no stub actions.
- `do deploy_preview "pr"` as a verb doesn't exist. A ruleset using it fails
  validation with "unknown action" and a pointer to the facts API. Better than
  accepting it and doing nothing.
- Talooner's half of the flow — reacting to preview and screenshot facts,
  requiring design review, posting the gallery link — works from phase 2 with no
  new machinery.

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

## App permissions

Minimum for v1:

| Permission | Level | For |
|---|---|---|
| Pull requests | write | reviews, comments, assignees, review requests |
| Checks | write | the `talooner` check run |
| Contents | read | ruleset, config files, diffs |
| Metadata | read | mandatory |
| Members | read | team membership for `review.<team>.approved` |

Explicitly **not** requested: `contents: write`, `administration`, `workflows`.
Talooner cannot push, cannot merge, cannot change settings, cannot edit CI. If a
future feature needs one of these, it's a new major version and an explicit
re-consent by every installation — GitHub forces that, and it's a feature.

Webhook events subscribed:

| Event | Used for |
|---|---|
| `issue_comment` (created) | **The v1 entry point** — `@talooner /review`, `/stop`, `/why`, `/plan` |
| `pull_request` (synchronize, reopened, edited, closed) | Re-evaluate a subscribed PR; unsubscribe on close |
| `pull_request_review` | `review.*` facts |
| `check_suite`, `check_run` (completed) | `pr.tests_passing`, `pr.lint_passing` |
| `installation`, `installation_repositories` | Install lifecycle |

`pull_request opened` is subscribed but does **not** trigger a review in v1 —
Talooner waits to be asked (`architecture.md`, "Invocation"). Auto-review on
open, opt-in per repo, is a later phase; it reuses the same subscription path.

Commands are honoured only from users with write access to the repo, checked
against the installation's permission API on every command. Without that gate,
any GitHub account could comment `@talooner /review` on a public repo and spend
the maintainer's LLM budget at will.
