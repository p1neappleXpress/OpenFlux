<?php
/**
 * cupsexit.php - a phpbox exit that talks to the client over cups.online.
 *
 * The censored client can only reach cups.online, so the chain is
 *   client  ->  cups.online (a shared-editor room)  ->  this PHP  ->  dst
 * This script joins the SAME cups room as the client, reads the client's
 * stream-mux frames (OPEN/DATA/CLOSE) hidden in cursor integers, dials the
 * destinations with fsockopen, and writes the replies back into the room as
 * cursors. It IS a cups participant (like transport/cupsonline) plus the mux
 * demux - no binary, no daemon, no listening socket needed.
 *
 * Run on the host (open in a browser so the host's bot check passes):
 *   https://<host>/cupsexit.php?k=TOKEN&url=<full cups room URL>
 * It runs for RUN_CAP seconds as one cups user, then returns; re-open (or a
 * pinger) to keep an exit present. The Go client uses --mode=stream with the
 * same room URL.
 *
 * Guards: shared token, ports 80/443 only, no private/loopback targets.
 * v0 is plaintext at the mux layer (the cups payload is stego, not secret);
 * wrap with the core's encryption before real use.
 *
 * Framing matches transport/cupsonline exactly: bytes -> 6-byte big-endian
 * numbers -> {row,column} cursors; a message is (2-byte chunk len + chunk),
 * and the reassembled stream is a series of (2-byte packet len + packet),
 * where each packet is one mux frame:
 *   type(1) | stream_id(4 BE) | len(4 BE) | payload      (1 OPEN "host:port",
 *   2 DATA, 3 CLOSE, 4 OPEN_OK, 5 OPEN_ERR reason)
 */

error_reporting(E_ALL & ~E_DEPRECATED);
const UA = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36';
const BYTES_PER_NUMBER = 6;
const RUN_CAP        = 140;    // seconds this exit stays in the room per request
const DIAL_TO        = 6;
const SEND_INTERVAL  = 0.018;  // s between cursor messages (cups throttles)
const MAX_MSG_DATA   = 3000;   // bytes of payload per cursor message

const OPEN = 1, DATA = 2, CLOSE = 3, OPEN_OK = 4, OPEN_ERR = 5;

$TOKEN = getenv('PHPBOX_TOKEN') ?: 'CHANGE-ME';

// Offline: round-trip the exit's whole framing chain (mux frame -> packet ->
// chunk -> cursors -> back) with no network. `php cupsexit.php selftest`.
if (PHP_SAPI === 'cli' && ($argv[1] ?? '') === 'selftest') { CupsExit::selfTest(); exit; }

header('Content-Type: text/plain; charset=utf-8');
if (!hash_equals($TOKEN, (string)($_GET['k'] ?? ''))) { http_response_code(404); exit("no\n"); }
$roomURL = $_GET['url'] ?? '';
if ($roomURL === '' && !empty($_GET['room'])) {
    $roomURL = 'https://interview.cups.online/live-coding/?room=' . preg_replace('/[^0-9a-f-]/', '', $_GET['room']);
}
if ($roomURL === '') { http_response_code(400); exit("need ?url=<cups room URL> or ?room=<uuid>\n"); }
@set_time_limit(0);
while (ob_get_level() > 0) { ob_end_flush(); }

$exit = new CupsExit($roomURL);
$exit->run();

// ===========================================================================
class CupsExit
{
    private array $auth;
    private $ws;
    private string $cookieFile;
    private string $recvBuf = '';   // reassembled cups packet stream
    private string $muxBuf  = '';   // reassembled mux frame stream
    private array  $socks   = [];   // stream_id => dst resource
    private float  $lastSend = 0.0;

    public function __construct(private string $roomURL) {
        $this->cookieFile = tempnam(sys_get_temp_dir(), 'cupsx');
    }

