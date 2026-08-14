# Security and correctness invariants

> **When to read this:** Read before touching `internal/fetch`, anything that handles a user-supplied URL, or the SSRF gates.

## Important security / correctness notes

- `subscription_url` is user input and must stay protected against SSRF.
- Fetching uses a safe HTTP client:
  - only `https` URLs are allowed
  - userinfo in URL is rejected
  - private/local targets are rejected
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
- Request context is passed explicitly through the stack. Prefer `ctx context.Context` as the first argument.
- Root `main.go` is the only normal place where `context.Background()` should be introduced.
