package ax

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func spawnFixture(t *testing.T) (string, Session) {
	t.Helper()
	dir := startTestServer(t)
	bin := testDir(t)
	for _, name := range []string{"tmux", "claude", "codex"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir, Session{ID: randomID("agt_"), SpawnRoot: randomID("tree_"), Workspace: bin, Terminal: terminalContext{Kind: "tmux", Socket: "/tmp/ax-test-socket", Pane: "%7"}}
}

func TestSpawnRetryKeepsOnePaneAndIdentity(t *testing.T) {
	dir, parent := spawnFixture(t)
	var calls atomic.Int32
	open := func(_ context.Context, terminal terminalContext, _, _, _ string) (string, error) {
		if terminal != parent.Terminal {
			t.Error("used another terminal's context")
		}
		calls.Add(1)
		return "%8", nil
	}
	req := spawnRequest{Host: "codex", Name: "worker", Args: []string{"-m", "a model with spaces"}}
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := spawnAgent(context.Background(), dir, parent, req, open)
			if err != nil || r.Ready || r.AgentID == "" {
				t.Errorf("unexpected launch result: %+v %v", r, err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("opened %d panes", calls.Load())
	}
	req.Args = []string{"-m", "different-model"}
	if _, err := spawnAgent(context.Background(), dir, parent, req, open); err == nil {
		t.Fatal("accepted a conflicting retry")
	}
	s, err := loadSession(filepath.Join(dir, "sessions", "worker.json"))
	if err != nil || s.AllowBypass || s.SpawnDepth != 1 || s.SpawnRoot != parent.SpawnRoot {
		t.Fatalf("incorrect child identity: %+v %v", s, err)
	}
}

func TestSpawnUnknownOutcomeNeverRetriesTerminal(t *testing.T) {
	dir, parent := spawnFixture(t)
	calls := 0
	open := func(context.Context, terminalContext, string, string, string) (string, error) {
		calls++
		return "", errors.New("terminal timeout after a possible split")
	}
	req := spawnRequest{Host: "claude", Name: "uncertain"}
	for range 2 {
		r, err := spawnAgent(context.Background(), dir, parent, req, open)
		if err != nil || r.Phase != "launch_requested" || r.Problem == "" || r.Ready {
			t.Fatalf("uncertain outcome hidden: %+v %v", r, err)
		}
	}
	if calls != 1 {
		t.Fatal("repeated an uncertain terminal mutation")
	}
}

func TestSpawnLateTerminalErrorDoesNotOverwriteDispatcher(t *testing.T) {
	for _, phase := range []string{"starting", "exited", "failed"} {
		t.Run(phase, func(t *testing.T) {
			dir, parent := spawnFixture(t)
			problem := ""
			if phase == "failed" {
				problem = "native launch failed"
			}
			open := func(context.Context, terminalContext, string, string, string) (string, error) {
				if err := updateSpawn(dir, "worker", func(r *spawnRecord) { r.Phase, r.Error = phase, problem }); err != nil {
					t.Fatal(err)
				}
				return "", errors.New("late terminal timeout")
			}
			r, err := spawnAgent(context.Background(), dir, parent, spawnRequest{Host: "claude", Name: "worker"}, open)
			if err != nil || r.Phase != phase || r.Problem != problem {
				t.Fatalf("lost dispatcher evidence: %+v %v", r, err)
			}
		})
	}
}

func TestSpawnChecksNamesArgumentsAndTreeBounds(t *testing.T) {
	dir, parent := spawnFixture(t)
	open := func(context.Context, terminalContext, string, string, string) (string, error) { return "%8", nil }
	for _, req := range []spawnRequest{
		{Host: "unknown", Name: "worker"}, {Host: "claude", Name: "../escape"},
		{Host: "claude", Name: "worker", Args: []string{"--name", "hijack"}},
		{Host: "claude", Name: "worker", Args: []string{"-n", "hijack"}},
		{Host: "claude", Name: "worker", Args: []string{"--name=hijack"}},
		{Host: "claude", Name: "worker", Args: []string{"bad\x00arg"}},
		{Host: "claude", Name: "worker", CWD: "relative"},
	} {
		if _, err := spawnAgent(context.Background(), dir, parent, req, open); err == nil {
			t.Fatalf("accepted invalid request: %+v", req)
		}
	}
	for i := range maxSpawnTree {
		req := spawnRequest{Host: "claude", Name: "worker" + string(rune('a'+i))}
		if _, err := spawnAgent(context.Background(), dir, parent, req, open); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := spawnAgent(context.Background(), dir, parent, spawnRequest{Host: "claude", Name: "extra"}, open); err == nil {
		t.Fatal("tree quota not enforced")
	}
	parent.SpawnDepth = maxSpawnDepth
	if _, err := spawnAgent(context.Background(), dir, parent, spawnRequest{Host: "claude", Name: "deep"}, open); err == nil {
		t.Fatal("nested launch depth not enforced")
	}
	if err := Launch(context.Background(), dir, "claude", []string{"--name", "workera"}); err == nil {
		t.Fatal("normal launch stole a pending pane reservation")
	}
}

func TestSpawnReadinessRequiresNativeBindingAndBridge(t *testing.T) {
	dir, parent := spawnFixture(t)
	open := func(context.Context, terminalContext, string, string, string) (string, error) { return "%8", nil }
	if _, err := spawnAgent(context.Background(), dir, parent, spawnRequest{Host: "claude", Name: "worker"}, open); err != nil {
		t.Fatal(err)
	}
	s, _ := loadSession(filepath.Join(dir, "sessions", "worker.json"))
	c, err := dial(socketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	if err = c.call("ax.connect", object{"version": "1", "agent_id": s.ID, "secret": s.Secret}, nil); err != nil {
		t.Fatal(err)
	}
	if err = c.call("ax.presence", object{"native_session_id": uuid(), "state": "ready", "permission_mode": "default"}, nil); err != nil {
		t.Fatal(err)
	}
	r, _ := spawnStatus(context.Background(), dir, "worker")
	if r.Ready || r.Phase != "native_started" {
		t.Fatalf("ready before bridge activation: %+v", r)
	}
	if err = c.call("ax.ready", object{}, nil); err != nil {
		t.Fatal(err)
	}
	r, _ = spawnStatus(context.Background(), dir, "worker")
	if !r.Ready || r.Phase != "connected" {
		t.Fatalf("native readiness missed: %+v", r)
	}
	parent.Name, parent.Host, parent.Mesh, parent.Secret = "parent", "claude", testMesh, randomID("")+randomID("")
	parent.Native = uuid()
	sender, err := dial(socketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer sender.close()
	for _, step := range []struct {
		method string
		args   any
	}{
		{"ax.enroll", parent},
		{"ax.connect", object{"version": "1", "agent_id": parent.ID, "secret": parent.Secret}},
		{"ax.presence", object{"native_session_id": parent.Native, "state": "ready", "permission_mode": "default"}},
		{"ax.ready", object{}},
	} {
		if err = sender.call(step.method, step.args, nil); err != nil {
			t.Fatal(err)
		}
	}
	var sent Message
	if err = sender.call("ax.send", object{"target": "worker", "text": "Review this change", "client_message_id": "task"}, &sent); err != nil {
		t.Fatal(err)
	}
	select {
	case offered := <-c.offers:
		if offered.ID != sent.ID {
			t.Fatal("wrong task offered")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("spawned identity did not receive queued task")
	}
	if err = c.call("ax.receipt", object{"message_id": sent.ID, "receipt": "channel_written"}, nil); err != nil {
		t.Fatal(err)
	}
	if err = c.call("ax.reply", object{"message_id": sent.ID, "text": "Reviewed", "client_message_id": "reply"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case reply := <-sender.offers:
		if reply.Parent != sent.ID || reply.Text != "Reviewed" {
			t.Fatalf("wrong reply: %+v", reply)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reply did not return to the parent")
	}
}

func TestTerminalCommandTargetsCapturedPane(t *testing.T) {
	dir, parent := spawnFixture(t)
	output := filepath.Join(parent.Workspace, "terminal-args")
	script := "#!/bin/sh\nprintf '%s\\0' \"$@\" > " + shellQuote(output) + "\nprintf '%%19\\n'\n"
	if err := os.WriteFile(filepath.Join(parent.Workspace, "tmux"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_PANE", "%999")
	pane, err := openAgentPane(context.Background(), parent.Terminal, dir, "worker", strings.Repeat("x", 64))
	if err != nil || pane != "%19" {
		t.Fatalf("pane: %s %v", pane, err)
	}
	data, _ := os.ReadFile(output)
	args := strings.Split(string(data), "\x00")
	want := []string{"-S", parent.Terminal.Socket, "split-window", "-h", "-d", "-P", "-F", "#{pane_id}", "-t", "%7"}
	if !reflect.DeepEqual(args[:len(want)], want) {
		t.Fatalf("target changed: %q", args)
	}
	if !strings.Contains(args[len(want)], " spawn-run ") {
		t.Fatal("terminal did not receive the reservation dispatcher")
	}
}

func TestSpawnCLISharesTerminalBudgetAndHonorsParentDepth(t *testing.T) {
	dir, parent := spawnFixture(t)
	t.Setenv("AX_SESSION_FILE", "")
	t.Setenv("TMUX", parent.Terminal.Socket+",123,1")
	t.Setenv("TMUX_PANE", parent.Terminal.Pane)
	if err := os.WriteFile(filepath.Join(parent.Workspace, "tmux"), []byte("#!/bin/sh\nprintf '%%44\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	args := func(i int) []string {
		return []string{"claude", "--name", fmt.Sprintf("cli%d", i), "--cwd", parent.Workspace}
	}
	for i := range maxSpawnTree {
		if _, err := spawnCLI(context.Background(), dir, args(i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := spawnCLI(context.Background(), dir, args(maxSpawnTree)); err == nil {
		t.Fatal("each CLI call got a new tree budget")
	}
	// The removed spellings get an error that names their replacement.
	for _, legacy := range [][]string{
		{"claude", "--name", "x", "-cwd", parent.Workspace},
		{"claude", "--name", "x", "-cwd=" + parent.Workspace},
		{"claude", "-name", "x", "--cwd", parent.Workspace},
	} {
		_, err := spawnCLI(context.Background(), dir, legacy)
		if err == nil || !strings.Contains(err.Error(), "no longer the AX") {
			t.Fatalf("%q: %v", legacy, err)
		}
	}
	if _, err := spawnCLI(context.Background(), dir, args(0)); err != nil {
		t.Fatalf("retry consumed another slot: %v", err)
	}
	parent.SpawnDepth = maxSpawnDepth
	file := filepath.Join(dir, "parent.json")
	if err := saveSession(file, parent); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_SESSION_FILE", file)
	if _, err := spawnCLI(context.Background(), dir, args(maxSpawnTree+1)); err == nil {
		t.Fatal("CLI ignored parent nesting limit")
	}
}

func TestSpawnRunnerPreservesArgumentsAndClearsImplicitBypass(t *testing.T) {
	dir, parent := spawnFixture(t)
	t.Setenv("AX_ALLOW_BYPASS", "1")
	t.Setenv("AX_SESSION_FILE", "parent-session")
	t.Setenv("AX_HOME", dir)
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("TMUX", "/tmp/child-socket,123,0")
	t.Setenv("TMUX_PANE", "%8")
	argsPath := filepath.Join(parent.Workspace, "observed-args")
	envPath := filepath.Join(parent.Workspace, "observed-env")
	script := "#!/bin/sh\nprintf '%s\\0' \"$@\" > " + shellQuote(argsPath) + "\nprintf '%s|%s|%s' \"$AX_ALLOW_BYPASS\" \"$CLAUDECODE\" \"$AX_SESSION_FILE\" > " + shellQuote(envPath) + "\n"
	if err := os.WriteFile(filepath.Join(parent.Workspace, "claude"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	native := []string{"--model", "literal ' $() `text`; value"}
	open := func(context.Context, terminalContext, string, string, string) (string, error) {
		return "", errors.New("uncertain terminal response")
	}
	if _, err := spawnAgent(context.Background(), dir, parent, spawnRequest{Host: "claude", Name: "worker", Args: native}, open); err != nil {
		t.Fatal(err)
	}
	var record spawnRecord
	if err := readPrivateJSON(spawnPath(dir, "worker"), &record); err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	if err := runSpawn(context.Background(), dir, "worker", record.Token); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(argsPath)
	actual := strings.Split(string(data), "\x00")
	if !reflect.DeepEqual(actual[:2], native) {
		t.Fatalf("changed native argument text: %q", actual[:2])
	}
	for i, arg := range actual {
		if arg == "--allowedTools" && strings.Contains(actual[i+1], "spawn_agent") {
			t.Fatal("spawn tool bypasses native approval")
		}
	}
	env, _ := os.ReadFile(envPath)
	if string(env) != "||"+filepath.Join(dir, "sessions", "worker.json") {
		t.Fatalf("inherited parent permissions or identity: %s", env)
	}
	s, _ := loadSession(filepath.Join(dir, "sessions", "worker.json"))
	if s.AllowBypass || s.SpawnToken != "" || s.Terminal.Pane != "%8" {
		t.Fatalf("wrong child binding: %+v", s)
	}
	if err := runSpawn(context.Background(), dir, "worker", record.Token); err == nil {
		t.Fatal("used the same reservation twice")
	}
	r, _ := spawnStatus(context.Background(), dir, "worker")
	if r.Phase != "exited" || r.Ready || r.Problem != "" {
		t.Fatalf("exit state not recorded: %+v", r)
	}
}

func TestSpawnToolRequiresExplicitUserRequest(t *testing.T) {
	if !strings.Contains(instructions, "only when the user explicitly asks") {
		t.Fatal("server instructions omit user-only launch policy")
	}
	for _, spec := range toolList() {
		if spec["name"] == "spawn_agent" {
			if !strings.Contains(spec["description"].(string), "only when the user explicitly asks") {
				t.Fatal("launch tool omits user-only policy")
			}
			props := spec["inputSchema"].(object)["properties"].(object)
			if props["args"].(object)["type"] != "array" {
				t.Fatal("native arguments are not structured")
			}
			return
		}
	}
	t.Fatal("missing spawn tool")
}

func TestDetectTerminalPrefersTmuxAndDoesNotFallback(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	t.Setenv("ITERM_SESSION_ID", "w1t1p1:43125C5A-7233-4248-9D01-3266FED7947E")
	t.Setenv("TMUX", "/tmp/a,b/socket,123,1")
	t.Setenv("TMUX_PANE", "%12")
	if got := detectTerminal(); got.Kind != "tmux" || got.Socket != "/tmp/a,b/socket" || got.Pane != "%12" {
		t.Fatalf("wrong terminal: %+v", got)
	}
	t.Setenv("TMUX_PANE", "")
	if got := detectTerminal(); got.Kind != "" {
		t.Fatalf("fell back from incomplete tmux context: %+v", got)
	}
}

func TestSpawnRunnerExplicitClaudeBypassOptIn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		allow bool
	}{
		{"permission_mode", []string{"--permission-mode", "bypassPermissions"}, true},
		{"permission_mode_equals", []string{"--permission-mode=bypassPermissions"}, true},
		{"skip_permissions", []string{"--dangerously-skip-permissions"}, true},
		{"ordinary", nil, false},
		{"default_mode", []string{"--permission-mode", "default"}, false},
		{"allow_option_only", []string{"--allow-dangerously-skip-permissions"}, false},
		{"after_separator", []string{"--", "--permission-mode", "bypassPermissions"}, false},
		{"grok_option", []string{"--yolo"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, parent := spawnFixture(t)
			t.Setenv("AX_ALLOW_BYPASS", "1") // The parent setting must still be stripped.
			t.Setenv("AX_SESSION_FILE", "parent-session")
			t.Setenv("CLAUDECODE", "1")
			argsPath := filepath.Join(parent.Workspace, "child-args")
			script := "#!/bin/sh\nprintf '%s\\0' \"$@\" > " + shellQuote(argsPath) + "\n"
			if err := os.WriteFile(filepath.Join(parent.Workspace, "claude"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			open := func(context.Context, terminalContext, string, string, string) (string, error) {
				return "", errors.New("test pane reservation")
			}
			if _, err := spawnAgent(context.Background(), dir, parent, spawnRequest{Host: "claude", Name: "worker", Args: tc.args}, open); err != nil {
				t.Fatal(err)
			}
			var record spawnRecord
			if err := readPrivateJSON(spawnPath(dir, "worker"), &record); err != nil {
				t.Fatal(err)
			}
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			defer os.Chdir(cwd)
			if err = runSpawn(context.Background(), dir, "worker", record.Token); err != nil {
				t.Fatal(err)
			}
			child, err := loadSession(filepath.Join(dir, "sessions", "worker.json"))
			if err != nil {
				t.Fatal(err)
			}
			if child.AllowBypass != tc.allow {
				t.Fatalf("AX opt-in=%t, want %t", child.AllowBypass, tc.allow)
			}
			observed, err := os.ReadFile(argsPath)
			if err != nil {
				t.Fatal(err)
			}
			actual := strings.Split(string(observed), "\x00")
			prefix := len(tc.args)
			if i := slices.Index(tc.args, "--"); i >= 0 {
				prefix = i
				tail := actual[len(actual)-1-(len(tc.args)-i) : len(actual)-1]
				if !reflect.DeepEqual(tail, tc.args[i:]) {
					t.Fatal("changed arguments after the native separator")
				}
			}
			if !reflect.DeepEqual(actual[:prefix], append([]string{}, tc.args[:prefix]...)) {
				t.Fatal("changed native permission arguments")
			}
			admin, err := dial(socketPath(dir))
			if err != nil {
				t.Fatal(err)
			}
			defer admin.close()
			var agents []Agent
			if err = admin.call("ax.inspect", object{}, &agents); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, agent := range agents {
				if agent.ID == child.ID {
					found = true
					if agent.AllowBypass != tc.allow {
						t.Fatal("broker did not receive child's explicit consent")
					}
				}
			}
			if !found {
				t.Fatal("child was not enrolled")
			}
		})
	}
}
