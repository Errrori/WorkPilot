package agent

import "strings"

// Mention is the wake word that triggers the group chat agent.
const Mention = "@WorkPilot"

// IsMention reports whether the message addresses the agent.
func IsMention(content string) bool {
	return strings.Contains(strings.ToLower(content), strings.ToLower(Mention))
}

// StripMention removes the wake word from the instruction.
func StripMention(content string) string {
	idx := strings.Index(strings.ToLower(content), strings.ToLower(Mention))
	if idx < 0 {
		return strings.TrimSpace(content)
	}
	return strings.TrimSpace(content[:idx] + content[idx+len(Mention):])
}
