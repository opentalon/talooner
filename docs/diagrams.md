# Talooner — design diagrams

Shareable overview for the dev team. Mermaid, so it renders on GitHub and stays
diffable in review.

Companion docs: `architecture.md` (prose), `facts.md`, `actions.md`, `auth.md`.

## Notation

**C4 for structure, UML for behaviour.** Nothing else.

| Level | Diagram | Notation |
|---|---|---|
| C4 L1 — Context | §0 | Mermaid `flowchart` |
| C4 L2 — Container | §1 | Mermaid `flowchart` |
| C4 L3 — Component | §2a | Mermaid `flowchart` |
| C4 L4 — Code | — | Skipped. The code is the code. |
| Behaviour | §3–§6 | UML sequence |
| Behaviour | §7 | UML state machine |
| Supporting | §8, §9 | Mermaid `flowchart` |

Two notation decisions worth knowing before someone "fixes" them:

- **Not full UML.** Class and deployment diagrams would encode decisions that
  aren't made yet and go stale on the first refactor. Sequence and state
  diagrams are the parts of UML that are genuinely universal, so those we use.
- **Not Mermaid's native `C4Context` / `C4Container` syntax**, despite following
  the C4 model. It's still experimental and has no real layout engine —
  relationship labels land on top of the boxes and everything stacks into one
  column. Plain `flowchart` renders the same model correctly. Revisit if
  Mermaid's C4 support gets a layout engine.

| # | Diagram | Answers |
|---|---|---|
| 0 | System context (C4 L1) | Who uses it, what it touches, who pays |
| 1 | Containers (C4 L2) | What runs where inside the boundary |
| 2a | Components (C4 L3) | The bot's internals; the plugin's are in `talooner-plugin/diagrams.md` §2 |
| 3 | `@talooner /review` flow | The v1 happy path, end to end |
| 4 | Re-evaluation on push | Reactive rules, and retraction |
| 5 | `llm_review` | Why there's no cache layer, and where determinism comes from |
| 6 | Fork PRs | Which ruleset governs writes |
| 7 | PR lifecycle | When the bot is and isn't watching |
| 8 | Credentials | Blast radius per secret |
| 9 | Fact sources | Where every fact comes from |

All diagrams verified rendering with `mermaid-cli` 11.16.

---

## 0. System context — C4 L1

Zoomed all the way out: who uses Talooner, what it talks to, and where the money
goes. One box for the whole system.

```mermaid
flowchart TB
    DEV(["<b>Maintainer</b><br/>invokes reviews<br/>reads the verdict"])
    CONTRIB(["<b>Contributor</b><br/>opens PRs, incl. from forks<br/><i>cannot invoke Talooner</i>"])

    GH["<b>GitHub</b><br/>repos · PRs · checks · reviews<br/><i>also runs the action</i>"]

    subgraph SELF["Your VPS"]
        TAL["<b>Talooner cluster</b><br/>rules · facts · decisions<br/><i>rules decide; a model only answers questions</i>"]
    end

    LLM["<b>LLM provider</b><br/>consulted only when a rule<br/>fires llm_review · your account"]
    CI["<b>Your CI</b><br/>preview builds · scans<br/>reports results back as facts"]

    DEV -->|"@talooner /review"| GH
    CONTRIB -->|"opens PRs"| GH
    GH -->|"Actions run:<br/>facts out, actions in"| TAL
    TAL -->|"reviews · comments<br/>check runs<br/><i>via the runner</i>"| GH
    TAL -->|"llm_review only"| LLM
    CI -->|"POST facts"| TAL

    classDef person fill:#f3e8ff,stroke:#7a3fb5,color:#2a0f45
    classDef ext fill:#f0f0f0,stroke:#7a7a7a,color:#2a2a2a
    classDef sys fill:#e8f0fe,stroke:#3b6bb5,stroke-width:2px,color:#102040
    class DEV,CONTRIB person
    class GH,LLM,CI ext
    class TAL sys
    style SELF fill:#fbfdff,stroke:#3b6bb5,stroke-width:2px,stroke-dasharray:7 5
```

Three things this level exists to make unmissable:

- **Contributors cannot invoke Talooner.** Only someone with write access can.
  That single arrow removes most of the fork threat model.
- **Every external arrow costs the VPS owner money, and nobody else.** There is
  no shared service in this picture, by design and permanently.
- **Talooner's GitHub half runs on GitHub.** It's an Action in the tenant's own
  repo, not a service listening for webhooks. The VPS holds rules, facts and
  credentials; it never calls GitHub.

