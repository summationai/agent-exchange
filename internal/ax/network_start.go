//go:build darwin || linux

package ax

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func networkAutoPath(dir string) string {
	return filepath.Join(dir, "network", "auto.json")
}

func networkReadySocket(dir string) string {
	return filepath.Join(dir, "net.sock")
}

func enableNetworkAdapter(ctx context.Context, dir string) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	if err := startNetworkAdapter(ctx, dir); err != nil {
		return err
	}
	return savePrivateJSON(networkAutoPath(dir), true)
}

func resumeNetworkAdapter(ctx context.Context, dir string) {
	var enabled bool
	if err := readNetworkJSON(networkAutoPath(dir), &enabled); err != nil || !enabled {
		return
	}
	if networkAdapterReserved(dir) {
		return
	}
	if err := startNetworkAdapter(ctx, dir); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "AX network:", err)
	}
}

func startNetworkAdapter(ctx context.Context, dir string) error {
	// A cold start includes the Tailscale probe and both broker startup windows.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if networkAdapterReady(dir) {
		return ensureBroker(dir)
	}
	if _, _, err := loadNetworkIdentity(dir); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := openDiagnosticLog(dir, "network.log")
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(exe, "network", "serve")
	cmd.Env = append(os.Environ(), "AX_HOME="+dir)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	exited := make(chan error, 1)
	go func() {
		exited <- cmd.Wait()
		close(done)
	}()
	started := false
	defer func() {
		if started {
			return
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var childError error
	var grace time.Time
	for {
		select {
		case <-ctx.Done():
			if !grace.IsZero() && ctx.Err() == context.DeadlineExceeded {
				return networkStartFailure(logFile.Name(), childError)
			}
			return fmt.Errorf("AX network listener did not start; check for an older listener and inspect %s: %w", logFile.Name(), ctx.Err())
		case childError = <-exited:
			exited = nil
			// Allow a concurrent starter to publish its reservation after
			// this child loses the attempt to bind the Tailscale address.
			grace = time.Now().Add(500 * time.Millisecond)
		case <-ticker.C:
		}
		if networkAdapterReady(dir) {
			started = true
			return nil
		}
		if !grace.IsZero() && time.Now().After(grace) && !networkAdapterReserved(dir) {
			return networkStartFailure(logFile.Name(), childError)
		}
	}
}

func networkStartFailure(logPath string, err error) error {
	return fmt.Errorf("AX network listener exited during startup (%v); inspect %s", err, logPath)
}

func networkAdapterReserved(dir string) bool {
	c, err := net.DialTimeout("unix", networkReadySocket(dir), 100*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func networkAdapterReady(dir string) bool {
	c, err := net.DialTimeout("unix", networkReadySocket(dir), 100*time.Millisecond)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	var ready [1]byte
	_, err = io.ReadFull(c, ready[:])
	return err == nil && ready[0] == 1
}

// This private socket confirms the adapter bound its Tailscale address,
// independently of whether the host can route to its own Tailscale IP.
func acceptNetworkReadiness(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		_, _ = c.Write([]byte{1})
		c.Close()
	}
}
