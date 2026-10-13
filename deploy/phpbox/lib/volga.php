<?php
// Volga (Yandex Docs / vyandex) for the PHP exit: the authorization dance against yandex.ru and the data codec.
// It mirrors transport/yandex/vyandex.go so a client on that transport and this exit meet in the same document.
//
// A document edit carries our bytes: packets are framed [uint16 len][bytes], concatenated, base64-encoded, and placed
// as the third item of a relay "bundle"; the WebSocket on push.yandex.ru delivers the peer's bundles back.
//
// Authorization is HTTP against yandex.ru, and there a human may be needed: a login wall (passport.yandex) or a
// SmartCaptcha (showcaptcha, not showcaptchafast). The node cannot pass those headless; it records the page with
// PhpboxAuth::need so the user solves it in a browser (the app's WebView, or the page's iframe) and the cookies come
// back to a=cookies. A plain PoW captcha (showcaptchafast) the node can and does solve itself.
//
// This is the authorization + codec only; volgaexit.php wires it to lib/mux.php (the stream mux) and lib/ws.php.

require_once __DIR__ . '/util.php';
require_once __DIR__ . '/volga_pow.php';

final class VolgaAuthError extends Exception
{
    // "captcha" (SmartCaptcha, needs a human), "login" (passport), "blocked" (the host's IP is refused), or "other".
    public string $kind;
    public string $solveURL = '';
    public function __construct(string $kind, string $message, string $solveURL = '')
    {
        parent::__construct($message);
        $this->kind = $kind;
        $this->solveURL = $solveURL;
    }
}

