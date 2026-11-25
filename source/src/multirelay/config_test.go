package multirelay

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) Config {
	t.Helper()
	file, err := os.Open("../../examples/multirelay/relays.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	config, err := Parse(file)
	if err != nil {
		t.Fatal(err)
	}
	return config
}
func TestConfigFingerprint(t *testing.T) {
	config := fixture(t)
	fingerprint, err := config.Fingerprint()
	if err != nil || len(fingerprint) != 64 {
		t.Fatal(fingerprint, err)
	}
	config.Pairs[0].Relays[0].Address = "LOCALHOST."
	other, _ := config.Fingerprint()
	if fingerprint != other {
		t.Fatal("alias should canonicalize")
	}
	config.Pairs[0], config.Pairs[1] = config.Pairs[1], config.Pairs[0]
	other, _ = config.Fingerprint()
	if fingerprint == other {
		t.Fatal("pair order must affect fingerprint")
	}
}
func TestConfigValidation(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Config)
	}{
		{"version", func(c *Config) { c.Version = 2 }},
		{"no-pairs", func(c *Config) { c.Pairs = nil }},
		{"one-relay", func(c *Config) { c.Pairs[0].Relays = c.Pairs[0].Relays[:1] }},
		{"same-id", func(c *Config) { c.Pairs[1].Relays[0].ID = c.Pairs[0].Relays[0].ID }},
		{"same-pair-id", func(c *Config) { c.Pairs[1].ID = c.Pairs[0].ID }},
		{"same-endpoint", func(c *Config) { c.Pairs[1].Relays[0].ControlPort = 9009 }},
		{"alias", func(c *Config) {
			c.Pairs[1].Relays[0].Address = "::ffff:127.0.0.1"
			c.Pairs[1].Relays[0].DataPort = 9010
		}},
		{"bad-address", func(c *Config) { c.Pairs[0].Relays[0].Address = "https://server" }},
		{"bad-port", func(c *Config) { c.Pairs[0].Relays[0].DataPort = 65536 }},
		{"same-ports", func(c *Config) { c.Pairs[0].Relays[0].DataPort = 9009 }},
		{"bad-env", func(c *Config) { c.Pairs[0].Relays[0].PasswordEnv = "password=value" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(t)
			tc.change(&c)
			if c.Validate() == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}
func TestStrictYAML(t *testing.T) {
	for _, input := range []string{"", "version: 1\nversion: 1", "version: 1\nunknown: super-secret", "version: secret-value", "---\nversion: 1\n---\nversion: 1", strings.Repeat("x", MaxConfigBytes+1)} {
		if _, err := Parse(strings.NewReader(input)); err == nil || strings.Contains(err.Error(), "super-secret") || strings.Contains(err.Error(), "secret-value") {
			t.Fatal("accepted malformed input or leaked value", err)
		}
	}
}
func TestOrderedPairsAndExhaustion(t *testing.T) {
	c := fixture(t)
	for i, name := range []string{"primary", "backup-1"} {
		pair, err := c.Pair(i)
		if err != nil || pair.ID != name {
			t.Fatal(pair, err)
		}
	}
	for _, i := range []int{-1, 2, 3} {
		if _, err := c.Pair(i); !errors.Is(err, ErrExhausted) {
			t.Fatal(err)
		}
	}
}
func TestAgreement(t *testing.T) {
	local := Agreement{Version: 1, Fingerprint: "abc", Transfer: "logical", Pair: "primary", Attempt: 0, Sender: true}
	peer := local
	peer.Sender = false
	if err := local.Validate(peer); err != nil {
		t.Fatal(err)
	}
	cases := []func(*Agreement){func(a *Agreement) { a.Version++ }, func(a *Agreement) { a.Fingerprint = "changed" }, func(a *Agreement) { a.Transfer = "other" }, func(a *Agreement) { a.Pair = "backup" }, func(a *Agreement) { a.Attempt++ }, func(a *Agreement) { a.Sender = true }}
	for _, change := range cases {
		bad := peer
		change(&bad)
		if local.Validate(bad) == nil {
			t.Fatal("accepted mismatched session")
		}
	}
}
func TestExplicitMissingConfigAndCredentials(t *testing.T) {
	t.Setenv("CROC_MULTIRELAY_CONFIG", t.TempDir()+"/missing.yaml")
	if _, err := LoadSelected(""); err == nil {
		t.Fatal("explicit missing config silently disabled")
	}
	t.Setenv("CROC_MR_TEST_PASSWORD", "private-value")
	relay := Relay{PasswordEnv: "CROC_MR_TEST_PASSWORD"}
	if password, err := relay.Password("default"); err != nil || password != "private-value" {
		t.Fatal("password lookup failed")
	}
	relay.PasswordEnv = "CROC_MR_NOT_SET"
	if _, err := relay.Password("default"); err == nil {
		t.Fatal("missing password silently fell back")
	}
}

func TestResolvePinnedPool(t *testing.T) {
	config := fixture(t)
	resolved, err := config.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Pairs[0].Relays[0].Address != "127.0.0.1" {
		t.Fatal("IP not pinned")
	}
	// Same-instance aliases that use different IDs cannot evade port overlap.
	config.Pairs[1].Relays[0].Address = "localhost"
	config.Pairs[1].Relays[0].ControlPort = config.Pairs[0].Relays[0].DataPort
	if _, err = config.Resolve(context.Background()); err == nil {
		t.Fatal("alias overlap accepted")
	}
}

func TestDefaultConfigurationDirectory(t *testing.T) {
	t.Setenv("CROC_CONFIG_DIR", t.TempDir())
	actual, err := DefaultPath()
	if err != nil || actual != filepath.Join(os.Getenv("CROC_CONFIG_DIR"), "multirelay.yaml") {
		t.Fatal(actual, err)
	}
}
