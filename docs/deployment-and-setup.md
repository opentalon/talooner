# Deployment and setup

A practical walkthrough for onboarding a repo to Talooner: loading
`talooner-plugin` into an already-running OpenTalon cluster, then wiring a
repo to talk to it. For the design behind any of this, see `auth.md`
(credentials, identity), `architecture.md` (how a run works), `actions.md`
(what a rule can do), and `facts.md` (what a rule can read).

This assumes an OpenTalon cluster is already up. Talooner is never a hosted
service — "you run the cluster" is permanent (`auth.md`, "Prerequisite").

## Part 1 — load `talooner-plugin` into the cluster

Skip this if the cluster already has `talooner-plugin` loaded for your
tenant — check with `talooner cluster whoami` (Part 2) before repeating this.

`talooner-plugin` is a normal OpenTalon plugin, loaded the same way any other
plugin is: a `plugins:` block in the cluster's `config.yaml`.

```yaml
plugins:
  talooner:
    enabled: true                 # required — PluginConfig.Enabled defaults false
    github: opentalon/talooner-plugin
    ref: <commit-sha>             # pin a commit; no tag past v0.1.0 exists yet
    grpc_port: 50100              # opens the inbound PluginService gateway
    config:
      tenants:
        - name: <tenant-name>
          api_key: "${TALOONER_API_KEY}"
          quota: {calls_limit: 0} # 0 = unlimited
      fact_retention_days: 90
      rate_limit_per_minute: 60
```

Notes, each one a real way this has failed before:

- **`enabled: true` is not implied by the block existing.** Omitting it means
  the plugin is silently skipped at startup (`plugin disabled, skipping`) —
  no error, no load.
- **At least one tenant is required.** `Configure` hard-fails on an empty
  tenant list; the plugin won't even start serving.
- **`grpc_port` is what makes the plugin reachable from outside the
  orchestrator's LLM tool-call loop.** `talooner`'s CLI and the GitHub Action
  both dial `PluginService` directly, as a peer of core, not through core's
  normal plugin-execution path — without `grpc_port`, nothing outside the
  cluster can reach it. The gateway forwards `Execute` verbatim to whatever
  `Client` the `Manager` already holds; it does not authenticate or inspect
  the request. Auth is the plugin's own concern — the tenant `api_key`
  above, carried as a normal `Execute` arg.
- `api_key` supports `${ENV_VAR}` expansion — set the real value in the
  cluster process's environment, never hardcode it in `config.yaml`.
- `ref` should be a commit sha, not a branch or `master` — the plugin is
  fetched via `git clone --branch <ref>` and built fresh (`internal/bundle/fetch.go`),
  so an unpinned ref means the cluster's behavior can change under you on
  restart.

If the cluster is deployed via `k8s-operator`, the plugin also needs an
`OpenTalonInstance` ingress entry so the gRPC port is reachable from outside
the cluster (a GitHub Actions runner has to reach it over the internet, or
you're running a self-hosted runner on the same network — see "Reachability"
in Part 2):

```yaml
spec:
  config:
    plugins:
      talooner:
        ingress:
          host: talooner.example.com
          path: "/"                 # must be "/" for GRPC — a path prefix never
                                     # matches a gRPC method path
          port: 50100
          protocol: GRPC
          className: nginx          # required — an Ingress with no className
                                     # is not picked up for TLS termination,
                                     # even if cert issuance succeeds
          tlsSecretName: talooner-plugin-tls
```

`spec.config.plugins` is k8s-resource config only (Ingress/Service); it does
not reach `config.yaml` — the `plugins:` block above is what the cluster
actually loads. The two are independent and both required.

Once deployed, confirm the plugin loaded before moving on:

```
kubectl logs <core-pod> | grep talooner
# expect: loaded plugin plugin=talooner
#         plugin gateway listening ... port=50100
```

## Part 2 — onboard a repo

```bash
gh extension install opentalon/gh-talooner
# or: download a talooner_<os>_<arch> binary from a talooner release directly
```

### Reachability

`OPENTALON_HOST` is the cluster's `grpc_port` from Part 1, reachable from
wherever the workflow runs — a GitHub-hosted runner needs it public
(`grpc://host:port`, TLS by default; `http://` only for a throwaway test);
a private cluster needs a self-hosted runner on the same network instead.
See `auth.md`, "Cluster auth" for the trade-offs.

### Login and confirm

```bash
talooner cluster login --url <OPENTALON_HOST> --key <tenant api_key>
talooner cluster whoami
#   tenant: <name>  quota: ...  models: ...  features: [...]
```

`login` only saves credentials locally (`~/.talooner/credentials`, not
committed anywhere); `whoami` is what actually dials the cluster. If this
fails, fix it before going further — `init` below depends on stored
credentials existing.

### Wire up the repo

Run inside a local clone of the target repo, at its root — `init` writes
files relative to the current directory, it does not clone anything itself:

```bash
cd <local clone of the target repo>
talooner init --repo <owner/name> [--org <org>]
```

