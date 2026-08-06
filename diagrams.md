# Talooner — design diagrams

Shareable overview for the dev team. Mermaid, so it renders on GitHub and stays
diffable in review.

Companion docs: `architecture.md` (prose), `facts.md`, `actions.md`, `auth.md`,
`roadmap.md`.

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

    GH["<b>GitHub</b><br/>repos · PRs · checks · reviews<br/><i>source of events, target of actions</i>"]

    subgraph SELF["Your VPS"]
        TAL["<b>Talooner</b><br/>deterministic PR reviewer<br/><i>rules decide; a model only answers questions</i>"]
    end

    LLM["<b>LLM provider</b><br/>consulted only when a rule<br/>fires llm_review · your account"]
    CI["<b>Your CI</b><br/>preview builds · scans<br/>reports results back as facts"]

    DEV -->|"@talooner /review"| GH
    CONTRIB -->|"opens PRs"| GH
    GH -->|"webhooks"| TAL
    TAL -->|"reviews · comments<br/>check runs"| GH
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

Two things this level exists to make unmissable:

- **Contributors cannot invoke Talooner.** Only someone with write access can.
  That single arrow removes most of the fork threat model.
- **Every external arrow costs the VPS owner money, and nobody else.** There is
  no shared service in this picture, by design and permanently.

---

## 1. Containers — C4 L2

Everything inside the dashed boundary runs on **your** VPS. There is no hosted
Talooner, no shared service, no third party.

```mermaid
flowchart TB
    subgraph GH["GitHub — SaaS"]
        direction LR
        APP["App installation<br/>webhooks out"]
        GAPI["REST / GraphQL API"]
        PR["Pull request"]
        GAPI --- PR
    end

    CI["<b>Your CI</b><br/>preview builds · scans<br/>screenshots"]

    subgraph VPS["Your VPS — you own and pay for all of this"]
        direction TB
        BOT["<b>talooner</b> — the bot<br/>GitHub App service + CLI<br/><i>stateless</i>"]

        subgraph CLUSTER["OpenTalon cluster"]
            direction LR
            PLUGIN["<b>talooner-plugin</b><br/>Talon engine · rulesets<br/>llm_review · explain"]
            DB[("<b>talon-db</b><br/>facts · decisions<br/>subscriptions")]
            PLUGIN <--> DB
        end

        BOT <-->|"gRPC"| PLUGIN
    end

    LLM["<b>LLM provider</b><br/>your account, your budget"]

    APP -->|"issue_comment<br/>pull_request<br/>check_suite"| BOT
    CI -->|"POST /api/v1/facts"| BOT
    BOT -->|"installation token<br/>reviews · comments<br/>check runs"| GAPI
    PLUGIN -->|"tenant credentials<br/>live only here"| LLM

    classDef ext fill:#f4f4f4,stroke:#8a8a8a,stroke-dasharray:5 4,color:#333
    classDef own fill:#e8f0fe,stroke:#3b6bb5,color:#102040
    classDef store fill:#ede7f6,stroke:#6a4fa3,color:#221040
    class APP,GAPI,PR,LLM,CI ext
    class BOT,PLUGIN own
    class DB store
    style VPS fill:#fbfdff,stroke:#3b6bb5,stroke-width:2px,stroke-dasharray:7 5
    style CLUSTER fill:#f2f8f0,stroke:#4a8f3c
    style GH fill:#fafafa,stroke:#8a8a8a,stroke-dasharray:5 4
```

Two things to read off this diagram:

- **The bot never touches an LLM.** Provider credentials live in the cluster and
  nowhere else. Compromise the bot, you get GitHub comment access; compromise the
  cluster, you get LLM spend. Never both.
- **Preview builds and scans are your CI's job.** Talooner has no dispatch verb
  for them. Your workflow does the work and POSTs the result as a fact; rules
  react to that fact.

---

## 2. Components — C4 L3

### 2a. `talooner` — knows GitHub, knows nothing about Talon

```mermaid
flowchart TB
    IN(["webhook from GitHub"]) --> WH
    WH["Webhook receiver<br/>HMAC verify · 202 fast"] --> CMD
    CMD["Command parser<br/>/review /stop /why /plan<br/>+ write-access gate"] --> Q
    Q["Queue<br/>serialized per PR"] --> AUTH
    AUTH["App auth<br/>JWT → installation token"] --> FACTS
    FACTS["Fact extractor<br/>diff · checks · CODEOWNERS"] --> OUT

    OUT(["action evaluate_pr →<br/>talooner-plugin"])
    BACK(["← actions[] + explain"]) --> EXEC
    EXEC["Action executor<br/><i>interface</i>"] --> EXEC1["GitHub executor<br/>real writes"]
    EXEC --> EXEC2["Printer executor<br/>rules plan / dry run"]
    EXEC1 --> GH(["GitHub API"])

    classDef bot fill:#e8f0fe,stroke:#3b6bb5,color:#102040
    classDef edge fill:#f4f4f4,stroke:#8a8a8a,color:#333
    class WH,CMD,Q,AUTH,FACTS,EXEC,EXEC1,EXEC2 bot
    class IN,OUT,BACK,GH edge
```

### 2b. `talooner-plugin` — knows Talon, knows nothing about GitHub

