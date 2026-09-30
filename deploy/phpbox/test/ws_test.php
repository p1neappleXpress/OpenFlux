<?php
// WsClient against a server that splits, fragments and closes (see wsserver.php):
//   php test/ws_test.php
require __DIR__ . '/../lib/ws.php';
$port = random_int(44000, 44999);
$p = proc_open([PHP_BINARY, __DIR__ . '/wsserver.php', (string)$port], [1 => ['pipe', 'w'], 2 => ['file', '/dev/null', 'w']], $pipes);
fgets($pipes[1]);
$ws = new WsClient();
$ok = true;
$check = function (string $name, bool $pass) use (&$ok) { printf("%-58s %s\n", $name, $pass ? 'OK' : 'FAIL'); $ok = $ok && $pass; };
$check('upgrade', $ws->connect("ws://127.0.0.1:$port/", 'http://x', ''));

$got = [];
$until = microtime(true) + 12;
while (microtime(true) < $until && !$ws->closed) {
    do {
        $m = $ws->readFrame(0.2);          // 0.2 s: shorter than the server's pauses inside one frame
        if ($m !== null && $m !== '') { $got[] = $m; }
    } while ($ws->pending());
}
$check('two messages in one TCP chunk come out separately', ($got[0] ?? '') === 'first' && ($got[1] ?? '') === 'second');
$check('a 60 KB message delivered slowly arrives whole', ($got[2] ?? '') === str_repeat('0123456789abcdef', 3840));
$check('a fragmented message is reassembled (ping in between)', ($got[3] ?? '') === 'frag-ment-ed');
$check('a header split across writes is handled', ($got[4] ?? '') === 'split-header');
$check('nothing extra or corrupt came out', count($got) === 5);
$check('a close frame sets $closed and drops the socket', $ws->closed && $ws->sock === null);
$check('readFrame after close returns null (no spin)', $ws->readFrame(0.05) === null);
proc_terminate($p);
echo $ok ? "WS TEST PASS\n" : "WS TEST FAIL\n";
exit($ok ? 0 : 1);
