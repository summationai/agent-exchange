package ax

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

var validOrigin = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

type provenanceFields struct {
	Origin  string `json:"origin"`
	Trust   string `json:"trust"`
	Tainted *bool  `json:"tainted"`
}

func jsonObject(value json.RawMessage, bound int, name string) error {
	if len(value) > bound {
		return fmt.Errorf("%s must be a JSON object at most %d bytes", name, bound)
	}
	var fields map[string]json.RawMessage
	if len(bytes.TrimSpace(value)) == 0 || json.Unmarshal(value, &fields) != nil || fields == nil {
		return fmt.Errorf("%s must be a JSON object", name)
	}
	return nil
}

func validateRelayContent(p *peer, provenance, data json.RawMessage) error {
	if p.Kind == "relay" && len(provenance) == 0 {
		return errors.New("a relay must send provenance")
	}
	if p.Kind != "relay" && len(provenance) > 0 {
		return errors.New("only a relay endpoint may set provenance")
	}
	if len(provenance) > 0 {
		if err := jsonObject(provenance, 4<<10, "provenance"); err != nil {
			return err
		}
		var fields provenanceFields
		if err := json.Unmarshal(provenance, &fields); err != nil || !validOrigin.MatchString(fields.Origin) || !slices.Contains([]string{"verified", "unverified", "system"}, fields.Trust) || fields.Tainted == nil {
			return errors.New("provenance requires origin token, trust (verified, unverified, system), and boolean tainted")
		}
	}
	if len(data) > 0 {
		return jsonObject(data, maxData, "data")
	}
	return nil
}

func validExternal(policy string) bool {
	return slices.Contains([]string{"", "refuse", "verified", "all"}, policy)
}
func externalPolicy(a Agent) string {
	if a.External == "" {
		return "refuse"
	}
	return a.External
}
func admitsExternal(to *peer, provenance json.RawMessage) (bool, string) {
	switch externalPolicy(to.Agent) {
	case "all":
		return true, ""
	case "verified":
		var p provenanceFields
		if json.Unmarshal(provenance, &p) != nil || p.Tainted == nil {
			return false, "invalid provenance"
		}
		if *p.Tainted {
			return false, "message is tainted"
		}
		if p.Trust != "verified" && p.Trust != "system" {
			return false, "message is unverified"
		}
		return true, ""
	default:
		return false, "endpoint is closed to external messages"
	}
}

// Versions before 0.8 render every message with peer delegation guidance.
// Development builds from 0.8 sources already implement the new boundary.
func relayVersion(version string) bool {
	var major, minor, patch int
	_, err := fmt.Sscanf(strings.TrimPrefix(version, "v"), "%d.%d.%d", &major, &minor, &patch)
	return err == nil && major >= 0 && minor >= 0 && patch >= 0 && (major > 0 || major == 0 && minor >= 8)
}

func externalHandoff(to *peer, m Message) bool {
	if len(m.Provenance) == 0 {
		return true
	}
	ok, _ := admitsExternal(to, m.Provenance)
	return ok && to.relayGuidance
}

func messageQueueReason(to, from *peer, m Message) string {
	if len(m.Provenance) > 0 {
		if ok, _ := admitsExternal(to, m.Provenance); !ok {
			return "Recipient external policy is " + externalPolicy(to.Agent) + "; this relayed message waits."
		}
		if !to.relayGuidance {
			return "Recipient bridge lacks external guidance; finish the AX update and relaunch this session."
		}
	}
	return queueReason(to, from)
}

// Snapshot comparison avoids polling on unchanged heartbeats. Only opted-in
// bridges get hints; their client coalesces them before fetching the list.
func (b *broker) peerSnapshots() string {
	for _, p := range b.peers {
		if p.conn != nil && p.conn.peerEvents {
			return string(raw(b.list()))
		}
	}
	return ""
}
func (b *broker) peersChanged() {
	for _, p := range b.peers {
		if p.conn != nil && p.conn.peerEvents {
			if p.conn.send(packet{Method: "ax.peers.changed"}) != nil {
				p.conn.Close()
			}
		}
	}
}

func peerChangeMethod(method string) bool {
	switch method {
	case "ax.enroll", "ax.connect", "ax.presence", "ax.lifecycle", "ax.external", "ax.policy", "ax.ready", "ax.capabilities":
		return true
	}
	return false
}
