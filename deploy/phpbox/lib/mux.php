<?php
// The carrier-agnostic phpbox stream mux, shared by every exit. A Carrier
// moves opaque "packets" (each one mux frame) over some link (cups, mailru);
// the Mux turns them into OPEN/DATA/CLOSE streams and dials the destinations.
//
// Everything runs in one non-blocking select loop (no fork, no threads: the
// free hosts forbid them):
//   - dials are asynchronous, so one slow destination never stalls the others;
//   - writes to a destination are queued when its socket is full (a partial
//     fwrite used to drop the rest of the frame);
//   - the frame buffer is consumed by offset, not re-copied on every frame.
// Hooks ($log, $onTick) let the node report what happens without the mux
// knowing about files or pages.

require_once __DIR__ . '/util.php';

interface Carrier
{
    public function setMux(Mux $m): void;
    public function connect(): bool;            // auth + join; false on failure
    public function sockets(): array;           // read sockets to select on (its WS)
    public function onReadable($sock): void;    // decode link -> $mux->onPacket(pkt) per packet
    public function sendPacket(string $frame): void; // encode + send one mux frame
}

final class Mux
{
    const OPEN = 1, DATA = 2, CLOSE = 3, OPEN_OK = 4, OPEN_ERR = 5;
    const ATTEMPT_TO = 3.0;        // seconds to wait for one address of a destination to accept
    const MAX_TRIES  = 3;          // addresses tried before the client is told the open failed
    // Bytes read from a destination per frame. Measured over Mail.ru: 64 KB frames move 3 parallel 4 MB downloads in
    // 22 s where 16 KB frames need 41 s - the carrier's cost is per message, not per byte. Tunable with $readChunk.
    const READ_CHUNK = 65536;
    const MAX_WBUF   = 8388608;    // queued bytes per stream before it is cut

    /** @var null|callable(string,string):void  fn($level, $message): error|warn|info|debug */
    public $log = null;
    /** @var null|callable(Mux):mixed  called about once a second; false stops the run */
    public $onTick = null;
    /** Destination names are sensitive (like the core's --sensitive): off hides the host. */
    public bool $sensitive = false;
    /** @var null|callable(string):array  fn($host): addresses - lets tests steer the dial; default is DNS */
    public $resolver = null;

    /** false = draining: keep serving the streams we have, ignore new OPENs (a newer generation owns them). */
    /** bytes read from a destination per DATA frame: bigger frames cost fewer carrier messages */
    public int $readChunk = self::READ_CHUNK;
    /** seconds a stream may carry no data either way before the exit closes it (its CLOSE can be lost on the way); 0 = never */
    public int $idleTimeout = 300;
    public bool $accepting = true;
    /** @var null|callable(int):bool  fn($sid): during a handover both generations see the same OPEN; true = this one owns it */
    public $claim = null;

    /** streams opened / closed / failed to open, bytes client->dst and dst->client */
    public array $stats = ['opened' => 0, 'closed' => 0, 'failed' => 0, 'up' => 0, 'down' => 0];

    private string $muxBuf = '';
    private int    $muxOff = 0;
    private array  $socks   = [];  // stream_id => connected destination socket
    private array  $pending = [];  // stream_id => ['s' => socket|null, 'until' => float, 'ips' => untried addresses, 'tries', 'port', 'label', 't0']
    private array  $wbuf    = [];  // stream_id => bytes waiting for the socket to accept them
    private array  $act     = [];  // stream_id => last time data moved on it

    public function __construct(private Carrier $c)
    {
        $c->setMux($this);
    }

    public function activeStreams(): int
    {
        return count($this->socks) + count($this->pending);
    }

    private function say(string $lvl, string $msg): void
    {
        if ($this->log) {
            ($this->log)($lvl, $msg);
        }
    }

    private float $nextReconnect = 0.0;
    private int   $reconnectFails = 0;

