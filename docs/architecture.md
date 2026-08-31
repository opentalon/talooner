# Talooner — architecture

> **`llm_review` described below is stale, and not reachable end-to-end.** This
> document's request flow, determinism section, and `module.*` framing describe
> the original PR-level design. What actually exists: `talooner-plugin` has a
> real `llm_review` executor and per-unit cache since 2026-08-28
> (`talooner-plugin/docs/llm-review.md`), and this repo has a `code_unit` fact
> layer since 2026-08-29 (Phase 1 below) — but that layer only produces its own
> `code.*` gating facts, and doesn't yet send `code_units` on `evaluate_pr` (#78).
> So no real PR reaches a model through this path yet. The design is per-unit,
> not per-PR — see [`expert-review-system.md`](expert-review-system.md) for the
> current specification and decisions. Sections below are corrected against the
> current code where they'd drifted independent of that change; where
> `llm_review` specifics appear, read them as historical intent.

## What it is

A GitHub Action that reviews pull requests by running the repo's own tln
ruleset. No model decides anything. Rules decide; an LLM is consulted only where
a rule says `do llm_review ...`, and its answer re-enters the engine as a fact.

Talooner has **no merge rights in v1.**. It is an advisory reviewer — think of it as a
linter that speaks human language and can read your docs. It comments, requests
changes, approves (advisory), and publishes check runs. Whether a check gates
merge is the repo owner's branch-protection setting, not Talooner's call.

## Not one component — an ecosystem

Running Talooner requires a **self-hosted OpenTalon cluster**. Permanently, not
as a v1 limitation: there is no hosted tier, no managed offering, and no plan for
one. You bring a VPS, you run the cluster, you supply the LLM credentials, you
pay for your own tokens. No cluster, no Talooner.

This is the whole reason the bot/plugin split exists. The cluster holds the LLM
credentials, so every token a rule spends is billed to whoever ran the rule.
Nobody's review load can ever land on someone else's API limits, because there is
no shared anything.

```
┌──────────────────────────────┐            ┌────────────────────┐
│  GitHub                      │            │ OpenTalon cluster  │
│                              │            │   (your VPS)       │
│  PR · comments · checks      │            │                    │
│            │                 │            │ ┌────────────────┐ │
│            │ native event    │   gRPC     │ │ talooner-plugin│ │
│            ▼                 │───────────▶│ │ • ruleset store│ │
│  ┌────────────────────────┐  │◀───────────│ │ • tln engine   │ │
│  │ Actions runner         │  │            │ │ • llm_review   │ │
│  │  opentalon/talooner@v1 │  │            │ │ • explain/audit│ │
│  │  • fact extraction     │  │            │ └───────┬────────┘ │
│  │  • action exec         │  │            │         │          │
│  │  • EPHEMERAL           │  │            │   ┌─────▼──────┐   │
│  └────────────────────────┘  │            │   │  tln-db    │   │
│            │ GITHUB_TOKEN    │            │   │ PR facts   │   │
│            ▼                 │            │   │ decisions  │   │
│  reviews · comments · checks │            │   └────────────┘   │
└──────────────────────────────┘            └────────────────────┘
                  ▲
      ┌───────────┴─────────┐
      │ talooner CLI        │   local authoring: rules validate / test / plan
      │ (gh extension)      │
      └─────────────────────┘
```

The whole GitHub half lives and dies inside one workflow run. There is no server
to operate, no endpoint to expose, no process to restart.

### Component split — and why

The action is **thin and ephemeral**; all state and all reasoning live
cluster-side.

| Concern | Where | Why there |
|---|---|---|
| Triggering | GitHub Actions | Native events. No webhook receiver, no HMAC, no delivery queue, no 10s deadline |
| GitHub credentials | Actions runtime | `GITHUB_TOKEN`, minted per run, scoped to one repo, expires when the job ends |
| Fact extraction from the GitHub API | action | Only the runner holds a token |
| Fact storage, reactive `changes` operator | plugin → tln-db | Facts must outlive a run that lasts 30 seconds |
| Ruleset parse, validate, compile | plugin | Rules are evaluated where facts live |
| tln engine, defeasible resolution | plugin | Same |
| `llm_review` | plugin | Only the cluster holds tenant LLM credentials |
| `explain` / audit trail | plugin → tln-db | Decisions are queried long after the PR closes |
| Action execution against GitHub | action | Only the runner holds a token |

