package mcp

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/nostosx/agentx/internal/media"

	"github.com/nostosx/agentx/model"
)

// The first revisions with audio content and with resource links.
const (
	audioRevision = "2025-03-26"
	linkRevision  = "2025-06-18"
)

// toolContent returns the content of a tool result for a client of
// revision version: the text, then its parts. Inline images and audio
// become image and audio content; other inline media, and audio for
// revisions without audio content, become embedded resources, as text
// when the media type is text and the content valid UTF-8, else as a
// blob. Media referenced by URL becomes a resource link, or text for
// revisions without links; provider file IDs, which a client cannot
// fetch, are described in text.
func toolContent(version string, res *model.ToolResult) []Content {
	var out []Content
	if res.Content != "" || len(res.Parts) == 0 {
		out = append(out, Content{Type: "text", Text: res.Content})
	}
	for i, p := range res.Parts {
		if c, ok := partContent(version, i, p); ok {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		out = append(out, Content{Type: "text"})
	}
	return out
}

// partContent returns the content of the i-th part of a tool result; ok
// is false for parts without a representation, such as reasoning.
func partContent(version string, i int, p model.Part) (c Content, ok bool) {
	if p.Type == model.PartText {
		return Content{Type: "text", Text: p.Text}, true
	}
	if !p.IsMedia() {
		return Content{}, false
	}
	if mt, data, inline := media.InlineData(p); inline {
		b64 := base64.StdEncoding.EncodeToString(data)
		switch {
		case p.Type == model.PartImage:
			return Content{Type: "image", Data: b64, MimeType: mt}, true
		case p.Type == model.PartAudio && version >= audioRevision:
			return Content{Type: "audio", Data: b64, MimeType: mt}, true
		}
		r := &ResourceContents{URI: attachmentURI(i, p), MimeType: mt}
		if strings.HasPrefix(mt, "text/") && utf8.Valid(data) {
			r.Text = string(data)
		} else {
			r.Blob = b64
		}
		return Content{Type: "resource", Resource: r}, true
	}
	if p.URL != "" {
		if version >= linkRevision {
			return Content{Type: "resource_link", URI: p.URL, Name: media.Filename(p), MimeType: media.MediaType(p)}, true
		}
		return Content{Type: "text", Text: fmt.Sprintf("[%s: %s]", p.Type, p.URL)}, true
	}
	if p.FileID != "" {
		return Content{Type: "text", Text: fmt.Sprintf("[%s: file %s of %s]", p.Type, p.FileID, p.Provider)}, true
	}
	return Content{}, false
}

// attachmentURI names the embedded resource of the i-th part of a tool
// result, which has no URI of its own: attachment:///<i>/<file name>.
func attachmentURI(i int, p model.Part) string {
	return fmt.Sprintf("attachment:///%d/%s", i, url.PathEscape(media.Filename(p)))
}
