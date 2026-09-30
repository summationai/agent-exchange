package ax

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const maxSpawnTree = 8
const maxSpawnDepth = 3

type spawnRequest struct {
	Host string   `json:"harness"`
	Name string   `json:"name"`
	CWD  string   `json:"cwd"`
	Args []string `json:"args,omitempty"`
}

type spawnRecord struct {
	Request  spawnRequest    `json:"request"`
	Parent   string          `json:"parent"`
	Root     string          `json:"root"`
	Depth    int             `json:"depth"`
	Terminal terminalContext `json:"terminal"`
	AgentID  string          `json:"agent_id"`
	Token    string          `json:"token"`
	Path     string          `json:"search_path"`
	Phase    string          `json:"phase"`
	Pane     string          `json:"pane,omitempty"`
	Error    string          `json:"error,omitempty"`
}

type spawnResult struct {
	Name       string `json:"name"`
	AgentID    string `json:"agent_id"`
	Harness    string `json:"harness"`
	Terminal   string `json:"terminal"`
	Pane       string `json:"pane,omitempty"`
	Phase      string `json:"phase"`
	AgentState string `json:"agent_state,omitempty"`
	Ready      bool   `json:"ready"`
	Problem    string `json:"problem,omitempty"`
	Next       string `json:"next_action"`
}

func spawnPath(dir, name string) string { return filepath.Join(dir, "spawns", name+".json") }

func readPrivateJSON(path string, out any) error {
	f, err := privateFile(path, syscall.O_RDONLY)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewDecoder(io.LimitReader(f, maxFrame)).Decode(out)
}

