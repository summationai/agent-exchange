//go:build darwin || linux

package ax

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const doctorProbeTimeout = 3 * time.Second
const doctorOutputLimit = 16 << 10

type doctorOutput struct{ buf bytes.Buffer }

func (b *doctorOutput) Write(p []byte) (int, error) {
	if len(p) > doctorOutputLimit-b.buf.Len() {
		return 0, io.ErrShortBuffer
	}
	return b.buf.Write(p)
}

// Probe only short-lived diagnostic commands. Own their process group so a
// timed-out wrapper cannot leave a child running or affect a coding session.
func doctorProbe(ctx context.Context, host string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, doctorProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, host, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 100 * time.Millisecond
	var stdout, stderr doctorOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	err := cmd.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return nil, err
	}
	return stdout.buf.Bytes(), err
}

func doctor(ctx context.Context, dir string, out io.Writer) {
	fmt.Fprintf(out, "AX version: %s\nAX home: %q\nBroker socket: %q\n", Version, dir, socketPath(dir))
	fmt.Fprintln(out, resourceStatus(dir))
	doctorBroker(ctx, dir, out)
	if path := os.Getenv("AX_SESSION_FILE"); path != "" {
		fmt.Fprintf(out, "Current AX session file: %q\n", path)
		if absolute, err := filepath.Abs(path); err == nil && filepath.Dir(absolute) != filepath.Join(dir, "sessions") {
			fmt.Fprintln(out, "Current AX session: file is outside this AX home's sessions directory; check AX_HOME and AX_SESSION_FILE together")
		}
		f, err := privateFile(path, syscall.O_RDONLY)
		var s Session
		if err == nil {
			err = json.NewDecoder(io.LimitReader(f, doctorOutputLimit)).Decode(&s)
			f.Close()
		}
		if err != nil {
			fmt.Fprintln(out, "Current AX session: unavailable; session file could not be read safely")
		} else {
			fmt.Fprintf(out, "Current AX session: name=%q harness=%q agent=%q native=%q workspace=%q\n", s.Name, s.Host, s.ID, s.Native, s.Workspace)
			if s.BindingError != "" {
				fmt.Fprintln(out, "Current AX session: "+s.BindingError)
			}
		}
	}
	for _, host := range []string{"claude", "codex", "grok", "opencode", "pi"} {
		b, err := doctorProbe(ctx, host, "--version")
		if err != nil {
			fmt.Fprintf(out, "%s: unavailable (missing, failed, or timed out)\n", host)
		} else {
			fmt.Fprintf(out, "%s: %q\n", host, strings.TrimSpace(string(b)))
		}
		if host == "claude" {
			doctorClaudeEnvironment(out, os.Getenv)
			if err == nil {
				// Claude may exit nonzero with valid JSON when logged out.
				status, _ := doctorProbe(ctx, host, "auth", "status")
				doctorClaudeAuth(out, status)
			}
		}
	}
	fmt.Fprintln(out, "Claude Channels: organization policy and feature availability are not verified locally. Team/Enterprise owners must enable Channels in Claude Code admin settings; launch Claude through AX to opt in.")
	fmt.Fprintln(out, "Discovery: compare AX home, socket, and fresh agent lists in each terminal. Different AX_HOME values intentionally isolate agents; repositories do not.")
	fmt.Fprintln(out, "Delivery: queued and acknowledged are reported separately. Doctor does not change privacy settings or restart sessions or the broker.")
}

func doctorBroker(ctx context.Context, dir string, out io.Writer) {
	c, err := dial(socketPath(dir))
	if err != nil {
		fmt.Fprintln(out, "Broker: unreachable; launch a named AX session or run ax agents to start it")
		return
	}
	defer c.close()
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var agents []Agent
	if err := c.callContext(ctx, "ax.inspect", object{}, &agents); err != nil {
		fmt.Fprintln(out, "Broker: connected but agent inspection failed or timed out")
		return
	}
	fmt.Fprintf(out, "Broker: reachable, %d registered agents\n", len(agents))
	for _, a := range agents {
		fmt.Fprintf(out, "  name=%q harness=%q online=%t state=%q agent=%q\n", a.Name, a.Host, a.Online, a.State, a.ID)
		fmt.Fprintf(out, "  tools=%t wake=%q confirmation=%q; verify a round trip with ax verify %s\n", a.Connectivity.Tools, a.Connectivity.Wake, a.Connectivity.Confirmation, a.Name)
		if c := a.Capabilities; c != nil {
			fmt.Fprintf(out, "    delivery: contract=%d content=%s boundary=%s adapter=%s\n", c.Version, c.Content, c.Boundary, c.AdapterVersion)
		} else {
			fmt.Fprintln(out, "    delivery: legacy adapter; capabilities unavailable")
		}
		if a.BindingError != "" {
			fmt.Fprintln(out, "  "+a.BindingError)
		}
	}
}

func doctorClaudeEnvironment(out io.Writer, getenv func(string) string) {
	for _, key := range []string{"DO_NOT_TRACK", "DISABLE_GROWTHBOOK", "DISABLE_TELEMETRY", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"} {
		v := getenv(key)
		enabled := v == "1" || strings.EqualFold(v, "true")
		if key == "DISABLE_TELEMETRY" || key == "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC" {
			enabled = v != ""
		}
		if enabled {
			fmt.Fprintf(out, "Claude Channels: shell setting %s requests disabled feature-flag fetching, which can make Channels unavailable. Claude settings may override it. Review your privacy preference before changing it; AX leaves it untouched.\n", key)
		}
	}
}

func doctorClaudeAuth(out io.Writer, data []byte) {
	var status struct {
		LoggedIn          *bool  `json:"loggedIn"`
		AuthMethod        string `json:"authMethod"`
		APIProvider       string `json:"apiProvider"`
		AnalyticsDisabled *bool  `json:"analyticsDisabled"`
	}
	if json.Unmarshal(data, &status) != nil || status.LoggedIn == nil {
		fmt.Fprintln(out, "Claude auth: unknown (unsupported output, failed, or timed out); check claude auth status locally")
		return
	}
	switch {
	case !*status.LoggedIn:
		fmt.Fprintln(out, "Claude Channels: not logged in; authenticate with Claude.ai or a supported Anthropic Console API key")
	case status.AuthMethod == "" || status.APIProvider == "":
		fmt.Fprintln(out, "Claude auth: logged in; provider eligibility unknown with this status output")
	case status.APIProvider == "bedrock" || status.APIProvider == "vertex" || status.APIProvider == "foundry":
		fmt.Fprintln(out, "Claude Channels: unavailable through Bedrock, Vertex, or Foundry; first-party Anthropic authentication is required")
	case status.APIProvider != "firstParty":
		fmt.Fprintln(out, "Claude auth: logged in; provider eligibility unknown with this status output")
	default:
		fmt.Fprintln(out, "Claude auth: logged in with first-party Anthropic authentication")
	}
	if status.AnalyticsDisabled == nil {
		fmt.Fprintln(out, "Claude feature-flag access: unknown; this auth status does not report analyticsDisabled")
	} else if *status.AnalyticsDisabled {
		fmt.Fprintln(out, "Claude Channels: effective analyticsDisabled=true; privacy or provider settings can prevent feature-flag fetching. Check Claude settings as well as shell variables; AX leaves them untouched.")
	} else {
		fmt.Fprintln(out, "Claude analytics: enabled; this alone does not prove Channels are available")
	}
}
