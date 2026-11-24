package ax

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAgyPreparePreservesNativeSyntaxAndKeepsIdentityPrivate(t *testing.T) {
	home, dir := testDir(t), testDir(t)
	t.Setenv("HOME", home)
	file := filepath.Join(dir, "session.json")
	s := Session{AllowBypass: true, Host: "agy", Native: uuid(), Secret: "PRIVATE_SECRET", LaunchID: "launch_test"}
	for _, native := range [][]string{nil, {"--resume", "chosen"}, {"--continue", "--model", "native-model"}, {"--print", "--remote-agent"}, {"-p", "-h"}, {"-print", "--help"}, {"--remote-agent", "--remote-agent=false"}, {"--", "--remote-agent"}} {
		args, env, cleanup, err := prepareAgy(context.Background(), dir, file, "agy", s, native, []string{"EXISTING=value"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		cleanup()
		want := native
		if len(want) == 0 {
			want = []string{"--resume", s.Native}
		}
		if !reflect.DeepEqual(args, want) || !reflect.DeepEqual(env, []string{"EXISTING=value", "AX_AGY_PID=" + strconv.Itoa(os.Getpid())}) {
			t.Fatalf("changed native launch: %q, %q", args, env)
		}
	}
	root := filepath.Join(home, ".gemini", "config", "plugins", "ax")
	for _, name := range []string{"plugin.json", "mcp_config.json", "hooks.json"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(s.Secret)) || bytes.Contains(data, []byte(file)) {
			t.Fatalf("session metadata escaped into global plugin: %s", name)
		}
		info, _ := os.Stat(filepath.Join(root, name))
		if info.Mode().Perm() != 0600 {
			t.Fatal("plugin configuration is not private")
		}
	}
	other := testDir(t)
	t.Setenv("HOME", other)
	_, _, _, err := prepareAgy(context.Background(), dir, file, "agy", s, []string{"--help"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(other, ".gemini")); !os.IsNotExist(err) {
		t.Fatal("help installed a plugin")
	}
	for _, native := range [][]string{{"--remote-agent"}, {"--remote-agent=true"}, {"acp"}} {
		if _, _, _, err := prepareAgy(context.Background(), dir, file, "agy", s, native, nil, nil); err == nil {
			t.Fatal("accepted remote execution without local hooks")
		}
	}
}

func TestAgyPluginPreservesUnrelatedPlugin(t *testing.T) {
	home := testDir(t)
	dir := filepath.Join(home, ".gemini", "config", "plugins", "ax")
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "plugin.json")
	if err := os.WriteFile(path, []byte(`{"name":"ax","description":"unrelated"}`), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err := installAgyPlugin(home, "/ax"); err == nil {
		t.Fatal("overwrote an unrelated plugin")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("unrelated plugin changed")
	}
}

func TestAgyPluginWaitsForConcurrentInstallation(t *testing.T) {
	home := testDir(t)
	dir := filepath.Join(home, ".gemini", "config", "plugins", "ax")
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	lock, err := lockSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	done := make(chan error, 1)
	go func() { done <- installAgyPlugin(home, "/ax") }()
	select {
	case err := <-done:
		t.Fatal("installation did not wait for shared plugin lock", err)
	case <-time.After(50 * time.Millisecond):
	}
	lock.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("installation did not finish after shared lock released")
	}
}

func TestAgyInactivePluginNeverExposesMessagingTools(t *testing.T) {
	var input, output bytes.Buffer
	enc := json.NewEncoder(&input)
	for i, method := range []string{"initialize", "notifications/initialized", "tools/list", "tools/call", "ping"} {
		p := packet{JSONRPC: "2.0", Method: method}
		if i != 1 {
			p.ID = raw(i + 1)
		}
		if err := enc.Encode(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := inactiveAgyBridge(&input, &output); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(&output)
	for i := 0; i < 4; i++ {
		var p packet
		if err := dec.Decode(&p); err != nil {
			t.Fatal(err)
		}
		if i == 1 && string(p.Result) != `{"tools":[]}` {
			t.Fatal("inactive plugin exposed tools", string(p.Result))
		}
		if i == 2 && (p.Error == nil || p.Error.Code != -32601) {
			t.Fatal("inactive plugin accepted a tool call")
		}
	}
	file := filepath.Join(testDir(t), "session.json")
	s := Session{Host: "codex", LaunchID: "launch_parent"}
	if err := saveSession(file, s); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_LAUNCH_ID", s.LaunchID)
	t.Setenv("AX_AGY_PID", strconv.Itoa(os.Getppid()))
	if _, active := activeAgySession(file); active {
		t.Fatal("inherited another harness identity")
	}
	s.Host = "agy"
	if err := saveSession(file, s); err != nil {
		t.Fatal(err)
	}
	if _, active := activeAgySession(file); !active {
		t.Fatal("matching native parent was inactive")
	}
	t.Setenv("AX_AGY_PID", strconv.Itoa(os.Getpid()))
	if _, active := activeAgySession(file); active {
		t.Fatal("nested plain process inherited parent identity")
	}
	var hookOut bytes.Buffer
	if err := agyHook(filepath.Dir(file), file, "SessionStart", bytes.NewBufferString(`{"conversationId":"invalid-child"}`), &hookOut); err != nil {
		t.Fatal("nested hook was not inert", err)
	}
	saved, err := loadSession(file)
	if err != nil || !reflect.DeepEqual(saved, s) {
		t.Fatal("nested hook changed parent session", saved, err)
	}
	t.Setenv("AX_AGY_PID", strconv.Itoa(os.Getppid()))
	t.Setenv("AX_LAUNCH_ID", "launch_stale")
	if _, active := activeAgySession(file); active {
		t.Fatal("accepted an earlier launch")
	}
}

func TestAgyHooksBindWithoutServingContentAndUseSharedInbox(t *testing.T) {
	t.Setenv("AX_AGY_PID", strconv.Itoa(os.Getppid()))
	t.Setenv("HOME", testDir(t))
	dir := startTestServer(t)
	s := Session{ID: randomID("agt_"), Secret: randomID(""), Name: "agy-test", Host: "agy", AllowBypass: true, Mesh: testMesh, LaunchID: randomID("launch_"), DeliveryMode: "manual"}
	file := filepath.Join(dir, "session.json")
	if err := saveSession(file, s); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_LAUNCH_ID", s.LaunchID)
	owner, err := dial(socketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.close()
	if err = owner.call("ax.enroll", s, nil); err != nil {
		t.Fatal(err)
	}
	native := uuid()
	hook := func(event, reason string, idle bool) object {
		t.Helper()
		var out bytes.Buffer
		err := agyHook(dir, file, event, bytes.NewReader(raw(object{"conversationId": native, "terminationReason": reason, "fullyIdle": idle})), &out)
		if err != nil {
			t.Fatal(err)
		}
		var result object
		if err = json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	if hook("SessionStart", "", false)["injectSteps"] == nil {
		t.Fatal("startup guidance missing")
	}
	saved, _ := loadSession(file)
	if saved.Native != native || !saved.Started {
		t.Fatal("native-selected conversation was not bound")
	}
	ctx, cancel := context.WithCancel(context.Background())
	host, child := net.Pipe()
	host.SetDeadline(time.Now().Add(10 * time.Second))
	done := make(chan error, 1)
	go func() { defer child.Close(); done <- runBridge(ctx, dir, file, child, child, nil) }()
	t.Cleanup(func() {
		host.Close()
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("MCP bridge did not exit")
		}
	})
	mcp := &attachedMCP{conn: host, read: json.NewDecoder(host)}
	mcp.rpc(t, "initialize", object{"protocolVersion": "2025-11-25"})
	mcp.rpc(t, "tools/list", object{})
	var agents struct {
		Agents []Agent `json:"agents"`
	}
	if err := mcp.call(t, "list_agents", object{}, &agents); err != "" {
		t.Fatal(err)
	}
	if len(agents.Agents) != 1 || agents.Agents[0].State != "busy" {
		t.Fatal("incorrect Antigravity readiness", agents)
	}
	var roster []discoveredAgent
	if err = owner.call("ax.inspect", object{}, &roster); err != nil || len(roster) != 1 || !roster[0].Connectivity.Tools || roster[0].Connectivity.Wake != "user_turn" {
		t.Fatal("incorrect native delivery contract", roster, err)
	}
	sender := connectDiscoveryPeer(t, dir, Session{ID: randomID("agt_"), Secret: randomID(""), Name: "sender", Host: "codex", Mesh: testMesh, Native: uuid()})
	var sent Message
	if err = sender.call("ax.send", object{"target": s.Name, "text": "PRIVATE_PEER_BODY", "client_message_id": "agy-hook-test"}, &sent); err != nil {
		t.Fatal(err)
	}
	if len(hook("Stop", "error", true)) != 0 {
		t.Fatal("continued after a native error")
	}
	first := hook("Stop", "model_stop", true)
	if len(first) != 0 {
		t.Fatal("completion hook forced work or exposed mail", first)
	}
	if len(hook("Stop", "model_stop", true)) != 0 {
		t.Fatal("completion hook forced a continuation")
	}
	var status struct {
		Message Message `json:"message"`
	}
	if err = owner.call("ax.inspect_message", object{"message_id": sent.ID}, &status); err != nil {
		t.Fatal(err)
	}
	if status.Message.State != "queued" {
		t.Fatal("a hook claimed content delivery", status.Message.State)
	}
	var inbox struct {
		Message *Message `json:"message"`
	}
	if _, err := suspendAgyBinding(file, s.LaunchID); err != nil {
		t.Fatal(err)
	}
	if err := mcp.call(t, "check_inbox", object{}, &inbox); err == "" || inbox.Message != nil {
		t.Fatal("skipped hook left the previous content binding usable")
	}
	hook("PreInvocation", "", false)
	lock, err := lockSession(file)
	if err != nil {
		t.Fatal(err)
	}
	var blockedOutput bytes.Buffer
	err = agyHook(dir, file, "PreInvocation", bytes.NewReader(raw(object{"conversationId": uuid()})), &blockedOutput)
	lock.Close()
	if err == nil || !strings.Contains(err.Error(), "AX session state is busy") {
		t.Fatal("expected the hook to time out on the session lock", err)
	}
	current, err := loadSession(file)
	if err != nil || !current.Started || current.Native != native {
		t.Fatal("test did not leave the previous saved binding in place", err)
	}
	if err := mcp.call(t, "check_inbox", object{}, &inbox); err == "" || inbox.Message != nil {
		t.Fatal("lock timeout left the previous mailbox readable")
	}
	if err = owner.call("ax.inspect_message", object{"message_id": sent.ID}, &status); err != nil || status.Message.State != "queued" {
		t.Fatal("failed hook consumed queued mail", status.Message.State, err)
	}
	hook("PreInvocation", "", false)
	for _, input := range []string{"{", "{}", `{"conversationId":false}`} {
		var output bytes.Buffer
		if err := agyHook(dir, file, "PreInvocation", bytes.NewBufferString(input), &output); err == nil {
			t.Fatal("accepted malformed native identity", input)
		}
		if err := mcp.call(t, "check_inbox", object{}, &inbox); err == "" || inbox.Message != nil {
			t.Fatal("failed hook left the previous mailbox readable")
		}
		if len(hook("Stop", "error", true)) != 0 {
			t.Fatal("Stop injected work after a failed native hook")
		}
		hook("PreInvocation", "", false)
	}
	if err := mcp.call(t, "check_inbox", object{}, &inbox); err != "" {
		t.Fatal(err)
	}
	if inbox.Message == nil || inbox.Message.Text != "PRIVATE_PEER_BODY" {
		t.Fatal("shared inbox did not deliver original content")
	}
	if err := mcp.call(t, "reply", object{"message_id": sent.ID, "text": "AX_AGY_REPLY_OK"}, nil); err != "" {
		t.Fatal(err)
	}
	select {
	case offer := <-sender.offers:
		var response Message
		if err = sender.call("ax.get_message", object{"message_id": offer.ID}, &response); err != nil || response.Text != "AX_AGY_REPLY_OK" {
			t.Fatal("cross-harness reply failed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cross-harness reply was not offered")
	}
	if err = sender.call("ax.send", object{"target": s.Name, "text": "HELD_BODY", "client_message_id": "agy-consent-hold"}, nil); err != nil {
		t.Fatal(err)
	}
	current, err = loadSession(file)
	if err != nil {
		t.Fatal(err)
	}
	current.AllowBypass = false
	if err = saveSession(file, current); err != nil {
		t.Fatal(err)
	}
	if err := mcp.call(t, "check_inbox", object{}, nil); err == "" {
		t.Fatal("mail delivered after consent was revoked")
	}
	current.AllowBypass = true
	if err = saveSession(file, current); err != nil {
		t.Fatal(err)
	}
	if err := mcp.call(t, "check_inbox", object{}, &inbox); err != "" || inbox.Message == nil || inbox.Message.Text != "HELD_BODY" {
		t.Fatal("held mail did not recover", err, inbox)
	}

	var out bytes.Buffer
	if err = agyHook(dir, file, "PreInvocation", bytes.NewReader(raw(object{"conversationId": uuid()})), &out); err == nil {
		t.Fatal("rebound to another native conversation")
	}
	saved, _ = loadSession(file)
	if saved.BindingError == "" || saved.Native != native {
		t.Fatal("conversation switch left previous identity usable")
	}
	if err := mcp.call(t, "list_agents", object{}, nil); err == "" {
		t.Fatal("MCP tools remained usable after binding conflict")
	}
}

func TestAgyInactiveHookAvoidsResourceAccounting(t *testing.T) {
	if os.Getenv("AX_TEST_AGY_INACTIVE_HOOK") == "1" {
		if err := Main([]string{"agy-hook", "PreInvocation"}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	dir := testDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAgyInactiveHookAvoidsResourceAccounting$")
	cmd.Env = append(os.Environ(), "AX_TEST_AGY_INACTIVE_HOOK=1", "AX_HOME="+dir, "HOME="+testDir(t))
	data, err := cmd.CombinedOutput()
	if err != nil || string(bytes.TrimSpace(data)) != "{}" {
		t.Fatalf("inactive hook: %q, %v", data, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("ordinary Antigravity wrote AX runtime or resource state", entries, err)
	}
}

func TestAgyHookShellKeepsOnlyNativeParentActive(t *testing.T) {
	if os.Getenv("AX_TEST_AGY_PARENT_PROBE") == "1" {
		os.Setenv("AX_SESSION_FILE", os.Getenv("AX_TEST_AGY_PARENT_FILE"))
		os.Setenv("AX_LAUNCH_ID", "launch_native")
		_, active := activeAgySession(os.Getenv("AX_SESSION_FILE"))
		fmt.Print(active)
		os.Exit(0)
	}
	home, dir := testDir(t), testDir(t)
	file := filepath.Join(dir, "session.json")
	s := Session{Host: "agy", LaunchID: "launch_native"}
	if err := saveSession(file, s); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(dir, "ax")
	script := "#!/bin/sh\nexec " + shellQuote(os.Args[0]) + " -test.run=^TestAgyHookShellKeepsOnlyNativeParentActive$\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := installAgyPlugin(home, wrapper); err != nil {
		t.Fatal(err)
	}
	var hooks map[string]map[string][]struct{ Command string }
	data, err := os.ReadFile(filepath.Join(home, ".gemini/config/plugins/ax/hooks.json"))
	if err != nil || json.Unmarshal(data, &hooks) != nil {
		t.Fatal("invalid generated hooks", err)
	}
	command := hooks["ax-messaging"]["SessionStart"][0].Command
	for _, nested := range []bool{false, true} {
		invoke := command
		if nested {
			// A child native process inherits AX env but has a different PID.
			invoke = "sh -c " + shellQuote(command) + "; :"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, "sh", "-c", invoke)
		cmd.Env = append(os.Environ(), "AX_TEST_AGY_PARENT_PROBE=1", "AX_TEST_AGY_PARENT_FILE="+file, "AX_AGY_PID="+strconv.Itoa(os.Getpid()))
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil || string(out) != strconv.FormatBool(!nested) {
			t.Fatalf("native parent check (nested=%v): %q, %v", nested, out, err)
		}
	}
}

func TestAgyLauncherUsesNativeExecAndManualDelivery(t *testing.T) {
	if dir := os.Getenv("AX_TEST_AGY_LAUNCH"); dir != "" {
		os.Setenv("AX_ALLOW_BYPASS", "1")
		if err := Launch(context.Background(), dir, "agy", []string{"-n", "helper", "--model", "native-model"}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	dir, home, bin := startTestServer(t), testDir(t), testDir(t)
	out := filepath.Join(bin, "args")
	script := "#!/bin/sh\nprintf '%s\\0' \"$@\" > " + shellQuote(out) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "agy"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAgyLauncherUsesNativeExecAndManualDelivery$")
	cmd.Env = append(os.Environ(), "AX_TEST_AGY_LAUNCH="+dir, "HOME="+home, "PATH="+bin+":"+os.Getenv("PATH"))
	if result, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native launcher fixture: %v: %s", err, result)
	}
	data, err := os.ReadFile(out)
	if err != nil || string(data) != "--model\x00native-model\x00" {
		t.Fatalf("native args changed: %q, %v", data, err)
	}
	s, err := loadSession(filepath.Join(dir, "sessions", "helper.json"))
	if err != nil || s.Host != "agy" || s.DeliveryMode != "manual" {
		t.Fatal("launcher did not persist native hook contract", s, err)
	}
}

func TestAgyNativePluginValidation(t *testing.T) {
	bin, err := exec.LookPath("agy")
	if err != nil {
		t.Skip("Antigravity CLI is not installed")
	}
	home := testDir(t)
	if err = installAgyPlugin(home, "/private/path with spaces/ax"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "plugin", "validate", filepath.Join(home, ".gemini/config/plugins/ax"), "--json")
	cmd.Env = append(os.Environ(), "HOME="+home)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("native plugin validation: %v: %s", err, output)
	}
}

func TestAgyResumeBindsOnFirstInvocation(t *testing.T) {
	t.Setenv("AX_AGY_PID", strconv.Itoa(os.Getppid()))
	t.Setenv("HOME", testDir(t))
	dir := startTestServer(t)
	s := Session{ID: randomID("agt_"), Secret: randomID(""), Name: "resumed", Host: "agy", AllowBypass: true, Mesh: testMesh, LaunchID: randomID("launch_"), Native: uuid(), DeliveryMode: "manual"}
	file := filepath.Join(dir, "session.json")
	if err := saveSession(file, s); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_LAUNCH_ID", s.LaunchID)
	c, err := dial(socketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	if err = c.call("ax.enroll", s, nil); err != nil {
		t.Fatal(err)
	}
	if err = c.call("ax.connect", object{"version": "1", "agent_id": s.ID, "secret": s.Secret, "launch_id": s.LaunchID}, nil); err != nil {
		t.Fatal(err)
	}
	if err = c.call("ax.ready", object{}, nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = agyHook(dir, file, "PreInvocation", bytes.NewReader(raw(object{"conversationId": s.Native})), &out); err != nil {
		t.Fatal(err)
	}
	saved, err := loadSession(file)
	if err != nil || !saved.Started || saved.Native != s.Native {
		t.Fatal("resume never bound without a new SessionStart", saved, err)
	}
	var agents []discoveredAgent
	if err = c.call("ax.list", object{}, &agents); err != nil || len(agents) != 1 || agents[0].State != "busy" {
		t.Fatal("first invocation did not report busy", agents, err)
	}
	// A later launch resets Started but must retain the saved conversation owner.
	for _, event := range []string{"SessionStart", "PreInvocation"} {
		if err = saveSession(file, s); err != nil {
			t.Fatal(err)
		}
		if err = agyHook(dir, file, event, bytes.NewReader(raw(object{"conversationId": uuid()})), &out); err == nil {
			t.Fatal("relaunch transferred mailbox to another conversation", event)
		}
		saved, err = loadSession(file)
		if err != nil || saved.Native != s.Native || saved.BindingError == "" || saved.Started {
			t.Fatal("relaunch conflict did not preserve binding", saved, err)
		}
	}
}

func TestAgyExplicitConsent(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"--dangerously-skip-permissions"}, true},
		{[]string{"--dangerously-skip-permissions=true"}, true},
		{[]string{"--dangerously-skip-permissions=false"}, false},
		{[]string{"--", "--dangerously-skip-permissions"}, false},
		{[]string{"--print", "--dangerously-skip-permissions"}, false},
		{[]string{"-print", "--dangerously-skip-permissions"}, false},
		{[]string{"-dangerously-skip-permissions"}, true},
		{[]string{"dangerously-skip-permissions"}, false},
		{[]string{"---dangerously-skip-permissions"}, false},
		{[]string{"-p", "-h"}, false},
		{[]string{"--settings", `{"toolPermission":"always-proceed"}`}, false},
		{[]string{"--settings", "/unchanged/native/settings.json"}, false},
	} {
		if got := agyExplicitBypass(tc.args); got != tc.want {
			t.Fatalf("bypass %q: %v", tc.args, got)
		}
	}
	p := &peer{Agent: Agent{Host: "agy", Permission: "unverified"}}
	if safe(p) {
		t.Fatal("unverified native permissions bypassed AX consent")
	}
	p.AllowBypass = true
	if !safe(p) {
		t.Fatal("saved consent not honored")
	}
	home, dir := testDir(t), testDir(t)
	t.Setenv("HOME", home)
	_, _, _, err := prepareAgy(context.Background(), dir, filepath.Join(dir, "s.json"), "agy", Session{Host: "agy"}, nil, nil, nil)
	if err == nil {
		t.Fatal("launched Antigravity without delivery consent")
	}
	if _, err = os.Stat(filepath.Join(home, ".gemini")); !os.IsNotExist(err) {
		t.Fatal("consent failure installed a plugin")
	}
}
