// Package importer parses external DSH (session.v4.jsonl) session logs into
// conversation rows for a host's import preview/migration. It reads the layout
// directly and never touches attachments.
package importer

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// ErrUnsupportedVersion reports a DSH session header this importer does not
// understand (only version 4 is supported).
var ErrUnsupportedVersion = errors.New("importer: unsupported DSH session version")

const supportedSessionVersion = 4

// sessionHeader is the first JSONL line of a DSH session log.
type sessionHeader struct {
	Type      string `json:"type"`
	Version   int    `json:"version"`
	ID        string `json:"id"`
	CreatedAt int64  `json:"createdAt"`
	Cwd       string `json:"cwd"`
}

// eventEnvelope is one {type, seq, time, data} JSONL line.
type eventEnvelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// contentPart is the text/reasoning subset of a DSH content part; image/file
// parts are counted as dropped (not imported).
type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// userMessageData is the flat data payload of user/message events. Message is
// the nested fallback some exports use.
type userMessageData struct {
	Content []contentPart `json:"content"`
	Role    string        `json:"role"`
	Source  struct {
		Kind string `json:"kind"`
	} `json:"source"`
	Message struct {
		Content []contentPart `json:"content"`
	} `json:"message"`
}

// assistantMessageData is the nested data payload of assistant/message events;
// Content is the flat fallback for hand-written/exported fixtures.
type assistantMessageData struct {
	Turn    int `json:"turn"`
	Message struct {
		Role    string        `json:"role"`
		Content []contentPart `json:"content"`
	} `json:"message"`
	Content []contentPart `json:"content"`
}

// contentParts returns the message content, preferring the nested shape when
// both are present.
func (d userMessageData) contentParts() []contentPart {
	if len(d.Message.Content) > 0 {
		return d.Message.Content
	}
	return d.Content
}

func (d assistantMessageData) contentParts() []contentPart {
	if len(d.Message.Content) > 0 {
		return d.Message.Content
	}
	return d.Content
}

// parsedMessage is one conversation row to import.
type Message struct {
	Role      string
	Text      string
	Reasoning string
}

// parsedSession is one parsable DSH session.
type Session struct {
	ID                 string
	Cwd                string
	CreatedAt          time.Time
	Messages           []Message
	DroppedAttachments int
}

// parseSessionFile parses one uncompressed session.v4.jsonl file.
func ParseSessionFile(path string) (*Session, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("importer: open %s: %w", path, err)
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64*1024)
	session := &Session{}
	headerSeen := false
	lineNo := 0
	for {
		line, readErr := reader.ReadString('\n')
		if len(line) > 0 {
			lineNo++
			trimmed := strings.TrimSpace(line)
			if trimmed != "" {
				if err := parseSessionLine(session, trimmed, headerSeen, path, lineNo); err != nil {
					return nil, err
				}
				headerSeen = true
			}
		}
		if readErr != nil {
			return finishParsedSession(session, headerSeen, path)
		}
	}
}

// finishParsedSession validates the header was present.
func finishParsedSession(session *Session, headerSeen bool, path string) (*Session, error) {
	if !headerSeen {
		return nil, fmt.Errorf("importer: %s: missing session header", path)
	}
	if strings.TrimSpace(session.ID) == "" {
		return nil, fmt.Errorf("importer: %s: session is missing its id", path)
	}
	return session, nil
}

// parseSessionLine parses the header or one event line.
func parseSessionLine(session *Session, line string, headerSeen bool, path string, lineNo int) error {
	if !headerSeen {
		var header sessionHeader
		if err := json.Unmarshal([]byte(line), &header); err != nil {
			return fmt.Errorf("importer: %s:%d: session header: %w", path, lineNo, err)
		}
		if header.Type != "session" {
			return fmt.Errorf("importer: %s:%d: first line is %q, want a session header", path, lineNo, header.Type)
		}
		if header.Version != supportedSessionVersion {
			return fmt.Errorf("%w: %s: header version %d; this build reads version %d",
				ErrUnsupportedVersion, path, header.Version, supportedSessionVersion)
		}
		session.ID = strings.TrimSpace(header.ID)
		session.Cwd = strings.TrimSpace(header.Cwd)
		if header.CreatedAt > 0 {
			session.CreatedAt = time.UnixMilli(header.CreatedAt).UTC()
		}
		return nil
	}

	var envelope eventEnvelope
	if err := json.Unmarshal([]byte(line), &envelope); err != nil {
		return fmt.Errorf("importer: %s:%d: event: %w", path, lineNo, err)
	}
	switch envelope.Type {
	case "user/message":
		var data userMessageData
		if err := json.Unmarshal(envelope.Data, &data); err != nil {
			return fmt.Errorf("importer: %s:%d: user/message: %w", path, lineNo, err)
		}
		// Only genesis user messages are conversation rows; DSH injects most
		// context through user/message events with a different source.kind.
		if !strings.EqualFold(strings.TrimSpace(data.Source.Kind), "user") {
			return nil
		}
		text, _, dropped := flattenContent(data.contentParts())
		session.DroppedAttachments += dropped
		if strings.TrimSpace(text) == "" {
			return nil
		}
		session.Messages = append(session.Messages, Message{Role: "user", Text: text})
	case "assistant/message":
		var data assistantMessageData
		if err := json.Unmarshal(envelope.Data, &data); err != nil {
			return fmt.Errorf("importer: %s:%d: assistant/message: %w", path, lineNo, err)
		}
		text, reasoning, dropped := flattenContent(data.contentParts())
		session.DroppedAttachments += dropped
		// Step-level assistant events only carry reasoning/tool-call parts; only
		// real replies (visible text) become transcript rows.
		if strings.TrimSpace(text) == "" {
			return nil
		}
		session.Messages = append(session.Messages, Message{Role: "assistant", Text: text, Reasoning: reasoning})
	}
	return nil
}

// flattenContent extracts visible text and reasoning from content parts and
// counts image/file parts that are not imported.
func flattenContent(parts []contentPart) (text, reasoning string, dropped int) {
	var textBuilder, reasoningBuilder strings.Builder
	for _, part := range parts {
		kind := strings.ToLower(strings.TrimSpace(part.Type))
		switch {
		case kind == "text":
			textBuilder.WriteString(part.Text)
		case kind == "reasoning":
			reasoningBuilder.WriteString(part.Text)
		case kind == "image", kind == "file", strings.Contains(kind, "image"), strings.Contains(kind, "file"):
			dropped++
		}
	}
	return strings.TrimSpace(textBuilder.String()), strings.TrimSpace(reasoningBuilder.String()), dropped
}