---

## 1. Containers — C4 L2

Two boundaries now: what GitHub runs for you, and what you run. You pay for the
VPS; GitHub runs the ephemeral half.

```mermaid
flowchart TB
    subgraph GH["GitHub — SaaS"]
        direction TB
        EV["Events<br/>issue_comment · pull_request<br/>check_suite"]
        RUN["<b>Actions runner</b><br/>opentalon/talooner@v1<br/><i>ephemeral, one job per event</i>"]
        GAPI["REST API"]
        PR["Pull request"]
        EV -->|"triggers"| RUN
        GAPI --- PR
    end

    CI["<b>Your CI</b><br/>preview builds · scans<br/>screenshots"]

    subgraph VPS["Your VPS — you own and pay for all of this"]
        subgraph CLUSTER["OpenTalon cluster"]
            direction LR
            PLUGIN["<b>talooner-plugin</b><br/>Talon engine · rulesets<br/>llm_review · explain"]
            DB[("<b>talon-db</b><br/>facts · decisions<br/>subscriptions")]
            PLUGIN <--> DB
        end
    end

    LLM["<b>LLM provider</b><br/>your account, your budget"]

    RUN <-->|"gRPC + TLS<br/>OPENTALON_API_KEY"| PLUGIN
    RUN -->|"GITHUB_TOKEN<br/>reviews · comments<br/>check runs"| GAPI
    CI -->|"POST /api/v1/facts"| PLUGIN
    PLUGIN -->|"tenant credentials<br/>live only here"| LLM

    classDef ext fill:#f4f4f4,stroke:#8a8a8a,stroke-dasharray:5 4,color:#333
    classDef own fill:#e8f0fe,stroke:#3b6bb5,color:#102040
    classDef store fill:#ede7f6,stroke:#6a4fa3,color:#221040
    class EV,GAPI,PR,LLM,CI ext
    class RUN,PLUGIN own
    class DB store
    style VPS fill:#fbfdff,stroke:#3b6bb5,stroke-width:2px,stroke-dasharray:7 5
    style CLUSTER fill:#f2f8f0,stroke:#4a8f3c
    style GH fill:#fafafa,stroke:#8a8a8a,stroke-dasharray:5 4
```

Three things to read off this diagram:

- **The runner never touches an LLM.** Provider credentials live in the cluster
  and nowhere else. Compromise a run, you get one repo's GitHub access for a few
  minutes; compromise the cluster, you get LLM spend. Never both.
- **Arrows into the cluster, never out.** The cluster holds no GitHub credential
  and initiates nothing. That's why an externally POSTed fact can't produce a
  comment on its own (decision 20).
- **Preview builds and scans are your CI's job**, and they POST to the cluster
  directly — there is no bot endpoint to POST to. Talooner has no dispatch verb;
  your workflow does the work, rules react to the fact.

---

## 2. Components — C4 L3

### 2a. `talooner` the action — knows GitHub, knows nothing about Talon

```mermaid
flowchart TB
    IN(["workflow event<br/>GITHUB_EVENT_PATH"]) --> EVT
    EVT["Event parser<br/>→ repo · pr · trigger"] --> CMD
    CMD["Command parser<br/>/review [--force] /stop /why /plan<br/>+ write-access gate"] --> SUB
    SUB{"subscribed?<br/><i>ask the plugin</i>"} -->|"no, and not /review"| SKIP(["exit 0 — skipped job"])
    SUB -->|"yes"| FACTS
    FACTS["Fact extractor<br/>diff · checks · CODEOWNERS<br/><i>GITHUB_TOKEN</i>"] --> OUT

    OUT(["action evaluate_pr →<br/>talooner-plugin"])
    BACK(["← actions[] + explain"]) --> EXEC
    EXEC["Action executor<br/><i>interface</i>"] --> EXEC1["GitHub executor<br/>real writes"]
    EXEC --> EXEC2["Printer executor<br/>rules plan / dry run"]
    EXEC1 --> GH(["GitHub API"])

    classDef bot fill:#e8f0fe,stroke:#3b6bb5,color:#102040
    classDef edge fill:#f4f4f4,stroke:#8a8a8a,color:#333
    class EVT,CMD,SUB,FACTS,EXEC,EXEC1,EXEC2 bot
    class IN,OUT,BACK,GH,SKIP edge
```

Gone from this picture, compared to a webhook service: the HMAC verifier, the
202-in-10-seconds path, the per-PR queue, and the App auth chain. Concurrency is
the workflow's `concurrency:` block; auth is a token the runner is handed.

