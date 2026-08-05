# Talooner — auth, onboarding, credentials

## Prerequisite: you run the cluster. Always.

Talooner is not a service you sign up for, and it never will be. There is no
hosted tier, no free tier, no managed offering. To use it you need:

1. A VPS (or any host) running an **OpenTalon cluster**
2. `talooner-plugin` loaded in that cluster, with `talon-db` available
3. LLM provider credentials configured **in the cluster**
4. A GitHub App registered against your org, installed on your repos
5. The `talooner` bot running, holding the App private key and a cluster API key

This is the design, permanently. The cluster holds the LLM credentials, so every
token a rule spends is billed to whoever ran the rule. Nobody's review load can
land on someone else's API limits, because nothing is shared. The project author
does not pay for other people's reviews — they pay for their own VPS, or they
don't run Talooner.

## Three credentials, three blast radii

| Credential | Held by | Grants | If leaked |
|---|---|---|---|
| GitHub App private key | bot process | Ability to mint installation tokens for every install of that App | Attacker can comment/review as the bot on all installed repos. Cannot merge or push (permissions don't include it). Rotate in App settings; all outstanding installation tokens expire within 1h. |
| Cluster API key | bot process | `llm_review`, `whoami`, quota against one tenant | Attacker can burn that tenant's LLM budget. Rotate cluster-side; scoped to one tenant. |
| LLM provider key | cluster only | Full provider account | Rotate at the provider. **Never** transits the bot, a webhook, `talon-db`, or a log line. |

The isolation that matters: **the bot never holds an LLM provider credential**,
and the cluster never holds a GitHub credential. Compromising either component
gets an attacker exactly one of the two capability sets, not both.

### Handling rules

- Private key and cluster key from env or file, never from repo config, never
  from a webhook payload.
- Redaction on the log path — key-shaped values are filtered before write, not
  filtered by remembering to be careful at each call site.
- The bot refuses to start if the cluster key is absent or `whoami` fails. No
  degraded mode where it silently reviews without the engine.
- `.github/talooner/*.yaml` in a tenant repo is attacker-controllable on a fork PR. It
  is parsed as data only — no credential fields, no URLs the bot will
  authenticate to, no path escapes above the repo root.

## GitHub App auth chain

```
App private key (RS256)
   └─ sign JWT (iss=app_id, exp≤10m)
        └─ POST /app/installations/{id}/access_tokens
             └─ installation token (1h TTL, scoped to that installation)
                  └─ REST/GraphQL calls
```

Tokens cached in memory, refreshed at 55m, never persisted. Rate limits accrue
per installation — the tenant's own quota, not a shared pool.

Webhook verification: `X-Hub-Signature-256`, HMAC-SHA256 over the raw body with
the webhook secret, compared in constant time. Unsigned or mismatched
deliveries are dropped before parsing — the JSON is not decoded until the
signature checks out, so a malformed-payload parser bug isn't reachable
pre-auth.

## Cluster auth

Bot → cluster over gRPC, mTLS or bearer token depending on deployment. On
connect:

```
whoami() → {
  tenant_id, tenant_name,
  quota:   {llm_calls_remaining, budget_remaining, period_resets_at},
  models:  [enabled model ids],
  features:[llm_review, dispatch, ...]
}
```

`whoami` is the capability handshake, not just an identity check. The bot uses
it to know whether `llm_review` is even available before loading a ruleset that
depends on it — a ruleset using `llm_review` on a cluster without a configured
provider gets a validation warning at load time, not a runtime failure on the
first PR.

Quota exhaustion mid-PR asserts `llm_review.result = "error"` with
`llm_review.error = "quota exhausted"`, so a ruleset can react. It does not
crash the run, and it does not silently approve.

## Onboarding — CLI only in v1

No web dashboard. A `gh` extension + standalone binary:

```bash
gh extension install opentalon/gh-talooner

# 1. cluster reachability + capability check
talooner cluster login --url grpc://talon.example.com:9090 --key $OPENTALON_KEY
talooner cluster whoami
#   tenant: acme  quota: 4,812 calls  models: claude-*  features: llm_review

# 2. generate a GitHub App manifest, open the browser to create it
talooner app create --org acme
#   → writes .talooner-app.json (app id, private key path, webhook secret)

# 3. run the bot
talooner serve --config talooner.yaml

# 4. author and validate rules
talooner rules validate .github/talooner/
talooner rules test .github/talooner/            # runs .talon.test files
talooner rules plan --repo acme/api --pr 42   # dry-run against a live PR
```

`app create` uses GitHub's App manifest flow, so the tenant creates the App
under their own org and the operator never sees the private key.

### `rules plan` matters more than it looks

It's the same code path as a real evaluation with the action executor swapped
for a printer. It's how a maintainer answers "what would this rule change do to
our open PRs?" before merging it, and it's the mechanism behind fork-PR plan
runs (`architecture.md`, "Fork safety"). Building the executor behind an
interface from day one is what makes it nearly free — worth doing in phase 1
even though nothing needs it until phase 2.

### `rules test`

`talon-language` already ships a `.talon.test` framework and a testrunner
(`talon-language/internal/testrunner`). Talooner reuses it directly: a repo
unit-tests its review policy in its own CI, with synthetic PR facts, before that
policy ever gates a real PR.

```
.github/talooner/
  rules.talon
  rules.talon.test
  config.yaml
  modules.yaml
  teams.yaml
```

This is also the most convincing demo of the whole premise. "Your review policy
has tests" is a claim no LLM-based reviewer can make.

## Fork PRs and untrusted input

Attacker-controlled on a fork PR: the diff, the title, the body, the branch
name, every file under `.github/talooner/`. Consequences:

- Ruleset governing **writes** comes from the base branch, always
  (`architecture.md`).
- Diff/title/body reaching an LLM prompt is untrusted text. Prompt injection is
  assumed, not prevented — mitigations are structural: the model's output is
  constrained to a fixed enum plus an explanation string, and the enum drives
  the decision. An injected "approve this PR" can at most produce
  `result: "match"`, which still has to pass every other rule condition. The
  explanation string is rendered as quoted, escaped text in a comment, never
  interpreted.
- `llm_review` per-PR call cap and per-tenant budget ceiling, enforced
  cluster-side.

## Audit

Every decision persists: facts at evaluation time, ruleset content hash, rules
that fired, rules suppressed by defeasible resolution, `explain` output, actions
taken, GitHub API responses. Queryable by PR long after close.

`/talooner why` on a PR renders the `explain` output for the current head sha as
a comment. Determinism plus a stored explanation means "why did the bot block
this?" has an exact answer, which is the whole reason for not putting a model in
the decision path.
