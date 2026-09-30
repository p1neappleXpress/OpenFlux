<?php
if (is_file(__DIR__ . '/config.php')) { require_once __DIR__ . '/config.php'; }   // token of a deployed node
/**
 * cupsexit.php - a phpbox exit that reaches the client over cups.online.
 *
 *   client (--mode=stream, --transport=cupsonline, --url <room>)
 *         ->  cups.online room  ->  cupsexit.php  ->  dst
 *
 * It joins the same cups room, reads the client's mux frames hidden in cursor
 * integers, dials dst, and writes replies back as cursors. Framing matches
 * transport/cupsonline byte-for-byte. Shared mux/ws/util live in lib/.
 *
 * Run (open in a browser so the host's bot check passes; ~140s per request):
 *   https://<host>/cupsexit.php?k=PHPBOX_TOKEN&room=<uuid>   (or &url=<room URL>)
 * A browser gets the status page (starts the node unless one already runs);
 * a pinger/curl runs the node directly, as before. See lib/node.php.
 * Offline framing self-test: php cupsexit.php selftest
 */

error_reporting(E_ALL & ~E_DEPRECATED);
require_once __DIR__ . '/lib/util.php';
require_once __DIR__ . '/lib/ws.php';
require_once __DIR__ . '/lib/mux.php';
require_once __DIR__ . '/lib/node.php';

const RUN_CAP       = 140;
const SEND_INTERVAL = 0.018;   // s between cursor messages (cups throttles)
const MAX_MSG_DATA  = 3000;    // payload bytes per cursor message
const BYTES_PER_NUMBER = 6;

// ===========================================================================
final class CupsCarrier implements Carrier
{
    private Mux $mux;
    private WsClient $ws;
    private array $auth = [];
    private string $cookieFile;
    private string $recvBuf = '';
    private float $lastSend = 0.0;

    public function __construct(private string $roomURL)
    {
        $this->cookieFile = tempnam(sys_get_temp_dir(), 'cupsx');
        $this->ws = new WsClient();
    }

    public function setMux(Mux $m): void { $this->mux = $m; }
    public function sockets(): array { return $this->ws->sock ? [$this->ws->sock] : []; }

    public function connect(): bool
    {
        [$html, $code] = PhpboxUtil::http($this->roomURL, $this->cookieFile, 'GET', null,
            ['Accept: text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8', 'Accept-Language: ru-RU,ru;q=0.9']);
        if ($code !== 200) { echo "GET room status $code\n"; return false; }
        $a = [
            'roomUUID'  => PhpboxUtil::scrape('/data-room="\{&quot;uuid&quot;:\s*&quot;([0-9a-f-]{36})&quot;/', $html),
            'userUUID'  => PhpboxUtil::scrape('/data-user="\{&quot;uuid&quot;:\s*&quot;([0-9a-f-]{36})&quot;/', $html),
            'connToken' => PhpboxUtil::scrape('/<meta[^>]+name="centrifuge-connection-token"[^>]+content="([^"]+)"/', $html),
            'connURL'   => PhpboxUtil::scrape('/<meta[^>]+name="centrifuge-connection-url"[^>]+content="([^"]+)"/', $html),
            'subURL'    => PhpboxUtil::scrape('/<meta[^>]+name="centrifuge-subscription-token-url"[^>]+content="([^"]+)"/', $html),
        ];
        foreach ($a as $k => $v) { if ($v === '') { echo "missing $k\n"; return false; } }
        $a['csrf'] = PhpboxUtil::cookie($this->cookieFile, 'csrftoken');
        $a['channel'] = '$shared_editor:room-' . $a['roomUUID'];
        [$body, $c2] = PhpboxUtil::http($a['subURL'], $this->cookieFile, 'POST', json_encode(['channel' => $a['channel']]),
            ['Content-Type: application/json', 'X-CSRFToken: ' . $a['csrf'],
             'Origin: ' . PhpboxUtil::originOf($this->roomURL), 'Referer: ' . $this->roomURL]);
        if ($c2 !== 200) { echo "sub token status $c2\n"; return false; }
        $a['subToken'] = json_decode($body, true)['token'] ?? '';
        if ($a['subToken'] === '') { echo "empty sub token\n"; return false; }
        $this->auth = $a;
        echo "joined room {$a['roomUUID']} as {$a['userUUID']}\n";

        $wsURL = rtrim(preg_replace('#^http#', 'ws', $a['connURL']), '/') . '/websocket';
        if (!$this->ws->connect($wsURL, PhpboxUtil::originOf($a['connURL']), PhpboxUtil::cookieHeader($this->cookieFile))) {
            echo "ws handshake failed\n"; return false;
        }
        $this->ws->writeText(json_encode(['id' => 1, 'connect' => ['token' => $a['connToken'], 'name' => 'js']]));
        if (!$this->expect(1)) { echo "connect reply missing\n"; return false; }
        $this->ws->writeText(json_encode(['id' => 2, 'subscribe' => ['channel' => $a['channel'], 'token' => $a['subToken']]]));
        if (!$this->expect(2)) { echo "subscribe reply missing\n"; return false; }
        return true;
    }