    /** The carrier's socket is gone (the server closed it, or the link broke): join again, with backoff. */
    private function reconnect(): void
    {
        $this->say('warn', 'link to the carrier is down; reconnecting' . ($this->reconnectFails ? " (attempt " . ($this->reconnectFails + 1) . ')' : ''));
        $ok = false;
        try {
            $ok = $this->c->connect();
        } catch (Throwable $e) {
            $this->say('warn', 'reconnect failed: ' . $e->getMessage());
        }
        if ($ok) {
            $this->reconnectFails = 0;
            $this->nextReconnect = 0.0;
            $this->stats['reconnects'] = ($this->stats['reconnects'] ?? 0) + 1;
            $this->say('info', 'reconnected to the carrier (frames in flight were lost; open streams may stall)');
        } else {
            $this->reconnectFails++;
            $this->nextReconnect = microtime(true) + min(10.0, 0.5 * (2 ** min($this->reconnectFails, 5)));
        }
    }

    /** run holds the exit in the room for up to $cap seconds, pumping both ways. */
    public function run(int $cap): string
    {
        $start = microtime(true);
        $nextTick = 0.0;
        $lastStats = $this->stats;
        $lastStatsAt = $start;
        $reason = 'cap';
        while (true) {
            $now = microtime(true);
            if ($now - $start >= $cap) {
                break;
            }
            if ($now >= $nextTick) {
                $nextTick = $now + 1.0;
                $this->expirePending($now);
                $this->reapIdle($now);
                if ($this->onTick && ($this->onTick)($this) === false) {
                    $reason = 'stopped';
                    break;
                }
                if ($now - $lastStatsAt >= 5.0) {
                    $dUp = $this->stats['up'] - $lastStats['up'];
                    $dDown = $this->stats['down'] - $lastStats['down'];
                    if ($dUp || $dDown || $this->activeStreams()) {
                        $this->say('debug', sprintf('traffic %ds: %d streams, up %s, down %s',
                            (int)($now - $lastStatsAt), $this->activeStreams(), self::human($dUp), self::human($dDown)));
                    }
                    $lastStats = $this->stats;
                    $lastStatsAt = $now;
                }
            }
            $carrierSocks = $this->c->sockets();
            if (!$carrierSocks && microtime(true) >= $this->nextReconnect) {
                $this->reconnect();                  // the link to the room/document is gone: join again
                $carrierSocks = $this->c->sockets();
            }
            $read = array_merge($carrierSocks, array_values($this->socks));
            $write = [];
            foreach ($this->pending as $p) {
                if ($p['s']) { $write[] = $p['s']; }
            }
            foreach (array_keys($this->wbuf) as $sid) {
                if (isset($this->socks[$sid])) {
                    $write[] = $this->socks[$sid];
                }
            }
            if (!$read && !$write) {
                usleep(100000);
                continue;
            }
            $e = null;
            if (@stream_select($read, $write, $e, 0, 200000) > 0) {
                foreach ($write as $s) {
                    $this->onDstWritable($s);
                }
                foreach ($read as $s) {
                    if (in_array($s, $carrierSocks, true)) {
                        $this->c->onReadable($s);
                    } else {
                        $this->onDstReadable($s);
                    }
                }
            }
        }
        // Tell the client which streams end with us, so it reconnects at once instead of waiting for a timeout.
        foreach (array_keys($this->socks + $this->pending) as $sid) {
            $this->sendFrame(self::CLOSE, $sid, '');
        }
        foreach ($this->socks as $s) {
            @fclose($s);
        }
        foreach ($this->pending as $p) {
            if ($p['s']) { @fclose($p['s']); }
        }
        $this->socks = $this->pending = $this->wbuf = [];
        return $reason;
    }

    /** onPacket takes reassembled link bytes and drains whole mux frames. */
    public function onPacket(string $pkt): void
    {
        $this->muxBuf .= $pkt;
        $n = strlen($this->muxBuf);
        while ($n - $this->muxOff >= 9) {
            $h = unpack('Ctype/Nsid/Nlen', substr($this->muxBuf, $this->muxOff, 9));
            if ($n - $this->muxOff < 9 + $h['len']) {
                break;
            }
            $payload = substr($this->muxBuf, $this->muxOff + 9, $h['len']);
            $this->muxOff += 9 + $h['len'];
            $this->applyMux($h['type'], $h['sid'], $payload);
        }
        if ($this->muxOff > 0) {                     // compact once per packet, not once per frame
            $this->muxBuf = $this->muxOff >= $n ? '' : substr($this->muxBuf, $this->muxOff);
            $this->muxOff = 0;
        }
    }

    public function sendFrame(int $type, int $sid, string $payload): void
    {
        $this->c->sendPacket(pack('CNN', $type, $sid, strlen($payload)) . $payload);
    }

