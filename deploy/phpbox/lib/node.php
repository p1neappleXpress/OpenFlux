<?php
// The page + control side of a phpbox exit: one controller every exit file
// (cupsexit.php, mailruexit.php, ...) hands its Carrier factory to.
//
//   node.php?k=TOKEN&url=TARGET            browser -> the status page (starts the node for you)
//                                          anything else (a pinger, curl) -> runs the node, as before
//   ...&a=run | ui | status | log | stop   explicit actions (JSON for status/log/stop)
//
// "Already running" is decided from a heartbeat the running node writes every
// second (plus an flock), so a second open - or a pinger - never starts a
// second node on the same target; it just reports the first one.
//
// Self-renewing tunnel (&chain=1, what the page asks for): a request lives only
// ~cap seconds on these hosts, so before it ends a node starts its successor -
// a request to its own host, which passes the host's bot check because it comes
// from the host itself - and they hand over:
//   1. the successor joins and reports "serving";
//   2. the old generation stops taking NEW streams ("draining") and keeps
//      serving the ones it has until they end or its own cap;
//   3. while both are up, a new stream goes to whichever creates its marker
//      directory first (mkdir is atomic), so no stream is served twice.
// Only one generation ever takes new streams; a stop (the page, &a=stop) ends
// the whole chain and keeps a dying generation from spawning another.
//
// Links are NOT parsed here. The page parses openflux:// links in the browser
// with the core's own parser (WebAssembly), so there is one parser for every
// client; this file only ever sees the carrier's own address (?url=).

require_once __DIR__ . '/util.php';
require_once __DIR__ . '/mux.php';

/** An append-only ring log, one JSON object per line: {"t":ms,"l":level,"m":message}. */
final class PhpboxLog
{
    const MAX_BYTES = 262144;

    public function __construct(private string $file) {}

    public function write(string $level, string $msg): void
    {
        $line = json_encode(['t' => (int)(microtime(true) * 1000), 'l' => $level, 'm' => $msg], JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES) . "\n";
        @file_put_contents($this->file, $line, FILE_APPEND | LOCK_EX);
        if (mt_rand(1, 50) === 1) {
            $this->trim();
        }
    }

    private function trim(): void
    {
        if (@filesize($this->file) <= self::MAX_BYTES) {
            return;
        }
        $keep = @file_get_contents($this->file, false, null, (int)(self::MAX_BYTES / 2));
        if ($keep === false) {
            return;
        }
        $nl = strpos($keep, "\n");
        @file_put_contents($this->file, $nl === false ? '' : substr($keep, $nl + 1), LOCK_EX);
    }

    public function clear(): void
    {
        @file_put_contents($this->file, '', LOCK_EX);
    }

    /** Lines after byte offset $since. Offsets restart at 0 when the ring was trimmed. */
    public function tail(int $since): array
    {
        $size = (int)@filesize($this->file);
        $reset = $since > $size;
        if ($reset) {
            $since = 0;
        }
        $lines = [];
        $next = $since;
        if ($size > $since) {
            $chunk = (string)@file_get_contents($this->file, false, null, $since, 65536);
            $end = strrpos($chunk, "\n");           // only whole lines; a half-written one waits
            if ($end !== false) {
                $chunk = substr($chunk, 0, $end + 1);
                $next = $since + strlen($chunk);
                foreach (explode("\n", rtrim($chunk, "\n")) as $l) {
                    $o = json_decode($l, true);
                    if (is_array($o)) {
                        $lines[] = $o;
                    }
                }
            }
        }
        return ['lines' => $lines, 'next' => $next, 'reset' => $reset];
    }
}

/** Heartbeat file of one node (per carrier+target). */
final class PhpboxState
{
    const STALE = 15;   // a node that has not beaten for this long is gone

    public function __construct(private string $file) {}

    public function read(): array
    {
        $o = json_decode((string)@file_get_contents($this->file), true);
        return is_array($o) ? $o : [];
    }

    public function write(array $s): void
    {
        $tmp = $this->file . '.' . getmypid();
        if (@file_put_contents($tmp, json_encode($s)) !== false) {
            @rename($tmp, $this->file);
        }
    }

    public static function alive(array $s): bool
    {
        return in_array($s['phase'] ?? '', ['connecting', 'serving', 'draining'], true)
            && (time() - (int)($s['beat'] ?? 0)) < self::STALE;
    }

