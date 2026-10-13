# Авто-создание выходной ноды пула подписки (дизайн)

Дата: 2026-10-11.
Статус: согласован.

## Проблема

Канал единственного пула подписки (`main` → `fork-all`) удалён с сервера
(известный инцидент 2026-10-11). `opensub` не мог собрать профили, отвечал
`"profiles": null`, а приложения не могли декодировать ответ
(«Сервис ответил непонятно…», показан последний список).

Уже сделанные исправления (обе стороны):
- сервис больше не отдаёт `null` — `profiles()` возвращает `[]`
  (`subscription/server/links.go`);
- приложение читает и `"profiles":null` как пустой список
  (`Json coerceInputValues`, `HttpSubscriptionApi`).

Осталось главное: у пула должна быть рабочая нода, и создавать её должен сам
сервер, а не владелец вручную через мастер «Своя нода».

## Цель

1. Выходная нода подписки живёт на нашем сервере (где `opensub`).
2. `opensub` сам провижинит канал, когда его файлов нет: ключ, порт,
   транспорты, systemd-юнит `openflux-node@<канал>`, запуск ноды.
3. Создание транспортов автоматизировано: комнаты cups.online (публичный
   анонимный сервис, `transport/cupsonline.CreateRoomList`) без аккаунтов.
4. Подписчик снова получает локации после обновления; никакой `null`.

## Конфигурация (`/etc/openflux-sub/config.json`)

`Pool` получает поля:

```json
{
  "id": "main", "name": "Основной", "channel": "fork-all",
  "autoNode": true,
  "transports": [ { "type": "cupsonline" } ],
  "mode": "l4"
}
```

- `autoNode: true` — сервис создаёт на этом хосте ноду канала пула, если её
  файлов нет. По умолчанию `false` (существующее поведение).
- `transports` — тpанспорты канала (как в `provision.ChannelTransport`).
  Транспорт `{ "type": "cupsonline" }` с пустым `url` означает «создать комнаты
  самому». Для `cupsonline` с непустым `url` и для прочих типов URL обязателен
  (проверка как в `CheckTransports`).
  Пустое значение `transports` + `autoNode` = только cups.online-комнаты.
- `mode`: `l4` (по умолчанию) или `l3`.

`server.Config.Check()` дополняется: при `autoNode` каждый транспорт
проходит `provision.CheckTransports` с разрешением пустого URL только для
`cupsonline`; имя канала должно быть валидным для `node-install.sh`.

## Провижининг-компонент

В `subscription/server` добавляется `nodeProvisioner` (интерфейс, в тестах —
фейк):

```go
type nodeProvisioner interface {
    // Ensure ставит канал exit-ноды на этом хосте, если его файлов нет.
    // Возвращает порт или ошибку.
    Ensure(channel string, spec nodeSpec) error
}
```

`nodeSpec`: канал, транспорты (после создания комнат), режим.

Боевая реализация (новый файл `provision.go` в `subscription/server`):

1. Если есть транспорт `cupsonline` с пустым URL — `cupsonline.CreateRoomList`
   (таймаут, как в `NodeCreateCupsRooms`, 2 мин); полученный список комнат
   подставляется в `url` транспорта.
2. Ключ: `provision.NewKey()` (64 hex).
3. Порт: `node-install.sh plan` (порт выбирает скрипт), затем `apply` с
   `port=`.
4. Конфиг для скрипта собирается как в CLI-мастере (`channel=`,
   `cupsonline=`, `key=`, `port=`, `mode=`, `autoupdate=no`) и передаётся
   скрипту через 0600-файл (тот его читает и удаляет).
5. Вызов: `sh <nodeScript> apply <config>`; `nodeScript` — из конфига,
   по умолчанию `/opt/openflux-node/node-install.sh` (на хосте уже v1.4.1 и
   ядро в `/opt/openflux-node/bin`, скрипт его переиспользует).
6. После успешного `apply` нода запущена (`openflux-node@<канал>`), файлы
   `/etc/openflux-node/<канал>/` на месте — `linkCache` подхватит их сам.

## Когда включается

1. **Фон.** `Serve` при старте и далее каждые ~3 минуты обходит все пулы с
   `autoNode`: файлы канала отсутствуют → `Ensure`; есть → пропуск.
   (Это же лечит ситуацию, когда подписчиков ещё не было.)
2. **По запросу.** В `profiles()` канал нужного пула не читается и
   `autoNode` → запустить фоновый `Ensure`, HTTP не блокируем. Ответ до
   готовности канала остаётся `"profiles":[]`.

## Синхронизация и отказы

- Single-flight по имени канала: `sync.Mutex` + `map[string]bool inFlight`,
  повторный вызов на время провижининга игнорируется (не создаёт копии).
- Cooldown после неудачи (~3 мин, `map[string]time.Time`): не долбим
  cups.online при отказе (у cups есть лимиты 403/429).
- Ошибки — только в лог (`s.logf`), документ всё равно `[]`, ничего не падает.
- Повторная проверка файлов ещё раз под замком перед `Ensure`: если
  параллельный путь уже создал канал — пропускаем.

## Линки подписчиков

После провижининга `linkCache.link` видит свежие `node.conf`/`encryption-key`
и выдаёт профили обычным путём; новых полей в `Document`/`Profile` нет.
Подписчик получает локацию при следующем обновлении (вручную «Обновить» или
фоновый ритм `updateHours`).

## Права: почему нужен root и что меняется

Сейчас `openflux-sub` служит с `ProtectSystem=strict`,
`ReadWritePaths=/var/lib/openflux-sub`, `NoNewPrivileges=true` — из под него
нельзя писать в `/etc/openflux-node` и управлять юнитами, а `rotate` работает
только как CLI-команда под root.

Для авто-провижининга `serve` должен иметь root-доступ к файлам каналов и
`systemctl start openflux-node@<канал>`. Решение (вариант 1, согласован):

- `User=root` для `opensub serve` (или отдельный drop-in) и
  `ReadWritePaths` добавить: `/etc/openflux-node`, `/var/lib/openflux-node`,
  `/opt/openflux-node`.
- Остальной песочнице остаться (`ProtectSystem=strict`, `PrivateTmp`,
  `NoNewPrivileges`, `RestrictAddressFamilies`). l3 дополнительно потребует
  `AF_NETLINK` — по умолчанию не нужно (mode l4).
- Корень доступа сервиса сужается до каналов нод; ключи каналов и так
  читаются только root/`openflux-node`.

## Тестирование

- `server_test.go`: фейковый `nodeProvisioner`, фиксирующий вызовы `Ensure`:
  - пул с `autoNode` и отсутствующим каналом → после `Ensure` документ
    содержит профиль (фейк пишет канал как `writeChannel`);
  - single-flight: два параллельных запроса документов → один `Ensure`;
  - cooldown: при ошибке повторный `Ensure` не раньше интервала;
  - `autoNode=false` — прежнее поведение (провижин не вызывается).
- `cupsonline` — комнаты уже покрыты тестами в `transport/cupsonline`.
- Проверка на сервере: `opensub status` показывает пул `main` в `ok` ;
  регистрация тестового аккаунта → в документе профиль `main`;
  `openflux-node@fork-all` active; клиент декодирует профиль.

## Связанное

- `subscription/server/links.go` — `profiles()` (уже: `[]` вместо `null`).
- `OpenFluxClientShared` — `coerceInputValues` (уже).