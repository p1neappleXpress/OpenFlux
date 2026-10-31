# transport/ok — OpenFlux carrier через звонки ok.ru (PORTAL)

Каркас по образцу `transport/oneme`. Медиа-слой (videowebrtc + ICE injection)
почти тот же; **отличается только auth + start/accept call**.

## Соответствие файлов

| oneme | ok | Назначение |
|-------|-----|------------|
| max_transport.go | ok_transport.go | Start/Stop/Send, роль exit/client |
| max_wclient.go | ok_wclient.go | Сессия ok.ru (cookies), HTTP API |
| max_call.go | ok_call.go | WebRTC + ws2 videowebrtc |
| max_call_helper.go | ok_call_helper.go | craftEndpoint с clientType=PORTAL |
| max_data.go | ok_data.go | типы |
| max_utils.go | ok_utils.go | log/uuid |

## Поток

```
Exit:  cookies A → (TODO listen incoming) → ws2 PORTAL tgt=accept → DC/ICE tunnel
Client: cookies B → getCallToken/start → ws2 PORTAL tgt=start → uid A → tunnel
```

## Флаги (предложение)

```
--transport=ok
--ok-cookies=<string|path>   # Cookie: AUTHCODE=...; JSESSIONID=...
--ok-uid=<id>                # userId exit (client звонит сюда), напр. 910703757272
```

## getCallToken (снято)

```
POST https://ok.ru/web-api/v2/videochat/getCallToken
body: {"id": 30}
→ {
    "success": true,
    "result": {
      "apiUrl": "https://calls.okcdn.ru",
      "token":  "$0tOAysM...QDYC5aJMwr"
    }
  }
```

`id: 30` — не peer uid. Token — для **calls.okcdn.ru**, не для ws2.

## Следующий HAR (шаг 2)

Фильтр Network: `calls.okcdn.ru` сразу после getCallToken при **исходящем** звонке.

Нужны: URL, method, headers (Authorization?), body, response → conversationId / media token / videowebrtc.

Плюс: как exit узнаёт о **входящем** звонке.


## Start outgoing call (снято) — ok.ru/dk

```
POST/GET https://ok.ru/dk
  cmd=VideoChatCall
  st.call.ft=CALL_AUDIO
  st.call.dir=OUTGOING
  st.call.opponentId=<peer uid>    # e.g. 910703757272
  gwt.requested=<token>
```

Response: HTML panel `data-module="call2"` with:
- data-opponent-id, data-participant-id
- data-params JSON (platform WEB, clientType PORTAL, protocolVersion 3, …)
- js-participants: local + remote USER ids

After this, conversationId/vcid appears and
vchat.getConversationParams can be called on calls.okcdn.ru.


## Incoming call panel (exit) — ok.ru/dk

```
cmd=VideoChatCall
st.call.ft=CALL_VIDEO|CALL_AUDIO
st.call.dir=INCOMING
st.call.conversationId=<uuid>   # already assigned by OK
st.call.opponentId=<caller uid>
gwt.requested=...
```

HTML: `data-conversation-id`, `data-direction="INCOMING"`.

Still TODO: how the page **learns** about ringing before this dk call
(websocket / long-poll / push) — that is the exit WaitIncomingCall source.


## videowebrtc ws2 signaling (PORTAL, tgt=accept) — снято

Connect:
```
wss://videowebrtc.okcdn.ru/ws2?userId=<local>&entityType=USER
  &conversationId=<uuid>&token=<media token>
  &platform=WEB&appVersion=1.1&version=5&device=browser
  &capabilities=3F&clientType=PORTAL&tgt=accept
```

Server → client (JSON notifications), same shape as MAX path:

1. `notification: connection`
   - peerId, endpoint (ws2 with peerId)
   - conversationParams.turn / stun
   - conversation.state=ACTIVE, topology=DIRECT
   - participants[]: externalId.type=ODKL, state CALLED|ACCEPTED, roles CREATOR

2. `notification: settings-update` — camera/screenSharing/badNet

3. `notification: hungup` — reason CANCELED, participantId

4. `notification: closed-conversation`

Tunnel data can reuse oneme ICE-injection / DataChannel on this socket.


## Incoming ring — PeriodicManager (NewsFetchCoordinator.js)

```
POST https://ok.ru/push?cmd=PeriodicManager&gwt.requested=<hash>&sse=true&p_sId=<id>
Content-Type: application/x-www-form-urlencoded
body: blocks=MainFeedsNewFeedPush,PS,VCP,...&p_NLP=<ts>
```

- Long-lived stream (SSE or long-poll).
- Chunks split by `<!--SSP-->` (join marker `<!--SP-->`).
- Header `Rendered-Blocks` lists which blocks were in the chunk.
- **VCP** = Video Call Push → feeds calls bootstrap `onPush` → VideoChatCall INCOMING.

In transport: `OkClient.SetPushConfig` + `WaitIncomingCall` / `WaitIncomingCallPush`
(`ok_push.go`). Adblock often blocks `/push` — disable on ok.ru when capturing.

Exact VCP chunk HTML still best taken from a live EventStream with adblock off.

## Cookie keep-alive

Before `getCallToken`, the client GETs `https://ok.ru/` (and `/messages`) with the
current Cookie jar and merges `Set-Cookie` into `OkClient.cookies`. OK rotates
`JSESSIONID` and `AUTHCODE` this way; without it long-lived exit nodes lose the
calls token even though the browser would still look "logged in".

Requires a full web session (AUTHCODE present). Accounts stuck on
`anonymContactImprovementStart` often get `apiUrl` without `token`.