Writes, in order:

- `.github/workflows/talooner.yml`
- `.github/talooner/rules.tln` (starter policy)
- `.github/talooner/rules.tln.test`
- `gh secret set OPENTALON_HOST` / `OPENTALON_API_KEY` — repo-scoped by
  default, org-scoped with `--org <org>` (the sane choice past a couple of
  repos on one cluster)

Nothing is committed automatically — `init` only writes local files and sets
secrets; commit and push the workflow and ruleset yourself.

### Two hand-fixes `init` does not make for you

**1. The action pin.** `init`'s template pins `uses: opentalon/talooner@v1`
— **`v1` does not exist as a tag yet.** Until a real `vX.Y.Z` release is cut,
edit `.github/workflows/talooner.yml` to pin a real published tag instead
(check `git tag --list` in `talooner` — currently `v0.0.1-alpha2`, or newer)
or a commit sha off `master`. This has to be bumped by hand for every new
prerelease until `v1` exists; nothing tracks it automatically.

**2. `.github/talooner/config.yaml` — not written by `init` at all.**
Without it, `pr.tests_passing` and `pr.lint_passing` are never set (they
require the tenant to declare which CI check names to match), so any rule
conditioned on them silently never fires. Add it by hand, naming your repo's
real CI check names exactly:

```yaml
checks:
  tests: ["Test"]     # your test job's check name, as it appears on a PR
  lint: ["Lint"]       # your lint job's check name
```

### The other prerequisite: repo/org Actions settings

Two separate GitHub Actions settings gate different things, and it's easy to
check the wrong one:

- **"Policies"** (which actions/workflows are allowed to run at all) — not
  this one.
- **"Workflow permissions"**, further down the same settings page — has a
  checkbox: **"Allow GitHub Actions to create and approve pull requests."**
  Off by default. Without it, `talooner`'s `approve`/`block` actions fail —
  the sticky comment and check run still post fine (different permission
  path), but the review itself comes back `403`/`422`, and the check run
  reads `TAL-E-REVIEW-PERM` (see Troubleshooting).

  Set at `https://github.com/organizations/<org>/settings/actions` (org
  level) or the repo's own Settings → Actions → General (repo level, only
  if the org hasn't locked it — an org-wide lock overrides the repo
  toggle, and only an org admin can change it there).

## Part 3 — author and test rules, before they gate a real PR

All local, no cluster writes:

```bash
talooner rules validate .github/talooner/       # compiles the ruleset
talooner rules test .github/talooner/           # runs rules.tln.test
talooner rules plan --repo <owner/name> --pr <n>  # dry-run against a live PR, zero writes
```

`rules.tln` is a starting point, not a template to leave untouched — see
`actions.md` for the full predicate/verb surface and `facts.md` for what a
condition can read (`pr.*`, `user.*`, `module.*`, `team.*`, `review.*`).
Two things the starter ruleset deliberately omits, both compile-time traps
if reintroduced carelessly:

- **No `strict` rule block.** The plugin always loads its own strict base
  ruleset server-side ("never approve a PR with unresolved conflicts") and
  imports it into every tenant ruleset. Declaring a rule with that same name
  is a compile error, not extra safety.
- **No `do notify` rule.** The `notify` verb has no executor yet — a fired
  `notify` action fails the whole run rather than degrading gracefully.

## Part 4 — run it

```bash
gh pr comment <n> --repo <owner/name> --body "@talooner /review"
```

`@talooner` is a plain string match in the workflow's `if:` condition, not a
GitHub mention — no GitHub user or App named `talooner` needs to exist for
this to work. Expect one `talooner` check run and one sticky comment on the
PR, updated in place (never duplicated) on every push and re-trigger.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| Check run says "no rules fired" or reports success with nothing matched, but a rule should have fired | `.github/talooner/config.yaml` missing or its `checks:` names don't match your real CI job names | Add/fix `config.yaml` (Part 2) |
| Check run reads `TAL-E-REVIEW-PERM`, review never posts, comment/check still work | "Allow GitHub Actions to create and approve pull requests" is off | Part 2, "The other prerequisite" |
| `uses: opentalon/talooner@v1` fails to resolve | `v1` doesn't exist as a tag yet | Pin a real tag or commit sha instead (Part 2) |
| `talooner init` fails with "no stored credentials" | `cluster login` wasn't run, or was run in a different environment | `talooner cluster login` again, same machine/CI you're running `init` from |
| Plugin never loads, cluster logs show `plugin disabled, skipping` | Missing `enabled: true` in the `plugins:` block | Part 1 |

**When reproducing a failure that's specific to the Actions bot's
credentials, don't fire the same API call yourself with `gh api` or a
personal token** — you're authenticated as a human with different
permissions than `GITHUB_TOKEN`, and the call may just succeed for real
instead of failing the same way. Reproduce it inside the actual workflow run
(or a fake in tests), not against a live PR with your own credentials.
