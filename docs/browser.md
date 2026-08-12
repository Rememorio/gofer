# Browser automation

Gofer owns one stateful Chrome session per active thread. Browser tools share
the page, cookies, and navigation history until the thread closes its session
or the bounded session manager evicts an idle session. A lease pins a session
while an action is running, so eviction cannot race an in-flight tool call.

## Tools

The browser integration exposes navigation, snapshot, click, type, scroll,
back, screenshot, and close operations. Snapshots assign short-lived numeric
references to visible interactive elements. A ref is accepted only until the
next snapshot, which avoids exposing arbitrary selectors or JavaScript to a
model. Page text is explicitly marked as untrusted data before it enters model
context.

Screenshots are created atomically under the thread `outputs/` directory and
registered with the artifact catalog. Filenames are normalized and always get
a random suffix, preventing accidental replacement of an existing output.

## Network boundary

Top-level navigation accepts only absolute HTTP or HTTPS URLs without embedded
credentials. Unless `allow_private_addresses` is enabled, the URL guard rejects
localhost, private, loopback, link-local, multicast, carrier-grade NAT,
documentation, benchmark, unspecified, and reserved address ranges. DNS errors
are fail-closed.

Chrome Fetch interception applies the same policy to redirects and
subresources. Guard work is bounded; excess paused requests are rejected. This
substantially reduces browser-driven SSRF exposure, but deployment-level
egress controls remain the final network boundary because DNS answers and
routing can change after application validation.

## Configuration

Browser support is disabled in the baseline configuration. It can launch a
local Chrome/Chromium executable or attach to an operator-controlled remote
CDP endpoint. Session count, idle timeout, action timeout, viewport, private
address access, and headful mode are explicit configuration values; see
`config.example.yaml`.

Unit tests use a deterministic runner. An optional integration test launches a
real locally installed Chrome:

```sh
GOFER_BROWSER_INTEGRATION=1 go test -race ./internal/browser -run '^TestChromeIntegration$' -count=1
```
