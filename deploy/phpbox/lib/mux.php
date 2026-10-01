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
// Flow control: a client that says so in OPEN ("host:port\0fc") gets windows. The exit
// reads a destination only while that stream (and all streams together) have less than a
// window of DATA unacknowledged, and acknowledges what it wrote to destinations. Without
// it a saturated carrier's queue (a document server's) grows by megabytes and every small
// frame - a TLS handshake, a CLOSE - waits behind it: downloads starved new connections.
// Acks carry running totals (uint32), so a lost one is repaired by the next.
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

/**
 * A carrier whose server takes one message per interval (cups.online throttles cursor updates). The mux paces it
 * from its own loop instead of the carrier sleeping between messages: a sleep inside the loop stalled every stream
 * (a 64 KB frame was ~22 messages, ~0.4 s asleep), so under a download new connections timed out and acks waited.
 * A message carries whole packets only (2-byte length + one frame each, several per message): two senders in one
 * room - generations handing over - then never cut into each other's packets on the receiving side.
 */
interface PacedCarrier extends Carrier
{
    public function msgCap(): int;                       // bytes of packets one message holds
    public function interval(): float;                   // seconds between messages, at least
    public function rate(): int;                         // packet bytes per second the server forwards (0 = no limit)
    public function unconfirmed(): int;                  // messages sent that the server has not shown back yet
    public function confirmAll(): void;                  // forget them (they will not be shown back)
    public function sendPackets(string $packets): void;  // send one message now (no waiting)
}

final class Mux
{
    const OPEN = 1, DATA = 2, CLOSE = 3, OPEN_OK = 4, OPEN_ERR = 5, ACK = 6;
    const MASK = 0xFFFFFFFF;
    const ACK_EVERY = 32768;          // ack after this many bytes were written to a destination (or when its queue empties)
    const STALL_AFTER = 20;           // seconds blocked on a window with no ack: assume the acks were lost
    const ATTEMPT_TO = 3.0;        // seconds to wait for one address of a destination to accept
    const MAX_TRIES  = 3;          // addresses tried before the client is told the open failed
    // Bytes read from a destination per frame. Measured over Mail.ru: 64 KB frames move 3 parallel 4 MB downloads in
    // 22 s where 16 KB frames need 41 s - the carrier's cost is per message, not per byte. Tunable with $readChunk.
    const READ_CHUNK = 65536;
    const MAX_WBUF   = 8388608;    // queued bytes per stream before it is cut
    // Paced carriers: DATA waiting for the carrier, per stream and in all, before the mux stops reading destinations.
    const OUT_STREAM_MAX = 49152;
    const OUT_ALL_MAX    = 262144;
    // Messages a paced carrier may have at the server unconfirmed. cups.online sends every member's cursors to every
    // member, the sender too: our own message coming back is the server saying it took it. Past a few unconfirmed,
    // the server is behind - and what it has queued for us (the client's OPENs, its pings) waits behind our echoes:
    // it ended the link with "no pong" under a download. Sending only as fast as it confirms keeps that queue short.
    const OUT_WINDOW     = 12;   // ~0.5 s of the server's work at its ~72 KB/s: enough to keep it busy
    const OUT_CONFIRM_TO = 3.0;    // seconds without a confirmation before the window is assumed lost (a reconnect)

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
    /** flow-control windows in bytes: per stream, and over all streams; 0 = never honour a client's request for them */
    public int $streamWindow = 262144;
    public int $totalWindow = 524288;
    /** seconds a stream may carry no data either way before the exit closes it (its CLOSE can be lost on the way); 0 = never */
    public int $idleTimeout = 300;
    public bool $accepting = true;
    /**
     * Bytes per second read from destinations, over all streams (0 = no limit). A generation sets it while its successor
     * is in the room: the document server sends every member's messages to every member, the successor gets ours too,
     * and at full speed its queue at the server grew by seconds - the client's OPENs to it waited behind our download.
     */
    public int $readRate = 0;
    private float $rateSec = 0.0;
    private int $rateUsed = 0;
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
    private array  $fc      = [];  // stream_id => ['sent','acked','wrote','ackedSent','since'] for streams with flow control
    private array  $ackDirty = []; // stream_id => true: bytes written since the last ack
    private int    $inAll   = 0;   // unacknowledged DATA over all streams

