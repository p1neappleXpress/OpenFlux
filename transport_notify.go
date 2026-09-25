package main

import (
	"encoding/json"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"universal-bypass-tool/transport"
	"universal-bypass-tool/transport/yandex"
	"universal-bypass-tool/utils"
)

// notifyTransportError печатает человекочитаемую причину для случаев, когда
// транспорт сам себе помочь не может: интерактивная SmartCaptcha или редирект
// на логин. Раньше это выглядело как молчаливый бесконечный реконнект — самая
// дорогая в диагностике ситуация (см. ноды, накрутившие 2700 попыток).
//
// Печатаем через log, а не utils.Debugf: сообщение должно быть видно без
// --debug, а на iOS log перенаправлен в кольцевой буфер и доезжает до панели
// журнала в приложении.
func notifyTransportError(err error, transportName, url, reason string) {
	switch reason {
	case "smartcaptcha":
		log.Printf("!! %s: Яндекс требует интерактивную капчу (SmartCaptcha) для %s", transportName, url)
		log.Printf("!! Внутренний PoW-солвер её не проходит. Откройте документ в браузере, " +
			"пройдите капчу и передайте свежие куки (ApplyCookies), либо смените IP/документ.")
	case "login":
		log.Printf("!! %s: документ %s требует входа (редирект на паспорт) — он не публичен с этого адреса", transportName, url)
	default:
		log.Printf("!! %s: %v (%s)", transportName, err, reason)
	}
}

// ---- реестр живых Yandex.Docs транспортов ----
//
// Нужен, чтобы ApplyCookies дошёл до транспорта снаружи: к моменту вызова он
// завёрнут в Adaptive/Multiplex/Encrypted, и достать его из обёрток нельзя.
// Реестр сбрасывается при каждом старте, иначе в него копились бы транспорты
// прошлых сессий.
var (
	ydocsMu     sync.Mutex
	ydocsActive []*yandex.YandexDocsTransport

	// captchaPending — URL документа, упёршегося в SmartCaptcha У НАС. Решать с
	// ВЫКЛЮЧЕННЫМ туннелем (иначе страница не загрузится: пакеты уходят в мёртвый
	// туннель) и применять локально.
	captchaPending string

	// remoteCaptchaPending — капча у ПИРА (ноды). Решать наоборот, с ПОДНЯТЫМ
	// туннелем: проверка привязывается к адресу, который её прошёл, а куки нужны
	// годные для адреса ноды. С погашенным туннелем выход был бы с адреса
	// телефона, и ноде такие куки бесполезны.
	remoteCaptchaPending string
)

// resetYandexRegistry вызывается перед сборкой нового стека транспортов.
func resetYandexRegistry() {
	ydocsMu.Lock()
	ydocsActive = nil
	captchaPending = ""
	ydocsMu.Unlock()
}

// newYandexDocs собирает Yandex.Docs транспорт с подключённым уведомителем.
// Общая точка для CLI и обоих iOS-мостов, чтобы причина не терялась ни в одном
// из них.
func newYandexDocs(u string, config transport.TransportConfig) transport.Transport {
	t := yandex.NewYandexDocsTransport(u, config)
	t.SetErrorNotifier(func(err error, transportName, url, reason string) {
		if reason == "smartcaptcha" {
			ydocsMu.Lock()
			captchaPending = url
			ydocsMu.Unlock()
			// Просим куки сразу: если сессия ещё жива, они доедут и нода
			// переживёт следующий реконнект.
			requestCookies(reason)
			reportAuthRequired("yandex", url, reason)
		}
		notifyTransportError(err, transportName, url, reason)
	})
	ydocsMu.Lock()
	ydocsActive = append(ydocsActive, t)
	seed := initialCookies
	ydocsMu.Unlock()

	// Засеваем до Start: транспорт ещё не запущен, поэтому ApplyCookies просто
	// наполняет банку и никакого реконнекта не планирует.
	if seed != "" {
		if values := parseCookieHeader(seed); len(values) > 0 {
			if err := t.ApplyCookies(values); err != nil {
				log.Printf("seed cookies: %v", err)
			}
		}
	}
	return t
}

// initialCookies — куки, добытые ДО старта туннеля (пользователь прошёл капчу
// при выключенном VPN). Засеваются в банку транспорта при создании, поэтому
// первый же fetchDocInfo идёт уже с ними: иначе он гарантированно упёрся бы в
// ту же капчу, а ApplyCookies после старта означал бы лишний реконнект.
var initialCookies string

