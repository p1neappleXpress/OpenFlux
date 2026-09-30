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
        return in_array($s['phase'] ?? '', ['connecting', 'serving'], true)
            && (time() - (int)($s['beat'] ?? 0)) < self::STALE;
    }
}

final class PhpboxNode
{
    const VERSION = '0.3';

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
        if ($action === 'ui')     { $this->servePage($target); }

        if ($target === '') {
            $this->json(['error' => 'need_target'], 400);
        }
        $key   = substr(sha1($this->carrier . '|' . $target), 0, 12);
        $dir   = PhpboxUtil::stateDir();
        $log   = new PhpboxLog("$dir/$key.log");
        $state = new PhpboxState("$dir/$key.state.json");

        switch ($action) {
            case 'status':
                $this->json($this->status($state));
            case 'log':
                $this->json($log->tail((int)($_GET['since'] ?? 0)));
            case 'stop':
                $alive = PhpboxState::alive($state->read());
                if ($alive) {
                    @touch("$dir/$key.stop");
                    $log->write('info', 'stop requested from the page');
                }
                $this->json(['ok' => true, 'was_running' => $alive]);
            case 'run':
                $this->run($target, $key, $dir, $log, $state);
                exit;
            default:
                $this->json(['error' => 'unknown_action'], 400);
        }
    }

    // ---- run ------------------------------------------------------------

    private function run(string $target, string $key, string $dir, PhpboxLog $log, PhpboxState $state): void
    {
        header('Content-Type: text/plain; charset=utf-8');
        header('Cache-Control: no-store');
        header('X-Accel-Buffering: no');
        @ini_set('zlib.output_compression', '0');
        ignore_user_abort(true);      // a dropped tab or a proxy 504 must not stop the node
        @set_time_limit(0);           // disabled on some free hosts; harmless there
        while (ob_get_level() > 0) { ob_end_flush(); }

        $prev = $state->read();
        if (PhpboxState::alive($prev)) {
            $log->write('info', 'another open asked to start the node: already running (gen ' . ($prev['gen'] ?? '?') . ')');
            echo "already running\n";
            return;
        }
        $lock = @fopen("$dir/$key.lock", 'c');
        if ($lock && !flock($lock, LOCK_EX | LOCK_NB)) {
            echo "already running\n";   // another request holds the lock but has not beaten yet
            return;
        }
        @unlink("$dir/$key.stop");

        $gen = (int)($prev['gen'] ?? 0) + 1;
        $started = time();
        $sensitive = !empty($_GET['sensitive']);
        $s = [
            'carrier' => $this->carrier, 'pid' => getmypid(), 'gen' => $gen, 'phase' => 'connecting',
            'started' => $started, 'beat' => $started, 'cap' => $this->cap, 'elapsed' => 0,
            'streams' => 0, 'opened' => 0, 'failed' => 0, 'up' => 0, 'down' => 0,
            'sensitive' => $sensitive, 'php' => PHP_VERSION, 'version' => self::VERSION,
        ];
        $state->write($s);
        $log->write('info', "node gen $gen starting on {$this->carrier} (phpbox " . self::VERSION . ', php ' . PHP_VERSION . ')');

        // Whatever a carrier echoes is also a log line (and still goes to the response).
        ob_start(function (string $buf) use ($log) {
            foreach (preg_split('/\R/', $buf) as $l) {
                if (trim($l) !== '') { $log->write('info', trim($l)); }
            }
            return $buf;
        }, 1);

        $ended = false;
        $finish = function (string $why, string $lvl = 'info') use (&$s, $state, $log, &$ended) {
            if ($ended) { return; }
            $ended = true;
            $s['phase'] = 'idle';
            $s['ended'] = time();
            $s['elapsed'] = time() - $s['started'];
            $s['reason'] = $why;
            $state->write($s);
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
        $log->write('info', "joined; serving up to {$this->cap}s" . ($sensitive ? ' (destinations are logged)' : ' (destinations are hidden: add &sensitive=1 to log them)'));
        echo "{$this->carrier} exit ready; serving up to {$this->cap}s\n";

        $mux = new Mux($carrier);
        $mux->sensitive = $sensitive;
        $mux->log = fn(string $lvl, string $m) => $log->write($lvl, $m);
        $mux->onTick = function (Mux $m) use (&$s, $state, $started, $dir, $key) {
            $s['beat'] = time();
            $s['elapsed'] = time() - $started;
            $s['streams'] = $m->activeStreams();
            $s['opened'] = $m->stats['opened'];
            $s['failed'] = $m->stats['failed'];
            $s['up'] = $m->stats['up'];
            $s['down'] = $m->stats['down'];
            $state->write($s);
            if (file_exists("$dir/$key.stop")) {
                @unlink("$dir/$key.stop");
                return false;
            }
            return true;
        };
        $why = $mux->run($this->cap);
        $finish($why === 'stopped' ? 'stopped from the page' : "reached the {$this->cap}s cap");
        echo "exit done\n";
    }

    // ---- status ---------------------------------------------------------

    private function status(PhpboxState $state): array
    {
        $s = $state->read();
        $alive = PhpboxState::alive($s);
        if ($alive) {
            $s['elapsed'] = max((int)($s['elapsed'] ?? 0), time() - (int)$s['started']);
        }
        return [
            'running' => $alive,
            'state'   => $s,
            'age'     => isset($s['beat']) ? time() - (int)$s['beat'] : null,
            'now'     => time(),
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
