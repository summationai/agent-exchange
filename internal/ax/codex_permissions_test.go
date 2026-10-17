package ax

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestCodexNativeSelectionCarriesEffectivePermissions(t *testing.T) {
	for _, method := range []string{"thread/start", "thread/resume", "thread/fork"} {
		for _, policy := range []struct{ native, permission string }{{"readOnly", "read-only"}, {"workspaceWrite", "workspace-write"}, {"dangerFullAccess", "danger-full-access"}, {"futurePolicy", "unknown"}} {
			o := codexSessionObserver{pending: map[string]bool{}}
			native := uuid()
			response := raw(object{"id": 1, "result": object{"thread": object{"id": native}, "sandbox": object{"type": policy.native}}})
			if o.response(response) != nil {
				t.Fatal("unsolicited response bound AX")
			}
			o.request(raw(object{"id": 1, "method": method}))
			var hook struct{ Session, Permission string }
			var event map[string]string
			if err := json.Unmarshal(o.response(response), &event); err != nil {
				t.Fatal(err)
			}
			hook.Session, hook.Permission = event["session_id"], event["permission_mode"]
			if hook.Session != native || hook.Permission != policy.permission || event["hook_event_name"] != "SessionStart" {
				t.Fatalf("%s: %+v", method, event)
			}
			if o.response(response) != nil {
				t.Fatal("duplicate response rebound AX")
			}
		}
	}
	o := codexSessionObserver{pending: map[string]bool{}}
	for i := 0; i < 100; i++ {
		o.request(raw(object{"id": i, "method": "thread/resume"}))
	}
	if len(o.pending) != 16 {
		t.Fatalf("unbounded pending selections: %d", len(o.pending))
	}
	if o.response(raw(object{"id": 0, "error": object{"code": -1, "message": "not found"}})) != nil {
		t.Fatal("failed selection bound AX")
	}
	if o.response(raw(object{"id": 1, "method": "thread/started", "params": object{"thread": object{"id": uuid()}}})) != nil {
		t.Fatal("notification bound AX")
	}
}

func TestCodexNativeSelectionBindsBeforeFirstTurn(t *testing.T) {
	dir := startTestServer(t)
	file := filepath.Join(dir, "session.json")
	s := Session{ID: randomID("agt_"), Secret: randomID("") + randomID(""), Name: "automatic", Host: "codex", Mesh: testMesh}
	if err := saveSession(file, s); err != nil {
		t.Fatal(err)
	}
	broker, err := dial(socketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer broker.close()
	if err = broker.call("ax.enroll", s, nil); err != nil {
		t.Fatal(err)
	}
	native := uuid()
	backendPath := filepath.Join(dir, "native.sock")
	listener, err := net.Listen("unix", backendPath)
	if err != nil {
		t.Fatal(err)
	}
	request := raw(object{"id": 7, "method": "thread/resume", "params": object{"threadId": native, "sandbox": "workspace-write"}})
	response := raw(object{"id": 7, "result": object{"thread": object{"id": native}, "sandbox": object{"type": "workspaceWrite"}, "unknownFutureField": true}})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, data, err := conn.ReadMessage()
		if err != nil || !bytes.Equal(data, request) {
			t.Errorf("native request changed: %s %v", data, err)
			return
		}
		conn.WriteMessage(websocket.TextMessage, response)
	})}
	go server.Serve(listener)
	defer server.Close()
	errors := make(chan error, 1)
	remote, cleanup, err := codexSessionSocket(context.Background(), dir, file, "unix://"+backendPath, false, errors)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	conn, err := dialCodex(context.Background(), remote)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err = conn.WriteMessage(websocket.TextMessage, request); err != nil {
		t.Fatal(err)
	}
	_, got, err := conn.ReadMessage()
	if err != nil || !bytes.Equal(got, response) {
		t.Fatalf("native response changed: %s %v", got, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var agents []Agent
		if err = broker.call("ax.inspect", object{}, &agents); err != nil {
			t.Fatal(err)
		}
		if len(agents) == 1 && agents[0].Native == native && agents[0].Permission == "workspace-write" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("startup was not bound: %+v", agents)
		}
		time.Sleep(10 * time.Millisecond)
	}
	saved, err := loadSession(file)
	if err != nil || !saved.Started || saved.Native != native {
		t.Fatalf("native selection not saved: %+v %v", saved, err)
	}
	other := uuid()
	err = Hook(dir, file, bytes.NewReader(raw(object{"hook_event_name": "SessionStart", "session_id": other, "permission_mode": "read-only"})))
	if err == nil {
		t.Fatal("native selection replaced saved identity")
	}
	select {
	case err = <-errors:
		t.Fatal(err)
	default:
	}
}

