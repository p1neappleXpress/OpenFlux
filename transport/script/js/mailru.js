// Script-transport port of transport/mailru/mailru.go. Line-for-line the
// same protocol: tunnels packets through the chat ("message") channel of
// Mail.ru's public-document coauthoring WebSocket. Every constant, timing,
// and message shape below matches the native transport exactly - this file
// is a faithfulness test for the host API, not a rewrite of the protocol.
//
// Why the chat channel and not the cursor field: the current Mail.ru server
// (R7/OnlyOffice 2026.2.1.2268) validates cursor and saveChanges writes
// against the live document model and closes the socket ("access deny",
// code 4004) on any synthetic one. The chat channel is free-form text the
// server broadcasts verbatim to every other participant without validating
// it against the document, so it is the carrier. The old cursor keep-alive
// and saveChanges "editor activity" are gone; the Socket.IO ping/pong keeps
// the WebSocket up on its own.
//
// The Go<->JS packet boundary (write()/emit()) carries raw bytes as
// ArrayBuffers; the chat message needs those bytes as base64 text behind the
// CHAT_DATA_TAG marker, so this script calls the host's base64.encode/decode
// explicitly at that one seam - fast (native Go codec, no hand-rolled JS
// base64) and the only place bytes<->text conversion happens at all.

var USER_AGENT =
  "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36";
// CHAT_DATA_TAG prefixes every tunnel payload carried over the chat channel,
// so a receiver can tell our frames from a human's chat message and skip
// anything it cannot decode as ours. Same as chatDataTag in mailru.go.
var CHAT_DATA_TAG = "OFX1";
var MAX_RECONNECT_ATTEMPTS = 999999; // transport.DefaultConfig().MaxReconnectAttempts

var weblink = "";
var running = false;
var sock = null;
var baseUserID = "";
var userID = null;
var userCounter = 0;
var connectedAt = null;
var docInfo = null; // the live session's fetchDocInfo result (editor user id for self-echo filtering)
var ownID = ""; // docInfo.editorUserId: the server echoes our own chat back, skip it
var connectGen = 0; // bumped by every connectToDoc: a stale attempt closes what it opened
var reconnectTimer = null; // at most one reconnect pending

function pad(n, width) {
  var s = String(n);
  while (s.length < width) s = "0" + s;
  return s;
}

function randUserID() {
  return pad(Math.floor(Math.random() * 1000000000), 10);
}

// decodeChatData extracts tunnel packets from a server chat broadcast
//
//   42["message",{"type":"message","messages":[{"message":"…","useridoriginal":"…"},…]}]
//
// It keeps only entries from other participants (useridoriginal != ownID, since
// the server echoes every chat back to its own sender) whose text carries
// CHAT_DATA_TAG, and base64-decodes the remainder. Human chat and presence
// entries (no tag) are ignored. Same logic as decodeChatData in mailru.go.
function decodeChatData(text, own) {
  var i = text.indexOf("[");
  if (i < 0) return [];
  var arr;
  try {
    arr = JSON.parse(text.slice(i));
  } catch (e) {
    return [];
  }
  if (!arr || arr.length < 2) return [];
  var payload = arr[1];
  if (!payload || payload.type !== "message" || !payload.messages) return [];
  var out = [];
  for (var k = 0; k < payload.messages.length; k++) {
    var m = payload.messages[k];
    if (!m || typeof m.message !== "string") continue;
    if (own && m.useridoriginal === own) continue; // the server's echo of our own chat
    if (m.message.indexOf(CHAT_DATA_TAG) !== 0) continue; // a human's chat, or a presence entry
    var bytes;
    try {
      bytes = base64.decode(m.message.slice(CHAT_DATA_TAG.length));
    } catch (e) {
      continue;
    }
    out.push(bytes);
  }
  return out;
}

// normalizeWeblink accepts either a bare weblink ("AbCdEfGh1/IjKlMnOp2") or
// a full public URL, same as NewMailruDocsTransport.
function normalizeWeblink(link) {
  link = String(link).trim();
  var prefixes = [
    "https://cloud.mail.ru/public/",
    "http://cloud.mail.ru/public/",
    "https://cloud.mail.ru/",
    "http://cloud.mail.ru/",
  ];
  for (var i = 0; i < prefixes.length; i++) {
    if (link.indexOf(prefixes[i]) === 0) {
      return link.slice(prefixes[i].length).replace(/^\/+|\/+$/g, "");
    }
  }
  return link;
}