    private function label(string $host, int $port): string
    {
        return $this->sensitive ? "$host:$port" : "*:$port";
    }

    private function applyMux(int $type, int $sid, string $payload): void
    {
        if ($type === self::OPEN) {
            if (!$this->accepting) {
                $this->say('debug', "stream $sid left to the newer generation");
                return;
            }
            if ($this->claim && !($this->claim)($sid)) {
                $this->say('debug', "stream $sid taken by the other generation");
                return;
            }
            [$host, $port] = array_pad(explode(':', $payload, 2), 2, '');
            $port  = (int)$port;
            $label = $this->label($host, $port);
            if (isset($this->socks[$sid]) || isset($this->pending[$sid])) {
                $this->closeStream($sid, false);     // the client reuses an id: the old one is gone
            }
            if (!in_array($port, [80, 443], true) && !PhpboxUtil::testMode()) {
                $this->say('debug', "stream $sid refused $label: port");
                $this->stats['failed']++;
                $this->sendFrame(self::OPEN_ERR, $sid, 'port');
                return;
            }
            $ips = $this->resolver ? array_values(($this->resolver)($host)) : PhpboxUtil::resolveAll($host);
            if (!$ips || PhpboxUtil::isPrivate($host)) {
                $this->say('debug', "stream $sid refused $label: " . (!$ips ? 'no such host' : 'blocked'));
                $this->stats['failed']++;
                $this->sendFrame(self::OPEN_ERR, $sid, !$ips ? 'dns' : 'blocked');
                return;
            }
            $this->pending[$sid] = ['s' => null, 'ips' => $ips, 'port' => $port, 'tries' => 0, 'label' => $label, 't0' => microtime(true)];
            $this->dial($sid);
        } elseif ($type === self::DATA) {
            if (isset($this->socks[$sid])) {
                $this->write($sid, $payload);
            } elseif (isset($this->pending[$sid])) {
                $this->stats['up'] += strlen($payload);
                $this->wbuf[$sid] = ($this->wbuf[$sid] ?? '') . $payload;   // sent when the dial completes
            }
        } elseif ($type === self::CLOSE) {
            if (isset($this->socks[$sid]) || isset($this->pending[$sid])) {
                $this->closeStream($sid, false);
            }
        }
    }

    /** Start (or restart on the next address) the connect for a pending stream. */
    private function dial(int $sid): void
    {
        $p = &$this->pending[$sid];
        while ($p['ips']) {
            $ip = array_shift($p['ips']);
            $p['tries']++;
            $addr = str_contains($ip, ':') ? "[$ip]" : $ip;
            $s = @stream_socket_client("tcp://$addr:{$p['port']}", $en, $es, 0, STREAM_CLIENT_CONNECT | STREAM_CLIENT_ASYNC_CONNECT);
            if ($s) {
                stream_set_blocking($s, false);
                $p['s'] = $s;
                $p['until'] = microtime(true) + self::ATTEMPT_TO;
                $this->say('debug', "stream $sid dialing {$p['label']}" . ($p['tries'] > 1 ? " (address {$p['tries']})" : ''));
                return;
            }
            $this->say('debug', "stream $sid dial {$p['label']} failed at once: $es");
            if ($p['tries'] >= self::MAX_TRIES) { break; }
        }
        $label = $p['label'];
        unset($p);
        unset($this->pending[$sid], $this->wbuf[$sid]);
        $this->stats['failed']++;
        $this->sendFrame(self::OPEN_ERR, $sid, 'refused');
    }

    /** The current address did not work: try the next one, or tell the client the open failed. */
    private function redial(int $sid, string $why): void
    {
        $p = $this->pending[$sid];
        if ($p['s']) { @fclose($p['s']); }
        $this->pending[$sid]['s'] = null;
        if ($p['ips'] && $p['tries'] < self::MAX_TRIES) {
            $this->say('debug', "stream $sid {$p['label']} $why; trying the next address");
            $this->dial($sid);
            return;
        }
        unset($this->pending[$sid], $this->wbuf[$sid]);
        $this->stats['failed']++;
        $this->say('debug', "stream $sid dial {$p['label']} $why");
        $this->sendFrame(self::OPEN_ERR, $sid, $why === 'timed out' ? 'timeout' : 'refused');
    }

