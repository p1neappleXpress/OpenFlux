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
    public function sendPacket(string $f): void
    {
        $h = unpack('Ctype/Nsid/Nlen', substr($f, 0, 9));
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

$fast = random_int(41000, 41999); $slow = $fast + 1000; $dead = $fast + 2000;
$s1 = server($fast, 0); $s2 = server($slow, 2);

$c = new MemCarrier();
$mux = new Mux($c);
$logs = [];
$mux->log = function ($l, $m) use (&$logs) { $logs[] = "$l $m"; };
$big = random_bytes(2 * 1024 * 1024);
$ok = true;
$check = function (string $name, bool $pass) use (&$ok) { printf("%-46s %s\n", $name, $pass ? 'OK' : 'FAIL'); $ok = $ok && $pass; };
$n = 0;
$mux->onTick = function (Mux $m) use (&$n, $c, $fast, $slow, $dead, $big, $check) {
    $n++;
    if ($n === 1) {
        $c->feed(Mux::OPEN, 1, "127.0.0.1:$fast");
        $c->feed(Mux::OPEN, 2, "127.0.0.1:$dead");             // nothing listens there
        $c->feed(Mux::OPEN, 3, "127.0.0.1:$slow");
        $c->feed(Mux::DATA, 1, 'hel');                          // DATA right behind OPEN, before the dial completes
        $c->feed(Mux::DATA, 1, 'lo');
    } elseif ($n === 2) {
        $check('async dial: OPEN_OK for a live server', $c->has(Mux::OPEN_OK, 1));
        $check('async dial: OPEN_ERR for a dead port', $c->has(Mux::OPEN_ERR, 2));
        $check('data sent during the dial arrives in order', $c->take(Mux::DATA, 1) === 'hello');
        $check('a slow dial does not block the others', $c->has(Mux::OPEN_OK, 3));
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
$check('stats count the failed open', $mux->stats['failed'] === 1);
$check('destinations are hidden from the log by default', !preg_grep('/127\.0\.0\.1/', $logs));
foreach ([$s1, $s2] as $p) { proc_terminate($p); }
echo $ok ? "MUX TEST PASS\n" : "MUX TEST FAIL\n" . implode("\n", array_slice($logs, -15)) . "\n";
exit($ok ? 0 : 1);
