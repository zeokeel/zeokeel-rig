package importer

// Host-agnostic helpers for turning parsed messages into a session title and
// rig messages.

import (
	"strings"

	"github.com/zeokeel/zeokeel-rig"
)

const (
	// defaultImportedTitle is used when a session has no user message.
	defaultImportedTitle = "导入的会话"
	// titleRunes bounds an imported session title.
	titleRunes = 60
)

func Title(messages []Message) string {
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		text := strings.TrimSpace(message.Text)
		if text == "" {
			continue
		}
		runes := []rune(text)
		if len(runes) > titleRunes {
			runes = runes[:titleRunes]
		}
		return string(runes)
	}
	return defaultImportedTitle
}

// importedRigMessages rebuilds the durable rig history.
func Messages(messages []Message) []rig.Message {
	out := make([]rig.Message, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case "user":
			out = append(out, rig.NewMessage(rig.RoleUser, message.Text))
		case "assistant":
			assistant := rig.NewMessage(rig.RoleAssistant, message.Text)
			assistant.Reasoning = message.Reasoning
			out = append(out, assistant)
		}
	}
	return out
}
