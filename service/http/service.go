package http

import (
	"context"
	"fmt"
	"github.com/spiral/roadrunner"
	"github.com/spiral/roadrunner/service/env"
	"github.com/spiral/roadrunner/service/http/attributes"
	"github.com/spiral/roadrunner/service/rpc"
	"golang.org/x/net/http2"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

const (
	// ID contains default svc name.
	ID = "http"

	// EventInitSSL thrown at moment of https initialization. SSL server passed as context.
	EventInitSSL = 750
)

// http middleware type.
type middleware func(f http.HandlerFunc) http.HandlerFunc

type ServiceStats struct {
	Accepted uint64
	Success  uint64
	Error    uint64
}

// Service manages rr, http servers.
type Service struct {
	cfg        *Config
	env        env.Environment
	stats 	   *ServiceStats
	lsns       []func(event int, ctx interface{})
	mdwr       []middleware
	mu         sync.Mutex
	rr         *roadrunner.Server
	controller roadrunner.Controller
	handler    *Handler
	http       *http.Server
	https      *http.Server
}

// Attach attaches controller. Currently only one controller is supported.
func (s *Service) Attach(w roadrunner.Controller) {
	s.controller = w
}

// AddMiddleware adds new net/http mdwr.
func (s *Service) AddMiddleware(m middleware) {
	s.mdwr = append(s.mdwr, m)
}

// AddListener attaches server event controller.
func (s *Service) AddListener(l func(event int, ctx interface{})) {
	s.lsns = append(s.lsns, l)
}

// Init must return configure svc and return true if svc hasStatus enabled. Must return error in case of
// misconfiguration. Services must not be used without proper configuration pushed first.
func (s *Service) Init(cfg *Config, r *rpc.Service, e env.Environment) (bool, error) {
	s.cfg = cfg
	s.env = e

	if r != nil {
		if err := r.Register(ID, &rpcServer{s}); err != nil {
			return false, err
		}
	}

	return true, nil
}

// Serve serves the svc.
func (s *Service) Serve() error {
	s.mu.Lock()

	if s.env != nil {
		if err := s.env.Copy(s.cfg.Workers); err != nil {
			return nil
		}
	}

	s.stats = &ServiceStats{};
	s.AddListener((&statsListener{stats: s.stats}).listener);

	s.cfg.Workers.SetEnv("RR_HTTP", "true")

	s.rr = roadrunner.NewServer(s.cfg.Workers)
	s.rr.Listen(s.throw)

	if s.controller != nil {
		s.rr.Attach(s.controller)
	}

	s.handler = &Handler{cfg: s.cfg, rr: s.rr}
	s.handler.Listen(s.throw)

	s.http = &http.Server{Addr: s.cfg.Address, Handler: s}

	if s.cfg.EnableTLS() {
		s.https = s.initSSL()
	}

	s.mu.Unlock()

	if err := s.rr.Start(); err != nil {
		return err
	}
	defer s.rr.Stop()

	err := make(chan error, 2)
	go func() { err <- s.http.ListenAndServe() }()
	if s.https != nil {
		go func() { err <- s.https.ListenAndServeTLS(s.cfg.SSL.Cert, s.cfg.SSL.Key) }()
	}

	return <-err
}

// Stop stops the http.
func (s *Service) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.http == nil {
		return
	}

	if s.https != nil {
		go s.https.Shutdown(context.Background())
	}

	go s.http.Shutdown(context.Background())
}

// Server returns associated rr server (if any).
func (s *Service) Server() *roadrunner.Server {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.rr
}

// ServeHTTP handles connection using set of middleware and rr PSR-7 server.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.https != nil && r.TLS == nil && s.cfg.SSL.Redirect {
		target := &url.URL{
			Scheme:   "https",
			Host:     s.tlsAddr(r.Host, false),
			Path:     r.URL.Path,
			RawQuery: r.URL.RawQuery,
		}

		http.Redirect(w, r, target.String(), http.StatusTemporaryRedirect)
		return
	}

	r = attributes.Init(r)

	// chaining middleware
	f := s.handler.ServeHTTP
	for _, m := range s.mdwr {
		f = m(f)
	}
	f(w, r)
}

// Init https server.
func (s *Service) initSSL() *http.Server {
	server := &http.Server{Addr: s.tlsAddr(s.cfg.Address, true), Handler: s}
	s.throw(EventInitSSL, server)

	// Enable HTTP/2 support by default
	http2.ConfigureServer(server, &http2.Server{})

	return server
}

// throw handles service, server and pool events.
func (s *Service) throw(event int, ctx interface{}) {
	for _, l := range s.lsns {
		l(event, ctx)
	}

	if event == roadrunner.EventServerFailure {
		// underlying rr server is dead
		s.Stop()
	}
}

// tlsAddr replaces listen or host port with port configured by SSL config.
func (s *Service) tlsAddr(host string, forcePort bool) string {
	// remove current forcePort first
	host = strings.Split(host, ":")[0]

	if forcePort || s.cfg.SSL.Port != 443 {
		host = fmt.Sprintf("%s:%v", host, s.cfg.SSL.Port)
	}

	return host
}