final class VolgaAuth
{
    const UA = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:153.0) Gecko/20100101 Firefox/153.0';

    public string $token = '';
    public string $requestPath = '';
    public string $sessionID = '';
    public int $userID = 0;
    public string $userIDStr = '';
    public string $sign = '';
    public string $ts = '';
    public string $cookieFile = '';

    // ---- pure parsers (offline-testable) -----------------------------------

    /** The JSON of the <script id="client-config"> block, or null. */
    public static function clientConfig(string $html): ?array
    {
        if (!preg_match('/<script[^>]*id="client-config"[^>]*>(.*?)<\/script>/s', $html, $m)) {
            return null;
        }
        $cfg = json_decode(trim($m[1]), true);
        return is_array($cfg) ? $cfg : null;
    }

    /** token, request-path and the parsed `json` (sessionId, userId, xiva{sign,ts,user}) out of the auth/initial Location. */
    public static function parseLocation(string $location): array
    {
        $q = [];
        parse_str((string)parse_url($location, PHP_URL_QUERY), $q);
        $out = ['token' => (string)($q['token'] ?? ''), 'request_path' => (string)($q['request-path'] ?? '')];
        $j = isset($q['json']) ? json_decode((string)$q['json'], true) : null;
        if (is_array($j)) {
            $out['session_id'] = (string)($j['sessionId'] ?? '');
            $out['user_id'] = (int)($j['userId'] ?? 0);
            $xiva = is_array($j['xiva'] ?? null) ? $j['xiva'] : [];
            $out['sign'] = (string)($xiva['sign'] ?? '');
            $out['ts'] = (string)($xiva['ts'] ?? '');
            $out['user'] = (string)($xiva['user'] ?? '');
        }
        return $out;
    }

    /** The access-token TTL as the form field wants it (a bare integer, else the string). */
    public static function ttl($v): string
    {
        if (is_int($v) || is_float($v)) { return (string)(int)$v; }
        if (is_string($v) && $v !== '') { return $v; }
        return '0';
    }

    // ---- data codec (offline-testable, shared with volgaexit) --------------

    /** Frame packets as [uint16 len][bytes]..., concatenate, base64 — the blob a relay bundle carries. */
    public static function packetsToBlob(array $packets): string
    {
        $blob = '';
        foreach ($packets as $p) {
            $blob .= pack('n', strlen($p)) . $p;
        }
        return base64_encode($blob);
    }

    /** The reverse: whole packets out of a base64 blob; a trailing partial (never expected) is dropped. */
    public static function blobToPackets(string $b64): array
    {
        $blob = base64_decode($b64, true);
        if ($blob === false) { return []; }
        $out = [];
        $off = 0;
        $n = strlen($blob);
        while ($n - $off >= 2) {
            $len = unpack('n', substr($blob, $off, 2))[1];
            if ($n - $off < 2 + $len) { break; }
            $out[] = substr($blob, $off + 2, $len);
            $off += 2 + $len;
        }
        return $out;
    }

    // ---- authorization (live: needs the network and, past a login/SmartCaptcha, the user's cookies) ----

    /**
     * Runs the transport's authorization against $docURL using $cookieFile (empty until the user has solved a login).
     * Returns a VolgaAuth on success. Throws VolgaAuthError("login"/"captcha", ..., solveURL) when a human is needed,
     * or ("blocked"/"other", ...) otherwise. $log may be null.
     */
    public static function authorize(string $docURL, string $cookieFile, ?PhpboxLog $log = null): VolgaAuth
    {
        $say = function (string $m) use ($log) { if ($log) { $log->write('debug', '[volga] ' . $m); } };
        $hdr = [
            'User-Agent: ' . self::UA,
            'Accept-Language: ru-RU,ru;q=0.9',
            'Accept: text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8',
        ];

        [$finalBody, $finalURL] = self::follow($docURL, $cookieFile, $hdr, $say);

        $cfg = self::clientConfig($finalBody);
        if ($cfg === null) {
            throw new VolgaAuthError('other', 'client-config not found at ' . $finalURL);
        }
        $office = is_array($cfg['officeActionData'] ?? null) ? $cfg['officeActionData'] : null;
        $editor = is_array($cfg['editorParams'] ?? null) ? $cfg['editorParams'] : [];
        if ($office === null) {
            throw new VolgaAuthError('other', 'officeActionData missing');
        }
        $actionURL = (string)($office['action_url'] ?? '');
        $accessToken = (string)($office['access_token'] ?? '');
        if ($actionURL === '' || $accessToken === '') {
            throw new VolgaAuthError('other', 'action_url or access_token missing');
        }

        // POST the access token; the answer is a 302 whose Location carries token, request-path and the xiva json.
        $form = http_build_query(['access_token' => $accessToken, 'access_token_ttl' => self::ttl($office['access_token_ttl'] ?? 0)]);
        [$code, $location] = PhpboxUtil::httpHead($actionURL, $cookieFile, 'POST', $form, array_merge($hdr, [
            'Content-Type: application/x-www-form-urlencoded',
            'Origin: https://disk.yandex.ru',
            'Referer: ' . $finalURL,
            'Upgrade-Insecure-Requests: 1',
            'Sec-Fetch-Dest: iframe',
            'Sec-Fetch-Mode: navigate',
            'Sec-Fetch-Site: cross-site',
        ]));
        if ($code !== 302 || $location === '') {
            throw new VolgaAuthError('other', "auth/initial status $code (expected 302 with a Location)");
        }
        if (strpos($location, '/document/error/') !== false) {
            throw new VolgaAuthError('other', 'auth/initial returned /document/error/');
        }

        $loc = self::parseLocation($location);
        if (($loc['token'] ?? '') === '' || ($loc['request_path'] ?? '') === '') {
            throw new VolgaAuthError('other', 'no token/request-path in the auth Location');
        }
        // Complete the handshake (sets the session cookies the relay and WS need).
        PhpboxUtil::httpHead($location, $cookieFile, 'GET', null, array_merge($hdr, ['Referer: ' . $actionURL]));

        $a = new VolgaAuth();
        $a->cookieFile = $cookieFile;
        $a->token = (string)$loc['token'];
        $a->requestPath = (string)$loc['request_path'];
        $a->sessionID = (string)($loc['session_id'] ?? '');
        $a->userID = (int)($loc['user_id'] ?? 0);
        $a->userIDStr = (string)($loc['user'] ?? '');
        $a->sign = (string)($loc['sign'] ?? '');
        $a->ts = (string)($loc['ts'] ?? '');
        if ($a->token === '' || $a->requestPath === '' || $a->userIDStr === '' || $a->sign === '') {
            throw new VolgaAuthError('other', 'incomplete authorization (token/rp/user/sign)');
        }
        $say("auth OK: user={$a->userIDStr} rp={$a->requestPath}");
        return $a;
    }

    /** GET docURL and follow redirects, handling the captcha/login forks. Returns [finalBody, finalURL]. */
    private static function follow(string $docURL, string $cookieFile, array $hdr, callable $say): array
    {
        $current = $docURL;
        for ($i = 0; $i < 15; $i++) {
            $h = $i > 0 ? array_merge($hdr, ['Referer: ' . $docURL]) : $hdr;
            [$body, $code, $location] = PhpboxUtil::httpFull($current, $cookieFile, 'GET', null, $h);
            $say("GET $current -> $code");
            if ($code === 200) {
                return [$body, $current];
            }
            if ($code >= 300 && $code < 400 && $location !== '') {
                // SmartCaptcha: needs a human.
                if (strpos($location, 'showcaptcha') !== false && strpos($location, 'showcaptchafast') === false) {
                    throw new VolgaAuthError('captcha', 'Yandex SmartCaptcha', $location);
                }
                if (strpos($location, 'passport.yandex') !== false) {
                    throw new VolgaAuthError('login', 'Yandex login required', $location);
                }
                if (strpos($location, 'showcaptchafast') !== false) {
                    // PoW captcha: the node solves it itself (see lib/volga_pow.php), then retries from the document.
                    if (!VolgaPoW::solve($location, $cookieFile, self::UA, $say)) {
                        throw new VolgaAuthError('captcha', 'could not solve the PoW captcha', $location);
                    }
                    $current = $docURL;
                    continue;
                }
                if ($location[0] === '/') {
                    $u = parse_url($current);
                    $location = $u['scheme'] . '://' . $u['host'] . $location;
                }
                $current = $location;
                continue;
            }
            throw new VolgaAuthError('other', "unexpected status $code at $current");
        }
        throw new VolgaAuthError('other', 'too many redirects');
    }
}
