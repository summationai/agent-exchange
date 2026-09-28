package ax

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// A harness owns session selection and native wake delivery. The broker and
// messaging tools remain shared, independent of the harness's implementation.
type harnessAdapter struct {
	nativeLaunch bool
	toolPrefix   string
	nativeID     func(string) bool
	// Native option that sets the harness's own session display name, when it has
	// one. AX passes its name through so the agent is identifiable in native UI.
	nameFlag         string
	readyOnDiscovery bool
	prepare          func(context.Context, string, string, string, Session, []string, []string, chan<- error) ([]string, []string, func(), error)
}

var harnesses = map[string]harnessAdapter{
	"claude":   {toolPrefix: "mcp__ax__", nativeID: validNative, nameFlag: "--name", readyOnDiscovery: true},
	"codex":    {toolPrefix: "ax.", nativeID: validNative, readyOnDiscovery: true},
	"grok":     {toolPrefix: "ax__", nativeID: validNative, readyOnDiscovery: true},
	"opencode": {toolPrefix: "ax_", nativeID: regexp.MustCompile(`^ses_[a-zA-Z0-9]+$`).MatchString, readyOnDiscovery: true},
	"pi":       {toolPrefix: "ax_", nativeID: validNative, nameFlag: "--name", nativeLaunch: true},
	"external": {toolPrefix: "ax.", nativeID: validID.MatchString, readyOnDiscovery: true},
}

func init() {
	grok, opencode := harnesses["grok"], harnesses["opencode"]
	grok.prepare, opencode.prepare = prepareGrok, prepareOpenCode
	harnesses["grok"], harnesses["opencode"] = grok, opencode
	pi := harnesses["pi"]
	pi.prepare = preparePi
	harnesses["pi"] = pi
}

func nativeID(host, id string) bool {
	a, ok := harnesses[host]
	return ok && a.nativeID(id)
}

func toolName(host, name string) string { return harnesses[host].toolPrefix + name }

func bindAdapter(dir, file, native, permission, state string) error {
	s, err := loadSession(file)
	if err != nil {
		return err
	}
	if !nativeID(s.Host, native) {
		return errors.New("invalid native session ID")
	}
	if s.Native != "" && s.Native != native {
		return fmt.Errorf("AX name %q belongs to conversation %s; choose a new AX name for %s", s.Name, s.Native, native)
	}
	s.Native, s.Started = native, true
	if err = saveSession(file, s); err != nil {
		return err
	}
	c, err := dial(socketPath(dir))
	if err != nil {
		return err
	}
	defer c.close()
	return c.call("ax.lifecycle", object{"agent_id": s.ID, "secret": s.Secret, "native_session_id": native, "permission_mode": permission, "state": state}, nil)
}

// The adapter socket is private to one launch. A native plugin may wait for a
// wake here, or a Go adapter can supply its own native ingress implementation.
type adapterWake struct {
	ID     string `json:"id"`
	Native string `json:"native"`
	Text   string `json:"text"`
	done   chan error
}

// The native adapter proved that it did not submit the wake. This is the only
// error safe to defer; timeouts and ambiguous native responses never replay.
type deliveryDeferred struct{ State string }

func (e *deliveryDeferred) Error() string {
	return "native session became " + e.State + " before handoff"
}

type adapterHost struct {
	mu      sync.Mutex
	pending map[string]*adapterWake
	wakes   chan *adapterWake
	wake    func(context.Context, string, string) error
}

func startAdapterHost(ctx context.Context, dir, file string, failures chan<- error, wake func(context.Context, string, string) error) (string, func(), error) {
	path := filepath.Join(dir, randomID("adapter_")+".sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		return "", nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		l.Close()
		return "", nil, err
	}
	server := adapterServer(ctx, dir, file, failures, wake)
	go server.Serve(limitListener(l, 16))
	return path, func() { server.Close(); os.Remove(path) }, nil
}

func adapterServer(ctx context.Context, dir, file string, failures chan<- error, wake func(context.Context, string, string) error) *http.Server {
	a := &adapterHost{pending: map[string]*adapterWake{}, wakes: make(chan *adapterWake), wake: wake}
	wakeSlots := make(chan struct{}, 8)
	mux := http.NewServeMux()
	mux.HandleFunc("/bind", func(w http.ResponseWriter, r *http.Request) {
		var input struct{ Native, Permission, State string }
		err := json.NewDecoder(io.LimitReader(r.Body, maxFrame)).Decode(&input)
		if err == nil {
			err = bindAdapter(dir, file, input.Native, input.Permission, input.State)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			select {
			case failures <- err:
			default:
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/wake", func(w http.ResponseWriter, r *http.Request) {
		select {
		case wakeSlots <- struct{}{}:
			defer func() { <-wakeSlots }()
		default:
			http.Error(w, "AX wake capacity reached", http.StatusServiceUnavailable)
			return
		}
		wakeCtx, stop := context.WithTimeout(r.Context(), 15*time.Second)
		defer stop()
		r = r.WithContext(wakeCtx)
		var input adapterWake
		err := json.NewDecoder(io.LimitReader(r.Body, maxFrame)).Decode(&input)
		s, loadErr := loadSession(file)
		if err != nil || loadErr != nil || !s.Started || input.Native != s.Native {
			http.Error(w, "adapter session is not ready", http.StatusConflict)
			return
		}
		if a.wake != nil {
			err = a.wake(r.Context(), input.Native, input.Text)
		} else {
			input.ID, input.done = randomID("wake_"), make(chan error, 1)
			a.mu.Lock()
			a.pending[input.ID] = &input
			a.mu.Unlock()
			defer func() { a.mu.Lock(); delete(a.pending, input.ID); a.mu.Unlock() }()
			select {
			case a.wakes <- &input:
				select {
				case err = <-input.done:
				case <-r.Context().Done():
					err = r.Context().Err()
				}
			case <-r.Context().Done():
				err = r.Context().Err()
			}
		}
		if err != nil {
			var deferred *deliveryDeferred
			if errors.As(err, &deferred) {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(object{"deferred_state": deferred.State})
				return
			}
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/next", func(w http.ResponseWriter, r *http.Request) {
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case wake := <-a.wakes:
			json.NewEncoder(w).Encode(wake)
		case <-r.Context().Done():
		case <-timer.C:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("/receipt", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			ID, Error     string
			DeferredState string `json:"deferred_state"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, maxFrame)).Decode(&input) != nil {
			http.Error(w, "invalid receipt", 400)
			return
		}
		a.mu.Lock()
		pending := a.pending[input.ID]
		a.mu.Unlock()
		if pending != nil {
			var err error
			if input.Error != "" {
				err = errors.New(input.Error)
			}
			if input.DeferredState == "busy" || input.DeferredState == "blocked" {
				err = &deliveryDeferred{State: input.DeferredState}
			}
			select {
			case pending.done <- err:
			default:
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192, BaseContext: func(net.Listener) context.Context { return ctx }}
}

func notifyAdapter(ctx context.Context, s Session, text string) error {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", s.AdapterSocket)
	}}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, "POST", "http://ax/wake", bytes.NewReader(raw(adapterWake{Native: s.Native, Text: text})))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusConflict {
		var result struct {
			State string `json:"deferred_state"`
		}
		if json.NewDecoder(io.LimitReader(res.Body, 1024)).Decode(&result) == nil && (result.State == "busy" || result.State == "blocked") {
			return &deliveryDeferred{State: result.State}
		}
	}
	if res.StatusCode != http.StatusNoContent {
		return fmt.Errorf("native wake was not confirmed: %s", res.Status)
	}
	return nil
}
