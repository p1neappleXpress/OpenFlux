# openflux:// v2 — ссылка, которая настраивает всё

Статус: спецификация. Код формата живёт в ядре (`share/share.go`, `--parse-link` /
`--make-link`), приложение и Android только передают JSON в ядро и обратно
(`CoreShareLinkCodec`), поэтому все правки формата — в Go, приложения учатся новым
полям без перекомпиляции.

## Чего не хватает v1

Сейчас `Config` несёт только: `name`, `negotiate`, `codec`, `secret`, `context`,
`mode`, `transports[{type,name,url,priority,dial}]`. Из этого нельзя:

- задать **параметры JS-транспорта** (ключ комнаты, токен, имя канала, путь к скрипту) —
  сейчас это кодируется внутрь `url` строкой, а у JS-транспортов параметры объект;
- включить/выключить транспорт, не удаляя его;
- передать настройки узла (таймауты, размеры батча) и адрес выхода;
- отличить «транспорт для данных» от «транспорта для управления».

## Формат

`openflux://v2/` + `base64url(deflate(json))` — та же обёртка, что v1 (сжатие и
нормализация те же), меняется только тело и префикс.

```jsonc
{
  "v": 2,                       // версия тела; отсутствие = 1 legacy
  "name": "Мой выход",          // имя профиля
  "secret": "<hex>",            // общий секрет сессии
  "context": "<строка>",        // контекст шифрования (== exit --url)
  "negotiate": true,            // аутентифицированная сессия
  "codec": "batched",           // "batched" | "legacy"
  "mode": "",                   // "" | "stream"
  "node": "host:port",          // опционально: адрес выхода для direct-сценариев
  "transports": [
    {
      "type": "cbir",           // тип; JS-транспорты — по имени их манифеста
      "name": "cbir · картинки",
      "priority": 100,          // больше — важнее; cbir = 100
      "enabled": true,
      "url": "",                // если транспорту нужен один адрес
      "params": {               // ← новое: произвольные параметры JS-транспорта
        "room": "…",
        "token": "…"
      },
      "role": "data"            // "data" | "control" | "both" (по умолчанию both)
    }
  ],
  "settings": {                 // ← новое: то, что раньше задавалось флагами
    "handshakeTimeout": "600s",
    "batchBytes": 65536,
    "dialTimeout": "30s"
  }
}
```

Правила:

- **v2 обязана читаться v1-клиентами как «слишком новая»** — префикс отличается, старый
  `Decode` вернёт `CodeUnsupportedVersion` («обновите OpenFlux»), а не мусор.
- **v1 остаётся навсегда**: `Decode` принимает оба префикса, `Encode` пишет v2.
- `params` — `map[string]string` (значения строками: JSON-числа и булевы в ссылке не
  нужны, а строки не ломают совместимость).
- `priority` сортирует транспорты в приложении; `role=data` означает «по этому
  транспорту идёт трафик», `role=control` — «только управление» (наш случай: cups
  как control, cbir как data).
- `settings` — необязательные; отсутствие = значения по умолчанию ядра.

## Что меняется в коде

1. `share/share.go`
   - `const PrefixV1 = "openflux://v1/"`, `const PrefixV2 = "openflux://v2/"`, `Prefix = PrefixV2`.
   - `type Transport` += `Enabled *bool`, `Params map[string]string`, `Role string`.
   - `type Config` += `V int`, `Node string`, `Settings map[string]string`.
   - `Encode` ставит `v:2`, пишет `PrefixV2`.
   - `Decode` выбирает версию по префиксу, для v1 оставляет прежнюю валидацию,
     для v2 — новую (`ValidateV2`: `role` из перечисления, `params` без пустых ключей,
     `priority` в разумных пределах, `settings` только известные ключи).
2. `share/share_test.go` — тесты: v1-ссылка читается, v2 читается, v2 в v1-клиенте даёт
   `unsupported_version`, round-trip сохраняет `params`/`role`/`settings`, ссылка с
   `cbir` проходит `Shareable`.
3. `cmd/` (`--make-link`, `--parse-link`) — без изменений интерфейса, только новое тело.
4. Приложение: `ShareConfig` (Kotlin) += `transports[].params/role/enabled`, `settings`,
   `node`; прокидываются в `Provision`/`Profile`. Ядро уже отдаёт JSON — правки чисто
   декларативные.
5. Документация: `docs/transports.md` (как описывать JS-транспорт в ссылке),
   `docs/cli-reference.md` (`--make-link` с новыми полями).

## Порядок работ

1. Go: типы + `Encode`/`Decode` v2 + тесты (без сети).
2. Go: `Shareable`/`Validate` для JS-типов (тип считается известным, если он есть в
   каталоге JS-транспортов, а не в `knownTypes`).
3. Kotlin: поля в `ShareConfig`, прокид в профиль, тест `CoreLinksContractTest`.
4. Сборка APK и проверка: ссылка открывается, транспорты встают по приоритету,
   `cbir` первым.

## Открытые вопросы

- Как ядро узнаёт каталог JS-транспортов, чтобы валидировать `type`? Вариант: список
  разрешённых типов приходит в `--parse-link` вместе с конфигом приложения
  (`OPENFLUX_JS_TRANSPORTS="cbir,vkteams"`).
- `params` секретны? Если да — их надо шифровать общим секретом; сейчас ссылка целиком
  и так секретна (в ней лежит `secret`), так что `params` в открытом виде.