Lives in the other repo:
[`talooner-plugin/diagrams.md`](https://github.com/opentalon/talooner-plugin/blob/main/diagrams.md)
§2. Ruleset loader → engine → `llm_review` → defeasible resolution → `explain`,
all over `talon-db`.

The seam: the plugin returns an **abstract action list**, the bot translates it
into API calls. Consequences worth stating to the team —

- `talooner-plugin` is testable with zero GitHub fixtures.
- The bot holds no engine state, so it restarts freely.
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
    participant Bot as talooner
    participant Plug as talooner-plugin
    participant DB as talon-db

    Dev->>GH: comment "@talooner /review"
    GH->>Bot: webhook issue_comment
    Bot->>Bot: verify X-Hub-Signature-256
    Bot-->>GH: 202 Accepted
    Note over Bot: must answer in under 10s —<br/>all real work is async

    Bot->>GH: does commenter have write access?
    alt no write access
        Bot-->>Dev: ignored — prevents budget burn<br/>by drive-by accounts
    else has write access
        Bot->>GH: mint installation token, 1h TTL
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
    end
```

---

## 4. Flow — subscribed re-evaluation, and retraction

Once invoked, the PR is watched. This is where reactive rules
(`when "pr.files_changed" changes`) earn their keep.

```mermaid
sequenceDiagram
    autonumber
    actor Dev as Developer
    participant GH as GitHub
    participant Bot as talooner
    participant Plug as talooner-plugin
    participant DB as talon-db

    Dev->>GH: git push (new head sha)
    GH->>Bot: webhook pull_request synchronize
    Bot->>Plug: action is_subscribed — repo, pr

    alt not subscribed
        Plug-->>Bot: no
        Bot-->>Bot: drop — never reviewed unasked
    else subscribed
        Plug-->>Bot: yes
        Bot->>Bot: cancel any in-flight run —<br/>its head sha is stale
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
    Unwatched --> Unwatched: push or check completes — ignored

    Subscribed --> Subscribed: push — full re-evaluation
    Subscribed --> Subscribed: check_suite completed — re-evaluate
    Subscribed --> Subscribed: review submitted — review.* facts
    Subscribed --> Subscribed: CI POSTs custom fact — engine wakes

    Subscribed --> Unwatched: "@talooner /stop"
    Subscribed --> Closed: PR closed or merged

    Closed --> [*]: facts expire after retention

    note right of Unwatched
        v1 default. Auto-review on PR open
        is opt-in, a later phase.
        Decisions and explain outlive the facts.
    end note
```

Subscription is state, and the bot is stateless — so it lives in `talon-db` with
everything else. A bot restart must not silently stop watching a PR someone asked
it to watch.

---

## 8. Credentials and blast radius

```mermaid
flowchart LR
    subgraph BOTP["talooner process"]
        K1["GitHub App private key"]
        K2["Cluster API key"]
    end

    subgraph CLP["OpenTalon cluster"]
        K3["LLM provider credentials"]
    end

    K1 -->|"mints"| IT["Installation token<br/>1h TTL, one install"]
    IT -->|"can"| CAN["✅ comment · review<br/>check run · assign"]
    IT -.->|"cannot — permission<br/>never requested"| CANT["❌ merge · push · edit CI<br/>change settings"]

    K2 -->|"can"| RPC["gRPC to cluster<br/>evaluate_pr · whoami"]
    K3 -->|"can"| SPEND["LLM spend<br/>your account, your budget"]

    classDef no fill:#fdeaea,stroke:#c0392b,color:#611
    classDef yes fill:#eef7ee,stroke:#4a8f3c,color:#161
    class CANT no
    class CAN,SPEND yes
```

| Credential | Held by | If leaked |
|---|---|---|
| GitHub App private key | bot | Comment/review as the bot on installed repos. **Cannot merge or push** — the permission isn't requested. Rotate; tokens die within 1h. |
| Cluster API key | bot | Burn that tenant's LLM budget. Rotate cluster-side. |
| LLM provider key | cluster only | Full provider account. Never transits the bot, a webhook, `talon-db`, or a log line. |

App permissions requested: `pull_requests: write`, `checks: write`,
`contents: read`, `metadata: read`, `members: read`. Explicitly **not**
requested: `contents: write`, `administration`, `workflows`.

---

## 9. Where facts come from

```mermaid
flowchart LR
    GHAPI["GitHub API"] -->|"diff stats, title, body,<br/>labels, check runs"| PRF["<b>pr.*</b><br/>built-in, always asserted"]
    CO[".github/CODEOWNERS"] --> USR["<b>user.*</b><br/>who owns this code"]
    MOD["modules.yaml"] --> USR
    MOD --> MODF["<b>module.*</b><br/>docs URL, owner"]
    TEAMS["teams.yaml"] --> TF["<b>team.*</b>"]
    RULES["rules.talon"] -->|"define blocks over<br/>pr.changed_files"| TOUCH["<b>pr.touches_*</b><br/>Talon-native path predicates"]
    PRF --> TOUCH
    REV["pull_request_review<br/>events"] --> REVF["<b>review.*</b>"]
    ENGINE["Talon engine"] --> LLMF["<b>llm_review.*</b><br/>pinned to head_sha"]
    YOURCI["Your CI<br/>POST /api/v1/facts"] --> CUSTOM["<b>preview.* screenshots.*<br/>dependency_scan.*</b>"]

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
with `.talon.test` — which is the claim no LLM-based reviewer can make.

Custom fact names are namespaced away from `pr.*` and `review.*`. Without that, a
workflow could POST `pr.tests_passing: true` and defeat the entire ruleset.

### One trap the team must know about

A condition on an **unset** fact evaluates to *unknown*, not false — and
`not <unknown>` is unknown, not true. Otherwise a PR whose fact extraction failed
sails through `not is "critical_path"` and gets auto-approved.

This is a property of `talon-language`'s evaluator, not of Talooner, and it is
the first thing phase 0 verifies. See `facts.md`, "Unset is not false".
