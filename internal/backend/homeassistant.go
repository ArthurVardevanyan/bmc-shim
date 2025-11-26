package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type HomeAssistant struct {
	baseURL  string
	token    string
	entityID string
	client   *http.Client
}

func NewHomeAssistant(baseURL, token, entityID string) (*HomeAssistant, error) {
	if baseURL == "" || token == "" || entityID == "" {
		return nil, fmt.Errorf("homeassistant backend requires baseURL, token, and entityID")
	}
	// Ensure no trailing slash on URL
	baseURL = strings.TrimRight(baseURL, "/")
	return &HomeAssistant{
		baseURL:  baseURL,
		token:    token,
		entityID: entityID,
		client:   &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func (h *HomeAssistant) PowerOn(ctx context.Context) error {
	return h.callService(ctx, "switch", "turn_on")
}

func (h *HomeAssistant) PowerOff(ctx context.Context) error {
	return h.callService(ctx, "switch", "turn_off")
}

func (h *HomeAssistant) CurrentState(ctx context.Context) (bool, error) {
	state, _, err := h.fetchState(ctx)
	if err != nil {
		return false, err
	}
	return strings.ToLower(state) == "on", nil
}

func (h *HomeAssistant) DisplayName(ctx context.Context) (string, error) {
	_, name, err := h.fetchState(ctx)
	return name, err
}

func (h *HomeAssistant) callService(ctx context.Context, domain, service string) error {
	payload := map[string]any{"entity_id": h.entityID}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL+"/api/services/"+domain+"/"+service, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("homeassistant service %s.%s: http %d", domain, service, resp.StatusCode)
	}
	return nil
}

// fetchState returns (state, friendlyName, error)
func (h *HomeAssistant) fetchState(ctx context.Context) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.baseURL+"/api/states/"+h.entityID, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Accept", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("homeassistant state: http %d", resp.StatusCode)
	}
	var body struct {
		State      string                 `json:"state"`
		Attributes map[string]interface{} `json:"attributes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", "", err
	}
	name := ""
	if v, ok := body.Attributes["friendly_name"]; ok {
		if s, ok := v.(string); ok {
			name = s
		}
	}
	return body.State, name, nil
}
