# Talooner — architecture

## What it is

A GitHub App that reviews pull requests by running the repo's own Talon ruleset.
No model decides anything. Rules decide; an LLM is consulted only where a rule
says `do llm_review ...`, and its answer re-enters the engine as a fact.

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
┌──────────────┐  webhooks   ┌───────────────────┐   gRPC    ┌────────────────────┐
│  GitHub      │────────────▶│  talooner (bot)   │──────────▶│ OpenTalon cluster  │
│  (App inst.) │◀────────────│                   │◀──────────│                    │
└──────────────┘  REST/GQL   │ • App auth        │           │ ┌────────────────┐ │
                             │ • webhook verify  │           │ │ talooner-plugin│ │
                             │ • fact extraction │           │ │ • ruleset store│ │
                             │ • action exec     │           │ │ • Talon engine │ │
                             │ • STATELESS       │           │ │ • llm_review   │ │
                             └───────────────────┘           │ │ • explain/audit│ │
                                      ▲                      │ └───────┬────────┘ │
                                      │                      │         │          │
                             ┌────────┴────────┐             │   ┌─────▼──────┐   │
                             │ talooner CLI    │             │   │  talon-db  │   │
                             │ (gh extension)  │             │   │ PR facts   │   │
                             └─────────────────┘             │   └────────────┘   │
                                                             └────────────────────┘
```

### Component split — and why

The bot is **thin and stateless**; all state and all reasoning live cluster-side.

| Concern | Where | Why there |
|---|---|---|
| GitHub App JWT → installation token | bot | Needs the App private key; nothing else does |
| Webhook HMAC verify, event queue | bot | Must answer GitHub within 10s |
| Fact extraction from the GitHub API | bot | Only the bot holds an installation token |
| Fact storage, reactive `changes` operator | plugin → talon-db | Facts must outlive a single webhook |
| Ruleset parse, validate, compile | plugin | Rules are evaluated where facts live |
| Talon engine, defeasible resolution | plugin | Same |
| `llm_review` | plugin | Only the cluster holds tenant LLM credentials |
| `explain` / audit trail | plugin → talon-db | Decisions are queried long after the PR closes |
| Action execution against GitHub | bot | Only the bot holds an installation token |

The seam is: **the bot knows GitHub and knows nothing about Talon; the plugin
knows Talon and knows nothing about GitHub.** The plugin returns an abstract
action list; the bot translates it into API calls. That keeps `talooner-plugin`
testable without a GitHub fixture, and keeps the bot free of engine state.

### Why the bot is stateless

Facts already have to live in `talon-db` for reactive rules
(`when "pr.files_changed" changes`) to work at all. Duplicating any of it
bot-side would create a second source of truth for the same PR. So the bot keeps
nothing across requests except its in-memory queue; a restart mid-PR loses at
most an unprocessed webhook, which GitHub redelivers.

Subscription state (which PRs were invoked with `@talooner /review`) is state
too, so it lives cluster-side as a fact like everything else. The bot asks the
plugin "is this PR subscribed?" rather than remembering. A bot restart must not
silently stop reviewing a PR someone asked it to watch.

## Repos

Two repos. They are separate concepts and they version independently.

| Repo | Module | Contents |
|---|---|---|
| `talooner` | `github.com/opentalon/talooner` | The bot: GitHub App service + CLI (`cmd/talooner-bot`, `cmd/talooner`) |
| `talooner-plugin` | `github.com/opentalon/talooner-plugin` | OpenTalon gRPC plugin: engine, ruleset store, fact store, `llm_review`, and **the proto** |

The shared proto lives in `talooner-plugin`, since the plugin is the server and
owns the contract. The bot imports the generated Go package as a normal tagged
dependency — the same relationship `mcp-plugin` has with `opentalon`. Landing
order for a contract change: plugin first, tag, then bump the bot.

Naming: **Talooner** is the ecosystem *and* the bot. The bot is plain `talooner`;
everything else in the ecosystem is `talooner-*`.

### Dependency chain

`talooner-plugin` links `talon-language`, which carries
`replace github.com/opentalon/talon-db => ../talon-db`. A `replace` is not
transitive through a dependency — the **consuming** module must restate it. So
`talooner-plugin/go.mod` needs both:

```
replace github.com/opentalon/talon-db => ../talon-db
```

and a sibling `talon-db/` checkout, plus the same CI clone step
`talon-language/.github/workflows/ci.yml` uses. This is the documented workspace
convention (`CLAUDE.md`, "Cross-repo wiring"), not a workaround.

The bot links neither `talon-language` nor `talon-db` — it only speaks the
plugin's gRPC contract. That mirrors `opentalon-agents`, which deliberately
links no `talon-language` code.

## Invocation — explicit in v1

v1 does not review on its own. A human asks it to:

```
@talooner /review
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

