// Package multirelay validates the ordered, disjoint relay pool used by croc.
package multirelay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

const Version = 1
const MaxConfigBytes = 1 << 20

// Relay identifies an isolated relay process. Passwords are read from the
// named environment variable, never from YAML or the canonical fingerprint.
type Relay struct {
	ID          string `yaml:"id" json:"id"`
	Address     string `yaml:"address" json:"address"`
	ControlPort int    `yaml:"control_port" json:"control_port"`
	DataPort    int    `yaml:"data_port" json:"data_port"`
	PasswordEnv string `yaml:"password_env,omitempty" json:"password_env,omitempty"`
}
type Pair struct {
	ID     string  `yaml:"id" json:"id"`
	Relays []Relay `yaml:"relays" json:"relays"`
}
type Config struct {
	Version int    `yaml:"version" json:"version"`
	Pairs   []Pair `yaml:"pairs" json:"pairs"`
}

var identifier = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
var envName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// DefaultPath follows croc's configuration directory precedence, including
// CROC_CONFIG_DIR and XDG_CONFIG_HOME on all supported operating systems.
func DefaultPath() (string, error) {
	if directory, set := os.LookupEnv("CROC_CONFIG_DIR"); set {
		return filepath.Join(directory, "multirelay.yaml"), nil
	}
	if directory, set := os.LookupEnv("XDG_CONFIG_HOME"); set {
		return filepath.Join(directory, "croc", "multirelay.yaml"), nil
	}
	directory, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, ".config", "croc", "multirelay.yaml"), nil
}

// LoadSelected enables the mode only if the explicit path, CROC_MULTIRELAY_CONFIG,
// or the default file exists. An explicit missing or invalid file is an error.
func LoadSelected(path string) (*Config, error) {
	explicit := path != ""
	if !explicit {
		path = os.Getenv("CROC_MULTIRELAY_CONFIG")
		explicit = path != ""
	}
	if path == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return nil, err
		}
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) && !explicit {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open multirelay config: %w", err)
	}
	defer file.Close()
	config, err := Parse(file)
	if err != nil {
		return nil, err
	}
	return &config, nil
}

func Parse(reader io.Reader) (Config, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxConfigBytes+1))
	if err != nil {
		return Config{}, errors.New("cannot read multirelay configuration")
	}
	if len(data) > MaxConfigBytes {
		return Config{}, errors.New("multirelay configuration exceeds 1 MiB")
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	var config Config
	// Parser diagnostics may quote supplied values, including accidentally pasted
	// credentials. Keep the error useful without echoing the input.
	if err := decoder.Decode(&config); err != nil {
		return Config{}, errors.New("invalid multirelay YAML: check schema, duplicate keys and value types")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Config{}, errors.New("multirelay YAML must contain exactly one document")
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func canonicalHost(host string) (string, error) {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "localhost" {
		return "127.0.0.1", nil
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() {
			return "", errors.New("invalid relay IP address")
		}
		if ip.IsLoopback() {
			return "127.0.0.1", nil
		}
		return ip.Unmap().String(), nil
	}
	if len(host) == 0 || len(host) > 253 {
		return "", errors.New("invalid relay hostname")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("invalid relay hostname")
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-') {
				return "", errors.New("invalid relay hostname")
			}
		}
	}
	return host, nil
}

func (config Config) Validate() error {
	if config.Version != Version {
		return fmt.Errorf("unsupported multirelay configuration version %d", config.Version)
	}
	if len(config.Pairs) == 0 || len(config.Pairs) > 32 {
		return errors.New("multirelay requires 1 to 32 ordered pairs")
	}
	pairs, relays, endpoints := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for pi, pair := range config.Pairs {
		if !identifier.MatchString(pair.ID) || pairs[pair.ID] {
			return fmt.Errorf("pair %d: invalid or duplicate id", pi)
		}
		pairs[pair.ID] = true
		if len(pair.Relays) != 2 {
			return fmt.Errorf("pair %d: exactly two relays required", pi)
		}
		for ri, relay := range pair.Relays {
			if !identifier.MatchString(relay.ID) || relays[relay.ID] {
				return fmt.Errorf("pair %d lane %d: invalid or reused relay id", pi, ri)
			}
			relays[relay.ID] = true
			host, err := canonicalHost(relay.Address)
			if err != nil {
				return fmt.Errorf("pair %d lane %d: invalid address", pi, ri)
			}
			if relay.ControlPort < 1 || relay.ControlPort > 65535 || relay.DataPort < 1 || relay.DataPort > 65535 || relay.ControlPort == relay.DataPort {
				return fmt.Errorf("pair %d lane %d: distinct control/data ports in 1..65535 required", pi, ri)
			}
			if relay.PasswordEnv != "" && !envName.MatchString(relay.PasswordEnv) {
				return fmt.Errorf("pair %d lane %d: invalid password environment name", pi, ri)
			}
			for _, port := range []int{relay.ControlPort, relay.DataPort} {
				endpoint := net.JoinHostPort(host, fmt.Sprint(port))
				if endpoints[endpoint] {
					return fmt.Errorf("pair %d lane %d: reused relay endpoint or recognizable alias", pi, ri)
				}
				endpoints[endpoint] = true
			}
		}
	}
	return nil
}

