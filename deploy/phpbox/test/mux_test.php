<?php
// Mux self-test over a real local TCP echo server (no network, no hosting):
//   php test/mux_test.php
// Covers async dial, OPEN_ERR, id reuse, ordered echo, and the write queue
// (2 MB into a server that does not read for 2 s used to lose data).
putenv('PHPBOX_ALLOW_PRIVATE=1');
require __DIR__ . '/../lib/mux.php';

final class MemCarrier implements Carrier
{
    public array $out = [];                      // frames the mux sent: [type, sid, payload]
    private Mux $m;
    public function setMux(Mux $m): void { $this->m = $m; }
    public function connect(): bool { return true; }
    public function sockets(): array { return []; }
    public function onReadable($sock): void {}
    public array $dataBytes = [];                // sid => DATA bytes seen (payloads are kept only up to a cap)
    private int $stored = 0;
    public function sendPacket(string $f): void
    {
        $h = unpack('Ctype/Nsid/Nlen', substr($f, 0, 9));
        if ($h['type'] === Mux::DATA) {
            $this->dataBytes[$h['sid']] = ($this->dataBytes[$h['sid']] ?? 0) + $h['len'];
            if ($this->stored > 8 * 1024 * 1024) { return; }       // a flood in a test must not eat the memory
            $this->stored += $h['len'];
        }
        $this->out[] = [$h['type'], $h['sid'], substr($f, 9, $h['len'])];
    }
    public function feed(int $type, int $sid, string $payload): void
    {
        $this->m->onPacket(pack('CNN', $type, $sid, strlen($payload)) . $payload);
    }
    public function take(int $type, int $sid): string
    {
        $b = '';
        foreach ($this->out as $o) { if ($o[0] === $type && $o[1] === $sid) { $b .= $o[2]; } }
        return $b;
    }
    public function has(int $type, int $sid): bool
    {
        foreach ($this->out as $o) { if ($o[0] === $type && $o[1] === $sid) { return true; } }
        return false;
    }
}

function server(int $port, int $delay)
{
    $p = proc_open([PHP_BINARY, __DIR__ . '/echoserver.php', (string)$port, (string)$delay], [1 => ['pipe', 'w'], 2 => ['file', '/dev/null', 'w']], $pipes);
    fgets($pipes[1]);                            // "ready"
    return $p;
}

$fast = random_int(41000, 41999); $slow = $fast + 1000; $dead = $fast + 2000; $alt = $fast + 3000;
$s1 = server($fast, 0); $s2 = server($slow, 2); $s3 = server($alt, 0);   // the echo server serves one connection at a time

$c = new MemCarrier();
$mux = new Mux($c);
$logs = [];
$mux->log = function ($l, $m) use (&$logs) { $logs[] = "$l $m"; };
$mux->resolver = fn(string $h) => $h === 'multi.test' ? ['::1', '127.0.0.1'] : [$h];
$big = random_bytes(2 * 1024 * 1024);
$ok = true;
$check = function (string $name, bool $pass) use (&$ok) { printf("%-46s %s\n", $name, $pass ? 'OK' : 'FAIL'); $ok = $ok && $pass; };
$n = 0;
$mux->onTick = function (Mux $m) use (&$n, $c, $fast, $slow, $dead, $alt, $big, $check) {
    $n++;
    if ($n === 1) {
        $c->feed(Mux::OPEN, 1, "127.0.0.1:$fast");
        $c->feed(Mux::OPEN, 2, "127.0.0.1:$dead");             // nothing listens there
        $c->feed(Mux::OPEN, 3, "127.0.0.1:$slow");
        $c->feed(Mux::OPEN, 6, "multi.test:$alt");            // its first address refuses, the second works
        $c->feed(Mux::DATA, 1, 'hel');                          // DATA right behind OPEN, before the dial completes
        $c->feed(Mux::DATA, 1, 'lo');
    } elseif ($n === 2) {
        $check('async dial: OPEN_OK for a live server', $c->has(Mux::OPEN_OK, 1));
        $check('async dial: OPEN_ERR for a dead port', $c->has(Mux::OPEN_ERR, 2));
        $check('data sent during the dial arrives in order', $c->take(Mux::DATA, 1) === 'hello');
        $check('a slow dial does not block the others', $c->has(Mux::OPEN_OK, 3));
        $check('a refused address falls back to the next one', $c->has(Mux::OPEN_OK, 6) && !$c->has(Mux::OPEN_ERR, 6));
        foreach (str_split($big, 60000) as $part) { $c->feed(Mux::DATA, 3, $part); }   // 2 MB at a server that is not reading
        $c->feed(Mux::CLOSE, 1, '');
        $c->feed(Mux::OPEN, 1, "127.0.0.1:$fast");             // the id is reused
    } elseif ($n === 3) {
        $c->feed(Mux::DATA, 1, 'again');
    } elseif ($n === 4) {
        $check('reused stream id works', str_ends_with($c->take(Mux::DATA, 1), 'again'));
    } elseif ($n >= 9) {
        return false;
    }
    return true;
};
$why = $mux->run(30);
$echoed = $c->take(Mux::DATA, 3);
$check('write queue: 2 MB echoed back intact', strlen($echoed) === strlen($big) && md5($echoed) === md5($big));
$check('run stops when a tick says so', $why === 'stopped');
$check('stats count both directions', $mux->stats['up'] >= strlen($big) && $mux->stats['down'] >= strlen($big));
$check('stats count the failed open (only the dead port)', $mux->stats['failed'] === 1);
$check('destinations are hidden from the log by default', !preg_grep('/127\.0\.0\.1/', $logs));
$check('ending a run tells the client about streams still open', $c->has(Mux::CLOSE, 3) && $c->has(Mux::CLOSE, 1));

