# Web research

Gofer provides opt-in `web_search` and `web_fetch` tools for lightweight,
source-oriented research without opening a stateful browser session. Search
results use one provider-independent JSON shape, and fetched pages are reduced
to bounded readable text before entering model context. Both results are
classified as untrusted remote content and pass through prompt-injection
guardrails.

## Search providers

`web.search.provider` supports:

- `brave`: uses the Brave Web Search API. Configure `api_key`; the official
  endpoint is used unless `endpoint` is explicitly set.
- `searxng`: uses a self-hosted SearXNG JSON search endpoint. Configure
  `endpoint` with the instance root or its `/search` URL.

The tool accepts a query of at most 400 characters and an optional result
limit no larger than the configured `max_results`. Safe search is normalized
as `off`, `moderate`, or `strict`. Results with non-HTTP URLs or embedded
credentials are discarded, fragments are removed, and duplicate citation
URLs are collapsed.

## Fetch boundary

`web_fetch` accepts one absolute HTTP(S) URL without credentials. It follows a
bounded number of redirects and accepts only HTML, XHTML, plain text, and JSON.
Response bytes are capped before parsing; extracted content receives a second
character budget. HTML scripts, styles, forms, navigation, and footer content
are omitted from the readable result.

Every initial URL, redirect, DNS answer, and final transport connection is
checked by the shared network guard. The HTTP transport dials an approved IP
directly, which prevents a second DNS resolution from changing the destination
after validation. Loopback, private, link-local, multicast, carrier-grade NAT,
documentation, benchmark, unspecified, and reserved ranges are blocked unless
the operator explicitly enables `allow_private_addresses`. Environment proxy
settings are not used by these tools, so they cannot bypass the guarded dialer.

Application checks complement rather than replace deployment-level egress
controls. Private address access should be enabled only for a trusted internal
search or content service.

## Defaults

Search and fetch are disabled by default. The example configuration documents
timeouts, redirect limits, response budgets, user agent, and separate private
address opt-ins. When enabled, `/api/features` reports `web_search` and
`web_fetch` independently.