    /** Alive and still taking new streams (not draining). */
    public static function accepting(array $s): bool
    {
        return self::alive($s) && ($s['phase'] ?? '') !== 'draining';
    }

    /** All generations of one node that have a state file: gen => state. */
    public static function generations(string $dir, string $key): array
    {
        $out = [];
        foreach (glob("$dir/$key.g*.json") ?: [] as $f) {
            if (preg_match('/\.g(\d+)\.json$/', $f, $m)) {
                $o = json_decode((string)@file_get_contents($f), true);
                if (is_array($o)) { $out[(int)$m[1]] = $o; }
            }
        }
        ksort($out);
        return $out;
    }
}

final class PhpboxNode
{
    const VERSION = '0.4';
    const CHAIN_CAP = 240;      // lifetime of one generation in chain mode (plain mode: the exit's own cap, 140)
    const MAX_DRAIN = 60;       // seconds a generation keeps serving its streams after handing over

    /**
     * @param string   $carrier  'cupsonline' | 'mailru' - the transport type a link must carry to fit this exit
     * @param string   $title    shown in the page
     * @param callable $target   fn(array $get): string  - the carrier's own address from the request ('' if none)
     * @param callable $factory  fn(string $target): Carrier
     */
    public function __construct(
        private string $carrier,
        private string $title,
        private $target,
        private $factory,
        private int $cap = 140,
    ) {}

    public function handle(): void
    {
        error_reporting(E_ALL & ~E_DEPRECATED);
        $token = getenv('PHPBOX_TOKEN') ?: (defined('PHPBOX_TOKEN') ? PHPBOX_TOKEN : 'CHANGE-ME'); // putenv is disabled on some free hosts: config.php also define()s it
        if (!hash_equals($token, (string)($_GET['k'] ?? ''))) {
            http_response_code(404);
            header('Content-Type: text/plain; charset=utf-8');
            exit("no\n");
        }
        $target = trim((string)($this->target)($_GET));
        $action = (string)($_GET['a'] ?? '');
        if ($action === '') {
            $action = $this->wantsPage() ? 'ui' : 'run';
        }

        if ($action === 'wasm')   { $this->serveAsset('share.wasm.gz', 'application/wasm', true); }
        if ($action === 'ping')   { $this->json($this->ping()); }   // what a deploy step asks: is this the node, and can this host run it?
        if ($action === 'ui')     { $this->servePage($target); }

        if ($target === '') {
            $this->json(['error' => 'need_target'], 400);
        }
        $key = substr(sha1($this->carrier . '|' . $target), 0, 12);
        $dir = PhpboxUtil::stateDir();
        $log = new PhpboxLog("$dir/$key.log");

        switch ($action) {
            case 'status':
                $this->json($this->status($dir, $key));
            case 'log':
                $this->json($log->tail((int)($_GET['since'] ?? 0)));
            case 'stop':
                $alive = array_filter(PhpboxState::generations($dir, $key), fn($g) => PhpboxState::alive($g));
                if ($alive) {
                    @touch("$dir/$key.stop");   // every generation sees it; none spawns another
                    $log->write('info', 'stop requested from the page');
                }
                $this->json(['ok' => true, 'was_running' => (bool)$alive]);
            case 'run':
                $this->run($target, $key, $dir, $log);
                exit;
            default:
                $this->json(['error' => 'unknown_action'], 400);
        }
    }

    // ---- run ------------------------------------------------------------

