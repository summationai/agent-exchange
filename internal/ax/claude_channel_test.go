package ax

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Claude can drop channel opt-in when agent view re-hosts a conversation.
// Exercise the launch boundary for both fresh and resumed conversations.
func TestClaudeLaunchKeepsChannelInForeground(t *testing.T) {
	for _, resume := range []bool{false, true} {
		name := "fresh"
		if resume {
			name = "resumed"
		}
		t.Run(name, func(t *testing.T) {
			dir, bin := startTestServer(t), testDir(t)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			out := filepath.Join(bin, "args")
			script := "#!/bin/sh\nif [ \"$1\" = --help ]; then exit 0; fi\nprintf '%s\\0' \"$@\" > " + shellQuote(out) + "\n"
			if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			args := []string{"-n", "api"}
			if resume {
				args = append(args, "-r", uuid())
			}
			if err := Launch(context.Background(), dir, "claude", args); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			argv := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
			i := slices.Index(argv, "--dangerously-load-development-channels")
			if i < 0 || i+1 >= len(argv) || argv[i+1] != "server:ax" {
				t.Fatal("AX channel was not enabled")
			}
			i = slices.Index(argv, "--settings")
			if i < 0 || i+1 >= len(argv) {
				t.Fatal("missing native settings")
			}
			settings := argv[i+1]
			var config struct {
				DisableAgentView bool                       `json:"disableAgentView"`
				Hooks            map[string]json.RawMessage `json:"hooks"`
			}
			if err := json.Unmarshal([]byte(settings), &config); err != nil {
				t.Fatal(err)
			}
			if !config.DisableAgentView || len(config.Hooks) == 0 {
				t.Fatal("Claude can re-host the session and silently lose the AX channel")
			}
			// Optional native contract check. This read-only command must refuse
			// agent view before it starts a supervisor or a model session.
			if native := os.Getenv("AX_TEST_CLAUDE_BIN"); native != "" {
				ctx, cancel := context.WithTimeout(context.Background(), doctorProbeTimeout)
				defer cancel()
				output, err := exec.CommandContext(ctx, native, "agents", "--json", "--settings", settings).CombinedOutput()
				if ctx.Err() != nil || err == nil || !strings.Contains(string(output), "disabled by the 'disableAgentView' setting") {
					t.Fatalf("native Claude did not refuse re-hosting: %v: %s", err, output)
				}
			}
		})
	}
}
