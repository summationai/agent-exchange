package ax

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAdapterSeparatesDeferralFromUnknownOutcome(t *testing.T) {
	dir := startTestServer(t)
	s := Session{Host: "opencode", Native: "ses_fixture", Started: true}
	file := filepath.Join(dir, "adapter-session.json")
	if err := saveSession(file, s); err != nil {
		t.Fatal(err)
	}
	for _, cause := range []error{&deliveryDeferred{State: "busy"}, errors.New("response lost"), nil} {
		path, stop, err := startAdapterHost(context.Background(), dir, file, nil, func(context.Context, string, string) error { return cause })
		if err != nil {
			t.Fatal(err)
		}
		s.AdapterSocket = path
		err = notifyAdapter(context.Background(), s, "fixed wake")
		stop()
		var deferred *deliveryDeferred
		if _, want := cause.(*deliveryDeferred); errors.As(err, &deferred) != want {
			t.Fatal("wrong certainty", err)
		}
		if cause == nil && err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenCodeDefersBeforeNativePrompt(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, node, "testdata/opencode_host.mjs", "opencode.js").CombinedOutput()
	if err != nil {
		t.Fatalf("native plugin fixture: %v\n%s", err, output)
	}
}

func TestNativeAdapterDiscoveryConnectsResumedSession(t *testing.T) {
	for _, host := range []string{"grok", "opencode"} {
		t.Run(host, func(t *testing.T) {
			dir := startTestServer(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := Session{ID: randomID("agt_"), Secret: randomID("") + randomID(""), Name: host, Host: host, Mesh: testMesh}
			file := filepath.Join(dir, "session.json")
			c, err := dial(socketPath(dir))
			if err != nil {
				t.Fatal(err)
			}
			defer c.close()
			if err = c.call("ax.enroll", s, nil); err != nil {
				t.Fatal(err)
			}
			if err = c.call("ax.connect", object{"version": "1", "agent_id": s.ID, "secret": s.Secret}, nil); err != nil {
				t.Fatal(err)
			}
			wakes := make(chan string, 2)
			socket, cancelHost, err := startAdapterHost(ctx, dir, file, make(chan error, 1), func(_ context.Context, native, text string) error {
				if !nativeID(host, native) || !strings.Contains(text, toolName(host, "list_agents")) {
					t.Error("incorrect native setup")
				}
				wakes <- native
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			defer cancelHost()
			s.AdapterSocket = socket
			if err = saveSession(file, s); err != nil {
				t.Fatal(err)
			}
			s.Native = uuid()
			if host == "opencode" {
				s.Native = "ses_1234abc"
			}
			if err = bindAdapter(dir, file, s.Native, "default", "ready"); err != nil {
				t.Fatal(err)
			}
			b := &bridge{session: s, file: file, dir: dir, c: c, ctx: ctx}
			b.bootstrap()
			if b.active {
				t.Fatal("activated before native tool discovery")
			}
			b.toolsListed = true
			b.bootstrap()
			b.bootstrap()
			if !b.active || len(wakes) != 1 {
				t.Fatalf("active=%v wakes=%d", b.active, len(wakes))
			}
			if err = bindAdapter(dir, file, uuid(), "default", "ready"); err == nil {
				t.Fatal("AX name silently rebound")
			}
		})
	}
}

func TestOpenCodeKeepsNativeArgumentsAndConfiguration(t *testing.T) {
	dir := startTestServer(t)
	s := Session{Host: "opencode", Native: "ses_original"}
	file := filepath.Join(dir, "session.json")
	if err := saveSession(file, s); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE_CONFIG_CONTENT", `{"model":"example/model","mcp":{"existing":{"type":"local","command":["other"]}}}`)
	t.Setenv("OPENCODE_TUI_CONFIG", "")
	args := []string{"--session", "ses_original", "--model", "example/model", "--prompt", "literal @file /command"}
	actual, env, stop, err := prepareOpenCode(context.Background(), dir, file, "opencode", s, args, nil, make(chan error, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if !reflect.DeepEqual(args, actual) {
		t.Fatalf("native arguments changed: %q", actual)
	}
	for _, entry := range env {
		if !strings.HasPrefix(entry, "OPENCODE_CONFIG_CONTENT=") {
			continue
		}
		var config struct {
			Model string
			MCP   map[string]any
		}
		if err = json.Unmarshal([]byte(strings.TrimPrefix(entry, "OPENCODE_CONFIG_CONTENT=")), &config); err != nil {
			t.Fatal(err)
		}
		if config.Model != "example/model" || config.MCP["existing"] == nil || config.MCP["ax"] == nil {
			t.Fatal("native configuration lost")
		}
	}
	_, _, _, err = prepareOpenCode(context.Background(), dir, file, "opencode", s, []string{"--pure"}, nil, make(chan error, 1))
	if err == nil {
		t.Fatal("silently launched without required plugin")
	}
	if _, err = os.Stat(filepath.Join(dir, "config.json")); !os.IsNotExist(err) {
		t.Fatal("wrote global configuration")
	}
}

func TestGrokProtocolBindsAndConfirmsNativeQueue(t *testing.T) {
	tui, client := net.Pipe()
	upstream, server := net.Pipe()
	defer tui.Close()
	defer client.Close()
	defer upstream.Close()
	defer server.Close()
	for _, conn := range []net.Conn{tui, client, upstream, server} {
		conn.SetDeadline(time.Now().Add(2 * time.Second))
	}
	g := &grokConnection{conn: upstream, permission: "default", prompts: map[string]chan error{}}
	bound := make(chan string, 1)
	go proxyGrok(client, g, object{"name": "ax", "command": "ax", "args": []string{"bridge"}}, func(id, permission string) error { bound <- id; return nil })
	request := object{"jsonrpc": "2.0", "id": 7, "method": "session/new", "params": object{"cwd": "/workspace", "mcpServers": []object{{"name": "other"}}, "_meta": object{"modelId": "user-model"}}}
	if err := grokWrite(tui, object{"type": "acp", "payload": string(raw(request))}); err != nil {
		t.Fatal(err)
	}
	frame, err := grokRead(server)
	if err != nil {
		t.Fatal(err)
	}
	var forwarded struct {
		Params struct {
			Cwd     string
			Servers []object `json:"mcpServers"`
			Meta    object   `json:"_meta"`
		}
	}
	json.Unmarshal([]byte(frame["payload"].(string)), &forwarded)
	if forwarded.Params.Cwd != "/workspace" || len(forwarded.Params.Servers) != 2 || forwarded.Params.Meta["modelId"] != "user-model" {
		t.Fatal("lost native session parameters")
	}
	id := uuid()
	go grokWrite(server, object{"type": "acp", "payload": string(raw(object{"jsonrpc": "2.0", "id": 7, "result": object{"sessionId": id}}))})
	if _, err = grokRead(tui); err != nil {
		t.Fatal(err)
	}
	if <-bound != id {
		t.Fatal("bound wrong native session")
	}
	wakeDone := make(chan error, 1)
	go func() { wakeDone <- g.wake(context.Background(), id, "fixed AX wake") }()
	frame, err = grokRead(server)
	if err != nil {
		t.Fatal(err)
	}
	var wake struct {
		Params struct {
			Meta struct {
				ID string `json:"promptId"`
			} `json:"_meta"`
		}
	}
	json.Unmarshal([]byte(frame["payload"].(string)), &wake)
	select {
	case <-wakeDone:
		t.Fatal("socket write falsely counted as native acceptance")
	default:
	}
	go grokWrite(server, object{"type": "acp", "payload": string(raw(object{"jsonrpc": "2.0", "method": "_x.ai/queue/changed", "params": object{"sessionId": id, "runningPromptId": wake.Params.Meta.ID}}))})
	if _, err = grokRead(tui); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-wakeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("native queue acceptance was missed")
	}
}

func TestGrokProxyPreservesClientExitCause(t *testing.T) {
	for _, name := range []string{"quit", "malformed"} {
		t.Run(name, func(t *testing.T) {
			malformed := name == "malformed"
			tui, client := net.Pipe()
			upstream, server := net.Pipe()
			defer tui.Close()
			defer server.Close()
			defer upstream.Close()
			done := make(chan error, 1)
			go func() { done <- proxyGrok(client, &grokConnection{conn: upstream}, nil, nil) }()
			if malformed {
				if err := grokWrite(tui, object{"type": "acp", "payload": "{"}); err != nil {
					t.Fatal(err)
				}
			} else {
				tui.Close()
			}
			select {
			case err := <-done:
				if malformed {
					if err == nil || err.Error() != "invalid Grok ACP payload" {
						t.Fatalf("lost protocol error: %v", err)
					}
				} else if !errors.Is(err, io.EOF) {
					t.Fatalf("normal exit became a transport failure: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("proxy did not stop after client exit")
			}
		})
	}
}

func TestOpenCodeTUIKeepsNestedAndRelativePlugins(t *testing.T) {
	for _, tc := range []struct{ input, expected string }{
		{`{"tui":{"theme":"custom","plugin":["existing-package"]}}`, `{"theme":"custom","plugin":["existing-package","file:///ax/plugin.js"]}`},
		{`{"tui":{"theme":"custom","plugin":["nested"]},"plugin":["top"]}`, `{"theme":"custom","plugin":["top","file:///ax/plugin.js"]}`},
		{`{"plugin":["./local.js",["../other.js",{"option":true}],"file:///absolute.js","package@1"]}`, `{"plugin":["file:///user/config/local.js",["file:///user/other.js",{"option":true}],"file:///absolute.js","package@1","file:///ax/plugin.js"]}`},
	} {
		var input, expected object
		json.Unmarshal([]byte(tc.input), &input)
		json.Unmarshal([]byte(tc.expected), &expected)
		if actual := openCodeTUI(input, "/user/config", "/ax/plugin.js"); !reflect.DeepEqual(actual, expected) {
			t.Fatalf("configuration changed: %s", raw(actual))
		}
	}
}

func TestGrokPermissionGrantIsLimitedToAXTools(t *testing.T) {
	for _, name := range []string{"ax__list_agents", "ax__send_message", "ax__reply", "other__reply", "run_terminal_command", "ax__unknown", "ax__spawn_agent"} {
		rpc := object{"method": "session/request_permission", "params": object{"options": []any{object{"kind": "allow_once", "optionId": "once"}, object{"kind": "allow_always", "_meta": object{"server_prefix": "ax", "tool_name": name}}}}}
		// Exercise decoded JSON, as received from the native protocol.
		json.Unmarshal(raw(rpc), &rpc)
		want := ""
		if name == "ax__list_agents" || name == "ax__send_message" || name == "ax__reply" {
			want = "once"
		}
		if got := grokAXPermission(rpc); got != want {
			t.Fatalf("%s: %s", name, got)
		}
		rpc["params"].(map[string]any)["_meta"] = object{"hookAsk": object{"reason": "required"}}
		if grokAXPermission(rpc) != "" {
			t.Fatal("overrode a hook requiring confirmation")
		}
	}
}
