package ax

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestResumedCodexToolReclaimsBridgeAndReadsAcceptedMail(t *testing.T) {
	dir := startTestServer(t)
	s := Session{ID: randomID("agt_"), Secret: randomID("") + randomID(""), Name: "reviewer", Host: "codex", Mesh: testMesh, Native: uuid(), Started: true}
	owner := connectDiscoveryPeer(t, dir, s)
	file := filepath.Join(dir, "reviewer.json")
	if err := saveSession(file, s); err != nil {
		t.Fatal(err)
	}
	sender := connectDiscoveryPeer(t, dir, Session{ID: randomID("agt_"), Secret: randomID("") + randomID(""), Name: "builder", Host: "codex", Mesh: testMesh, Native: uuid()})
	var sent Message
	if err := sender.call("ax.send", object{"target": s.Name, "text": "review the original request", "client_message_id": "original"}, &sent); err != nil {
		t.Fatal(err)
	}
	select {
	case offer := <-owner.offers:
		if err := owner.call("ax.receipt", object{"message_id": offer.ID, "receipt": "wake_accepted", "handoff_id": offer.HandoffID}, nil); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("original bridge did not receive wake")
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &bridge{ctx: ctx, dir: dir, file: file, session: s, out: io.Discard}
	done := make(chan struct{})
	go func() { defer close(done); b.connectLoop() }()
	t.Cleanup(func() { cancel(); <-done })
	// Tool discovery alone cannot decide which MCP client owns the conversation.
	time.Sleep(50 * time.Millisecond)
	if err := owner.call("ax.heartbeat", object{}, nil); err != nil {
		t.Fatalf("background duplicate displaced original bridge: %v", err)
	}
	meta := raw(object{"threadId": s.Native})
	callCtx, stop := context.WithTimeout(ctx, 900*time.Millisecond)
	defer stop()
	var got object
	if err := b.toolCall(callCtx, meta, "ax.get_message", object{"message_id": sent.ID}, &got); err != nil {
		t.Fatalf("resumed conversation cannot fetch its accepted wake: %v", err)
	}
	if got["message_id"] != sent.ID || got["text"] != sent.Text {
		t.Fatalf("recovery changed the original message: %v", got)
	}
	if err := owner.call("ax.heartbeat", object{}, nil); err == nil {
		t.Fatal("replaced bridge retained its lease")
	}
}

func TestInheritedCodexThreadCannotClaimBridge(t *testing.T) {
	dir := startTestServer(t)
	s := Session{LaunchID: "current", ID: randomID("agt_"), Secret: randomID("") + randomID(""), Name: "parent", Host: "codex", Mesh: testMesh, Native: uuid(), Started: true}
	admin, err := dial(socketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.close()
	if err := admin.call("ax.enroll", s, nil); err != nil {
		t.Fatal(err)
	}
	owner, err := dial(socketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.close()
	if err := owner.call("ax.connect", object{"version": "1", "agent_id": s.ID, "secret": s.Secret, "launch_id": s.LaunchID}, nil); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "parent.json")
	if err := saveSession(file, s); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, launch, thread, want string
	}{
		{"inherited child", "current", uuid(), "another Codex session"},
		{"old launch", "previous", s.Native, "earlier launch"},
		{"missing metadata", "current", "", "did not supply session metadata"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := s
			copy.LaunchID = tc.launch
			b := &bridge{session: copy, file: file, dir: dir}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := b.toolCall(ctx, raw(object{"threadId": tc.thread}), "ax.list", object{}, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) || b.claim != nil {
				t.Fatalf("unrelated client requested ownership: %v", err)
			}
			if err := owner.call("ax.heartbeat", object{}, nil); err != nil {
				t.Fatalf("unrelated client fenced the owner: %v", err)
			}
		})
	}
}

func TestBrokerClaimRequiresCurrentCodexIdentity(t *testing.T) {
	for _, invalid := range []string{"native", "secret", "launch", "harness"} {
		t.Run(invalid, func(t *testing.T) {
			b, _ := localBroker(t)
			s, owner, _ := endpoint(t, b, "reviewer", testMesh)
			args := object{"version": "1", "agent_id": s.ID, "secret": s.Secret, "claim_native_session_id": s.Native, "claim_deadline_ms": time.Now().Add(time.Second).UnixMilli()}
			switch invalid {
			case "native":
				args["claim_native_session_id"] = uuid()
			case "secret":
				args["secret"] = strings.Repeat("x", 64)
			case "launch":
				args["launch_id"] = "earlier"
			case "harness":
				b.peers[s.ID].Host = "claude"
			}
			x, y := net.Pipe()
			defer x.Close()
			defer y.Close()
			if _, err := b.request(&serverConn{Conn: x}, "ax.connect", raw(args)); err == nil {
				t.Fatal("unverified client replaced the existing bridge")
			}
			request(t, b, owner, "ax.heartbeat", object{})
		})
	}
}

