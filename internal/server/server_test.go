package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ArthurVardevanyan/bmc-shim/internal/backend"
)

// fakeBackend is a power backend that reports a configurable state.
type fakeBackend struct{ on bool }

func (f *fakeBackend) PowerOn(context.Context) error  { f.on = true; return nil }
func (f *fakeBackend) PowerOff(context.Context) error { f.on = false; return nil }
func (f *fakeBackend) CurrentState(context.Context) (bool, error) {
	return f.on, nil
}

func newTestServer(user, pass string) (*Server, *fakeBackend) {
	be := &fakeBackend{}
	return New(Config{
		Username: user,
		Password: pass,
		Systems:  map[string]backend.Backend{"6": be},
	}), be
}

func do(t *testing.T, s *Server, method, path, body, user, pass string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("invalid JSON %q: %v", rec.Body.String(), err)
	}
	return m
}

func boot(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	b, ok := m["Boot"].(map[string]any)
	if !ok {
		t.Fatalf("no Boot object in %v", m)
	}
	return b
}

func TestPatchNestedBootRoundTrip(t *testing.T) {
	s, _ := newTestServer("", "")

	// Ironic sends the override nested under "Boot".
	rec := do(t, s, http.MethodPatch, "/redfish/v1/Systems/6",
		`{"Boot": {"BootSourceOverrideTarget": "Hdd", "BootSourceOverrideEnabled": "Continuous"}}`, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200", rec.Code)
	}

	got := boot(t, decode(t, do(t, s, http.MethodGet, "/redfish/v1/Systems/6", "", "", "")))
	if got["BootSourceOverrideTarget"] != "Hdd" || got["BootSourceOverrideEnabled"] != "Continuous" {
		t.Fatalf("GET after PATCH = %v, want Hdd/Continuous", got)
	}
}

func TestPatchKeepsFieldsThatAreNotSent(t *testing.T) {
	s, _ := newTestServer("", "")

	// Ironic sets the mode and the target in separate calls.
	do(t, s, http.MethodPatch, "/redfish/v1/Systems/6",
		`{"Boot": {"BootSourceOverrideTarget": "Pxe", "BootSourceOverrideEnabled": "Once"}}`, "", "")
	do(t, s, http.MethodPatch, "/redfish/v1/Systems/6",
		`{"Boot": {"BootSourceOverrideMode": "UEFI"}}`, "", "")

	got := boot(t, decode(t, do(t, s, http.MethodGet, "/redfish/v1/Systems/6", "", "", "")))
	if got["BootSourceOverrideTarget"] != "Pxe" || got["BootSourceOverrideEnabled"] != "Once" ||
		got["BootSourceOverrideMode"] != "UEFI" {
		t.Fatalf("boot = %v, want Pxe/Once/UEFI", got)
	}
}

func TestPatchRespondsWithRealPowerState(t *testing.T) {
	s, be := newTestServer("", "")
	be.on = false

	rec := do(t, s, http.MethodPatch, "/redfish/v1/Systems/6", `{"Boot": {"BootSourceOverrideMode": "UEFI"}}`, "", "")
	if got := decode(t, rec)["PowerState"]; got != "Off" {
		t.Fatalf("PATCH PowerState = %v, want Off (must come from the backend)", got)
	}
}

func TestPatchErrors(t *testing.T) {
	s, _ := newTestServer("", "")

	if rec := do(t, s, http.MethodPatch, "/redfish/v1/Systems/6", `not json`, "", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("bad JSON status = %d, want 400", rec.Code)
	}
	if rec := do(t, s, http.MethodPatch, "/redfish/v1/Systems/99", `{}`, "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown system status = %d, want 404", rec.Code)
	}
	if rec := do(t, s, http.MethodDelete, "/redfish/v1/Systems/6", ``, "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE status = %d, want 405", rec.Code)
	}
}

func TestUnpatchedSystemDefaultsToOneTimePXE(t *testing.T) {
	s, _ := newTestServer("", "")
	got := boot(t, decode(t, do(t, s, http.MethodGet, "/redfish/v1/Systems/6", "", "", "")))
	if got["BootSourceOverrideTarget"] != "Pxe" || got["BootSourceOverrideEnabled"] != "Once" {
		t.Fatalf("default boot = %v, want Pxe/Once", got)
	}
}

func TestManagersRequireAuth(t *testing.T) {
	s, _ := newTestServer("admin", "secret")

	for _, path := range []string{"/redfish/v1/Managers", "/redfish/v1/Managers/1", "/redfish/v1/Managers/1/VirtualMedia"} {
		if rec := do(t, s, http.MethodGet, path, "", "", ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without credentials = %d, want 401", path, rec.Code)
		}
		if rec := do(t, s, http.MethodGet, path, "", "admin", "secret"); rec.Code != http.StatusOK {
			t.Errorf("%s with credentials = %d, want 200", path, rec.Code)
		}
	}
}

func TestVirtualMediaIsAnEmptyCollection(t *testing.T) {
	s, _ := newTestServer("", "")
	m := decode(t, do(t, s, http.MethodGet, "/redfish/v1/Managers/1/VirtualMedia", "", "", ""))
	if m["Members@odata.count"] != float64(0) {
		t.Fatalf("VirtualMedia = %v, want an empty collection", m)
	}
	if rec := do(t, s, http.MethodGet, "/redfish/v1/Managers/2", "", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown manager status = %d, want 404", rec.Code)
	}
}

func TestResetStillWorks(t *testing.T) {
	s, be := newTestServer("", "")
	if rec := do(t, s, http.MethodPost, "/redfish/v1/Systems/6/Actions/ComputerSystem.Reset", `{"ResetType":"On"}`, "", ""); rec.Code != http.StatusOK || !be.on {
		t.Fatalf("On: status=%d on=%v", rec.Code, be.on)
	}
	if rec := do(t, s, http.MethodPost, "/redfish/v1/Systems/6/Actions/ComputerSystem.Reset", `{"ResetType":"ForceOff"}`, "", ""); rec.Code != http.StatusOK || be.on {
		t.Fatalf("ForceOff: status=%d on=%v", rec.Code, be.on)
	}
}
