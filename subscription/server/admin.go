package server

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
)

var errUnknownOrder = errors.New("the order's account or plan no longer exists")

// adminRoutes is what the owner (and a payment bot) calls with the admin token.
func (s *Server) adminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/info", s.admin(s.adminInfo))
	mux.HandleFunc("GET /admin/users", s.admin(s.adminUsers))
	mux.HandleFunc("POST /admin/users/{login}/grant", s.admin(s.adminGrant))
	mux.HandleFunc("POST /admin/users/{login}/ban", s.admin(s.adminBan))
	mux.HandleFunc("POST /admin/users/{login}/reset-password", s.admin(s.adminResetPassword))
	mux.HandleFunc("DELETE /admin/users/{login}", s.admin(s.adminDeleteUser))
	mux.HandleFunc("POST /admin/keys", s.admin(s.adminAddKey))
	mux.HandleFunc("GET /admin/keys", s.admin(s.adminKeys))
	mux.HandleFunc("POST /admin/keys/{id}/extend", s.admin(s.adminExtendKey))
	mux.HandleFunc("DELETE /admin/keys/{id}", s.admin(s.adminRevokeKey))
	mux.HandleFunc("GET /admin/orders", s.admin(s.adminOrders))
	mux.HandleFunc("POST /admin/orders/{id}/paid", s.admin(s.adminPaid))
}

func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if s.lim.count("adminfail:"+ip, failWindow) >= maxFails {
			writeErr(w, http.StatusTooManyRequests, "rate_limited", "Слишком много попыток.")
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.AdminToken)) != 1 {
			s.lim.add("adminfail:"+ip, failWindow)
			writeErr(w, http.StatusUnauthorized, "unauthorized", "Нужен токен администратора.")
			return
		}
		h(w, r)
	}
}

func (s *Server) adminInfo(w http.ResponseWriter, _ *http.Request) {
	type pool struct {
		ID, Name, Channel string
		OK                bool
		Error             string `json:",omitempty"`
	}
	var pools []pool
	for _, p := range s.cfg.Pools {
		pl := pool{ID: p.ID, Name: p.Name, Channel: p.Channel, OK: true}
		if p.PerUser {
			pl.Channel = "(per-user)"
			pools = append(pools, pl)
			continue
		}
		if _, err := s.links.link(p, p.Channel, nil); err != nil {
			pl.OK, pl.Error = false, err.Error()
			if p.AutoNode {
				s.start(nodeRequest{Pool: p, Channel: p.Channel})
			}
		}
		pools = append(pools, pl)
	}
	writeJSON(w, http.StatusOK, map[string]any{"pin": s.pin, "hostPort": s.hostPort(), "selfSigned": s.cfg.CertFile == "", "pools": pools})
}

type adminUser struct {
	ID        string `json:"id"`
	Login     string `json:"login"`
	Status    string `json:"status"`
	Plan      string `json:"plan,omitempty"`
	Expires   int64  `json:"expires"`
	Devices   int    `json:"devices"`
	CreatedAt int64  `json:"createdAt"`
	Banned    bool   `json:"banned,omitempty"`
	Note      string `json:"note,omitempty"`
}

func (s *Server) adminUsers(w http.ResponseWriter, _ *http.Request) {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	out := []adminUser{}
	for _, u := range s.st.sortedUsers() {
		out = append(out, adminUser{u.ID, u.Login, s.userStatus(u), u.PlanID, u.Expires, len(u.Devices), u.CreatedAt, u.Banned, u.Note})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) userOr404(w http.ResponseWriter, login string) *User {
	u := s.st.userByLogin(login)
	if u == nil {
		writeErr(w, http.StatusNotFound, "not_found", "Такого аккаунта нет.")
	}
	return u
}

// adminGrant extends an account by a plan's days, or by a number of days.
func (s *Server) adminGrant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PlanID string `json:"planId"`
		Days   int    `json:"days"`
	}
	if readJSON(r, &req) != nil || (req.PlanID == "" && req.Days < 1) || req.Days > 3650 {
		writeErr(w, http.StatusBadRequest, "bad_request", "Нужен planId или число дней (1..3650).")
		return
	}
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	u := s.userOr404(w, r.PathValue("login"))
	if u == nil {
		return
	}
	p := Plan{Days: req.Days}
	if req.PlanID != "" {
		var ok bool
		if p, ok = s.cfg.plan(req.PlanID); !ok {
			writeErr(w, http.StatusNotFound, "not_found", "Такого тарифа нет.")
			return
		}
	} else if cur, ok := s.cfg.plan(u.PlanID); ok {
		p.ID = cur.ID // days only: keeps the account's plan
	} else {
		p.ID = s.cfg.Plans[0].ID
	}
	s.extend(u, p)
	if s.save(w) {
		writeJSON(w, http.StatusOK, adminUser{u.ID, u.Login, s.userStatus(u), u.PlanID, u.Expires, len(u.Devices), u.CreatedAt, u.Banned, u.Note})
	}
}

func (s *Server) adminBan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Banned bool `json:"banned"`
	}
	if readJSON(r, &req) != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "Неверный запрос.")
		return
	}
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	u := s.userOr404(w, r.PathValue("login"))
	if u == nil {
		return
	}
	u.Banned = req.Banned
	if req.Banned {
		u.Devices = nil // signed out everywhere
	}
	if s.save(w) {
		writeJSON(w, http.StatusOK, map[string]any{"login": u.Login, "banned": u.Banned})
	}
}

