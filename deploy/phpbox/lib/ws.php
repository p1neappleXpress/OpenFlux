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
        $frame .= $mask;
        for ($i = 0; $i < $len; $i++) {
            $frame .= $payload[$i] ^ $mask[$i % 4];
        }
        @fwrite($this->sock, $frame);
    }

    /**
     * readFrame reads one message. Returns the text payload, '' for a handled
     * control frame (WS ping is auto-ponged), or null on close/timeout.
     */
    public function readFrame(float $timeout): ?string
    {
        if (!$this->sock) {
            return null;
        }
        stream_set_timeout($this->sock, (int)$timeout, (int)(($timeout - (int)$timeout) * 1e6));
        $b0 = @fread($this->sock, 1);
        if ($b0 === '' || $b0 === false) {
            return null;
        }
        $b1 = @fread($this->sock, 1);
        if ($b1 === '' || $b1 === false) {
            return null;
        }
        $opcode = ord($b0) & 0x0f;
        $len = ord($b1) & 0x7f;
        $masked = (ord($b1) & 0x80) !== 0;
        if ($len === 126) {
            $len = unpack('n', $this->readN(2))[1];
        } elseif ($len === 127) {
            $len = unpack('J', $this->readN(8))[1];
        }
        $mask = $masked ? $this->readN(4) : '';
        $data = $this->readN($len);
        if ($masked && strlen($mask) === 4) {
            for ($i = 0; $i < strlen($data); $i++) {
                $data[$i] = $data[$i] ^ $mask[$i % 4];
            }
        }
        if ($opcode === 0x8) {
            return null;                       // close
        }
        if ($opcode === 0x9) {
            $this->writeFrame(0xA, $data);     // ping -> pong
            return '';
        }
        if ($opcode === 0xA) {
            return '';                         // pong
        }
        return $data;                          // text / binary / continuation
    }

    private function readN(int $n): string
    {
        $data = '';
        while (strlen($data) < $n) {
            $chunk = @fread($this->sock, $n - strlen($data));
            if ($chunk === '' || $chunk === false) {
                break;
            }
            $data .= $chunk;
        }
        return $data;
    }

    public function close(): void
    {
        if ($this->sock) {
            @fclose($this->sock);
            $this->sock = null;
        }
    }
}
