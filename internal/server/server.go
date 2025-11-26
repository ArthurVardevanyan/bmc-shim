package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ArthurVardevanyan/bmc-shim/internal/backend"
)

type Config struct {
	Listen   string
	Username string
	Password string
	Systems  map[string]backend.Backend
}

type Server struct {
	cfg  Config
	http *http.Server
	mu   sync.RWMutex
	last map[string]bool
}

func New(cfg Config) *Server {
	mux := http.NewServeMux()
	if cfg.Systems == nil {
		cfg.Systems = map[string]backend.Backend{}
	}
	s := &Server{cfg: cfg, last: map[string]bool{}}
	s.http = &http.Server{
		Addr:         cfg.Listen,
		Handler:      s.authMiddleware(mux),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	mux.HandleFunc("/redfish/v1/", s.handleRoot)
	mux.HandleFunc("/redfish/v1/Systems", s.handleSystems)
	mux.HandleFunc("/redfish/v1/Systems/", s.handleSystem)

	return s
}

func (s *Server) Start() error {
	ids := make([]string, 0, len(s.cfg.Systems))
	for id := range s.cfg.Systems {
		ids = append(ids, id)
	}
	log.Printf("bmc-shim listening on %s (systems: %v)", s.cfg.Listen, ids)
	return s.http.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Username == "" && s.cfg.Password == "" {
			next.ServeHTTP(w, r)
			return
		}
		usr, pwd, ok := r.BasicAuth()
		if !ok || usr != s.cfg.Username || pwd != s.cfg.Password {
			w.Header().Set("WWW-Authenticate", "Basic realm=redfish")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"@odata.type": "#ServiceRoot.v1_0_0.ServiceRoot",
		"@odata.id":   "/redfish/v1/",
		"Name":        "BMC Shim ServiceRoot",
		"Systems": map[string]string{
			"@odata.id": "/redfish/v1/Systems",
		},
	})
}

func (s *Server) handleSystems(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	members := make([]map[string]string, 0, len(s.cfg.Systems))
	for id := range s.cfg.Systems {
		members = append(members, map[string]string{"@odata.id": "/redfish/v1/Systems/" + id})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"@odata.id":           "/redfish/v1/Systems",
		"Members":             members,
		"Members@odata.count": len(members),
		"Name":                "Systems Collection",
	})
}

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	// Expect paths like /redfish/v1/Systems/<id>[/Actions/ComputerSystem.Reset]
	path := strings.TrimPrefix(r.URL.Path, "/redfish/v1/Systems/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	if strings.HasSuffix(path, "/Actions/ComputerSystem.Reset") {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimSuffix(path, "/Actions/ComputerSystem.Reset")
		id = strings.TrimSuffix(id, "/")
		be, ok := s.cfg.Systems[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		var body struct{ ResetType string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := s.applyReset(r.Context(), id, be, body.ResetType); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimSuffix(path, "/")
	be, ok := s.cfg.Systems[id]
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Prefer backend-reported state when available
	on := false
	if ps, ok := be.(backend.PowerStateProvider); ok {
		if v, err := ps.CurrentState(r.Context()); err == nil {
			on = v
		} else {
			s.mu.RLock()
			on = s.last[id]
			s.mu.RUnlock()
		}
	} else {
		s.mu.RLock()
		on = s.last[id]
		s.mu.RUnlock()
	}
	powerState := "Off"
	if on {
		powerState = "On"
	}

	// Determine friendly name
	name := "System " + id
	if np, ok := be.(backend.NameProvider); ok {
		if n, err := np.DisplayName(r.Context()); err == nil && n != "" {
			name = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"@odata.id":  "/redfish/v1/Systems/" + id,
		"Id":         id,
		"Name":       name,
		"PowerState": powerState,
		"Actions": map[string]any{
			"#ComputerSystem.Reset": map[string]any{
				"target":                            "/redfish/v1/Systems/" + id + "/Actions/ComputerSystem.Reset",
				"ResetType@Redfish.AllowableValues": []string{"On", "ForceOff", "GracefulShutdown", "ForceRestart"},
			},
		},
	})
}

func (s *Server) applyReset(ctx context.Context, id string, be backend.Backend, resetType string) error {
	switch resetType {
	case "On":
		if err := be.PowerOn(ctx); err != nil {
			return err
		}
		s.mu.Lock()
		s.last[id] = true
		s.mu.Unlock()
		return nil
	case "ForceOff", "GracefulShutdown", "Off":
		if err := be.PowerOff(ctx); err != nil {
			return err
		}
		s.mu.Lock()
		s.last[id] = false
		s.mu.Unlock()
		return nil
	case "ForceRestart", "GracefulRestart":
		// simple restart: off then on
		if err := be.PowerOff(ctx); err != nil {
			return err
		}
		time.Sleep(2 * time.Second)
		if err := be.PowerOn(ctx); err != nil {
			return err
		}
		s.mu.Lock()
		s.last[id] = true
		s.mu.Unlock()
		return nil
	default:
		return errors.New("unsupported ResetType")
	}
}
