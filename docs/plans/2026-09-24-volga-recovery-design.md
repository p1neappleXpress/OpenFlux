# Volga transport recovery

The Volga transport currently authorizes once at startup. Its WebSocket listener reconnects with the same authorization values forever. A stale session can therefore leave the HTTP relay accepting packets while the opposite end receives none, which was observed on the first Android client and cleared by restarting the exit node.

On every WebSocket reconnect, obtain fresh Yandex authorization and publish it atomically to both the listener and HTTP relay. Periodically rotate the connection even when the WebSocket receives keepalive messages. If a user packet has no inbound response for one minute, close the WebSocket so the normal reconnect path refreshes authorization. Transport keepalives must not trigger this detector. Keep the existing session when reauthorization fails and retry with backoff; never log tokens or cookies.

The transport protocol and profile format remain unchanged. Both exit nodes and clients should use the updated binary for the full benefit. Validate with unit tests covering refresh publication, stalled-traffic detection, and a race-enabled test. Then build and deploy the exit image, update local clients and Android APKs, and verify actual traffic.
