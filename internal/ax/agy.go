package ax

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const agyInstructions = "On each new user turn, call the AX check_inbox tool once before other work; read any returned peer task and guidance. Native hooks bind this conversation and report readiness. There is no automatic idle wake; replies need another user turn. Never poll or sleep waiting. " + delegation

func agyFlagName(arg string) string {
	return strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
}

func agyTakesValue(arg string) bool {
	if !strings.HasPrefix(arg, "-") {
		return false
	}
	switch agyFlagName(arg) {
	case "agent", "model", "project", "effort", "mode", "print", "p", "prompt", "prompt-interactive", "i", "conversation", "resume", "r", "add-dir", "log-file", "input-format", "output-format", "json-schema", "print-timeout", "settings":
		return true
	}
	return false
}

// Match the native Go flag parser: one or two dashes, string values consumed
// even when they look like flags, and no options after a positional argument.
func agyOptions(args []string) []string {
	var options []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" || !strings.HasPrefix(arg, "-") || arg == "-" {
			break
		}
		options = append(options, agyFlagName(arg))
		if agyTakesValue(arg) {
			i++
		}
	}
	return options
}

func prepareAgy(_ context.Context, dir, file, _ string, s Session, args, env []string, _ chan<- error) ([]string, []string, func(), error) {
	noop := func() {}
	for _, arg := range agyOptions(args) {
		if arg == "h" || arg == "help" {
			return args, env, noop, nil
		}
	}
	if len(args) > 0 && args[0] == "acp" {
		return nil, nil, noop, errors.New("AX supports the Antigravity CLI, not its multi-session ACP server mode")
	}
	if !s.AllowBypass {
		return nil, nil, noop, errors.New("Antigravity cannot report its live approval mode; authorize AX delivery once with AX_ALLOW_BYPASS=1, without changing Antigravity permissions")
	}
	// Remote agents do not inherit this machine's optional MCP child or hooks.
	remote := false
	for _, arg := range agyOptions(args) {
		if arg == "remote-agent" {
			remote = true
		}
		if value, ok := strings.CutPrefix(arg, "remote-agent="); ok {
			remote, _ = strconv.ParseBool(value)
		}
	}
	if remote {
		return nil, nil, noop, errors.New("AX Antigravity messaging needs a local agent; remote-agent is not supported")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, noop, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, noop, err
	}
	if err = installAgyPlugin(home, exe); err != nil {
		return nil, nil, noop, err
	}
	s.DeliveryMode = "manual"
	if err = saveSession(file, s); err != nil {
		return nil, nil, noop, err
	}
	if len(args) == 0 && s.Native != "" {
		args = []string{"--resume", s.Native}
	}
	return args, append(env, "AX_AGY_PID="+strconv.Itoa(os.Getpid())), noop, nil
}

// The plugin is shared across launches; identity stays in the inherited AX
// environment, never in global configuration. Ordinary agy sessions get no tools.
func installAgyPlugin(home, exe string) error {
	dir := filepath.Join(home, ".gemini", "config", "plugins", "ax")
	if err := privateDir(dir); err != nil {
		return err
	}
	// Independent AX names share this plugin; use the existing bounded wait.
	lock, err := lockSession(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	marker := filepath.Join(dir, "ax-managed.json")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		var owned struct{ Owner string }
		data, readErr := os.ReadFile(marker)
		if readErr != nil || json.Unmarshal(data, &owned) != nil || owned.Owner != "agent-exchange" {
			return errors.New("Antigravity already has an unrelated ax plugin; preserve it and rename it before using AX")
		}
	}
	hooks := object{}
	for _, event := range []string{"SessionStart", "PreInvocation", "Stop"} {
		// Hooks run through sh -c. Exec preserves agy's direct parent identity,
		// just as the native MCP command does, without process-tree polling.
		hooks[event] = []object{{"type": "command", "command": "exec " + shellQuote(exe) + " agy-hook " + event, "timeout": 3}}
	}
	for _, entry := range []struct {
		name  string
		value any
	}{
		{"ax-managed.json", object{"owner": "agent-exchange"}},
		{"mcp_config.json", object{"mcpServers": object{"ax": object{"command": exe, "args": []string{"agy-bridge"}}}}},
		{"hooks.json", object{"ax-messaging": hooks}},
		{"plugin.json", object{"name": "ax", "displayName": "Agent Exchange", "description": "Local AX messaging for named Antigravity CLI sessions."}},
	} {
		if err := savePrivateJSON(filepath.Join(dir, entry.name), entry.value); err != nil {
			return err
		}
	}
	return nil
}