    private function run(string $target, string $key, string $dir, PhpboxLog $log): void
    {
        header('Content-Type: text/plain; charset=utf-8');
        header('Cache-Control: no-store');
        header('X-Accel-Buffering: no');
        @ini_set('zlib.output_compression', '0');
        ignore_user_abort(true);      // a dropped tab or a proxy 504 must not stop the node
        @set_time_limit(0);           // disabled on some free hosts; harmless there
        while (ob_get_level() > 0) { ob_end_flush(); }

        $chain = !empty($_GET['chain']);
        $succ  = !empty($_GET['succ']);            // started by the previous generation, not by a person
        $from  = (int)($_GET['from'] ?? 0);
        $cap   = $chain ? self::CHAIN_CAP : $this->cap;
        if (isset($_GET['cap'])) {
            $cap = max(PhpboxUtil::testMode() ? 6 : 30, min(900, (int)$_GET['cap']));
        }
        $drain   = min(self::MAX_DRAIN, intdiv($cap, 3));
        $spawnAt = $cap - $drain;                  // when this generation starts its successor

        $gens = PhpboxState::generations($dir, $key);
        if (!$succ) {
            $accepting = array_filter($gens, fn($g) => PhpboxState::accepting($g));
            if ($accepting) {
                $g = max(array_keys($accepting));
                $log->write('info', "another open asked to start the node: already running (gen $g)");
                echo "already running\n";
                return;
            }
            @unlink("$dir/$key.stop");             // a person starting it again clears an earlier stop
        } elseif (is_file("$dir/$key.stop")) {
            $log->write('info', 'successor not started: stop was requested');
            echo "stopped\n";
            return;
        }
        $gen = $succ ? $from + 1 : ($gens ? max(array_keys($gens)) : 0) + 1;
        if ($succ && isset($gens[$gen]) && PhpboxState::alive($gens[$gen])) {
            echo "already running\n";            // a duplicate successor request
            return;
        }
        $lock = @fopen("$dir/$key.g$gen.lock", 'c');
        if ($lock && !flock($lock, LOCK_EX | LOCK_NB)) {
            echo "already running\n";              // another request holds this generation but has not beaten yet
            return;
        }
        foreach ($gens as $old => $_) {            // forget generations long gone
            if ($old < $gen - 4) { @unlink("$dir/$key.g$old.json"); @unlink("$dir/$key.g$old.lock"); }
        }

        $state   = new PhpboxState("$dir/$key.g$gen.json");
        $started = time();
        $sensitive = !empty($_GET['sensitive']);
        $s = [
            'carrier' => $this->carrier, 'pid' => getmypid(), 'gen' => $gen, 'phase' => 'connecting',
            'started' => $started, 'beat' => $started, 'cap' => $cap, 'elapsed' => 0,
            'chain' => $chain, 'spawn_at' => $spawnAt, 'from' => $succ ? $from : null,
            'streams' => 0, 'opened' => 0, 'failed' => 0, 'up' => 0, 'down' => 0,
            'sensitive' => $sensitive, 'php' => PHP_VERSION, 'version' => self::VERSION,
        ];
        $state->write($s);
        $log->write('info', "node gen $gen starting on {$this->carrier}" . ($succ ? " (successor of gen $from)" : '')
            . ' (phpbox ' . self::VERSION . ', php ' . PHP_VERSION . ')');

        // Whatever a carrier echoes is also a log line (and still goes to the response).
        // The response goes nowhere useful: a successor's request was closed by the generation that started it, and
        // a person's tab or a proxy's 504 closes the first one. On hosts where ignore_user_abort does not hold, PHP
        // stops the script at its next write to a closed connection - which silently ended every successor right
        // after "joined". So a successor writes nothing, and a first run only its opening lines.
        $quietAt = time() + 3;
        ob_start(function (string $buf) use ($log, $succ, $quietAt) {
            foreach (preg_split('/\R/', $buf) as $l) {
                if (trim($l) !== '') { $log->write('info', trim($l)); }
            }
            return ($succ || time() > $quietAt) ? '' : $buf;
        }, 1);

        $ownMine = "$dir/$key.own.$gen";           // markers for streams while a successor is coming up
        $ownPrev = "$dir/$key.own.$from";          // ... and while we are the successor
        $rmOwn = function (string $d) {
            foreach (glob("$d/*") ?: [] as $f) { @rmdir($f); }
            @rmdir($d);
        };
        $ended = false;
        $finish = function (string $why, string $lvl = 'info') use (&$s, $state, $log, &$ended, $rmOwn, $ownMine) {
            if ($ended) { return; }
            $ended = true;
            $s['phase'] = 'idle';
            $s['ended'] = time();
            $s['elapsed'] = time() - $s['started'];
            $s['reason'] = $why;
            $state->write($s);
            $rmOwn($ownMine);
            $log->write($lvl, "node gen {$s['gen']} ended: $why");
        };
        register_shutdown_function(function () use (&$finish) {   // CPU limit, fatal error
            $e = error_get_last();
            $finish($e ? 'died: ' . $e['message'] : 'process ended', $e ? 'error' : 'info');
        });

        $carrier = ($this->factory)($target);
        if (!$carrier->connect()) {
            $finish('could not join (see the lines above)', 'error');
            echo "connect failed\n";
            return;
        }

        $s['phase'] = 'serving';
        $s['beat'] = time();
        $state->write($s);
        $log->write('info', "joined; serving" . ($chain ? ", handing over to a successor at {$spawnAt}s" : '') . " up to {$cap}s"
            . ($sensitive ? ' (destinations are logged)' : ' (destinations are hidden: add &sensitive=1 to log them)'));
        echo "{$this->carrier} exit ready; serving up to {$cap}s\n";

        $mux = new Mux($carrier);
        $mux->sensitive = $sensitive;
        if (isset($_GET['win'])) { $w = max(0, min(4194304, (int)$_GET['win'])); $mux->streamWindow = $w; $mux->totalWindow = $w * 2; }   // 0 = no flow control
        if (isset($_GET['idle'])) { $mux->idleTimeout = max(0, min(3600, (int)$_GET['idle'])); }
        if (isset($_GET['chunk'])) { $mux->readChunk = max(2048, min(262144, (int)$_GET['chunk'])); }
        $mux->log = fn(string $lvl, string $m) => $log->write($lvl, $m);

        // Handover bookkeeping (chain mode).
        $asSucc = $succ;                                 // the previous generation may still take the same OPENs
        $asPred = false;                                 // we spawned a successor that may take them too
        $spawned = false; $lastSpawn = 0; $spawnTries = 0; $frozen = false; $endWhy = '';
        $predState = $succ ? new PhpboxState("$dir/$key.g$from.json") : null;
        $succState = new PhpboxState("$dir/$key." . 'g' . ($gen + 1) . '.json');
        if ($asSucc) { @mkdir($ownPrev, 0700, true); }
        $mux->claim = function (int $sid) use (&$asSucc, &$asPred, $ownPrev, $ownMine): bool {
            // mkdir is atomic: the first generation to create the marker owns the stream. Recursive, because the
            // previous generation removes its marker directory when it ends, up to a second before we notice.
            if ($asSucc && !@mkdir("$ownPrev/$sid", 0700, true)) { return false; }
            if ($asPred && !@mkdir("$ownMine/$sid", 0700, true)) { return false; }
            return true;
        };

        $mux->onTick = function (Mux $m) use (&$s, $state, $started, $dir, $key, $chain, $spawnAt, $target, $cap, $sensitive, $gen,
                                              $log, &$asSucc, &$asPred, &$spawned, &$lastSpawn, &$spawnTries, &$frozen, &$endWhy,
                                              $predState, $succState, $ownMine) {
            $s['beat'] = time();
            $s['elapsed'] = time() - $started;
            $s['streams'] = $m->activeStreams();
            $s['opened'] = $m->stats['opened'];
            $s['failed'] = $m->stats['failed'];
            $s['up'] = $m->stats['up'];
            $s['down'] = $m->stats['down'];
            $state->write($s);
            if (file_exists("$dir/$key.stop")) {          // left in place: every generation and any spawn sees it
                $endWhy = 'stopped';
                return false;
            }
            if ($asSucc && !PhpboxState::accepting($predState->read())) {
                $asSucc = false;                          // the previous generation no longer takes streams: no contest
                $log->write('debug', 'previous generation stopped taking new streams');
            }
            if ($chain && !$frozen) {
                if ($s['elapsed'] >= $spawnAt && time() - $lastSpawn >= 15) {
                    $lastSpawn = time();
                    @mkdir($ownMine, 0700, true);
                    $asPred = true;                       // from now on new streams are claimed, not assumed
                    if ($this->spawn($target, $gen, $cap, $sensitive, $log, $spawnTries++)) {
                        $spawned = true;
                        $log->write('info', "started gen " . ($gen + 1) . ' to take over');
                    } else {
                        $log->write('warn', 'could not start the successor; retrying');
                    }
                }
                if ($spawned) {
                    $n = $succState->read();
                    if (PhpboxState::alive($n) && ($n['phase'] ?? '') === 'serving') {
                        $m->accepting = false;            // the successor takes every new stream from here
                        $frozen = true;
                        $asPred = false;
                        $s['phase'] = 'draining';
                        $state->write($s);
                        $log->write('info', 'handed over to gen ' . ($gen + 1) . "; draining {$m->activeStreams()} stream(s)");
                    }
                }
            }
            if ($frozen && $m->activeStreams() === 0) {
                $endWhy = 'handed';
                return false;
            }
            return true;
        };
        $why = $mux->run($cap);
        if ($endWhy === 'stopped')     { $finish('stopped from the page'); }
        elseif ($endWhy === 'handed')  { $finish('handed over to gen ' . ($gen + 1) . ' (its streams ended)'); }
        elseif ($frozen)               { $finish('handed over to gen ' . ($gen + 1) . "; {$cap}s cap cut the streams still open"); }
        elseif ($chain)                { $finish("chain broken: no successor was serving before the {$cap}s cap", 'error'); }
        else                           { $finish("reached the {$cap}s cap"); }
        echo "exit done\n";
    }

