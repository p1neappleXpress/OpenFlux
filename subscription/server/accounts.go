package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

const (
	minPasswordRunes = 10
	failWindow       = 15 * time.Minute
	maxFails         = 5
)

var loginRe = regexp.MustCompile(`^[\p{L}\p{N}._@+-]{3,64}$`)

// hashPassword is bcrypt over the SHA-256 of the password, so a long password
// is not cut at bcrypt's 72 bytes.
func hashPassword(p string, cost int) (string, error) {
	h, err := bcrypt.GenerateFromPassword(prehash(p), cost)
	return string(h), err
}

func checkPassword(hash, p string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), prehash(p)) == nil
}

func prehash(p string) []byte {
	sum := sha256.Sum256([]byte(p))
	return []byte(base64.StdEncoding.EncodeToString(sum[:]))
}

func validLogin(l string) bool { return loginRe.MatchString(l) }

func validPassword(p, login string) (string, bool) {
	switch {
	case utf8.RuneCountInString(p) < minPasswordRunes:
		return "Пароль не короче 10 символов.", false
	case len(p) > 256:
		return "Пароль слишком длинный.", false
	case normLogin(p) == normLogin(login):
		return "Пароль не должен совпадать с логином.", false
	}
	return "", true
}

// newRecoveryKey is 20 random bytes in groups of four letters: easy to write down.
func newRecoveryKey() string {
	raw := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(randToken(15)))[:32]
	var parts []string
	for i := 0; i < len(raw); i += 4 {
		parts = append(parts, raw[i:i+4])
	}
	return strings.Join(parts, "-")
}

func normRecovery(k string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(k)))
}

func deviceName(n string) string {
	n = strings.TrimSpace(n)
	if n == "" {
		return "Устройство"
	}
	if r := []rune(n); len(r) > 60 {
		n = string(r[:60])
	}
	return n
}

// ---- JSON shapes ----

