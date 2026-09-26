# OpenFlux

[English](README.md) | **Русский**

Исследовательский инструмент сетевого стека. IPv4 TCP/UDP-туннель с подключаемыми
транспортами, батчированным zstd-кодеком и двумя бэкендами выходной ноды
(L3 raw forward / L4 gVisor proxy).

# Отказ от ответственности

Автор OpenFlux **не призывает** использовать данный проект для обхода
блокировок или нарушения правил каких-либо платформ, а также **не несёт
ответственности** за финальные сценарии использования утилиты пользователями
в реальной жизни или сети Интернет. Любые специфические технические
особенности приложения - не более чем **архитектурное совпадение**, созданное
**без какого-либо умысла**.

Проект является **полностью некоммерческим**, не содержит **платных функций,
скрытых подписок или коммерческой выгоды**.

Автор **не несёт ответственности** за форки, модификации и производные
версии OpenFlux, созданные третьими лицами. Любые изменения, добавленные
в форк, являются ответственностью его автора.

Автор **не несёт ответственности** за:

- Любое использование OpenFlux третьими лицами
- Последствия, вызванные использованием форков и модификаций
- Ущерб, возникший в результате работы производных версий
- Нарушения, совершённые с использованием форков

Оригинальный код предоставляется **как есть** («as is»), **без каких-либо
гарантий**.

## Клиенты