    /** selfTest round-trips the framing chain offline (no cups). */
    public static function selfTest(): void
    {
        $x = new self('cli');
        $ok = true;
        foreach (['', 'A', 'hello', str_repeat('Z', 250)] as $p) {
            $frame = pack('CNN', DATA, 7, strlen($p)) . $p;   // mux frame (= packet)
            $blob  = pack('n', strlen($frame)) . $frame;       // packet framing
            $msg   = pack('n', strlen($blob)) . $blob;         // chunk framing
            $cursors = $x->cursorsFromBytes($msg);

            $decoded  = $x->bytesFromCursors($cursors);
            $chunkLen = unpack('n', substr($decoded, 0, 2))[1];
            $chunk    = substr($decoded, 2, $chunkLen);
            $ln       = unpack('n', substr($chunk, 0, 2))[1];
            $pkt      = substr($chunk, 2, $ln);
            $h        = unpack('Ctype/Nsid/Nlen', substr($pkt, 0, 9));
            $got      = substr($pkt, 9, $h['len']);

            $pass = ($h['type'] === DATA && $h['sid'] === 7 && $got === $p);
            $ok = $ok && $pass;
            printf("selftest len=%-4d %s\n", strlen($p), $pass ? 'OK' : 'FAIL');
        }
        echo $ok ? "SELFTEST PASS\n" : "SELFTEST FAIL\n";
        exit($ok ? 0 : 1);
    }

    public function run(): void
    {
        $a = $this->authorize($this->roomURL);
        if (!$a) { echo "auth failed\n"; return; }
        $this->auth = $a;
        echo "joined room {$a['roomUUID']} as {$a['userUUID']}\n";
        if (!$this->wsJoin()) { echo "ws join failed\n"; return; }
        echo "cups exit ready; serving up to " . RUN_CAP . "s\n";

        $start = microtime(true);
        while (microtime(true) - $start < RUN_CAP) {
            if (connection_aborted()) { break; }
            $read = array_merge([$this->ws], array_values($this->socks));
            $w = $e = null;
            if (@stream_select($read, $w, $e, 0, 200000) > 0) {
                foreach ($read as $s) {
                    if ($s === $this->ws) { $this->onWSReadable(); }
                    else { $this->onDstReadable($s); }
                }
            }
        }
        foreach ($this->socks as $s) { @fclose($s); }
        @fclose($this->ws);
        @unlink($this->cookieFile);
        echo "exit done\n";
    }

    // ---- cups receive: WS -> cursors -> bytes -> packets -> mux frames -----
    private function onWSReadable(): void
    {
        $msg = $this->wsRead(0.2);
        if ($msg === null || $msg === '') { return; }
        foreach (explode("\n", trim($msg)) as $line) {
            if ($line === '' ) { continue; }
            if ($line === '{}') { $this->wsWriteText('{}'); continue; }
            $obj = json_decode($line, true);
            if (!is_array($obj)) { continue; }
            $cursors = $this->peerCursors($obj);
            if ($cursors === null) { continue; }
            $this->ingestCursors($cursors);
        }
    }

    private function ingestCursors(array $cursors): void
    {
        $decoded = $this->bytesFromCursors($cursors);
        if (strlen($decoded) < 2) { return; }
        $chunkLen = unpack('n', substr($decoded, 0, 2))[1];
        if (2 + $chunkLen > strlen($decoded)) { return; }
        $this->recvBuf .= substr($decoded, 2, $chunkLen);

        // pull whole packets (2-byte len + packet); each packet is mux bytes
        while (strlen($this->recvBuf) >= 2) {
            $ln = unpack('n', substr($this->recvBuf, 0, 2))[1];
            if ($ln === 0) { $this->recvBuf = ''; break; }
            if (strlen($this->recvBuf) < 2 + $ln) { break; }
            $pkt = substr($this->recvBuf, 2, $ln);
            $this->recvBuf = substr($this->recvBuf, 2 + $ln);
            $this->muxBuf .= $pkt;
        }
        $this->drainMux();
    }

    private function drainMux(): void
    {
        while (strlen($this->muxBuf) >= 9) {
            $h = unpack('Ctype/Nsid/Nlen', substr($this->muxBuf, 0, 9));
            if (strlen($this->muxBuf) < 9 + $h['len']) { break; }
            $payload = substr($this->muxBuf, 9, $h['len']);
            $this->muxBuf = substr($this->muxBuf, 9 + $h['len']);
            $this->applyMux($h['type'], $h['sid'], $payload);
        }
    }