The seam is: **the action knows GitHub and knows nothing about tln; the plugin
knows tln and knows nothing about GitHub.** The plugin returns an abstract
action list; the action translates it into API calls. That keeps
`talooner-plugin` testable without a GitHub fixture, and keeps the GitHub half
free of engine state.

### Why the GitHub half holds nothing

It cannot. A workflow run is a fresh container that exits when the job ends.
Everything that must survive between events — facts, subscriptions, decisions,
`explain` output, `llm_review` results — is in `tln-db` already, because
reactive rules (`when "pr.files_changed" changes`) required that regardless.

Subscription state (which PRs were invoked with `!talooner /review`) is state
too, so it lives cluster-side as a fact like everything else. Each run asks the
plugin "is this PR subscribed?" rather than remembering.

This is the same design as before this decision — the GitHub half was already
specified as stateless. Moving it into a runner just removed the ability to
cheat.

### What this costs

Stated plainly, because it is a real trade:

- **Nothing is alive between events.** A fact POSTed by your CI cannot produce a
  comment on its own; see "No reactive wake in v1" below.
- **Cold start per event.** Roughly 10–30s of runner startup before Talooner does
  anything. Irrelevant for a reviewer, fatal for anything interactive.
- **The cluster must be reachable from the runner.** A public gRPC endpoint with
  TLS and an API key, or a self-hosted runner on the same network. This is the
  one genuinely new operational requirement.
- **Actions minutes.** Free on public repos, billed to the tenant on private
  ones. Consistent with the rest of the model: you pay for your own reviews.

## Repos

Two repos. They are separate concepts and they version independently.

| Repo | Module | Contents |
|---|---|---|
| `talooner` | `github.com/opentalon/talooner` | The action (`action.yml` + `cmd/talooner-action`) and the CLI (`cmd/talooner`) |
| `talooner-plugin` | `github.com/opentalon/talooner-plugin` | OpenTalon gRPC plugin: engine, ruleset store, fact store, `llm_review`, and **the proto** |

The shared proto lives in `talooner-plugin`, since the plugin is the server and
owns the contract. The bot imports the generated Go package as a normal tagged
dependency — the same relationship `mcp-plugin` has with `opentalon`. Landing
order for a contract change: plugin first, tag, then bump the bot.

Everything cluster-side is documented in that repo and not duplicated here. The
docs in `talooner/` describe the bot, the GitHub surface, and the contract from
the caller's side.

Naming: **Talooner** is the ecosystem *and* the bot. The bot is plain `talooner`;
everything else in the ecosystem is `talooner-*`.

### Action repo layout

```
talooner/
  action.yml              # the action definition — inputs, runs: docker
  Dockerfile              # pinned image, so a run doesn't compile Go
  cmd/
    talooner-action/      # entrypoint: one event in, actions executed, exit
    talooner/             # the CLI / gh extension
  internal/
    github/               # REST client over GITHUB_TOKEN
    event/                # parse GITHUB_EVENT_PATH into {repo, pr, trigger}
    command/              # !talooner /review /stop /why /plan + write-access gate
    facts/                # extractors: diff, checks, CODEOWNERS, modules, teams
    action/               # executor interface + one file per tln verb
      executor.go         #   interface + registry keyed by verb
      github.go           #   real writes
      printer.go          #   dry run — this is `rules plan`
      approve.go
      block.go
      comment.go
      assign.go
      require.go
      notify.go
      emit.go
    cluster/              # talooner-plugin client: whoami, evaluate_pr, ...
    config/
```

Three things this layout is deliberately encoding:

**`command/` and `action/` are not the same concept.** A *command* is a human
typing `!talooner /review` in a PR comment — it arrives in the event payload, is
gated on write access, and decides *whether to evaluate*. An *action* is a tln verb
the plugin returned — it arrives as data from the engine and decides *what to do
to GitHub*. Different inputs, different auth, different tests. Collapsing them
into one package makes the write-access gate ambiguous, which is a security
control, not a stylistic detail.

