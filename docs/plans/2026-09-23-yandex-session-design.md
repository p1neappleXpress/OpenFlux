# Yandex session support

## Goal

Allow the `vyandex` transport to use an existing browser session so the client and exit node can request a Yandex Document as the signed-in user. This is an experiment for the CAPTCHA observed from the current VPS, not a guarantee that Yandex will accept the connection.

## Approaches considered

- **Netscape `cookies.txt` file (chosen):** A conventional export format can represent cookie domain, path, expiry, and secure scope. The Go cookie jar can then send only cookies applicable to each Yandex host. The file is supplied separately on each machine and never placed in the repository.
- **Raw `Cookie` header:** Simpler to implement, but would attach one domain's cookies to unrelated Yandex hosts and ignore expiry and path scope.
- **Browser process on the VPS:** Could reuse browser state directly, but adds substantial memory and lifecycle cost on the 891 MiB VPS.

## Design

Add an optional `--yandex-cookies-file` flag, valid only with `--transport=vyandex`. On startup, parse a Netscape-format cookie export into a fresh `http.CookieJar` before the first document request. Accept only `.yandex.ru` and its subdomains, ignore expired cookies, and reject malformed or empty files with errors that omit cookie values. Preserve existing anonymous behavior when the flag is absent. Keep the jar within the transport, so redirects, document authorization, relay, and WebSocket setup continue through the existing code path.

The user will export their own Yandex session locally, place a copy on the VPS with restrictive file permissions, and point both peers at their respective files. No account password or cookie value belongs in the CLI arguments, logs, Git, or chat. The current AES-256-GCM channel key remains separate. We will test cookie scoping and error handling locally, then try a real connection. If the server still receives CAPTCHA, stop and report that authenticated cookies did not resolve it.
