# OpenFlux

[English](README.md) | **Русский**

Исследовательский инструмент сетевого стека. TCP-туннель с подключаемыми транспортами.

## Обзор
```
Client (SOCKS5) --> Transport --> Exit Node --> Internet
```

## Требования
1. Golang v. 1.26.3+ — требуется для сборки бинарника десктопного клиента / выходной ноды (universal-bypass-tool);
2. Android Native Development Kit (NDK) v.27.0.12077973+ — требуется для сборки бинарника для Android-клиента;
3. XCode v. 26.6+ — требуется для сборки бинарника для iOS-клиента;
4. VPS / VDS выходная нода на Linux.

## Обзор

TCP-пакеты передаются через Transport. На данный момент доступны два транспорта:
1. Yandex — отправляет пакеты через курсорные сообщения Yandex Docs;
2. Max — отправляет пакеты через WebRTC DataChannel.

Клиентская часть запускает SOCKS5-прокси, выходная нода декапсулирует и пересылает пакеты в пункт назначения.

## Структура

```
OpenFlux/
├── main.go                     # Точка входа CLI (клиент / выходная нода)
├── export_ios.go               # cgo-мост для статической библиотеки iOS (build tag: ios)
├── transport/
│   ├── transport.go            # Интерфейс Transport
│   ├── compressor.go           # Обёртка сжатия
│   ├── yandex/                 # Бэкенд Yandex Docs
│   └── oneme/                  # Бэкенд MAX Messenger
├── tunnel/
│   ├── tunnel.go               # Ядро TCP-тоннеля
│   ├── endpoint.go             # Виртуальный NIC
│   └── rawsocket_{linux,darwin,windows}.go  # Raw-сокет (выходная нода), по ОС
├── socks5/                     # SOCKS5-сервер
├── network/                    # Контрольные суммы, разбор пакетов
├── utils/                      # Логирование
├── ios-app/                    # iOS-клиент на SwiftUI (XcodeGen), линкует liboflux.a
├── build_ios.sh                # Сборка статической библиотеки iOS (liboflux.a)
├── build_ios_app.sh            # Сборка + архив + экспорт IPA приложения iOS
└── build_android.sh            # Сборка клиентского бинарника Android
```

## Сборка (бинарник десктоп-клиента / выходной ноды)

```bash
go mod tidy
go build -o universal-bypass-tool .
```

## Сборка для Android (клиентский бинарник)
```bash
export ANDROID_NDK_HOME=<путь до вашего Android NDK>
./build_android.sh
```

## Сборка для iOS (клиентский бинарник)
```bash
export XCODE_PATH="<путь до вашего Xcode.app>" # опционально, по умолчанию /Applications/Xcode.app
./build_ios.sh
```

## Использование

### 1. Настройка выходной ноды
1. У вас должен быть root-доступ выходной ноде;
2. Поддерживается только устаревший редактор документов Yandex (переключается в настройках интерфейса).

Команды для настройки выходной ноды:
```bash
sudo iptables -A OUTPUT -p tcp --tcp-flags RST RST -j DROP
sudo ./universal-bypass-tool --exit-node --url "YOUR_YANDEX_DOC_URL" --debug
```

### 1. Настройка десктопного клиента:

Команды для настройки десктопного клиента:
```bash
./universal-bypass-tool --client --url "YOUR_YANDEX_DOC_URL" --socks5 :1080 --debug
```

Затем настройте SOCKS5-прокси в браузере на localhost:1080.

## Флаги

| Флаг          | По умолчанию        | Описание                       |
|---------------|---------------------|--------------------------------|
| `--client`    |                     | Запуск в режиме клиента        |
| `--exit-node` |                     | Запуск в режиме ноды           |
| `--socks5`    | `:1080`             | Адрес SOCKS5 прокси            |
| `--url`       | `https://localhost` | URL документа (Yandex Docs)    |
| `--maxToken`  | ``                  | Токен авторизации (Max)        |
| `--maxUid`    | ``                  | ID пользователя (Max)          |
| `--debug`     | `false`             | Включить подробное логирование |
| `--transport` | `yandex`            | Выбор транспорта               |

## Реализация собственных транспортов

Вы можете реализовать интерфейс `Transport` из `transport/transport.go` и зарегистрировать свой транспорт в switch-блоке в main.go.

## Лицензия

Проект распространяется под лицензией **GNU General Public License v3.0 or later**.
Полный текст — в файле [LICENSE](LICENSE).

Лицензии третьих сторон — в файле [NOTICE](NOTICE).

## Дисклеймер

Только для образовательного использования. Тестируйте на собственных машинах и сетях.