func TestBridgeCancellationClosesIdleMCPInput(t *testing.T) {
	dir := startTestServer(t)
	s := Session{ID: randomID("agt_"), Secret: randomID("") + randomID(""), Name: "idle", Host: "external", Mesh: testMesh, Native: "idle", Started: true, DeliveryMode: "manual"}
	owner := connectDiscoveryPeer(t, dir, s)
	owner.close()
	file := filepath.Join(dir, "idle.json")
	if err := saveSession(file, s); err != nil {
		t.Fatal(err)
	}
	in, writer := io.Pipe()
	defer in.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Bridge(ctx, dir, file, in, io.Discard) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("normal shutdown reported failure: %v", err)
		}
	case <-time.After(time.Second):
		in.Close()
		<-done
		t.Fatal("cancelled bridge remains alive waiting for idle MCP input")
	}
}

func TestExpiredToolDoesNotLeaveBackgroundOwnershipClaim(t *testing.T) {
	dir := startTestServer(t)
	s := Session{ID: randomID("agt_"), Secret: randomID("") + randomID(""), Name: "parent", Host: "codex", Mesh: testMesh, Native: uuid(), Started: true}
	owner := connectDiscoveryPeer(t, dir, s)
	file := filepath.Join(dir, "parent.json")
	if err := saveSession(file, s); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &bridge{session: s, file: file, dir: dir, ctx: ctx}
	callCtx, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	defer stop()
	if err := b.toolCall(callCtx, raw(object{"threadId": s.Native}), "ax.list", object{}, nil); err == nil {
		t.Fatal("tool succeeded without a connector")
	}
	if b.claim != nil {
		t.Fatal("expired tool left a claim for future background reconnects")
	}
	done := make(chan struct{})
	go func() { defer close(done); b.connectLoop() }()
	defer func() { cancel(); <-done }()
	time.Sleep(100 * time.Millisecond)
	if err := owner.call("ax.heartbeat", object{}, nil); err != nil {
		t.Fatalf("background retry of expired tool displaced the owner: %v", err)
	}
}

func TestWaitingToolCannotBindUnstartedConversation(t *testing.T) {
	dir := startTestServer(t)
	s := Session{LaunchID: "new", ID: randomID("agt_"), Secret: randomID("") + randomID(""), Name: "parent", Host: "codex", Mesh: testMesh, Native: uuid()}
	file := filepath.Join(dir, "parent.json")
	if err := saveSession(file, s); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(file)
	b := &bridge{session: s, file: file, dir: dir}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_ = b.toolCall(ctx, raw(object{"threadId": uuid()}), "ax.list", object{}, nil)
	after, _ := os.ReadFile(file)
	if !bytes.Equal(before, after) || b.claim != nil {
		t.Fatal("waiting inherited client changed the saved native binding")
	}
}

func TestWaitingToolClaimsAfterNativeBinding(t *testing.T) {
	dir := startTestServer(t)
	s := Session{ID: randomID("agt_"), Secret: randomID("") + randomID(""), Name: "parent", Host: "codex", Mesh: testMesh, Native: uuid()}
	owner := connectDiscoveryPeer(t, dir, s)
	file := filepath.Join(dir, "parent.json")
	if err := saveSession(file, s); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &bridge{session: s, file: file, dir: dir, ctx: ctx, out: io.Discard}
	done := make(chan struct{})
	go func() { defer close(done); b.connectLoop() }()
	defer func() { cancel(); <-done }()
	written := make(chan error, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		bound := s
		bound.Started = true
		written <- saveSession(file, bound)
	}()
	callCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	err := b.toolCall(callCtx, raw(object{"threadId": s.Native}), "ax.list", object{}, nil)
	if writeErr := <-written; writeErr != nil {
		t.Fatal(writeErr)
	}
	if err != nil {
		t.Fatalf("first tool call missed the native binding: %v", err)
	}
	if err := owner.call("ax.heartbeat", object{}, nil); err == nil {
		t.Fatal("original bridge kept the lease after recovery")
	}
}

