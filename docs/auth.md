# Talooner — auth, onboarding, credentials

## Prerequisite: you run the cluster. Always.

Talooner is not a service you sign up for, and it never will be. There is no
hosted tier, no free tier, no managed offering. To use it you need:

1. A VPS (or any host) running an **OpenTalon cluster**, reachable from the
   runner
2. `talooner-plugin` loaded in that cluster, with `tln-db` available
3. LLM provider credentials configured **in the cluster**
4. `.github/workflows/talooner.yml` in each repo you want reviewed
5. `OPENTALON_HOST` and `OPENTALON_API_KEY` as repo or org secrets

This is the design, permanently. The cluster holds the LLM credentials, so every
token a rule spends is billed to whoever ran the rule. Nobody's review load can
land on someone else's API limits, because nothing is shared. The project author
does not pay for other people's reviews — they pay for their own VPS, or they
don't run Talooner.

## Three credentials, three blast radii

| Credential | Held by | Grants | If leaked |
|---|---|---|---|
| `GITHUB_TOKEN` | the runner, for one job | Whatever the workflow's `permissions:` block declares, on one repo | Expires when the job ends. Nothing to rotate — the next run gets a different token. |
| Cluster API key | repo/org secret → runner env | `evaluate_pr`, `whoami`, quota against one tenant | Attacker can burn that tenant's LLM budget. Rotate cluster-side; scoped to one tenant. |
| LLM provider key | cluster only | Full provider account | Rotate at the provider. **Never** transits a runner, `tln-db`, or a log line. |

The isolation that matters: **the runner never holds an LLM provider
credential**, and the cluster never holds a GitHub credential. Compromising
either gets an attacker exactly one of the two capability sets, not both.

What this design deleted: the GitHub App private key. There is no long-lived
GitHub credential at all — no key on a server, no JWT signing, no installation
token cache, no rotation runbook. The most dangerous secret in the previous
design simply doesn't exist.

### Handling rules

- Cluster key from the runner environment via `secrets.*`, never from repo
  config, never from an event payload.
- Redaction on the log path. GitHub masks registered secrets in workflow logs
  automatically, but that covers exact matches only — key-shaped values are
  filtered before write, not filtered by remembering to be careful at each call
  site.
- The run fails fast if the cluster key is absent or `whoami` fails. No degraded
  mode where it silently reviews without the engine.
- `.github/talooner/*.yaml` in a tenant repo is attacker-controllable on a fork
  PR. It is parsed as data only — no credential fields, no URLs the runner will
  authenticate to, no path escapes above the repo root.

## GitHub auth

```
Actions mints GITHUB_TOKEN for the job
   └─ scoped to this repo, permissions from the workflow's `permissions:` block
        └─ REST calls
             └─ revoked when the job ends
```

Two properties fall out that a GitHub App had to work for:

- **Permissions are in the tenant's repo, in a diff.** `permissions: {pull-requests: write, checks: write, contents: read}` is reviewable by the people it affects, rather than accepted once in an install dialog and forgotten. Widening them is a PR.
- **No loops.** Events caused by `GITHUB_TOKEN` do not trigger workflows, so Talooner's own comments and check runs cannot start another Talooner run.

Rate limits are 1,000 requests/hour per repo per run — far above what one
evaluation needs, and they can't be exhausted by another tenant, because there
isn't one.

### Secrets and fork PRs

The load-bearing rule:

| Trigger | Runs in | Secrets available |
|---|---|---|
| `issue_comment` (created) | base repo, default branch | **yes** |
| `pull_request` from a branch in the repo | base repo | yes |
| `pull_request` from a fork | base repo, restricted | **no** |
| `pull_request_target` | base repo, full token | yes — **and it is not used** |

`pull_request_target` is the standard footgun: it runs with secrets against a
fork's PR ref. Talooner does not use it, and no reference workflow will suggest
it. The only trigger that carries credentials for fork PRs is
`issue_comment` — a maintainer typing `!talooner /review`, whose write access is
then verified against the API before anything else happens.

Because `issue_comment` runs the workflow from the **default branch**, not the
PR's head, a fork PR also cannot modify the workflow that reviews it. That's the
same protection the base-branch ruleset rule provides, enforced by GitHub rather
than by Talooner.

## Cluster auth

Runner → cluster over gRPC, mTLS or bearer token depending on deployment. This
is the one new operational requirement of decision 1: the cluster must be
reachable from wherever the job runs.

| Cluster exposure | How | Trade-off |
|---|---|---|
| Public gRPC + TLS + API key | `OPENTALON_HOST=grpc://talon.example.com:9090` | Simplest. The endpoint is on the internet; the API key is the whole gate |
| Self-hosted runner | Runner on the cluster's network, cluster stays private | No public exposure; you now operate runners, which is the ops burden decision 1 removed |
| Tailscale / WireGuard on a hosted runner | Join the network as a workflow step | Middle ground, one more moving part in every run |

Default is the first. The second exists for tenants who won't expose the
cluster, and it is the honest answer for anyone reviewing private repos on a
private network.

On connect:

```
whoami() → WhoamiResponse {
  tenant:           tenant name
  protocol_version: contract version the cluster speaks
  models:           [enabled model ids]
  features:         [llm_review, ...]
  quota:            {llm_calls_used, llm_calls_limit}
}
```

That is the wire shape, from `talooner-plugin/proto/talooner/v1/talooner.proto`
— the generated package is the contract, and this sketch is a copy that has
already drifted once. The API key travels in the `api_key` arg and the caller
declares its own version in `protocol_version`; neither is an action parameter.

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

