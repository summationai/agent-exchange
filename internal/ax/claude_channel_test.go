package ax

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Claude can drop channel opt-in when agent view re-hosts a conversation.
// Exercise the launch boundary for both fresh and resumed conversations.
func TestClaudeLaunchKeepsChannelInForeground(t *testing.T) {
	for _, resume := range []bool{false, true} {
		name := "fresh"
		if resume {
			name = "resumed"
		}
		t.Run(name, func(t *testing.T) {
			dir, bin := startTestServer(t), testDir(t)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			out := filepath.Join(bin, "args")
			script := "#!/bin/sh\nif [ \"$1\" = --help ]; then exit 0; fi\nprintf '%s\\0' \"$@\" > " + shellQuote(out) + "\n"
			if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			args := []string{"-n", "api"}
			if resume {
				args = append(args, "-r", uuid())
			}
			if err := Launch(context.Background(), dir, "claude", args); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			argv := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
			i := slices.Index(argv, "--dangerously-load-development-channels")
			if i < 0 || i+1 >= len(argv) || argv[i+1] != "server:ax" {
				t.Fatal("AX channel was not enabled")
			}
			i = slices.Index(argv, "--settings")
			if i < 0 || i+1 >= len(argv) {
				t.Fatal("missing native settings")
			}
			settings := argv[i+1]
			var config struct {
				DisableAgentView bool                       `json:"disableAgentView"`
				Hooks            map[string]json.RawMessage `json:"hooks"`
			}
			if err := json.Unmarshal([]byte(settings), &config); err != nil {
				t.Fatal(err)
			}
			if !config.DisableAgentView || len(config.Hooks) == 0 {
				t.Fatal("Claude can re-host the session and silently lose the AX channel")
			}
			// Optional native contract check. This read-only command must refuse
			// agent view before it starts a supervisor or a model session.
			if native := os.Getenv("AX_TEST_CLAUDE_BIN"); native != "" {
				ctx, cancel := context.WithTimeout(context.Background(), doctorProbeTimeout)
				defer cancel()
				output, err := exec.CommandContext(ctx, native, "agents", "--json", "--settings", settings).CombinedOutput()
				if ctx.Err() != nil || err == nil || !strings.Contains(string(output), "disabled by the 'disableAgentView' setting") {
					t.Fatalf("native Claude did not refuse re-hosting: %v: %s", err, output)
				}
			}
		})
	}
}

// Tool discovery happens before Claude's first model turn. A spawned peer must
// not consume its first queued task until its fixed startup turn calls AX.
func TestClaudeSpawnWaitsForStartupToolCall(t *testing.T) {
	dir := startTestServer(t)
	s := Session{ID: randomID("agt_"), Secret: randomID(""), Name: "worker", Host: "claude", Mesh: testMesh, Native: uuid(), Started: true, ClaudeStartup: true}
	file := filepath.Join(dir, "session.json")
	if err := saveSession(file, s); err != nil {
		t.Fatal(err)
	}
	c, err := dial(socketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	for _, step := range []struct {
		method string
		args   any
	}{
		{"ax.enroll", s}, {"ax.connect", object{"version": "1", "agent_id": s.ID, "secret": s.Secret}},
		{"ax.presence", object{"native_session_id": s.Native, "permission_mode": "default", "state": "ready"}},
	} {
		if err := c.call(step.method, step.args, nil); err != nil {
			t.Fatal(err)
		}
	}
	sender := connectDiscoveryPeer(t, dir, Session{ID: randomID("agt_"), Secret: randomID(""), Name: "coordinator", Host: "codex", Mesh: testMesh, Native: uuid()})
	var sent Message
	if err := sender.call("ax.send", object{"target": "worker", "text": "first task", "client_message_id": "first"}, &sent); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	b := &bridge{session: s, file: file, dir: dir, c: c, ctx: context.Background(), out: &out, toolsListed: true}
	b.bootstrap()
	if b.active || out.Len() != 0 {
		t.Fatal("MCP discovery released mail before the startup turn")
	}
	var status struct{ Message Message }
	if err := sender.call("ax.status", object{"message_id": sent.ID}, &status); err != nil {
		t.Fatal(err)
	}
	if status.Message.State != "queued" {
		t.Fatal("first task was offered too early")
	}
	if err := b.toolCall(context.Background(), nil, "ax.list", object{}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case offer := <-c.offers:
		if offer.ID != sent.ID {
			t.Fatal("wrong first task")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("startup tool call did not release the first task")
	}
	saved, err := loadSession(file)
	if err != nil || saved.ClaudeStartup {
		t.Fatal("startup barrier was not durably cleared")
	}
	b.bootstrap()
	select {
	case <-c.offers:
		t.Fatal("bootstrap replayed the first task")
	default:
	}
	// A replacement bridge resumes discovery activation, with no second setup turn.
	b.active, b.bootstrapped = false, false
	b.bootstrap()
	if !b.active || out.Len() != 0 {
		t.Fatal("reconnect required another startup turn")
	}
}

func TestClaudeStartupPreservesNativePrompts(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{nil, true}, {[]string{"--exclude-dynamic-system-prompt-sections"}, true},
		{[]string{"--bare"}, false}, {[]string{"--safe-mode"}, false}, {[]string{"--permission-mode", "bypassPermissions"}, true},
		{[]string{"--model", "sonnet", "--effort=high"}, true},
		{[]string{"--mcp-config", "config.json", "--allowedTools", "Read", "Bash"}, true},
		{[]string{"--"}, true}, {[]string{"--", "native prompt"}, false},
		{[]string{"native prompt"}, false}, {[]string{"--model", "sonnet", "native prompt"}, false},
		{[]string{"--resume"}, false}, {[]string{"-r", "saved"}, false},
		{[]string{"--print"}, false}, {[]string{"--help"}, false},
		{[]string{"--future-option", "value"}, false}, {[]string{"--model"}, false},
	} {
		if got := claudeNeedsStartup(tc.args); got != tc.want {
			t.Fatalf("%q: seed=%v want=%v", tc.args, got, tc.want)
		}
	}
}
