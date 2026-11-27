package ax

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Captured by the session launcher, never inferred from the broker's terminal.
type terminalContext struct {
	Kind   string `json:"kind,omitempty"`
	Pane   string `json:"pane,omitempty"`
	Socket string `json:"socket,omitempty"`
	Bin    string `json:"bin,omitempty"`
}

var tmuxPane = regexp.MustCompile(`^%[0-9]+$`)
var itermPane = regexp.MustCompile(`^[0-9A-Fa-f-]{36}$`)
var rexBlock = regexp.MustCompile(`^block:[0-9a-f-]{36}$`)

func detectTerminal() terminalContext {
	if tmux := os.Getenv("TMUX"); tmux != "" {
		// Socket paths can contain commas; the last two fields are server PID and index.
		parts := strings.Split(tmux, ",")
		if len(parts) >= 3 && tmuxPane.MatchString(os.Getenv("TMUX_PANE")) {
			return terminalContext{Kind: "tmux", Pane: os.Getenv("TMUX_PANE"), Socket: strings.Join(parts[:len(parts)-2], ",")}
		}
		return terminalContext{} // Never fall back to iTerm when tmux targeting is incomplete.
	}
	if block := os.Getenv("REX_BLOCK"); block != "" {
		// The Rex CLI is not always on PATH, so the launcher keeps its location for later splits.
		server, dir := os.Getenv("REX_SERVER"), os.Getenv("REX_BIN_DIR")
		if rexBlock.MatchString(block) && server != "" && filepath.IsAbs(dir) {
			return terminalContext{Kind: "rex", Pane: block, Socket: server, Bin: filepath.Join(dir, "rex")}
		}
		return terminalContext{}
	}
	if runtime.GOOS == "darwin" && os.Getenv("TERM_PROGRAM") == "iTerm.app" {
		id := os.Getenv("ITERM_SESSION_ID")
		if i := strings.LastIndex(id, ":"); i >= 0 {
			id = id[i+1:]
		}
		if itermPane.MatchString(id) {
			return terminalContext{Kind: "iterm", Pane: id}
		}
	}
	return terminalContext{}
}

func (t terminalContext) validate() error {
	switch {
	case t.Kind == "tmux" && tmuxPane.MatchString(t.Pane) && strings.HasPrefix(t.Socket, "/"):
		_, err := exec.LookPath("tmux")
		return err
	case t.Kind == "iterm" && runtime.GOOS == "darwin" && itermPane.MatchString(t.Pane):
		return nil
	case t.Kind == "rex" && rexBlock.MatchString(t.Pane) && t.Socket != "" && filepath.IsAbs(t.Bin):
		_, err := exec.LookPath(t.Bin)
		return err
	default:
		return errors.New("pane launch needs the caller's tmux, Rex, or iTerm2 session; relaunch AX inside one of those terminals")
	}
}

// Arguments go to a fixed script as data. Never use the focused window/session.
const splitITerm = `on run argv
  tell application "iTerm2"
    repeat with w in windows
      repeat with t in tabs of w
        repeat with s in sessions of t
          if (unique ID of s) is (item 1 of argv) then
            tell s
              set child to split vertically with default profile command (item 2 of argv)
            end tell
            return unique ID of child
          end if
        end repeat
      end repeat
    end repeat
  end tell
  error "The calling iTerm2 session no longer exists"
end run`

func openAgentPane(ctx context.Context, terminal terminalContext, dir, name, token string) (string, error) {
	if err := terminal.validate(); err != nil {
		return "", err
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	// Only the AX executable and reservation locator enter the terminal command.
	// Native arguments and task text never cross its shell/parser boundary.
	command := "exec " + shellQuote(exe) + " spawn-run " + shellQuote(dir) + " " + shellQuote(name) + " " + shellQuote(token)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	switch terminal.Kind {
	case "tmux":
		cmd = exec.CommandContext(ctx, "tmux", "-S", terminal.Socket, "split-window", "-h", "-d", "-P", "-F", "#{pane_id}", "-t", terminal.Pane, command)
	case "rex":
		// Rex starts the argument list in the login shell's environment without parsing it.
		// With autostart off, a missing server is an error. Rex finds blocks only in the caller's
		// session, taken from REX_BLOCK or REX_SESSION, so REX_BLOCK names the captured block.
		cmd = exec.CommandContext(ctx, terminal.Bin, "split", "--autostart=false", "-S", terminal.Socket, "-b", terminal.Pane, "--split", "right", "--focus=false", "--json", "--", exe, "spawn-run", dir, name, token)
		cmd.Env = append(os.Environ(), "REX_BLOCK="+terminal.Pane, "REX_SESSION=")
	default:
		cmd = exec.CommandContext(ctx, "/usr/bin/osascript", "-", terminal.Pane, "/bin/sh -c "+shellQuote(command))
		cmd.Stdin = strings.NewReader(splitITerm)
	}
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("terminal launch was not confirmed: %w (%s); inspect the terminal before taking further action", err, strings.TrimSpace(stderr.String()))
	}
	pane := strings.TrimSpace(string(output))
	if terminal.Kind == "rex" {
		var created struct {
			BlockIDs []string `json:"block_ids"`
		}
		pane = ""
		if json.Unmarshal(output, &created) == nil && len(created.BlockIDs) == 1 {
			pane = created.BlockIDs[0]
		}
	}
	if (terminal.Kind == "tmux" && !tmuxPane.MatchString(pane)) || (terminal.Kind == "iterm" && !itermPane.MatchString(pane)) || (terminal.Kind == "rex" && !rexBlock.MatchString(pane)) {
		return "", errors.New("terminal returned an unrecognized pane ID; the launch may have succeeded")
	}
	return pane, nil
}
