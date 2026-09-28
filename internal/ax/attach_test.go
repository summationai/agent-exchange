package ax

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

type attachedMCP struct {
	conn net.Conn
	read *json.Decoder
	next int
}

func startAttachedMCP(t *testing.T, dir string, args ...string) *attachedMCP {
	t.Helper()
	host, child := net.Pipe()
	host.SetDeadline(time.Now().Add(20 * time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { defer child.Close(); done <- attach(ctx, dir, args, child, child, nil) }()
	t.Cleanup(func() {
		host.Close()
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("attached child did not exit")
		}
	})
	m := &attachedMCP{conn: host, read: json.NewDecoder(host)}
	m.rpc(t, "initialize", object{"protocolVersion": "2025-11-25"})
	m.rpc(t, "tools/list", object{})
	return m
}

func (m *attachedMCP) rpc(t *testing.T, method string, args any) json.RawMessage {
	t.Helper()
	m.next++
	if err := json.NewEncoder(m.conn).Encode(packet{JSONRPC: "2.0", ID: raw(m.next), Method: method, Params: raw(args)}); err != nil {
		t.Fatal(err)
	}
	var p packet
	if err := m.read.Decode(&p); err != nil {
		t.Fatal(err)
	}
	if string(p.ID) != string(raw(m.next)) || p.Error != nil {
		t.Fatalf("bad RPC response: %+v", p)
	}
	return p.Result
}

func (m *attachedMCP) call(t *testing.T, name string, args object, result any) string {
	t.Helper()
	data := m.rpc(t, "tools/call", object{"name": name, "arguments": args})
	var response struct {
		IsError bool `json:"isError"`
		Content []struct{ Text string }
	}
	if err := json.Unmarshal(data, &response); err != nil || len(response.Content) != 1 {
		t.Fatalf("bad tool response: %s", data)
	}
	if response.IsError {
		return response.Content[0].Text
	}
	if result != nil {
		if err := json.Unmarshal([]byte(response.Content[0].Text), result); err != nil {
			t.Fatal(err)
		}
	}
	return ""
}

func TestAttachedManualRuntimeAndNativePeerRoundTrip(t *testing.T) {
	dir := startTestServer(t)
	m := startAttachedMCP(t, dir, "-n", "planner", "-s", "conversation_1", "-p", "default")
	var discovered struct {
		Agents []Agent `json:"agents"`
	}
	if err := m.call(t, "list_agents", object{}, &discovered); err != "" {
		t.Fatal(err)
	}
	if len(discovered.Agents) != 1 || discovered.Agents[0].State != "user-turn" || !discovered.Agents[0].Connectivity.Tools || discovered.Agents[0].Connectivity.Wake != "user_turn" {
		t.Fatalf("false readiness: %+v", discovered)
	}
	if discovered.Agents[0].DeliveryMode != "manual" || discovered.Agents[0].Capabilities != nil {
		t.Fatalf("attachment claimed a native delivery contract: %+v", discovered)
	}
	s := Session{ID: randomID("agt_"), Secret: randomID(""), Name: "native", Host: "codex", Mesh: testMesh, Native: uuid()}
	native := connectDiscoveryPeer(t, dir, s)
	var sent Message
	if err := native.call("ax.send", object{"target": "planner", "text": "please review", "client_message_id": "first"}, &sent); err != nil {
		t.Fatal(err)
	}
	var pending pendingPage
	if err := m.call(t, "list_pending", object{}, &pending); err != "" {
		t.Fatal(err)
	}
	if len(pending.Messages) != 1 || pending.Messages[0].State != "queued" || pending.Messages[0].Preview != "" {
		t.Fatalf("listing offered manual mail: %+v", pending)
	}
	var fetched struct {
		Message *struct {
			Message
			Guidance string `json:"guidance"`
		} `json:"message"`
	}
	if err := m.call(t, "check_inbox", object{}, &fetched); err != "" {
		t.Fatal(err)
	}
	if fetched.Message == nil || fetched.Message.ID != sent.ID || fetched.Message.State != "content_served" {
		t.Fatalf("missing manual receive: %+v", fetched)
	}
	if fetched.Message.Text != "please review" || fetched.Message.Guidance != peerGuidance || fetched.Message.Thread != sent.Thread {
		t.Fatalf("manual content lost body, thread, or guidance: %+v", fetched.Message)
	}
	var replied object
	if err := m.call(t, "reply", object{"message_id": sent.ID, "text": "reviewed"}, &replied); err != "" {
		t.Fatal(err)
	}
	if !strings.Contains(replied["next_action"].(string), "no automatic wake") || replied["submission"] != "submitted" || strings.Contains(string(raw(replied)), "reviewed") {
		t.Fatalf("misleading manual send receipt: %+v", replied)
	}
	select {
	case reply := <-native.offers:
		if reply.Parent != sent.ID || reply.Text != "reviewed" {
			t.Fatalf("incorrect native reply: %+v", reply)
		}
	case <-time.After(time.Second):
		t.Fatal("reply did not reach native adapter")
	}
	if err := m.call(t, "spawn_agent", object{"harness": "codex", "name": "unasked"}, nil); !strings.Contains(err, "cannot launch") {
		t.Fatalf("attached runtime could launch: %s", err)
	}
	// The native host, not a model tool argument, owns permission changes.
	if err := json.NewEncoder(m.conn).Encode(packet{JSONRPC: "2.0", Method: "notifications/ax/presence", Params: raw(object{"native_session_id": "conversation_1", "permission_mode": "unknown", "state": "ready"})}); err != nil {
		t.Fatal(err)
	}
	if err := m.call(t, "check_inbox", object{"permission_mode": "default", "delivery_mode": "adapter"}, nil); !strings.Contains(err, "blocked") {
		t.Fatalf("model overrode host permission: %s", err)
	}
}

func TestAttachmentLocksIdentityAndResumes(t *testing.T) {
	dir := startTestServer(t)
	s, _, release, err := attachedSession(dir, "planner", "original", "manual", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := attachedSession(dir, "planner", "original", "manual", ""); err == nil {
		t.Fatal("duplicate attachment acquired name")
	}
	release()
	if _, _, _, err := attachedSession(dir, "planner", "different", "manual", ""); err == nil {
		t.Fatal("changed native conversation")
	}
	resumed, _, stop, err := attachedSession(dir, "planner", "original", "manual", "")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if resumed.ID != s.ID || resumed.Secret != s.Secret {
		t.Fatal("resume replaced durable identity")
	}
}

func TestManualReceivePreservesPolicyAndUncertainBarrier(t *testing.T) {
	b, _ := localBroker(t)
	_, sender, _ := endpoint(t, b, "sender", testMesh)
	s, recipient, _ := endpoint(t, b, "receiver", testMesh)
	p := b.peers[s.ID]
	p.DeliveryMode = "manual"
	first := request(t, b, sender, "ax.send", object{"target": s.Name, "text": "first", "client_message_id": "one"}).(Message)
	second := request(t, b, sender, "ax.send", object{"target": s.Name, "text": "second", "client_message_id": "two"}).(Message)
	b.dispatch()
	if got, _ := b.message(first.ID); got.State != "queued" {
		t.Fatal("manual endpoint was woken")
	}
	p.Policy = "hold"
	if _, err := b.request(recipient, "ax.check_inbox", raw(object{})); err == nil {
		t.Fatal("ignored hold policy")
	}
	p.Policy = "accept"
	b.peers[sender.agent].Permission = "unknown"
	if _, err := b.request(recipient, "ax.check_inbox", raw(object{})); err == nil {
		t.Fatal("ignored sender permission")
	}
	b.peers[sender.agent].Permission = "default"
	if err := b.event(first.ID, "delivery_uncertain", p.epoch); err != nil {
		t.Fatal(err)
	}
	if _, err := b.request(recipient, "ax.check_inbox", raw(object{})); err == nil {
		t.Fatal("skipped uncertain FIFO head")
	}
	if got, _ := b.message(second.ID); got.State != "queued" {
		t.Fatal("consumed next item")
	}
	request(t, b, recipient, "ax.get_message", object{"message_id": first.ID})
	request(t, b, recipient, "ax.ack", object{"message_id": first.ID})
	got := request(t, b, recipient, "ax.check_inbox", object{}).(object)["message"].(Message)
	if got.ID != second.ID {
		t.Fatal("did not advance after explicit recovery")
	}
}

func TestVerifyRequiresMatchingReplyThroughAttachedWake(t *testing.T) {
	dir := startTestServer(t)
	path := filepath.Join(dir, "wake.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	wakes := make(chan adapterWake, 4)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var wake adapterWake
		if r.Method != "POST" || r.URL.Path != "/wake" || json.NewDecoder(r.Body).Decode(&wake) != nil {
			http.Error(w, "bad wake", 400)
			return
		}
		wakes <- wake
		w.WriteHeader(http.StatusNoContent)
	})}
	go server.Serve(l)
	defer server.Close()
	m := startAttachedMCP(t, dir, "-n", "worker", "-s", "existing", "-p", "default", "-w", path)
	if err := m.call(t, "list_agents", object{}, nil); err != "" {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan error, 1)
	var output bytes.Buffer
	go func() { done <- verifyAgent(ctx, dir, "worker", &output) }()
	var wake adapterWake
	select {
	case wake = <-wakes:
	case <-ctx.Done():
		t.Fatal("no native wake")
	}
	if wake.Native != "existing" || strings.Contains(wake.Text, "AX_VERIFY_") {
		t.Fatalf("wake leaked body or selected wrong conversation: %+v", wake)
	}
	select {
	case err := <-done:
		t.Fatalf("wake acceptance passed verification: %v", err)
	default:
	}
	id := regexp.MustCompile(`msg_[a-f0-9]+`).FindString(wake.Text)
	var received Message
	if err := m.call(t, "get_message", object{"message_id": id}, &received); err != "" {
		t.Fatal(err)
	}
	if err := m.call(t, "ack_message", object{"message_id": id}, nil); err != "" {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("acknowledgment alone passed verification: %v", err)
	default:
	}
	token := regexp.MustCompile(`AX_VERIFY_[a-f0-9]+`).FindString(received.Text)
	if err := m.call(t, "reply", object{"message_id": id, "text": "wrong token"}, nil); err != "" {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("wrong token passed verification: %v", err)
	default:
	}
	if err := m.call(t, "reply", object{"message_id": id, "text": token}, nil); err != "" {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil || !strings.Contains(output.String(), "Verified worker") {
			t.Fatalf("round trip failed: %v %s", err, &output)
		}
	case <-ctx.Done():
		t.Fatal("reply did not complete verification")
	}
	// Losing the adapter socket must leave the host's MCP connection usable.
	server.Close()
	if err := m.call(t, "list_agents", object{}, nil); err != "" {
		t.Fatal("adapter exit killed messaging tools: ", err)
	}
}

