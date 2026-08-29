# Talooner — facts

Everything a rule can match on. Facts live in `tln-db`, scoped per pull
request, asserted by the bot and read by the engine.

This file is the **extraction** side: what the bot produces and where each fact
comes from. What happens to those facts once they reach the cluster — scoping,
lifetime, namespace enforcement, retention — is
[`talooner-plugin/facts.md`](https://github.com/opentalon/talooner-plugin/blob/main/facts.md).

## Where configuration lives

In the repo being reviewed. Talooner reads it with the `GITHUB_TOKEN` the run
already has; there is nowhere else it could live without inventing a second
source of truth outside version control.

```
your-repo/
  .github/
    workflows/
      talooner.yml       ← the trigger + the two secrets
    talooner/
      rules.tln        ← the review policy
      rules.tln.test   ← tests for the policy
      config.yaml      ← check-name patterns, caps, toggles
      modules.yaml     ← module → docs URL / owner
      teams.yaml       ← logical team → GitHub team
```

So yes: to add Talooner to your repo you commit these files to your repo. The
policy is versioned, reviewed, diffable, and testable like any other code —
which is most of the point. A review policy that lives in a web dashboard is a
policy nobody can review.

Read from the **base** branch when governing writes (`architecture.md`, "Fork
safety"). Absent `.github/talooner/` → the bot replies to `@talooner /review`
with a one-comment "no ruleset found" and does nothing else.

## Namespaces

| Namespace | Source | Lifetime |
|---|---|---|
| `pr.*` | GitHub API + diff, extracted by the bot | per PR, re-asserted each run |
| `user.*` | CODEOWNERS + `modules.yaml` + GitHub | per PR run |
| `repo.*` | repo config / GitHub metadata | per PR run |
| `review.*` | GitHub API (review list), extracted by the bot | per PR, re-asserted each run |
| `llm_review.*` | plugin, from an LLM call | pinned to head sha |
| `module.*`, `team.*` | tenant-supplied lookup tables | static per repo |
| custom | pushed by CI via the facts API | until PR closes |

## Built-in `pr.*` facts

Pure functions of the PR at a given head sha. Always asserted, never absent —
with three deliberate exceptions, all of them "we do not know", not "it is
false":

- `pr.mergeable` is omitted while GitHub still reports `null` (see below).
- `pr.tests_passing` and `pr.lint_passing` are omitted whenever the fact has no
  determined value — the patterns in `config.yaml` match no check, any matched
  check is still running, or a matched check has a conclusion the bot does not
  recognise (see below). A positive condition on an omitted fact simply does not
  fire (facts.md, "Unset is false"), which is exactly the gate's safe state.

| Fact | Type | Source |
|---|---|---|
| `pr.number` | int | payload |
| `pr.head_sha`, `pr.base_sha` | string | payload |
| `pr.author` | string | payload |
| `pr.is_fork` | bool | `head.repo.full_name != base.repo.full_name` |
| `pr.draft` | bool | payload |
| `pr.title`, `pr.body` | string | payload |
| `pr.has_description` | bool | `len(strings.TrimSpace(body)) > 0` |
| `pr.lines_changed` | int | `additions + deletions` |
| `pr.additions`, `pr.deletions` | int | payload |
| `pr.files_changed` | int | payload |
| `pr.changed_files` | list\<string\> | Files API, paginated |
| `pr.commits` | int | payload |
| `pr.labels` | list\<string\> | payload |
| `pr.mergeable` | bool | payload — see below |
| `pr.checks_pending` | bool | check runs + statuses — see below |
| `pr.tests_passing` | bool | check runs — see below |
| `pr.lint_passing` | bool | check runs — see below |
| `pr.diff` | string | Files API patches, concatenated and size-capped at 1 MiB |
| `pr.diff_truncated` | bool | true when `pr.diff` hit the cap and was cut short (issue #9) |
| `pr.new_dependencies` | int | manifest diff — see below |
| `pr.upgraded_dependencies` | int | manifest diff — see below |

### `pr.mergeable` and `pr.checks_pending`

These two are the **only** attributes the plugin's `strict` base ruleset matches
(`talooner-plugin/internal/ruleset/base/talooner.tln`): `pr.mergeable == false`
blocks on unresolved conflicts, `pr.checks_pending == true` blocks while required
checks are still running. The next person to prune an unused fact should find
that out here, not from a PR getting approved over a merge conflict.

- `pr.mergeable` is GitHub's `mergeable` field. **It is nullable, and that is
  the whole decision.** GitHub computes mergeability asynchronously and returns
  `null` on a PR whose background job has not finished — the common case right
  after a push, which is when a run fires. The bot polls the PR endpoint a
  bounded number of times (`ResolveMergeable`); what is still `null` past that
  budget is **omitted, not asserted false**. "We do not know yet" is not "there
  are conflicts", and asserting `false` there would block a clean PR. Both base
  rules are positive conditions, so an omitted `pr.mergeable` simply does not
  fire — the strict floor stays inert on exactly the PRs it exists for. This is
  the one built-in fact that may be absent, on purpose.
- `pr.checks_pending` is derived, not a payload field: `true` when any check run
  on the head sha is `queued`/`in_progress` **or** any commit status is
  `pending`. It is asserted `false` explicitly when everything has settled — a PR
  with zero check runs and zero statuses is a settled PR, not an unknown one.
  Unlike C3's `tests_passing`/`lint_passing` it is not filtered by the tenant's
  check patterns: a pending check nobody named is still a pending check, and the
  base rule is about not reviewing a moving target. It shares C3's check-run
  fetch; whichever landed first owns the API call, and this is the one.

### `tests_passing` / `lint_passing`

Neither is knowable generically. Both are derived from check runs and statuses
on the head sha, matched against tenant-declared name patterns:

```yaml
# .github/talooner/config.yaml
checks:
  tests: ["test", "ci/*", "*unit*"]
  lint:  ["lint", "golangci-lint", "rubocop"]
```

Semantics, and they matter:

- all matched checks `success` → `true`
- any matched check `failure`/`timed_out`/`cancelled` → `false`
- any still `queued`/`in_progress` → **fact unset**, not `false`
- no check matches any pattern → **fact unset**, not `true`

The matched set can also contain a check whose conclusion the bot does not
recognise — GitHub emits `neutral`, `skipped`, `stale` and `action_required` on
completed checks, none of which is "success" or a named failure. Any matched
check with one of those conclusions **unsettles** the fact: it is left unset
rather than guessed. Precedence when a mixed set lands: pending beats everything
(a gate must not fire while CI is in flight); a recognised failure beats an
unknown conclusion (a PR with one red test and one neutral test is *not*
passing); an unknown conclusion beats success-only (we do not claim passing on a
check whose outcome we cannot name). The alternative — unset on any unknown even
when another matched check failed — would let a red test go unblocked behind a
neutral one, which is the worse direction.

The patterns come from `config.yaml` (`checks.tests` / `checks.lint`), read from
the **base** branch at its own ref like the ruleset (architecture.md, "Fork
safety") — C3. A missing file is an answer: no patterns, so both facts stay
unset. A present-but-unparseable file fails the run, the same fail-open shape as
a broken ruleset (the bot's own fault is a neutral check, never a policy
outcome).

See "Unset is false" below. A rule that auto-approves on
`attr "pr.tests_passing" == true` must not fire while CI is still running, and must
not fire on a repo that has no tests at all — leaving the fact unset achieves
that, because a positive condition on an unset fact doesn't match.

### `pr.diff` and `pr.diff_truncated`

`pr.diff` is the concatenated unified diffs of every file the PR touches, pulled
from the same Files API `patch` field `pr.changed_files` reads the names from.
Binary files (null `patch`) contribute nothing, and the files are joined with a
newline so each one's `diff --git` header stays intact.

It is **size-capped at 1 MiB** (`github.DiffMaxBytes`), a default the
`config.yaml` cap will override in E1. The cap is file-granular: whole files are
appended while they fit, and the moment the next file would push past the limit
the loop stops. **`pr.diff_truncated` is a first-class fact** — a rule can match
on it (`attr "pr.diff_truncated" == true`), and v1.5's `llm_review` depends on it
being honest. Both facts are always asserted:

- a diff that fits is asserted complete, `pr.diff_truncated` false;
- a diff cut off at the cap is asserted with `pr.diff_truncated` true — it must
  never read as complete;
- a single file larger than the cap yields an empty `pr.diff` and
  `pr.diff_truncated` true, because shipping half a patch is worse than shipping
  none with the flag set;
- a PR whose changes are all binary gets an empty `pr.diff` and
  `pr.diff_truncated` false, which is the honest answer — there is nothing
  textual to show.

The unhappy paths that define the cap — exactly at, one byte over, one byte under,
a binary file, and individually-small patches that collectively exceed it — all
behave so that a truncated diff is always flagged, never silent.

### `pr.new_dependencies` / `pr.upgraded_dependencies`

Counts of dependencies newly added, and version-bumped, across recognised
manifests, parsed from `pr.diff` (the diff C2 already fetches — no extra API
call). Both are asserted always: a PR that touches none of either gets `0`,
which is the honest answer and reads as "no security review needed", not a
dead extractor.

A dependency name both added and removed in the same manifest file is a
version bump — it counts toward `pr.upgraded_dependencies`, never
`pr.new_dependencies`. A name only removed, with no matching add, is a plain
removal and counts toward neither (issue #11).

Recognised manifests (matched by basename, in any directory):

- `go.mod` — `require` entries, both block (`require ( … )`) and single-line
  (`require module vX`) forms;
- `package.json` — entries inside `dependencies`, `devDependencies`,
  `optionalDependencies` and `peerDependencies` only (a top-level field of the
  same `"key": "value"` shape is not a dependency);
- `Gemfile` — `gem` declarations;
- `requirements.txt` — spec lines (`name==1.2.3`, `name>=1.0`, …);
- `Cargo.toml` — keys inside `[dependencies]`, `[dev-dependencies]` and the
  per-dependency `[dependencies.foo]` form (the `[package]` table's
  `name`/`version`/`edition` are not dependencies).

Lockfiles are **not** read — `go.sum`, `package-lock.json`, `yarn.lock`,
`pnpm-lock.yaml`, `Gemfile.lock`, `Cargo.lock`, `composer.lock`, `poetry.lock`,
`Pipfile.lock` and anything ending in `.lock`. Lockfile churn is a *consequence*
of a manifest change, so counting it would double-count.

**Version bumps are not new dependencies.** Within a file, a dependency whose
name is both added and removed in the same diff is an upgrade, and is excluded
from the count. Only net-new names across the recognised manifests are counted.

The recognised set is deliberately narrow: a format the extractor misreads
counts the wrong number of dependencies, and no base rule depends on this fact,
so a format it does not recognise is safer left at zero than guessed. Extend
`manifestNames` in `internal/facts/dependencies.go` to add one.

**An unparseable manifest fails the whole extraction, not a confident zero**
(issue #11). A manifest that shows real additions/deletions in the PR's
changed-file stats (C2 already fetches these too — no extra API call) but
never appears in `pr.diff` — GitHub's Files API returns a null patch for
binary or oversized files, and `pr.diff` drops those files entirely — cannot
be read at all, so `0` here would be a guess, not the honest "no dependency
changes" answer. That case errors the whole `PR` extraction, same as any other
extractor that cannot produce its full set (package comment,
`internal/facts/facts.go`). A manifest reformatted with no semantic change is
unaffected: it's present in the diff, just with nothing the parser reads as a
dependency line, so it still reads `0`.

## `user.*` — who is responsible for this code

The person a rule wants to tag. Distinct from `pr.author`: the author wrote the
change, the responsible user owns the code being changed. Often different people,
and the whole reason to have both.

| Fact | Type | Source |
|---|---|---|
| `user.owner` | string | Primary owner of the touched code — CODEOWNERS, else the most recent toucher |
| `user.owners` | list\<string\> | All owners across touched paths |
| `user.author` | string | Alias of `pr.author`, for symmetry |
| `user.reviewer` | string | Currently requested reviewer, if one |
| `user.last_toucher` | string | Author of the most recent prior commit to the touched paths |

Resolution order for `user.owner`, first hit wins:

1. `.github/CODEOWNERS` — GitHub's own mechanism, already in most repos, already
   the thing people maintain. Reusing it beats inventing a parallel ownership
   file that drifts.
2. `user.last_toucher` — the author of the most recent commit to touch any of
   the changed paths, from `git log` on the base branch (a fork PR's own
   commits are never in view).
3. Unset. Not `pr.author` — falling back to the author would let a rule
   "escalate to the owner" silently escalate to the person who wrote the change,
   which is exactly the wrong answer and is invisible when it happens.

`modules.yaml`'s `owner:` is not in this chain — it only feeds `module.owner`
(below), a separate fact for a separate purpose (the module's documented owner,
not necessarily who a rule should escalate a specific PR to).

### Implemented in v1 (C5)

`user.author` is always asserted (an alias of `pr.author`, for symmetry).
`user.reviewer` is asserted when the PR has a standing review request: the first
requested user login, else the first requested team slug; left unset when nothing
is requested.

`user.owner` and `user.owners` are a waterfall over tiers 1–2, not a per-path
merge: tier 1 (CODEOWNERS) is tried against every changed path first, and only
when it names nobody for *any* of them is tier 2 (git log) even called — it is a
real API call per touched path, so it is never paid for a PR CODEOWNERS already
answers. Tier 1: the last matching CODEOWNERS rule wins per GitHub,
`user.owners` is the sorted, de-duplicated union across every touched path,
`user.owner` is the first owner of the first touched path CODEOWNERS assigns.
CODEOWNERS is read from the **base** branch at its own ref, like the ruleset and
config (architecture.md, "Fork safety") — a fork PR cannot name its own owners.
The three locations GitHub consults (`.github/CODEOWNERS`, `CODEOWNERS`,
`docs/CODEOWNERS`) are tried in priority order.

Tier 2 queries GitHub's commits API, one path at a time — the endpoint takes a
single `path` filter, so there is no one-call equivalent of `git log -- path1
path2 ...` — capped at the first 25 changed paths (changed-file order) to bound
worst-case API calls on a huge PR; that cap is a documented, deterministic scope,
not a guess. History is walked from the PR's **base** sha, the same trust
boundary as tier 1, so a fork PR's own commits never count. The winner is the
single most recent commit across every queried path; its GitHub login is
`user.owner` (and the sole entry of `user.owners`) and is also asserted as
`user.last_toucher`. A commit whose author has no linked GitHub account, or a
path with no prior commit at all (added by this PR), contributes nothing —
never guessed from a raw git name or email.

`user.last_toucher` is asserted **only** when tier 2 is what resolved
`user.owner` — i.e. CODEOWNERS was silent and the git-log query found a linked
author. It is not computed, and stays unset, whenever CODEOWNERS already
answered; a rule reading it directly should not expect it on every PR.

A path neither tier covers — no CODEOWNERS, and no linked-account commit history
either — leaves `user.owner`, `user.owners`, and `user.last_toucher` **unset**
rather than guessed at `pr.author`. Safe under "Unset is false" — a rule gated on
`attr "user.owner" == ...` simply does not fire for a path neither tier covers,
the same quiet non-match as a repo with neither signal.

```yaml
# .github/talooner/modules.yaml
- path: internal/auth/
  documentation_url: https://docs.example.com/auth
  owner: "@alice"
- path: billing/
  documentation_url: https://docs.example.com/billing
  owner: "@org/payments"
```

Usable in conditions and as an action target:

```talon
rule "Tag the owner on critical paths" {
  for records where type == "pr"
    and is "critical_path"
  requires "review.senior_engineer"
  do assign "pr" attr "user.owner"
  do comment "pr" "Touches code owned by {attr.user.owner} — their review is required"
}

rule "Escalate when the author owns the code they changed" {
  for records where type == "pr"
    and is "critical_path"
    and attr "user.owner" == attr "pr.author"
  requires "review.senior_engineer"
  do require "review.senior_engineer"
  do comment "pr" "Author is the code owner — needs an independent reviewer"
}
```

`attr "user.owner"` in an action argument is resolved by the **engine** against
the matched row, not passed through as the string `"user.owner"` for the bot to
look up. Same for `{attr.user.owner}` in the comment body. See
[`tln-language/docs/actions.md`](https://github.com/opentalon/tln-language/blob/main/docs/actions.md).

That second rule is the case that motivates the namespace: self-review of
critical code is invisible to CODEOWNERS (GitHub won't request a review from the
author) but trivially expressible once ownership is a fact.

Requires `members: read` to expand a team handle into members.

## Project-specific facts — tln-native

`pr.touches_auth`, `pr.touches_payments`, `pr.touches_css` are path predicates.
They are **defined in tln**, not in YAML, so policy stays in one file and gets
the same validation, testing, and `explain` treatment as everything else:

```talon
define "pr.touches_auth" {
  attr "pr.changed_files" contains "internal/auth/"
    or attr "pr.changed_files" contains "app/models/user.rb"
}

define "pr.touches_payments" {
  attr "pr.changed_files" contains "billing/"
    or attr "pr.changed_files" contains "payment"
}

define "ui_change" {
  attr "pr.changed_files" ends_with ".css"
    or attr "pr.changed_files" ends_with ".scss"
    or attr "pr.changed_files" contains "app/components/"
}
```

`grammar.ebnf:515` gives `contains | starts_with | ends_with | matches
[phrase]` against a **string** operand. `pr.changed_files` is a list, so these
predicates have to mean "any element contains / ends with" — existential
quantification over the list.

**They do, since 2026-08-07.** Phase 0 found they didn't — both evaluator paths
type-asserted their operands to `string` and returned false for a list, with no
diagnostic. Fixed generally in `tln-language` rather than worked around here:
[`tln-language#158`](https://github.com/opentalon/tln-language/issues/158),
landed in `tln-language` 35109f0 and `tln-db` e1c8ddb, so both backends
quantify. The `pr.changed_paths_joined` fallback is dropped.

**There is no glob matching, and that is what these examples used to assume.**
`matches` is a contiguous case-insensitive substring scan locally and term-AND on
Datalevin — `matches "**/*.css"` matches nothing, because no path contains that
text. Write path predicates with `contains`, `starts_with`, and `ends_with`. The
cost is precision: `contains "payment"` also matches `docs/payment-notes.md`, and
`ends_with ".css"` can't exclude `vendor/`. Narrow with a prefix
(`starts_with "app/"`) when that matters. Real glob support is a possible
`tln-language` ask, not a blocker.

One edge that follows from the quantification: a list with **no string elements
matches nothing** — there is no fallback to the scalar path. An empty
`pr.changed_files` therefore fails every positive predicate, which is why the
extractor asserts the fact even when the list is empty rather than leaving it
unset (see "Unset is false" below).

## `module.*` and `team.*` — lookup tables

External to the diff. Tenant-supplied, committed to the repo, read from the base
branch at the same ref as the ruleset and config (architecture.md, "Fork safety")
so a fork PR cannot redefine what it touches. `modules.yaml` carries both
`documentation_url` and `owner` per path prefix:

```yaml
# .github/talooner/modules.yaml
- path: internal/auth/
  documentation_url: https://docs.example.com/auth
  owner: "@alice"
- path: billing/
  documentation_url: https://docs.example.com/billing
  owner: "@org/payments"
```

```yaml
# .github/talooner/teams.yaml
senior_oncall: "@org/senior-engineers"
designers:     "@org/design"
security_team: "@org/security"
```

### The facts

| Fact | Type | When asserted |
|---|---|---|
| `module.touched_count` | int | Always — the number of configured modules the PR's files fall under |
| `module.documentation_url` | string | Only when ≥1 module touched — the **primary** module's doc URL |
| `module.documentation_urls` | list\<string\> | Only when ≥1 module touched — every touched module's doc URL, de-duplicated and sorted |
| `module.owner` | string | Only when the **primary** module declares an owner |

`module.touched_count` is **always** asserted, reading 0 when the PR touches no
configured module — the honest answer, not an unset fact, so a rule
`when attr "module.touched_count" > 1 do comment "pr" "Split this PR by module"`
fires correctly even on the empty case. The other three stay unset when nothing
is touched, so a rule gated on them simply does not fire — the safe direction.

### Primary module: most changed lines, path order on a tie

**This single-eval-per-PR cardinality is what
[`expert-review-system.md`](expert-review-system.md) deliberately changes for
LLM review** (its own "Key decisions" #2) — once that lands, a touched PR gets
one `code_unit` record per touched module/service, not one binding to a
`primary` module. Nothing here changes until then; `module.*` as described
below is what's live today and unaffected by the new spec outside of the
`llm_review` path.

A PR touching five modules is evaluated **once**, not five times. `module.*`
binds to the **primary** touched module: the one whose files carry the most
changed lines (additions + deletions summed across its prefix). On a tie the
module whose path sorts first wins, so the same PR resolves identically on a
re-run — the determinism the brief asks for. The lines count comes from the
Files API, and a PR larger than one page fails the run rather than returning a
prefix, because a dropped page would silently mis-pick the primary module.

The alternative — re-running the ruleset once per touched module — matches the
brief's "verify code matches documentation" more literally, but it multiplies
every `llm_review` by the number of touched modules and makes "did the bot
approve?" a fold over N results instead of one answer. Not worth it.

What this costs, so it isn't a surprise later: a PR that changes `auth/` heavily
and `billing/` slightly only ever checks its diff against the `auth/` docs. The
`billing/` docs go unverified. `module.documentation_urls` is still asserted so a
rule *can* reference all of them — a future `llm_review` variant could take the
list — and `module.touched_count` lets a ruleset require narrow PRs.

### `team.*`: a logical name the repo maps to a GitHub team

`requires "review.<name>"` resolves `<name>` through `teams.yaml` when present:
the mapped value is taken as configured by the repo's own maintainers, so it is
not run through the slug check and may name a team in another org
(`"@org/security"`). A name absent from the map falls back to the path-derived
slug (`review.security` → team `security` in the repo's org); `review.@alice`
still means the user alice. The same resolution is what `review.<name>.*`
(below) enumerates fact names for and matches `requested_teams` against.

## `review.*` (C7, issue #14)

Extracted from the PR's full review history (`GET .../pulls/{n}/reviews`), not
from `pull_request_review` events — Talooner is invoked by comment, not by
that trigger (architecture.md, "Invocation"), and re-deriving from the current
list on every run means the facts are correct however the run was fired.

| Fact | Meaning |
|---|---|
| `review.human.approved` | a non-bot approving review exists **at the current head sha** |
| `review.changes_requested` | any reviewer's latest decision is `REQUEST_CHANGES` |
| `review.<name>.requested` | that team's review request is currently standing |
| `review.<name>.approved` | a CODEOWNERS-proxy member of that team approved at the current head sha |
| `review.<name>.stale` | such an approval exists, but at an old commit, and nothing fresher supersedes it |

`review.human.approved` and `review.changes_requested` are always asserted —
a PR with no reviews at all gets `false` for both, the honest answer, not a
dead extractor. `<name>` ranges over every key in `teams.yaml` plus every team
slug directly requested on the PR that no `teams.yaml` key already resolves
to (a requested slug that resolves to the same target as a configured entry
is folded into that entry's own name, so one physical team never gets two
sets of facts); a team never mentioned by either source gets no `review.*`
facts at all, the same "no fact key" answer `module.*` gives an untouched
module.

Every reviewer's full history is folded to one standing decision, the way
GitHub's own merge box does: a dismissal flips the *same* review's `state` to
`DISMISSED` rather than adding a new event, and a `COMMENTED` review never
overrides an earlier decision — only a fresh `APPROVED` or
`CHANGES_REQUESTED` from that login does. So a `REQUEST_CHANGES` a reviewer
later resolves with their own approval stops counting; a bot's approval never
counts toward `review.human.approved`, regardless of its state.

Approvals are **dismissed on push** by GitHub only if the repo enables that
setting. Talooner does not assume it: `review.human.approved` and
`review.<name>.approved` both require `commit_id == pr.head_sha`, so an
approval from three pushes ago simply stops being an approval rather than
being trusted stale. For the team-scoped fact, that superseded approval isn't
just dropped silently — it is reported as `review.<name>.stale` (true only
while no fresher qualifying approval exists), which a comment can act on
("re-request review") even though it doesn't gate anything itself.
`review.changes_requested` has no sha check: GitHub itself keeps a change
request blocking regardless of new commits, until the same reviewer submits a
new decision or a human dismisses it, so this fact matches that behaviour
rather than resetting it at every push.

**Team membership is a CODEOWNERS proxy, not a real lookup (Evgeny's call,
actions.md "Workflow permissions").** `GITHUB_TOKEN` is repo-scoped and cannot
read org team membership, and the default has to work with no extra secret.
So `review.<name>.approved` does not ask "is this login a member of the
team" — it asks "does CODEOWNERS list this login on the same rule as the
team, for a path this PR touches". Concretely: for every changed path, take
the last matching CODEOWNERS rule (same resolution `user.owner` uses); if
that rule's owners include the resolved team (`@org/slug`), every other
individual (`@login`, not another `@org/team`) on that same line is treated as
a proxy member for this PR. A rule like

```
/critical/  @org/security @alice @bob
```

makes alice and bob stand in for `org/security` on paths under `critical/`.
**The gap this leaves, on purpose:** a team CODEOWNERS never lists alongside
an individual anywhere never resolves an approval — `review.<name>.approved`
stays `false` even when a real member of that GitHub team approves. That is
"slightly wrong for teams not in CODEOWNERS", the cost the issue's default
option accepts; an optional org-scoped PAT for real membership resolution was
the other option and was not taken, to keep the default secret-free.
`review.<name>.requested` needs no such proxy — GitHub's `requested_teams` is
readable with no extra scope, so it is a direct match against the resolved
team's slug.

## `code.*` — the LLM-review gate

**Phase 1 of [`expert-review-system.md`](expert-review-system.md), shipped.**
`architectureFacts` (`internal/facts/architecture.go`) classifies every changed
file into a `code_unit` — a touched model, controller, or service — and rolls
the result up into PR-level facts a ruleset can gate on cheaply, before any
LLM spend. The per-unit records themselves (`kind`, `path`, `important`,
`doc_ref`, `diff_slice`) are not sent to the cluster yet: Phase 2 adds the
proto field that carries them (`expert-review-system.md`, "Phase 2"). Today
`code.*` is the gate half only.

| Fact | Type | When asserted |
|---|---|---|
| `code.models_changed` | list\<string\> | Always — the touched model unit paths |
| `code.controllers_changed` | list\<string\> | Always — the touched controller unit paths |
| `code.services_changed` | list\<string\> | Always — the touched service unit paths |
| `code.touches_model` | bool | Always — `code.models_changed` is non-empty |
| `code.touches_controller` | bool | Always — `code.controllers_changed` is non-empty |
| `code.touches_service` | bool | Always — `code.services_changed` is non-empty |

All six are always asserted, empty list / `false` included — a PR touching
nothing under a known layer gets the honest zero, not a dead extractor (see
"Unset is false" below).

### Unit granularity: file for Rails, package directory for Go

A changed file is classified by the longest matching prefix, checked against
`architecture.yaml` overrides first, then the built-in layer table:

| Prefix | Kind | Unit |
|---|---|---|
| `app/models/` | model | the file itself |
| `app/controllers/` | controller | the file itself |
| `app/services/` | service | the file itself |
| `internal/<pkg>/` | service | the top-level directory under `internal/` |
| `cmd/<pkg>/` | service | the top-level directory under `cmd/` |

Both tables are checked on every PR regardless of what language the repo is
actually written in: Talooner has no repo-tree listing to detect that from,
only the changed files a run already fetched, and a Rails prefix can never
collide with a Go one, so this costs nothing and needs no extra API call. A
file directly under `internal/` or `cmd/` with no package subdirectory (e.g.
`internal/doc.go`) forms no unit — "package dirs" means a subdirectory, not a
loose file at the prefix root.

A unit's `doc_ref` follows a co-located naming convention: strip the file
extension, then the kind's conventional suffix (`_service`, `_controller`) if
it has one, then look under `docs/<kind>s/`. `app/services/orders_service.rb`
→ `docs/services/orders.md`; a Go unit's directory name is used directly:
`internal/auth` → `docs/services/auth.md`.

### `architecture.yaml`: override or extend the built-in layers

```yaml
# .github/talooner/architecture.yaml
- path: app/services/orders_service.rb
  kind: service
  doc_ref: docs/services/orders.md
- path: legacy/
  kind: model
```

Read from the base branch like `modules.yaml`, so a fork PR cannot redefine
its own layer conventions. A rule's `path` is a prefix, matched the same way
`module.*`'s prefixes are; the longest matching prefix wins, so a narrower
override beats a broader one and beats the built-in table. `kind` is required
and must be `model`, `controller` or `service` — a tenant error otherwise, the
same shape as an unparseable `modules.yaml`. `doc_ref` may be omitted: the
unit still exists with its overridden kind, it simply carries no doc to review
against — an override does not fall back to the co-located naming convention,
because a maintainer writing an override past the built-in tables has no
convention to fall back to.

## `llm_review.*`

**Not implemented in code yet — spec only, unshipped.** Nothing in this repo
or `talooner-plugin` executes an `llm_review` today: `internal/cluster` only
references the feature name (`whoami` capability check, `/review --force`
rejected with `ErrForceUnsupported` until it lands), and `validate_ruleset`
has no `llm_review` verb to accept. The table below is this repo's oldest,
PR-level design for the fact shape — single `doc_url`, one evaluation per PR.

**Superseded by [`expert-review-system.md`](expert-review-system.md)
before it ever shipped.** The design that's actually going to land is
per-`code_unit`, not per-PR: `doc_ref` (a repo path) replaces `doc_url`, each
touched unit gets its own `diff_slice`, and the cache key gains a `path`
component — `(pr, head_sha, path, doc_ref, prompt_version)`. See that doc's
"Key decisions" #2 and #4. Treat the table below as historical intent, not a
target to build toward.

| Fact | Type |
|---|---|
| `llm_review.result` | enum: `match` \| `mismatch` \| `unclear` \| `too_large` \| `error` |
| `llm_review.explanation` | string |
| `llm_review.doc_url` | string |
| `llm_review.error` | string, set only when `result == "error"` |

Keyed by `(pr, head_sha, doc_url, prompt_version)`. Rules must handle `unclear`
and `error`; a ruleset that only matches `match` and `mismatch` silently does
nothing on failure, which is the safe direction but should produce a lint
warning at ruleset-validation time.

## Custom facts

For anything Talooner can't know — `preview.status`, `screenshots.gallery_url`,
`dependency_scan.vulnerabilities`. Pushed by the tenant's CI:

```
POST /api/v1/facts
Authorization: Bearer <tenant token>
{"repo": "org/repo", "pr": 42, "facts": {"preview.status": "deployed",
                                         "preview.url": "https://..."}}
```

Asserting a fact wakes the engine, so reactive rules
(`when attr "preview.status" == "deployed"`) fire. This is the mechanism behind every
v2 action: Talooner doesn't build preview environments, it reacts to a fact
saying one exists.

Custom fact names are namespaced away from `pr.*` and `review.*` — CI cannot
overwrite a built-in fact. Without that, a workflow could POST
`pr.tests_passing: true` and defeat the entire ruleset.

## Unset is false, and that asymmetry is load-bearing

The single most dangerous detail here. Phase 0 settled it, and not the way this
document originally assumed.

`tln-language`'s evaluator is **two-valued**, with closed-world
negation-as-failure. There is no `unknown`. A missing attribute makes its pattern
fail, which makes any enclosing `not` *succeed*. Verified against both backends —
see `talooner-plugin/OPEN-QUESTIONS.md` A1 for the probe and the code
references.

The consequence splits by condition shape, and the split is what to remember:

| Condition | Fact unset | Safe? |
|---|---|---|
| `attr "pr.tests_passing" == true` | doesn't match, rule doesn't fire | yes |
| `attr "module.documentation_url" != ""` | doesn't match | yes |
| `not is "critical_path"` | **matches, rule fires** | **no** |
| `not attr "pr.touches_auth" == true` | **matches, rule fires** | **no** |

Positive conditions on an unset fact fail closed: the rule simply doesn't fire.
That is why the bot still leaves a fact **unset** rather than guessing a
default — `pr.tests_passing` while CI is running, or on a repo with no matching
checks. A rule gated on `attr "pr.tests_passing" == true` correctly stays quiet in
both cases.

Negated conditions fail *open*. A rule shaped `not is "critical_path"` reads a
failed extraction as "not on the critical path" and approves. **v1 accepts this
risk** — see the A1 decision — which makes two things the author's
responsibility:

- Extraction asserts facts explicitly, negative cases included. Unset means "the
  extractor never ran or died", not "we determined it's false".
- Rules that grant something (`allow`, approve) should be gated on positive
  conditions wherever possible. Every `not`-shaped rule is a path from "extractor
  crashed" to "approved", and nothing in the review output will say so.

The guard that closes this — a `strict` rule on a `pr.facts_complete` flag — is
written out in `talooner-plugin/OPEN-QUESTIONS.md` A1. It was deliberately not
built for v1; nothing forecloses adding it.

## Retention

Facts outlive a single run and are the plugin's to expire — defaults and how
they're enforced are in
[`talooner-plugin/facts.md`](https://github.com/opentalon/talooner-plugin/blob/main/facts.md),
"Scoping and lifetime". The GitHub half keeps nothing — it is a container that
exits.

One consequence worth stating with the retention rules: a fact asserted from
outside (your CI POSTing `preview.status`) sits in `tln-db` doing nothing until
something evaluates. In v1 that something is a human typing `@talooner /review`
(decision 20). If retention expires the fact before anyone does, the rule that
wanted it never fires — so retention must outlive a realistic "nobody looked at
this PR for a few days".
