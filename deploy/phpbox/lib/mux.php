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
    const DIAL_TO    = 6;          // seconds to wait for a destination to accept
    const READ_CHUNK = 16384;
    const MAX_WBUF   = 8388608;    // queued bytes per stream before it is cut

    /** @var null|callable(string,string):void  fn($level, $message): error|warn|info|debug */
    public $log = null;
    /** @var null|callable(Mux):mixed  called about once a second; false stops the run */
    public $onTick = null;
    /** Destination names are sensitive (like the core's --sensitive): off hides the host. */
    public bool $sensitive = false;

    /** streams opened / closed / failed to open, bytes client->dst and dst->client */
    public array $stats = ['opened' => 0, 'closed' => 0, 'failed' => 0, 'up' => 0, 'down' => 0];

    private string $muxBuf = '';
    private int    $muxOff = 0;
    private array  $socks   = [];  // stream_id => connected destination socket
    private array  $pending = [];  // stream_id => ['s' => socket, 'until' => float, 'label' => string]
    private array  $wbuf    = [];  // stream_id => bytes waiting for the socket to accept them

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
            $read = array_merge($carrierSocks, array_values($this->socks));
            $write = [];
            foreach ($this->pending as $p) {
                $write[] = $p['s'];
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
        foreach ($this->socks as $s) {
            @fclose($s);
        }
        foreach ($this->pending as $p) {
            @fclose($p['s']);
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
            $ip = PhpboxUtil::resolve($host);
            if ($ip === '' || PhpboxUtil::isPrivate($host)) {
                $this->say('debug', "stream $sid refused $label: " . ($ip === '' ? 'no such host' : 'blocked'));
                $this->stats['failed']++;
                $this->sendFrame(self::OPEN_ERR, $sid, $ip === '' ? 'dns' : 'blocked');
                return;
            }
            $addr = str_contains($ip, ':') ? "[$ip]" : $ip;
            $s = @stream_socket_client("tcp://$addr:$port", $en, $es, 0, STREAM_CLIENT_CONNECT | STREAM_CLIENT_ASYNC_CONNECT);
            if (!$s) {
                $this->say('debug', "stream $sid dial $label failed: $es");
                $this->stats['failed']++;
                $this->sendFrame(self::OPEN_ERR, $sid, (string)$es);
                return;
            }
            stream_set_blocking($s, false);
            $this->pending[$sid] = ['s' => $s, 'until' => microtime(true) + self::DIAL_TO, 'label' => $label, 't0' => microtime(true)];
            $this->say('debug', "stream $sid dialing $label");
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

    /** Try to hand bytes to the destination now; queue what does not fit. */
    private function write(int $sid, string $data): void
    {
        $this->stats['up'] += strlen($data);
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
            unset($this->pending[$sid]);
            if (@stream_socket_get_name($s, true) === false) {
                @fclose($s);
                unset($this->wbuf[$sid]);
                $this->stats['failed']++;
                $this->say('debug', "stream $sid dial {$p['label']} refused");
                $this->sendFrame(self::OPEN_ERR, $sid, 'refused');
                return;
            }
            $this->socks[$sid] = $s;
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
        $d = @fread($s, self::READ_CHUNK);
        if ($d === '' || $d === false) {
            if ($d === false || feof($s)) { $this->closeStream($sid, true); }
            return;
        }
        $this->stats['down'] += strlen($d);
        $this->sendFrame(self::DATA, $sid, $d);
    }

    private function expirePending(float $now): void
    {
        foreach ($this->pending as $sid => $p) {
            if ($now >= $p['until']) {
                @fclose($p['s']);
                unset($this->pending[$sid], $this->wbuf[$sid]);
                $this->stats['failed']++;
                $this->say('debug', "stream $sid dial {$p['label']} timed out");
                $this->sendFrame(self::OPEN_ERR, $sid, 'timeout');
            }
        }
    }

    /** Close one stream; tell the client when the destination (not the client) ended it. */
    private function closeStream(int $sid, bool $notify): void
    {
        if (isset($this->socks[$sid])) {
            @fclose($this->socks[$sid]);
            $this->stats['closed']++;
        } elseif (isset($this->pending[$sid])) {
            @fclose($this->pending[$sid]['s']);
        }
        unset($this->socks[$sid], $this->pending[$sid], $this->wbuf[$sid]);
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
