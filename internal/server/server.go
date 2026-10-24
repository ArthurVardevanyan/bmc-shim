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
	mux.HandleFunc("/redfish/v1/Managers", s.handleManagers)
	mux.HandleFunc("/redfish/v1/Managers/", s.handleManager)
	mux.HandleFunc("/livez", s.handleLivez)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/startupz", s.handleLivez)

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
		if err := r.Body.Close(); err != nil {
			log.Printf("error closing request body: %v", err)
		}
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
			r.URL.Path == "/livez" || r.URL.Path == "/readyz" || r.URL.Path == "/startupz" {
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
		"Managers": map[string]string{
			"@odata.id": "/redfish/v1/Managers",
		},
	})
}

func (s *Server) handleLivez(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("ok")); err != nil {
		log.Printf("error writing response: %v", err)
	}
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// Check if we can reach at least one backend
	if len(s.cfg.Systems) == 0 {
		// No systems configured, technically ready but useless?
		// Let's say ok.
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("ok")); err != nil {
			log.Printf("error writing response: %v", err)
		}
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
		if _, err := w.Write([]byte("ok")); err != nil {
			log.Printf("error writing response: %v", err)
		}
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

	switch r.Method {
	case http.MethodGet:
		id := strings.TrimSuffix(path, "/")
		if _, ok := s.cfg.Systems[id]; !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, s.systemJSON(r.Context(), id))
	case http.MethodPatch:
		s.handleSystemPatch(w, r, strings.TrimSuffix(path, "/"))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// systemJSON renders a ComputerSystem for the (already validated) system id.
func (s *Server) systemJSON(ctx context.Context, id string) map[string]any {
	be := s.cfg.Systems[id]

	// Prefer backend-reported state when available
	on := false
	if ps, ok := be.(backend.PowerStateProvider); ok {
		if v, err := ps.CurrentState(ctx); err == nil {
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
		if n, err := np.DisplayName(ctx); err == nil && n != "" {
			name = n
		}
	}

	boot := s.bootState(id)

	return map[string]any{
		"@odata.id":  "/redfish/v1/Systems/" + id,
		"Id":         id,
		"Name":       name,
		"PowerState": powerState,
		"Boot": map[string]any{
			"BootSourceOverrideTarget":                         boot.BootSourceOverrideTarget,
			"BootSourceOverrideEnabled":                        boot.BootSourceOverrideEnabled,
			"BootSourceOverrideMode":                           boot.BootSourceOverrideMode,
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
	}
}

// bootState returns the stored boot override for a system.
//
// The state is held in memory per replica. A PATCH and the GET that Ironic uses
// to verify it can land on different replicas, so a system that has never been
// patched on this replica reports a one-time PXE boot (what metal3 asks for
// when it needs a network boot) rather than "None", which would make the
// verification fail.
func (s *Server) bootState(id string) Boot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if b, ok := s.boot[id]; ok {
		return b
	}
	return Boot{
		BootSourceOverrideTarget:  "Pxe",
		BootSourceOverrideEnabled: "Once",
		BootSourceOverrideMode:    "UEFI",
	}
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

func (s *Server) handleManagers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"@odata.id":           "/redfish/v1/Managers",
		"Members":             []map[string]string{{"@odata.id": "/redfish/v1/Managers/1"}},
		"Members@odata.count": 1,
		"Name":                "Managers Collection",
	})
}

// handleManager serves a minimal Manager (and an empty VirtualMedia
// collection) so clients such as metal3/sushy can finish their discovery.
func (s *Server) handleManager(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/redfish/v1/Managers/"), "/")
	if path == "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	switch path {
	case "1":
		writeJSON(w, http.StatusOK, map[string]any{
			"@odata.id":       "/redfish/v1/Managers/1",
			"Id":              "1",
			"Name":            "Manager",
			"ManagerType":     "BMC",
			"Description":     "Redfish BMC Shim Manager",
			"FirmwareVersion": "bmc-shim",
			"Status":          map[string]string{"State": "Enabled", "Health": "OK"},
			"VirtualMedia":    map[string]string{"@odata.id": "/redfish/v1/Managers/1/VirtualMedia"},
		})
	case "1/VirtualMedia":
		// Physical media only: advertise an empty collection.
		writeJSON(w, http.StatusOK, map[string]any{
			"@odata.id":           "/redfish/v1/Managers/1/VirtualMedia",
			"Name":                "Virtual Media Services",
			"Members":             []map[string]string{},
			"Members@odata.count": 0,
		})
	default:
		http.NotFound(w, r)
	}
}

// handleSystemPatch implements the part of PATCH /Systems/<id> that metal3
// uses: the nested "Boot" object (BootSourceOverrideTarget/Enabled/Mode).
// Fields that are not present keep their previous value, as Redfish PATCH
// semantics require (Ironic sends Mode and Target/Enabled in separate calls).
func (s *Server) handleSystemPatch(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := s.cfg.Systems[id]; !ok {
		http.NotFound(w, r)
		return
	}

	var body struct {
		Boot *struct {
			Target  *string `json:"BootSourceOverrideTarget"`
			Enabled *string `json:"BootSourceOverrideEnabled"`
			Mode    *string `json:"BootSourceOverrideMode"`
		} `json:"Boot"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if body.Boot != nil {
		cur := s.bootState(id)
		if body.Boot.Target != nil {
			cur.BootSourceOverrideTarget = *body.Boot.Target
		}
		if body.Boot.Enabled != nil {
			cur.BootSourceOverrideEnabled = *body.Boot.Enabled
		}
		if body.Boot.Mode != nil {
			cur.BootSourceOverrideMode = *body.Boot.Mode
		}
		s.mu.Lock()
		s.boot[id] = cur
		s.mu.Unlock()
	}

	writeJSON(w, http.StatusOK, s.systemJSON(r.Context(), id))
}