    // Outbox for a paced carrier: control frames (OPEN_OK, OPEN_ERR, ACK) go first; each stream's DATA and CLOSE keep
    // their order in that stream's queue, and streams take turns, so a new connection's handshake is not stuck behind
    // another stream's download.
    private bool   $paced    = false;
    private array  $ctlQ     = [];  // encoded control frames
    private array  $bulkQ    = [];  // stream_id => list of [type, payload]
    private array  $bulkLen  = [];  // stream_id => DATA bytes queued
    private int    $bulkAll  = 0;
    private int    $rrNext   = -1;  // the stream served last; the next message starts after it
    private float  $nextSlot = 0.0;
    private int    $lastUnconfirmed = 0;
    private float  $confirmSeen = 0.0;

    public function __construct(private Carrier $c)
    {
        $c->setMux($this);
        $this->paced = $c instanceof PacedCarrier;
    }

    /** Frames waiting for a paced carrier (0 for an unpaced one). */
    public function outboxBytes(): int
    {
        return $this->bulkAll + array_sum(array_map('strlen', $this->ctlQ));
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
        $why = method_exists($this->c, 'why') ? $this->c->why() : '';
        $this->say('warn', 'link to the carrier is down' . ($why !== '' ? " ($why)" : '') . '; reconnecting'
            . ($this->reconnectFails ? " (attempt " . ($this->reconnectFails + 1) . ')' : ''));
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
                            (int)($now - $lastStatsAt), $this->activeStreams(), self::human($dUp), self::human($dDown))
                            . ($this->paced ? sprintf(', waiting to send %s, unconfirmed %d', self::human($this->outboxBytes()), $this->c->unconfirmed()) : ''));
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
            $read = $carrierSocks;
            foreach ($this->socks as $sid => $sock) {
                if (!$this->blocked($sid, $now)) { $read[] = $sock; }
            }
            $write = [];
            foreach ($this->pending as $p) {
                if ($p['s']) { $write[] = $p['s']; }
            }
            foreach (array_keys($this->wbuf) as $sid) {
                if (isset($this->socks[$sid])) {
                    $write[] = $this->socks[$sid];
                }
            }
            $this->pump(microtime(true));
            $wait = 200000;
            if ($this->ctlQ || $this->bulkQ) {               // wake up for the next message slot
                $wait = max(1000, min($wait, (int)(($this->nextSlot - microtime(true)) * 1e6)));
            }
            if (!$read && !$write) {
                usleep(min($wait, 100000));
                continue;
            }
            $e = null;
            if (@stream_select($read, $write, $e, 0, $wait) > 0) {
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
            $this->flushAcks();
            $this->pump(microtime(true));
        }
        // Tell the client which streams end with us, so it reconnects at once instead of waiting for a timeout.
        foreach (array_keys($this->socks + $this->pending) as $sid) {
            $this->sendFrame(self::CLOSE, $sid, '');
        }
        $this->drainOutbox(5.0);
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
        if (!$this->paced) {
            $this->c->sendPacket(pack('CNN', $type, $sid, strlen($payload)) . $payload);
            return;
        }
        if ($type === self::DATA || $type === self::CLOSE) {
            $this->bulkQ[$sid][] = [$type, $payload];
            $this->bulkLen[$sid] = ($this->bulkLen[$sid] ?? 0) + strlen($payload);
            $this->bulkAll += strlen($payload);
        } else {
            $this->ctlQ[] = pack('CNN', $type, $sid, strlen($payload)) . $payload;
        }
    }

    /** Send the next message if its slot has come (paced carriers): one message per interval, never a sleep. */
    private function pump(float $now): void
    {
        if (!$this->paced || (!$this->ctlQ && !$this->bulkQ) || $now < $this->nextSlot) {
            return;
        }
        $u = $this->c->unconfirmed();
        if ($u < $this->lastUnconfirmed || $u === 0) { $this->confirmSeen = $now; }   // the server is taking them
        $this->lastUnconfirmed = $u;
        if ($u >= self::OUT_WINDOW) {
            if ($now - $this->confirmSeen < self::OUT_CONFIRM_TO) {
                return;                                  // wait for the server to catch up
            }
            $this->c->confirmAll();                      // none came back for long: they are not coming (a reconnect)
            $this->confirmSeen = $now;
        }
        $msg = $this->buildMessage($this->c->msgCap());
        if ($msg === '') {
            return;
        }
        $this->c->sendPackets($msg);
        $rate = $this->c->rate();
        $this->nextSlot = max($this->nextSlot, $now) + max($this->c->interval(), $rate > 0 ? strlen($msg) / $rate : 0.0);
    }

    /** Whole packets for one message: control frames first, then the streams in turn (DATA cut to fit). */
    private function buildMessage(int $cap): string
    {
        $buf = '';
        while ($this->ctlQ && strlen($buf) + 2 + strlen($this->ctlQ[0]) <= $cap) {
            $f = array_shift($this->ctlQ);
            $buf .= pack('n', strlen($f)) . $f;
        }
        $sids = array_keys($this->bulkQ);
        if (!$sids) {
            return $buf;
        }
        sort($sids);
        $start = 0;                                      // the first stream after the one served last
        foreach ($sids as $i => $sid) {
            if ($sid > $this->rrNext) { $start = $i; break; }
        }
        $order = array_merge(array_slice($sids, $start), array_slice($sids, 0, $start));
        foreach ($order as $sid) {
            while (isset($this->bulkQ[$sid])) {
                $room = $cap - strlen($buf) - 2 - 9;
                [$type, $payload] = $this->bulkQ[$sid][0];
                if ($type === self::CLOSE) {
                    if ($room < 0) { break 2; }
                    $buf .= pack('n', 9) . pack('CNN', self::CLOSE, $sid, 0);
                    $this->shiftBulk($sid);
                    continue;
                }
                if ($room < 256 && strlen($payload) > $room) { break 2; }   // too little left to be worth a cut
                $part = strlen($payload) > $room ? substr($payload, 0, $room) : $payload;
                $buf .= pack('n', 9 + strlen($part)) . pack('CNN', self::DATA, $sid, strlen($part)) . $part;
                $this->bulkLen[$sid] -= strlen($part);
                $this->bulkAll -= strlen($part);
                if (strlen($part) < strlen($payload)) {
                    $this->bulkQ[$sid][0][1] = substr($payload, strlen($part));
                    $this->rrNext = $sid;
                    break 2;                             // the message is full
                }
                $this->shiftBulk($sid);
            }
            $this->rrNext = $sid;
        }
        return $buf;
    }

    private function shiftBulk(int $sid): void
    {
        array_shift($this->bulkQ[$sid]);
        if (!$this->bulkQ[$sid]) {
            unset($this->bulkQ[$sid], $this->bulkLen[$sid]);
        }
    }

    /** At the end of a run: give what is queued a few seconds to go out. */
    private function drainOutbox(float $limit): void
    {
        $until = microtime(true) + $limit;
        while (($this->ctlQ || $this->bulkQ) && microtime(true) < $until) {
            $this->pump(microtime(true));
            usleep(max(1000, (int)(($this->nextSlot - microtime(true)) * 1e6)));
        }
    }

    private function label(string $host, int $port): string
    {
        return $this->sensitive ? "$host:$port" : "*:$port";
    }

    private function applyMux(int $type, int $sid, string $payload): void
    {
        if ($type === self::OPEN) {
            // The same OPEN again: the client did not hear our answer (lost while a carrier reconnected) and asks
            // again. Ids are never reused by a client, so this is that stream: answer again, do not dial twice.
            if (isset($this->socks[$sid])) {
                $this->sendFrame(self::OPEN_OK, $sid, isset($this->fc[$sid]) ? 'fc' : '');
                return;
            }
            if (isset($this->pending[$sid])) {
                return;                                // still dialing: the answer is on its way
            }
            if (!$this->accepting) {
                $this->say('debug', "stream $sid left to the newer generation");
                return;
            }
            if ($this->claim && !($this->claim)($sid)) {
                $this->say('debug', "stream $sid taken by the other generation");
                return;
            }
            [$target, $caps] = array_pad(explode("\0", $payload, 2), 2, '');
            [$host, $port] = array_pad(explode(':', $target, 2), 2, '');
            $port  = (int)$port;
            $wantFc = $this->streamWindow > 0 && in_array('fc', explode(',', $caps), true);
            $label = $this->label($host, $port);
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
            $this->pending[$sid] = ['s' => null, 'ips' => $ips, 'port' => $port, 'tries' => 0, 'label' => $label, 't0' => microtime(true), 'fc' => $wantFc];
            $this->dial($sid);
        } elseif ($type === self::DATA) {
            if (isset($this->socks[$sid])) {
                $this->write($sid, $payload);
            } elseif (isset($this->pending[$sid])) {
                $this->stats['up'] += strlen($payload);
                $this->wbuf[$sid] = ($this->wbuf[$sid] ?? '') . $payload;   // sent when the dial completes
            }
        } elseif ($type === self::ACK) {
            if (isset($this->fc[$sid]) && strlen($payload) === 4) {
                $v = unpack('N', $payload)[1];
                $f = &$this->fc[$sid];
                $delta = ($v - $f['acked']) & self::MASK;
                if ($delta !== 0 && $delta <= (($f['sent'] - $f['acked']) & self::MASK)) {   // never beyond what was sent
                    $f['acked'] = $v;
                    $f['since'] = 0.0;
                    $this->inAll -= $delta;
                }
                unset($f);
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
            $this->wrote($sid, $n);
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
            if (!empty($p['fc'])) {
                $this->fc[$sid] = ['sent' => 0, 'acked' => 0, 'wrote' => 0, 'ackedSent' => 0, 'since' => 0.0];
            }
            $this->stats['opened']++;
            $this->say('debug', sprintf('stream %d open %s (%d ms)', $sid, $p['label'], (int)((microtime(true) - $p['t0']) * 1000)));
            $this->sendFrame(self::OPEN_OK, $sid, isset($this->fc[$sid]) ? 'fc' : '');
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
                $this->wrote($sid, $n);
            } elseif ($n > 0) {
                $this->wbuf[$sid] = substr($this->wbuf[$sid], $n);
                $this->wrote($sid, $n);
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
        $this->rateUsed += strlen($d);
        $this->act[$sid] = microtime(true);
        if (isset($this->fc[$sid])) {
            $this->fc[$sid]['sent'] = ($this->fc[$sid]['sent'] + strlen($d)) & self::MASK;
            $this->inAll += strlen($d);
        }
        $this->sendFrame(self::DATA, $sid, $d);
    }

    /** True while a stream has a window of unacknowledged DATA out (or all streams together do): stop reading it. */
    private function blocked(int $sid, float $now): bool
    {
        if ($this->paced && (($this->bulkLen[$sid] ?? 0) >= self::OUT_STREAM_MAX || $this->bulkAll >= self::OUT_ALL_MAX)) {
            return true;                               // the carrier is behind: let it catch up before reading more
        }
        if ($this->readRate > 0) {
            if ($now - $this->rateSec >= 1.0) { $this->rateSec = $now; $this->rateUsed = 0; }
            if ($this->rateUsed >= $this->readRate) { return true; }
        }
        if (!isset($this->fc[$sid])) {
            return false;
        }
        $f = &$this->fc[$sid];
        $inflight = ($f['sent'] - $f['acked']) & self::MASK;
        if ($inflight === 0 || ($inflight < $this->streamWindow && $this->inAll < $this->totalWindow)) {
            $f['since'] = 0.0;
            return false;                          // a stream with nothing in flight may always read
        }
        if ($f['since'] === 0.0) {
            $f['since'] = $now;
        } elseif ($now - $f['since'] > self::STALL_AFTER) {
            $this->say('warn', "stream $sid: no ack for " . self::STALL_AFTER . "s with " . self::human($inflight) . ' in flight; assuming they were lost');
            $this->inAll -= $inflight;
            $f['acked'] = $f['sent'];
            $f['since'] = 0.0;
            return false;
        }
        return true;
    }

    /** $n more bytes reached a destination: owe the client an ack for them. */
    private function wrote(int $sid, int $n): void
    {
        if (isset($this->fc[$sid]) && $n > 0) {
            $this->fc[$sid]['wrote'] = ($this->fc[$sid]['wrote'] + $n) & self::MASK;
            $this->ackDirty[$sid] = true;
        }
    }

    /** Send owed acks: after a real chunk was written, or once a stream's write queue has emptied (keeps the client's pipe full). */
    private function flushAcks(): void
    {
        foreach ($this->ackDirty as $sid => $_) {
            if (!isset($this->fc[$sid])) {
                unset($this->ackDirty[$sid]);
                continue;
            }
            $f = &$this->fc[$sid];
            $due = ($f['wrote'] - $f['ackedSent']) & self::MASK;
            if ($due >= self::ACK_EVERY || ($due > 0 && !isset($this->wbuf[$sid]))) {
                $f['ackedSent'] = $f['wrote'];
                unset($this->ackDirty[$sid]);
                $this->sendFrame(self::ACK, $sid, pack('N', $f['wrote']));
            }
            unset($f);
        }
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
        if (isset($this->fc[$sid])) {
            $this->inAll -= ($this->fc[$sid]['sent'] - $this->fc[$sid]['acked']) & self::MASK;
            unset($this->fc[$sid], $this->ackDirty[$sid]);
        }
        unset($this->socks[$sid], $this->pending[$sid], $this->wbuf[$sid], $this->act[$sid]);
        if (!$notify && isset($this->bulkQ[$sid])) {     // the client is done with it: what we still hold is for no one
            $this->bulkAll -= $this->bulkLen[$sid] ?? 0;
            unset($this->bulkQ[$sid], $this->bulkLen[$sid]);
        }
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