// ---- idle streams are reaped (a lost CLOSE must not leak a socket) ----
$ci = new MemCarrier(); $mi = new Mux($ci); $mi->idleTimeout = 2;
$ticks = 0;
$mi->onTick = function (Mux $m) use (&$ticks, $ci, $fast) {
    $ticks++;
    if ($ticks === 1) { $ci->feed(Mux::OPEN, 1, "127.0.0.1:$fast"); }
    return $ticks < 6;
};
$mi->run(20);
$check('an idle stream is closed and the client told', $ci->has(Mux::CLOSE, 1));

// ---- handover rules (two generations read the same OPEN) ----
$open = fn(Mux $m, MemCarrier $cc, int $sid) => $cc->feed(Mux::OPEN, $sid, "127.0.0.1:$fast");
$cd = new MemCarrier(); $md = new Mux($cd); $md->accepting = false; $open($md, $cd, 9);
$check('a draining generation ignores new streams', $md->activeStreams() === 0 && !$cd->out);
$cn = new MemCarrier(); $mn = new Mux($cn); $mn->claim = fn(int $sid) => false; $open($mn, $cn, 9);
$check('a stream claimed by the other generation is not dialed', $mn->activeStreams() === 0);
$cy = new MemCarrier(); $my = new Mux($cy); $my->claim = fn(int $sid) => true; $open($my, $cy, 9);
$check('a stream this generation claims is dialed', $my->activeStreams() === 1);
$dirClaim = sys_get_temp_dir() . '/mux-claim-' . getmypid(); @mkdir($dirClaim);
$claimFn = fn(int $sid) => @mkdir("$dirClaim/$sid");   // what node.php does: mkdir is atomic
$ca = new MemCarrier(); $ma = new Mux($ca); $ma->claim = $claimFn;
$cb = new MemCarrier(); $mb = new Mux($cb); $mb->claim = $claimFn;
foreach ([1, 2, 3, 4, 5] as $sid) { $open($ma, $ca, $sid); $open($mb, $cb, $sid); }
$check('two generations never both serve a stream', $ma->activeStreams() + $mb->activeStreams() === 5);
foreach (glob("$dirClaim/*") as $f) { @rmdir($f); } @rmdir($dirClaim);

// ---- flow control: a client that asks for windows gets them; one that does not, does not ----
function sendserver(int $port) {
    $p = proc_open([PHP_BINARY, __DIR__ . '/sendserver.php', (string)$port], [1 => ['pipe', 'w'], 2 => ['file', '/dev/null', 'w']], $pipes);
    fgets($pipes[1]);
    return $p;
}
$bulkPort = $fast + 4000; $bulk2 = $fast + 5000;
$sb1 = sendserver($bulkPort); $sb2 = sendserver($bulk2);
$fcRun = function (string $caps, int $port, callable $script) {
    $c = new MemCarrier(); $m = new Mux($c); $m->resolver = fn(string $h) => [$h];
    $n = 0;
    $m->onTick = function (Mux $mm) use (&$n, $c, $port, $caps, $script) { $n++; if ($n === 1) { $c->feed(Mux::OPEN, 1, "127.0.0.1:$port" . $caps); } return $script($n, $c, $mm); };
    $m->run(30);
    return $c;
};
$got = fn(MemCarrier $c) => $c->dataBytes[1] ?? 0;
$okPayload = fn(MemCarrier $c) => (function () use ($c) { foreach ($c->out as $o) { if ($o[0] === Mux::OPEN_OK && $o[1] === 1) { return $o[2]; } } return null; })();

$noAck = $fcRun("\0fc", $bulkPort, fn($n, $c, $mm) => $n < 4);
$check('OPEN with the fc suffix is answered OPEN_OK "fc"', $okPayload($noAck) === 'fc');
$check('without acks the exit stops at about one window', $got($noAck) >= 262144 && $got($noAck) <= 262144 + 2 * 65536);

$acked = $fcRun("\0fc", $bulkPort, function ($n, $c, $mm) {
    if ($n >= 2 && $n <= 6) {                       // the client consumes what it got, and says so
        $have = $c->dataBytes[1] ?? 0;
        $c->feed(Mux::ACK, 1, pack('N', $have & 0xFFFFFFFF));
    }
    return $n < 7;
});
$check('acks let the stream run past a window', $got($acked) > 2 * 262144);

$old = $fcRun('', $bulkPort + 1000, fn($n, $c, $mm) => $n < 4);
$check('a client that did not ask gets OPEN_OK with no caps', $okPayload($old) === '');
$check('and no windows: data flows far past one window without acks', $got($old) > 3 * 262144);

// the exit acknowledges what it writes to destinations (running total, so a lost ack is repaired by the next)
$upAck = $fcRun("\0fc", $fast, function ($n, $c, $mm) {
    if ($n === 2) { foreach (str_split(str_repeat('u', 100000), 25000) as $part) { $c->feed(Mux::DATA, 1, $part); } }
    return $n < 5;
});
$total = 0; foreach ($upAck->out as $o) { if ($o[0] === Mux::ACK && $o[1] === 1) { $total = unpack('N', $o[2])[1]; } }
$check('the exit acks the bytes it wrote (running total = 100000)', $total === 100000);
foreach ([$sb1, $sb2] as $p) { proc_terminate($p); }

foreach ([$s1, $s2, $s3] as $p) { proc_terminate($p); }
echo $ok ? "MUX TEST PASS\n" : "MUX TEST FAIL\n" . implode("\n", array_slice($logs, -15)) . "\n";
exit($ok ? 0 : 1);
