package ax

import (
	"context"
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

//go:embed pi.js
var piExtension []byte

func preparePi(_ context.Context, dir, file, _ string, s Session, args, env []string, _ chan<- error) ([]string, []string, func(), error) {
	noop := func() {}
	if nativeHelp(args) {
		return args, env, noop, nil
	}
	if len(args) == 0 && s.Native != "" {
		resume := s.Native
		if s.NativeFile != "" {
			resume = s.NativeFile
		}
		args = []string{"--session", resume}
	}
	directory := filepath.Join(dir, "extensions")
	if err := privateDir(directory); err != nil {
		return nil, nil, noop, err
	}
	// Publish atomically so a crash or simultaneous launch cannot leave a
	// partially written extension. Existing sessions retain their versioned path.
	path := filepath.Join(directory, "pi_"+digest(string(piExtension))[:16]+".mjs")
	tmp := path + "." + randomID("")
	f, err := privateFile(tmp, syscall.O_CREAT|syscall.O_EXCL|syscall.O_WRONLY)
	if err != nil {
		return nil, nil, noop, err
	}
	defer os.Remove(tmp)
	_, err = f.Write(piExtension)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		return nil, nil, noop, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, noop, err
	}
	guidance := strings.ReplaceAll(instructions, "MCP server", "AX extension")
	guidance += " Pi tools use the ax_ prefix. AX custom messages contain the full peer body; no get_message call is needed."
	options := object{"command": exe, "sessionFile": file, "tools": sessionTools(s), "instructions": guidance}
	env = append(env, "AX_PI="+string(raw(options)))
	return withAXOptions(args, []string{"-e", path}), env, noop, nil
}
