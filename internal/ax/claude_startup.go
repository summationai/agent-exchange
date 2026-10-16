package ax

import (
	"slices"
	"strings"
)

const claudeStartupPrompt = "Initialize AX messaging: call mcp__ax__list_agents once, then finish this turn. AX delivers peer tasks through its channel. Do not poll, sleep waiting, launch another agent, or replay earlier tasks."

// Only seed option-only pane launches. Preserve native prompts, resume pickers,
// commands and unfamiliar syntax rather than guessing where another prompt fits.
func claudeNeedsStartup(args []string) bool {
	for len(args) > 0 {
		flag, _, equal := strings.Cut(args[0], "=")
		args = args[1:]
		switch {
		case flag == "--":
			return len(args) == 0
		case slices.Contains(strings.Fields("--model --effort --permission-mode --settings --setting-sources --append-system-prompt --append-system-prompt-file --system-prompt --system-prompt-file --fallback-model --autocompact --agent --agents --plugin-dir --plugin-url --session-id"), flag):
			if !equal {
				if len(args) == 0 {
					return false
				}
				args = args[1:]
			}
		case slices.Contains(strings.Fields("--mcp-config --add-dir --allowedTools --allowed-tools --disallowedTools --disallowed-tools --tools --betas"), flag):
			if !equal {
				for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
					args = args[1:]
				}
			}
		case slices.Contains(strings.Fields("--dangerously-skip-permissions --allow-dangerously-skip-permissions --no-chrome --chrome --ide --disable-slash-commands --brief --verbose --exclude-dynamic-system-prompt-sections --restricted --ax-screen-reader"), flag):
		default:
			return false
		}
	}
	return true
}
