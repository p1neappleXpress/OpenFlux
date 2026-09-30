<?php
if (is_file(__DIR__ . '/config.php')) { require_once __DIR__ . '/config.php'; }   // token of a deployed node
/**
 * phpbox.php - stream-mux exit for the OpenFlux client on plain PHP hosting
 * (including free shared hosting that cannot run a binary, hold a process,
 * or listen on a socket). See transport/phpbox in the core for the client.
 *
 * NOT a packet exit and NOT an open proxy: the client speaks a tiny stream
 * mux (OPEN host:port / DATA / CLOSE) over two half-duplex HTTP channels,
 * because such hosts buffer request bodies but stream responses:
 *
 *   down (GET,  long-poll) - THE session process: holds the dst sockets,
 *      stream_select()s them, and streams dst->client frames in the response.
 *      It also drains the upstream bus each loop and applies OPEN/DATA/CLOSE.
 *   up   (POST, short)     - appends client->dst frames to the per-session bus
 *      (a file; no MySQL and no persistent process needed), returns at once.
 *
 * A session id is minted per down-connection, so a reconnect after the host
 * caps the request (RUN_CAP) starts clean; MTProto and short HTTPS tolerate
 * it. Guards: a shared token, only ports 80/443, and no private/loopback
 * targets. v0 carries frames in the clear - the OpenFlux client should wrap
 * the carrier with the core's encryption before real use.
 *
 * Setup: upload next to a domain, set PHPBOX_TOKEN in the environment (or
 * edit the constant), point the client's phpbox transport at its URL.
 *
 * Frame: type(1) | stream_id(4 BE) | len(4 BE) | payload(len)
 *   1 OPEN "host:port"  2 DATA  3 CLOSE  4 OPEN_OK  5 OPEN_ERR reason
 */

$TOKEN = getenv('PHPBOX_TOKEN') ?: (defined('PHPBOX_TOKEN') ? PHPBOX_TOKEN : 'CHANGE-ME'); // putenv is disabled on some free hosts: config.php also define()s it
$BUS_DIR = getenv('PHPBOX_BUS') ?: (sys_get_temp_dir() . '/phpbox-bus');
const RUN_CAP = 140;   // seconds a down-poll holds before returning (client reconnects)
const DIAL_TO = 6;

const OPEN = 1, DATA = 2, CLOSE = 3, OPEN_OK = 4, OPEN_ERR = 5;

if (!hash_equals($TOKEN, (string)($_GET['k'] ?? ''))) { http_response_code(404); exit("no\n"); }
$sess = preg_replace('/[^a-zA-Z0-9]/', '', $_GET['s'] ?? '');
if ($sess === '') { http_response_code(400); exit("no session\n"); }
@mkdir($BUS_DIR, 0700, true);
$busUp = $BUS_DIR . "/$sess.up";

switch ($_GET['r'] ?? '') {
    case 'up':   up_ingest($busUp); break;
    case 'down': down_pump($busUp); break;
    default:     http_response_code(400); exit("bad role\n");
}

// ---- upstream: append the POSTed frames to the bus, return now -----------
function up_ingest(string $busUp): void
{
    $body = file_get_contents('php://input');
    $fp = fopen($busUp, 'ab');
    if ($fp) { flock($fp, LOCK_EX); fwrite($fp, $body); flock($fp, LOCK_UN); fclose($fp); }
    header('Content-Type: text/plain');
    echo 'ok ' . strlen($body) . "\n";
}

// ---- downstream: the session. Hold sockets, pump both ways ---------------
function down_pump(string $busUp): void
{
    @set_time_limit(0);
    ignore_user_abort(false);              // client gone => end the session
    header('Content-Type: application/octet-stream');
    header('X-Accel-Buffering: no');       // ask LiteSpeed/nginx not to buffer
    header('Content-Encoding: none');
    while (ob_get_level() > 0) { ob_end_flush(); }
    echo str_repeat(' ', 4096) . "\n";     // pad past any fixed output buffer
    @flush();
    @touch($busUp);

    $socks = [];                            // stream_id => resource
    $upfp  = fopen($busUp, 'rb');
    $inbuf = '';
    $start = microtime(true);

    while (microtime(true) - $start < RUN_CAP) {
        $chunk = stream_get_contents($upfp);
        if ($chunk !== false && $chunk !== '') { $inbuf .= $chunk; }
        while (($f = take_frame($inbuf)) !== null) {
            [$type, $sid, $payload] = $f;
            if ($type === OPEN) {
                [$host, $port] = array_pad(explode(':', $payload, 2), 2, '');
                if (!in_array((int)$port, [80, 443], true)) { emit(OPEN_ERR, $sid, 'port'); continue; }
                if (is_private_host($host)) { emit(OPEN_ERR, $sid, 'blocked'); continue; }
                $s = @stream_socket_client("tcp://$host:$port", $e, $es, DIAL_TO, STREAM_CLIENT_CONNECT);
                if ($s) { stream_set_blocking($s, false); $socks[$sid] = $s; emit(OPEN_OK, $sid, ''); }
                else { emit(OPEN_ERR, $sid, (string)$es); }
            } elseif ($type === DATA && isset($socks[$sid])) {
                @fwrite($socks[$sid], $payload);
            } elseif ($type === CLOSE && isset($socks[$sid])) {
                @fclose($socks[$sid]); unset($socks[$sid]);
            }
        }

        if ($socks) {
            $r = $socks; $w = $ex = null;
            if (@stream_select($r, $w, $ex, 0, 200000) > 0) {
                foreach ($r as $s) {
                    $sid = array_search($s, $socks, true);
                    $d = @fread($s, 32768);
                    if ($d === '' || $d === false) {
                        if (feof($s)) { emit(CLOSE, $sid, ''); @fclose($s); unset($socks[$sid]); }
                    } else {
                        emit(DATA, $sid, $d);
                    }
                }
            }
        } else {
            usleep(100000);
        }
        if (connection_aborted()) { break; }
    }
    foreach ($socks as $s) { @fclose($s); }
}

function emit(int $type, int $sid, string $payload): void
{
    echo pack('CNN', $type, $sid, strlen($payload)) . $payload;
    @flush();
}

/** Pull one whole frame off the front of $buf, or null if incomplete. */
function take_frame(string &$buf): ?array
{
    if (strlen($buf) < 9) { return null; }
    $h = unpack('Ctype/Nsid/Nlen', substr($buf, 0, 9));
    if (strlen($buf) < 9 + $h['len']) { return null; }
    $payload = substr($buf, 9, $h['len']);
    $buf = substr($buf, 9 + $h['len']);
    return [$h['type'], $h['sid'], $payload];
}

/** Refuse loopback / private / reserved targets (no SSRF into the host LAN). */
function is_private_host(string $host): bool
{
    if (getenv('PHPBOX_ALLOW_PRIVATE') === '1') { return false; } // local testing only
    $ips = [];
    if (filter_var($host, FILTER_VALIDATE_IP)) {
        $ips[] = $host;
    } else {
        foreach (@dns_get_record($host, DNS_A + DNS_AAAA) ?: [] as $rec) {
            $ips[] = $rec['ip'] ?? $rec['ipv6'] ?? null;
        }
        if (!array_filter($ips)) { $ips = [gethostbyname($host)]; }
    }
    foreach (array_filter($ips) as $ip) {
        if (!filter_var($ip, FILTER_VALIDATE_IP,
                FILTER_FLAG_NO_PRIV_RANGE | FILTER_FLAG_NO_RES_RANGE)) {
            return true;
        }
    }
    return false;
}