    private function applyMux(int $type, int $sid, string $payload): void
    {
        if ($type === OPEN) {
            [$host, $port] = array_pad(explode(':', $payload, 2), 2, '');
            if (!in_array((int)$port, [80, 443], true)) { $this->sendFrame(OPEN_ERR, $sid, 'port'); return; }
            if ($this->isPrivate($host)) { $this->sendFrame(OPEN_ERR, $sid, 'blocked'); return; }
            $s = @stream_socket_client("tcp://$host:$port", $e, $es, DIAL_TO, STREAM_CLIENT_CONNECT);
            if ($s) { stream_set_blocking($s, false); $this->socks[$sid] = $s; $this->sendFrame(OPEN_OK, $sid, ''); }
            else { $this->sendFrame(OPEN_ERR, $sid, (string)$es); }
        } elseif ($type === DATA && isset($this->socks[$sid])) {
            @fwrite($this->socks[$sid], $payload);
        } elseif ($type === CLOSE && isset($this->socks[$sid])) {
            @fclose($this->socks[$sid]); unset($this->socks[$sid]);
        }
    }

    // ---- dst -> client -----------------------------------------------------
    private function onDstReadable($s): void
    {
        $sid = array_search($s, $this->socks, true);
        if ($sid === false) { return; }
        $d = @fread($s, MAX_MSG_DATA - 32);
        if ($d === '' || $d === false) {
            if (feof($s)) { $this->sendFrame(CLOSE, $sid, ''); @fclose($s); unset($this->socks[$sid]); }
            return;
        }
        $this->sendFrame(DATA, $sid, $d);
    }

    /** sendFrame: mux frame -> packet framing -> chunk framing -> cursors -> WS. */
    private function sendFrame(int $type, int $sid, string $payload): void
    {
        $frame = pack('CNN', $type, $sid, strlen($payload)) . $payload;   // mux frame = packet
        $blob  = pack('n', strlen($frame)) . $frame;                       // packet framing
        // chunk if bigger than one message (rare for control frames / small reads)
        for ($off = 0; $off < strlen($blob); $off += MAX_MSG_DATA) {
            $chunk = substr($blob, $off, MAX_MSG_DATA);
            $msgPayload = pack('n', strlen($chunk)) . $chunk;              // chunk framing
            $this->pace();
            $this->publishCursors($this->cursorsFromBytes($msgPayload));
        }
    }

    private function pace(): void
    {
        $wait = SEND_INTERVAL - (microtime(true) - $this->lastSend);
        if ($wait > 0) { usleep((int)($wait * 1e6)); }
        $this->lastSend = microtime(true);
    }

    private function publishCursors(array $cursors): void
    {
        $this->wsWriteText(json_encode([
            'rpc' => ['method' => 'shared_editor_change_cursors',
                'data' => ['cursors' => $cursors, 'ranges' => [],
                    'room' => $this->auth['roomUUID'], 'user' => $this->auth['userUUID']]],
            'id' => random_int(100, 1 << 20),
        ]));
    }

    // ---- cursor codec (matches transport/cupsonline) -----------------------
    private function cursorsFromBytes(string $blob): array
    {
        $n = intdiv(strlen($blob) + BYTES_PER_NUMBER - 1, BYTES_PER_NUMBER);
        $cur = [];
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
        for ($i = 0; $i < count($nums); $i += 2) {
            $cur[] = ['row' => $nums[$i], 'column' => $nums[$i + 1] ?? 0];
        }
        return $cur;
    }

    private function bytesFromCursors(array $cursors): string
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

    private function peerCursors(array $obj): ?array
    {
        $data = $obj['push']['pub']['data'] ?? null;
        if (!is_array($data) || ($data['type'] ?? '') !== 'cursors_update') { return null; }
        $p = $data['payload'] ?? null;
        if (!is_array($p) || ($p['user_uuid'] ?? '') === $this->auth['userUUID']) { return null; }
        $cur = $p['cursors'] ?? null;
        return is_array($cur) && $cur ? $cur : null;
    }