Once invoked on a PR, the PR is **subscribed**: subsequent pushes and check
completions re-evaluate automatically, because that's what the reactive rules
(`when "pr.files_changed" changes`) mean. Subscription is per PR and ends when
the PR closes. `@talooner /stop` unsubscribes.

Automatic review on PR open, opt-in per repo via config, is a later phase. The
subscription machinery is identical; only the trigger differs.

Command surface in v1:

| Command | Effect |
|---|---|
| `@talooner /review` | Evaluate now, subscribe this PR |
| `@talooner /stop` | Unsubscribe |
| `@talooner /why` | Render `explain` for the current head sha |
| `@talooner /plan` | Evaluate the head-branch ruleset with no writes |

Commands are honoured only from users with write access to the repo. Otherwise
any drive-by account could invoke reviews and burn the maintainer's LLM budget.

## Request flow

### Ingest

```
POST /webhook
  ├─ verify X-Hub-Signature-256 (HMAC-SHA256, webhook secret)
  ├─ reject unknown event types
  ├─ enqueue {delivery_id, event, payload}
  └─ 202 Accepted            ← must be under 10s, GitHub's timeout
```

Delivery id is the idempotency key. GitHub redelivers on timeout; a redelivered
id that already produced a completed run is dropped.

### Evaluate

```
worker picks {installation_id, repo, pr}
  0. is this PR subscribed? (invoked via @talooner /review)  — else drop
  1. mint installation access token (cached, 1h TTL, refresh at 55m)
  2. load ruleset  ← BASE branch (see "Fork safety")
  3. extract facts (see facts.md)
  4. plugin action "evaluate_pr" {repo, pr, head_sha, facts JSON, ruleset, mode}
     (an OpenTalon plugin action, not a bespoke rpc — see plugin.md)
       └─ plugin: assert facts into talon-db, run engine,
                  resolve conflicts (defeasible), issue llm_review as needed,
                  return actions + explanation
  5. execute actions against GitHub (see actions.md)
  6. record outcome
```

`llm_review` runs *inside* step 4, cluster-side, synchronously from the engine's
point of view. The bot never sees an LLM.

### Concurrency

One in-flight evaluation per PR, serialized. A push arriving mid-evaluation
cancels the in-flight run — its head sha is already stale, and its actions would
be wrong. Actions are idempotent by construction: comments are sticky (edited in
place, keyed by a marker), check runs are updated by name, reviews are dismissed
and re-issued rather than stacked.

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

Three independent credentials. See `auth.md`.

1. **GitHub App private key** — bot only. Signs a JWT, exchanged for a
   short-lived installation token scoped to one install.
2. **Cluster API key** — bot → OpenTalon. Presented on connect; `whoami` returns
   tenant id, quota, enabled models. The bot refuses to start without it.
3. **LLM provider credentials** — cluster only. Never transit the bot, never
   appear in a webhook, never land in `talon-db`.

## Determinism

Same head sha + same base ruleset ⇒ same actions. Holds because:

- Fact extraction is a pure function of the PR at that sha.
- Conflict resolution is defeasible, not load-order dependent.
- `llm_review` results are stored as facts keyed by
  `(pr, head_sha, doc_url, prompt_version)`. A re-run at the same sha reads the
  stored fact instead of calling the model. New commit → new sha → fact absent →
  fresh call. The fact store *is* the cache; no separate layer.

A per-PR conversation is retained cluster-side for continuity and better
explanations, but each `llm_review` is a scoped turn whose result pins to its
head sha. The conversation informs the answer; it never changes an answer
already recorded.

## Deployment

**Fully self-hosted, permanently.** You run the OpenTalon cluster, the
`talooner-plugin` inside it, and the bot, and you register your own GitHub App
via the App manifest flow (the CLI generates the manifest). Nobody else ever
holds your secrets, because there is nobody else — there is no operator, no
central service, no shared App.

Consequences, stated plainly since they're the cost of this model:

- Onboarding is "provision a VPS", not "click install". That is a real adoption
  ceiling and it will not be lifted.
- There is no telemetry, no central error reporting, no way to know how many
  installs exist or what broke on them. Bug reports arrive only if someone files
  one.
- Every tenant runs a different version. Cross-repo compatibility between
  `talooner` and `talooner-plugin` has to be an explicit versioned contract, not
  an assumption — the version-skew failure mode the workspace `CLAUDE.md` already
  warns about, except now the two halves are operated by the same person on the
  same box, which makes it tractable.

The upside is that the security posture is trivial to reason about: the operator
holds no tenant secrets because the operator does not exist.