func TestCodexBypassKeepsNativeArguments(t *testing.T) {
	flag := "--dangerously-bypass-approvals-and-sandbox"
	for _, tc := range []struct {
		args, want []string
		bypass     bool
	}{
		{[]string{flag, "resume"}, []string{"resume"}, true},
		{[]string{"resume", "id", flag, "prompt"}, []string{"resume", "id", "prompt"}, true},
		{[]string{"--yolo", "resume", "--last"}, []string{"resume", "--last"}, true},
		{[]string{"resume", "--", flag}, []string{"resume", "--", flag}, false},
		{[]string{"-c", flag}, []string{"-c", flag}, false},
		{[]string{"-m", flag}, []string{"-m", flag}, false},
		{[]string{"resume", "id", "-s", "read-only"}, []string{"resume", "id", "-s", "read-only"}, false},
	} {
		got, bypass, err := codexBypassArgs(tc.args)
		if err != nil || bypass != tc.bypass || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%q: args=%q bypass=%v err=%v", tc.args, got, bypass, err)
		}
	}
	for _, conflict := range []string{"-anever", "--ask-for-approval=never", "--approve-for-me", "--not-so-yolo", "-sread-only", "--sandbox=read-only"} {
		if _, _, err := codexBypassArgs([]string{flag, "resume", conflict}); err == nil {
			t.Fatalf("silently overrode conflicting permission option %s", conflict)
		}
	}
}

func TestCodexBypassOnlyChangesSessionPermissions(t *testing.T) {
	for _, method := range []string{"thread/start", "thread/resume", "thread/fork", "thread/queue/add", "turn/start", "initialize", "thread/settings/update", ""} {
		data := raw(object{"id": "same-id", "method": method, "extra": object{"keep": true}, "params": object{"threadId": "original", "model": "chosen", "sandbox": "read-only", "permissions": "restricted", "config": object{"future": true}}})
		got, err := codexBypassRequest(data)
		if err != nil {
			t.Fatal(err)
		}
		if method != "thread/start" && method != "thread/resume" && method != "thread/fork" {
			if string(got) != string(data) {
				t.Fatalf("rewrote unrelated frame %s", method)
			}
			continue
		}
		var want object
		json.Unmarshal(data, &want)
		params := want["params"].(map[string]any)
		params["approvalPolicy"], params["sandbox"] = "never", "danger-full-access"
		delete(params, "permissions")
		if string(got) != string(raw(want)) {
			t.Fatalf("lost session fields: %s", got)
		}
	}
	for _, data := range []string{`{`, `{"method":"thread/resume","params":null}`} {
		if _, err := codexBypassRequest([]byte(data)); err == nil {
			t.Fatal("invalid session request accepted")
		}
	}
}

func TestCodexBypassSocketForwardsAndCloses(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ax-permissions-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "backend.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var p packet
		if conn.ReadJSON(&p) == nil {
			conn.WriteJSON(object{"id": p.ID, "result": json.RawMessage(p.Params)})
		}
	})}
	go server.Serve(listener)
	defer server.Close()
	file := filepath.Join(dir, "session.json")
	if err := saveSession(file, Session{}); err != nil {
		t.Fatal(err)
	}
	remote, cleanup, err := codexSessionSocket(context.Background(), dir, file, "unix://"+path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	conn, err := dialCodex(context.Background(), remote)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var got object
	if err := codexCall(conn, 7, "thread/resume", object{"threadId": "original"}, &got); err != nil || got["approvalPolicy"] != "never" || got["sandbox"] != "danger-full-access" || got["threadId"] != "original" {
		t.Fatalf("native resume did not receive permissions: %+v %v", got, err)
	}
	cleanup()
	if _, err = dialCodex(context.Background(), remote); err == nil {
		t.Fatal("permission adapter remained available after cleanup")
	}
}
