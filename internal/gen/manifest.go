package gen

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Manifest describes a generated data set and its planted scenarios.
type Manifest struct {
	GeneratedAt time.Time `json:"generatedAt"`
	Seed        uint64    `json:"seed"`
	Players     int       `json:"players"`
	Events      int       `json:"events"`
	Days        int       `json:"days"`
	// Devices is the size of the device index space; see model.
	Devices       int `json:"devices"`
	SharedDevices int `json:"sharedDevices"`
	SharedPlayers int `json:"sharedPlayers"`
	// IDFormats documents the background ID spaces for the load test.
	IDFormats map[string]string `json:"idFormats"`
	Cases     []Case            `json:"cases"`
}

// Case is one planted scenario: a decision request and its expected outcome.
type Case struct {
	Name     string          `json:"name"`
	Rule     string          `json:"rule"` // rule under test, or "MULTI"
	Kind     string          `json:"kind"` // fires | exact-threshold | one-below | edge
	About    string          `json:"about"`
	PlayerID string          `json:"playerId"`
	Request  DecisionRequest `json:"request"`
	Expected Expected        `json:"expected"`
}

// DecisionRequest is the body of POST /decisions.
type DecisionRequest struct {
	Action          string `json:"action"`
	PlayerID        string `json:"playerId"`
	Amount          string `json:"amount,omitempty"`
	Currency        string `json:"currency,omitempty"`
	DeviceID        string `json:"deviceId,omitempty"`
	PaymentMethodID string `json:"paymentMethodId,omitempty"`
}

// Expected is the decision and the fired rules, in rule order.
type Expected struct {
	Decision string   `json:"decision"`
	Rules    []string `json:"rules"`
}

// Write saves the manifest as indented JSON.
func (m *Manifest) Write(path string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// ReadManifest loads a manifest written by Write.
func ReadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("manifest %s: %w", path, err)
	}
	return &m, nil
}