**Action file names match tln verbs exactly.** The plugin returns
`{"verb": "approve", ...}` as a string; the bot dispatches through a registry
keyed by that string. If the file is `approve.go` and the verb is `approve`,
adding a verb to the DSL and adding a file stay in lockstep, and an unknown verb
is a single lookup failure with a clear error rather than a silent no-op.

Note there is no `reject.go` — `reject` is not in the vocabulary. The verbs are
`approve`, `block`, `comment`, `assign`, `require`, `notify`, `emit`
(`actions.md`). "Request changes" is how `block` renders on GitHub, not a
separate verb. Keeping the file set and the grammar identical is what stops the
two drifting.

**No `helpers/` or `utils/`.** Packages are named for what they do. A shared
helper package accumulates unrelated code until it depends on everything and
nothing can import it without a cycle. If something is genuinely shared, it
belongs in the package that owns the concept.

`internal/` rather than `src/` — Go convention, matches every other repo in the
workspace, and `internal/` is enforced by the compiler rather than by agreement.

### Dependency chain

The bot links neither `tln-language` nor `tln-db` — it only speaks the
plugin's contract, consuming the generated Go package as a normal tagged
dependency. That mirrors `opentalon-agents`, which deliberately links no
`tln-language` code, and it's why the bot builds without a sibling `tln-db/`
checkout.

