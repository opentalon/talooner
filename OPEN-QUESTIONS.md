# Talooner — open questions

Resolved decisions live in `README.md`. What remains.

---

## A. Blocking phase 0 — verify against `talon-language`, don't assume

Answerable by reading code, not by discussion. Each one silently produces wrong
reviews if assumed.

**A1. Three-valued evaluation.** Does a condition on an unset fact evaluate to
unknown (rule doesn't fire), and is `not <unknown>` unknown rather than true?
Two-valued logic means a PR whose fact extraction failed gets auto-approved by
`not is "critical_path"`. Hard blocker. See `facts.md`, "Unset is not false".

**A2. List operands.** Do `contains` / `matches` (`grammar.ebnf:515`) quantify
existentially over a list like `pr.changed_files`, or are they string-only? Every
`pr.touches_*` predicate depends on it. If string-only: fix it generally in
`internal/executor`, or have Talooner assert a joined string as a fallback?

**A3. Facts as action arguments.** Can an action take a fact reference rather
than a literal — `do assign "pr" "user.owner"`? The whole `user.*` namespace is
pointless if not.

**A4. Interpolation position.** Is `{ident.field}` (`grammar.ebnf:601`) available
in action arguments, or only in labels?

**A5. Cross-ruleset defeasible.** Does `overrides` / priority resolution work
across two rulesets loaded together (Talooner's `strict` base + the tenant's)?

**A6. External wake.** Can an out-of-band fact assertion wake the reactive engine
mid-PR? This is the *only* path for preview / screenshot / dependency-scan rules
now that dispatch is off the table — so it moved from nice-to-have to required.

**A7. `talon-db` at this shape.** Thousands of small, short-lived, concurrent
fact scopes — one per open PR — plus subscription state. Fits, or needs work?

---

## B. Still needing your call

**B1. Licensing.** Recommendation and reasoning in the section below. Needs a
yes/no from you.

**B2. Where does subscription state live?** It's cluster-side (the bot is
stateless), but is it a `talon-db` fact like everything else, or plugin-local
metadata outside the fact store? Fact is more consistent and makes
`when "pr.subscribed" == true` expressible; metadata is cleaner separation.
Leaning fact.

**B3. Does `@talooner /review` re-evaluate, or only subscribe?** If a PR is
already subscribed and someone comments `/review` again — full re-evaluation
including a fresh `llm_review` at the same sha (costs money, bypasses the fact
cache), or a no-op that re-renders the existing verdict? Leaning re-render, with
`/review --force` for the expensive path.

**B4. Bot identity on GitHub.** The App's display name is what appears on every
review. `talooner[bot]` follows convention. Confirm, and confirm the org that
owns the App listing if you ever publish it to the Marketplace (listing is free
and doesn't imply a hosted service).

**B5. Retention defaults.** 30 days for facts, 1 year for decisions are
placeholders. Since the tenant runs their own storage, these could just as well
be "keep forever, you own the disk". Leaning: configurable, default 90d facts /
forever for decisions.

---

## C. Deferred to the phase that needs them

- Org-level shared rulesets and non-overridable org policy (phase 4)
- Auto-review on PR open, opt-in per repo (phase 4)
- Community ruleset distribution and versioning (phase 4)
- `k8s-operator` support for `talooner-plugin` in the CRD (phase 4)
- Merge rights — explicitly out of scope for v1, revisit only with a concrete
  reason and a permissions re-consent plan

---

## Licensing — recommendation

**Apache-2.0 on both `talooner` and `talooner-plugin`.**

Current state of the workspace: `opentalon`, `talon-language`, `talon-db`, and
`opentalon-agents` are Apache-2.0. `opentalon-workflows` has no LICENSE file at
all, which makes it proprietary by default — that's the existing "paid plugin"
pattern.

Why Apache and not that pattern here:

1. **There's nothing to sell.** The usual open-core monetisation is a hosted
   tier. You've ruled that out permanently. What's left is selling a self-hosted
   binary licence — which means a licence server, entitlement checks, piracy you
   can't police, and support obligations, for a product with no users yet. The
   revenue would not cover the machinery.

2. **The stated goal is adoption, not revenue.** "Battle-test OpenTalon" and
   "give something to the open-source world" both want the widest possible
   installed base. Every licence restriction subtracts from that, and you're
   already asking users to provision a VPS — that's a steep enough ask without
   also asking them to pay or to accept a non-OSI licence.

3. **Fencing the plugin is the worst version of open-core here.** The plugin is
   where the OpenTalon dogfooding happens. Closing it means the interesting
   half — the part that proves `talon-language` works on a real workload — is
   the part nobody can read, learn from, or contribute to. That defeats the
   entire reason for building Talooner.

4. **Apache-2.0 specifically, not MIT**, because of the patent grant and the
   trademark clause. Both matter for a tool corporations install into their
   review pipeline, and it matches the rest of the workspace so there's no
   licence-compatibility question when the plugin links `talon-language`.

If you want a monetisation path later, the ones that don't require changing this
licence: paid support/SLA, a hosted **OpenTalon cluster** (not Talooner — the
cluster is the thing that's genuinely annoying to operate and the thing people
would actually pay to avoid), or paid `talooner-*` ecosystem components that are
genuinely optional. The bot and the plugin should be free either way; they're the
demo.

The one licence worth considering instead is **BSL 1.1** — source-available,
free to self-host, converts to Apache after four years, forbids offering it as a
competing hosted service. It costs you nothing today and blocks someone else
from building the SaaS you declined to build. The cost is that "source-available"
reads as not-open-source to a lot of people, and the corporate legal review that
Apache passes silently becomes a conversation. Given that adoption is the goal
and there's no revenue to protect, that trade isn't worth it.

**Recommendation: Apache-2.0, both repos. If you want the SaaS clause, BSL 1.1
on `talooner-plugin` only — but I'd skip it.**