// reconnectBackoff: exponential with jitter, capped at 15s - identical
// formula to reconnectBackoff() in mailru.go.
function reconnectBackoff(n) {
  if (n < 1) n = 1;
  var shift = n - 1;
  if (shift > 5) shift = 5;
  var d = 500 * Math.pow(2, shift);
  if (d > 15000) d = 15000;
  d += Math.floor(Math.random() * (d / 2 + 1));
  return d;
}

function scheduleReconnect(attempt) {
  var next = attempt + 1;
  if (!running || next >= MAX_RECONNECT_ATTEMPTS) return;
  if (reconnectTimer) return; // one is already pending: a second would open a duplicate session
  var d = reconnectBackoff(next);
  reconnectTimer = setTimeout(function () {
    reconnectTimer = null;
    if (!running) return;
    connectToDoc(next);
  }, d);
}

// fetchDocInfo POSTs to Mail.ru's public-document editor API and returns
// the fields needed to open the collaborative WebSocket - same endpoint,
// headers and field extraction as fetchDocInfo() in mailru.go.
async function fetchDocInfo(link) {
  var body = JSON.stringify({
    "x-email": "anonym",
    public: "/" + link,
    platform: "desktop_web",
  });
  var res = await http.fetch({
    url: "https://cloud.mail.ru/api/v4/r7/edit",
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Accept: "application/json, text/plain, */*",
      "User-Agent": USER_AGENT,
      "X-Api-Version": "4",
      Referer: "https://cloud.mail.ru/public/" + link + "?weblink=" + link,
    },
    body: body,
  });
  if (res.status !== 200) {
    throw new Error("mailru: api returned status " + res.status);
  }
  var data = JSON.parse(res.body);
  var doc = data.document;
  if (!doc) throw new Error("mailru: document object missing");
  var editorConfig = data.editorConfig;
  if (!editorConfig) throw new Error("mailru: editorConfig object missing");

  var apiBase = data.api || "";
  var wsBase = apiBase.replace("https://", "wss://");
  return {
    token: data.token || "",
    docKey: doc.key,
    wsURL: wsBase + "/doc/" + doc.key + "/c/?EIO=4&transport=websocket",
    fileType: doc.fileType,
    docUrl: doc.url,
    docTitle: doc.title,
    // document.permissions is an object of booleans, not a number -
    // sending it as anything else makes the editor server reject the auth
    // message with "access deny" (same caveat as the native transport).
    permissions: doc.permissions || {},
    callbackUrl: editorConfig.callbackUrl || "",
    editorUserId: (editorConfig.user && editorConfig.user.id) || "",
  };
}

function handleMessage(text) {
  // Socket.IO ping/pong.
  if (text === "2") {
    if (sock) {
      try {
        sock.send("3");
      } catch (e) {}
    }
    return;
  }
  if (text === "3") return;

  if (text.indexOf('"type":"auth"') !== -1 && text.indexOf('"result":1') !== -1) {
    return; // auth ack - fire-and-forget, same as the native transport
  }

  // Tunnel data arrives as a chat broadcast the server relays from the other
  // participant:
  //   42["message",{"type":"message","messages":[{message,useridoriginal,…}]}]
  // decodeChatData keeps only our-tagged frames from other participants.
  if (text.indexOf('"messages"') !== -1) {
    var packets = decodeChatData(text, ownID);
    for (var i = 0; i < packets.length; i++) {
      emit(packets[i]);
    }
  }
}

function onSocketClose(attempt) {
  sock = null;
  docInfo = null;
  ownID = "";
  var next = attempt;
  // A connection that had been up for >15s gets the fast (attempt=1)
  // backoff on its next try instead of continuing to climb - identical to
  // the `next = -1` branch in mailru.go's read-error handler.
  if (connectedAt !== null && Date.now() - connectedAt > 15000) {
    next = -1;
  }
  if (running) setState("reconnecting");
  scheduleReconnect(next);
}

