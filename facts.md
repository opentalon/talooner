# Talooner — facts

Everything a rule can match on. Facts live in `talon-db`, scoped per pull
request, asserted by the bot and read by the engine.

## Where configuration lives

In the repo being reviewed. Talooner reads it with the installation token it
already has; there is nowhere else it could live without inventing a second
source of truth outside version control.

```
your-repo/
  .github/
    talooner/
      rules.talon          ← the review policy
      rules.talon.test     ← tests for the policy
      config.yaml          ← check-name patterns, caps, toggles
      modules.yaml         ← module → docs URL / owner
      teams.yaml           ← logical team → GitHub team
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

Pure functions of the PR at a given head sha. Always asserted, never absent.

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
| `pr.tests_passing` | bool | check runs — see below |
| `pr.lint_passing` | bool | check runs — see below |
| `pr.diff` | string (ref) | Files API patches, size-capped |
| `pr.new_dependencies` | int | manifest diff — see below |

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

See "Unset is not false" below. A rule that auto-approves on
`"pr.tests_passing" == true` must not fire while CI is still running, and must
not fire on a repo that has no tests at all.

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
  when is "critical_path"
  do assign "pr" "user.owner"
  do comment "pr" "Touches code owned by {user.owner} — their review is required"
}

rule "Escalate when the author owns the code they changed" {
  when is "critical_path"
    and "user.owner" == "pr.author"
  do require "review.senior_engineer"
  do comment "pr" "Author is the code owner — needs an independent reviewer"
}
```

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
  "pr.changed_files" contains "internal/auth/"
    or "pr.changed_files" contains "app/models/user.rb"
}

define "pr.touches_payments" {
  "pr.changed_files" contains "billing/"
    or "pr.changed_files" matches "**/payment*"
}

define "ui_change" {
  "pr.changed_files" matches "**/*.css"
    or "pr.changed_files" matches "**/*.scss"
    or "pr.changed_files" contains "app/components/"
}
```

`grammar.ebnf:515` gives `contains | starts_with | ends_with | matches
[phrase]`. **Open item:** those operators are specified against a string
operand. `pr.changed_files` is a list, so `contains` must mean "any element
contains" and `matches` must mean "any element matches" — existential
quantification over the list. Confirm the executor does this; if it doesn't,
it's a small change in `talon-language/internal/executor` and worth making
generally, not a Talooner special case.

Fallback if list semantics turn out to be a problem: also assert
`pr.changed_paths_joined` (newline-joined) so plain string `contains` works.
Ugly; prefer fixing the operator.

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
  `when "module.touched_count" > 1 do comment "pr" "Split this PR by module"`.

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
(`when "preview.status" == "deployed"`) fire. This is the mechanism behind every
v2 action: Talooner doesn't build preview environments, it reacts to a fact
saying one exists.

Custom fact names are namespaced away from `pr.*` and `review.*` — CI cannot
overwrite a built-in fact. Without that, a workflow could POST
`pr.tests_passing: true` and defeat the entire ruleset.

## Unset is not false

The single most dangerous detail here.

`"module.documentation_url" != ""` on an **unset** fact must not match. If unset
coerces to the empty string, every module without docs matches `!= ""` as false
— fine — but the inverse pattern (`"pr.touches_auth" == false` on a fact that
failed to compute) would silently classify a critical PR as safe and auto-approve
it.

Rules:

1. A condition on an unset fact evaluates to **unknown**, not false.
2. A rule with any unknown condition **does not fire**.
3. `not is "critical_path"` where `critical_path` is unknown is **also
   unknown** — negation of unknown is unknown, not true. This is the case that
   would otherwise auto-approve a PR whose fact extraction failed.
4. Rules that didn't fire due to unknowns appear in `explain` output, so a
   maintainer can see "would have approved but `tests_passing` was unset".

Confirm points 1–3 against `talon-language`'s actual evaluator before relying on
them. If the engine is two-valued, this becomes a prerequisite change in
`talon-language`, not a Talooner-local concern — and it's the first thing to
verify in phase 0.

## Retention

Facts live until the PR closes, then a configurable grace period (default 30
days) for audit queries, then deletion. Decisions and their `explain` records
are retained longer (default 1 year) since "why did the bot block this?" is
asked long after the fact.