func activeAgySession(file string) (Session, bool) {
	s, err := loadSession(file)
	return s, err == nil && s.Host == "agy" && s.LaunchID != "" && s.LaunchID == os.Getenv("AX_LAUNCH_ID") && os.Getenv("AX_AGY_PID") == strconv.Itoa(os.Getppid())
}

// Publish revocation before attempting the session lock. Each hook approves
// only its own generation, so it cannot clear a newer hook's revocation.
func suspendAgyBinding(file, launch string) (string, error) {
	generation := launch + "." + randomID("")
	path := file + ".agy-binding"
	if err := savePrivateJSON(path, generation); err != nil {
		// A missing marker also denies access if replacing it fails.
		_ = os.Remove(path)
		return "", err
	}
	return generation, nil
}

func agyBindingMatches(file, generation string) bool {
	if generation == "" {
		return false
	}
	f, err := privateFile(file+".agy-binding", syscall.O_RDONLY|syscall.O_NONBLOCK)
	if err != nil {
		return false
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, 256))
	return err == nil && bytes.Equal(body, raw(generation))
}

func confirmAgyBinding(file, launch, native, generation string) error {
	lock, err := lockSession(file)
	if err != nil {
		return err
	}
	defer lock.Close()
	s, active := activeAgySession(file)
	if !active || s.LaunchID != launch || !s.Started || s.Native != native || s.BindingError != "" || !agyBindingMatches(file, generation) {
		return errors.New("Antigravity hook no longer owns the current AX binding")
	}
	s.AgyBinding = generation
	return saveSession(file, s)
}

// A global plugin must remain a valid, inert MCP server outside an AX launch.
// Reuse Main's cancellable input pipe; no broker or reconnect loop is started.
func inactiveAgyBridge(in io.Reader, out io.Writer) error {
	scan := bufio.NewScanner(in)
	scan.Buffer(make([]byte, 4096), maxFrame)
	enc := json.NewEncoder(out)
	for scan.Scan() {
		var p packet
		if err := json.Unmarshal(scan.Bytes(), &p); err != nil {
			return err
		}
		if len(p.ID) == 0 {
			continue
		}
		r := packet{JSONRPC: "2.0", ID: p.ID}
		switch p.Method {
		case "initialize":
			var params struct {
				Version string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(p.Params, &params)
			if params.Version != "2025-11-25" && params.Version != "2025-06-18" && params.Version != "2024-11-05" {
				params.Version = "2025-11-25"
			}
			r.Result = raw(object{"protocolVersion": params.Version, "capabilities": object{"tools": object{}}, "serverInfo": object{"name": "ax", "version": Version}})
		case "tools/list":
			r.Result = raw(object{"tools": []object{}})
		case "ping":
			r.Result = raw(object{})
		default:
			r.Error = &rpcError{Code: -32601, Message: "AX is inactive; launch Antigravity with ax agy -n NAME"}
		}
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return scan.Err()
}

// Hooks report native identity and readiness, never peer bodies or receipts.
// The shared check_inbox tool is the sole content gate.
func agyHook(dir, file, event string, in io.Reader, out io.Writer) error {
	defer func() { _ = json.NewEncoder(out).Encode(object{}) }()
	s, active := activeAgySession(file)
	if !active {
		return nil
	}
	// Revoke the previous turn's evidence before reading potentially malformed
	// input. A failed or timed-out hook must not leave old mail readable.
	generation, err := suspendAgyBinding(file, s.LaunchID)
	if err != nil {
		return err
	}
	var input struct {
		Native string `json:"conversationId"`
		Idle   bool   `json:"fullyIdle"`
	}
	if err := json.NewDecoder(io.LimitReader(in, maxFrame)).Decode(&input); err != nil {
		return err
	}
	if event != "SessionStart" && event != "PreInvocation" && event != "Stop" {
		return errors.New("unsupported Antigravity hook")
	}
	hostEvent := "AgyBusy"
	if event == "SessionStart" || event == "Stop" && input.Idle {
		hostEvent = "SessionStart"
	}

	// Reuse the shared lifecycle path, including its persistent binding-conflict
	// guard, so switching conversations cannot keep using the previous identity.
	payload := raw(object{"session_id": input.Native, "hook_event_name": hostEvent, "permission_mode": "unverified"})
	if err := Hook(dir, file, bytes.NewReader(payload), s.LaunchID); err != nil {
		return err
	}
	// Only a model invocation opens delivery. Startup and Stop report native
	// state but leave mail queued until the next verified invocation.
	if event == "PreInvocation" {
		return confirmAgyBinding(file, s.LaunchID, input.Native, generation)
	}
	return nil
}
