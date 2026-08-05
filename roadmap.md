# Talooner — roadmap

Talooner is two products: the bot, and the ecosystem work in OpenTalon that the
bot forces into existence. The second is the reason for doing this — a GitHub
reviewer is a demanding, public, real-world consumer of `talon-language`,
`talon-db`, and the plugin host. Every gap it hits is a gap a paying user would
have hit later and quieter.

Phases are cumulative. Each has an exit criterion that is a thing you can run,
not a checkbox.

---

## Phase 0 — verify the substrate

No Talooner code. Answer whether `talon-language` can express the ruleset in the
project brief, and fix it where it can't. Every one of these is a prerequisite
that will silently produce wrong reviews if assumed rather than checked.

| Question | Where | Why it blocks |
|---|---|---|
| Can a rule reference a fact as an action *argument* (`do assign "pr" "user.owner"`), not just as a literal? | `internal/executor` | The entire `user.*` namespace is useless otherwise |
| Is the evaluator three-valued? Does an unknown fact suppress a rule, and is `not <unknown>` unknown? | `talon-language/internal/executor` | Two-valued logic auto-approves PRs whose fact extraction failed. See `facts.md`, "Unset is not false". Hard blocker. |
| Do `contains` / `matches` quantify existentially over a list operand? | `internal/executor`, `grammar.ebnf:515` | Every `pr.touches_*` predicate depends on it |
| Is `{ident.field}` interpolation available in action arguments, not only labels? | `grammar.ebnf:601` | `do comment "pr" "... at {screenshots.gallery_url}"` |
| Does `talon-db` support many small short-lived fact scopes (one per PR) efficiently? | `talon-db` | Thousands of open PRs across installs |
| Can the reactive engine be woken by an external fact assertion mid-PR? | `internal/reactive` | Custom facts — the *only* path for preview/screenshot/scan rules |
| Does defeasible resolution work across two rulesets loaded together (Talooner base + tenant)? | `internal/defeasible` | `actions.md`, "Conflict resolution" |
| Does the plugin protocol (`opentalon/proto/plugin.proto`) fit a request/response with a large fact payload? | `opentalon/pkg/plugin` | The bot↔plugin seam |

**Exit:** a `.talon` file in `talon-language/examples/` expressing the brief's
ruleset, with a `.talon.test` that passes, running against synthetic PR facts.
No GitHub involved. If this can't be written, the design is wrong and it's cheap
to find out now.

Fixes land in `talon-language` / `talon-db` as their own PRs, per the workspace's
one-repo-at-a-time rule.

---

## Phase 1 — the walking skeleton

One repo, one ruleset, mechanical facts, two action types. No LLM.

**Bot** — `github.com/opentalon/talooner`
- GitHub App auth (JWT → installation token, cached)
- Webhook receive + HMAC verify + queue + 202
- `@talooner /review` command parsing, write-access check on the commenter
- Events: `issue_comment` (the trigger), `pull_request` synchronize/closed,
  `check_suite` completed — the latter two only for subscribed PRs
- Fact extraction: the built-in `pr.*` table in `facts.md`
- Actions: check run `talooner`, sticky comment
- Action executor behind an interface (the printer implementation is `rules plan`)

**Plugin** — `github.com/opentalon/talooner-plugin`
- Loads in an OpenTalon cluster, `talon-db` attached
- Owns the proto; the bot consumes the generated package as a tagged dep
- `EvaluatePR(facts, ruleset) → actions + explain`
- Per-PR fact scoping, subscription state, retention

**CLI**
- `cluster login` / `whoami`
- `rules validate`, `rules test`
- `serve`

**Exit:** open a PR on a scratch repo with no description, comment
`@talooner /review` → Talooner posts a failing check run and one comment naming
the missing requirement. Push a fix → same comment edited, check goes green,
without being asked again. Both rules from the brief's "Block incomplete PRs" and
"Auto-approve safe changes" groups working end to end.

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
- Custom facts API (`POST /api/v1/facts`) — this is what makes preview /
  screenshot / dependency-scan rules work, with the tenant's CI doing the work
- `.github/talooner/config.yaml`, `modules.yaml`, `teams.yaml`

**Exit:** the brief's ruleset minus the `llm_review` and screenshot rules runs
on a real repo, and `opentalon/*` repos dogfood it. Dogfooding is the point at
which "battle-tested" stops being an aspiration.

---

## Phase 3 — `llm_review`

The only place a model enters, and it enters as a fact.

- Plugin-side LLM call using cluster-configured tenant credentials
- Prompt in a `.txt` file, never a Go literal (`opentalon/CLAUDE.md` is explicit)
- Fixed output enum: `match` | `mismatch` | `unclear` | `too_large` | `error`
- Result stored as a fact keyed by `(pr, head_sha, doc_url, prompt_version)` —
  the fact store is the cache, no separate layer
- Per-PR conversation retained cluster-side; each review a scoped turn
- Per-PR call cap, per-tenant budget ceiling, quota surfaced via `whoami`
- Prompt-injection posture: constrained enum output, explanation rendered as
  escaped quoted text
- Per-module evaluation cardinality decided and implemented (`facts.md`,
  "`module.*`")
- VCR cassettes for tests, per the core's convention

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
  `@talooner /review`, different trigger.
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
| `talon-language` | Three-valued evaluation guarantees; list-operand string operators; interpolation in action args; cross-ruleset defeasible resolution; external fact assertion waking reactive rules |
| `talon-db` | Many-small-scopes performance; retention/TTL; audit-oriented queries |
| `opentalon` | Plugin protocol fit for large payloads; tenant credential storage + quota accounting; `whoami` capability handshake |
| `k8s-operator` | `talooner-plugin` in the instance CRD, so "run a cluster" is a manifest |

Order matters and the workspace rule applies: land core changes first, then bump
dependents. A change spanning `talon-language` and Talooner is two PRs in two
repos.

---

## Deliberately out of scope

- **A hosted service.** Permanently. Self-hosted or nothing. This caps adoption
  and removes all telemetry; both are accepted costs.
- **Merge rights, in v1.** Talooner does not merge, push, or edit CI. If that
  changes later it means new App permissions and forced re-consent from every
  installation — expensive by design, and it should stay expensive.
- **Building preview environments, browser fleets, or a vulnerability database**
  — and no dispatch mechanism to call out to things that do. The facts API
  covers it.
- **An LLM anywhere in the decision path.** It answers questions; rules decide.
