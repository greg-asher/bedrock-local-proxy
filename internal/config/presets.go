package config

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxPresetNameLength bounds a preset name conservatively. The name
	// becomes a path segment and part of a generated Codex profile name.
	MaxPresetNameLength = 32
	// PortScanRange bounds the free-port search so an exhausted scan reports
	// the range it covered instead of proposing something unusable.
	PortScanRange = 100
)

// ValidatePresetName accepts only names that are safe as a filesystem path
// segment and as a Codex profile name. Lowercase only, because a name becomes
// a filename on case-insensitive filesystems; no dots or separators, so a
// name can never escape the directory it is joined into.
func ValidatePresetName(name string) error {
	if name == "" {
		return errors.New("preset name must not be empty")
	}
	if len(name) > MaxPresetNameLength {
		return fmt.Errorf("preset name must be at most %d characters", MaxPresetNameLength)
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '-' || r == '_') && i > 0:
		default:
			return fmt.Errorf("preset name %q must start with a lowercase letter or digit and contain only lowercase letters, digits, hyphens, and underscores", name)
		}
	}
	return nil
}

// NormalizeSessionTag validates and trims the optional ingestion tag. It is
// the single rule for both `--session-tag` and a preset's session_tag, so the
// two can never diverge.
func NormalizeSessionTag(value string) (string, error) {
	if err := validateSessionTag(value); err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func validateSessionTag(value string) error {
	if !utf8.ValidString(value) {
		return errors.New("must be valid UTF-8")
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return errors.New("must not be empty")
	}
	if len([]byte(trimmed)) > 128 {
		return errors.New("must be at most 128 UTF-8 bytes")
	}
	for _, r := range trimmed {
		if unicode.IsControl(r) {
			return errors.New("must not contain control characters")
		}
	}
	return nil
}

// ClaimedPorts reports every loopback address this configuration already
// commits to: the unnamed default's listen plus every preset's.
func (c Config) ClaimedPorts() map[string]string {
	claimed := map[string]string{c.Listen: "the top-level listen"}
	for _, name := range c.PresetNames() {
		claimed[c.Presets[name].Listen] = "presets." + name
	}
	return claimed
}

// FreeListenAddress proposes the first loopback address that this
// configuration does not already claim and that nothing is currently bound
// to. It walks upward from the top-level listen port over PortScanRange.
//
// A successful probe is advisory, not a reservation: another process can take
// the port before the proxy starts. That is acceptable because the proxy
// already fails clearly at bind. The scan removes the common case, not the
// race.
func (c Config) FreeListenAddress(probe func(string) bool) (string, error) {
	if probe == nil {
		probe = PortIsFree
	}
	host, portText, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return "", fmt.Errorf("read top-level listen %q: %w", c.Listen, err)
	}
	start, err := strconv.Atoi(portText)
	if err != nil {
		return "", fmt.Errorf("read top-level listen port %q: %w", portText, err)
	}
	claimed := c.ClaimedPorts()
	for port := start + 1; port <= start+PortScanRange; port++ {
		if port > 65535 {
			break
		}
		candidate := net.JoinHostPort(host, strconv.Itoa(port))
		if _, taken := claimed[candidate]; taken {
			continue
		}
		if !probe(candidate) {
			continue
		}
		return candidate, nil
	}
	return "", fmt.Errorf("no free loopback port between %d and %d; pass --listen with a chosen address", start+1, start+PortScanRange)
}

// PortIsFree reports whether a loopback address can be bound right now. The
// listener is closed immediately; this is a probe, not a reservation.
func PortIsFree(address string) bool {
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}
