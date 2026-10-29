# AGENTS.md

## Layout

Several Go modules in one repository: the root (`classad`, `ast`, `parser`), plus
`collections/`, `db/` and `dbrpc/`, each with its own `go.mod`. Build and test
from inside the module you changed, with `GOWORK=off`. A change spanning modules
needs each one's `go.mod` bumped and pinned in dependency order — `classad`,
then `collections`, then `db`, then `dbrpc` — and the pin lands after the thing
it points at is tagged, never in the same PR.

## Wire compatibility

`dbrpc` is a wire protocol between separately released programs. Two versions of
it are in production at any time, and the two rules below are not negotiable
because the failures they prevent are silent ones.

### Old clients must always work against new servers

A released opcode's request framing never changes meaning, and its reply never
loses a field a client might read. Add capability as a NEW opcode, or as a field
appended where an old reader stops before reaching it. Never reinterpret an
existing one.

This rule has no escape hatch, because the client is the half you do not control:
an access point runs the client for as long as its operators leave it installed,
and nothing warns them.

### New clients should work against old servers, or fail clearly

Prefer, in this order:

1. **Ask what the peer supports.** `Client.Supports(Feature)` answers from the
   version and feature list the server advertised in the CEDAR handshake
   (`ServerSessionAttrs` on the server, `ClientSessionAttrNames` plus
   `WithPeerSessionAttrs` on the client). A `SupportYes` means dispatch
   directly — no fallback, so none to leave untested.

2. **Emulate, when emulation is honest and affordable.** Judge it per call site,
   not per feature: the same emulation is right for a 50-row page and wrong for
   a 500,000-row backfill. Say in a comment what it costs and why it was
   acceptable here.

3. **Refuse, in a message naming the missing capability and the peer.** A clear
   refusal beats a silent degradation that returns subtly wrong numbers. Error
   text an operator will read should say which side is too old.

`SupportUnknown` is not `SupportNo`. Every feature predates advertisement, so a
silent peer may well implement all of them; treating silence as refusal breaks
exactly the old servers this rule protects. For a `SupportUnknown` peer the
legacy `Err*Unsupported` probe remains correct — that is what it is for.

When adding an opcode: add its `Feature`, list it in `serverFeatures`, and bump
`ProtocolVersion`. Forget the first two and every client keeps probing for
something the server has.

### Advertisement is sticky

A client caches what a server advertised with the CEDAR session, and sessions
outlive a daemon restart. After a server upgrade a client may keep reading the
pre-upgrade answer until its session expires. This is safe only because it errs
toward older behaviour, so never advertise a capability it is unsafe to
under-claim.

## Projections return at least their closure

A server may return MORE attributes than the projection asked for, and callers
must tolerate it. What a server must not do is return LESS than the transitive
closure of the projected attributes: an attribute holding an expression over its
siblings (`RequestMemory` written over `MemoryUsage`, `MemoryUsage` over
`ResidentSetSize`) evaluates to undefined once those siblings are projected away,
and the caller gets a plausible wrong number rather than an error.

So: do not add a projected read path that computes no closure, and do not
"optimize" one by dropping it. Only the store can compute it — whether an
attribute holds an expression is a property of the stored ad, which a client
cannot know for a query it has not run.

A caller needing exactly the requested attributes trims them itself, on the
grounds that it is the one that knows why.

## Tests that prove something

A capability test against a corpus of literal-valued attributes passes whether
or not the closure works. Seed expression-valued ads deliberately
(`seedWireExprAds` is the pattern) and assert the attribute EVALUATES, not that
it is present.

A compatibility test needs a peer that actually lacks the capability. A test
where both ends are this build exercises neither the fallback nor the refusal.
Fake the advertisement to get the branch you mean to test.

After writing a test, break the line it covers and confirm that test — by name —
fails. A fallback path that no test reaches is the normal state of this code,
and it is the state this file exists to prevent.