function connectToDoc(attempt) {
  if (!running) return;
  var myGen = ++connectGen;

  (async function () {
    try {
      var info = await fetchDocInfo(weblink);
      if (myGen !== connectGen || !running) return; // superseded while fetching
      var newSock = await ws.open(
        info.wsURL,
        {
          "User-Agent": USER_AGENT,
          // A real browser always sends Accept-Language; some Mail.ru edge
          // nodes answer 409 to the upgrade without it (issue #164).
          "Accept-Language": "en-US,en;q=0.9",
          Origin: "https://docs.datacloudmail.ru",
        },
        // Mail.ru's nginx requires permessage-deflate on the upgrade or it
        // answers 409 Conflict (issue #163). The native transport sets
        // EnableCompression for the same reason.
        { compress: true }
      );
      if (myGen !== connectGen || !running) {
        try {
          newSock.close();
        } catch (e) {}
        return;
      }

      sock = newSock;
      docInfo = info;
      ownID = info.editorUserId;
      if (userID === null) {
        userID = baseUserID + pad(userCounter++ % 1000, 3);
      }

      sock.onmessage = handleMessage;
      sock.onclose = function () {
        if (sock !== newSock) return; // a socket we already replaced or dropped on purpose
        onSocketClose(attempt);
      };

      // Auth fires immediately, same as the native transport: Mail.ru's
      // coauthoring server buffers these until its own session state
      // catches up, and waiting for an explicit ack here only stretches
      // the outage window on every reconnect.
      sock.send('40{"token":"' + info.token + '"}');

      var authMsg = {
        type: "auth",
        docid: info.docKey,
        documentCallbackUrl: info.callbackUrl,
        token: "fghhfgsjdgfjs",
        user: { id: info.editorUserId, username: userID, indexUser: -1 },
        editorType: 0,
        lastOtherSaveTime: -1,
        block: [],
        documentFormatSave: 65,
        view: false,
        isCloseCoAuthoring: false,
        openCmd: {
          c: "open",
          id: info.docKey,
          userid: info.editorUserId,
          format: info.fileType,
          url: info.docUrl,
          title: info.docTitle,
          lcid: 25,
          nobase64: true,
          convertToOrigin: ".pdf.xps.oxps.djvu",
        },
        lang: "ru",
        mode: "edit",
        permissions: info.permissions,
        IsAnonymousUser: false,
        timezoneOffset: -180,
        coEditingMode: "fast",
        jwtOpen: info.token,
        time: 1000,
        supportAuthChangesAck: true,
      };
      sock.send('42' + JSON.stringify(["message", authMsg]));

      connectedAt = Date.now();
      setState("connected");
    } catch (e) {
      setState("reconnecting", String(e));
      scheduleReconnect(attempt);
    }
  })();
}

var Transport = {
  info: function () {
    return {
      name: "mailru",
      version: "1.2.0",
      cookieDomain: "https://cloud.mail.ru/",
      mtu: 0, // unbounded - native mailru never fragments either
      reliable: false,
      ordered: false,
      halfDuplex: false,
      minIntervalMs: 0,
      params: [
        {
          key: "url",
          label: "Mail.ru public document link or weblink",
          type: "url",
          required: true,
        },
      ],
    };
  },

  open: function (cfg) {
    var raw = (cfg.params && cfg.params.url) || cfg.url || "";
    weblink = normalizeWeblink(raw);
    baseUserID = randUserID();
    running = true;
    connectToDoc(0);
  },

  write: function (bytes) {
    if (!sock) throw new Error("mailru: not connected");
    // Ride the document's chat channel: the server broadcasts a chat
    // "message" verbatim to every other participant without validating it
    // against the document model (unlike cursor/saveChanges, now rejected).
    // The CHAT_DATA_TAG marker tells our frames apart from a human's chat and
    // from the server's echo of our own messages. base64 is JSON-safe text.
    sock.send('42["message",{"type":"message","message":"' + CHAT_DATA_TAG + base64.encode(bytes) + '"}]');
  },

  close: function () {
    running = false;
    connectGen++;
    if (reconnectTimer) clearTimeout(reconnectTimer);
    reconnectTimer = null;
    if (sock) {
      try {
        sock.close();
      } catch (e) {}
    }
    sock = null;
    docInfo = null;
    ownID = "";
  },

  // Generic cookie exchange (see host.go / transport.go FetchCookies /
  // ApplyCookies): Go reads cookies straight out of the shared jar with no
  // help from this script, but only the script can make an already-open
  // socket pick up freshly applied cookies - so it reacts to this event by
  // forcing a reconnect, same as ApplyCookies forcing t.session = nil in
  // the native transport.
  onEvent: function (kind) {
    if (kind === "cookiesApplied") {
      userID = null;
      docInfo = null;
      ownID = "";
      connectGen++; // drop an attempt in flight: the one below starts from the new cookies
      var old = sock;
      sock = null; // its onclose is ignored (sock !== newSock): one reconnect, below
      if (old) {
        try {
          old.close();
        } catch (e) {}
      }
      setState("reconnecting");
      scheduleReconnect(0);
    }
  },
};