type deviceJSON struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"createdAt"`
	LastSeen  int64  `json:"lastSeen"`
	Current   bool   `json:"current,omitempty"`
}

type accountJSON struct {
	ID     string `json:"id"`
	Login  string `json:"login"`
	Status string `json:"status"` // active, expired, revoked
	// Expires is unix seconds; 0 means the account has never had a subscription.
	Expires        int64        `json:"expires"`
	Plan           *planInfo    `json:"plan,omitempty"`
	TrialAvailable bool         `json:"trialAvailable"`
	Devices        []deviceJSON `json:"devices"`
	DeviceLimit    int          `json:"deviceLimit"`
	Support        string       `json:"support,omitempty"`
}

type planInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Devices int    `json:"devices"`
}

type authJSON struct {
	DeviceToken string      `json:"deviceToken"`
	DeviceID    string      `json:"deviceId"`
	RecoveryKey string      `json:"recoveryKey,omitempty"`
	Account     accountJSON `json:"account"`
}

func devicesJSON(u *User, current string) []deviceJSON {
	out := make([]deviceJSON, 0, len(u.Devices))
	for _, d := range u.Devices {
		out = append(out, deviceJSON{ID: d.ID, Name: d.Name, CreatedAt: d.CreatedAt, LastSeen: d.LastSeen, Current: d.ID == current})
	}
	return out
}

// account describes u; the caller holds the lock.
func (s *Server) account(u *User, currentDevice string) accountJSON {
	a := accountJSON{
		ID: u.ID, Login: u.Login, Status: s.userStatus(u), Expires: u.Expires,
		Devices: devicesJSON(u, currentDevice), DeviceLimit: s.deviceLimit(u), Support: s.cfg.Support,
	}
	if p, ok := s.cfg.plan(u.PlanID); ok {
		a.Plan = &planInfo{p.ID, p.Name, p.Devices}
	}
	_, hasTrial := s.cfg.trialPlan()
	a.TrialAvailable = hasTrial && !u.TrialUsed && !u.Banned
	return a
}

func (s *Server) deviceLimit(u *User) int {
	if p, ok := s.cfg.plan(u.PlanID); ok {
		return p.Devices
	}
	if p, ok := s.cfg.trialPlan(); ok {
		return p.Devices
	}
	return s.cfg.Plans[0].Devices
}

// addDevice signs the account in on a new device; the caller holds the lock.
// At the device limit it replaces replaceID when given, else reports the limit.
func (s *Server) addDevice(u *User, name, replaceID string) (token, id string, limit bool) {
	if len(u.Devices) >= s.deviceLimit(u) {
		idx := -1
		for i, d := range u.Devices {
			if replaceID != "" && d.ID == replaceID {
				idx = i
			}
		}
		if idx < 0 {
			return "", "", true
		}
		u.Devices = append(u.Devices[:idx], u.Devices[idx+1:]...)
	}
	token, id = randToken(32), newID()
	now := s.unix()
	u.Devices = append(u.Devices, Device{ID: id, Name: deviceName(name), TokenHash: hashToken(token), CreatedAt: now, LastSeen: now})
	return token, id, false
}

func (s *Server) startTrial(u *User) bool {
	p, ok := s.cfg.trialPlan()
	if !ok || u.TrialUsed {
		return false
	}
	u.TrialUsed = true
	u.PlanID = p.ID
	u.Expires = s.now().Add(time.Duration(p.Days) * 24 * time.Hour).Unix()
	return true
}

// ---- register, login, recover ----

type authReq struct {
	Login           string `json:"login"`
	Password        string `json:"password"`
	DeviceName      string `json:"deviceName"`
	ReplaceDeviceID string `json:"replaceDeviceId"`
	RecoveryKey     string `json:"recoveryKey"`
	NewPassword     string `json:"newPassword"`
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req authReq
	if readJSON(r, &req) != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "Неверный запрос.")
		return
	}
	ip := clientIP(r)
	if s.lim.count("reg:"+ip, time.Hour) >= 5 {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "Слишком много регистраций с этого адреса, попробуйте позже.")
		return
	}
	login := normLogin(req.Login)
	if !validLogin(login) {
		writeErr(w, http.StatusBadRequest, "bad_login", "Логин: от 3 до 64 символов, буквы, цифры и . _ @ + -")
		return
	}
	if msg, ok := validPassword(req.Password, login); !ok {
		writeErr(w, http.StatusBadRequest, "weak_password", msg)
		return
	}
	hash, err := hashPassword(req.Password, s.cost())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "Не удалось создать аккаунт.")
		return
	}
	recovery := newRecoveryKey()
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	if s.st.userByLogin(login) != nil {
		writeErr(w, http.StatusConflict, "login_taken", "Такой логин уже занят.")
		return
	}
	u := &User{ID: newID(), Login: login, PassHash: hash, RecoveryHash: hashToken(normRecovery(recovery)), CreatedAt: s.unix()}
	if s.cfg.AutoTrial {
		s.startTrial(u)
	}
	token, id, _ := s.addDevice(u, req.DeviceName, "")
	s.st.state.Users[u.ID] = u
	if !s.save(w) {
		delete(s.st.state.Users, u.ID)
		return
	}
	s.lim.add("reg:"+ip, time.Hour)
	writeJSON(w, http.StatusCreated, authJSON{DeviceToken: token, DeviceID: id, RecoveryKey: recovery, Account: s.account(u, id)})
}

// credential checks login and password, with the attempt limits. It returns the
// account id, or writes the error. The bcrypt work is outside the lock.
func (s *Server) credential(w http.ResponseWriter, r *http.Request, login, password string) (string, bool) {
	login = normLogin(login)
	ip := clientIP(r)
	if s.lim.count("login:"+login, failWindow) >= maxFails || s.lim.count("ip:"+ip, failWindow) >= maxFails*4 {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "Слишком много попыток, подождите 15 минут.")
		return "", false
	}
	s.st.mu.Lock()
	u := s.st.userByLogin(login)
	hash, id := s.dummyHash, ""
	if u != nil {
		hash, id = u.PassHash, u.ID
	}
	s.st.mu.Unlock()
	ok := checkPassword(hash, password)
	if id == "" || !ok {
		s.lim.add("login:"+login, failWindow)
		s.lim.add("ip:"+ip, failWindow)
		writeErr(w, http.StatusUnauthorized, "invalid_credentials", "Неверный логин или пароль.")
		return "", false
	}
	s.lim.reset("login:" + login)
	return id, true
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req authReq
	if readJSON(r, &req) != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "Неверный запрос.")
		return
	}
	id, ok := s.credential(w, r, req.Login, req.Password)
	if !ok {
		return
	}
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	u := s.st.state.Users[id]
	if u == nil {
		writeErr(w, http.StatusUnauthorized, "invalid_credentials", "Неверный логин или пароль.")
		return
	}
	if u.Banned {
		writeErr(w, http.StatusForbidden, "banned", "Доступ отключён. Обратитесь в поддержку.")
		return
	}
	s.finishAuth(w, u, req.DeviceName, req.ReplaceDeviceID, "", http.StatusOK)
}

// finishAuth adds the device and answers; the caller holds the lock.
func (s *Server) finishAuth(w http.ResponseWriter, u *User, name, replaceID, recovery string, status int) {
	token, id, limit := s.addDevice(u, name, replaceID)
	if limit {
		writeJSON(w, http.StatusConflict, apiError{
			Error:   "device_limit",
			Message: "Достигнут предел устройств. Уберите одно из списка.",
			Devices: devicesJSON(u, ""),
		})
		return
	}
	if !s.save(w) {
		return
	}
	writeJSON(w, status, authJSON{DeviceToken: token, DeviceID: id, RecoveryKey: recovery, Account: s.account(u, id)})
}

func (s *Server) handleRecover(w http.ResponseWriter, r *http.Request) {
	var req authReq
	if readJSON(r, &req) != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "Неверный запрос.")
		return
	}
	login := normLogin(req.Login)
	ip := clientIP(r)
	if s.lim.count("login:"+login, failWindow) >= maxFails || s.lim.count("ip:"+ip, failWindow) >= maxFails*4 {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "Слишком много попыток, подождите 15 минут.")
		return
	}
	if msg, ok := validPassword(req.NewPassword, login); !ok {
		writeErr(w, http.StatusBadRequest, "weak_password", msg)
		return
	}
	hash, err := hashPassword(req.NewPassword, s.cost())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "Не удалось сменить пароль.")
		return
	}
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	u := s.st.userByLogin(login)
	want := hashToken(normRecovery(req.RecoveryKey))
	if u == nil || subtle.ConstantTimeCompare([]byte(u.RecoveryHash), []byte(want)) != 1 {
		s.lim.add("login:"+login, failWindow)
		s.lim.add("ip:"+ip, failWindow)
		writeErr(w, http.StatusUnauthorized, "invalid_credentials", "Неверный логин или ключ восстановления.")
		return
	}
	if u.Banned {
		writeErr(w, http.StatusForbidden, "banned", "Доступ отключён. Обратитесь в поддержку.")
		return
	}
	// A new password and a new recovery key; every device signs in again.
	recovery := newRecoveryKey()
	u.PassHash, u.RecoveryHash, u.Devices = hash, hashToken(normRecovery(recovery)), nil
	s.lim.reset("login:" + login)
	s.finishAuth(w, u, req.DeviceName, "", recovery, http.StatusOK)
}

// ---- the signed-in account ----

func (s *Server) handleAccount(w http.ResponseWriter, _ *http.Request, u *User, d *Device) {
	writeJSON(w, http.StatusOK, s.account(u, d.ID))
}

func (s *Server) handleSubscription(w http.ResponseWriter, r *http.Request, u *User, _ *Device) {
	writeJSON(w, http.StatusOK, s.document(s.userStatus(u), u.Expires, s.userPools(u), u.ID, clientTransports(r)))
}

func (s *Server) handleTrial(w http.ResponseWriter, r *http.Request, u *User, d *Device) {
	ip := clientIP(r)
	if s.lim.count("trial:"+ip, 24*time.Hour) >= 3 {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "Пробный период с этого адреса уже выдавали, обратитесь в поддержку.")
		return
	}
	if !s.startTrial(u) {
		writeErr(w, http.StatusConflict, "no_trial", "Пробный период недоступен.")
		return
	}
	if !s.save(w) {
		return
	}
	s.lim.add("trial:"+ip, 24*time.Hour)
	writeJSON(w, http.StatusOK, s.account(u, d.ID))
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request, u *User, d *Device) {
	var req authReq
	if readJSON(r, &req) != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "Неверный запрос.")
		return
	}
	if s.lim.count("login:"+u.Login, failWindow) >= maxFails {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "Слишком много попыток, подождите 15 минут.")
		return
	}
	if msg, ok := validPassword(req.NewPassword, u.Login); !ok {
		writeErr(w, http.StatusBadRequest, "weak_password", msg)
		return
	}
	// bcrypt takes a moment: the lock is not held for it by other requests' sake.
	oldHash := u.PassHash
	s.st.mu.Unlock()
	okOld := checkPassword(oldHash, req.Password)
	hash, herr := hashPassword(req.NewPassword, s.cost())
	s.st.mu.Lock()
	if !okOld {
		s.lim.add("login:"+u.Login, failWindow)
		writeErr(w, http.StatusUnauthorized, "invalid_credentials", "Неверный пароль.")
		return
	}
	if herr != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "Не удалось сменить пароль.")
		return
	}
	u.PassHash = hash
	// The other devices sign in again with the new password.
	for i := range u.Devices {
		if u.Devices[i].ID == d.ID {
			u.Devices = []Device{u.Devices[i]}
			break
		}
	}
	if s.save(w) {
		writeJSON(w, http.StatusOK, s.account(u, u.Devices[0].ID))
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, _ *http.Request, u *User, d *Device) {
	s.removeDevice(u, d.ID)
	if s.save(w) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func (s *Server) handleRemoveDevice(w http.ResponseWriter, r *http.Request, u *User, d *Device) {
	if !s.removeDevice(u, r.PathValue("id")) {
		writeErr(w, http.StatusNotFound, "not_found", "Такого устройства нет.")
		return
	}
	if s.save(w) {
		// d may now point past the end of the slice: describe the account for the caller's device by id.
		writeJSON(w, http.StatusOK, s.account(u, ""))
	}
}

func (s *Server) removeDevice(u *User, id string) bool {
	for i := range u.Devices {
		if u.Devices[i].ID == id {
			u.Devices = append(u.Devices[:i], u.Devices[i+1:]...)
			return true
		}
	}
	return false
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request, u *User, _ *Device) {
	var req authReq
	if readJSON(r, &req) != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "Неверный запрос.")
		return
	}
	if s.lim.count("login:"+u.Login, failWindow) >= maxFails {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "Слишком много попыток, подождите 15 минут.")
		return
	}
	hash, id, login := u.PassHash, u.ID, u.Login
	s.st.mu.Unlock()
	ok := checkPassword(hash, req.Password)
	s.st.mu.Lock()
	if !ok {
		s.lim.add("login:"+login, failWindow)
		writeErr(w, http.StatusUnauthorized, "invalid_credentials", "Неверный пароль.")
		return
	}
	delete(s.st.state.Users, id)
	for oid, o := range s.st.state.Orders {
		if o.UserID == id {
			delete(s.st.state.Orders, oid)
		}
	}
	if s.save(w) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// ---- orders ----

func (s *Server) handleCheckout(w http.ResponseWriter, r *http.Request, u *User, _ *Device) {
	var req struct {
		PlanID string `json:"planId"`
	}
	if readJSON(r, &req) != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "Неверный запрос.")
		return
	}
	p, ok := s.cfg.plan(req.PlanID)
	if !ok || p.Trial {
		writeErr(w, http.StatusNotFound, "not_found", "Такого тарифа нет.")
		return
	}
	// One open order per plan: asking again returns it.
	var order *Order
	for _, o := range s.st.state.Orders {
		if o.UserID == u.ID && o.PlanID == p.ID && o.Status == "pending" {
			order = o
		}
	}
	if order == nil {
		order = &Order{ID: newID(), UserID: u.ID, PlanID: p.ID, Status: "pending", CreatedAt: s.unix()}
		s.st.state.Orders[order.ID] = order
		if !s.save(w) {
			delete(s.st.state.Orders, order.ID)
			return
		}
	}
	writeJSON(w, http.StatusOK, s.orderJSON(order, u))
}

func (s *Server) handleOrder(w http.ResponseWriter, r *http.Request, u *User, _ *Device) {
	o := s.st.state.Orders[r.PathValue("id")]
	if o == nil || o.UserID != u.ID {
		writeErr(w, http.StatusNotFound, "not_found", "Такого заказа нет.")
		return
	}
	writeJSON(w, http.StatusOK, s.orderJSON(o, u))
}

func (s *Server) orderJSON(o *Order, u *User) map[string]any {
	out := map[string]any{"orderId": o.ID, "planId": o.PlanID, "status": o.Status, "expires": u.Expires}
	if o.Status == "pending" {
		// The manual provider has no page to send people to: the owner confirms the payment.
		out["paymentUrl"] = ""
		out["instructions"] = s.cfg.Payment.Instructions
	}
	return out
}

// markPaid extends the account by the order's plan; the caller holds the lock.
func (s *Server) markPaid(o *Order) error {
	if o.Status == "paid" {
		return nil
	}
	u := s.st.state.Users[o.UserID]
	p, ok := s.cfg.plan(o.PlanID)
	if u == nil || !ok {
		return errUnknownOrder
	}
	s.extend(u, p)
	o.Status, o.PaidAt = "paid", s.unix()
	return nil
}

// extend adds the plan's days from the later of now and the current end.
func (s *Server) extend(u *User, p Plan) {
	from := s.now()
	if cur := time.Unix(u.Expires, 0); u.Expires != 0 && cur.After(from) {
		from = cur
	}
	u.Expires = from.Add(time.Duration(p.Days) * 24 * time.Hour).Unix()
	u.PlanID = p.ID
}
