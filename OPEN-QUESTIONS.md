# Talooner — open questions

Resolved decisions live in `README.md`. What remains, and what needs your call.

---

## A. Phase 0 — substrate verification, closed

Verified against `talon-language` / `talon-db` at 2026-08-06, with runnable
probes. The three substrate fixes it filed landed 2026-08-07 —
[`talon-language#158`](https://github.com/opentalon/talon-language/issues/158)
(list operands), [#159](https://github.com/opentalon/talon-language/issues/159)
(import shadowing), and
[`opentalon#325`](https://github.com/opentalon/opentalon/issues/325) (payload
ceiling) — so **nothing in phase 0 blocks anything any more**. The full findings
and the decisions they force live in
[`talooner-plugin/OPEN-QUESTIONS.md`](https://github.com/opentalon/talooner-plugin/blob/main/OPEN-QUESTIONS.md)
§A, since they land on the plugin rather than the bot.

**The one with a bot-visible consequence landed on the bad side.** The evaluator
is two-valued with closed-world negation-as-failure: an unset fact makes its
pattern fail, which makes the enclosing `not` succeed. Probed directly — a PR
with `critical_path` unset matched `not is "critical_path"` and was allowed. A PR
whose extraction partially failed gets auto-approved.

**Decision: accepted for v1.** Missing facts approve rather than block. The
residual risk is knowingly taken — a PR whose extraction died is approved with a
review comment reporting no problems.

That still settles what "leave the fact unset" means for every extractor in
`internal/facts`: it means *false*, not *unknown*, so unset is never a way to say
"couldn't determine". Extractors assert their facts explicitly, negative cases
included.

`facts.md` has been rewritten accordingly — the section is now "Unset is false,
and that asymmetry is load-bearing", and it is required reading before writing
any rule that grants something.

One bot-visible consequence of the #158 fix, in `facts.md` too: list predicates
quantify, but `matches` is a substring scan rather than a glob, so path
predicates are written with `contains` / `starts_with` / `ends_with`.

---

## B. Still needing your call

**B4. Team membership without an App.** `review.<team>.approved` needs org team
membership, which a repo-scoped `GITHUB_TOKEN` cannot read (decision 1). Two ways
out: derive team approval from CODEOWNERS review requests — no extra permission,
covers the common case, slightly wrong for teams not in CODEOWNERS — or accept an
optional PAT secret for orgs that need real resolution. The default must work
with no extra secret. Not blocking until `review.*` lands in phase 2. See
`actions.md`, "Workflow permissions".

**B5. Cluster exposure default.** Public gRPC + TLS + API key is the documented
default; a self-hosted runner is the answer for tenants who won't expose the
cluster. Fine as a documented choice, but if the honest recommendation for
private repos turns out to be "run your own runner", that's a heavier onboarding
story than the README currently admits. Revisit after the first real deployment.
See `auth.md`, "Cluster auth".
