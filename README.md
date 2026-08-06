# Talooner

Deterministic GitHub review bot built on OpenTalon + Talon language.

A repo declares its review policy as Talon rules. Talooner ingests PR facts,
runs the rules through the Talon inference engine, and executes the resulting
actions against the GitHub API. No model decides whether a PR is approved —
rules do. An LLM is called only where a rule explicitly asks for it
(`do llm_review ...`), and its output re-enters the engine as a fact.

Talooner has **no merge rights in v1**. It is an intelligent linter that speaks
human language and can read your docs, positioned before human review rather than
instead of it.

Self-hosted, permanently. You run an OpenTalon cluster on your own VPS with your
own LLM credentials. There is no hosted tier and no plan for one.

> **Status: design phase.** This repo currently contains design documents only —
> no code, no binary, nothing runnable. Everything here is subject to change
> until phase 1 lands. See `roadmap.md`.

## Example

A repo declares its policy in `.github/talooner/rules.talon`:

```talon
define "small_change" {
  "pr.lines_changed" < 50
  "pr.files_changed" < 5
}

define "critical_path" {
  "pr.changed_files" contains "internal/auth/"
    or "pr.changed_files" contains "billing/"
}

rule "Auto-approve safe changes" {
  when is "small_change"
    and "pr.tests_passing" == true
    and "pr.has_description" == true
    and not is "critical_path"
  do approve "pr"
}

rule "Require human review for critical paths" {
  when is "critical_path"
  do require "review.senior_engineer"
  do assign "pr" "user.owner"
  do comment "pr" "Touches critical code owned by {user.owner} — human approval required"
}
```

A maintainer comments `@talooner /review`. Talooner extracts the facts, runs the
rules, and reports. Same commit, same rules, same verdict — every time.

Because the policy is a file in your repo, it is versioned, diffable, reviewable,
and unit-testable with `.talon.test` before it ever gates a real PR. That is a
claim no LLM-based reviewer can make.

## Docs

| File | Contents |
|---|---|
| `diagrams.md` | **Start here** — C4 levels 1–3 plus UML sequence/state: context, components, flows, credentials, fact sources |
| `architecture.md` | Components, bot/plugin split, request flow, fork safety, determinism |
| `facts.md` | Fact vocabulary, extraction, three-valued semantics |
| `actions.md` | Action catalog, GitHub semantics, conflict resolution, App permissions |
| `auth.md` | Credentials, onboarding CLI, untrusted input, audit |
| `roadmap.md` | Phases 0–4, and what this drags into the other repos |
| `OPEN-QUESTIONS.md` | What's still undecided |

The server side is documented in its own repo:
[`talooner-plugin`](https://github.com/opentalon/talooner-plugin) — protocol,
engine internals, fact scoping, `llm_review`, cluster deployment. Start with its
`README.md`.

## Decisions so far

| # | Decision |
|---|---|
| 1 | **GitHub App**, not a bot user. Per-installation rate limits, per-install scoped tokens, declared permissions. |
| 2 | **Thin stateless bot + `talooner-plugin` in an OpenTalon cluster.** Bot knows GitHub and nothing about Talon; plugin knows Talon and nothing about GitHub. |
| 3 | **Self-hosted. Forever.** No hosted tier, ever. You bring a VPS and run the cluster; the cluster holds your LLM credentials; you pay for your own tokens. |
| 4 | **Advisory, never merges. in v1.** `contents: read`, no `contents: write`. Check runs gate merges only if the repo owner marks them required. |
| 5 | **Facts live in `talon-db`**, per PR, persistent — required for reactive rules. |
| 6 | **Path predicates are Talon-native** (`define` over `pr.changed_files`), not YAML globs. |
| 7 | **Defeasible conflict resolution**, not ad-hoc "block wins". |
| 8 | **CLI / `gh` extension only in v1.** No web dashboard. |
| 9 | **No LLM cache layer** — `llm_review` results are facts keyed by head sha, which is the cache. |
| 10 | **Base-branch ruleset governs writes**; head-branch rulesets get read-only plan runs. |
| 11 | **No dispatch actions.** `deploy_preview` / `screenshot` / `scan_dependencies` are not verbs. The tenant's CI does the work and POSTs the result to the facts API; rules react. |
| 12 | **Two repos.** `talooner` (bot + CLI) and `talooner-plugin` (engine, fact store, proto). Separate concepts, separate versions. |
| 13 | **Config lives in the reviewed repo**, at `.github/talooner/`. Policy is versioned, diffable, and testable like any other code. |
| 14 | **Explicit invocation in v1** — `@talooner /review`. Nothing happens until asked; the PR is then subscribed to pushes. |
| 15 | **One evaluation per PR.** `module.*` binds to the primary touched module, not N runs. |
| 16 | **`user.*` namespace** for code ownership, resolved from CODEOWNERS then `modules.yaml`. Distinct from `pr.author`. |
| 17 | **Naming.** Talooner is the ecosystem *and* the bot. Bot = `talooner`; everything else = `talooner-*`. |

## What running this will require

Not a service you sign up for. There is no hosted tier and no plan for one — the
cluster holds the LLM credentials, so every token a rule spends is billed to
whoever ran the rule.

1. A VPS running an **OpenTalon cluster**
2. `talooner-plugin` loaded in that cluster, with `talon-db` available
3. LLM provider credentials configured **in the cluster**
4. A GitHub App registered against your org, installed on your repos
5. The `talooner` bot running, holding the App private key and a cluster API key

Details in `auth.md`.

## Related repos

| Repo | Role |
|---|---|
| [`opentalon`](https://github.com/opentalon/opentalon) | Core orchestration platform and plugin host |
| [`talon-language`](https://github.com/opentalon/talon-language) | The Talon DSL: grammar, parser, inference engine, `.talon.test` |
| [`talon-db`](https://github.com/opentalon/talon-db) | Embedded fact store backing Talon |
| [`talooner-plugin`](https://github.com/opentalon/talooner-plugin) | Server side — engine, fact store, proto |

## Contributing

Design phase, so the highest-value contribution right now is disagreement.
`OPEN-QUESTIONS.md` lists what's undecided. The phase-0 substrate questions —
answerable by reading `talon-language`, and each one blocks implementation — live
in
[`talooner-plugin/OPEN-QUESTIONS.md`](https://github.com/opentalon/talooner-plugin/blob/main/OPEN-QUESTIONS.md)
§A.

## License

Apache-2.0. See `LICENSE`.
