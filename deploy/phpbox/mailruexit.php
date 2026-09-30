<?php
/**
 * mailruexit.php - a phpbox exit that reaches the client over Mail.ru's cloud
 * document editor (docs.datacloudmail.ru), the sibling of cupsexit.php.
 *
 *   client (--mode=stream, --transport=mailru, --url <public doc link>)
 *         ->  Mail.ru doc  ->  mailruexit.php  ->  dst
 *
 * It opens the same public document, reads the client's mux frames from the
 * base64 in the "cursor" field of the coauthoring (Socket.IO) protocol, dials
 * dst, and writes replies back the same way. Matches transport/mailru: one WS
 * message carries exactly one packet (one mux frame), base64-encoded, with no
 * extra length framing. Shared mux/ws/util live in lib/.
 *
 * Run (browser, ~140s per request):
 *   https://<host>/mailruexit.php?k=PHPBOX_TOKEN&url=<public doc link or weblink>
 * Offline self-test: php mailruexit.php selftest
 */

error_reporting(E_ALL & ~E_DEPRECATED);
require_once __DIR__ . '/lib/util.php';
require_once __DIR__ . '/lib/ws.php';
require_once __DIR__ . '/lib/mux.php';
require_once __DIR__ . '/lib/node.php';

const RUN_CAP = 140;

// ===========================================================================
final class MailruCarrier implements Carrier
{
    const API_EDIT = 'https://cloud.mail.ru/api/v4/r7/edit';
    const ORIGIN   = 'https://docs.datacloudmail.ru';
    private Mux $mux;
    private WsClient $ws;
    private array $info = [];
    private string $cookieFile;
    private string $userID;
    private string $weblink;

    public function __construct(string $link)
    {
        $this->weblink = self::normalizeWeblink($link);
        $this->cookieFile = tempnam(sys_get_temp_dir(), 'mrx');
        $this->ws = new WsClient();
        $this->userID = sprintf('%010d', random_int(0, 999999999));
    }

    public function setMux(Mux $m): void { $this->mux = $m; }
    public function sockets(): array { return $this->ws->sock ? [$this->ws->sock] : []; }

    public function connect(): bool
    {
        $info = $this->fetchDocInfo();
        if (!$info) { return false; }
        $this->info = $info;
        echo "opened doc {$info['docKey']}\n";

        if (!$this->ws->connect($info['wsURL'], self::ORIGIN, PhpboxUtil::cookieHeader($this->cookieFile))) {
            echo "ws handshake failed\n"; return false;
        }
        // Auth, fired immediately (as transport/mailru; the server buffers it).
        $this->ws->writeText('40' . json_encode(['token' => $info['token']]));
        $auth = [
            'type' => 'auth', 'docid' => $info['docKey'], 'documentCallbackUrl' => $info['callbackURL'],
            'token' => 'fghhfgsjdgfjs',
            'user' => ['id' => $info['editorUserID'], 'username' => $this->userID, 'indexUser' => -1],
            'editorType' => 0, 'lastOtherSaveTime' => -1, 'block' => [], 'documentFormatSave' => 65,
            'view' => false, 'isCloseCoAuthoring' => false,
            'openCmd' => ['c' => 'open', 'id' => $info['docKey'], 'userid' => $info['editorUserID'],
                'format' => $info['fileType'], 'url' => $info['docURL'], 'title' => $info['docTitle'],
                'lcid' => 25, 'nobase64' => true, 'convertToOrigin' => '.pdf.xps.oxps.djvu'],
            'lang' => 'ru', 'mode' => 'edit', 'permissions' => (object)$info['permissions'],
            'IsAnonymousUser' => false, 'timezoneOffset' => -180, 'coEditingMode' => 'fast',
            'jwtOpen' => $info['token'], 'time' => 1000, 'supportAuthChangesAck' => true,
        ];
        $this->ws->writeText('42' . json_encode(['message', $auth]));
        return true;
    }

