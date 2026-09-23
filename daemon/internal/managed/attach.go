package managed

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

// Attachments: files sent along with a prompt. A managed session takes the Anthropic message format
// on stdin, so images travel as image blocks and other files as text blocks. The cross-session inbox
// of an observed session carries a plain string, so there only text files can go, inlined.

// isImage says whether the attachment can be an image block (the types the API accepts).
func isImage(mediaType string) bool {
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// inlineText renders a non-image attachment as a tagged block the model can tell apart from the prompt.
func inlineText(a model.Attachment) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(a.Data)
	if err != nil {
		return "", fmt.Errorf("attachment %s: %w", a.Name, err)
	}
	body := strings.TrimRight(string(raw), "\n")
	return fmt.Sprintf("<attached-file name=%q>\n%s\n</attached-file>", a.Name, body), nil
}

// ContentBlocks builds a user message's content: one image block per image, one text block per other
// file, then the prompt text. The prompt may be empty when files are all there is to say.
func ContentBlocks(text string, atts []model.Attachment) ([]map[string]any, error) {
	blocks := make([]map[string]any, 0, len(atts)+1)
	for _, a := range atts {
		if isImage(a.MediaType) {
			blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": a.MediaType, "data": a.Data}})
			continue
		}
		s, err := inlineText(a)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, map[string]any{"type": "text", "text": s})
	}
	if strings.TrimSpace(text) != "" || len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	return blocks, nil
}

// InlineAttachments folds text attachments into the prompt for a path that carries only a string.
// Images cannot go that way and are refused rather than silently dropped.
func InlineAttachments(text string, atts []model.Attachment) (string, error) {
	if len(atts) == 0 {
		return text, nil
	}
	parts := make([]string, 0, len(atts)+1)
	for _, a := range atts {
		if isImage(a.MediaType) {
			return "", fmt.Errorf("%s: images can only be sent to sessions started by Vineyard", a.Name)
		}
		s, err := inlineText(a)
		if err != nil {
			return "", err
		}
		parts = append(parts, s)
	}
	if strings.TrimSpace(text) != "" {
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n\n"), nil
}

// SendWithAttachments delivers a prompt plus files to a managed session.
func (m *Manager) SendWithAttachments(sid, text string, atts []model.Attachment) error {
	if len(atts) == 0 {
		return m.Send(sid, text)
	}
	p, err := m.get(sid)
	if err != nil {
		return err
	}
	content, err := ContentBlocks(text, atts)
	if err != nil {
		return err
	}
	return m.write(p, map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": content},
	})
}