The plugin does link both, and therefore inherits the workspace's `replace`
convention. See
[`talooner-plugin/docs/deployment.md`](https://github.com/opentalon/talooner-plugin/blob/main/docs/deployment.md),
"Dependency chain" — read it before the first build over there.

## Invocation — explicit in v1

v1 does not review on its own. A human asks it to:

```
!talooner /review
```

posted as a PR comment. Nothing happens on `pull_request opened` alone.

Reasons this is right for v1, beyond it being less work:

- **Consent.** A bot that reviews every PR the moment it's installed is a bot
  people uninstall. Explicit invocation means the first review is always wanted.
- **Blast radius.** A ruleset bug annoys the one person who asked, not every open
  PR in the org.
- **Spend.** No `llm_review` fires unless somebody asked for it. Matters a lot
  when phase 3 lands.
- **Fork PRs.** An attacker opening a fork PR can't make the bot do anything; a
  maintainer has to invoke it. This weakens — though does not remove — the fork
  threat model below.
- **Secrets.** GitHub does not expose repo secrets to workflows triggered by
  `pull_request` from a fork. `issue_comment` runs in the base repo's context and
  does. So under decision 1, explicit invocation isn't only a UX choice — it is
  the trigger on which Talooner can reach the cluster at all. A fork push
  literally cannot start a run with credentials.

Once invoked on a PR, the PR is **subscribed**: subsequent pushes and check
completions re-evaluate automatically, because that's what the reactive rules
(`when "pr.files_changed" changes`) mean. Subscription is per PR and ends when
the PR closes. `!talooner /stop` unsubscribes.

Automatic review on PR open, opt-in per repo via config, is a later phase. The
subscription machinery is identical; only the trigger differs.

Command surface in v1:

| Command | Effect |
|---|---|
| `!talooner /review` | Evaluate now, subscribe this PR. Always a full re-evaluation |
| `!talooner /review --force` | Same, but bypass the `llm_review` fact cache at this sha — costs money |
| `!talooner /stop` | Unsubscribe |
| `!talooner /why` | Render `explain` for the current head sha |
| `!talooner /plan` | Evaluate the head-branch ruleset with no writes |

Commands are honoured only from users with write access to the repo. Otherwise
any drive-by account could invoke reviews and burn the maintainer's LLM budget.

### Re-invoking `/review`

`/review` re-extracts every fact and re-evaluates, every time. There is no
re-render shortcut and no "nothing changed" path, because there is nothing to
optimise: `llm_review` results are facts keyed by `(pr, head_sha, doc_url,
prompt_version)` (decision 9), so a second evaluation at the same sha reads the
stored fact instead of calling a model. The expensive part is already cached by
construction; the rest is a handful of GitHub API reads and an engine run.

Always re-evaluating is also what makes manual wake work. Externally asserted
facts — `preview.status` POSTed by your CI — only enter a verdict when something
re-evaluates, and in v1 that something is a human typing `/review`.

`--force` busts the `llm_review` cache at the current sha. Use it when the input
didn't change but the answer might: a nondeterministic model, an edited base
ruleset, an extractor fixed since the last run. It maps to a cache-bypass
argument on the plugin's `evaluate_pr`, is gated by the same write-access check,
and is the only command in v1 that can spend LLM budget on a sha already
answered.

### No reactive wake in v1

tln's reactive rules still work — they fire when facts change *during* an
evaluation. What v1 does not have is anything to notice a fact changing while no
run is in progress.

Concretely: your CI POSTs `preview.status = "deployed"` an hour after the last
run. The engine has no process to wake, and the cluster deliberately holds no
GitHub credentials, so it cannot comment on its own. The fact sits in `tln-db`
until the next evaluation, which a maintainer triggers with `/review`.

This is accepted for v1, not overlooked. The alternatives all cost something:

| | How | Why not v1 |
|---|---|---|
| `repository_dispatch` | Tenant CI POSTs the fact, then dispatches a run | Two calls in the tenant's workflow, and a second trigger path to document and secure |
| `schedule` | Cron workflow polls for pending actions | 5-minute floor, burns Actions minutes on repos with nothing to do |
| Long-running bot | What decision 1 removed | Reintroduces the process, the endpoint, and the ops burden |

`repository_dispatch` is the likely phase-4 answer if manual wake proves
annoying. It is additive — same evaluation path, one more trigger in the
workflow — so deferring it costs nothing later.

## Request flow

### Trigger

```
GitHub event (issue_comment | pull_request | check_suite)
  └─ workflow `if:` filters obvious non-matches before a runner starts
       └─ runner boots, action entrypoint reads GITHUB_EVENT_PATH
```

No signature verification, no delivery queue, no 10-second deadline — the event
arrives as a JSON file on disk in a container GitHub already authenticated. The
whole `webhook/` package this design used to need is gone.

Idempotency was keyed on webhook delivery id; it is now `(head_sha, event,
run_attempt)`, deduped plugin-side. GitHub re-running a job is the only
redelivery, and it's explicit.

### Evaluate

```
runner starts with {repo, pr, event, GITHUB_TOKEN}
  0. is this PR subscribed? (invoked via !talooner /review)  — else exit 0
  1. load ruleset  ← BASE branch (see "Fork safety")
  2. extract facts (see facts.md)
  3. plugin action "evaluate_pr" {repo, pr, head_sha, facts JSON, ruleset, mode}
     (an OpenTalon plugin action, not a bespoke rpc —
      see talooner-plugin/docs/protocol.md)
       └─ plugin: assert facts into tln-db, run engine,
                  resolve conflicts (defeasible), issue llm_review as needed,
                  return actions + explanation
  4. execute actions against GitHub (see actions.md)
  5. record outcome, exit
```

Step 0 exiting non-error matters: an unsubscribed PR must show a skipped job, not
a red X on someone's PR.

`llm_review` runs *inside* step 3, cluster-side, synchronously from the engine's
point of view. The runner never sees an LLM.

### Concurrency

Handled by the workflow first, with the plugin as a backstop:

```yaml
concurrency:
  group: talooner-${{ github.event.pull_request.number || github.event.issue.number }}
  cancel-in-progress: false
```

One run per PR at a time. `cancel-in-progress: false` because a cancelled run can
leave GitHub half-written — the actions are idempotent, but a killed container
doesn't finish the set. Queueing instead means the later run starts from current
facts anyway, so a stale in-flight evaluation self-corrects rather than needing
to be killed.

That block is in the tenant's file, though, and they can delete it. So the plugin
enforces the same thing from its side: a second `evaluate_pr` for a `(repo, pr)`
already in flight is rejected with a 409 rather than queued. It has to be — fact
assertion is a non-atomic read-modify-write, so two overlapping runs would
interleave into a mixed fact set rather than the later one simply winning
(`talooner-plugin/docs/OPEN-QUESTIONS.md` A7, B6).

Actions remain idempotent by construction: comments are sticky (edited in place,
keyed by a marker), check runs are updated by name, reviews are dismissed and
re-issued rather than stacked.

One free property comes with this: events triggered by `GITHUB_TOKEN` do not
trigger further workflows. Talooner's own comments and check runs cannot start
another Talooner run. Under a webhook bot, loop prevention was code you had to
write and test.

## Fork safety

The ruleset that governs writes is always the one on the **base** branch. Never
the head branch, never a fork.

Talooner cannot merge, so a malicious head-branch ruleset cannot approve itself
into main. The real exposures are cheaper but still real:

- **Spend.** A fork PR could add a hundred `llm_review` rules and burn the
  tenant's LLM budget.
- **Noise.** It could make the bot post arbitrary attacker-authored text as a
  first-party review comment on the tenant's repo.

Base-branch-only removes both. To keep rule changes testable, a PR that modifies
the ruleset gets a **plan run**: the head ruleset is evaluated in a mode that
executes no GitHub writes, and the bot posts one comment showing what would
differ from the base ruleset's decisions. That is also the honest UX — you see
your rule change's effect before it can act.

Additional limits regardless of branch: per-PR cap on `llm_review` invocations,
per-tenant budget ceiling enforced cluster-side by the plugin.

## Auth

Three credentials, two of which Talooner never stores. See `auth.md`.

1. **`GITHUB_TOKEN`** — minted by Actions per run, scoped to the one repo, dies
   with the job. Permissions are declared in the workflow, not in an App
   registration, so they're visible in the tenant's own diff.
2. **Cluster API key** — runner → OpenTalon, from `secrets.OPENTALON_API_KEY`.
   Presented on connect; `whoami` returns tenant id, quota, enabled models. The
   run fails fast without it.
3. **LLM provider credentials** — cluster only. Never reach the runner, never
   appear in a workflow, never land in `tln-db`.

There is no long-lived GitHub credential anywhere in this design. Nothing to
rotate, nothing to leak from a server that doesn't exist.

## Determinism

Same head sha + same base ruleset ⇒ same actions. Holds because:

- Fact extraction is a pure function of the PR at that sha.
- Conflict resolution is defeasible, not load-order dependent.
- `llm_review` results are stored as facts keyed by
  `(pr, head_sha, doc_url, prompt_version)`. A re-run at the same sha reads the
  stored fact instead of calling the model. New commit → new sha → fact absent →
  fresh call. The fact store *is* the cache; no separate layer. (Historical
  design — the key gains a `path` component and moves to per-unit under
  [`expert-review-system.md`](expert-review-system.md); neither shape is built
  yet.)

A per-PR conversation is retained cluster-side for continuity and better
explanations, but each `llm_review` is a scoped turn whose result pins to its
head sha. The conversation informs the answer; it never changes an answer
already recorded.

## Deployment

**Fully self-hosted, permanently.** You run the OpenTalon cluster and the
`talooner-plugin` inside it. The GitHub half you don't run at all — it's a
workflow file and two secrets, and the compute is GitHub's. Nobody else ever
holds your secrets, because there is nobody else: no operator, no central
service, no shared App.

What you install, end to end:

```
1. cluster on your VPS, reachable over TLS       (once)
2. commit .github/workflows/talooner.yml         (per repo, 20 lines)
3. set OPENTALON_HOST + OPENTALON_API_KEY        (per repo or org-wide)
```

Consequences, stated plainly since they're the cost of this model:

- Onboarding is still "provision a VPS", not "click install". Steps 2–3 are
  trivial; step 1 is the adoption ceiling and it will not be lifted.
- There is no telemetry, no central error reporting, no way to know how many
  installs exist or what broke on them. Bug reports arrive only if someone files
  one.
- Every tenant pins their own action version (`@v1`, or a sha). A bad release
  doesn't propagate until someone bumps — good for blast radius, bad for
  security fixes reaching people.
- Every tenant runs a different version. Cross-repo compatibility between
  `talooner` and `talooner-plugin` has to be an explicit versioned contract, not
  an assumption — the version-skew failure mode the workspace `CLAUDE.md` already
  warns about. It's now slightly worse than "same person, same box": the action
  version is pinned per repo in a workflow file, while the plugin version is
  whatever the cluster runs. `whoami` returning a protocol version, checked at
  run start, is how that stays a clear error instead of a strange one.

The upside is that the security posture is trivial to reason about: the operator
holds no tenant secrets because the operator does not exist, and the tenant holds
no long-lived GitHub secret because Actions mints one per run.
