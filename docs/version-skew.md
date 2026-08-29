# Version skew

There is no telemetry and no hosted tier — self-hosted forever
(`auth.md`, "Prerequisite"). Nobody's dashboard tells you when the other
side of this contract moves, so compatibility is an explicit versioned
contract instead, enforced at the `whoami` handshake.

## The two independent knobs

| Side | What it is | Who sets it | Where |
|---|---|---|---|
| Action version | The `talooner` build a workflow invokes | Whoever edits that repo's workflow file, not necessarily whoever operates the cluster | `uses: opentalon/talooner@<ref>` in `.github/workflows/talooner.yml` |
| Plugin version | The `talooner-plugin` build the cluster runs | The cluster operator | `ref:` in the cluster's `config.yaml` `plugins.talooner` block (`deployment-and-setup.md`, Part 1) |

Neither knows the other exists. A tenant with 30 repos has 30
independently pinned action versions talking to one cluster's plugin
version — upgrading the plugin doesn't touch any of them, and bumping one
repo's action pin doesn't touch the other 29 or the cluster.

## What actually gets checked: `protocol_version`, not build version

The action version (`v0.0.1-alpha2`, a commit sha, eventually `vX.Y.Z`)
and the plugin version are release artifacts. Neither is compared
directly — there's no ordering between "action `v0.0.1-alpha2`" and
"plugin `abc1234`" for either side to check. What's actually compared is
a small integer, `protocol_version`, carried in the generated
`taloonerpb` package both repos import
(`talooner-plugin/proto/taloonerpb/version.go`):

```go
ProtocolVersion uint32 = 1  // contract version this build implements/reports
ProtocolFloor   uint32 = 1  // lowest caller version this build will serve
```

Every `whoami` call declares the caller's `protocol_version`
(`internal/cluster/cluster.go:274`); every `whoami` response declares the
callee's (`internal/service/whoami.go:44`). Both directions of skew are
checked, one on each side, because each side can only see the other's
declared version, not its floor:

- **Plugin older than the action needs** — the plugin can't know the
  action's floor, so the action checks it: `internal/cluster/whoami.go:69`
  compares the cluster's reported `protocol_version` against this build's
  `ProtocolFloor` and fails with `ErrProtocolSkew` if the cluster is
  behind.
- **Action older than the plugin's floor** — the action can't know the
  plugin's floor, so the plugin checks it: `internal/service/whoami.go:36`
  compares the caller's declared `protocol_version` against `s.floor` and
  refuses with an error naming the floor to upgrade past.

Either way the run fails fast at the handshake, before touching a PR —
one clear message at the top of a run instead of a `notify` verb refusing
mid-evaluation or a rule silently never firing.

## Bumping the contract: plugin first, tag, then bump the action

This order applies to every future contract change, not just this one:

1. Land the change in `talooner-plugin`, bump `ProtocolVersion` (and
   `ProtocolFloor` only if it's breaking — see the comment on those
   constants), tag a release.
2. Only then bump `talooner`'s `taloonerpb` dependency and, if the action
   needs the new capability, its own `ProtocolVersion`/`ProtocolFloor`.

Landing it the other way round — action first — ships a caller that
declares a `protocol_version` no deployed plugin can serve yet, and every
tenant still on the old plugin starts failing handshakes for a feature
they never asked for.

## Current state (2026-08-29)

No plain `vX.Y.Z` tag exists for either repo yet:

- `talooner`: latest is `v0.0.1-alpha2`. Every consumer's action pin is a
  prerelease tag or commit sha, bumped by hand (`deployment-and-setup.md`,
  "The action pin").
- `talooner-plugin`: latest tag is `v0.1.0`; `talooner`'s `go.mod` pins a
  `master` pseudo-version past that
  (`v0.1.1-0.20260825121205-e5a52b5955a3`) because no real tag covers the
  commit it needs yet.
- `ProtocolVersion` / `ProtocolFloor` are both `1` on both sides — no skew
  has happened in practice yet, only the machinery for detecting it.
