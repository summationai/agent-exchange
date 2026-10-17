package ax

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResumeBindingConflictIsDurableAndActionable(t *testing.T) {
	for _, host := range []string{"claude", "codex"} {
		t.Run(host, func(t *testing.T) {
			dir := startTestServer(t)
			original, selected := uuid(), uuid()
			s := Session{ID: randomID("agt_"), Secret: randomID("") + randomID(""), Name: "saved", Host: host, Mesh: testMesh, Native: original}
			file := filepath.Join(dir, "session.json")
			if err := saveSession(file, s); err != nil {
				t.Fatal(err)
			}
			c, err := dial(socketPath(dir))
			if err != nil {
				t.Fatal(err)
			}
			defer c.close()
			if err = c.call("ax.enroll", s, nil); err != nil {
				t.Fatal(err)
			}
			sender := connectDiscoveryPeer(t, dir, Session{ID: randomID("agt_"), Secret: randomID("") + randomID(""), Name: "sender", Host: "codex", Mesh: testMesh, Native: uuid()})
			var mail Message
			if err = sender.call("ax.send", object{"target": s.Name, "text": "original mail", "client_message_id": "durable"}, &mail); err != nil {
				t.Fatal(err)
			}
			hook := func(native, event string) error {
				return Hook(dir, file, bytes.NewReader(raw(object{"hook_event_name": event, "source": "resume", "session_id": native, "permission_mode": "default"})))
			}
			conflict := hook(selected, "SessionStart")
			if conflict == nil || !strings.Contains(conflict.Error(), original) || !strings.Contains(conflict.Error(), selected) || !strings.Contains(conflict.Error(), "unused AX name") {
				t.Fatalf("missing recovery detail: %v", conflict)
			}
			// Late hooks from either conversation cannot erase this launch's conflict.
			for _, id := range []string{original, selected} {
				if hook(id, "SessionStart") == nil || hook(id, "Stop") == nil {
					t.Fatal("late hook cleared conflict")
				}
			}
			after, err := loadSession(file)
			if err != nil || after.Native != original || after.ID != s.ID || after.Secret != s.Secret || after.Started || after.BindingError != conflict.Error() {
				t.Fatalf("saved state changed incorrectly: %+v %v", after, err)
			}
			if err = c.call("ax.connect", object{"version": "1", "agent_id": s.ID, "secret": s.Secret}, nil); err != nil {
				t.Fatal(err)
			}
			if err = c.call("ax.ready", object{}, nil); err == nil {
				t.Fatal("conflict activated delivery")
			}
			agents := discoveryList(t, c)
			if a := agents[s.ID]; a.State != "binding-conflict" || a.BindingError != conflict.Error() {
				t.Fatalf("hidden conflict: %+v", a)
			}
			var out bytes.Buffer
			b := &bridge{session: s, file: file, dir: dir, c: c, ctx: context.Background(), out: &out, toolsListed: true}
			b.bootstrap()
			if b.active || out.Len() != 0 {
				t.Fatal("conflicted session was bootstrapped")
			}
			if err = b.toolCall(context.Background(), nil, "ax.list", object{}, nil); err == nil || err.Error() != conflict.Error() {
				t.Fatalf("generic tool error: %v", err)
			}
			doctorBroker(context.Background(), dir, &out)
			if !strings.Contains(out.String(), conflict.Error()) {
				t.Fatal("doctor hid recovery instructions")
			}
			var status struct{ Message Message }
			if err = sender.call("ax.status", object{"message_id": mail.ID}, &status); err != nil {
				t.Fatal(err)
			}
			if status.Message.State != "queued" || status.Message.Recipient != s.ID || status.Message.Text != "original mail" {
				t.Fatalf("mail moved or replayed: %+v", status)
			}
			// A deliberate relaunch resets the error while retaining the same identity.
			after.BindingError = ""
			if err = saveSession(file, after); err != nil {
				t.Fatal(err)
			}
			admin, err := dial(socketPath(dir))
			if err != nil {
				t.Fatal(err)
			}
			defer admin.close()
			if err = admin.call("ax.enroll", after, nil); err != nil {
				t.Fatal(err)
			}
			if err = hook(original, "SessionStart"); err != nil {
				t.Fatal(err)
			}
			if err = c.call("ax.ready", object{}, nil); err != nil {
				t.Fatal(err)
			}
			if a := discoveryList(t, c)[s.ID]; a.BindingError != "" || a.Native != original {
				t.Fatalf("recovery changed identity: %+v", a)
			}
		})
	}
}