    /** Try to hand bytes to the destination now; queue what does not fit. */
    private function write(int $sid, string $data): void
    {
        $this->stats['up'] += strlen($data);
        $this->act[$sid] = microtime(true);
        if (isset($this->wbuf[$sid])) {
            $this->wbuf[$sid] .= $data;              // keep order behind what is already queued
        } else {
            $n = @fwrite($this->socks[$sid], $data);
            if ($n === false) {
                $this->closeStream($sid, true);
                return;
            }
            if ($n < strlen($data)) {
                $this->wbuf[$sid] = substr($data, $n);
            }
        }
        if (isset($this->wbuf[$sid]) && strlen($this->wbuf[$sid]) > self::MAX_WBUF) {
            $this->say('warn', "stream $sid cut: destination not accepting data (" . self::human(strlen($this->wbuf[$sid])) . ' queued)');
            $this->closeStream($sid, true);
        }
    }

    private function onDstWritable($s): void
    {
        $sid = false;
        foreach ($this->pending as $id => $p) {
            if ($p['s'] === $s) { $sid = $id; break; }
        }
        if ($sid !== false) {                        // a dial finished: connected or refused
            $p = $this->pending[$sid];
            if (@stream_socket_get_name($s, true) === false) {
                $this->redial($sid, 'refused');
                return;
            }
            unset($this->pending[$sid]);
            $this->socks[$sid] = $s;
            $this->act[$sid] = microtime(true);
            $this->stats['opened']++;
            $this->say('debug', sprintf('stream %d open %s (%d ms)', $sid, $p['label'], (int)((microtime(true) - $p['t0']) * 1000)));
            $this->sendFrame(self::OPEN_OK, $sid, '');
        } else {
            $sid = array_search($s, $this->socks, true);
            if ($sid === false) { return; }
        }
        if (isset($this->wbuf[$sid])) {              // flush what was queued
            $n = @fwrite($s, $this->wbuf[$sid]);
            if ($n === false) {
                $this->closeStream($sid, true);
            } elseif ($n >= strlen($this->wbuf[$sid])) {
                unset($this->wbuf[$sid]);
            } elseif ($n > 0) {
                $this->wbuf[$sid] = substr($this->wbuf[$sid], $n);
            }
        }
    }

    private function onDstReadable($s): void
    {
        $sid = array_search($s, $this->socks, true);
        if ($sid === false) { return; }
        $d = @fread($s, $this->readChunk);
        if ($d === '' || $d === false) {
            if ($d === false || feof($s)) { $this->closeStream($sid, true); }
            return;
        }
        $this->stats['down'] += strlen($d);
        $this->act[$sid] = microtime(true);
        $this->sendFrame(self::DATA, $sid, $d);
    }

    /** Close streams that carried nothing for $idleTimeout s: a lost CLOSE must not leak a socket on the host. */
    private function reapIdle(float $now): void
    {
        if ($this->idleTimeout <= 0) {
            return;
        }
        foreach ($this->socks as $sid => $_) {
            if ($now - ($this->act[$sid] ?? $now) > $this->idleTimeout) {
                $this->say('debug', "stream $sid idle for {$this->idleTimeout}s: closing it");
                $this->closeStream($sid, true);
            }
        }
    }

    private function expirePending(float $now): void
    {
        foreach ($this->pending as $sid => $p) {
            if ($now >= ($p['until'] ?? 0)) {
                $this->redial($sid, 'timed out');
            }
        }
    }

    /** Close one stream; tell the client when the destination (not the client) ended it. */
    private function closeStream(int $sid, bool $notify): void
    {
        if (isset($this->socks[$sid])) {
            @fclose($this->socks[$sid]);
            $this->stats['closed']++;
        } elseif (isset($this->pending[$sid]) && $this->pending[$sid]['s']) {
            @fclose($this->pending[$sid]['s']);
        }
        unset($this->socks[$sid], $this->pending[$sid], $this->wbuf[$sid], $this->act[$sid]);
        if ($notify) {
            $this->sendFrame(self::CLOSE, $sid, '');
        }
        $this->say('debug', "stream $sid closed" . ($notify ? ' by destination' : ' by client'));
    }

    public static function human(int $b): string
    {
        if ($b < 1024) { return "$b B"; }
        if ($b < 1048576) { return sprintf('%.1f KB', $b / 1024); }
        return sprintf('%.2f MB', $b / 1048576);
    }
}
