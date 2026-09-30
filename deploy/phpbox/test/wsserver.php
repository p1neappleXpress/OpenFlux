<?php
// Test helper: a WebSocket server that misbehaves the way real links do.
//   php wsserver.php <port>
[$_, $port] = $argv;
$srv = stream_socket_server("tcp://127.0.0.1:$port", $e, $es) or exit(1);
echo "ready\n";
$c = stream_socket_accept($srv, 20);
$req = '';
while (!str_contains($req, "\r\n\r\n")) { $req .= fread($c, 1024); }
preg_match('/Sec-WebSocket-Key: (\S+)/i', $req, $m);
$accept = base64_encode(sha1($m[1] . '258EAFA5-E914-47DA-95CA-C5AB0DC85B11', true));
fwrite($c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: $accept\r\n\r\n");

function frame(string $payload, int $op = 1, bool $fin = true): string {
    $len = strlen($payload);
    $h = chr(($fin ? 0x80 : 0) | $op);
    if ($len < 126) { $h .= chr($len); } elseif ($len < 65536) { $h .= chr(126) . pack('n', $len); } else { $h .= chr(127) . pack('J', $len); }
    return $h . $payload;
}
stream_set_blocking($c, true);
// 1. a small message, and right behind it (same write) a second one: both arrive in one TCP chunk
fwrite($c, frame('first') . frame('second'));
usleep(300000);
// 2. a 60 KB message delivered in three slow pieces (longer than the client's read timeout)
$big = str_repeat('0123456789abcdef', 3840);
$f = frame($big);
foreach ([substr($f, 0, 1000), substr($f, 1000, 30000), substr($f, 31000)] as $piece) { fwrite($c, $piece); usleep(450000); }
// 3. a message fragmented in three frames, with a ping in the middle
fwrite($c, frame('frag-', 1, false) . frame('ment-', 0, false));
usleep(100000);
fwrite($c, frame('ping!', 9));
fwrite($c, frame('ed', 0, true));
usleep(300000);
// 4. a header split across two writes
$f = frame('split-header');
fwrite($c, substr($f, 0, 1)); usleep(200000); fwrite($c, substr($f, 1));
usleep(300000);
// 5. close
fwrite($c, frame('', 8));
usleep(300000);
fclose($c);