    /** Start the next generation: a request to our own host, which the host's bot check lets through. */
    private function spawn(string $target, int $gen, int $cap, bool $sensitive, PhpboxLog $log, int $attempt = 0): bool
    {
        $hostport = (string)($_SERVER['HTTP_HOST'] ?? '');
        if ($hostport === '') { return false; }
        [$h, $p] = array_pad(explode(':', $hostport, 2), 2, null);
        $tls = (!empty($_SERVER['HTTPS']) && $_SERVER['HTTPS'] !== 'off')
            || ($_SERVER['HTTP_X_FORWARDED_PROTO'] ?? '') === 'https' || ($_SERVER['SERVER_PORT'] ?? '') === '443';
        if ($attempt % 2 === 1) { $tls = !$tls; }          // a retry tries the other scheme (hosts that redirect http to https)
        $port = (int)($p ?: ($tls ? 443 : 80));
        $path = (string)strtok((string)($_SERVER['REQUEST_URI'] ?? '/'), '?');
        $qs = http_build_query([
            'k' => (string)$_GET['k'], 'a' => 'run', 'url' => $target, 'succ' => 1, 'from' => $gen,
            'chain' => 1, 'cap' => $cap, 'sensitive' => $sensitive ? 1 : 0,
        ]);
        $fp = @fsockopen(($tls ? 'ssl://' : '') . $h, $port, $en, $es, 10);
        if (!$fp) {
            $log->write('warn', "self-request to " . ($tls ? 'https' : 'http') . "://$h:$port failed: $en $es");
            return false;
        }
        fwrite($fp, "GET $path?$qs HTTP/1.1\r\nHost: $hostport\r\nUser-Agent: phpbox-chain\r\nAccept: text/plain\r\nConnection: close\r\n\r\n");
        // A running successor stays silent, so no answer is what success looks like. An answer within a moment is a
        // redirect, the host's bot check or an error page - say so instead of waiting for a generation that never comes.
        $r = [$fp]; $w = $e = null;
        if (@stream_select($r, $w, $e, 0, 300000)) {
            $head = (string)@fread($fp, 1024);
            $line = strtok($head, "\r\n");
            $ok = preg_match('#^HTTP/\S+ 200#', (string)$line) && stripos($head, 'slowAES') === false;
            if (!$ok) {
                $log->write('warn', 'self-request to ' . ($tls ? 'https' : 'http') . "://$h:$port was answered: "
                    . ($line !== false && $line !== '' ? $line : 'nothing usable') . (stripos($head, 'slowAES') !== false ? ' (the host\'s browser check)' : ''));
                fclose($fp);
                return false;
            }
        }
        fclose($fp);                                   // the successor outlives this socket and writes nothing to it
        return true;
    }

