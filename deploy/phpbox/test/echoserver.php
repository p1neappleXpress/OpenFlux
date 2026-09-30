<?php
// Test helper: a TCP echo server. php echoserver.php <port> [delay_seconds_before_reading]
[$_, $port, $delay] = array_pad($argv, 3, 0);
$srv = stream_socket_server("tcp://127.0.0.1:$port", $e, $es) or exit(1);
echo "ready\n";
while ($c = @stream_socket_accept($srv, 30)) {
    if ($delay > 0) { sleep((int)$delay); }
    while (($d = fread($c, 65536)) !== '' && $d !== false) { fwrite($c, $d); }
    fclose($c);
}
