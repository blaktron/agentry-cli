# Security policy

The CLI and the runner are part of the Agentry open core; one policy covers
all of it. **Do not open a public issue for a security vulnerability.**

Report it as described in the core's
[SECURITY.md](https://github.com/blaktron/agentry/blob/main/SECURITY.md):
email **security@secureagentry.net** (or use GitHub's private vulnerability
reporting on this repository) with what you found, how to reproduce it and
what an attacker gains. The same response targets apply (a human reads it
within 2 business days; an assessment within 5; a fix or mitigation for
high/critical issues within 30 days; credit with the advisory if you want it).

Particularly interesting here:

- **Verification bypass** — an install or run that proceeds on an archive
  whose digest or DSSE attestation does not verify, or that accepts a
  signing key the registry did not publish (`pkg/trust` pins keys on first
  use).
- **Runner containment** — a QuickAgent reaching the network, the filesystem,
  a subprocess or a secret its manifest did not declare; a gateway started
  without its network block; a receipt that misstates what was enforced.
- **Archive extraction** — path traversal, links or bombs in `pkg/archive`
  (shared with the server).
- **Credential handling** — a token or session id written to a log, a
  receipt, an export or a profile it should not be in.

Malicious content on a registry is reported to that registry, not here. A
signed-in account posts the report to
`POST /api/v1/agents/{ns}/{name}/abuse-reports`; without an account, email the
security address that instance's operator publishes.