func mustSession(t *testing.T, file string) Session {
	t.Helper()
	s, err := loadSession(file)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestExplicitResumeCanReuseAXName(t *testing.T) {
	dir := startTestServer(t)
	bin := testDir(t)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	selected := uuid()
	out := filepath.Join(bin, "args")
	script := "#!/bin/sh\nprintf '%s\\0' \"$@\" > " + shellQuote(out) + "\nexit 99\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(filepath.Join(dir, "sessions")); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "sessions", "saved.json")
	original := Session{ID: randomID("agt_"), Secret: randomID("") + randomID(""), Name: "saved", Host: "claude", Mesh: testMesh, Native: uuid(), Started: true}
	if err := saveSession(file, original); err != nil {
		t.Fatal(err)
	}
	// A finishing old hook holds this same state lock. A new launch must
	// wait before loading or replacing its generation.
	stateLock, err := lockSession(file)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Launch(context.Background(), dir, "claude", []string{"-n", "saved", "-r", selected}) }()
	select {
	case err := <-done:
		stateLock.Close()
		t.Fatalf("launch ignored concurrent state writer: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if saved := mustSession(t, file); saved.LaunchID != "" {
		stateLock.Close()
		t.Fatal("launch replaced state while previous writer held lock")
	}
	stateLock.Close()
	err = <-done
	if err == nil || err.Error() != "exit status 99" {
		t.Fatalf("native harness was not launched: %v", err)
	}
	data, _ := os.ReadFile(out)
	if !bytes.Contains(data, []byte(selected)) {
		t.Fatal("native resume argument lost")
	}
	saved := mustSession(t, file)
	if saved.LaunchID == "" || saved.Started || saved.ID != original.ID || saved.Secret != original.Secret {
		t.Fatal("launch did not preserve AX identity")
	}
	if err = Hook(dir, file, bytes.NewReader(raw(object{"session_id": selected, "hook_event_name": "SessionStart", "permission_mode": "default"})), saved.LaunchID); err != nil {
		t.Fatal(err)
	}
	saved = mustSession(t, file)
	if saved.Native != selected || !saved.Started || saved.BindingError != "" {
		t.Fatal("native selection did not replace saved conversation")
	}
}

func TestProvisionalStartupNeverRebindsPersistedIdentity(t *testing.T) {
	dir := startTestServer(t)
	s := Session{ID: randomID("agt_"), Secret: randomID("") + randomID(""), Name: "fresh", Host: "claude", Mesh: testMesh}
	file := filepath.Join(dir, "session.json")
	if err := saveSession(file, s); err != nil {
		t.Fatal(err)
	}
	c, err := dial(socketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	if err = c.call("ax.enroll", s, nil); err != nil {
		t.Fatal(err)
	}
	native := uuid()
	first := object{"hook_event_name": "SessionStart", "source": "startup", "session_id": native, "transcript_path": filepath.Join(dir, "not-created.jsonl"), "permission_mode": "default"}
	if err = Hook(dir, file, bytes.NewReader(raw(first))); err != nil {
		t.Fatal(err)
	}
	first["source"], first["session_id"] = "resume", uuid()
	if err = Hook(dir, file, bytes.NewReader(raw(first))); err == nil {
		t.Fatal("provisional startup silently rebound")
	}
	saved, err := loadSession(file)
	if err != nil || saved.Native != native || saved.BindingError == "" {
		t.Fatalf("lost first binding: %+v %v", saved, err)
	}
}

func TestBrokerKeepsBindingConflictAcrossRestart(t *testing.T) {
	b, dir := localBroker(t)
	s, c, _ := endpoint(t, b, "saved", testMesh)
	message := bindingConflict(s, uuid()).Error()
	request(t, b, &serverConn{}, "ax.lifecycle", object{"agent_id": s.ID, "secret": s.Secret, "native_session_id": s.Native, "binding_error": message, "state": "blocked"})
	request(t, b, &serverConn{}, "ax.lifecycle", object{"agent_id": s.ID, "secret": s.Secret, "native_session_id": s.Native, "state": "ready"})
	if b.peers[s.ID].State != "blocked" {
		t.Fatal("late lifecycle cleared broker guard")
	}
	if _, err := b.request(c, "ax.presence", raw(object{"state": "ready"})); err == nil {
		t.Fatal("MCP presence cleared broker guard")
	}
	b.db.Close()
	reopened, err := openBroker(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
	a := reopened.list()[0]
	if a.State != "binding-conflict" || a.BindingError != message || a.Native != s.Native {
		t.Fatalf("conflict lost on restart: %+v", a)
	}
}

func TestNewLaunchSelectsConversationAndFencesPreviousLaunch(t *testing.T) {
	for _, host := range []string{"claude", "codex", "grok", "opencode", "pi"} {
		t.Run(host, func(t *testing.T) {
			b, dir := localBroker(t)
			s, oldConn, _ := endpoint(t, b, "role", testMesh)
			s.Host = host
			s.LaunchID = "previous"
			s.Started = true
			if host == "opencode" {
				s.Native = "ses_original"
			}
			p := b.peers[s.ID]
			p.Host = host
			p.Native = s.Native
			p.LaunchID = s.LaunchID
			if err := b.save(p); err != nil {
				t.Fatal(err)
			}
			sender, sendConn, _ := endpoint(t, b, "sender", testMesh)
			delivered := request(t, b, sendConn, "ax.send", object{"target": s.ID, "text": "already read", "client_message_id": "old"}).(Message)
			if err := b.event(delivered.ID, "acknowledged", p.epoch); err != nil {
				t.Fatal(err)
			}
			queued := request(t, b, sendConn, "ax.send", object{"target": s.ID, "text": "waiting for role", "client_message_id": "queued"}).(Message)
			current := s
			current.LaunchID = "current"
			current.Started = false
			request(t, b, &serverConn{}, "ax.enroll", current)
			if b.peers[s.ID].Native != "" || b.peers[s.ID].ready {
				t.Fatal("old native binding survived new launch")
			}
			if _, err := b.request(oldConn, "ax.list", raw(object{})); err == nil {
				t.Fatal("old connection retained ownership")
			}
			for _, method := range []string{"ax.connect", "ax.lifecycle"} {
				if _, err := b.request(&serverConn{}, method, raw(object{"version": "1", "agent_id": s.ID, "secret": s.Secret, "launch_id": "previous", "native_session_id": s.Native, "state": "ready"})); err == nil {
					t.Fatalf("old launch reached %s", method)
				}
			}
			native := uuid()
			if host == "opencode" {
				native = "ses_selected"
			}
			request(t, b, &serverConn{}, "ax.lifecycle", object{"agent_id": s.ID, "secret": s.Secret, "launch_id": "current", "native_session_id": native, "permission_mode": "default", "state": "ready"})
			conn, client := net.Pipe()
			defer conn.Close()
			defer client.Close()
			c := &serverConn{Conn: conn}
			request(t, b, c, "ax.connect", object{"version": "1", "agent_id": s.ID, "secret": s.Secret, "launch_id": "current"})
			request(t, b, c, "ax.ready", object{})
			if b.peers[s.ID].Native != native {
				t.Fatal("native selection did not bind")
			}
			for _, m := range []Message{delivered, queued} {
				saved, err := b.message(m.ID)
				if err != nil || saved.State != m.State && !(m.ID == delivered.ID && saved.State == "acknowledged") {
					t.Fatal("launch replayed or discarded saved mail")
				}
			}
			if got := request(t, b, sendConn, "ax.status", object{"message_id": queued.ID}).(object)["message"].(Message); got.Sender.ID != sender.ID || got.Recipient != s.ID {
				t.Fatal("mail identity changed")
			}
			// A new generation must stay fenced after a broker restart too.
			if err := b.db.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := openBroker(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.db.Close()
			if _, err = reopened.request(&serverConn{}, "ax.lifecycle", raw(object{"agent_id": s.ID, "secret": s.Secret, "launch_id": "previous", "native_session_id": native, "state": "ready"})); err == nil {
				t.Fatal("restart forgot launch fence")
			}
		})
	}
}

func TestStaleHookAndBridgeCannotAdoptNewLaunch(t *testing.T) {
	dir := startTestServer(t)
	file := filepath.Join(dir, "session.json")
	original := Session{LaunchID: "old", ID: randomID("agt_"), Secret: randomID("") + randomID(""), Name: "role", Host: "claude", Mesh: testMesh, Native: uuid(), Started: true}
	current := original
	current.LaunchID = "new"
	current.Started = false
	if err := saveSession(file, current); err != nil {
		t.Fatal(err)
	}
	c, err := dial(socketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	if err = c.call("ax.enroll", current, nil); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(file)
	if err = Hook(dir, file, bytes.NewReader(raw(object{"session_id": original.Native, "hook_event_name": "SessionStart"})), "old"); err == nil {
		t.Fatal("stale hook selected old conversation")
	}
	b := &bridge{session: original, file: file, dir: dir, ctx: context.Background(), toolsListed: true}
	b.bootstrap()
	if b.bootstrapped || b.session.LaunchID != "old" {
		t.Fatal("stale bridge adopted current launch")
	}
	if _, err = b.bindingLocked(nil); err == nil {
		t.Fatal("stale tool call adopted current launch")
	}
	if err = bindAdapter(dir, file, original.Native, "default", "ready", "old"); err == nil {
		t.Fatal("stale adapter adopted current launch")
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(before, after) {
		t.Fatal("stale source mutated current session")
	}
	selected := uuid()
	if err = Hook(dir, file, bytes.NewReader(raw(object{"session_id": selected, "hook_event_name": "SessionStart", "permission_mode": "default"})), "new"); err != nil {
		t.Fatal(err)
	}
	saved := mustSession(t, file)
	if saved.Native != selected || !saved.Started || saved.BindingError != "" {
		t.Fatal("current launch failed to select")
	}
	// A second conversation inside the already-bound launch remains a conflict.
	if err = Hook(dir, file, bytes.NewReader(raw(object{"session_id": original.Native, "hook_event_name": "SessionStart"})), "new"); err == nil {
		t.Fatal("already-bound launch switched to another conversation")
	}
}
