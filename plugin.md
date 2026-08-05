# `talooner-plugin` — the server side

Everything that runs **inside the OpenTalon cluster**. Written to be handed to
one backend dev as a self-contained brief; the other docs cover the bot and the
GitHub surface.

Repo: `github.com/opentalon/talooner-plugin`.

Diagram: `diagrams.md` §2b (internals), §1 (placement in the cluster),
§3–§5 (its role in each flow).

---

## Responsibility

The plugin receives extracted facts and ruleset text, and returns a decision. It
never touches GitHub, never sees a webhook, never holds a GitHub credential.

| Owns | Does not own |
|---|---|
| Ruleset parse / validate / compile | Anything GitHub-shaped |
| Fact assertion and per-PR scoping in `talon-db` | Fact *extraction* — that's the bot |
| Talon engine execution, reactive rules | Executing actions — it returns them as data |
| Defeasible conflict resolution | Deciding what a GitHub check run is |
| `llm_review` — the only LLM call in the system | The GitHub App private key |
| `explain` / audit persistence | Webhook verification |
| Subscription state | Rate limits against GitHub |

The seam is deliberate: the plugin returns an **abstract action list** and the
bot translates it into API calls. Consequence for whoever builds this — the
entire plugin is testable with zero GitHub fixtures. Feed it facts, assert on
returned actions.

---

## Protocol — this is not a custom gRPC service

**Correction to earlier drafts of `architecture.md`, which sketched
`EvaluatePR` as a bespoke rpc.** It isn't one. OpenTalon's plugin host defines a
fixed service in `opentalon/proto/plugin.proto:9`:

```proto
service PluginService {
  rpc Init(PluginInitRequest) returns (google.protobuf.Empty);
  rpc Execute(ToolCallRequest) returns (ToolResultResponse);
  rpc Capabilities(google.protobuf.Empty) returns (PluginCapabilities);
  rpc RefreshCapabilities(google.protobuf.Empty) returns (PluginCapabilities);
  rpc ExecuteBidi(stream HostMessage) returns (stream PluginMessage);
}
```

A plugin does not add rpcs. It declares **actions**, and the host routes calls to
`Execute`. So `EvaluatePR` is an *action name*, not a method. This changes the
contract design in ways worth knowing before writing code:

```proto
message ToolCallRequest {
  string id = 1;
  string plugin = 2;
  string action = 3;
  map<string, string> args = 4;      // ← string values only
  map<string, CredentialHeader> credential_headers = 6;
}

message ToolResultResponse {
  string call_id = 1;
  string content = 2;                // human-readable
  string error = 3;
  string structured_content = 4;     // ← JSON, this is the real return channel
}
```

`args` is `map<string, string>`. Facts are structured data, so they travel as a
JSON blob in a single arg, and the decision comes back as JSON in
`structured_content`. `content` carries a human-readable summary — useful in
logs and when a person invokes the action directly.

### Actions to declare

| Action | Args | Returns (`structured_content`) |
|---|---|---|
| `evaluate_pr` | `repo`, `pr`, `head_sha`, `facts` (JSON), `ruleset` (text), `mode` (`execute` \| `plan`) | `{actions: [...], explain: {...}, warnings: [...]}` |
| `is_subscribed` | `repo`, `pr` | `{subscribed: bool, since: ts}` |
| `set_subscription` | `repo`, `pr`, `state` | `{subscribed: bool}` |
| `assert_facts` | `repo`, `pr`, `facts` (JSON) | `{woke_rules: [...], actions: [...]}` — the custom-facts path |
| `validate_ruleset` | `ruleset` (text) | `{valid: bool, diagnostics: [...]}` — powers `talooner rules validate` |
| `explain_pr` | `repo`, `pr`, `head_sha` | `{explain: {...}}` — powers `@talooner /why` |
| `whoami` | — | `{tenant, quota, models, features}` |

### Every action must set `user_only: true`

`Action.user_only` (`plugin.proto:173`) hides an action from the LLM and blocks
LLM-sourced calls. Talooner's actions must all set it.

Without it, any LLM running in that cluster — an unrelated conversation, a
different channel — could invoke `talooner.evaluate_pr` with arguments it made
up, and the bot would faithfully execute the returned actions against a real
repo. A model must never be able to reach into the decision path. The whole
design premise is that rules decide and the model answers questions; `user_only`
is what enforces that at the protocol level rather than by convention.