// setInitialCookies задаёт (или очищает пустой строкой) эти куки.
func setInitialCookies(raw string) {
	ydocsMu.Lock()
	initialCookies = strings.TrimSpace(raw)
	n := len(parseCookieHeader(initialCookies))
	ydocsMu.Unlock()
	if n > 0 {
		log.Printf("Captcha cookies preloaded (%d): %s — will seed the transport",
			n, cookieNames(parseCookieHeader(initialCookies)))
	}
}

// pendingCaptchaURL — URL, для которого нужна интерактивная капча ("" = нет).
//
// Живое соединение снимает флаг: капча могла отвалиться сама (сменился IP,
// истёк её срок), и тогда никто бы флаг не сбросил — баннер висел бы поверх
// рабочего туннеля.
func pendingCaptchaURL() string {
	ydocsMu.Lock()
	defer ydocsMu.Unlock()
	if captchaPending == "" {
		return ""
	}
	for _, t := range ydocsActive {
		if t.IsConnected() {
			captchaPending = ""
			return ""
		}
	}
	return captchaPending
}

// applyCookiesToYandex раздаёт куки всем живым Yandex.Docs транспортам. Куки —
// в формате заголовка Cookie ("a=1; b=2"), как их отдаёт WebView.
//
// Раздаём всем, а не только тому, чей URL совпал: при мультиплексе документы
// живут на одном домене, и куки капчи привязаны к домену, а не к документу.
func applyCookiesToYandex(rawCookies string) int {
	values := parseCookieHeader(rawCookies)
	if len(values) == 0 {
		return 0
	}
	ydocsMu.Lock()
	targets := make([]*yandex.YandexDocsTransport, len(ydocsActive))
	copy(targets, ydocsActive)
	captchaPending = ""
	ydocsMu.Unlock()

	applied := 0
	for _, t := range targets {
		if err := t.ApplyCookies(values); err != nil {
			log.Printf("apply cookies: %v", err)
			continue
		}
		applied++
	}
	log.Printf("Captcha cookies applied to %d transport(s): %s", applied, cookieNames(values))
	return applied
}

// cookiesFilePath — путь, заданный --cookies-file. Нужен не только для чтения:
// нода, получившая куки по контрольному каналу, пишет их сюда, и тогда их
// подхватывают ДРУГИЕ процессы на той же машине. Это единственный способ
// передать куки между сервисами: у каждого свой процесс и свой реестр, а
// spravka привязана к IP машины, который у них общий.
var cookiesFilePath atomic.Pointer[string]

func setCookiesFilePath(path string) {
	if path == "" {
		return
	}
	cookiesFilePath.Store(&path)
}

// persistCookies пишет куки в файл --cookies-file, если он задан. Пишем только
// при изменении: watcher того же процесса иначе увидел бы свою же запись и
// применил её по кругу.
func persistCookies(header string) {
	p := cookiesFilePath.Load()
	if p == nil || *p == "" || header == "" {
		return
	}
	if old, err := os.ReadFile(*p); err == nil && strings.TrimSpace(string(old)) == header {
		return
	}
	if err := os.WriteFile(*p, []byte(header+"\n"), 0o600); err != nil {
		log.Printf("persist cookies to %s: %v", *p, err)
		return
	}
	log.Printf("Cookies written to %s for the other services on this host", *p)
}

// watchCookiesFile следит за файлом с куками и применяет его содержимое к живым
// Yandex.Docs транспортам — при старте (если файл уже есть) и дальше при каждом
// изменении.
//
// Это единственный способ пройти SmartCaptcha на exit-ноде: PoW она считает
// сама, а интерактивную капчу решить не может — нет ни браузера, ни человека.
// Оператор видит причину в журнале, проходит капчу в браузере у себя, кладёт
// строку кук в этот файл, и нода подхватывает её на ходу.
//
// Следим, а не читаем однократно при старте: капча возникает на уже работающей
// ноде, и перезапуск ради кук сбросил бы все туннели.
//
// Формат файла — как заголовок Cookie: "a=1; b=2" (то, что даёт devtools
// «copy as cURL»). Действует только на транспорт yandex: остальные в реестр не
// попадают.
func watchCookiesFile(path string) {
	var lastMod time.Time
	var lastSize int64

	apply := func(why string) {
		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("cookies file: %v", err)
			return
		}
		raw := strings.TrimSpace(string(data))
		if raw == "" {
			return
		}
		log.Printf("Cookies file %s (%s): applying", path, why)
		applyCookiesToYandex(raw)
	}

	if st, err := os.Stat(path); err == nil {
		lastMod, lastSize = st.ModTime(), st.Size()
		apply("present at startup")
	} else {
		log.Printf("Cookies file %s not present yet — will pick it up when it appears", path)
	}

	for {
		time.Sleep(3 * time.Second)
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		// И mtime, и размер: запись через ">" на той же секунде меняет размер,
		// но может не сдвинуть mtime с секундной гранулярностью.
		if st.ModTime().Equal(lastMod) && st.Size() == lastSize {
			continue
		}
		lastMod, lastSize = st.ModTime(), st.Size()
		apply("changed")
	}
}

