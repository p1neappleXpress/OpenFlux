package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/p1neappleXpress/OpenFlux/subscription"
	"github.com/p1neappleXpress/OpenFlux/transport/avito"
	"github.com/p1neappleXpress/OpenFlux/transport/cupsonline"
)

// Server is the subscription service.
type Server struct {
	cfg   *Config
	st    *store
	links *linkCache
	prov  nodeProvisioner
	w     *nodeWatch
	lim   *limiter
	now   func() time.Time
	logf  func(format string, args ...any)
	pin   string
	cert  tls.Certificate
	// dummyHash is compared against when a login does not exist, so the answer
	// takes as long as for a wrong password.
	dummyHash string
}

// Options are for tests.
type Options struct {
	Now  func() time.Time
	Logf func(format string, args ...any)
	// Provisioner makes a pool's channel; scriptProvisioner by default.
	Provisioner nodeProvisioner
}

// New opens the state and loads (or makes) the certificate.
func New(cfg *Config, opt Options) (*Server, error) {
	if err := cfg.Check(); err != nil {
		return nil, err
	}
	st, err := openStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, st: st, now: time.Now, logf: log.Printf}
	if opt.Now != nil {
		s.now = opt.Now
	}
	if opt.Logf != nil {
		s.logf = opt.Logf
	}
	s.links = newLinkCache(cfg.NodeConfDir)
	s.w = newNodeWatch()
	if opt.Provisioner != nil {
		s.prov = opt.Provisioner
	} else {
		s.prov = &scriptProvisioner{cfg: cfg, logf: s.logf, exec: runCmd, rooms: cupsonline.CreateRoomList, avitoRooms: func(ctx context.Context) (string, error) { return avito.CreateRoomList(ctx, "") }, docs: accountDocFactory{}}
	}
	s.lim = newLimiter(func() time.Time { return s.now() })
	if s.dummyHash, err = hashPassword("dummy-password-for-timing", s.cost()); err != nil {
		return nil, err
	}
	if s.cert, s.pin, err = loadCertificate(cfg); err != nil {
		return nil, err
	}
	return s, nil
}

// Pin is the hex SHA-256 of the service's certificate.
func (s *Server) Pin() string { return s.pin }

// Invite is the key people use to reach a subscription of this service (without
// an account): the document address of token plus the certificate pin.
func (s *Server) Invite(token, name string) subscription.Invite {
	inv := subscription.Invite{URL: "https://" + s.hostPort() + "/sub/" + token, Name: name}
	if s.cfg.CertFile == "" {
		inv.Pin = s.pin
	}
	return inv
}

func (s *Server) hostPort() string {
	port := "443"
	if _, p, err := net.SplitHostPort(s.cfg.Listen); err == nil && p != "" {
		port = p
	}
	h := s.cfg.PublicHost
	if strings.Contains(h, ":") && !strings.HasPrefix(h, "[") {
		h = "[" + h + "]"
	}
	return h + ":" + port
}

// Handler is the whole HTTP API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /sub/{token}", s.handleKey)
	mux.HandleFunc("GET /api/v1/plans", s.handlePlans)
	mux.HandleFunc("POST /api/v1/register", s.handleRegister)
	mux.HandleFunc("POST /api/v1/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/recover", s.handleRecover)
	mux.HandleFunc("GET /api/v1/account", s.authed(s.handleAccount))
	mux.HandleFunc("GET /api/v1/subscription", s.authed(s.handleSubscription))
	mux.HandleFunc("POST /api/v1/trial", s.authed(s.handleTrial))
	mux.HandleFunc("POST /api/v1/checkout", s.authed(s.handleCheckout))
	mux.HandleFunc("GET /api/v1/orders/{id}", s.authed(s.handleOrder))
	mux.HandleFunc("POST /api/v1/password", s.authed(s.handlePassword))
	mux.HandleFunc("POST /api/v1/logout", s.authed(s.handleLogout))
	mux.HandleFunc("DELETE /api/v1/devices/{id}", s.authed(s.handleRemoveDevice))
	mux.HandleFunc("DELETE /api/v1/account", s.authed(s.handleDeleteAccount))
	// The same as DELETE with a body, for HTTP clients that cannot send one.
	mux.HandleFunc("POST /api/v1/account/delete", s.authed(s.handleDeleteAccount))
	s.adminRoutes(mux)
	return http.MaxBytesHandler(noStore(mux), 32<<10)
}

// Serve listens with TLS until ctx ends.
func (s *Server) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	s.startNodeWatch(ctx)
	srv := &http.Server{
		Handler:           s.Handler(),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{s.cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	s.logf("opensub: listening on %s, certificate pin sha256:%s", ln.Addr(), s.pin)
	if err := srv.ServeTLS(ln, "", ""); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func noStore(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		h.ServeHTTP(w, r)
	})
}