func TestAttachRejectsNetworkWakeAndUnknownPermission(t *testing.T) {
	dir := startTestServer(t)
	err := attach(context.Background(), dir, []string{"-n", "remote", "-s", "one", "-w", "https://example.com/wake"}, strings.NewReader(""), io.Discard, nil)
	if err == nil {
		t.Fatal("accepted remote wake URL")
	}
	m := startAttachedMCP(t, dir, "-n", "unconfigured", "-s", "one")
	if err := m.call(t, "check_inbox", object{}, nil); !strings.Contains(err, "blocked") {
		t.Fatalf("unknown permission was treated as ready: %s", err)
	}
	tools := string(m.rpc(t, "tools/list", object{}))
	if !strings.Contains(tools, "check_inbox") || strings.Contains(tools, "spawn_agent") || strings.Contains(tools, "AX wakes you for replies") {
		t.Fatalf("misleading tool capabilities: %s", tools)
	}
}

func TestAttachedCLIExitsWhileStdinIsIdle(t *testing.T) {
	if os.Getenv("AX_TEST_ATTACH_CHILD") == "1" {
		_ = Main([]string{"attach", "-n", "idle", "-s", "idle-conversation", "-p", "default"})
		os.Exit(0)
	}
	dir := startTestServer(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestAttachedCLIExitsWhileStdinIsIdle$")
	cmd.Env = append(os.Environ(), "AX_HOME="+dir, "AX_TEST_ATTACH_CHILD=1")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	defer in.Close()
	if err = json.NewEncoder(in).Encode(packet{JSONRPC: "2.0", ID: raw(1), Method: "initialize", Params: raw(object{})}); err != nil {
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	go func() { var p packet; ready <- json.NewDecoder(out).Decode(&p) }()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("MCP child failed to initialize")
	}
	if err = cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("MCP child exit: %v %s", err, &stderr)
		}
	case <-time.After(3 * time.Second):
		cmd.Process.Kill()
		<-done
		t.Fatal("SIGTERM left MCP child blocked on stdin")
	}
}
