# Talooner

Deterministic GitHub review bot built on OpenTalon + tln language.

A repo declares its review policy as tln rules. Talooner ingests PR facts,
runs the rules through the tln inference engine, and executes the resulting
actions against the GitHub API. No model decides whether a PR is approved —
rules do. An LLM is called only where a rule explicitly asks for it
(`do llm_review ...`), and its output re-enters the engine as a fact.

Talooner has **no merge rights in v1**. It is an intelligent linter that speaks
human language and can read your docs, positioned before human review rather than
instead of it.

Self-hosted, permanently. You run an OpenTalon cluster on your own VPS with your
own LLM credentials. There is no hosted tier and no plan for one.

> **Status: early v1.** The walking skeleton runs end to end — event, command
> gate, subscription, base-branch ruleset, `pr.*` facts, `evaluate_pr` — and the
> decision is published as the `talooner` check run. The rest of the verdict is
> not: no review, no comments, no assignments yet, and most of the fact set is
> still to come. Not usable on a real repo. See the
> [v1 milestone](https://github.com/opentalon/talooner/milestone/1).

## Example

A repo declares its policy in `.github/talooner/rules.tln`:

```talon
define "small_change" {
  attr "pr.lines_changed" < 50
  attr "pr.files_changed" < 5
}

define "critical_path" {
  attr "pr.changed_files" contains "internal/auth/"
    or attr "pr.changed_files" contains "billing/"
}

rule "Auto-approve safe changes" {
  for records where type == "pr"
    and is "small_change"
    and attr "pr.tests_passing" == true
    and attr "pr.has_description" == true
    and not is "critical_path"
  allow "merge"
  do approve "pr"
}

rule "Require human review for critical paths" {
  for records where type == "pr"
    and is "critical_path"
  requires "review.senior_engineer"
  do require "review.senior_engineer"
  do assign "pr" attr "user.owner"
  do comment "pr" "Touches critical code owned by {attr.user.owner} — human approval required"
}
```

The runnable version of this ruleset, with a passing test suite, is
[`examples/talooner_review.tln`](https://github.com/opentalon/tln-language/blob/main/examples/talooner_review.tln)
in `tln-language`.

and wires up the action in `.github/workflows/talooner.yml`:

```yaml
name: talooner
on:
  issue_comment: {types: [created]}
  pull_request:  {types: [synchronize, reopened, closed]}
  check_suite:   {types: [completed]}

concurrency:
  group: talooner-${{ github.event.pull_request.number || github.event.issue.number }}
  cancel-in-progress: false

jobs:
  review:
    if: github.event_name != 'issue_comment' || startsWith(github.event.comment.body, '@talooner')
    runs-on: ubuntu-latest
    permissions:
      pull-requests: write
      checks: write
      contents: read
    steps:
      - uses: opentalon/talooner@v1
        with:
          opentalon_host:    ${{ secrets.OPENTALON_HOST }}
          opentalon_api_key: ${{ secrets.OPENTALON_API_KEY }}
```

A maintainer comments `@talooner /review`. A runner starts, extracts the facts,
sends them to your cluster, executes whatever actions come back, and exits. Same
commit, same rules, same verdict — every time.

Your LLM credentials are not in that workflow and never touch the runner. They
live in your cluster, which is the only thing that talks to a model.

Because the policy is a file in your repo, it is versioned, diffable, reviewable,
and unit-testable with `.tln.test` before it ever gates a real PR. That is a
claim no LLM-based reviewer can make.

## Docs

| File | Contents |
|---|---|
| `diagrams.md` | **Start here** — C4 levels 1–3 plus UML sequence/state: context, components, flows, credentials, fact sources |
| `architecture.md` | Components, bot/plugin split, request flow, fork safety, determinism |
| `facts.md` | Fact vocabulary, extraction, three-valued semantics |
| `actions.md` | Action catalog, GitHub semantics, conflict resolution, App permissions |
| `auth.md` | Credentials, onboarding CLI, untrusted input, audit |

Phasing and remaining work live in the
[v1 milestone](https://github.com/opentalon/talooner/milestone/1) rather than in
a doc.

The server side is documented in its own repo:
[`talooner-plugin`](https://github.com/opentalon/talooner-plugin) — protocol,
engine internals, fact scoping, `llm_review`, cluster deployment. Start with its
`README.md`.

## Decisions so far

| # | Decision |
|---|---|
| 1 | **A GitHub Action, not a hosted App.** Talooner runs inside the reviewed repo's own Actions runner, triggered by native workflow events. No webhook endpoint, no long-running process, no App registration, no shared anything. Identity is `github-actions[bot]`; credentials are the run's `GITHUB_TOKEN` plus two repo secrets. |
| 2 | **Thin stateless action + `talooner-plugin` in an OpenTalon cluster.** The action knows GitHub and nothing about tln; the plugin knows tln and nothing about GitHub. All state is cluster-side, which is what lets the GitHub half be a process that lives for 30 seconds. |
| 3 | **Self-hosted. Forever.** No hosted tier, ever. You bring a VPS and run the cluster; the cluster holds your LLM credentials; you pay for your own tokens. |
| 4 | **Advisory, never merges. in v1.** `contents: read`, no `contents: write`. Check runs gate merges only if the repo owner marks them required. |
| 5 | **Facts live in `tln-db`**, per PR, persistent — required for reactive rules. |
| 6 | **Path predicates are tln-native** (`define` over `pr.changed_files`), not YAML globs. |
| 7 | **Defeasible conflict resolution**, not ad-hoc "block wins". |
| 8 | **CLI / `gh` extension only in v1.** No web dashboard. |
| 9 | **No LLM cache layer** — `llm_review` results are facts keyed by head sha, which is the cache. |
| 10 | **Base-branch ruleset governs writes**; head-branch rulesets get read-only plan runs. |
| 11 | **No dispatch actions.** `deploy_preview` / `screenshot` / `scan_dependencies` are not verbs. The tenant's CI does the work and POSTs the result to the facts API; rules react. |
| 12 | **Two repos.** `talooner` (bot + CLI) and `talooner-plugin` (engine, fact store, proto). Separate concepts, separate versions. |
| 13 | **Config lives in the reviewed repo**, at `.github/talooner/`. Policy is versioned, diffable, and testable like any other code. |
| 14 | **Explicit invocation in v1** — `@talooner /review`. Nothing happens until asked; the PR is then subscribed to pushes. Doubly load-bearing under decision 1: `issue_comment` runs in base-repo context where the secrets exist, while a fork's `pull_request` event gets none. |
| 15 | **One evaluation per PR.** `module.*` binds to the primary touched module, not N runs. |
| 16 | **`user.*` namespace** for code ownership, resolved from CODEOWNERS then `modules.yaml`. Distinct from `pr.author`. |
| 17 | **Naming.** Talooner is the ecosystem *and* the bot. Bot = `talooner`; everything else = `talooner-*`. |
| 18 | **`/review` always re-evaluates.** No re-render shortcut: decision 9 already makes re-evaluation at an unchanged sha free, because `llm_review` results are facts keyed by sha. `/review --force` busts that cache — the only command that can spend on a sha already answered. |
| 19 | **Identity is `github-actions[bot]`.** No App to name, no globally unique name to claim, no name collision between installs. The brand is the action ref `opentalon/talooner@v1`. |
| 20 | **No reactive wake in v1.** Nothing is alive between events, so a fact POSTed by your CI does not by itself produce a comment. Someone types `@talooner /review` and the next run picks it up. Dispatch- and poll-based wake are phase 4, if ever. |

## What running this will require

Not a service you sign up for. There is no hosted tier and no plan for one — the
cluster holds the LLM credentials, so every token a rule spends is billed to
whoever ran the rule.

1. A VPS running an **OpenTalon cluster**, reachable from GitHub's runners over
   TLS (or a self-hosted runner, if the cluster stays private)
2. `talooner-plugin` loaded in that cluster, with `tln-db` available
3. LLM provider credentials configured **in the cluster**
4. `.github/workflows/talooner.yml` in each reviewed repo
5. Two repo (or org) secrets: `OPENTALON_HOST`, `OPENTALON_API_KEY`

No App to register, no private key to hold, no process to keep up. Items 1–3 are
the standing cost; 4–5 are a two-minute setup per repo. Details in `auth.md`.

## Related repos

| Repo | Role |
|---|---|
| [`opentalon`](https://github.com/opentalon/opentalon) | Core orchestration platform and plugin host |
| [`tln-language`](https://github.com/opentalon/tln-language) | The tln DSL: grammar, parser, inference engine, `.tln.test` |
| [`tln-db`](https://github.com/opentalon/tln-db) | Embedded fact store backing tln |
| [`talooner-plugin`](https://github.com/opentalon/talooner-plugin) | Server side — engine, fact store, proto |

## Contributing

Design phase, so the highest-value contribution right now is disagreement. The
decisions table above records what was chosen; `facts.md` records the one
accepted risk (unset facts read as false, so a failed extraction can approve)
that is worth arguing with if you think it's wrong. The phase-0 substrate
questions are answered in
[`talooner-plugin/OPEN-QUESTIONS.md`](https://github.com/opentalon/talooner-plugin/blob/main/OPEN-QUESTIONS.md);
three of them turned into upstream fixes, all landed 2026-08-07, and the phase-0
exit artifact
([`examples/talooner_review.tln`](https://github.com/opentalon/tln-language/blob/main/examples/talooner_review.tln))
is in `tln-language` with a passing test suite.

## License

Apache-2.0. See `LICENSE`.
