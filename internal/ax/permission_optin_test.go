//go:build darwin || linux

package ax

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBypassOptInSurvivesNamedResume(t *testing.T) {
	for _, surface := range []string{"launch", "attach"} {
		t.Run(surface, func(t *testing.T) {
			dir := startTestServer(t)
			bin := testDir(t)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			snapshot := filepath.Join(bin, "session.json")
			script := "#!/bin/sh\numask 077\nif [ \"$1\" = --help ]; then exit 0; fi\ncat \"$AX_SESSION_FILE\" > " + shellQuote(snapshot) + "\n"
			if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("AX_ALLOW_BYPASS", "")
			var original string
			for _, step := range []struct {
				env  *string
				want bool
			}{
				{nil, false}, {stringPointer("1"), true}, {nil, true},
				{stringPointer("0"), false}, {nil, false},
			} {
				if step.env == nil {
					if err := os.Unsetenv("AX_ALLOW_BYPASS"); err != nil {
						t.Fatal(err)
					}
				} else {
					t.Setenv("AX_ALLOW_BYPASS", *step.env)
				}
				var saved Session
				var err error
				if surface == "launch" {
					if err = Launch(context.Background(), dir, "claude", []string{"-n", "api"}); err != nil {
						t.Fatal(err)
					}
					saved, err = loadSession(snapshot)
				} else {
					var release func()
					saved, _, release, err = attachedSession(dir, "api", "conversation", "manual", "")
					release()
				}
				if err != nil {
					t.Fatal(err)
				}
				if saved.AllowBypass != step.want {
					t.Fatalf("env=%v: allow_bypass=%v want=%v", step.env, saved.AllowBypass, step.want)
				}
				if original != "" && saved.ID != original {
					t.Fatal("resume changed identity")
				}
				original = saved.ID
			}
		})
	}
}

func stringPointer(value string) *string { return &value }