    // ---- ping -----------------------------------------------------------

    /** Facts about this host for the installer: what the node needs, and whether the host has it. */
    private function ping(): array
    {
        $dir = PhpboxUtil::stateDir();
        $needs = [
            'fsockopen'     => function_exists('fsockopen'),
            'stream_select' => function_exists('stream_select'),
            'stream_socket_client' => function_exists('stream_socket_client'),
            'usleep'        => function_exists('usleep'),
            'openssl'       => extension_loaded('openssl'),
            'json'          => function_exists('json_encode'),
        ];
        return [
            'phpbox'    => self::VERSION,
            'carrier'   => $this->carrier,
            'php'       => PHP_VERSION,
            'sapi'      => PHP_SAPI,
            'needs'     => $needs,
            'missing'   => array_keys(array_filter($needs, fn($ok) => !$ok)),
            'state_dir' => is_dir($dir) && is_writable($dir),
            'parser'    => is_file(dirname(__DIR__) . '/assets/share.wasm.gz'),
            'time'      => time(),
        ];
    }

    // ---- status ---------------------------------------------------------

    private function status(string $dir, string $key): array
    {
        $gens  = PhpboxState::generations($dir, $key);
        $alive = array_filter($gens, fn($g) => PhpboxState::alive($g));
        $accepting = array_filter($alive, fn($g) => PhpboxState::accepting($g));
        // The state shown: the generation taking streams, else one that is winding down, else the last one that ran.
        $cur = $accepting ? $accepting[max(array_keys($accepting))]
            : ($alive ? $alive[max(array_keys($alive))] : ($gens ? $gens[max(array_keys($gens))] : []));
        if (PhpboxState::alive($cur)) {
            $cur['elapsed'] = max((int)($cur['elapsed'] ?? 0), time() - (int)$cur['started']);
        }
        foreach (['streams', 'opened', 'failed', 'up', 'down'] as $f) {      // totals across overlapping generations
            $cur[$f] = array_sum(array_map(fn($g) => (int)($g[$f] ?? 0), $alive ?: [$cur]));
        }
        $draining = count($alive) - count($accepting);
        return [
            'running'  => (bool)$accepting,
            'draining' => $draining,
            'chain'    => !empty($cur['chain']),
            'next_in'  => ($accepting && !empty($cur['chain'])) ? max(0, (int)$cur['spawn_at'] - (int)$cur['elapsed']) : null,
            'stopping' => $alive && is_file("$dir/$key.stop"),
            'state'    => $cur,
            'age'      => isset($cur['beat']) ? time() - (int)$cur['beat'] : null,
            'now'      => time(),
        ];
    }