    // ---- auth + websocket (as transport/cupsonline) ------------------------
    private function authorize(string $roomURL): ?array
    {
        [$html, $code] = $this->http($roomURL, 'GET', null, []);
        if ($code !== 200) { echo "GET room status $code\n"; return null; }
        $a = [
            'roomUUID'  => $this->scrape('/data-room="\{&quot;uuid&quot;:\s*&quot;([0-9a-f-]{36})&quot;/', $html),
            'userUUID'  => $this->scrape('/data-user="\{&quot;uuid&quot;:\s*&quot;([0-9a-f-]{36})&quot;/', $html),
            'connToken' => $this->scrape('/<meta[^>]+name="centrifuge-connection-token"[^>]+content="([^"]+)"/', $html),
            'connURL'   => $this->scrape('/<meta[^>]+name="centrifuge-connection-url"[^>]+content="([^"]+)"/', $html),
            'subURL'    => $this->scrape('/<meta[^>]+name="centrifuge-subscription-token-url"[^>]+content="([^"]+)"/', $html),
        ];
        foreach ($a as $k => $v) { if ($v === '') { echo "missing $k\n"; return null; } }
        $a['csrf'] = $this->cookie('csrftoken');
        $a['channel'] = '$shared_editor:room-' . $a['roomUUID'];
        [$body, $c2] = $this->http($a['subURL'], 'POST', json_encode(['channel' => $a['channel']]),
            ['Content-Type: application/json', 'X-CSRFToken: ' . $a['csrf'],
             'Origin: ' . $this->originOf($roomURL), 'Referer: ' . $roomURL]);
        if ($c2 !== 200) { echo "sub token status $c2\n"; return null; }
        $a['subToken'] = json_decode($body, true)['token'] ?? '';
        if ($a['subToken'] === '') { echo "empty sub token\n"; return null; }
        return $a;
    }

    private function wsJoin(): bool
    {
        $wsURL = rtrim(preg_replace('#^http#', 'ws', $this->auth['connURL']), '/') . '/websocket';
        $p = parse_url($wsURL);
        $secure = ($p['scheme'] === 'wss');
        $host = $p['host'];
        $port = $p['port'] ?? ($secure ? 443 : 80);
        $cookies = [];
        foreach (@file($this->cookieFile) ?: [] as $l) {
            $c = explode("\t", trim($l));
            if (count($c) === 7) { $cookies[] = $c[5] . '=' . $c[6]; }
        }
        $this->ws = @stream_socket_client(($secure ? 'ssl' : 'tcp') . "://$host:$port", $e, $es, 10, STREAM_CLIENT_CONNECT);
        if (!$this->ws) { echo "ws dial: $es\n"; return false; }
        $key = base64_encode(random_bytes(16));
        fwrite($this->ws, "GET {$p['path']} HTTP/1.1\r\nHost: $host\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
            . "Sec-WebSocket-Key: $key\r\nSec-WebSocket-Version: 13\r\nUser-Agent: " . UA . "\r\n"
            . "Origin: " . $this->originOf($this->auth['connURL']) . "\r\n"
            . ($cookies ? "Cookie: " . implode('; ', $cookies) . "\r\n" : '') . "\r\n");
        $resp = '';
        while (!feof($this->ws)) { $resp .= fgets($this->ws); if (str_contains($resp, "\r\n\r\n")) break; }
        if (!str_contains($resp, ' 101 ')) { echo "ws handshake failed\n"; return false; }

        $this->wsWriteText(json_encode(['id' => 1, 'connect' => ['token' => $this->auth['connToken'], 'name' => 'js']]));
        if (!$this->wsExpect(1)) { return false; }
        $this->wsWriteText(json_encode(['id' => 2, 'subscribe' => ['channel' => $this->auth['channel'], 'token' => $this->auth['subToken']]]));
        return $this->wsExpect(2);
    }

    private function wsExpect(int $id): bool
    {
        $deadline = microtime(true) + 12;
        while (microtime(true) < $deadline) {
            $f = $this->wsRead(3.0);
            if ($f === null) { continue; }
            if ($f === '{}') { $this->wsWriteText('{}'); continue; }
            foreach (explode("\n", trim((string)$f)) as $line) {
                $o = json_decode($line, true);
                if (is_array($o) && (($o['id'] ?? null) === $id)) { return true; }
            }
        }
        return false;
    }

