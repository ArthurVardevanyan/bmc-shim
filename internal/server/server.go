package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

type Boot struct {
	BootSourceOverrideTarget  string `json:"BootSourceOverrideTarget"`
	BootSourceOverrideEnabled string `json:"BootSourceOverrideEnabled"`
	BootSourceOverrideMode    string `json:"BootSourceOverrideMode,omitempty"`
}

type Server struct {
	cfg  Config
	http *http.Server
	mu   sync.RWMutex
	last map[string]bool
	boot map[string]Boot
}

func New(cfg Config) *Server {
	mux := http.NewServeMux()
	if cfg.Systems == nil {
		cfg.Systems = map[string]backend.Backend{}
	}
	s := &Server{
		cfg:  cfg,
		last: map[string]bool{},
		boot: map[string]Boot{},
	}
	s.http = &http.Server{
		Addr:         cfg.Listen,
		Handler:      s.loggingMiddleware(s.authMiddleware(mux)),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	mux.HandleFunc("/redfish/v1/", s.handleRoot)
	mux.HandleFunc("/redfish/v1/Systems", s.handleSystems)
	mux.HandleFunc("/redfish/v1/Systems/", s.handleSystem)
	mux.HandleFunc("/livez", s.handleLivez)
	mux.HandleFunc("/readyz", s.handleReadyz)

	return s
}

func (s *Server) Start() error {
	ids := make([]string, 0, len(s.cfg.Systems))
	for id := range s.cfg.Systems {
		ids = append(ids, id)
	}
	log.Printf("bmc-shim listening on %s (HTTP) (systems: %v)", s.cfg.Listen, ids)
	return s.http.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		bodyBytes, _ := io.ReadAll(r.Body)
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

		forwarded := r.Header.Get("X-Forwarded-For")
		log.Printf("REQ: %s %s RemoteAddr: %s X-Forwarded-For: %s Body: %s", r.Method, r.URL.RequestURI(), r.RemoteAddr, forwarded, string(bodyBytes))
		next.ServeHTTP(w, r)
		log.Printf("RES: %s %s RemoteAddr: %s X-Forwarded-For: %s (%v)", r.Method, r.URL.RequestURI(), r.RemoteAddr, forwarded, time.Since(start))
	})
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Allow unauthenticated access to the root service to support discovery
		// Also allow health checks
		if r.URL.Path == "/redfish/v1/" || r.URL.Path == "/redfish/v1" ||
			r.URL.Path == "/livez" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}

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
		"Id":          "RootService",
		"Name":        "BMC Shim ServiceRoot",
		"Systems": map[string]string{
			"@odata.id": "/redfish/v1/Systems",
		},
	})
}

func (s *Server) handleLivez(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// Check if we can reach at least one backend
	if len(s.cfg.Systems) == 0 {
		// No systems configured, technically ready but useless?
		// Let's say ok.
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
		return
	}

	// Try to ping backends. If at least one succeeds, we are ready.
	// We don't want to fail if one of many is down, as long as the service is functional.
	// But if ALL are down, we are probably not ready.
	success := false
	for _, be := range s.cfg.Systems {
		if hc, ok := be.(backend.HealthChecker); ok {
			if err := hc.Ping(r.Context()); err == nil {
				success = true
				break
			}
		} else {
			// Backend doesn't support health check, assume it's fine
			success = true
			break
		}
	}

	if success {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	} else {
		http.Error(w, "all backends failed", http.StatusServiceUnavailable)
	}
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

	// Get or initialize Boot info for this system
	s.mu.RLock()
	boot := s.boot[id]
	s.mu.RUnlock()
	if boot.BootSourceOverrideTarget == "" {
		boot = Boot{
			BootSourceOverrideTarget:  "None",
			BootSourceOverrideEnabled: "Disabled",
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"@odata.id":  "/redfish/v1/Systems/" + id,
		"Id":         id,
		"Name":       name,
		"PowerState": powerState,
		"Boot": map[string]any{
			"BootSourceOverrideTarget":                         boot.BootSourceOverrideTarget,
			"BootSourceOverrideEnabled":                        boot.BootSourceOverrideEnabled,
			"BootSourceOverrideTarget@Redfish.AllowableValues": []string{"None", "Pxe", "Hdd"},
		},
		"Links": map[string]any{
			"ManagedBy": []map[string]string{
				{"@odata.id": "/redfish/v1/Managers/1"},
			},
		},
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
