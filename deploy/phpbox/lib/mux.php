<?php
// The carrier-agnostic phpbox stream mux, shared by every exit. A Carrier
// moves opaque "packets" (each one mux frame) over some link (cups, mailru);
// the Mux turns them into OPEN/DATA/CLOSE streams and dials the destinations.

require_once __DIR__ . '/util.php';

interface Carrier
{
    public function setMux(Mux $m): void;
    public function connect(): bool;            // auth + join; false on failure
    public function sockets(): array;           // read sockets to select on (its WS)
    public function onReadable($sock): void;    // decode link -> $mux->onPacket(pkt) per packet
    public function sendPacket(string $frame): void; // encode + send one mux frame
}

final class Mux
{
    const OPEN = 1, DATA = 2, CLOSE = 3, OPEN_OK = 4, OPEN_ERR = 5;
    const DIAL_TO = 6;
    const READ_CHUNK = 8192;

    private string $muxBuf = '';
    private array  $socks  = [];   // stream_id => dst resource

    public function __construct(private Carrier $c)
    {
        $c->setMux($this);
    }

    /** run holds the exit in the room for up to $cap seconds, pumping both ways. */
    public function run(int $cap): void
    {
        $start = microtime(true);
        while (microtime(true) - $start < $cap) {
            if (connection_aborted()) {
                break;
            }
            $carrierSocks = $this->c->sockets();
            $read = array_merge($carrierSocks, array_values($this->socks));
            if (!$read) {
                usleep(100000);
                continue;
            }
            $w = $e = null;
            if (@stream_select($read, $w, $e, 0, 200000) > 0) {
                foreach ($read as $s) {
                    if (in_array($s, $carrierSocks, true)) {
                        $this->c->onReadable($s);
                    } else {
                        $this->onDstReadable($s);
                    }
                }
            }
        }
        foreach ($this->socks as $s) {
            @fclose($s);
        }
    }

    /** onPacket takes reassembled link bytes and drains whole mux frames. */
    public function onPacket(string $pkt): void
    {
        $this->muxBuf .= $pkt;
        while (strlen($this->muxBuf) >= 9) {
            $h = unpack('Ctype/Nsid/Nlen', substr($this->muxBuf, 0, 9));
            if (strlen($this->muxBuf) < 9 + $h['len']) {
                break;
            }
            $payload = substr($this->muxBuf, 9, $h['len']);
            $this->muxBuf = substr($this->muxBuf, 9 + $h['len']);
            $this->applyMux($h['type'], $h['sid'], $payload);
        }
    }

    public function sendFrame(int $type, int $sid, string $payload): void
    {
        $this->c->sendPacket(pack('CNN', $type, $sid, strlen($payload)) . $payload);
    }

    private function applyMux(int $type, int $sid, string $payload): void
    {
        if ($type === self::OPEN) {
            [$host, $port] = array_pad(explode(':', $payload, 2), 2, '');
            if (!in_array((int)$port, [80, 443], true)) { $this->sendFrame(self::OPEN_ERR, $sid, 'port'); return; }
            if (PhpboxUtil::isPrivate($host)) { $this->sendFrame(self::OPEN_ERR, $sid, 'blocked'); return; }
            $s = @stream_socket_client("tcp://$host:$port", $e, $es, self::DIAL_TO, STREAM_CLIENT_CONNECT);
            if ($s) { stream_set_blocking($s, false); $this->socks[$sid] = $s; $this->sendFrame(self::OPEN_OK, $sid, ''); }
            else { $this->sendFrame(self::OPEN_ERR, $sid, (string)$es); }
        } elseif ($type === self::DATA && isset($this->socks[$sid])) {
            @fwrite($this->socks[$sid], $payload);
        } elseif ($type === self::CLOSE && isset($this->socks[$sid])) {
            @fclose($this->socks[$sid]); unset($this->socks[$sid]);
        }
    }

    private function onDstReadable($s): void
    {
        $sid = array_search($s, $this->socks, true);
        if ($sid === false) { return; }
        $d = @fread($s, self::READ_CHUNK);
        if ($d === '' || $d === false) {
            if (feof($s)) { $this->sendFrame(self::CLOSE, $sid, ''); @fclose($s); unset($this->socks[$sid]); }
            return;
        }
        $this->sendFrame(self::DATA, $sid, $d);
    }
}
