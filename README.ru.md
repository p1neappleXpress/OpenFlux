# OpenFlux

[English](README.md) | **Русский**

Исследовательский инструмент сетевого стека. TCP-туннель с подключаемыми
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
| **iOS**     | [TestFlight бета](https://testflight.apple.com/join/BwnAcdus) | Системный VPN через Network Extension |

> **iOS-приложение** сделано [@saharev1](https://github.com/saharev1) -
> полноценный iOS-клиент, пайплайн TestFlight, системный VPN, DNS-over-TLS
> и множество фиксов стабильности. ОГРОМНОЕ спасибо!
>
> **Android-приложение** - [p1neappleXpress/OpenFluxAndroid](https://github.com/p1neappleXpress/OpenFluxAndroid).

## Архитектура

Любой клиент работает с любым бэкендом выходной ноды. `--mode` выбирается
на **выходной ноде**, а не на клиенте.

```
Клиент (любой): macOS (utun) / Linux / Windows / iOS (packet tunnel) / Android
                    |
                    v
               Транспорт (Yandex.Docs / Volga / MAX / Cups / Mail.ru)
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

В `l3` выходная нода ничего не терминирует: она форвардит сырые IP-пакеты
с SNAT/DNAT (conntrack + фильтр по egress-IP). Одно TCP-соединение
end-to-end между клиентом и реальным сервером.

В `l4` выходная нода терминирует TCP в userspace-стеке gVisor, затем
переподключается к реальному серверу через `net.Dial`. Работает на любой ОС
без root.

Клиент терминирует TCP локально (gVisor, utun или NEPacketTunnelProvider),
затем отправляет сырые IP-пакеты в транспорт.

## Бэкенды выходной ноды

У выходной ноды ровно **два** бэкенда, выбираются флагом `--mode` на
**выходной ноде**. Клиент бэкенд не выбирает - один и тот же клиент
работает с любым из них.

| `--mode` | Бэкенд | Форвардинг | Требует | Платформы |
|----------|--------|-----------|---------|-----------|
| `l3` | Сырой L3 | SNAT/DNAT сырых IPv4-пакетов через SOCK_RAW + conntrack. Без userspace TCP-стека. | root / CAP_NET_RAW | только Linux |
| `l4` (алиас `proxy`) | gVisor proxy | Терминирует TCP в userspace-стеке gVisor, затем `net.Dial` к реальному серверу. | ничего | Linux, macOS, Windows |

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

Дополнительно код L3 сам дропает клиентские RST до `sendto()`, так что
правило выше нужно только для RST, которые генерирует ядро.

## Ключевые особенности

- **Подключаемые транспорты** - Yandex.Docs (WS), Yandex Volga (HTTP relay + WS),
  MAX/OneMe (WebRTC DataChannel), Cups.online (Centrifugo-комнаты),
  Mail.ru Docs (WS).
- **Батчинг + zstd** - склеивает множество туннельных пакетов в одно
  транспортное сообщение. Меньше сообщений в канале, выше скорость. См.
  `transport/batched.go` и `transport/framing.go`.
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
  bench.go                         # Хелперы бенчмарка
  tun_darwin.go                    # macOS utun L3-клиент
  tun_watch.go                     # Watcher сокетов для bypass-маршрутов
  tun_other.go                     # Заглушки для не-darwin платформ
  export_ios.go                    # cgo-мост для iOS-статической библиотеки
  transport/
    transport.go                   # Интерфейс Transport
    batched.go                     # BatchedTransport (склейка + zstd)
    framing.go                     # Wire-формат батчированных кадров
    compressor.go                  # Legacy per-packet LZ4-кодек
    encrypted.go                   # Опциональная AES-256-GCM обёртка
    yandex/                        # Бэкенды Yandex.Docs + Volga
    oneme/                         # Бэкенд MAX Messenger
    cupsonline/                    # Бэкенд Cups.online
    mailru/                        # Бэкенд Mail.ru Docs
  tunnel/
    tunnel.go                      # Клиентский туннель (gVisor + TunnelLinkEndpoint)
    endpoint.go                    # Виртуальный NIC (клиент)
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
    rawsocket_linux.go             # Legacy raw exit (оставлен для референса)
    rawsocket_{darwin,windows}.go  # Заглушки
    windivert/                     # WinDivert-бэкенд (есть, но к L3 не подключён)
  socks5/                          # SOCKS5-сервер (fallback на клиенте)
  network/                         # Контрольные суммы, разбор пакетов
  utils/                           # Логирование
  ios-app/                         # iOS-клиент на SwiftUI (XcodeGen)
  build_ios.sh                     # Сборка статической библиотеки iOS (liboflux.a)
  build_ios_app.sh                 # Сборка + архив + экспорт IPA iOS
  build_android.sh                 # Сборка клиентского бинарника Android
  scripts/
    cleanup-utun.sh                # Удалить stale-маршруты utun (macOS)
    build-flx-linux-img.sh         # Сборка минимального Alpine rootfs для QEMU
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
режим по умолчанию на всех платформах, кроме macOS.

### Выбор кодека

По умолчанию транспорт использует батчированный + zstd кодек
(`transport/batched.go` + `transport/framing.go`). Для старого per-packet
LZ4-кодека передайте `--codec=legacy`:

```
./openflux --role=client --codec=legacy ...
```

**Важно:** батчированный wire-формат НЕ совместим с legacy LZ4.
Клиент и выходная нода должны использовать один и тот же кодек (оба - новые,
либо оба - `--codec=legacy`).

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
| `--transport` | `-t` | `yandex` | `yandex` \| `vyandex` \| `oneme` \| `cupsonline` \| `mailru` |
| `--mode` | `-m` | `l3` | Режим выходной ноды: `l3` \| `l4` |
| `--codec` | `-c` | `batched` | `batched` \| `legacy` |
| `--url` | `-u` | `http://#` | URL документа |
| `--socks5` | `-s` | `:1080` | Адрес SOCKS5-прокси |
| `--upstream-proxy` | | | Вышестоящий SOCKS5-прокси для exit node (включает l4, авто-определение из -s/--socks5) |
| `--local-ip` | `-l` | (авто) | Egress IP для l3 SNAT / фильтра RST |
| `--debug` | `-d` | `false` | Подробное per-packet логирование |
| `--encryption-key-file` | | | Файл с общим секретом для AES-256-GCM |
| `--maxToken` | | | Токен авторизации MAX (`--transport=oneme`) |
| `--maxUid` | | | ID пользователя MAX (`--transport=oneme`) |
| `--bench-bytes` | | `0` | Сколько MB залить (`--role=bench-send`) |
| `--bench-compressible` | | `false` | Сжимаемый payload (bench) |

Устаревшие (оставлены на один релиз, автоматически маппятся на новые флаги):
`--client`, `--exit-node`, `--tun`, `--socks5-mode`, `--legacy`,
`--bench-send`, `--bench-sink`.

## Реализация собственных транспортов

Реализуйте интерфейс `Transport` из `transport/transport.go` и
зарегистрируйте свой транспорт в `switch`-блоке `main.go` (см.
`transport/mailru/` как полный пример). Батчированный кодек
(`BatchedTransport`) оборачивает любой транспорт - новый бэкенд получает
батчинг бесплатно.

## TODO

- **L3-выход на Windows и macOS.** Сейчас L3-выход работает только на Linux
  (SOCK_RAW); Windows и macOS используют `--mode=l4`. Пакет
  `tunnel/windivert/` (Windows) есть, но к L3-форвардеру пока не подключён.
  Нативный L3-выход для macOS не реализован.
- **Запуск выходной ноды (QEMU).**

## Лицензия

GNU General Public License v3.0 or later. Полный текст - в файле LICENSE.

Лицензии третьих сторон - в файле [NOTICE](NOTICE).
