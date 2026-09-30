<?php
// Test helper: writes endless data to whoever connects (a bulk download). php sendserver.php <port>
[$_, $port] = $argv;
$srv = stream_socket_server("tcp://127.0.0.1:$port", $e, $es) or exit(1);
echo "ready\n";
$chunk = str_repeat('x', 65536);
while ($c = @stream_socket_accept($srv, 30)) {
    $until = microtime(true) + 6;
    while (microtime(true) < $until && @fwrite($c, $chunk) !== false) { }
    @fclose($c);
}
