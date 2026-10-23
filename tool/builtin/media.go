package builtin

import (
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"path/filepath"

	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/tool"
)

// Limits of the media read_file returns. Providers cap images at a few
// megabytes, such as 3.75 MB on Bedrock and 5 MB on Anthropic through
// Vertex AI and Bedrock, and requests at 32 MB or more, which base64
// encoding inflates by a third.
const (
	maxImageBytes = 5 << 20
	maxPDFBytes   = 20 << 20
)

// mediaTypes are the media types read_file returns as parts rather than
// text: the image formats every provider takes, and PDF.
var mediaTypes = map[string]bool{
	"image/png":       true,
	"image/jpeg":      true,
	"image/gif":       true,
	"image/webp":      true,
	"application/pdf": true,
}

// sniffMedia returns the media type of a file whose content starts with
// head if read_file returns it as an image or document, or "".
func sniffMedia(head []byte) string {
	if mt := http.DetectContentType(head); mediaTypes[mt] {
		return mt
	}
	return ""
}

// readMedia returns an image or PDF file, read from r, as a part of the
// result, as Claude Code's Read tool does.
func (e *env) readMedia(name, rel string, st fs.FileInfo, r io.Reader, mediaType string) (*tool.Result, error) {
	kind, limit := "an image", int64(maxImageBytes)
	if mediaType == "application/pdf" {
		kind, limit = "a PDF document", maxPDFBytes
	}
	if st.Size() > limit {
		return tool.Errorf("%s is %s of %s, larger than the %s read_file returns", name, kind, mediaSize(st.Size()), mediaSize(limit)), nil
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return tool.Errorf("%s is %s larger than the %s read_file returns", name, kind, mediaSize(limit)), nil
	}
	e.Files.record(e.abs(rel), st, data)
	slash := filepath.ToSlash(rel)
	label, p := "Image", model.ImagePart(mediaType, data)
	p.Filename = path.Base(slash)
	if mediaType == "application/pdf" {
		label, p = "PDF document", model.DocumentPart(mediaType, data, path.Base(slash))
	}
	return tool.Media(fmt.Sprintf("%s %s (%s, %s)", label, slash, mediaType, mediaSize(int64(len(data)))), p), nil
}

// mediaSize formats a byte count.
func mediaSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}
