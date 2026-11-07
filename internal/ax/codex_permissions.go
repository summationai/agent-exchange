package ax

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// The remote TUI rejects permission flags on resume. Apply an explicit bypass
// choice to the native session request instead, before its first turn can run.
func codexBypassArgs(args []string) ([]string, bool, error) {
	var out []string
	bypass, conflict := false, false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			out = append(out, args[i:]...)
			break
		}
		if arg == "--dangerously-bypass-approvals-and-sandbox" || arg == "--yolo" {
			bypass = true
			continue
		}
		key, _, _ := strings.Cut(arg, "=")
		if key == "--sandbox" || key == "--ask-for-approval" || key == "--approve-for-me" || key == "--not-so-yolo" || (strings.HasPrefix(key, "-s") && !strings.HasPrefix(key, "--")) || (strings.HasPrefix(key, "-a") && !strings.HasPrefix(key, "--")) {
			conflict = true
		}
		out = append(out, arg)
		// Do not interpret option values as AX's permission choice.
		switch arg {
		case "-c", "--config", "-m", "--model", "-p", "--profile", "-s", "--sandbox", "-a", "--ask-for-approval", "-i", "--image", "-C", "--cd", "--add-dir", "--enable", "--disable", "--local-provider", "--remote", "--remote-auth-token-env":
			if i+1 < len(args) {
				i++
				out = append(out, args[i])
			}
		}
	}
	if bypass && conflict {
		return nil, false, fmt.Errorf("choose bypass or separate sandbox/approval options, not both")
	}
	return out, bypass, nil
}

// Only the TUI uses this private adapter. AX's queue and metadata connections go
// directly to the backend. No peer message can select or change permissions.
func codexSessionSocket(ctx context.Context, dir, file, remote string, bypass bool, startupErrors chan<- error) (string, func(), error) {
	session, err := loadSession(file)
	if err != nil {
		return "", nil, err
	}
	path := filepath.Join(dir, randomID("permissions_")+".sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		return "", nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		listener.Close()
		return "", nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	// One bounded worker keeps broker failures off every native transport path.
	events := make(chan []byte, 8)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-events:
				if err := Hook(dir, file, bytes.NewReader(event), session.LaunchID); err != nil {
					select {
					case startupErrors <- err:
					default:
					}
				}
			}
		}
	}()
	server := &http.Server{ReadHeaderTimeout: 3 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backend, err := dialCodex(ctx, remote)
		if err != nil {
			http.Error(w, "Codex backend unavailable", http.StatusBadGateway)
			return
		}
		defer backend.Close()
		front, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer front.Close()
		front.SetReadLimit(64 << 20)
		backend.SetReadLimit(64 << 20)
		stop := context.AfterFunc(ctx, func() { front.Close(); backend.Close() })
		defer stop()
		observer := &codexSessionObserver{pending: make(map[string]bool)}
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer front.Close()
			for {
				kind, data, err := backend.ReadMessage()
				if err != nil || front.WriteMessage(kind, data) != nil {
					return
				}
				if event := observer.response(data); event != nil {
					select {
					case events <- event:
					default:
						select {
						case startupErrors <- fmt.Errorf("Codex session changes exceeded AX's startup queue; messaging may be unavailable"):
						default:
						}
					}
				}
			}
		}()
		for {
			kind, data, err := front.ReadMessage()
			if err != nil {
				break
			}
			observer.request(data)
			if bypass {
				data, err = codexBypassRequest(data)
			}
			if err != nil || backend.WriteMessage(kind, data) != nil {
				break
			}
		}
		backend.Close()
		<-done
	})}
	go server.Serve(limitListener(listener, 8))
	return "unix://" + path, func() { cancel(); server.Close(); os.Remove(path) }, nil
}

// Observe only responses to the TUI's own conversation selection. Unsolicited
// notifications, subagent creation, and thread/read results cannot bind AX.
type codexSessionObserver struct {
	mu      sync.Mutex
	pending map[string]bool
}

func (o *codexSessionObserver) request(data []byte) {
	var p packet
	if json.Unmarshal(data, &p) != nil || len(p.ID) == 0 || len(p.ID) > 128 {
		return
	}
	switch p.Method {
	case "thread/start", "thread/resume", "thread/fork":
		o.mu.Lock()
		defer o.mu.Unlock()
		if len(o.pending) < 16 {
			o.pending[string(p.ID)] = true
		}
	}
}

func (o *codexSessionObserver) response(data []byte) []byte {
	var p packet
	if json.Unmarshal(data, &p) != nil || p.Method != "" {
		return nil
	}
	o.mu.Lock()
	matched := o.pending[string(p.ID)]
	delete(o.pending, string(p.ID))
	o.mu.Unlock()
	if !matched || p.Error != nil {
		return nil
	}
	var result struct {
		Thread  struct{ ID string }   `json:"thread"`
		Sandbox struct{ Type string } `json:"sandbox"`
	}
	if json.Unmarshal(p.Result, &result) != nil || !validNative(result.Thread.ID) {
		return nil
	}
	permission := "unknown"
	switch result.Sandbox.Type {
	case "readOnly":
		permission = "read-only"
	case "workspaceWrite":
		permission = "workspace-write"
	case "dangerFullAccess":
		permission = "danger-full-access"
	}
	return raw(object{"hook_event_name": "SessionStart", "session_id": result.Thread.ID, "permission_mode": permission})
}

func codexBypassRequest(data []byte) ([]byte, error) {
	var request struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(data, &request); err != nil {
		return nil, err
	}
	switch request.Method {
	case "thread/start", "thread/resume", "thread/fork":
		// Preserve unknown protocol fields and all responses/notifications verbatim.
		var envelope map[string]json.RawMessage
		var params map[string]json.RawMessage
		if err := json.Unmarshal(data, &envelope); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(envelope["params"], &params); err != nil || params == nil {
			return nil, fmt.Errorf("invalid Codex session parameters")
		}
		params["approvalPolicy"] = raw("never")
		params["sandbox"] = raw("danger-full-access")
		delete(params, "permissions") // Named and legacy sandbox choices are exclusive.
		envelope["params"] = raw(params)
		return raw(envelope), nil
	default:
		return data, nil
	}
}