func parseCookieHeader(raw string) map[string]string {
	out := make(map[string]string)
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.IndexByte(part, '=')
		if eq <= 0 {
			continue
		}
		name := strings.TrimSpace(part[:eq])
		value := strings.TrimSpace(part[eq+1:])
		if name != "" {
			out[name] = value
		}
	}
	return out
}

// ---- обмен куками по контрольному каналу ----
//
// Нода не может пройти интерактивную капчу (нет браузера), клиент может.
// Поэтому нода просит куки, клиент отдаёт. Границы применимости — в
// transport.ControlTransport: пока у ноды жива сессия, канал есть; если она уже
// не может открыть документ, спасать поздно, нужен --cookies-file. Отсюда и
// правило «слать заранее»: клиент отдаёт куки сам, как только видит канал, а не
// ждёт просьбы.

var (
	ctrlMu   sync.Mutex
	ctrlConn *transport.ControlTransport
	ctrlExit bool
)

// wrapControl ставит контрольный слой снаружи всего остального (но внутри
// шифрования, если оно есть), чтобы контрольные кадры шифровались как данные.
func wrapControl(inner transport.Transport, exitNode bool) transport.Transport {
	ct := transport.NewControlTransport(inner)
	ctrlMu.Lock()
	ctrlConn = ct
	ctrlExit = exitNode
	ctrlMu.Unlock()

	ct.SetControlHandler(func(typ byte, body []byte) {
		var p transport.CookiesPayload
		if err := json.Unmarshal(body, &p); err != nil {
			utils.Debugf("[CTRL] bad payload type=%d: %v", typ, err)
			return
		}
		switch typ {
		case transport.CtrlCookiesRequest:
			if exitNode {
				return // просить должна нода, а не отвечать на просьбу
			}
			log.Printf("Peer asked for cookies (%s) — sending", p.Reason)
			offerCookies()
		case transport.CtrlAuthRequired:
			if exitNode {
				return // сообщает нода, реагирует клиент
			}
			var ap transport.AuthRequiredPayload
			if err := json.Unmarshal(body, &ap); err != nil || ap.URL == "" {
				return
			}
			ydocsMu.Lock()
			remoteCaptchaPending = ap.URL
			ydocsMu.Unlock()
			log.Printf("!! Узел не может пройти проверку для %s (%s)", ap.URL, ap.Reason)
			log.Printf("!! Пройдите её НЕ отключая туннель — куки должны быть выданы на адрес узла.")
		case transport.CtrlCookiesOffer:
			if !exitNode {
				return // на клиенте свои куки лучше присланных
			}
			if len(p.Cookies) == 0 {
				return
			}
			log.Printf("Peer sent %d cookies (%s) — applying", len(p.Cookies), p.Reason)
			header := cookieHeaderFrom(p.Cookies)
			applyCookiesToYandex(header)
			// Ключевое: раздаём куки и соседним сервисам на этой же машине.
			// Клиент прошёл проверку через туннель, значит spravka выдана на
			// наш IP и годится всем процессам здесь, не только этому.
			persistCookies(header)
		}
	})
	return ct
}

func cookieHeaderFrom(values map[string]string) string {
	parts := make([]string, 0, len(values))
	for k, v := range values {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, "; ")
}

// requestCookies — нода упёрлась в капчу и просит куки у клиента. Отправка
// провалится, если сессии уже нет; это ожидаемо и не ошибка.
func requestCookies(reason string) {
	ctrlMu.Lock()
	ct, isExit := ctrlConn, ctrlExit
	ctrlMu.Unlock()
	if ct == nil || !isExit {
		return
	}
	if err := ct.SendControl(transport.CtrlCookiesRequest,
		transport.CookiesPayload{Reason: reason}); err != nil {
		utils.Debugf("[CTRL] cookie request not sent (no live channel): %v", err)
		return
	}
	log.Printf("Asked the client for cookies (%s)", reason)
}

