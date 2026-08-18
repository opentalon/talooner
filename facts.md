# Talooner — facts

Everything a rule can match on. Facts live in `talon-db`, scoped per pull
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
| `review.*` | `pull_request_review` events | per PR, accumulates |
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

### `pr.new_dependencies`

Count of added entries across recognised manifests (`go.mod`, `package.json`,
`Gemfile`, `requirements.txt`, `Cargo.toml`, …), parsed from the diff.
Lockfile-only churn does not count. Version bumps of existing deps do not count
as *new*; a separate `pr.upgraded_dependencies` covers those.

## `user.*` — who is responsible for this code

The person a rule wants to tag. Distinct from `pr.author`: the author wrote the
change, the responsible user owns the code being changed. Often different people,
and the whole reason to have both.

| Fact | Type | Source |
|---|---|---|
| `user.owner` | string | Primary owner of the touched code — CODEOWNERS, else `modules.yaml` |
| `user.owners` | list\<string\> | All owners across touched paths |
| `user.author` | string | Alias of `pr.author`, for symmetry |
| `user.reviewer` | string | Currently requested reviewer, if one |
| `user.last_toucher` | string | Author of the most recent prior commit to the touched paths |

Resolution order for `user.owner`, first hit wins:

1. `.github/CODEOWNERS` — GitHub's own mechanism, already in most repos, already
   the thing people maintain. Reusing it beats inventing a parallel ownership
   file that drifts.
2. `owner:` in `modules.yaml`, for repos without CODEOWNERS or where Talooner
   ownership should differ from GitHub's auto-request behaviour.
3. `user.last_toucher` from `git log` on the touched paths.
4. Unset. Not `pr.author` — falling back to the author would let a rule
   "escalate to the owner" silently escalate to the person who wrote the change,
   which is exactly the wrong answer and is invisible when it happens.

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
[`talon-language/docs/actions.md`](https://github.com/opentalon/talon-language/blob/main/docs/actions.md).

That second rule is the case that motivates the namespace: self-review of
critical code is invisible to CODEOWNERS (GitHub won't request a review from the
author) but trivially expressible once ownership is a fact.

Requires `members: read` to expand a team handle into members.

## Project-specific facts — Talon-native

`pr.touches_auth`, `pr.touches_payments`, `pr.touches_css` are path predicates.
They are **defined in Talon**, not in YAML, so policy stays in one file and gets
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
diagnostic. Fixed generally in `talon-language` rather than worked around here:
[`talon-language#158`](https://github.com/opentalon/talon-language/issues/158),
landed in `talon-language` 35109f0 and `talon-db` e1c8ddb, so both backends
quantify. The `pr.changed_paths_joined` fallback is dropped.

**There is no glob matching, and that is what these examples used to assume.**
`matches` is a contiguous case-insensitive substring scan locally and term-AND on
Datalevin — `matches "**/*.css"` matches nothing, because no path contains that
text. Write path predicates with `contains`, `starts_with`, and `ends_with`. The
cost is precision: `contains "payment"` also matches `docs/payment-notes.md`, and
`ends_with ".css"` can't exclude `vendor/`. Narrow with a prefix
(`starts_with "app/"`) when that matters. Real glob support is a possible
`talon-language` ask, not a blocker.

One edge that follows from the quantification: a list with **no string elements
matches nothing** — there is no fallback to the scalar path. An empty
`pr.changed_files` therefore fails every positive predicate, which is why the
extractor asserts the fact even when the list is empty rather than leaving it
unset (see "Unset is false" below).

## `module.*` and `team.*` — lookup tables

External to the diff. Tenant-supplied, committed to the repo. `modules.yaml` is
shown under `user.*` above — it carries both `documentation_url` and `owner`.

```yaml
# .github/talooner/teams.yaml
senior_oncall: "@org/senior-engineers"
designers:     "@org/design"
security_team: "@org/security"
```

### Cardinality: one evaluation per PR

A PR touching five modules is evaluated **once**, not five times. `module.*`
binds to the **primary** touched module: the one with the most changed lines,
ties broken by path order for determinism.

The alternative — re-running the ruleset once per touched module — matches the
brief's "verify code matches documentation" more literally, but it multiplies
every `llm_review` by the number of touched modules and makes "did the bot
approve?" a fold over N results instead of one answer. Not worth it.

What this costs, so it isn't a surprise later: a PR that changes `auth/` heavily
and `billing/` slightly only ever checks its diff against the `auth/` docs. The
`billing/` docs go unverified. Two mitigations available inside the single-
evaluation model:

- `module.documentation_urls` (list) is still asserted, so a rule *can* reference
  all of them — a future `llm_review` variant could take the list.
- A ruleset that wants per-module strictness can require narrow PRs:
  `when attr "module.touched_count" > 1 do comment "pr" "Split this PR by module"`.

`module.touched_count` is asserted for exactly this.

## `review.*`

Populated from `pull_request_review` and review-request events:

| Fact | Meaning |
|---|---|
| `review.human.approved` | any non-bot approving review exists |
| `review.<team>.approved` | approving review from a member of the mapped team |
| `review.<team>.requested` | review request outstanding |
| `review.changes_requested` | any `REQUEST_CHANGES` outstanding |

`do require "review.senior_engineer"` maps `senior_engineer` through
`teams.yaml` to a GitHub team, requests its review, and satisfies when a member
of that team approves. Team membership needs `members: read`.

Approvals are **dismissed on push** by GitHub only if the repo enables that
setting. Talooner does not assume it — `review.*.approved` is re-derived from
the current review list on every run, and a review whose `commit_id` predates
the current head sha is reported separately as `review.<team>.stale`.

## `llm_review.*`

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

`talon-language`'s evaluator is **two-valued**, with closed-world
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
outside (your CI POSTing `preview.status`) sits in `talon-db` doing nothing until
something evaluates. In v1 that something is a human typing `@talooner /review`
(decision 20). If retention expires the fact before anyone does, the rule that
wanted it never fires — so retention must outlive a realistic "nobody looked at
this PR for a few days".