    // ---- page -----------------------------------------------------------

    /** A browser navigating here wants the page; a pinger or curl wants the node to run. */
    private function wantsPage(): bool
    {
        if (!empty($_GET['headless'])) { return false; }
        $accept = (string)($_SERVER['HTTP_ACCEPT'] ?? '');
        $mode   = (string)($_SERVER['HTTP_SEC_FETCH_MODE'] ?? '');
        if ($mode !== '' && $mode !== 'navigate') { return false; }
        return stripos($accept, 'text/html') !== false;
    }

    private function servePage(string $target): void
    {
        require_once __DIR__ . '/ui.php';
        $assets = dirname(__DIR__) . '/assets';
        header('Content-Type: text/html; charset=utf-8');
        header('Cache-Control: no-store');
        header('X-Robots-Tag: noindex');
        $self = strtok((string)($_SERVER['REQUEST_URI'] ?? ''), '?');
        echo PhpboxUi::render([
            'carrier' => $this->carrier,
            'title'   => $this->title,
            'target'  => $target,
            'k'       => (string)$_GET['k'],
            'self'    => $self,
            'auto'    => !isset($_GET['auto']) || $_GET['auto'] !== '0',
            'sensitive' => !empty($_GET['sensitive']),
            'cap'     => $this->cap,
            'version' => self::VERSION,
            'php'     => PHP_VERSION,
            'hasWasm' => is_file("$assets/share.wasm.gz"),
            'wasmExec' => (string)@file_get_contents("$assets/wasm_exec.js"),
        ]);
        exit;
    }

    private function serveAsset(string $name, string $type, bool $gzip): void
    {
        $f = dirname(__DIR__) . '/assets/' . $name;
        if (!is_file($f)) {
            http_response_code(404);
            exit;
        }
        header('Content-Type: ' . $type);
        if ($gzip) { header('Content-Encoding: gzip'); }
        header('Cache-Control: private, max-age=86400');
        header('Content-Length: ' . filesize($f));
        readfile($f);
        exit;
    }

    private function json(array $o, int $code = 200): void
    {
        http_response_code($code);
        header('Content-Type: application/json; charset=utf-8');
        header('Cache-Control: no-store');
        echo json_encode($o, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES);
        exit;
    }
}