// Fingerprint is deterministic, ordered, and contains no credential values.
func (config Config) Fingerprint() (string, error) {
	if err := config.Validate(); err != nil {
		return "", err
	}
	canonical := Config{Version: config.Version, Pairs: make([]Pair, len(config.Pairs))}
	for i, pair := range config.Pairs {
		canonical.Pairs[i] = Pair{ID: pair.ID, Relays: append([]Relay(nil), pair.Relays...)}
		for j := range pair.Relays {
			canonical.Pairs[i].Relays[j].Address, _ = canonicalHost(pair.Relays[j].Address)
		}
	}
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
func (relay Relay) ControlAddress() string {
	return net.JoinHostPort(relay.Address, fmt.Sprint(relay.ControlPort))
}
func (relay Relay) DataAddress() string {
	return net.JoinHostPort(relay.Address, fmt.Sprint(relay.DataPort))
}
func (relay Relay) Password(fallback string) (string, error) {
	if relay.PasswordEnv == "" {
		return fallback, nil
	}
	value, ok := os.LookupEnv(relay.PasswordEnv)
	if !ok || value == "" {
		return "", errors.New("required relay password environment variable is unset")
	}
	return value, nil
}

var ErrExhausted = errors.New("no unused multirelay pair remains")

func (config Config) Pair(attempt int) (Pair, error) {
	if attempt < 0 || attempt >= len(config.Pairs) {
		return Pair{}, ErrExhausted
	}
	return config.Pairs[attempt], nil
}

// Agreement is carried only inside croc's PAKE-authenticated encrypted channel.
// Validate compares public session state. It does not authenticate raw input.
type Agreement struct {
	Version     int    `json:"version"`
	Fingerprint string `json:"fingerprint"`
	Transfer    string `json:"transfer"`
	Pair        string `json:"pair"`
	Attempt     int    `json:"attempt"`
	Sender      bool   `json:"sender"`
}

func (expected Agreement) Validate(peer Agreement) error {
	if expected.Version != Version || peer.Version != Version || expected.Fingerprint != peer.Fingerprint || expected.Transfer != peer.Transfer || expected.Pair != peer.Pair || expected.Attempt != peer.Attempt || expected.Sender == peer.Sender {
		return errors.New("authenticated multirelay configuration, attempt or role mismatch")
	}
	return nil
}

// Resolve pins endpoints for the lifetime of a logical transfer and checks DNS
// aliases across the whole pool. It cannot attest that different endpoints are
// operated by distinct machines; that remains a deployment assumption.
func (config Config) Resolve(ctx context.Context) (Config, error) {
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	result := Config{Version: config.Version, Pairs: make([]Pair, len(config.Pairs))}
	endpoints := map[string]bool{}
	for i, pair := range config.Pairs {
		result.Pairs[i] = Pair{ID: pair.ID, Relays: append([]Relay(nil), pair.Relays...)}
		for j, relay := range pair.Relays {
			host, err := canonicalHost(relay.Address)
			if err != nil {
				return Config{}, err
			}
			var addresses []netip.Addr
			if address, err := netip.ParseAddr(host); err == nil {
				addresses = []netip.Addr{address}
			} else {
				addresses, err = net.DefaultResolver.LookupNetIP(ctx, "ip", host)
				if err != nil || len(addresses) == 0 {
					return Config{}, fmt.Errorf("pair %d lane %d: relay DNS resolution failed", i, j)
				}
			}
			sort.Slice(addresses, func(a, b int) bool { return addresses[a].Compare(addresses[b]) < 0 })
			local := map[string]bool{}
			for _, address := range addresses {
				canonical, err := canonicalHost(address.String())
				if err != nil {
					return Config{}, fmt.Errorf("pair %d lane %d: unusable resolved address", i, j)
				}
				for _, port := range []int{relay.ControlPort, relay.DataPort} {
					endpoint := net.JoinHostPort(canonical, fmt.Sprint(port))
					if local[endpoint] {
						continue
					}
					local[endpoint] = true
					if endpoints[endpoint] {
						return Config{}, fmt.Errorf("pair %d lane %d: relay DNS aliases overlap another endpoint", i, j)
					}
				}
			}
			for endpoint := range local {
				endpoints[endpoint] = true
			}
			result.Pairs[i].Relays[j].Address = addresses[0].Unmap().String()
		}
	}
	return result, nil
}
