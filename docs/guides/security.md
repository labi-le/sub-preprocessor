# Security and correctness invariants

> **When to read this:** Read before touching `internal/fetch`, anything that handles a user-supplied URL, or the SSRF gates.

## Important security / correctness notes

- `subscription_url` is user input and must stay protected against SSRF.
- Fetching uses a safe HTTP client:
  - only `https` URLs are allowed
  - userinfo in URL is rejected
  - private/local targets are rejected
  - the guarded dialer tries EVERY public resolved address before giving up, not
    only the first: one dead answer (a v6 address with no v6 egress listed ahead
    of the live v4 one) must not burn the request, so each public address is
    dialed in order and the last dial error is returned only when all of them
    failed — `non-public target is not allowed` means the answer held NO public
    address at all (`dialPublicAddrs`, `internal/fetch/fetch.go:571-586`)
  - a host whose EVERY dot-separated part is a number `inet_aton` accepts —
    decimal, `0x` hex or `0`-prefixed octal — is rejected rather than passing the
    IP gate as a hostname. **That rule is the contract; the spellings below are
    examples of it, not the set.** `netip.ParseAddr` answers for none of them, so
    each would otherwise read as a domain name, and getaddrinfo resolves it to
    whatever address it spells: measured under `CGO_ENABLED=1`, `2130706433`,
    `0x7f000001`, `127.1` and `0177.0.0.1` all reach `127.0.0.1`, and
    `0300.0250.0.1` reaches `192.168.0.1` — so the reach is not only loopback.
    The crawler's client — the one fetching channel-supplied URLs — has no
    dial-time guard behind this gate. A host is refused only when the WHOLE of it
    is such a number, so `12345.example.com` and `cafe.beef` still pass: this is
    not "contains digits"
  - env proxy usage is disabled (`Transport.Proxy = nil`) to avoid SSRF bypass via proxy
- Do not reintroduce implicit proxy support unless SSRF validation is redesigned.
- A DNS negative cache records a verdict about DNS, not about the caller:
  `internal/resolver` stores a failed lookup only when the caller's own context
  is still live (`ctx.Err() == nil`), so a lookup aborted by caller cancellation
  or a caller deadline poisons nothing for the negative TTL — the resolver tests
  the PARENT context because its own derived timeout expires `resolveCtx` too,
  and that expiry is a genuine DNS failure that stays cacheable
  (`internal/resolver/resolver.go:108-114`).
- An oversized subscription body is a 4xx, not an upstream fault: `GET /` maps
  the fetch layer's `response too large: over …` refusal onto the same 413 as
  preprocess's 50k-node ceiling, so oversize stays distinguishable from a 502
  (`isResponseTooLarge`, `internal/server/server.go:293-297` and `:341-345`).
- Request context is passed explicitly through the stack. Prefer `ctx context.Context` as the first argument.
- Root `main.go` is the only normal place where `context.Background()` should be introduced.