func TestWaitingClaimRetriesAfterBrokerNativeBinding(t *testing.T) {
	dir := startTestServer(t)
	s := Session{LaunchID: "current", ID: randomID("agt_"), Secret: randomID("") + randomID(""), Name: "parent", Host: "codex", Mesh: testMesh, Native: uuid(), Started: true}
	admin, err := dial(socketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.close()
	if err := admin.call("ax.enroll", s, nil); err != nil {
		t.Fatal(err)
	}
	owner, err := dial(socketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.close()
	if err := owner.call("ax.connect", object{"version": "1", "agent_id": s.ID, "secret": s.Secret, "launch_id": s.LaunchID}, nil); err != nil {
		t.Fatal(err)
	}
	// A nonempty launch clears the broker binding. The file is already saved,
	// as it is just before SessionStart submits its lifecycle notification.
	file := filepath.Join(dir, "parent.json")
	if err := saveSession(file, s); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &bridge{session: s, file: file, dir: dir, ctx: ctx, out: io.Discard}
	done := make(chan struct{})
	go func() { defer close(done); b.connectLoop() }()
	defer func() { cancel(); <-done }()
	time.Sleep(50 * time.Millisecond)
	bound := make(chan error, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		bound <- admin.call("ax.lifecycle", object{"agent_id": s.ID, "secret": s.Secret, "launch_id": s.LaunchID, "native_session_id": s.Native, "permission_mode": "default", "state": "ready"}, nil)
	}()
	callCtx, stop := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer stop()
	err = b.toolCall(callCtx, raw(object{"threadId": s.Native}), "ax.list", object{}, nil)
	if bindErr := <-bound; bindErr != nil {
		t.Fatal(bindErr)
	}
	if err != nil {
		t.Fatalf("claim missed the broker's delayed native binding: %v", err)
	}
}

func TestBridgeCLIExitsWhileInheritedStdinIsIdle(t *testing.T) {
	if os.Getenv("AX_TEST_BRIDGE_CHILD") == "1" {
		// TestMain clears inherited AX identity; restore only this child fixture.
		os.Setenv("AX_SESSION_FILE", os.Getenv("AX_TEST_BRIDGE_FILE"))
		if err := Main([]string{"bridge"}); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	dir := startTestServer(t)
	file := filepath.Join(dir, "idle.json")
	s := Session{ID: randomID("agt_"), Secret: randomID("") + randomID(""), Name: "idle", Host: "external", Mesh: testMesh, Native: "idle", Started: true, DeliveryMode: "manual"}
	if err := saveSession(file, s); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestBridgeCLIExitsWhileInheritedStdinIsIdle$")
	cmd.Env = append(os.Environ(), "AX_HOME="+dir, "AX_TEST_BRIDGE_FILE="+file, "AX_TEST_BRIDGE_CHILD=1")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	if err := json.NewEncoder(in).Encode(packet{JSONRPC: "2.0", ID: raw(1), Method: "initialize", Params: raw(object{})}); err != nil {
		t.Fatal(err)
	}
	var ready packet
	if err := json.NewDecoder(out).Decode(&ready); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		cmd.Process.Kill()
		<-done
		t.Fatal("SIGTERM left bridge blocked on inherited stdin")
	}
}

func TestDisplacedToolRetryCannotReclaimBridge(t *testing.T) {
	dir := startTestServer(t)
	s := Session{ID: randomID("agt_"), Secret: randomID(""), Name: "reviewer", Host: "codex", Mesh: testMesh, Native: uuid(), Started: true}
	replacement := connectDiscoveryPeer(t, dir, s)
	file := filepath.Join(dir, "reviewer.json")
	if err := saveSession(file, s); err != nil {
		t.Fatal(err)
	}
	x, y := net.Pipe()
	old := newClient(x)
	defer y.Close()
	ctx, cancel := context.WithCancel(context.Background())
	b := &bridge{ctx: ctx, dir: dir, file: file, session: s, c: old, out: io.Discard}
	done := make(chan struct{})
	go func() { defer close(done); b.connectLoop() }()
	defer func() { cancel(); old.close(); <-done }()
	// The displaced client's in-flight heartbeat loses its response when fenced.
	go func() { readFrame(y); y.Close() }()
	callCtx, stop := context.WithTimeout(ctx, 350*time.Millisecond)
	defer stop()
	_ = b.toolCall(callCtx, raw(object{"threadId": s.Native}), "ax.list", object{}, nil)
	if err := replacement.call("ax.heartbeat", object{}, nil); err != nil {
		t.Fatalf("automatic retry fenced the replacement owner: %v", err)
	}
}

func TestBrokerRejectsSubmittedExpiredClaim(t *testing.T) {
	b, _ := localBroker(t)
	s, owner, _ := endpoint(t, b, "current", testMesh)
	b.peers[s.ID].Host = "codex"
	x, y := net.Pipe()
	defer x.Close()
	defer y.Close()
	// The request reached the broker while the caller's deadline was live,
	// but was processed only after that deadline. It must not fence the owner.
	args := object{"version": "1", "agent_id": s.ID, "secret": s.Secret, "claim_native_session_id": s.Native, "claim_deadline_ms": time.Now().Add(-time.Millisecond).UnixMilli()}
	if _, err := b.request(&serverConn{Conn: x}, "ax.connect", raw(args)); err == nil {
		t.Fatal("expired submitted claim replaced the owner")
	}
	request(t, b, owner, "ax.heartbeat", object{})
}
