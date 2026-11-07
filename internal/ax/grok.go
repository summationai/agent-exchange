package ax

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type grokConnection struct {
	mu         sync.Mutex
	conn       net.Conn
	native     string
	permission string
	prompts    map[string]chan error
}

func grokRead(r io.Reader) (object, error) {
	var n uint32
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return nil, err
	}
	if n > 64<<20 {
		return nil, errors.New("Grok frame exceeds 64 MiB")
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}
	var frame object
	err := json.Unmarshal(data, &frame)
	return frame, err
}

func grokWrite(w io.Writer, frame object) error {
	data := raw(frame)
	if err := binary.Write(w, binary.BigEndian, uint32(len(data))); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

func (g *grokConnection) send(frame object) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return grokWrite(g.conn, frame)
}

func (g *grokConnection) wake(ctx context.Context, native, text string) error {
	id := uuid()
	done := make(chan error, 1)
	g.mu.Lock()
	if native != g.native {
		g.mu.Unlock()
		return errors.New("Grok session is not ready")
	}
	if len(g.prompts) >= 8 {
		g.mu.Unlock()
		return errors.New("AX wake capacity reached")
	}
	g.prompts[id] = done
	g.mu.Unlock()
	defer func() { g.mu.Lock(); delete(g.prompts, id); g.mu.Unlock() }()
	request := object{"jsonrpc": "2.0", "id": "ax_" + id, "method": "session/prompt", "params": object{"sessionId": native, "prompt": []object{{"type": "text", "text": text}}, "_meta": object{"promptId": id, "verbatim": true}}}
	if err := g.send(object{"type": "acp", "payload": string(raw(request))}); err != nil {
		return err
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func prepareGrok(ctx context.Context, dir, file, bin string, s Session, args, env []string, failures chan<- error) ([]string, []string, func(), error) {
	if nativeHelp(args) {
		return args, env, func() {}, nil
	}
	for _, arg := range args {
		if arg == "--no-leader" {
			return nil, nil, nil, errors.New("AX requires Grok's native leader connection")
		}
		if arg == "--leader-socket" || strings.HasPrefix(arg, "--leader-socket=") {
			return nil, nil, nil, errors.New("AX owns the local Grok leader connection")
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	real := filepath.Join(dir, randomID("grok_")+".sock")
	proxy := filepath.Join(dir, randomID("grok_tui_")+".sock")
	l, err := net.Listen("unix", proxy)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	l = limitListener(l, 8)
	os.Chmod(proxy, 0600)
	log, err := openDiagnosticLog(dir, "grok.log")
	if err != nil {
		cancel()
		l.Close()
		return nil, nil, nil, err
	}
	cmd := exec.CommandContext(ctx, bin, "agent", "leader", "--leader-socket", real, "--relay-on-demand", "--no-auto-update", "--no-exit-on-disconnect")
	cmd.Env, cmd.Stdout, cmd.Stderr = env, log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		cancel()
		l.Close()
		log.Close()
		return nil, nil, nil, err
	}
	done := make(chan error, 1)
	go watchDiagnosticLog(ctx, dir, "grok.log")
	go func() { done <- cmd.Wait(); log.Close() }()
	var mu sync.Mutex
	var active *grokConnection
	connections := map[net.Conn]bool{}
	cleanup := func() {
		cancel()
		l.Close()
		mu.Lock()
		for conn := range connections {
			conn.Close()
		}
		mu.Unlock()
		<-done
		os.Remove(real)
		os.Remove(proxy)
	}
	control, stop, err := startAdapterHost(ctx, dir, file, failures, func(ctx context.Context, native, text string) error {
		mu.Lock()
		g := active
		mu.Unlock()
		if g == nil {
			return errors.New("Grok session is still starting")
		}
		return g.wake(ctx, native, text)
	})
	if err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	closeAll := func() { stop(); cleanup() }
	s.AdapterSocket = control
	if err = saveSession(file, s); err != nil {
		closeAll()
		return nil, nil, nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		closeAll()
		return nil, nil, nil, err
	}
	mcp := object{"name": "ax", "command": exe, "args": []string{"bridge"}, "env": []object{{"name": "AX_HOME", "value": dir}, {"name": "AX_SESSION_FILE", "value": file}, {"name": "AX_LAUNCH_ID", "value": s.LaunchID}}}
	go func() {
		for {
			client, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			connections[client] = true
			mu.Unlock()
			go func() {
				defer client.Close()
				defer func() { mu.Lock(); delete(connections, client); mu.Unlock() }()
				var upstream net.Conn
				deadline := time.Now().Add(30 * time.Second)
				for ctx.Err() == nil && time.Now().Before(deadline) {
					upstream, err = net.DialTimeout("unix", real, time.Second)
					if err == nil {
						break
					}
					select {
					case <-ctx.Done():
						return
					case <-time.After(50 * time.Millisecond):
					}
				}
				if upstream == nil {
					select {
					case failures <- fmt.Errorf("Grok leader did not start; inspect %s", filepath.Join(dir, "grok.log")):
					default:
					}
					return
				}
				defer upstream.Close()
				g := &grokConnection{conn: upstream, permission: "default", prompts: map[string]chan error{}}
				err = proxyGrok(client, g, mcp, func(native, permission string) error {
					if err := bindAdapter(dir, file, native, permission, "ready", s.LaunchID); err != nil {
						return err
					}
					mu.Lock()
					active = g
					mu.Unlock()
					return nil
				})
				if err != nil && ctx.Err() == nil && !errors.Is(err, io.EOF) {
					select {
					case failures <- err:
					default:
					}
				}
			}()
		}
	}()
	if len(args) == 0 && s.Native != "" {
		args = []string{"--resume", s.Native}
	}
	return withAXOptions(args, []string{"--leader", "--leader-socket", proxy}), env, closeAll, nil
}

func proxyGrok(client net.Conn, g *grokConnection, mcp object, bind func(string, string) error) error {
	var pendingMu sync.Mutex
	pending := map[string]string{}
	done := make(chan error, 1)
	go func() {
		defer g.conn.Close()
		for {
			frame, err := grokRead(client)
			if err != nil {
				done <- err
				return
			}
			if frame["type"] == "register" {
				caps, _ := frame["capabilities"].(map[string]any)
				g.mu.Lock()
				if caps["yolo_mode"] == true {
					g.permission = "bypassPermissions"
				} else if caps["auto_mode"] == true {
					g.permission = "auto"
				}
				g.mu.Unlock()
			}
			if frame["type"] == "acp" {
				var rpc object
				payload, _ := frame["payload"].(string)
				if json.Unmarshal([]byte(payload), &rpc) != nil {
					done <- errors.New("invalid Grok ACP payload")
					return
				}
				method, _ := rpc["method"].(string)
				if method == "session/new" || method == "session/load" || method == "session/resume" {
					pendingMu.Lock()
					full := len(pending) >= 64
					pendingMu.Unlock()
					// Forward native traffic even if AX cannot track more selections.
					// Capacity must not disconnect the user's TUI or grow without bound.
					if full {
						if err = g.send(frame); err != nil {
							done <- err
							return
						}
						continue
					}
					params, _ := rpc["params"].(map[string]any)
					if params == nil {
						params = object{}
						rpc["params"] = params
					}
					servers, _ := params["mcpServers"].([]any)
					params["mcpServers"] = append(servers, mcp)
					native, _ := params["sessionId"].(string)
					pendingMu.Lock()
					pending[string(raw(rpc["id"]))] = native
					pendingMu.Unlock()
					frame["payload"] = string(raw(rpc))
				}
			}
			if err = g.send(frame); err != nil {
				done <- err
				return
			}
		}
	}()
	defer client.Close()
	for {
		frame, err := grokRead(g.conn)
		if err != nil {
			// The client reader closes upstream to unblock this read. Preserve
			// its exit cause instead of reporting that intentional close.
			select {
			case cause := <-done:
				return cause
			default:
			}
			return err
		}
		if frame["type"] == "acp" {
			var rpc object
			payload, _ := frame["payload"].(string)
			if err = json.Unmarshal([]byte(payload), &rpc); err != nil {
				return err
			}
			id, _ := rpc["id"].(string)
			if option := grokAXPermission(rpc); option != "" {
				response := object{"jsonrpc": "2.0", "id": rpc["id"], "result": object{"outcome": object{"outcome": "selected", "optionId": option}}}
				if err = g.send(object{"type": "acp", "payload": string(raw(response))}); err != nil {
					return err
				}
				continue
			}
			if strings.HasPrefix(id, "ax_") && rpc["method"] == nil {
				g.mu.Lock()
				waiter := g.prompts[strings.TrimPrefix(id, "ax_")]
				delete(g.prompts, strings.TrimPrefix(id, "ax_"))
				g.mu.Unlock()
				if waiter != nil {
					var failure error
					if rpc["error"] != nil {
						failure = fmt.Errorf("Grok prompt rejected: %s", raw(rpc["error"]))
					}
					select {
					case waiter <- failure:
					default:
					}
				}
				continue
			}
			key := string(raw(rpc["id"]))
			pendingMu.Lock()
			native, matched := pending[key]
			if matched {
				delete(pending, key)
			}
			pendingMu.Unlock()
			if matched && rpc["error"] == nil {
				result, _ := rpc["result"].(map[string]any)
				if created, ok := result["sessionId"].(string); ok {
					native = created
				}
				g.mu.Lock()
				g.native = native
				permission := g.permission
				g.mu.Unlock()
				if err = bind(native, permission); err != nil {
					return err
				}
			}
			if rpc["method"] == "_x.ai/queue/changed" || rpc["method"] == "x.ai/queue/changed" {
				params, _ := rpc["params"].(map[string]any)
				g.mu.Lock()
				if params["sessionId"] == g.native {
					ids := []any{params["runningPromptId"]}
					entries, _ := params["entries"].([]any)
					for _, entry := range entries {
						if e, ok := entry.(map[string]any); ok {
							ids = append(ids, e["id"])
						}
					}
					for _, id := range ids {
						if id, ok := id.(string); ok {
							if waiter := g.prompts[id]; waiter != nil {
								select {
								case waiter <- nil:
								default:
								}
							}
						}
					}
				}
				g.mu.Unlock()
			}
		}
		if err = grokWrite(client, frame); err != nil {
			return err
		}
	}
}

// Launching AX authorizes its messaging tools, as with Claude's per-launch
// tool allowlist. Never approve another tool, persist a grant, or answer a hook
// that explicitly asks for confirmation.
func grokAXPermission(rpc object) string {
	if rpc["method"] != "session/request_permission" {
		return ""
	}
	params, _ := rpc["params"].(map[string]any)
	if meta, _ := params["_meta"].(map[string]any); len(meta) > 0 {
		return ""
	}
	options, _ := params["options"].([]any)
	var allow string
	known := false
	for _, item := range options {
		option, _ := item.(map[string]any)
		if option["kind"] == "allow_once" {
			allow, _ = option["optionId"].(string)
		}
		meta, _ := option["_meta"].(map[string]any)
		if meta["server_prefix"] != "ax" {
			continue
		}
		for _, tool := range toolSpecs {
			if tool.name != "spawn_agent" && meta["tool_name"] == "ax__"+tool.name {
				known = true
			}
		}
	}
	if known {
		return allow
	}
	return ""
}
