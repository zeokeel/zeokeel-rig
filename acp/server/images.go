package server

// ACP prompt content blocks: text concatenation and image-block decoding into
// chat attachments. Each image must carry an image/* mime type and a non-empty,
// decodable base64 payload no larger than MaxPromptImageBytes; violations are
// -32602 invalid params.

import (
	"encoding/base64"
	"fmt"
	"strings"

	rigacp "github.com/zeokeel/zeokeel-rig/acp"
)

// contentBlock is one ACP prompt content block. Text blocks carry Text; image
// blocks carry the base64 Data plus the MimeType of ACP's image block shape.
type contentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

// promptBlocks returns the block list of a prompt request. Content is accepted
// as a fallback for clients that use it instead of prompt.
func promptBlocks(params promptParams) []contentBlock {
	if len(params.Prompt) > 0 {
		return params.Prompt
	}
	return params.Content
}

// buildPromptRequest turns ACP content blocks into a PromptRequest: text blocks
// are concatenated (then trimmed), image blocks are validated and decoded.
func buildPromptRequest(params promptParams) (PromptRequest, *rigacp.Error) {
	var (
		text        strings.Builder
		attachments []Attachment
	)
	for index, block := range promptBlocks(params) {
		switch strings.ToLower(strings.TrimSpace(block.Type)) {
		case "text":
			text.WriteString(block.Text)
		case "image":
			attachment, rpcErr := decodeImageBlock(block, index+1, len(attachments)+1)
			if rpcErr != nil {
				return PromptRequest{}, rpcErr
			}
			attachments = append(attachments, attachment)
		}
	}
	return PromptRequest{Text: strings.TrimSpace(text.String()), Attachments: attachments}, nil
}

// decodeImageBlock validates and decodes one ACP image content block.
func decodeImageBlock(block contentBlock, position, ordinal int) (Attachment, *rigacp.Error) {
	mimeType := strings.TrimSpace(block.MimeType)
	if mimeType == "" {
		return Attachment{}, invalidParams(fmt.Sprintf(
			"session/prompt: image block %d: mimeType is required", position))
	}
	if !strings.HasPrefix(strings.ToLower(mimeType), "image/") {
		return Attachment{}, invalidParams(fmt.Sprintf(
			"session/prompt: image block %d: mimeType %q must be an image/* type", position, mimeType))
	}
	data := strings.TrimSpace(block.Data)
	if data == "" {
		return Attachment{}, invalidParams(fmt.Sprintf(
			"session/prompt: image block %d: data (base64) is required", position))
	}
	decoded, err := decodePromptImage(data)
	if err != nil {
		return Attachment{}, invalidParams(fmt.Sprintf(
			"session/prompt: image block %d: invalid base64 data: %v", position, err))
	}
	if len(decoded) == 0 {
		return Attachment{}, invalidParams(fmt.Sprintf(
			"session/prompt: image block %d: decoded image data is empty", position))
	}
	if len(decoded) > MaxPromptImageBytes {
		return Attachment{}, invalidParams(fmt.Sprintf(
			"session/prompt: image block %d: decoded image is %d bytes, over the %d MiB limit",
			position, len(decoded), MaxPromptImageBytes>>20))
	}
	return Attachment{
		ID:   fmt.Sprintf("acp-image-%d", ordinal),
		Name: fmt.Sprintf("image-%d.%s", ordinal, imageExtension(mimeType)),
		MIME: mimeType,
		Data: decoded,
	}, nil
}

// decodePromptImage decodes ACP image data as standard base64, tolerating the
// unpadded encoding some clients emit.
func decodePromptImage(data string) ([]byte, error) {
	if decoded, err := base64.StdEncoding.DecodeString(data); err == nil {
		return decoded, nil
	}
	return base64.RawStdEncoding.DecodeString(data)
}

// imageExtension derives a safe file-name suffix from an image MIME type: the
// subtype without parameters or structured-syntax suffixes ("image/png" ->
// "png", "image/svg+xml" -> "svg"), else "img".
func imageExtension(mimeType string) string {
	subtype := strings.ToLower(strings.TrimSpace(mimeType))
	if slash := strings.IndexByte(subtype, '/'); slash >= 0 {
		subtype = subtype[slash+1:]
	}
	if plus := strings.IndexByte(subtype, '+'); plus >= 0 {
		subtype = subtype[:plus]
	}
	var builder strings.Builder
	for _, char := range subtype {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			builder.WriteRune(char)
		}
	}
	if builder.Len() == 0 {
		return "img"
	}
	return builder.String()
}
