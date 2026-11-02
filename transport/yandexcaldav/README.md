# yandexcaldav (native)

The Go twin of the `yandex-caldav` JS transport. Packets ride the
`DESCRIPTION` of two events per channel in a private calendar on
`caldav.yandex.ru`:

```
<calendar>/ofx-<channel>-c2s.ics   written by the client, read by the exit
<calendar>/ofx-<channel>-s2c.ics   written by the exit, read by the client
```

Every PUT carries all unacknowledged frames that fit in one event (640 KiB);
the ack in the peer's `DESCRIPTION` frees the writer's ring. The wire is
OFX1, byte-for-byte the JS transport's, so a native lane and a JS lane are
interchangeable as far as the events are concerned (they are still different
transports to the core).

## Use

Needs a Yandex **Calendar** app password (Яндекс ID → Безопасность → Пароли
приложений → Календарь). The exit creates the `OpenFlux` calendar if it is
missing.

```bash
# exit
./openflux --role=exit --mode=l4 --transport=yandex-caldav --url=main \
  --caldav-email=you@yandex.ru --caldav-password='app password' \
  --caldav-calendar=OpenFlux --encryption-key-file=secret.txt

# client
./openflux --role=client --inbound=socks5 --transport=yandex-caldav --url=main \
  --caldav-email=you@yandex.ru --caldav-password='app password' \
  --caldav-calendar=OpenFlux --encryption-key-file=secret.txt
```

Or in a `.conf`:

```ini
[Transport "caldav"]
Type = yandex-caldav
Priority = 100
URL = main
Email = you@yandex.ru
Password = app password
Calendar = OpenFlux
```

Parallel lanes are the core's striped group: put a comma-separated list in
`URL`, e.g. `lane-1,lane-2,lane-3,lane-4`, the same on both ends. To spread
lanes over accounts, prefix channels with `aN-` and set
`Accounts = first@yandex.ru:pw1|second@yandex.ru:pw2` (native) or the
`accounts` setting (JS).

## Limits

Measured on 2026-10-31: six native lanes moved 8 × 1 MiB in 25.8 s
(~317 KB/s); the JS transport with 2 lanes × 3 slots reached ~450 KB/s. The
raw CalDAV carrier does ~2.4 MB/s on the same link, so the ceiling is the
tunnel's TCP over the carrier RTT, not this package. A single lane does
~75 KB/s in a 1 MiB test.
