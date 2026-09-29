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

    /** Refuse loopback / private / reserved targets (no SSRF into the host LAN). */
    public static function isPrivate(string $host): bool
    {
        if (getenv('PHPBOX_ALLOW_PRIVATE') === '1') {
            return false; // local testing only
        }
        $ip = filter_var($host, FILTER_VALIDATE_IP) ? $host : gethostbyname($host);
        return !filter_var($ip, FILTER_VALIDATE_IP,
            FILTER_FLAG_NO_PRIV_RANGE | FILTER_FLAG_NO_RES_RANGE);
    }
}