    public function onReadable($sock): void
    {
        $msg = $this->ws->readFrame(0.2);
        if ($msg === null || $msg === '') { return; }
        if ($msg === '2') { $this->ws->writeText('3'); return; }   // Socket.IO ping
        if ($msg === '3') { return; }
        if (str_contains($msg, '---KA---')) { return; }
        if (!str_contains($msg, 'cursor')) { return; }
        if (!preg_match('/"cursor":"[^;]+;([^"]+)"/', $msg, $m)) { return; }
        $pkt = base64_decode($m[1], true);
        if ($pkt === false || $pkt === '') { return; }
        $this->mux->onPacket($pkt);                                // one message = one packet
    }

    public function sendPacket(string $frame): void
    {
        $b64 = base64_encode($frame);
        $this->ws->writeText('42' . json_encode(['message', ['type' => 'cursor', 'cursor' => "18;$b64"]]));
    }

    private function fetchDocInfo(): ?array
    {
        [$body, $code] = PhpboxUtil::http(self::API_EDIT, $this->cookieFile, 'POST',
            json_encode(['x-email' => 'anonym', 'public' => '/' . $this->weblink, 'platform' => 'desktop_web']),
            ['Content-Type: application/json', 'Accept: application/json, text/plain, */*',
             'X-Api-Version: 4', 'Referer: https://cloud.mail.ru/public/' . $this->weblink . '?weblink=' . $this->weblink]);
        if ($code !== 200) { echo "edit API status $code\n"; return null; }
        $res = json_decode($body, true);
        if (!is_array($res)) { echo "edit API bad JSON\n"; return null; }
        $doc = $res['document'] ?? null;
        $ec  = $res['editorConfig'] ?? null;
        if (!is_array($doc) || !is_array($ec)) { echo "edit API missing document/editorConfig\n"; return null; }
        $api = $res['api'] ?? '';
        $key = $doc['key'] ?? '';
        if ($api === '' || $key === '') { echo "edit API missing api/key\n"; return null; }
        return [
            'token'        => $res['token'] ?? '',
            'docKey'       => $key,
            'wsURL'        => preg_replace('#^https#', 'wss', $api) . "/doc/$key/c/?EIO=4&transport=websocket",
            'fileType'     => $doc['fileType'] ?? '',
            'docURL'       => $doc['url'] ?? '',
            'docTitle'     => $doc['title'] ?? '',
            'permissions'  => is_array($doc['permissions'] ?? null) ? $doc['permissions'] : [],
            'callbackURL'  => $ec['callbackUrl'] ?? '',
            'editorUserID' => $ec['user']['id'] ?? '',
        ];
    }

    private static function normalizeWeblink(string $w): string
    {
        $w = trim($w);
        foreach (['https://cloud.mail.ru/public/', 'http://cloud.mail.ru/public/',
                  'https://cloud.mail.ru/', 'http://cloud.mail.ru/'] as $prefix) {
            if (str_starts_with($w, $prefix)) { return trim(substr($w, strlen($prefix)), '/'); }
        }
        return $w;
    }

    /** Offline round-trip: mailru carries one mux frame per message, base64, no framing. */
    public static function selfTest(): void
    {
        $ok = true;
        foreach (['', 'A', 'hello', str_repeat('Z', 4000)] as $p) {
            $frame = pack('CNN', Mux::DATA, 7, strlen($p)) . $p;
            $wire  = base64_encode($frame);                 // as sent in the cursor field
            $pkt   = base64_decode($wire, true);            // as decoded on receive
            $h     = unpack('Ctype/Nsid/Nlen', substr($pkt, 0, 9));
            $got   = substr($pkt, 9, $h['len']);
            $pass  = ($h['type'] === Mux::DATA && $h['sid'] === 7 && $got === $p);
            $ok = $ok && $pass;
            printf("selftest len=%-5d %s\n", strlen($p), $pass ? 'OK' : 'FAIL');
        }
        echo $ok ? "SELFTEST PASS\n" : "SELFTEST FAIL\n";
        exit($ok ? 0 : 1);
    }
}

// ---- entry point (after the class so it is declared before use) -----------
if (PHP_SAPI === 'cli' && ($argv[1] ?? '') === 'selftest') { MailruCarrier::selfTest(); exit; }

(new PhpboxNode('mailru', 'Mail.ru', fn(array $g): string => (string)($g['url'] ?? ''),
    fn(string $link) => new MailruCarrier($link), RUN_CAP))->handle();