    private function wsWriteText(string $payload): void
    {
        $len = strlen($payload);
        $frame = chr(0x81);
        $mask = random_bytes(4);
        if ($len < 126) { $frame .= chr(0x80 | $len); }
        elseif ($len < 65536) { $frame .= chr(0x80 | 126) . pack('n', $len); }
        else { $frame .= chr(0x80 | 127) . pack('J', $len); }
        $frame .= $mask;
        for ($i = 0; $i < $len; $i++) { $frame .= $payload[$i] ^ $mask[$i % 4]; }
        @fwrite($this->ws, $frame);
    }

    private function wsRead(float $timeout): ?string
    {
        stream_set_timeout($this->ws, (int)$timeout, (int)(($timeout - (int)$timeout) * 1e6));
        $b0 = @fread($this->ws, 1);
        if ($b0 === '' || $b0 === false) { return null; }
        $b1 = @fread($this->ws, 1);
        if ($b1 === '' || $b1 === false) { return null; }
        $opcode = ord($b0) & 0x0f;
        $len = ord($b1) & 0x7f;
        $masked = (ord($b1) & 0x80) !== 0;
        if ($len === 126) { $len = unpack('n', fread($this->ws, 2))[1]; }
        elseif ($len === 127) { $len = unpack('J', fread($this->ws, 8))[1]; }
        $mask = $masked ? fread($this->ws, 4) : '';
        $data = '';
        while (strlen($data) < $len) {
            $chunk = fread($this->ws, $len - strlen($data));
            if ($chunk === '' || $chunk === false) { break; }
            $data .= $chunk;
        }
        if ($masked && $mask !== '') { for ($i = 0; $i < strlen($data); $i++) { $data[$i] = $data[$i] ^ $mask[$i % 4]; } }
        if ($opcode === 0x8) { return null; }
        if ($opcode === 0x9) { $this->wsWritePong($data); return ''; }
        if ($opcode === 0xA) { return ''; }
        return $data;
    }

    private function wsWritePong(string $payload): void
    {
        $mask = random_bytes(4);
        $frame = chr(0x8A) . chr(0x80 | strlen($payload)) . $mask;
        for ($i = 0; $i < strlen($payload); $i++) { $frame .= $payload[$i] ^ $mask[$i % 4]; }
        @fwrite($this->ws, $frame);
    }

    // ---- small helpers -----------------------------------------------------
    private function http(string $url, string $method, ?string $body, array $headers): array
    {
        $ch = curl_init($url);
        curl_setopt_array($ch, [
            CURLOPT_RETURNTRANSFER => true, CURLOPT_USERAGENT => UA,
            CURLOPT_COOKIEJAR => $this->cookieFile, CURLOPT_COOKIEFILE => $this->cookieFile,
            CURLOPT_FOLLOWLOCATION => true, CURLOPT_TIMEOUT => 30,
            CURLOPT_HTTPHEADER => array_merge(['Accept-Language: ru-RU,ru;q=0.9'], $headers),
        ]);
        if ($method === 'POST') { curl_setopt($ch, CURLOPT_POST, true); curl_setopt($ch, CURLOPT_POSTFIELDS, $body); }
        $out = curl_exec($ch);
        $code = curl_getinfo($ch, CURLINFO_HTTP_CODE);
        return [$out ?: '', $code];
    }

    private function scrape(string $re, string $html): string { return preg_match($re, $html, $m) ? $m[1] : ''; }

    private function cookie(string $name): string
    {
        foreach (@file($this->cookieFile) ?: [] as $l) {
            $p = explode("\t", trim($l));
            if (count($p) === 7 && $p[5] === $name) { return $p[6]; }
        }
        return '';
    }

    private function originOf(string $u): string { $p = parse_url($u); return $p['scheme'] . '://' . $p['host']; }

    private function isPrivate(string $host): bool
    {
        if (getenv('PHPBOX_ALLOW_PRIVATE') === '1') { return false; }
        $ip = filter_var($host, FILTER_VALIDATE_IP) ? $host : gethostbyname($host);
        return !filter_var($ip, FILTER_VALIDATE_IP, FILTER_FLAG_NO_PRIV_RANGE | FILTER_FLAG_NO_RES_RANGE);
    }
}