// adminResetPassword gives the account a temporary password, for a person who
// lost both the password and the recovery key. Every device signs in again.
func (s *Server) adminResetPassword(w http.ResponseWriter, r *http.Request) {
	temp := randToken(12)
	hash, err := hashPassword(temp, s.cost())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "Не удалось.")
		return
	}
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	u := s.userOr404(w, r.PathValue("login"))
	if u == nil {
		return
	}
	u.PassHash, u.Devices = hash, nil
	s.lim.reset("login:" + u.Login)
	if s.save(w) {
		writeJSON(w, http.StatusOK, map[string]string{"login": u.Login, "password": temp})
	}
}

func (s *Server) adminDeleteUser(w http.ResponseWriter, r *http.Request) {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	u := s.userOr404(w, r.PathValue("login"))
	if u == nil {
		return
	}
	delete(s.st.state.Users, u.ID)
	for id, o := range s.st.state.Orders {
		if o.UserID == u.ID {
			delete(s.st.state.Orders, id)
		}
	}
	if s.save(w) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// ---- keys without an account ----

func (s *Server) adminAddKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string   `json:"name"`
		Days  int      `json:"days"`
		Pools []string `json:"pools"`
	}
	if readJSON(r, &req) != nil || req.Days < 0 || req.Days > 3650 {
		writeErr(w, http.StatusBadRequest, "bad_request", "Дней от 0 (бессрочно) до 3650.")
		return
	}
	known := map[string]bool{}
	for _, p := range s.cfg.Pools {
		known[p.ID] = true
	}
	for _, id := range req.Pools {
		if !known[id] {
			writeErr(w, http.StatusBadRequest, "bad_request", "Нет локации "+id+".")
			return
		}
	}
	token := randToken(32)
	k := &Key{ID: newID(), TokenHash: hashToken(token), Name: strings.TrimSpace(req.Name), Pools: req.Pools, CreatedAt: s.unix()}
	if req.Days > 0 {
		k.Expires = s.now().Add(time.Duration(req.Days) * 24 * time.Hour).Unix()
	}
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	s.st.state.Keys[k.ID] = k
	if !s.save(w) {
		delete(s.st.state.Keys, k.ID)
		return
	}
	inv := s.Invite(token, k.Name)
	writeJSON(w, http.StatusCreated, map[string]any{"id": k.ID, "token": token, "url": inv.URL, "invite": inv.Link(), "expires": k.Expires})
}

func (s *Server) adminKeys(w http.ResponseWriter, _ *http.Request) {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	keys := make([]*Key, 0, len(s.st.state.Keys))
	for _, k := range s.st.state.Keys {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].CreatedAt < keys[j].CreatedAt })
	type keyJSON struct {
		ID        string   `json:"id"`
		Name      string   `json:"name"`
		Expires   int64    `json:"expires"`
		Revoked   bool     `json:"revoked,omitempty"`
		Pools     []string `json:"pools,omitempty"`
		CreatedAt int64    `json:"createdAt"`
	}
	out := []keyJSON{}
	for _, k := range keys {
		out = append(out, keyJSON{k.ID, k.Name, k.Expires, k.Revoked, k.Pools, k.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) adminExtendKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Days int `json:"days"`
	}
	if readJSON(r, &req) != nil || req.Days < 1 || req.Days > 3650 {
		writeErr(w, http.StatusBadRequest, "bad_request", "Дней от 1 до 3650.")
		return
	}
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	k := s.st.state.Keys[r.PathValue("id")]
	if k == nil {
		writeErr(w, http.StatusNotFound, "not_found", "Такого ключа нет.")
		return
	}
	from := s.now()
	if cur := time.Unix(k.Expires, 0); k.Expires != 0 && cur.After(from) {
		from = cur
	}
	k.Expires, k.Revoked = from.Add(time.Duration(req.Days)*24*time.Hour).Unix(), false
	if s.save(w) {
		writeJSON(w, http.StatusOK, map[string]int64{"expires": k.Expires})
	}
}

func (s *Server) adminRevokeKey(w http.ResponseWriter, r *http.Request) {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	k := s.st.state.Keys[r.PathValue("id")]
	if k == nil {
		writeErr(w, http.StatusNotFound, "not_found", "Такого ключа нет.")
		return
	}
	k.Revoked = true
	if s.save(w) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// ---- orders ----

func (s *Server) adminOrders(w http.ResponseWriter, _ *http.Request) {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	type orderJSON struct {
		ID        string `json:"id"`
		Login     string `json:"login"`
		PlanID    string `json:"planId"`
		Status    string `json:"status"`
		CreatedAt int64  `json:"createdAt"`
		PaidAt    int64  `json:"paidAt,omitempty"`
	}
	out := []orderJSON{}
	for _, o := range s.st.state.Orders {
		login := "?"
		if u := s.st.state.Users[o.UserID]; u != nil {
			login = u.Login
		}
		out = append(out, orderJSON{o.ID, login, o.PlanID, o.Status, o.CreatedAt, o.PaidAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	writeJSON(w, http.StatusOK, out)
}

// adminPaid is what a payment bot, a cash desk or the owner calls once the money
// has come: the account gets the plan's days.
func (s *Server) adminPaid(w http.ResponseWriter, r *http.Request) {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	o := s.st.state.Orders[r.PathValue("id")]
	if o == nil {
		writeErr(w, http.StatusNotFound, "not_found", "Такого заказа нет.")
		return
	}
	if err := s.markPaid(o); err != nil {
		writeErr(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	if s.save(w) {
		writeJSON(w, http.StatusOK, map[string]any{"orderId": o.ID, "status": o.Status, "expires": s.st.state.Users[o.UserID].Expires})
	}
}
