<?php
// Renders the phpbox status page from lib/page.html. All state in the page
// comes from the status/log endpoints of node.php; the only thing injected
// here is the page's own configuration (never the token's secret material
// beyond the ?k= the visitor already has).

final class PhpboxUi
{
    public static function render(array $cfg): string
    {
        $tpl = (string)file_get_contents(__DIR__ . '/page.html');
        $wasmExec = $cfg['wasmExec'] ?? '';
        unset($cfg['wasmExec']);
        $json = json_encode($cfg, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES | JSON_HEX_TAG | JSON_HEX_AMP);
        return strtr($tpl, [
            '%%CONFIG%%'   => $json,
            '%%WASMEXEC%%' => str_replace('</script', '<\/script', $wasmExec),
            '%%TITLE%%'    => htmlspecialchars((string)($cfg['title'] ?? 'phpbox'), ENT_QUOTES),
        ]);
    }
}