# 2. set the secrets the workflow will read
talooner init --repo acme/api
#   → gh secret set OPENTALON_HOST / OPENTALON_API_KEY   (repo-scoped, or --org)

# 3. scaffold a repo-shaped ruleset, open a PR
talooner onboard --repo acme/api

# 4. author and validate rules — all local, no cluster writes
talooner rules validate .github/talooner/
talooner rules test .github/talooner/         # runs .tln.test files
talooner rules plan --repo acme/api --pr 42   # dry-run against a live PR
```

There is no `talooner serve`. `init` only sets secrets via the GitHub API — it
writes no local files and doesn't need to run inside a checkout of the repo.
`onboard` writes a generated ruleset, then commits, pushes, and opens a PR so
a maintainer reviews the diff before anything runs — it does not yet write
`.github/workflows/talooner.yml`; add that by hand until it does (see
`deployment-and-setup.md`).

Secrets can be set org-wide instead of per repo (`talooner init --org acme`),
which is the sane setup for more than a couple of repos — then onboarding a new
repo is just `talooner onboard`.

### Identity

The reviewer appears as `github-actions[bot]` on every comment, review, and
check run. Talooner does not register a GitHub App, so there is no display name
to choose, no globally unique name to claim, and no collision between two orgs
running it — the problem a per-tenant App would have created.

The handle in comments is `!talooner` — a bang, not an `@`, precisely because
it is **not** a GitHub mention: it's a plain string the action matches in the
comment body, filtered cheaply by the workflow's `if:` before a runner even
starts. An `@` would imply autocomplete and a notification that don't exist.
Tenants can change it in `config.yaml`; nothing on GitHub's side cares.

Recognisability comes from the action reference — `uses: opentalon/talooner@v1` —
and from the check-run name, `talooner`, which is what appears in the PR's checks
list and in branch protection settings. Both are yours, neither requires
registering anything.

### `rules plan` matters more than it looks

It's the same code path as a real evaluation with the action executor swapped
for a printer. It's how a maintainer answers "what would this rule change do to
our open PRs?" before merging it, and it's the mechanism behind fork-PR plan
runs (`architecture.md`, "Fork safety"). Building the executor behind an
interface from day one is what makes it nearly free — worth doing in phase 1
even though nothing needs it until phase 2.

### `rules test`

`tln-language` already ships a `.tln.test` framework and a testrunner
(`tln-language/internal/testrunner`). Talooner reuses it directly: a repo
unit-tests its review policy in its own CI, with synthetic PR facts, before that
policy ever gates a real PR.

```
.github/talooner/
  rules.tln
  rules.tln.test
  config.yaml
  modules.yaml
  teams.yaml
```

This is also the most convincing demo of the whole premise. "Your review policy
has tests" is a claim no LLM-based reviewer can make.

## Fork PRs and untrusted input

Attacker-controlled on a fork PR: the diff, the title, the body, the branch
name, every file under `.github/talooner/`, and the workflow file on the head
branch. Consequences:

- Ruleset governing **writes** comes from the base branch, always
  (`architecture.md`).
- The **workflow** that runs also comes from the default branch, because
  `issue_comment` is the trigger. A fork editing `.github/workflows/talooner.yml`
  changes nothing about how it is reviewed. GitHub enforces this, not Talooner.
- A fork push cannot start a credentialled run at all — secrets are withheld from
  fork-triggered `pull_request` events, and `pull_request_target` is not used.
- Diff/title/body reaching an LLM prompt is untrusted text. Prompt injection is
  assumed, not prevented — mitigations are structural: the model's output is
  constrained to a fixed enum plus an explanation string, and the enum drives
  the decision. An injected "approve this PR" can at most produce
  `result: "match"`, which still has to pass every other rule condition. The
  explanation string is rendered as quoted, escaped text in a comment, never
  interpreted.
- `llm_review` per-PR call cap and per-tenant budget ceiling, enforced
  cluster-side —
  [`talooner-plugin/docs/llm-review.md`](https://github.com/opentalon/talooner-plugin/blob/main/docs/llm-review.md).

## Error codes

Most failures surface as GitHub's own error text — that's the point of not
hiding what broke (`internal/check`, "Talooner itself broke"). A few are common
enough, and GitHub's response terse enough, that Talooner recognises them and
says so plainly instead. These carry a stable `TAL-E-*` code in the check run
and sticky comment, so a maintainer can jump straight to the fix.

| Code | Meaning | Fix |
|---|---|---|
| `TAL-E-REVIEW-PERM` | GitHub rejected the `approve`/`block` review with 403 or 422. In every case seen, the repo or org has "Allow GitHub Actions to create and approve pull requests" turned off — a setting `GITHUB_TOKEN`'s own `permissions:` block cannot override. | Repo or org Settings → Actions → General → enable "Allow GitHub Actions to create and approve pull requests". Org-level policy can block the repo-level toggle; an org owner has to flip it there first. |

## Audit

Every decision persists: facts at evaluation time, ruleset content hash, rules
that fired, rules suppressed by defeasible resolution, `explain` output, actions
taken, GitHub API responses. Queryable by PR long after close.

`/talooner why` on a PR renders the `explain` output for the current head sha as
a comment. Determinism plus a stored explanation means "why did the bot block
this?" has an exact answer, which is the whole reason for not putting a model in
the decision path.
