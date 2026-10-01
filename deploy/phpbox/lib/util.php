<?php
// Shared helpers for the phpbox exits (cups, mailru, ...).

final class PhpboxUtil
{
    const UA = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36';

    /** One HTTP request with a shared cookie jar file. Returns [body, status]. */
    public static function http(string $url, string $cookieFile, string $method, ?string $body, array $headers): array
    {
        $ch = curl_init($url);
        curl_setopt_array($ch, [
            CURLOPT_RETURNTRANSFER => true,
            CURLOPT_USERAGENT      => self::UA,
            CURLOPT_COOKIEJAR      => $cookieFile,
            CURLOPT_COOKIEFILE     => $cookieFile,
            CURLOPT_FOLLOWLOCATION => true,
            CURLOPT_TIMEOUT        => 30,
            CURLOPT_HTTPHEADER     => $headers,
        ]);
        if ($method === 'POST') {
            curl_setopt($ch, CURLOPT_POST, true);
            curl_setopt($ch, CURLOPT_POSTFIELDS, $body);
        }
        $out  = curl_exec($ch);
        $code = curl_getinfo($ch, CURLINFO_HTTP_CODE);
        return [$out !== false ? $out : '', $code];
    }

    public static function scrape(string $re, string $html): string
    {
        return preg_match($re, $html, $m) ? $m[1] : '';
    }

    /** Netscape cookie-jar value lookup by name. */
    public static function cookie(string $file, string $name): string
    {
        foreach (@file($file) ?: [] as $line) {
            $p = explode("\t", trim($line));
            if (count($p) === 7 && $p[5] === $name) {
                return $p[6];
            }
        }
        return '';
    }

    public static function cookieHeader(string $file): string
    {
        $out = [];
        foreach (@file($file) ?: [] as $line) {
            $c = explode("\t", trim($line));
            if (count($c) === 7) {
                $out[] = $c[5] . '=' . $c[6];
            }
        }
        return implode('; ', $out);
    }

    public static function originOf(string $u): string
    {
        $p = parse_url($u);
        return ($p['scheme'] ?? 'https') . '://' . ($p['host'] ?? '');
    }

    /** Host -> every address it has (IPv4 first), looked up once per run; [] when it does not resolve. */
    public static function resolveAll(string $host): array
    {
        static $cache = [];
        if (!isset($cache[$host])) {
            if (filter_var($host, FILTER_VALIDATE_IP)) {
                $cache[$host] = [$host];
            } else {
                $ips = @gethostbynamel($host) ?: [];
                $cache[$host] = array_values(array_filter($ips, fn($ip) => filter_var($ip, FILTER_VALIDATE_IP)));
            }
        }
        return $cache[$host];
    }

    /** Host -> its first address ('' when it does not resolve). */
    public static function resolve(string $host): string
    {
        return self::resolveAll($host)[0] ?? '';
    }

    /** Local testing only: lifts the private-address and port guards. Never set on a real host. */
    public static function testMode(): bool
    {
        return self::env('PHPBOX_ALLOW_PRIVATE') === '1' || (defined('PHPBOX_ALLOW_PRIVATE') && PHPBOX_ALLOW_PRIVATE === '1');
    }

    /** Refuse loopback / private / reserved targets (no SSRF into the host LAN): any address that is one blocks the host. */
    public static function isPrivate(string $host): bool
    {
        if (self::testMode()) {
            return false;
        }
        $ips = self::resolveAll($host);
        if (!$ips) {
            return true;
        }
        foreach ($ips as $ip) {
            if (!filter_var($ip, FILTER_VALIDATE_IP, FILTER_FLAG_NO_PRIV_RANGE | FILTER_FLAG_NO_RES_RANGE)) {
                return true;
            }
        }
        return false;
    }

    /** Where run state and logs live: a writable dir that is not web-served when the host has one. */
    /** getenv() where the host has it: a disabled function is gone in PHP 8, and calling it is a fatal error. */
    public static function env(string $name)
    {
        return function_exists('getenv') ? getenv($name) : false;
    }

    /** This process's id, or a random stand-in where getmypid() is disabled. */
    public static function pid(): int
    {
        static $pid = null;
        if ($pid === null) { $pid = function_exists('getmypid') ? (int)getmypid() : random_int(100000, 999999); }
        return $pid;
    }

    /**
     * Ask the host to keep running when the request's connection is gone, and to lift the time limit, where those
     * calls exist. Hosts disable them; in PHP 8 a disabled function is undefined and calling it ends the script.
     */
    public static function keepRunning(bool $afterClose = true): void
    {
        if (function_exists('ignore_user_abort')) { @ignore_user_abort($afterClose); }
        if (function_exists('set_time_limit')) { @set_time_limit(0); }
    }

    public static function stateDir(): string
    {
        $base = (is_dir('/home/tmp') && is_writable('/home/tmp')) ? '/home/tmp' : sys_get_temp_dir();
        $dir = $base . '/phpbox-state';
        if (!is_dir($dir)) {
            @mkdir($dir, 0700, true);
        }
        return $dir;
    }
}