`read_only: true` on `is_subscribed`, `validate_ruleset`, `explain_pr`,
`whoami`. The rest mutate.

### Open: payload size

`args` values are strings in a unary gRPC call. A large PR's fact blob plus
ruleset text is realistically tens to low hundreds of KB — fine. `pr.diff` is
the risk: it can be megabytes.

Options, in preference order:

1. **Don't send the diff.** The engine only needs it for `llm_review`. Send a
   content hash plus a fetch handle, and have the bot serve the diff back on
   demand — but that inverts the dependency and gives the plugin a reason to
   know about GitHub. Rejected unless forced.
2. **Send it, size-capped**, with `pr.diff_truncated = true` asserted past the
   cap. Rules can match on the truncation. Simple, honest, keeps the seam.
3. `ExecuteBidi` streaming if the cap turns out too small.

**Leaning 2.** Cap is a config value; start at 1 MB. This is the "plugin protocol
fit for large payloads" item in the phase-0 table (`roadmap.md`).

---

## Internals

See `diagrams.md` §2b for the picture. In execution order:

1. **gRPC surface** — implements `PluginService`, owns the proto. Decodes the
   fact JSON, validates the request shape.
2. **Ruleset loader** — parse, validate, compile. Two rulesets are always
   loaded: the tenant's, and Talooner's own `strict` base ruleset.
3. **Fact assertion** — facts land in `talon-db` under a per-PR scope. Facts
   absent from this request are *retracted*, not left stale; the bot always
   sends a full re-derivation, never a delta. This is what makes approval
   retraction work (`diagrams.md` §4).
4. **Engine** — `talon-language`'s RETE-ish reactive engine.
5. **`llm_review`** — invoked only when a rule fires it. See below.
6. **Defeasible resolution** — `strict` > `overrides` > priority
   (`talon-language/docs/defeasible.md`). Not an ad-hoc "block wins" in Go.
7. **`explain` / audit** — persisted before returning, so a decision is
   queryable even if the bot crashes before executing it.

### The base ruleset

Talooner ships rules it always loads, declared `strict` so a tenant ruleset
can't defeat them:

```talon
strict rule "Never approve a PR with unresolved conflicts" { ... }
strict rule "Never approve while required checks are still running" { ... }
```

Phase-0 open item: does `overrides`/priority resolution work correctly across
*two separately loaded* rulesets? If it doesn't, this design needs a change in
`talon-language/internal/defeasible`.

---

## Fact scoping and lifetime

One scope per `(repo, pr)`. Contents:

- Facts asserted by the bot each run (`pr.*`, `user.*`, `review.*`, …)
- Facts pushed by tenant CI (`preview.*`, `screenshots.*`, …)
- `llm_review.*` results, keyed by head sha
- Subscription state
- Decision + `explain` records

Retention: facts expire after a grace period once the PR closes; decisions and
`explain` outlive them, because "why did the bot block this?" gets asked months
later. Defaults are placeholders — see `OPEN-QUESTIONS.md` B5.

**Namespace enforcement lives here.** `assert_facts` must reject writes to
`pr.*`, `user.*`, `review.*`, and `llm_review.*`. Without that check, a tenant's
CI workflow can POST `pr.tests_passing: true` and defeat the entire ruleset. The
bot also filters, but the plugin is the last line and the one that owns the
store.

**Unset is not false.** A condition on an unset fact must evaluate to *unknown*,
and `not <unknown>` must be unknown — not true. Two-valued logic means a PR whose
fact extraction partially failed sails through `not is "critical_path"` and gets
auto-approved. This is a `talon-language` property, verified in phase 0, and it
is the single most dangerous detail in the system. `facts.md`, "Unset is not
false".

---

## `llm_review`

The only LLM call anywhere in Talooner, and it lives here because **the cluster
is the only component holding provider credentials**.

```
rule fires llm_review(doc_url, diff)
  → look up fact (pr, head_sha, doc_url, prompt_version)
      hit  → return it. No API call, no spend.
      miss → call the model → store result as a fact → return it
```

The fact store *is* the cache. No separate cache layer, no invalidation logic: a
new commit means a new head sha means the fact is absent means the model runs
again. See `diagrams.md` §5.

