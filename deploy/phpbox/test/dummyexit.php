<?php
// Test exit: a carrier that joins instantly and idles, so the page/state/log/stop
// machinery can be exercised without any network. Not deployed (build-bundle.sh skips test/).
require_once __DIR__ . '/../lib/node.php';

final class IdleCarrier implements Carrier
{
    public function __construct(private string $target) {}
    public function setMux(Mux $m): void {}
    public function connect(): bool
    {
        echo "idle carrier joined {$this->target}\n";
        return $this->target !== 'fail';
    }
    public function sockets(): array { return []; }
    public function onReadable($sock): void {}
    public function sendPacket(string $frame): void {}
}

(new PhpboxNode('mailru', 'test', fn(array $g): string => (string)($g['url'] ?? ''),
    fn(string $t) => new IdleCarrier($t), (int)(PhpboxUtil::env('TEST_CAP') ?: 8)))->handle();