func spawnLock(ctx context.Context, dir string) (*os.File, error) {
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		lock, err := lockFile(filepath.Join(dir, "spawn.lock"))
		if err == nil {
			return lock, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("another pane launch is being recorded: %w", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func normalizeSpawn(req spawnRequest, parent Session) (spawnRequest, error) {
	if harnesses[req.Host].nativeID == nil || !validName.MatchString(req.Name) {
		return req, errors.New("choose a supported harness and a unique AX name")
	}
	if harnesses[req.Host].nativeLaunch {
		// Exec would replace the dispatcher before it can record the native exit.
		return req, fmt.Errorf("pane spawning is not supported for %s yet; launch ax %s --name %s in a terminal", req.Host, req.Host, req.Name)
	}
	if _, err := exec.LookPath(req.Host); err != nil {
		return req, fmt.Errorf("install and sign in to %s first", req.Host)
	}
	if req.CWD == "" {
		req.CWD = parent.Workspace
	}
	if !filepath.IsAbs(req.CWD) {
		return req, errors.New("cwd must be an absolute directory path")
	}
	var err error
	req.CWD, err = filepath.EvalSymlinks(req.CWD)
	if err != nil {
		return req, err
	}
	info, err := os.Stat(req.CWD)
	if err != nil || !info.IsDir() {
		return req, errors.New("cwd must be an existing directory")
	}
	if len(req.Args) > 64 || len(raw(req)) > 16<<10 {
		return req, errors.New("native launch arguments exceed the limit")
	}
	for _, arg := range req.Args {
		if strings.ContainsRune(arg, 0) || !utf8.ValidString(arg) {
			return req, errors.New("native arguments must be valid text without NUL")
		}
	}
	name, native, err := launchArgs(append([]string{"--name", req.Name}, req.Args...))
	if err != nil || name != req.Name || !reflect.DeepEqual(native, req.Args) && !(len(native) == 0 && len(req.Args) == 0) {
		return req, errors.New("set the AX name through name; external policy is owner-only, not a native spawn argument")
	}
	if len(req.Args) == 0 {
		req.Args = nil
	}
	return req, nil
}

// A name is the retry key. The durable record is written before terminal I/O;
// a lost terminal response can never lead to an automatic second split.
func spawnAgent(ctx context.Context, dir string, parent Session, req spawnRequest, openPane func(context.Context, terminalContext, string, string, string) (string, error)) (spawnResult, error) {
	req, err := normalizeSpawn(req, parent)
	if err != nil {
		return spawnResult{}, err
	}
	if err = parent.Terminal.validate(); err != nil {
		return spawnResult{}, err
	}
	if !validID.MatchString(parent.SpawnRoot) || parent.SpawnDepth >= maxSpawnDepth {
		return spawnResult{}, errors.New("pane launch depth reached, or this AX session predates spawning support; start a fresh root AX session")
	}
	if err = privateDir(filepath.Join(dir, "spawns")); err != nil {
		return spawnResult{}, err
	}
	if err = privateDir(filepath.Join(dir, "sessions")); err != nil {
		return spawnResult{}, err
	}
	lock, err := spawnLock(ctx, dir)
	if err != nil {
		return spawnResult{}, err
	}
	// Close on every error path; release explicitly before asking the terminal.
	defer lock.Close()
	path := spawnPath(dir, req.Name)
	var record spawnRecord
	err = readPrivateJSON(path, &record)
	if err == nil {
		if record.Parent != parent.ID || record.Root != parent.SpawnRoot || !reflect.DeepEqual(record.Request, req) {
			return spawnResult{}, fmt.Errorf("this name belongs to another launch or request; inspect it with ax spawn-status %s", req.Name)
		}
		if record.Phase != "reserved" {
			lock.Close()
			return spawnStatus(ctx, dir, req.Name)
		}
	} else if !os.IsNotExist(err) {
		return spawnResult{}, err
	}
	file, err := namedSessionPath(filepath.Join(dir, "sessions"), req.Name)
	if err != nil {
		return spawnResult{}, err
	}
	nameLock, err := lockFile(file + ".lock")
	if err != nil {
		return spawnResult{}, fmt.Errorf("agent %q is already running", req.Name)
	}
	defer nameLock.Close()
	if record.Token == "" {
		if _, err = os.Lstat(file); err == nil {
			return spawnResult{}, errors.New("AX name already has a saved conversation; use its normal resume command or choose a new name")
		} else if !os.IsNotExist(err) {
			return spawnResult{}, err
		}
		rootFile := filepath.Join(dir, "spawns", parent.SpawnRoot+".tree")
		var names []string
		if err = readPrivateJSON(rootFile, &names); err != nil && !os.IsNotExist(err) {
			return spawnResult{}, err
		}
		if len(names) >= maxSpawnTree {
			return spawnResult{}, errors.New("this agent tree has used its eight pane launches; start a new root session to launch more")
		}
		if err = savePrivateJSON(rootFile, append(names, req.Name)); err != nil {
			return spawnResult{}, err
		}
		record = spawnRecord{Request: req, Parent: parent.ID, Root: parent.SpawnRoot, Depth: parent.SpawnDepth + 1, Terminal: parent.Terminal, AgentID: randomID("agt_"), Token: randomID("") + randomID(""), Path: os.Getenv("PATH"), Phase: "reserved"}
		if err = savePrivateJSON(path, record); err != nil {
			return spawnResult{}, err
		}
	}
	s, err := loadSession(file)
	if os.IsNotExist(err) {
		mesh, err := meshFor(req.CWD)
		if err != nil {
			return spawnResult{}, err
		}
		s = Session{ID: record.AgentID, Secret: record.Token, Name: req.Name, Host: req.Host, Mesh: mesh, Workspace: req.CWD, SpawnRoot: record.Root, SpawnDepth: record.Depth, SpawnToken: record.Token}
		if err = saveSession(file, s); err != nil {
			return spawnResult{}, err
		}
	} else if err != nil || s.ID != record.AgentID || s.SpawnToken != record.Token {
		return spawnResult{}, errors.New("pane reservation no longer matches the saved AX identity")
	}
	if err = ensureBroker(dir); err != nil {
		return spawnResult{}, err
	}
	c, err := dial(socketPath(dir))
	if err != nil {
		return spawnResult{}, err
	}
	err = c.callContext(ctx, "ax.enroll", s, nil)
	c.close()
	if err != nil {
		return spawnResult{}, err
	}
	if err = ctx.Err(); err != nil {
		return spawnResult{}, err
	}
	record.Phase = "launch_requested"
	if err = savePrivateJSON(path, record); err != nil {
		return spawnResult{}, err
	}
	nameLock.Close()
	lock.Close()
	pane, launchErr := openPane(ctx, record.Terminal, dir, req.Name, record.Token)
	err = updateSpawn(dir, req.Name, func(r *spawnRecord) {
		r.Pane = pane
		if launchErr != nil && r.Phase == "launch_requested" {
			r.Error = launchErr.Error()
		} else if launchErr == nil && r.Phase == "launch_requested" {
			r.Phase = "pane_created"
		}
	})
	if err != nil {
		return spawnResult{}, fmt.Errorf("pane launch outcome is unknown; reuse name %s to inspect it: %w", req.Name, err)
	}
	return spawnStatus(ctx, dir, req.Name)
}

func updateSpawn(dir, name string, update func(*spawnRecord)) error {
	lock, err := spawnLock(context.Background(), dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	var record spawnRecord
	if err = readPrivateJSON(spawnPath(dir, name), &record); err != nil {
		return err
	}
	update(&record)
	return savePrivateJSON(spawnPath(dir, name), record)
}

func spawnStatus(ctx context.Context, dir, name string) (spawnResult, error) {
	var r spawnRecord
	if !validName.MatchString(name) {
		return spawnResult{}, errors.New("invalid AX name")
	}
	if err := readPrivateJSON(spawnPath(dir, name), &r); err != nil {
		return spawnResult{}, err
	}
	result := spawnResult{Name: name, AgentID: r.AgentID, Harness: r.Request.Host, Terminal: r.Terminal.Kind, Pane: r.Pane, Phase: r.Phase, Problem: r.Error,
		Next: "Use send_message with this name to delegate the task through AX. Mail queues until the peer connects. Do not poll or repeat the launch under another name if its outcome is unknown."}
	if r.Phase == "failed" || r.Phase == "exited" {
		result.Next = "The launched session has ended. Inspect the terminal and use the normal AX resume command if needed; retrying spawn will not create another pane."
		return result, nil
	}
	c, err := dial(socketPath(dir))
	if err != nil {
		result.Problem = "Broker unavailable; terminal launch state retained."
		return result, nil
	}
	defer c.close()
	var agents []Agent
	if err = c.callContext(ctx, "ax.inspect", object{}, &agents); err != nil {
		result.Problem = "Broker readiness could not be checked."
		return result, nil
	}
	for _, a := range agents {
		if a.ID == r.AgentID {
			result.AgentState = a.State
			if a.Native != "" {
				result.Phase = "native_started"
			}
			result.Ready = a.Online && nativeID(a.Host, a.Native) && (a.State == "ready" || a.State == "busy")
			if result.Ready {
				result.Phase, result.Problem = "connected", ""
			}
			break
		}
	}
	return result, nil
}

// The terminal starts this short dispatcher; it does not depend on the caller's
// bridge, and uses the same native session launcher as an ordinary ax command.
func runSpawn(ctx context.Context, dir, name, token string) error {
	if !filepath.IsAbs(dir) || !validName.MatchString(name) || len(token) != 64 {
		return errors.New("invalid pane reservation")
	}
	if err := privateDir(dir); err != nil {
		return err
	}
	var record spawnRecord
	if err := readPrivateJSON(spawnPath(dir, name), &record); err != nil {
		return err
	}
	if record.Token != token {
		return errors.New("invalid pane reservation token")
	}
	owner, err := lockFile(spawnPath(dir, name) + ".run.lock")
	if err != nil {
		return errors.New("pane launch already has an owner")
	}
	defer owner.Close()
	if err = readPrivateJSON(spawnPath(dir, name), &record); err != nil {
		return err
	}
	if record.Phase != "launch_requested" && record.Phase != "pane_created" {
		return errors.New("this pane reservation has already been used")
	}
	if err = updateSpawn(dir, name, func(r *spawnRecord) { r.Phase, r.Error = "starting", "" }); err != nil {
		return err
	}
	// Do not forward the calling agent's credentials or implicit bypass setting.
	os.Unsetenv("AX_ALLOW_BYPASS")
	os.Unsetenv("AX_SESSION_FILE")
	os.Unsetenv("CLAUDECODE")
	os.Setenv("AX_HOME", dir)
	os.Setenv("PATH", record.Path)
	err = os.Chdir(record.Request.CWD)
	if err == nil {
		err = launch(ctx, dir, record.Request.Host, append([]string{"--name", name}, record.Request.Args...), token)
	}
	finalErr := updateSpawn(dir, name, func(r *spawnRecord) {
		r.Phase, r.Error = "exited", ""
		if err != nil {
			r.Phase, r.Error = "failed", err.Error()
		}
	})
	return errors.Join(err, finalErr)
}

func spawnCLI(ctx context.Context, dir string, args []string) (spawnResult, error) {
	if len(args) < 1 {
		return spawnResult{}, errors.New("usage: ax spawn HARNESS --name NAME [--cwd DIRECTORY] [native arguments]")
	}
	parent := Session{ID: "cli", Terminal: detectTerminal()}
	parent.SpawnRoot = "tree_" + digest(string(raw(parent.Terminal)))[:32]
	parent.Workspace, _ = os.Getwd()
	if file := os.Getenv("AX_SESSION_FILE"); file != "" {
		var err error
		parent, err = loadSession(file)
		if err != nil {
			return spawnResult{}, err
		}
	}
	req := spawnRequest{Host: args[0]}
	var native []string
	for i := 1; i < len(args); i++ {
		if args[i] == "--" {
			native = append(native, args[i:]...)
			break
		}
		// Unlike -name, a stray -cwd would reach the harness rather than fail name
		// validation, so reject it here instead of forwarding a half-parsed option.
		if args[i] == "-cwd" || strings.HasPrefix(args[i], "-cwd=") {
			return spawnResult{}, errors.New("-cwd is no longer the AX cwd option; use --cwd /absolute/path")
		}
		if args[i] == "--external" || strings.HasPrefix(args[i], "--external=") {
			return spawnResult{}, errors.New("--external is not supported by ax spawn; set the owner policy with ax external NAME POLICY after launch")
		}
		if args[i] == "--cwd" {
			i++
			if i == len(args) {
				return spawnResult{}, errors.New("cwd needs a value")
			}
			req.CWD = args[i]
		} else {
			native = append(native, args[i])
		}
	}
	var err error
	req.Name, req.Args, err = launchArgs(native)
	if err != nil {
		return spawnResult{}, err
	}
	// Manual CLI retries also use the existing reservation's tree.
	if parent.ID == "cli" {
		var old spawnRecord
		if readPrivateJSON(spawnPath(dir, req.Name), &old) == nil && old.Parent == "cli" {
			parent.SpawnRoot = old.Root
		}
	}
	return spawnAgent(ctx, dir, parent, req, openAgentPane)
}