// ---- helpers ----

type apiError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	// Devices come with device_limit: the account's devices, to choose one to replace.
	Devices []deviceJSON `json:"devices,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiError{Error: code, Message: msg})
}

func readJSON(r *http.Request, into any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, into)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) unix() int64 { return s.now().Unix() }

// cost is the bcrypt cost.
func (s *Server) cost() int {
	if s.cfg.BcryptCost > 0 {
		return s.cfg.BcryptCost
	}
	return 12
}

// document is what the apps get: the user's or key's status and locations,
// with the transports the app runs.
func (s *Server) document(status string, expires int64, pools []string, owner string, set transportSet) subscription.Document {
	d := subscription.Document{
		Version:     1,
		Title:       s.cfg.Title,
		Status:      status,
		Expires:     expires,
		UpdateHours: s.cfg.UpdateHours,
		Support:     s.cfg.Support,
		Announce:    s.cfg.Announce,
		Profiles:    []subscription.Profile{},
	}
	if status == subscription.StatusActive {
		d.Profiles = s.profiles(pools, owner, set)
	}
	return d
}

func (s *Server) userPools(u *User) []string {
	if p, ok := s.cfg.plan(u.PlanID); ok {
		return p.Pools
	}
	return nil
}

func (s *Server) userStatus(u *User) string {
	switch {
	case u.Banned:
		return subscription.StatusRevoked
	case u.Expires == 0 || s.unix() > u.Expires:
		return subscription.StatusExpired
	}
	return subscription.StatusActive
}

func (s *Server) handleKey(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.lim.count("keyfail:"+ip, 15*time.Minute) >= 30 {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "Слишком много запросов, подождите.")
		return
	}
	s.st.mu.Lock()
	k := s.st.keyByToken(r.PathValue("token"))
	var status string
	var pools []string
	var expires int64
	if k != nil {
		status, pools, expires = subscription.StatusActive, k.Pools, k.Expires
		switch {
		case k.Revoked:
			status = subscription.StatusRevoked
		case k.Expires != 0 && s.unix() > k.Expires:
			status = subscription.StatusExpired
		}
	}
	s.st.mu.Unlock()
	if k == nil {
		s.lim.add("keyfail:"+ip, 15*time.Minute)
		writeErr(w, http.StatusNotFound, "not_found", "Такого ключа нет.")
		return
	}
	writeJSON(w, http.StatusOK, s.document(status, expires, pools, k.ID, clientTransports(r)))
}

func (s *Server) handlePlans(w http.ResponseWriter, _ *http.Request) {
	type planJSON struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Days    int    `json:"days"`
		Devices int    `json:"devices"`
		Price   string `json:"price,omitempty"`
	}
	out := []planJSON{}
	for _, p := range s.cfg.Plans {
		if !p.Trial {
			out = append(out, planJSON{p.ID, p.Name, p.Days, p.Devices, p.Price})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"plans": out, "payment": s.cfg.Payment})
}

var errUnauthorized = errors.New("unauthorized")

type authedHandler func(w http.ResponseWriter, r *http.Request, u *User, d *Device)

// authed checks the device token and hands the handler the account and device,
// under the state lock (the handler must not take it again).
func (s *Server) authed(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" || token == r.Header.Get("Authorization") || len(token) > 200 {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "Войдите в аккаунт.")
			return
		}
		if s.lim.count("tokfail:"+ip, 15*time.Minute) >= 60 {
			writeErr(w, http.StatusTooManyRequests, "rate_limited", "Слишком много запросов, подождите.")
			return
		}
		s.st.mu.Lock()
		defer s.st.mu.Unlock()
		u, d := s.st.userByToken(token)
		if u == nil {
			s.lim.add("tokfail:"+ip, 15*time.Minute)
			writeErr(w, http.StatusUnauthorized, "unauthorized", "Войдите в аккаунт.")
			return
		}
		if u.Banned {
			writeErr(w, http.StatusForbidden, "banned", "Доступ отключён. Обратитесь в поддержку.")
			return
		}
		// LastSeen is kept to the minute-ish: no write for every poll.
		if s.unix()-d.LastSeen > 600 {
			d.LastSeen = s.unix()
			s.st.save()
		}
		h(w, r, u, d)
	}
}

func (s *Server) save(w http.ResponseWriter) bool {
	if err := s.st.save(); err != nil {
		s.logf("save state: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "Не удалось сохранить. Попробуйте ещё раз.")
		return false
	}
	return true
}