| Платформа | Скачать | Примечания |
|-----------|---------|------------|
| **macOS**   | сборка из исходников | CLI + utun L3-клиент (`--inbound=tun`, по умолчанию на macOS) |
| **Linux**   | сборка из исходников | CLI-клиент (SOCKS5) / выходная нода (L3 или L4) |
| **Windows** | сборка из исходников | CLI-клиент (SOCKS5) / выходная нода (`l4`, либо `l3` через QEMU - см. TODO) |
| **Android** | [Релизы OpenFluxAndroid](https://github.com/p1neappleXpress/OpenFluxAndroid) | Отдельный APK |
| **Android** | [Релизы OpenFlux-Android](https://github.com/damnurmum/OpenFlux-Android/releases/latest) | Форк: системный VPN или SOCKS5-прокси, сессии с несколькими транспортами, обработка капчи, телефон как выходная нода |
| **iOS**     | [TestFlight бета](https://testflight.apple.com/join/BwnAcdus) | Системный VPN через Network Extension |

> **iOS-приложение** сделано [@saharev1](https://github.com/saharev1) -
> полноценный iOS-клиент, пайплайн TestFlight, системный VPN, DNS-over-TLS
> и множество фиксов стабильности. ОГРОМНОЕ спасибо!
>
> **OpenFlux-Android** сделан [@damnurmum](https://github.com/damnurmum) -
> Android-клиент с системным VPN и режимом локального SOCKS5-прокси, профилями
> подключений, сессиями с несколькими транспортами и переключением между ними
> (включая direct), обработкой SmartCaptcha и входа во встроенном браузере (в том
> числе капчи выходной ноды, которая проходится через туннель с её адреса),
> режимом выходной ноды l4 на телефоне, Kill Switch и маршрутизацией по
> приложениям и доменам. Также внёс в этот репозиторий сквозное шифрование (#38),
> транспорт Mail.ru (#60) и устойчивость сессий с обработкой капчи ноды (#93).
> ОГРОМНОЕ спасибо!
>
> **Android-приложение** - [p1neappleXpress/OpenFluxAndroid](https://github.com/p1neappleXpress/OpenFluxAndroid).

## Архитектура

Любой клиент работает с любым бэкендом выходной ноды. `--mode` выбирается
на **выходной ноде**, а не на клиенте.

```
Клиент (любой): macOS (utun) / Linux / Windows / iOS (packet tunnel) / Android
                    |
                    v
               Транспорт (Yandex.Docs / Volga / Board / MAX / Cups / Mail.ru / Direct)
                    |
                    v
               Выходная нода  -->  Интернет
                 --mode l3   (сырой SNAT/DNAT, Linux + root)
                 --mode l4   (gVisor proxy, любая платформа)
```

| Клиент (любой)                          | Бэкенд выхода | Требует              |
|-----------------------------------------|---------------|----------------------|
| macOS / Linux / Windows / iOS / Android | `--mode l3`   | exit на Linux + root |
| macOS / Linux / Windows / iOS / Android | `--mode l4`   | ничего               |

В `l3` выходная нода ничего не терминирует: она форвардит сырые TCP- и
UDP-пакеты с SNAT/DNAT (conntrack + фильтр по egress-IP). TCP остаётся
end-to-end между клиентом и реальным сервером.

В `l4` выходная нода терминирует TCP/UDP в userspace-стеке gVisor, затем
подключается к реальному серверу. Работает на любой ОС без root.

Клиент терминирует TCP локально (gVisor, utun или NEPacketTunnelProvider),
затем отправляет сырые IP-пакеты в транспорт. В сессии с несколькими
транспортами они работают одновременно, и трафик переключается между ними (см.
[Сессии с несколькими транспортами](#сессии-с-несколькими-транспортами)).

## Бэкенды выходной ноды

У выходной ноды ровно **два** бэкенда, выбираются флагом `--mode` на
**выходной ноде**. Клиент бэкенд не выбирает - один и тот же клиент
работает с любым из них.

| `--mode` | Бэкенд | Форвардинг | Требует | Платформы |
|----------|--------|-----------|---------|-----------|
| `l3` | Сырой L3 | SNAT/DNAT сырых IPv4-пакетов через SOCK_RAW + conntrack. Без userspace TCP-стека. | root / CAP_NET_RAW | только Linux |
| `l4` (алиас `proxy`) | gVisor proxy | Терминирует TCP/UDP в userspace-стеке gVisor, затем подключается к реальному серверу. | ничего | Linux, macOS, Windows |

- `proxy` - устаревший алиас для `l4`; оба выбирают один и тот же бэкенд.
  Каноническое имя впредь - `l4`.
- **l3 быстрее** (одно TCP-соединение end-to-end, без двойной терминации),
  но только Linux и нужен root.
- **l4 работает везде** без root, ценой двойной терминации TCP
  (клиент -> gVisor на выходе -> реальный сервер).
- На Linux с root предпочитайте `l3`. На Windows целевой путь - `l3`
  внутри лёгкой QEMU-виртуалки (см. TODO); WinDivert-бэкенд пока не подключён,
  а `l4` - рабочий fallback, пока QEMU не поставлен. На хостах без root -
  `l4`.

### l3 и kernel-RST

В режиме `l3` ядро видит ответные пакеты для соединений, которые оно не
открывало, и шлёт RST, разрывая туннельные соединения. Их надо гасить:

```
# Scoped (рекомендуется): назначить отдельный egress-IP, запустить с --local-ip, затем:
sudo iptables -A OUTPUT -p tcp --tcp-flags RST RST -s <egress-ip> -j DROP

# Host-wide fallback (дропает ВСЕ исходящие RST; закрытые порты выглядят filtered):
sudo iptables -A OUTPUT -p tcp --tcp-flags RST RST -j DROP
```

Клиентские RST теперь форвардятся как обычно. Правило выше нужно только для
RST, которые локально генерирует ядро выходной ноды.

## Ключевые особенности

- **Подключаемые транспорты** - Yandex.Docs (WS), Yandex Volga (HTTP relay + WS),
  Yandex Board (WS), MAX/OneMe (WebRTC DataChannel), Cups.online
  (Centrifugo-комнаты), Mail.ru Docs (WS), Direct (прямое TCP до выходной
  ноды, только в сессии).
- **Батчинг + zstd** - склеивает множество туннельных пакетов в одно
  транспортное сообщение. Меньше сообщений в канале, выше скорость. См.
  `transport/batched.go` и `transport/framing.go`.
- **IPv4 UDP** - L4 и SOCKS5 `UDP ASSOCIATE` проверяются локальными echo-тестами.
  Linux raw L3 UDP остаётся экспериментальным; ограничения приведены ниже.
- **Аутентифицированные сессии** - флаг `--negotiate` внутри шифрованного
  канала: свежие идентификаторы сессии, лимиты пакетов и защита от повторов.
  Перезапущенный клиент или выходная нода принимаются снова после
  подтверждения свежего challenge, вторую сторону перезапускать не нужно.
  Запуск старого неаутентифицированного wire-v3 теперь запрещён.
- **Сессии с несколькими транспортами** - `--transports=direct:100,yandex:50`
  (или секции `[Transport]` в `.conf`) запускает все транспорты сразу. Трафик
  идёт по самому приоритетному из тех, что реально доходят до другой стороны,
  и переключается при его отказе. См.
  [Сессии с несколькими транспортами](#сессии-с-несколькими-транспортами).
- **Обработка капчи** - PoW-капча Яндекса решается автоматически. SmartCaptcha
  и требование входа уходят приложению через IPC; те, в которые упирается
  выходная нода, пересылаются клиенту по любому работающему транспорту вместе
  с локальным прокси, через который приложение проходит их с адреса самой
  ноды. См. [Капча](#капча).
- **Два бэкенда выхода** - `l3` (сырой SNAT/DNAT) и `l4` (gVisor proxy).
  См. [Бэкенды выходной ноды](#бэкенды-выходной-ноды).
- **macOS utun-клиент** - `--inbound=tun` (по умолчанию на macOS). Создаёт
  utun-интерфейс, следит за своими сокетами и ставит bypass-маршруты, затем
  забирает default-маршрут. Никакого SOCKS5, никакого gVisor на клиенте.
- **iOS packet tunnel** - NEPacketTunnelProvider, чистый L3-форвардинг.
- **Legacy-кодек** - `--codec=legacy` возвращает старый per-packet LZ4-кодек
  (совместим со старыми клиентами).
- **Опциональное шифрование** - `--encryption-key-file` оборачивает транспорт
  в AES-256-GCM. Обе стороны должны использовать один и тот же секрет.
- **Режимы бенчмарка** - `--role=bench-send --bench-bytes=N` / `--role=bench-sink`
  измеряют чистый goodput через транспорт, не задевая сеть хоста.

## Требования

1. **Go** - для сборки бинарника десктопного клиента / выходной ноды. Точная
   версия - в `go.mod`.
2. **Android NDK r27+** - для сборки бинарника Android-клиента.
3. **Xcode 26.6+** - для сборки бинарника iOS-клиента.
4. **Linux VPS / VDS** для выходной ноды. Бэкенд `l3` требует root; `l4`
   работает без root.

## Структура

```
OpenFlux/
  main.go                          # Точка входа CLI (клиент / exit / бенчи)
  conf.go                          # Разбор .conf
  transport_spec.go                # Разбор --transports, запуск сессии
  transport_factory.go             # Создание транспорта по типу
  ipc_handler.go                   # IPC: cookies от приложения
  auth_proxy.go                    # Локальный HTTP-прокси для проверок ноды
  share_cli.go                     # --share: ссылка и QR-код для клиентов
  share/                           # Ссылки openflux:// и QR-коды
  bench.go                         # Хелперы бенчмарка
  tun_darwin.go                    # macOS utun L3-клиент
  tun_watch.go                     # Watcher сокетов для bypass-маршрутов
  tun_learn.go, tun_other.go       # Хелперы utun / заглушки для не-darwin
  signals_{unix,windows}.go        # Сигналы завершения
  export_ios.go                    # cgo-мост для iOS-статической библиотеки
  export_ios_packet.go             # Мост packet tunnel для iOS
  transport/
    transport.go                   # Интерфейс Transport
    batched.go                     # BatchedTransport (склейка + zstd)
    framing.go                     # Wire-формат батчированных кадров
    compressor.go                  # Legacy per-packet LZ4-кодек
    encrypted.go                   # Опциональная AES-256-GCM обёртка
    session.go                     # Согласованная сессия с несколькими транспортами
    session_add_after_start.go     # Добавление транспорта в работающую сессию
    direct.go                      # Прямой TCP-транспорт
    portdemux.go                   # Разделение ответов между двумя стеками клиента
    cookies.go, cookiestore.go     # Обмен cookies и их хранение
    error_notifier.go              # Внешние ошибки (капча, вход)
    control/                       # Конверт и управляющие сообщения
    manager/                       # Транспорты, cookies и проверки в сессии
    ipc/                           # IPC приложение <-> ядро через Unix-сокет
    yandex/                        # Yandex.Docs, Volga, Board, решатель капчи
    oneme/                         # Бэкенд MAX Messenger
    cupsonline/                    # Бэкенд Cups.online
    mailru/                        # Бэкенд Mail.ru Docs
  tunnel/
    tunnel.go                      # Клиентский туннель (gVisor + TunnelLinkEndpoint)
    endpoint.go                    # Виртуальный NIC (клиент)
    packettunnel.go                # Packet tunnel (iOS)
    httpproxy.go                   # HTTP-прокси поверх стека туннеля
    exit.go                        # Диспетчер NewExitNode (l3 / l4)
    proxy_exit.go                  # L4 exit (gVisor + net.Dial)
    l3/
      l3.go                        # L3Exit: SNAT/DNAT, conntrack, фильтр egress
      backend.go                   # Интерфейс L3Backend
      backend_linux.go             # SOCK_RAW (Linux)
      backend_windows.go           # Заглушка (WinDivert не подключён)
      backend_other.go             # Заглушка для неподдерживаемых платформ
      conntrack.go                 # Таблица conntrack
      flow.go                      # Flow-ключи, SNAT/DNAT, checksums
      udp_nat.go                   # UDP NAT
      icmp.go                      # ICMP-ошибки и MTU
      reassembly.go                # Сборка IPv4-фрагментов
    windivert/                     # WinDivert-бэкенд (есть, но к L3 не подключён)
  socks5/                          # SOCKS5-сервер (fallback на клиенте)
  network/                         # Контрольные суммы, разбор пакетов
  utils/                           # Логирование
  ios-app/                         # iOS-клиент на SwiftUI (XcodeGen)
  build_all.sh                     # Кросс-сборка релизных бинарников
  build_ios.sh                     # Сборка статической библиотеки iOS (liboflux.a)
  build_ios_app.sh                 # Сборка + архив + экспорт IPA iOS
  build_android.sh                 # Сборка клиентского бинарника Android
  scripts/
    cleanup-utun.sh                # Удалить stale-маршруты utun (macOS)
```

## Сборка

```
go mod tidy
go build -o openflux .
```

Кросс-сборка для выходной ноды (Linux amd64), stripped:

```
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -trimpath -o openflux-linux .
```

## Использование

### Выходная нода - L3 (Linux, root)

```
sudo ./openflux --role=exit --mode=l3 \
    --transport=yandex \
    --url="YOUR_YANDEX_DOC_URL"
```

Требует root / CAP_NET_RAW. Поставьте правило iptables (см.
[l3 и kernel-RST](#l3-и-kernel-rst)).

### Выходная нода - L4 (любая ОС, без root)

```
./openflux --role=exit --mode=l4 \
    --transport=yandex \
    --url="YOUR_YANDEX_DOC_URL"
```

Fallback для платформ, где `l3` недоступен (Windows без WinDivert, macOS,
Linux без root). Медленнее `l3` (двойная терминация TCP).

### Клиент - macOS utun (по умолчанию на macOS)

```
sudo ./openflux --role=client --inbound=tun \
    --transport=yandex \
    --url="YOUR_YANDEX_DOC_URL"
```

Создаёт utun-интерфейс, ставит bypass-маршруты для транспорта, ждёт
подключения транспорта, затем забирает default-маршрут. SOCKS5 не нужен.
Требует sudo. Весь трафик, кроме транспорта, идёт через туннель.

### Клиент - SOCKS5 (все платформы, fallback)

```
./openflux --role=client --inbound=socks5 \
    --transport=yandex \
    --url="YOUR_YANDEX_DOC_URL" \
    --socks5=:1080
```

Настройте браузер / приложение на `127.0.0.1:1080` как SOCKS5-прокси. Это
режим по умолчанию на всех платформах, кроме macOS. Приложения с поддержкой
UDP могут использовать команду SOCKS5 `UDP ASSOCIATE`.

### Ограничения UDP

- Пока поддерживается только IPv4 UDP.
- L3 собирает IPv4-фрагменты: до 64 незавершённых датаграмм, 128 фрагментов
  на датаграмму и 4 МиБ на направление. Срок — 30 секунд, очистка при поступлении
  фрагментов; перекрывающиеся фрагменты отбрасываются.
- ICMP-ошибки передаются только для действующих TCP/UDP-трансляций, с проверкой
  сумм и восстановлением адресов/портов внутри цитируемого пакета. Ошибка
  EMSGSIZE возвращает MTU маршрута; пакеты без DF могут фрагментироваться.
  Исходящая фрагментация заголовков с IPv4 options не поддерживается.
- Это ICMP-based PMTU, не активный DPLPMTUD. При блокировке ICMP проблемы
  крупных DF-пакетов остаются возможны. Проверка в реальной сети ещё нужна.
- Linux raw L3 UDP резервирует выбранный ядром порт реальным UDP-сокетом для
  каждого удалённого endpoint и восстанавливает порт клиента в ответах. Это
  исключает занятие портов приложений хоста и должно предотвращать ложный ICMP
  port-unreachable без изменения firewall. Лимит — 256 трансляций; idle timeout —
  2 минуты (15 секунд для DNS). Сохранение исходного порта и endpoint-independent
  NAT/hole-punching не реализованы.
- Изолированный Linux raw-socket/ICMP тест проходит в GitHub Actions. Он проверяет
  loopback в отдельном network namespace, включая конфликт с портом хоста и ложный
  ICMP port-unreachable, но не заменяет Internet/PMTU canary. Для TCP прежние
  ограничения владения портами и требования подавления RST не изменились.
- На iOS non-DNS UDP сохраняет прежний fallback на TCP, пока приложение явно
  не вызовет `OpenFluxTunSetUDPEnabled(1)` для совместимого exit. При переходе
  на старый exit верните `0`. QUIC на физическом устройстве ещё не проверен.
- Большинство document/WebSocket-транспортов надёжные и упорядоченные. UDP
  через них работает, но потеря carrier-frame может вызвать head-of-line
  blocking; это не эквивалент нативного datagram-транспорта.

### Выбор кодека

По умолчанию транспорт использует батчированный + zstd кодек
(`transport/batched.go` + `transport/framing.go`). Для старого per-packet
LZ4-кодека передайте `--codec=legacy`:

```
./openflux --role=client --codec=legacy ...
```

**Важно:** batched и legacy LZ4 по-прежнему несовместимы. По умолчанию batched
остаётся v2. Старый `OPENFLUX_EXPERIMENTAL_WIRE_V3=1` теперь вызывает ошибку
запуска. Новый режим включается на обоих обновлённых CLI-узлах:

```
--codec=batched --encryption-key-file=/path/to/secret.txt --negotiate
```

Согласуются IPv4/TCP/UDP, передача ICMP-ошибок и максимальный размер IP-пакета.
L4 не объявляет raw ICMP forwarding. При несовместимости или отсутствии ответа
клиент прекращает попытки через 20 секунд, без перехода на менее защищённый
режим; выходная нода ждёт клиента сколько угодно. `--max-packet-size`
принимает 1280-65000 (по умолчанию 65000); это лимит полного IP-пакета, не MTU
Интернета.

Переподключения транспорта сессию сохраняют. Перезапущенный узел принимается
снова без перезапуска второго: hello от неизвестного отправителя получает
challenge, созданный только для него, и сессия заменяется, только когда этот
challenge вернули в ответ. Поэтому старый трафик, переигранный из транспорта
(шифротекст видит любой, у кого есть доступ к документу), сессию не собьёт.
Выходная нода обслуживает одного активного клиента за раз. Клиент, чья нода
замолчала на всех транспортах, сам начинает новый handshake примерно в течение
минуты. Новый обмен ключами и forward secrecy не реализованы.

Текущие iOS-сборки не включают этот режим: для них exit запускается без
`--negotiate`, UDP остаётся ручной настройкой. Детали формата: [спецификация](PROTOCOL_NEGOTIATION.md).

### Сессии с несколькими транспортами

Несколько транспортов в одной согласованной сессии, например прямое
TCP-подключение к выходной ноде и документ Яндекса как запасной канал:

```
# Выходная нода: direct на :8445 плюс документ
./openflux --role=exit --mode=l3 --negotiate \
    --transports=direct:100,yandex:50 --direct-listen=0.0.0.0:8445 \
    --encryption-key-file=secret.txt --url="YOUR_YANDEX_DOC_URL"

# Клиент
./openflux --role=client --inbound=socks5 --negotiate \
    --transports=direct:100,yandex:50 --direct-dial=EXIT_IP:8445 \
    --encryption-key-file=secret.txt --url="YOUR_YANDEX_DOC_URL"
```

- Все транспорты стартуют сразу. Тот, что не смог запуститься (например,
  из-за капчи), перезапускается в фоне с нарастающей паузой.
- Приоритет - это порядок переключения: трафик идёт по самому приоритетному
  транспорту из тех, что доходят до другой стороны. Транспорты с одинаковым
  приоритетом делят потоки между собой.
- Транспорт считается рабочим, только пока с той стороны по нему что-то
  приходит (молчащие пингуются), а не просто пока он подключён к своему
  документу. Со старыми версиями всё работает как раньше.
- Транспорты называются по своему типу. Ссылки на документы по типам:
  `--yandex-url`, `--vyandex-url`, `--boards-url`, `--mailru-url`,
  `--cupsonline-url`; для MAX - `--oneme-token` / `--oneme-uid`. Если
  `--yandex-url` не задан, для `yandex` используется `--url`.
- `--url` - это ещё и контекст шифрования: у обеих сторон он должен совпадать.
- Для `direct` порт выходной ноды должен быть доступен клиенту (откройте его в
  firewall); `direct` работает только в сессии.

То же самое файлом `.conf` (`./openflux --config=client.conf`; флаги из
командной строки важнее файла):

```
[Interface]
Role = client
Inbound = socks5
EncryptionKeyFile = secret.txt
URL = YOUR_YANDEX_DOC_URL

[Transport "direct"]
Priority = 100
Dial = EXIT_IP:8445

[Transport "yandex"]
Priority = 50
URL = YOUR_YANDEX_DOC_URL
```

Ключи `[Interface]`: `Role`, `Inbound`, `Transport`, `Mode`, `Codec`,
`Socks5`, `EncryptionKeyFile`, `CookieStore`, `IPCSocket`, `URL`, `Debug`.
Секции транспортов: `Type` (по умолчанию имя секции), `Priority` (по умолчанию
50), `URL`, `Dial` / `Listen` (direct) и `Token` / `UID` (MAX). `.conf` с
секциями транспортов всегда запускается как согласованная сессия.

### Раздача выходной ноды через QR-код

Запустите выходную ноду с `--share`, и она напечатает ссылку `openflux://` и
её QR-код (в терминале или в логе сервиса). Клиент сканирует его или
открывает ссылку и получает транспорты ноды, приоритеты, режим сессии, ключ и
контекст шифрования, а `direct` указывает на саму ноду:

```
./openflux --role=exit --mode=l3 --negotiate \
    --transports=direct:100,yandex:50 --direct-listen=0.0.0.0:8445 \
    --encryption-key-file=secret.txt --url="YOUR_YANDEX_DOC_URL" \
    --share --share-host=EXIT_PUBLIC_IP
```

- В ссылке лежит ключ шифрования: обращайтесь с ней и с QR-кодом как с файлом
  ключа.
- `--share-host` - адрес, по которому клиенты подключаются к `direct`; по
  умолчанию первый публичный IPv4 хоста.
- MAX в ссылку не попадает (токен принадлежит одному аккаунту), Cups.online
  тоже, если комнаты создаются при старте.
- Формат и отрисовка QR находятся в пакете `share` (`Encode`, `Decode`,
  `PNG`, `Bitmap`, `Terminal`), им могут пользоваться и приложения.

### Капча

- **PoW-капчу** (`showcaptchafast`) транспорт решает сам, ничего делать не
  нужно.
- **SmartCaptcha или требование входа на своём транспорте клиента**: с
  `--ipc-socket=PATH` ядро просит приложение (`CookiesRequest`), приложение
  открывает страницу во встроенном браузере и отвечает cookies
  (`CookiesOffer`); транспорт применяет их и переподключается.
- **То же на выходной ноде**: нода сообщает об этом клиенту управляющим
  сообщением по любому транспорту, который ещё работает (например, `direct`,
  пока застрял документ). Клиент передаёт это приложению как `CookiesRequest`
  с `remote: true` и `proxy`: локальным HTTP-прокси, соединения которого
  выходят через туннель и ноду, так что проверка проходится с адреса ноды.
  Приложение отвечает с `remote: true`, и нода применяет cookies. TCP-стек
  прокси делит адрес туннеля и использует локальные порты 12000-12999.
- На практике настоящий браузер с адреса ноды обычно пускают к документу
  сразу (капча нацелена на HTTP-клиент транспорта), так что обычно достаточно
  загрузить страницу и отправить её cookies.
- Cookies сохраняются в `--cookie-store` (по умолчанию
  `./cookies-<transport>.json`) и используются после перезапуска. Под systemd
  с `ProtectSystem=strict` укажите папку, доступную на запись.

### Шифрование (опционально)

```
./openflux ... --encryption-key-file=/path/to/secret.txt
```

Обе стороны должны использовать один и тот же файл-секрет. AES-256-GCM,
направленные ключи. Без флага - без шифрования, поведение не меняется.

### Бенчмарки

Измерьте чистый goodput через транспорт, не задевая сеть хоста:

```
# Отправитель: залить 100 MB
./openflux --role=bench-send --bench-bytes=100 --transport=yandex --url="..."

# Приёмник: измерить goodput
./openflux --role=bench-sink --transport=yandex --url="..."
```

### Другие транспорты

```
# Yandex Volga (HTTP relay + WS)
./openflux --role=exit --mode=l3 --transport=vyandex --url="..." --debug

# MAX / OneMe (WebRTC DataChannel)
./openflux --role=exit --mode=l3 --transport=oneme \
    --maxToken="..." --maxUid="..." --debug

# Cups.online (Centrifugo-комнаты)
./openflux --role=exit --mode=l3 --transport=cupsonline --debug
# печатает base64-список комнат; передайте его клиенту через --url

# Yandex Board (WS)
./openflux --role=exit --mode=l3 --transport=boards --url="..." --debug

# Mail.ru Docs (WS)
./openflux --role=exit --mode=l3 --transport=mailru \
    --url="YOUR_MAILRU_PUBLIC_LINK" --debug
# принимает как голый weblink (AbCdEfGh1/IjKlMnOp2), так и полный URL
# (https://cloud.mail.ru/public/AbCdEfGh1/IjKlMnOp2)
```

## Флаги

| Флаг | Короткий | По умолчанию | Описание |
|------|----------|--------------|----------|
| `--role` | `-r` | `client` | `client` \| `exit` \| `bench-send` \| `bench-sink` |
| `--inbound` | `-i` | (платформа) | `tun` (macOS) \| `socks5` |
| `--transport` | `-t` | `yandex` | `yandex` \| `vyandex` \| `boards` \| `oneme` \| `cupsonline` \| `mailru` |
| `--mode` | `-m` | `l3` | Режим выходной ноды: `l3` \| `l4` |
| `--codec` | `-c` | `batched` | `batched` \| `legacy` |
| `--url` | `-u` | `http://#` | URL документа |
| `--socks5` | `-s` | `:1080` | Адрес SOCKS5-прокси |
| `--local-ip` | `-l` | (авто) | Egress IP для l3 SNAT / фильтра RST |
| `--debug` | `-d`, `-dd`, `-ddd` | `0` | `1`: строка на каждый пакет (`-> 52 bytes - UDP ...`); `2`: плюс рабочие логи; `3`: плюс hexdump |
| `--sensitive` | | `false` | Ещё и ключи, а с `-ddd` — расшифрованные фреймы (куки, токены) |
| `--encryption-key-file` | | | Файл с общим секретом для AES-256-GCM |
| `--maxToken` | | | Токен авторизации MAX (`--transport=oneme`) |
| `--maxUid` | | | ID пользователя MAX (`--transport=oneme`) |
| `--bench-bytes` | | `0` | Сколько MB залить (`--role=bench-send`) |
| `--bench-compressible` | | `false` | Сжимаемый payload (bench) |
| `--negotiate` | | `false` | Аутентифицированная сессия (на обеих сторонах) |
| `--max-packet-size` | | `65000` | Максимальный IPv4-пакет в сессии (1280..65000) |
| `--transports` | | | Транспорты сессии с приоритетами, например `direct:100,yandex:50` |
| `--direct-dial` | | | Адрес выходной ноды для `direct` (клиент) |
| `--direct-listen` | | | Адрес прослушивания для `direct` (выходная нода) |
| `--yandex-url`, `--vyandex-url`, `--boards-url`, `--mailru-url`, `--cupsonline-url` | | | Ссылка на документ по типу транспорта в сессии |
| `--oneme-token`, `--oneme-uid` | | | Данные MAX в сессии |
| `--config` | | | Файл `.conf`; флаги важнее него |
| `--cookie-store` | | `./cookies-<transport>.json` | Файл с cookies |
| `--ipc-socket` | | | Unix-сокет для приложения (запросы капчи, cookies) |
| `--share` | | `false` | Выходная нода: напечатать ссылку `openflux://` и QR-код для клиентов |
| `--share-host` | | (первый публичный IPv4) | Выходная нода: адрес для `direct` в этой ссылке |

Устаревшие (оставлены на один релиз, автоматически маппятся на новые флаги):
`--client`, `--exit-node`, `--tun`, `--socks5-mode`, `--legacy`,
`--bench-send`, `--bench-sink`.

## Реализация собственных транспортов

Реализуйте интерфейс `Transport` из `transport/transport.go` и
зарегистрируйте свой транспорт в `transport_factory.go` (сессии,
`--transports`) и в `switch` по `--transport` в `main.go` (режим с одним
транспортом); полный пример - `transport/mailru/`. Батчированный кодек
(`BatchedTransport`) оборачивает любой транспорт - новый бэкенд получает
батчинг бесплатно. Чтобы участвовать в обработке капчи, реализуйте также
`transport.ErrorNotifier` и `transport.CookieExchanger`.

## TODO

- **L3-выход на Windows и macOS.** Сейчас L3-выход работает только на Linux
  (SOCK_RAW); Windows и macOS используют `--mode=l4`. Пакет
  `tunnel/windivert/` (Windows) есть, но к L3-форвардеру пока не подключён.
  Нативный L3-выход для macOS не реализован.
- **Запуск выходной ноды (QEMU).**

## Лицензия

GNU General Public License v3.0 or later. Полный текст - в файле LICENSE.

Лицензии третьих сторон - в файле [NOTICE](NOTICE).
