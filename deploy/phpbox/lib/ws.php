<?php
// Minimal WebSocket client (RFC 6455) shared by the phpbox carriers. It does
// the HTTP upgrade, masks client frames, and auto-pongs WS-level pings; the
// app-level protocol (Centrifuge, Engine.IO) lives in each carrier.

require_once __DIR__ . '/util.php';

final class WsClient
{
    /** @var resource|null */
    public $sock = null;

    /** connect performs the upgrade to wsURL (ws:// or wss://). */
    public function connect(string $wsURL, string $origin, string $cookieHeader): bool
    {
        $this->closeInfo = '';
        $p = parse_url($wsURL);
        $secure = ($p['scheme'] ?? 'wss') === 'wss';
        $host = $p['host'];
        $port = $p['port'] ?? ($secure ? 443 : 80);
        $path = ($p['path'] ?? '/') . (isset($p['query']) ? '?' . $p['query'] : '');

        $s = @stream_socket_client(($secure ? 'ssl' : 'tcp') . "://$host:$port", $e, $es, 12, STREAM_CLIENT_CONNECT);
        if (!$s) {
            return false;
        }
        $key = base64_encode(random_bytes(16));
        $req = "GET $path HTTP/1.1\r\nHost: $host\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
             . "Sec-WebSocket-Key: $key\r\nSec-WebSocket-Version: 13\r\nUser-Agent: " . PhpboxUtil::UA . "\r\n"
             . "Origin: $origin\r\n"
             . ($cookieHeader !== '' ? "Cookie: $cookieHeader\r\n" : '')
             . "\r\n";
        fwrite($s, $req);
        $resp = '';
        while (!feof($s)) {
            $resp .= fgets($s);
            if (str_contains($resp, "\r\n\r\n")) {
                break;
            }
        }
        if (!str_contains($resp, ' 101 ')) {
            return false;
        }
        $this->sock = $s;
        $this->closed = false;
        $this->rx = $this->frag = '';
        $this->q = [];
        return true;
    }

    public function writeText(string $payload): void
    {
        $this->writeFrame(0x1, $payload);
    }

    private function writeFrame(int $opcode, string $payload): void
    {
        if (!$this->sock) {
            return;
        }
        $len = strlen($payload);
        $frame = chr(0x80 | $opcode);
        if ($len < 126) {
            $frame .= chr(0x80 | $len);
        } elseif ($len < 65536) {
            $frame .= chr(0x80 | 126) . pack('n', $len);
        } else {
            $frame .= chr(0x80 | 127) . pack('J', $len);
        }
        $mask = random_bytes(4);
        // PHP XORs strings byte by byte up to the shorter one: no per-byte loop.
        $frame .= $mask . ($payload ^ str_repeat($mask, intdiv($len, 4) + 1));
        @fwrite($this->sock, $frame);
    }

    // ---- reading ---------------------------------------------------------
    // Bytes are buffered and whole frames parsed out of the buffer, so a frame that
    // arrives in pieces (a 22 KB message on a slow link) is never cut short, which
    // used to desynchronise the stream and break TLS inside the tunnel. Fragmented
    // messages are reassembled. "Closed" is a state ($closed), not a timeout.

    /** True once the server closed the socket, sent a close frame, or the link broke. */
    public bool $closed = false;

    private string $rx = '';
    private array  $q = [];          // complete messages not yet handed out
    private string $frag = '';       // a fragmented message being assembled

    /** More is ready without waiting on the network: a parsed message, or bytes already inside PHP's stream buffer
     *  (stream_select cannot see those, so a reader must keep reading while this is true). */
    public function pending(): bool
    {
        if ($this->q) {
            return true;
        }
        if (!$this->sock) {
            return false;
        }
        $m = stream_get_meta_data($this->sock);
        return ($m['unread_bytes'] ?? 0) > 0;
    }

    /**
     * readFrame returns the next message: its text; '' when bytes were consumed but no whole message is
     * ready yet (or a control frame was handled); null when nothing came within $timeout or the link is
     * closed (check $closed).
     */
    public function readFrame(float $timeout): ?string
    {
        if ($this->q) {
            return array_shift($this->q);
        }
        if (!$this->sock) {
            return null;
        }
        if (!$this->pending()) {
            $r = [$this->sock]; $w = $e = null;
            if (!@stream_select($r, $w, $e, (int)$timeout, (int)(($timeout - (int)$timeout) * 1e6))) {
                return null;
            }
        }
        $chunk = @fread($this->sock, 65536);
        if ($chunk === false || ($chunk === '' && feof($this->sock))) {
            $this->markClosed();
            return null;
        }
        if ($chunk === '') {
            return null;
        }
        $this->rx .= $chunk;
        $this->parse();
        if ($this->q) {
            return array_shift($this->q);
        }
        return $this->closed ? null : '';
    }

    private function parse(): void
    {
        while (strlen($this->rx) >= 2) {
            $b0 = ord($this->rx[0]);
            $b1 = ord($this->rx[1]);
            $len = $b1 & 0x7f;
            $off = 2;
            if ($len === 126) {
                if (strlen($this->rx) < 4) { return; }
                $len = unpack('n', substr($this->rx, 2, 2))[1];
                $off = 4;
            } elseif ($len === 127) {
                if (strlen($this->rx) < 10) { return; }
                $len = unpack('J', substr($this->rx, 2, 8))[1];
                $off = 10;
            }
            $masked = ($b1 & 0x80) !== 0;
            if ($masked) { $off += 4; }
            if ($len > 67108864) {                       // 64 MB: not a real message
                $this->markClosed();
                return;
            }
            if (strlen($this->rx) < $off + $len) {
                return;                                  // the rest of the frame is still on its way
            }
            $data = substr($this->rx, $off, $len);
            if ($masked) {
                $mask = substr($this->rx, $off - 4, 4);
                $data = $data ^ str_repeat($mask, intdiv($len, 4) + 1);
            }
            $this->rx = substr($this->rx, $off + $len);
            $opcode = $b0 & 0x0f;
            $fin = ($b0 & 0x80) !== 0;
            if ($opcode === 0x8) {                       // close: keep the server's code and reason for the log
                $this->closeInfo = strlen($data) >= 2 ? 'close ' . unpack('n', $data)[1] . (strlen($data) > 2 ? ' ' . substr($data, 2) : '') : 'close (no code)';
                $this->markClosed();
                return;
            }
            if ($opcode === 0x9) {                       // ping -> pong
                $this->writeFrame(0xA, $data);
                continue;
            }
            if ($opcode === 0xA) {                       // pong
                continue;
            }
            if ($opcode !== 0x0) {                       // text/binary starts a message
                $this->frag = '';
            }
            $this->frag .= $data;
            if ($fin) {
                $this->q[] = $this->frag;
                $this->frag = '';
            }
        }
    }

    /** Why the link ended, as far as the server said: a close frame's code and reason, or how the socket ended. */
    public string $closeInfo = '';

    private function markClosed(): void
    {
        if ($this->closeInfo === '') { $this->closeInfo = 'socket ended without a close frame'; }
        $this->closed = true;
        $this->close();
    }

    public function close(): void
    {
        if ($this->sock) {
            @fclose($this->sock);
            $this->sock = null;
        }
        $this->rx = '';
        $this->frag = '';
    }
}