    private function expect(int $id): bool
    {
        $deadline = microtime(true) + 12;
        while (microtime(true) < $deadline) {
            $f = $this->ws->readFrame(3.0);
            if ($f === null) { continue; }
            if ($f === '{}') { $this->ws->writeText('{}'); continue; }
            foreach (explode("\n", trim($f)) as $line) {
                $o = json_decode($line, true);
                if (is_array($o) && (($o['id'] ?? null) === $id)) { return true; }
            }
        }
        return false;
    }

    public function onReadable($sock): void
    {
        do {                                   // keep reading while PHP already holds more (select cannot see its buffer)
            $msg = $this->ws->readFrame(0.2);
            if ($msg !== null && $msg !== '') { $this->onMessage($msg); }
        } while ($this->ws->pending());
    }

    private function onMessage(string $msg): void
    {
        foreach (explode("\n", trim($msg)) as $line) {
            if ($line === '') { continue; }
            if ($line === '{}') { $this->ws->writeText('{}'); continue; }
            $obj = json_decode($line, true);
            if (!is_array($obj)) { continue; }
            $cursors = $this->peerCursors($obj);
            if ($cursors === null) { continue; }
            $this->recvBuf .= $this->cursorsToChunk($cursors);
            while (strlen($this->recvBuf) >= 2) {
                $ln = unpack('n', substr($this->recvBuf, 0, 2))[1];
                if ($ln === 0) { $this->recvBuf = ''; break; }
                if (strlen($this->recvBuf) < 2 + $ln) { break; }
                $pkt = substr($this->recvBuf, 2, $ln);
                $this->recvBuf = substr($this->recvBuf, 2 + $ln);
                $this->mux->onPacket($pkt);
            }
        }
    }

    public function sendPacket(string $frame): void
    {
        foreach ($this->frameToCursorMessages($frame) as $cur) {
            $this->pace();
            $this->ws->writeText(json_encode([
                'rpc' => ['method' => 'shared_editor_change_cursors',
                    'data' => ['cursors' => $cur, 'ranges' => [],
                        'room' => $this->auth['roomUUID'], 'user' => $this->auth['userUUID']]],
                'id' => random_int(100, 1 << 20),
            ]));
        }
    }

    /** frame -> packet framing (2-byte len) -> chunk framing per message -> cursors. */
    private function frameToCursorMessages(string $frame): array
    {
        $blob = pack('n', strlen($frame)) . $frame;
        $out = [];
        for ($off = 0; $off < strlen($blob); $off += MAX_MSG_DATA) {
            $chunk = substr($blob, $off, MAX_MSG_DATA);
            $out[] = self::cursorsFromBytes(pack('n', strlen($chunk)) . $chunk);
        }
        return $out;
    }

    private function cursorsToChunk(array $cursors): string
    {
        $decoded = self::bytesFromCursors($cursors);
        if (strlen($decoded) < 2) { return ''; }
        $chunkLen = unpack('n', substr($decoded, 0, 2))[1];
        if (2 + $chunkLen > strlen($decoded)) { return ''; }
        return substr($decoded, 2, $chunkLen);
    }

