# Talooner — roadmap

Talooner is two products: the bot, and the ecosystem work in OpenTalon that the
bot forces into existence. The second is the reason for doing this — a GitHub
reviewer is a demanding, public, real-world consumer of `talon-language`,
`talon-db`, and the plugin host. Every gap it hits is a gap a paying user would
have hit later and quieter.

Phases are cumulative. Each has an exit criterion that is a thing you can run,
not a checkbox.

---

## Phase 0 — verify the substrate ✅ answered, substrate fixed

No Talooner code, and **no bot work at all**. Phase 0 is entirely a question
about `talon-language` and `talon-db`: can they express the ruleset in the
project brief, and if not, fix them. Every item silently produces wrong reviews
if assumed rather than checked.

Answered 2026-08-06; the fixes it filed landed 2026-08-07. The full results table
lives in
[`talooner-plugin/roadmap.md`](https://github.com/opentalon/talooner-plugin/blob/main/roadmap.md),
since it's the plugin that depends on every answer. The two the bot has a stake
in:

- **Three-valued evaluation: no.** The evaluator is two-valued, so a PR whose
  fact extraction failed sails through `not is "critical_path"` and gets
  auto-approved. v1 accepts this as a known risk rather than blocking on it —
  see `facts.md`, "Unset is false", which is required reading before writing any
  rule that grants something.
- **List operands: no then, yes now.** `contains` / `matches` were string-only,
  which made every `pr.touches_*` predicate unimplementable as designed.
  [`talon-language#158`](https://github.com/opentalon/talon-language/issues/158)
  landed 2026-08-07 and they quantify over lists on both backends. The residue
  for the bot: `matches` is a substring scan, not a glob, so path predicates use
  `contains` / `ends_with` — see `facts.md`.

**Exit:** a `.talon` file in `talon-language/examples/` expressing the brief's
ruleset, with a `.talon.test` that passes, running against synthetic PR facts.
No GitHub involved. **Unblocked** — the design survived verification unchanged
and the substrate fixes have landed, so writing the exit artifact is the next
piece of work and the gate on starting phase 1.

The fixes landed in `talon-language` / `talon-db` / `opentalon` as their own PRs,
per the workspace's one-repo-at-a-time rule.

---

## Phase 1 — the walking skeleton

One repo, one ruleset, mechanical facts, two action types. No LLM.

**Action** — `github.com/opentalon/talooner`
- `action.yml` + pinned Docker image; inputs `opentalon_host`,
  `opentalon_api_key`, `github_token`
- Event parsing from `GITHUB_EVENT_PATH` → `{repo, pr, trigger}`
- `@talooner /review` command parsing, write-access check on the commenter.
  Always a full re-evaluation (decision 18); no `--force` yet, nothing to spend
- Triggers: `issue_comment` (the entry point), `pull_request`
  synchronize/reopened/closed, `check_suite` completed — the latter two exit 0
  unless the PR is subscribed
- Fact extraction over `GITHUB_TOKEN`: the built-in `pr.*` table in `facts.md`
- Actions: check run `talooner`, sticky comment
- Action executor behind an interface (the printer implementation is `rules plan`)
- Reference workflow, published and documented, since it's now part of the
  product surface rather than deployment trivia

What phase 1 explicitly does **not** build, and never will: webhook receiver,
HMAC verification, delivery queue, App auth chain, `serve`. That is roughly a
third of the original phase-1 bot, deleted by decision 1.

**Plugin** — tracked in
[`talooner-plugin/roadmap.md`](https://github.com/opentalon/talooner-plugin/blob/main/roadmap.md).
What the action needs from it in this phase: `evaluate_pr`, `is_subscribed`,
`set_subscription`, `validate_ruleset`, `whoami` (including a protocol version,
since action and cluster now version independently).

**CLI**
- `cluster login` / `whoami`
- `init` — write the workflow, the starter ruleset, and the secrets
- `rules validate`, `rules test`

**Exit:** open a PR on a scratch repo with no description, comment
`@talooner /review` → Talooner posts a failing check run and one comment naming
the missing requirement. Push a fix → same comment edited, check goes green,
without being asked again. Both rules from the brief's "Block incomplete PRs" and
"Auto-approve safe changes" groups working end to end, with nothing running
anywhere except a cluster and a runner that GitHub started.

The single highest-value thing here is not any feature — it's that the whole
loop exists and every later phase is an addition to a running system.

---

## Phase 2 — the full deterministic vocabulary

Everything in the brief that doesn't need a model.

- `pr.touches_*` via Talon-native path predicates
- `review.*` facts, `pull_request_review` events, team mapping via `teams.yaml`
- Actions: `approve`, `block`, `assign`, `require`, `emit`
- Review dismissal / retraction semantics (`actions.md`, "Reversibility")
- Defeasible conflict resolution + the Talooner `strict` base ruleset
- Fork safety: base-branch ruleset, head-branch plan runs
- `rules plan --repo --pr` against a live PR
- `user.*` facts: CODEOWNERS parsing, `modules.yaml` owners, self-review detection
- `@talooner /stop`, `/why`, `/plan` commands
- Custom facts API (`POST /api/v1/facts`, on the **cluster** — there is no bot
  endpoint) — this is what makes preview / screenshot / dependency-scan rules
  work, with the tenant's CI doing the work. Read at the next evaluation;
  nothing wakes on its own (decision 20)
- `.github/talooner/config.yaml`, `modules.yaml`, `teams.yaml`

**Exit:** the brief's ruleset minus the `llm_review` and screenshot rules runs
on a real repo, and `opentalon/*` repos dogfood it. Dogfooding is the point at
which "battle-tested" stops being an aspiration.

---

## Phase 3 — `llm_review`

The only place a model enters, and it enters as a fact. **Almost entirely
plugin-side** — the bot never sees an LLM. Details in
[`talooner-plugin/llm-review.md`](https://github.com/opentalon/talooner-plugin/blob/main/llm-review.md).

The bot's share of this phase:

- Surface remaining quota from `whoami` and warn at ruleset-load time when a
  ruleset uses `llm_review` on a cluster with no configured provider
- Render `llm_review.explanation` as escaped, quoted text in a comment, never
  interpreted — it is model output derived from an attacker-controllable diff
- Send `pr.diff` size-capped, asserting `pr.diff_truncated` past the cap
- `@talooner /review --force` — cache-bypass flag on `evaluate_pr`. Lands here,
  not phase 1: before `llm_review` exists, re-evaluation costs nothing and there
  is nothing to bypass. Same write-access gate as everything else

**Exit:** a PR whose code contradicts its module docs gets blocked with a
specific, quotable explanation — and re-running at the same sha makes no second
API call and produces byte-identical output.

---

## Phase 4 — ecosystem

The part that makes this more than one bot.

- **Ruleset sharing.** Community rulesets, versioned and importable
  (`talon-language/internal/imports` already exists). "Import the OWASP ruleset"
  is the network effect, and it's the only one available given there's no hosted
  service to accrue one.
- **Org-level rulesets.** One ruleset many repos import, optionally
  non-overridable by the repo.
- **Auto-review on PR open**, opt-in per repo. Same subscription path as
  `@talooner /review`, one more trigger in the workflow.
- **Reactive wake via `repository_dispatch`**, if manual `/review` after an
  externally POSTed fact proves annoying in practice (decision 20). Additive: one
  trigger, one documented CI snippet, no change to the evaluation path.
- **Reference `.github/talooner/` templates** — a Go repo starter, a Rails repo
  starter — so onboarding is copy-paste rather than blank page.
- **Example CI workflows** that push preview / screenshot / scan facts, since
  that's how those rules are meant to fire.
- `k8s-operator` support so "run a cluster" is a manifest, not a runbook.

---

## What this drags into OpenTalon

Tracked explicitly, because it's half the work and lands in other repos:

| Repo | Likely work |
|---|---|
| `talon-language` | **Landed:** list-operand string operators ([#158](https://github.com/opentalon/talon-language/issues/158)), import shadowing of `strict` rules ([#159](https://github.com/opentalon/talon-language/issues/159)). **Verified fine:** interpolation in action args, cross-ruleset defeasible resolution. **Declined for v1:** three-valued evaluation guarantees. **Open, not blocking:** glob path matching; external fact assertion waking reactive rules (phase 4) |
| `talon-db` | Many-small-scopes performance; retention/TTL — still a sweeper, there is no bulk delete; audit-oriented queries |
| `opentalon` | **Landed:** plugin protocol fit for large payloads ([#325](https://github.com/opentalon/opentalon/issues/325) — 32 MiB default). **Open:** tenant credential storage + quota accounting; `whoami` capability handshake **plus protocol version**; a gRPC endpoint safe to expose publicly with TLS, since the caller is now a GitHub runner rather than a process on the same box |
| `k8s-operator` | `talooner-plugin` in the instance CRD, so "run a cluster" is a manifest |

Order matters and the workspace rule applies: land core changes first, then bump
dependents. A change spanning `talon-language` and Talooner is two PRs in two
repos.

---

## Deliberately out of scope

- **A hosted service.** Permanently. Self-hosted or nothing. This caps adoption
  and removes all telemetry; both are accepted costs.
- **A long-running bot process of any kind** — no webhook receiver, no daemon, no
  `serve`. The GitHub half is an Action or it doesn't exist. The cost is decision
  20: nothing is alive to react between events.
- **Merge rights, in v1.** Talooner does not merge, push, or edit CI. If that
  changes later it means new App permissions and forced re-consent from every
  installation — expensive by design, and it should stay expensive.
- **Building preview environments, browser fleets, or a vulnerability database**
  — and no dispatch mechanism to call out to things that do. The facts API
  covers it.
- **An LLM anywhere in the decision path.** It answers questions; rules decide.