Constraints:

- Prompt lives in a `.txt` file, never a Go string literal
  (`opentalon/CLAUDE.md` is explicit about this and CI enforces it).
- Output is a **fixed enum** — `match` | `mismatch` | `unclear` | `too_large` |
  `error` — plus a free-text explanation. The enum drives decisions; the
  explanation is only ever rendered as escaped, quoted text.
- Prompt injection from the diff is *assumed*, not prevented. The mitigation is
  structural: an injected "approve this PR" can at most produce
  `result: "match"`, which still has to satisfy every other condition in the
  rule. That is why the output is constrained rather than free-form.
- Per-PR call cap and per-tenant budget ceiling, enforced here. Quota exhaustion
  asserts `llm_review.result = "error"` with an explanatory
  `llm_review.error` — it does not crash the run, and it must never silently
  approve.
- A per-PR conversation is retained for continuity, but each review is a scoped
  turn whose result pins to its head sha. The conversation informs an answer; it
  never changes an answer already recorded. That's what preserves "same sha ⇒
  same actions".

---

## Deployment in the cluster

The plugin is a normal OpenTalon plugin, so the existing `PluginConfig` in
`k8s-operator`'s CRD already fits — no operator changes needed to *run* it
(`k8s-operator/api/v1alpha1/opentaloninstance_types.go:312`):

```yaml
spec:
  config:
    plugins:
      - name: talooner
        source: github.com/opentalon/talooner-plugin@v0.1.0
        env:
          - name: TALOONER_DB_PATH
            value: /data/talooner.db
    models:
      - name: reviewer
        provider: anthropic
        apiKeySecret:
          name: talooner-llm
          key: api-key
```

The phase-4 `k8s-operator` item in `roadmap.md` is about first-class ergonomics
(a `talooner:` block, sane defaults, PVC sizing), not about feasibility.

### Dependency chain — read before the first build

`talooner-plugin` links `talon-language`, which carries
`replace github.com/opentalon/talon-db => ../talon-db`. **A `replace` is not
transitive** — the consuming module must restate it. So `talooner-plugin/go.mod`
needs its own `replace` line *and* a sibling `talon-db/` checkout, plus the CI
clone step that `talon-language/.github/workflows/ci.yml` already uses.

Without it every build fails with `replacement directory ../talon-db does not
exist`. This is the documented workspace convention, not a bug to fix.

---

## Testing

- **Unit**: feed facts + ruleset, assert on returned actions. No GitHub, no
  network.
- **Ruleset tests**: reuse `talon-language`'s `.talon.test` framework and
  `internal/testrunner` directly. `validate_ruleset` and `talooner rules test`
  are the same code path.
- **VCR cassettes** for `llm_review`, per the core's convention. Editing a
  prompt `.txt` invalidates cassettes and fails CI until re-recorded — see
  `opentalon/CLAUDE.md`.
- **Determinism test**, and this one is the product: evaluate the same fact set
  twice, assert byte-identical actions and exactly one LLM call.

---

## Phase-0 items this component depends on

None of these are Talooner code; all are verification (or fixes) in
`talon-language` / `talon-db`. Full table in `roadmap.md`.

| Item | Consequence if it doesn't hold |
|---|---|
| Three-valued evaluation, `not <unknown>` = unknown | Failed extraction auto-approves critical PRs. **Hard blocker.** |
| `contains`/`matches` quantify over list operands | Every `pr.touches_*` predicate is unimplementable as designed |
| Facts usable as action arguments (`do assign "pr" "user.owner"`) | The whole `user.*` namespace is dead weight |
| `{ident.field}` interpolation in action args | Comment templating doesn't work |
| Cross-ruleset defeasible resolution | The `strict` base ruleset can't protect anything |
| External fact assertion wakes reactive rules | `assert_facts` does nothing; preview/screenshot/scan rules never fire |
| `talon-db` handles many small concurrent scopes | Doesn't scale past a handful of open PRs |

Recommended first task for whoever takes this: write the brief's example ruleset
as a `.talon` + `.talon.test` in `talon-language/examples/`, running on synthetic
facts, no GitHub involved. It answers most of the table above and costs a day. If
it can't be written, the design is wrong and that's worth knowing before any
plugin code exists.