    private function pace(): void
    {
        $wait = SEND_INTERVAL - (microtime(true) - $this->lastSend);
        if ($wait > 0) { usleep((int)($wait * 1e6)); }
        $this->lastSend = microtime(true);
    }

    private function peerCursors(array $obj): ?array
    {
        $data = $obj['push']['pub']['data'] ?? null;
        if (!is_array($data) || ($data['type'] ?? '') !== 'cursors_update') { return null; }
        $p = $data['payload'] ?? null;
        if (!is_array($p) || ($p['user_uuid'] ?? '') === ($this->auth['userUUID'] ?? '')) { return null; }
        $cur = $p['cursors'] ?? null;
        return is_array($cur) && $cur ? $cur : null;
    }

    // ---- cursor codec (matches transport/cupsonline) -----------------------
    public static function cursorsFromBytes(string $blob): array
    {
        $n = intdiv(strlen($blob) + BYTES_PER_NUMBER - 1, BYTES_PER_NUMBER);
        $nums = [];
        for ($i = 0; $i < $n; $i++) {
            $v = 0;
            for ($j = 0; $j < BYTES_PER_NUMBER; $j++) {
                $v *= 256;
                $idx = $i * BYTES_PER_NUMBER + $j;
                if ($idx < strlen($blob)) { $v += ord($blob[$idx]); }
            }
            $nums[] = $v;
        }
        $cur = [];
        for ($i = 0; $i < count($nums); $i += 2) {
            $cur[] = ['row' => $nums[$i], 'column' => $nums[$i + 1] ?? 0];
        }
        return $cur;
    }

    public static function bytesFromCursors(array $cursors): string
    {
        $out = '';
        foreach ($cursors as $c) {
            foreach ([(int)($c['row'] ?? 0), (int)($c['column'] ?? 0)] as $v) {
                $b = '';
                for ($j = 0; $j < BYTES_PER_NUMBER; $j++) { $b = chr($v % 256) . $b; $v = intdiv($v, 256); }
                $out .= $b;
            }
        }
        return $out;
    }

    /** Offline round-trip of the cups framing chain (no network). */
    public static function selfTest(): void
    {
        $ok = true;
        foreach (['', 'A', 'hello', str_repeat('Z', 250), str_repeat('Q', 4000)] as $p) {
            $frame = pack('CNN', Mux::DATA, 7, strlen($p)) . $p;
            $blob  = pack('n', strlen($frame)) . $frame;
            $recv  = '';
            for ($off = 0; $off < strlen($blob); $off += MAX_MSG_DATA) {
                $chunk = substr($blob, $off, MAX_MSG_DATA);
                $cur   = self::cursorsFromBytes(pack('n', strlen($chunk)) . $chunk);
                $dec   = self::bytesFromCursors($cur);
                $recv .= substr($dec, 2, unpack('n', substr($dec, 0, 2))[1]);
            }
            $ln  = unpack('n', substr($recv, 0, 2))[1];
            $pkt = substr($recv, 2, $ln);
            $h   = unpack('Ctype/Nsid/Nlen', substr($pkt, 0, 9));
            $got = substr($pkt, 9, $h['len']);
            $pass = ($h['type'] === Mux::DATA && $h['sid'] === 7 && $got === $p);
            $ok = $ok && $pass;
            printf("selftest len=%-5d %s\n", strlen($p), $pass ? 'OK' : 'FAIL');
        }
        echo $ok ? "SELFTEST PASS\n" : "SELFTEST FAIL\n";
        exit($ok ? 0 : 1);
    }
}

// ---- entry point (after the class so it is declared before use) -----------
if (PHP_SAPI === 'cli' && ($argv[1] ?? '') === 'selftest') { CupsCarrier::selfTest(); exit; }

(new PhpboxNode('cupsonline', 'cups.online', function (array $g): string {
    $u = (string)($g['url'] ?? '');
    if ($u === '' && !empty($g['room'])) {
        $u = 'https://interview.cups.online/live-coding/?room=' . preg_replace('/[^0-9a-f-]/', '', (string)$g['room']);
    }
    return $u;
}, fn(string $room) => new CupsCarrier($room), RUN_CAP))->handle();
