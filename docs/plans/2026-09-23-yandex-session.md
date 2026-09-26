# Yandex Session Cookie Support Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Let the `vyandex` transport use a user-supplied Yandex browser session from a scoped cookie file.

**Architecture:** Parse Netscape cookies into the existing Go `http.CookieJar` before `authorize` makes its first request. Expose one optional CLI path, valid only for `vyandex`. Keep all cookie values out of flags, logs, tracked files, and errors.

**Tech Stack:** Go 1.26, `net/http/cookiejar`, Go tests, Docker build.

---

### Task 1: Parse and scope cookies

**Files:**
- Create: `transport/yandex/cookies.go`
- Create: `transport/yandex/cookies_test.go`

1. Write tests with representative Netscape rows for `.yandex.ru` and unrelated domains. Assert that only valid, unexpired Yandex cookies appear in a jar request to `https://docs.yandex.ru/`, and unrelated hosts receive none.
2. Run `go test ./transport/yandex -run TestLoadYandexCookies -v` and verify the expected compile/test failure.
3. Implement `loadYandexCookies(path string, jar http.CookieJar) error`. Handle `#HttpOnly_` rows, malformed rows, expiry, secure flag, and empty files. Errors must omit cookie values.
4. Run the focused tests again and inspect the result.

### Task 2: Use cookies during authorization

**Files:**
- Modify: `transport/yandex/vyandex.go`
- Modify: `transport/yandex/cookies_test.go`

1. Add a test proving the transport rejects an invalid cookie file before any document request, and a valid file is loaded into its authorization jar.
2. Observe the test fail.
3. Add `cookieFile` to `YandexVolgaTransport`, pass it into `authorize`, and load the file after creating the jar but before the first request. Preserve behavior when it is empty.
4. Run the focused tests.

### Task 3: Expose the CLI flag

**Files:**
- Modify: `main.go`
- Modify: `README.ru.md`

1. Add `--yandex-cookies-file`, reject it for transports other than `vyandex`, and pass it to the transport constructor. Describe Netscape export and local file handling in the README.
2. Run `go test ./...` and `go build ./...` in the repository's Go container. Verify the built CLI help shows the flag.

### Task 4: Prepare local artifacts and attempt the live connection

**Files:**
- Modify: `output/compose.client.yml` and `output/Start-OpenFlux.cmd` (untracked deployment artifacts)
- Modify: `/opt/openflux/compose.yml` on the VPS

1. Build Linux and Windows artifacts from the final source and stage them without enabling an automatic restart loop.
2. Once the user provides a cookie export through a file on disk, protect it with OS file permissions, run each side once, and verify an HTTP request through SOCKS5 returns the VPS exit IP.
3. If Yandex still serves CAPTCHA, keep the services stopped and report the exact result.