// currentCookies — что клиент может предложить: сначала то, что добыто снаружи
// (капча пройдена в браузере/WebView), иначе живая банка транспорта.
func currentCookies() map[string]string {
	ydocsMu.Lock()
	seed := initialCookies
	targets := make([]*yandex.YandexDocsTransport, len(ydocsActive))
	copy(targets, ydocsActive)
	ydocsMu.Unlock()

	if v := parseCookieHeader(seed); len(v) > 0 {
		return v
	}
	for _, t := range targets {
		if v, err := t.FetchCookies(); err == nil && len(v) > 0 {
			return v
		}
	}
	return nil
}

// offerCookies — клиент отдаёт куки ноде.
func offerCookies() {
	ctrlMu.Lock()
	ct, isExit := ctrlConn, ctrlExit
	ctrlMu.Unlock()
	if ct == nil || isExit {
		return
	}
	values := currentCookies()
	if len(values) == 0 {
		// Нечего отдать — пусть UI попросит пользователя пройти капчу.
		ydocsMu.Lock()
		if captchaPending == "" && len(ydocsActive) > 0 {
			captchaPending = ydocsActive[0].URL()
		}
		ydocsMu.Unlock()
		log.Printf("Peer needs cookies but we have none — user must pass the captcha")
		return
	}
	if !ct.IsConnected() {
		utils.Debugf("[CTRL] cookie offer skipped: carrier not connected")
		return
	}
	if err := ct.SendControl(transport.CtrlCookiesOffer,
		transport.CookiesPayload{Reason: "client", Cookies: values}); err != nil {
		utils.Debugf("[CTRL] cookie offer not sent: %v", err)
		return
	}
	log.Printf("Sent %d cookies to the peer", len(values))
}

// startCookieOffering — клиентская проактивная отдача. Смысл именно в «заранее»:
// к моменту, когда нода упрётся в капчу, у неё уже должны быть свежие куки, ведь
// просить будет поздно — канала не станет.
func startCookieOffering() {
	ctrlMu.Lock()
	isExit := ctrlExit
	ctrlMu.Unlock()
	if isExit {
		return
	}
	utils.SafeGo("cookieOffering", func() {
		for {
			time.Sleep(10 * time.Minute)
			if len(currentCookies()) > 0 {
				offerCookies()
			}
		}
	})
}

// reportAuthRequired — нода сообщает клиенту, ГДЕ она застряла, чтобы тот прошёл
// проверку через туннель (с адреса ноды) и отдал куки.
func reportAuthRequired(name, url, reason string) {
	ctrlMu.Lock()
	ct, isExit := ctrlConn, ctrlExit
	ctrlMu.Unlock()
	if ct == nil || !isExit {
		return
	}
	if err := ct.SendControl(transport.CtrlAuthRequired,
		transport.AuthRequiredPayload{Transport: name, URL: url, Reason: reason}); err != nil {
		utils.Debugf("[CTRL] AuthRequired not sent (no live channel): %v", err)
	}
}

// pendingRemoteCaptchaURL — адрес, для которого проверку должна пройти наша
// сторона в интересах ноды ("" = нечего проходить).
func pendingRemoteCaptchaURL() string {
	ydocsMu.Lock()
	defer ydocsMu.Unlock()
	return remoteCaptchaPending
}

// offerCookiesToPeer отдаёт ноде куки, добытые через туннель. Возвращает число
// переданных кук.
func offerCookiesToPeer(rawCookies string) int {
	values := parseCookieHeader(rawCookies)
	if len(values) == 0 {
		return 0
	}
	ctrlMu.Lock()
	ct, isExit := ctrlConn, ctrlExit
	ctrlMu.Unlock()
	if ct == nil || isExit {
		return 0
	}
	// Проверяем канал ДО отправки: Send у кодека лишь ставит кадр в очередь и
	// возвращает nil, поэтому «успех» без живого носителя ничего не значит —
	// раньше лог рапортовал об отправке в мёртвый транспорт.
	if !ct.IsConnected() {
		log.Printf("!! Куки НЕ отправлены: носитель не подключён. Подключитесь " +
			"рабочим транспортом (direct/boards) — выход должен идти с адреса узла.")
		return 0
	}
	if err := ct.SendControl(transport.CtrlCookiesOffer,
		transport.CookiesPayload{Reason: "solved-through-tunnel", Cookies: values}); err != nil {
		log.Printf("offer cookies to peer: %v", err)
		return 0
	}
	ydocsMu.Lock()
	remoteCaptchaPending = ""
	ydocsMu.Unlock()
	log.Printf("Sent %d cookies to the exit node (solved through the tunnel)", len(values))
	return len(values)
}

// cookieNames — только имена, без значений: значения это секреты, а для
// диагностики важно лишь, есть ли среди них нужное (у Яндекса — spravka).
func cookieNames(values map[string]string) string {
	names := make([]string, 0, len(values))
	for k := range values {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}