### 2b. `talooner-plugin` — knows Talon, knows nothing about GitHub

Lives in the other repo:
[`talooner-plugin/diagrams.md`](https://github.com/opentalon/talooner-plugin/blob/main/diagrams.md)
§2. Ruleset loader → engine → `llm_review` → defeasible resolution → `explain`,
all over `talon-db`.

The seam: the plugin returns an **abstract action list**, the bot translates it
into API calls. Consequences worth stating to the team —

- `talooner-plugin` is testable with zero GitHub fixtures.
- The action holds no engine state, which is what lets it be a process that
  exits.
- `rules plan` is not a separate code path. It's the same evaluation with the
  printer executor swapped in. Build the interface in phase 1 and dry-run is
  nearly free in phase 2.

---

## 3. Flow — `@talooner /review`

The v1 entry point. Nothing happens until a human asks.

```mermaid
sequenceDiagram
    autonumber
    actor Dev as Maintainer
    participant GH as GitHub
    participant Bot as talooner (runner)
    participant Plug as talooner-plugin
    participant DB as talon-db

    Dev->>GH: comment "@talooner /review"
    GH->>GH: workflow `if:` — body starts with @talooner?
    Note over GH: cheap filter — no runner starts<br/>for ordinary comments
    GH->>Bot: start job, mint GITHUB_TOKEN
    Note over Bot: fresh container, ~10–30s cold start.<br/>Secrets present: issue_comment runs<br/>in base repo context

    Bot->>GH: does commenter have write access?
    alt no write access
        Bot-->>Dev: exit 0, no writes — prevents budget burn<br/>by drive-by accounts
    else has write access
        Bot->>GH: fetch PR, files, checks, CODEOWNERS
        Bot->>GH: fetch .github/talooner/ from BASE branch
        Note over Bot,GH: base branch, never head —<br/>see diagram 6

        Bot->>Plug: action evaluate_pr — facts, ruleset, head_sha
        Plug->>DB: assert facts, mark PR subscribed
        Plug->>Plug: run engine + defeasible resolution
        Plug->>DB: persist decision + explain
        Plug-->>Bot: actions[] + explain

        Bot->>GH: check run "talooner" (success/failure/neutral)
        Bot->>GH: sticky comment (marker-keyed, edited in place)
        Bot->>GH: review APPROVE / REQUEST_CHANGES
        Bot-->>GH: exit 0 — container destroyed
    end
```

The write-access check runs **inside** the job, after a runner has already
started, so an unauthorised comment still costs a few seconds of Actions time.
That's the price of not having a gatekeeper process; the `if:` filter keeps it to
comments that at least mention the bot.

---

## 4. Flow — subscribed re-evaluation, and retraction

Once invoked, the PR is watched. This is where reactive rules
(`when "pr.files_changed" changes`) earn their keep.

```mermaid
sequenceDiagram
    autonumber
    actor Dev as Developer
    participant GH as GitHub
    participant Bot as talooner (runner)
    participant Plug as talooner-plugin
    participant DB as talon-db

    Dev->>GH: git push (new head sha)
    GH->>Bot: start job — pull_request synchronize
    Note over GH,Bot: concurrency group talooner-<pr><br/>queues behind any in-flight run
    Bot->>Plug: action is_subscribed — repo, pr

    alt not subscribed
        Plug-->>Bot: no
        Bot-->>Bot: exit 0 — never reviewed unasked
    else subscribed
        Plug-->>Bot: yes
        Bot->>GH: re-extract ALL facts at new sha
        Note over Bot: full re-derivation, never deltas —<br/>this is what makes retraction work

        Bot->>Plug: action evaluate_pr — facts, ruleset, new_sha
        Plug->>DB: re-assert facts, retract stale ones
        Plug-->>Bot: actions[] + explain

        alt PR grew past 500 lines — approval no longer holds
            Bot->>GH: dismiss previous approving review
            Bot->>GH: check run → failure
            Bot->>GH: edit sticky comment with new reason
        else still clean
            Bot->>GH: check run stays green, comment updated
        end
    end
```

**Reversibility is not uniform** — worth calling out to the team:

| Action | Reversible | On retraction |
|---|---|---|
| `approve` | yes | dismiss review, check → neutral |
| `block` | yes | check → success, dismiss REQUEST_CHANGES |
| `comment` | partly | edit to resolved state, never delete |
| `assign` / `require` | yes | remove assignee / withdraw request |
| `notify` | **no** | a sent Slack message stays sent |

---

## 5. Flow — `llm_review`, and why there is no cache layer

**Unimplemented, and describes the superseded PR-level design.**
[`expert-review-system.md`](expert-review-system.md) replaces this with a
per-`code_unit` tool call (`tool "llm" "review"`) and a cache key that adds a
`path` component. No diagram for the new flow exists yet — don't redraw this
one as if it were current; add a new one when that architecture lands instead.

```mermaid
sequenceDiagram
    autonumber
    participant ENG as Talon engine
    participant DB as talon-db
    participant LLMR as llm_review
    participant Core as OpenTalon core
    participant API as LLM provider

    ENG->>LLMR: rule fired: llm_review(doc_url, diff)
    LLMR->>DB: fact at key<br/>(pr, head_sha, doc_url, prompt_version)?

    alt fact exists — same sha, already answered
        DB-->>LLMR: cached result
        LLMR-->>ENG: llm_review.result (no API call, no spend)
    else fact absent — new sha or new prompt version
        LLMR->>Core: review request (tenant credentials)
        Core->>API: completion
        API-->>Core: response
        Core-->>LLMR: constrained output
        Note over LLMR: enum only:<br/>match | mismatch | unclear |<br/>too_large | error
        LLMR->>DB: store as fact, pinned to head_sha
        LLMR-->>ENG: llm_review.result
    end

    ENG->>ENG: result is now an ordinary fact —<br/>rules decide, the model does not
```

The fact store **is** the cache. No separate layer, no invalidation logic: a new
commit produces a new sha, the fact is absent, the model runs again.

That gives the headline property: **same head sha + same base ruleset ⇒ same
actions, byte for byte.** A per-PR conversation is retained cluster-side for
continuity, but it never changes an answer already recorded.

---

## 6. Flow — fork PRs and the base-branch rule

The ruleset that governs writes always comes from the base branch. A PR that
edits the ruleset gets a read-only plan run instead.

```mermaid
flowchart TB
    START["PR opened, maintainer runs<br/>@talooner /review"] --> FORK{"Does this PR modify<br/>.github/talooner/ ?"}

    FORK -->|"no"| NORM["Load ruleset from BASE branch"]
    NORM --> EVAL["Evaluate"]
    EVAL --> WRITE["GitHub executor<br/>real reviews, comments, check runs"]

    FORK -->|"yes"| BOTH["Load BOTH rulesets"]
    BOTH --> B1["BASE ruleset → evaluate → <b>writes</b>"]
    BOTH --> B2["HEAD ruleset → evaluate → <b>printer executor</b>"]
    B1 --> DIFF["Diff the two decision sets"]
    B2 --> DIFF
    DIFF --> PLAN["One comment:<br/>'this rule change would flip<br/>X from approve to block'"]

    classDef danger fill:#fdeaea,stroke:#c0392b,color:#611
    classDef safe fill:#eef7ee,stroke:#4a8f3c,color:#161
    class B2,PLAN safe
    class WRITE safe
```

Why this matters even though Talooner cannot merge:

- **Spend.** A fork PR adding a hundred `llm_review` rules would burn the
  maintainer's LLM budget.
- **Noise.** It could make the bot post attacker-authored text as a first-party
  review comment on the maintainer's repo.

Explicit invocation already blunts both — a maintainer must ask before anything
runs — but base-branch-only removes them, and the plan run means rule changes
stay reviewable.

---

## 7. PR lifecycle

```mermaid
stateDiagram-v2
    [*] --> Unwatched: PR opened

    Unwatched --> Subscribed: "@talooner /review" — write access required
    Unwatched --> Unwatched: push or check completes — job exits 0

    Subscribed --> Subscribed: "@talooner /review" — full re-evaluation
    Subscribed --> Subscribed: "@talooner /review --force" — also busts llm cache
    Subscribed --> Subscribed: push — full re-evaluation
    Subscribed --> Subscribed: check_suite completed — re-evaluate
    Subscribed --> Subscribed: review submitted — review.* facts

    Subscribed --> Unwatched: "@talooner /stop"
    Subscribed --> Closed: PR closed or merged

    Closed --> [*]: facts expire after retention

    note right of Subscribed
        CI POSTing a fact does NOT appear here.
        Nothing is running between events, so it
        lands in talon-db and waits for the next
        evaluation. Decision 20.
    end note

    note right of Unwatched
        v1 default. Auto-review on PR open
        is opt-in, a later phase.
        Decisions and explain outlive the facts.
    end note
```

Subscription is state, and there is no process to hold it — so it lives in
`talon-db` with everything else. Each run asks the plugin rather than
remembering.

---

## 8. Credentials and blast radius

```mermaid
flowchart LR
    subgraph BOTP["Actions runner — lives for one job"]
        K1["GITHUB_TOKEN<br/><i>minted per run</i>"]
        K2["OPENTALON_API_KEY<br/><i>from repo/org secret</i>"]
    end

    subgraph CLP["OpenTalon cluster"]
        K3["LLM provider credentials"]
    end

    K1 -->|"can, per workflow<br/>permissions: block"| CAN["✅ comment · review<br/>check run · assign"]
    K1 -.->|"cannot — not granted<br/>to the job"| CANT["❌ merge · push · edit CI<br/>change settings"]
    K1 -.->|"expires"| EXP["job ends → token dead"]

    K2 -->|"can"| RPC["gRPC to cluster<br/>evaluate_pr · whoami"]
    K3 -->|"can"| SPEND["LLM spend<br/>your account, your budget"]

    classDef no fill:#fdeaea,stroke:#c0392b,color:#611
    classDef yes fill:#eef7ee,stroke:#4a8f3c,color:#161
    class CANT no
    class CAN,SPEND,EXP yes
```

| Credential | Held by | If leaked |
|---|---|---|
| `GITHUB_TOKEN` | runner, one job | Comment/review on that one repo until the job ends. **Cannot merge or push** — not granted. Nothing to rotate. |
| Cluster API key | runner, from a secret | Burn that tenant's LLM budget. Rotate cluster-side. |
| LLM provider key | cluster only | Full provider account. Never transits a runner, `talon-db`, or a log line. |

Job permissions: `pull-requests: write`, `checks: write`, `contents: read`.
Everything else — `contents: write`, `actions`, `administration` — is simply not
granted to the job. Declared in the tenant's workflow file, so widening them is a
reviewable diff rather than an install dialog.

There is no long-lived GitHub credential in this design. The App private key,
which was the worst thing on this diagram, no longer exists.

---

## 9. Where facts come from

```mermaid
flowchart LR
    GHAPI["GitHub API"] -->|"diff stats, title, body,<br/>labels, check runs"| PRF["<b>pr.*</b><br/>built-in, always asserted"]
    CO[".github/CODEOWNERS"] --> USR["<b>user.*</b><br/>who owns this code"]
    MOD["modules.yaml"] --> USR
    MOD --> MODF["<b>module.*</b><br/>docs URL, owner"]
    TEAMS["teams.yaml"] --> TF["<b>team.*</b>"]
    RULES["rules.tln"] -->|"define blocks over<br/>pr.changed_files"| TOUCH["<b>pr.touches_*</b><br/>Talon-native path predicates"]
    PRF --> TOUCH
    REV["pull_request_review<br/>events"] --> REVF["<b>review.*</b>"]
    ENGINE["Talon engine"] --> LLMF["<b>llm_review.*</b><br/>pinned to head_sha"]
    YOURCI["Your CI<br/>POST to the cluster<br/>/api/v1/facts"] --> CUSTOM["<b>preview.* screenshots.*<br/>dependency_scan.*</b><br/><i>read at next evaluation</i>"]

    PRF --> STORE[("talon-db<br/>per-PR fact scope")]
    USR --> STORE
    MODF --> STORE
    TF --> STORE
    TOUCH --> STORE
    REVF --> STORE
    LLMF --> STORE
    CUSTOM --> STORE

    classDef repo fill:#fff8e6,stroke:#b58900,color:#432
    class CO,MOD,TEAMS,RULES repo
```

Everything yellow is **committed to the repo being reviewed**, under
`.github/talooner/`. The review policy is versioned, diffable, and unit-testable
with `.tln.test` — which is the claim no LLM-based reviewer can make.

Custom fact names are namespaced away from `pr.*` and `review.*`. Without that, a
workflow could POST `pr.tests_passing: true` and defeat the entire ruleset.

### One trap the team must know about

A condition on an **unset** fact evaluates to *false*, not unknown — so
`not <unset>` is **true**, and a PR whose fact extraction failed sails through
`not is "critical_path"` and gets auto-approved.

Phase 0 verified this against `talon-language`'s evaluator and v1 accepts it. The
asymmetry to hold onto: positive conditions on an unset fact fail closed (the
rule doesn't fire), negated ones fail open. See `facts.md`, "Unset is false".
