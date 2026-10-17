package ax

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

//go:embed opencode.js
var openCodePlugin []byte

func prepareOpenCode(ctx context.Context, dir, file, bin string, s Session, args, env []string, failures chan<- error) ([]string, []string, func(), error) {
	if nativeHelp(args) {
		return args, env, func() {}, nil
	}
	for _, arg := range args {
		if arg == "--pure" || arg == "--mini" {
			return nil, nil, nil, fmt.Errorf("AX needs OpenCode's full TUI with plugins enabled")
		}
	}
	if len(args) == 0 && s.Native != "" {
		args = []string{"--session", s.Native}
	}
	socket, stop, err := startAdapterHost(ctx, dir, file, failures, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	pluginDir, err := os.MkdirTemp(dir, "opencode_")
	if err != nil {
		stop()
		return nil, nil, nil, err
	}
	cleanup := func() { stop(); os.RemoveAll(pluginDir) }
	fail := func(err error) ([]string, []string, func(), error) { cleanup(); return nil, nil, nil, err }
	plugin := filepath.Join(pluginDir, "ax.js")
	if err = os.WriteFile(plugin, openCodePlugin, 0600); err != nil {
		return fail(err)
	}
	tui := object{}
	tuiBase := ""
	if path := os.Getenv("OPENCODE_TUI_CONFIG"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return fail(err)
		}
		if err = json.Unmarshal(data, &tui); err != nil {
			return fail(fmt.Errorf("AX needs a JSON OPENCODE_TUI_CONFIG: %w", err))
		}
		tuiBase, err = filepath.Abs(filepath.Dir(path))
		if err != nil {
			return fail(err)
		}
	}
	tui = openCodeTUI(tui, tuiBase, plugin)
	tuiPath := filepath.Join(pluginDir, "tui.json")
	if err = os.WriteFile(tuiPath, raw(tui), 0600); err != nil {
		return fail(err)
	}
	config := object{}
	if content := os.Getenv("OPENCODE_CONFIG_CONTENT"); content != "" {
		if err = json.Unmarshal([]byte(content), &config); err != nil {
			return fail(err)
		}
	}
	mcp, _ := config["mcp"].(map[string]any)
	if mcp == nil {
		mcp = object{}
	}
	exe, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	mcp["ax"] = object{"type": "local", "command": []string{exe, "bridge"}, "environment": object{"AX_HOME": dir, "AX_SESSION_FILE": file, "AX_LAUNCH_ID": s.LaunchID}, "enabled": true}
	config["mcp"] = mcp
	s.AdapterSocket = socket
	if err = saveSession(file, s); err != nil {
		return fail(err)
	}
	options := object{"socket": socket}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if arg == "--continue" || arg == "-c" || arg == "--fork" {
			options["waitForSession"] = true
		}
		if arg == "--auto" || arg == "--yolo" || arg == "--dangerously-skip-permissions" {
			options["auto"] = true
		}
		key, value, hasValue := strings.Cut(arg, "=")
		if key == "--session" || key == "-s" || key == "--prompt" {
			options["waitForSession"] = true
		}
		if key != "--model" && key != "-m" && key != "--agent" {
			continue
		}
		if !hasValue && i+1 < len(args) {
			i++
			value = args[i]
		}
		if key == "--agent" {
			options["agent"] = value
		} else {
			options["model"] = value
		}
	}
	env = append(env, "OPENCODE_CONFIG_CONTENT="+string(raw(config)), "OPENCODE_TUI_CONFIG="+tuiPath, "AX_OPENCODE="+string(raw(options)))
	return args, env, cleanup, nil
}

func openCodeTUI(config object, base, plugin string) object {
	// Match OpenCode's supported nested form, with top-level settings winning.
	if nested, ok := config["tui"].(map[string]any); ok {
		for key, value := range nested {
			if _, exists := config[key]; !exists {
				config[key] = value
			}
		}
	}
	delete(config, "tui")
	plugins, _ := config["plugin"].([]any)
	for i, entry := range plugins {
		value := entry
		if tuple, ok := entry.([]any); ok && len(tuple) > 0 {
			value = tuple[0]
		}
		spec, ok := value.(string)
		if !ok || (!strings.HasPrefix(spec, ".") && !filepath.IsAbs(spec)) {
			continue
		}
		if !filepath.IsAbs(spec) {
			spec = filepath.Join(base, spec)
		}
		resolved := (&url.URL{Scheme: "file", Path: spec}).String()
		if tuple, ok := entry.([]any); ok {
			tuple[0] = resolved
		} else {
			plugins[i] = resolved
		}
	}
	config["plugin"] = append(plugins, (&url.URL{Scheme: "file", Path: plugin}).String())
	return config
}

func nativeHelp(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--help" || arg == "-h" || arg == "--version" {
			return true
		}
	}
	return false
}
