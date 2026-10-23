package media

import (
	"cmp"
	"net/http"
	"path"
	"strings"

	"github.com/nostosx/kitx/dataurl"

	"github.com/nostosx/agentx/model"
)

// extensionTypes maps file extensions to media types. It is used instead
// of mime.TypeByExtension, whose table depends on the system.
var extensionTypes = map[string]string{
	"png":  "image/png",
	"jpg":  "image/jpeg",
	"jpeg": "image/jpeg",
	"gif":  "image/gif",
	"webp": "image/webp",
	"heic": "image/heic",
	"heif": "image/heif",
	"bmp":  "image/bmp",
	"pdf":  "application/pdf",
	"txt":  "text/plain",
	"md":   "text/markdown",
	"html": "text/html",
	"htm":  "text/html",
	"csv":  "text/csv",
	"xml":  "text/xml",
	"json": "application/json",
	"doc":  "application/msword",
	"docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"xls":  "application/vnd.ms-excel",
	"xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"wav":  "audio/wav",
	"mp3":  "audio/mpeg",
	"aac":  "audio/aac",
	"ogg":  "audio/ogg",
	"oga":  "audio/ogg",
	"opus": "audio/opus",
	"flac": "audio/flac",
	"m4a":  "audio/mp4",
	"aiff": "audio/aiff",
	"mp4":  "video/mp4",
	"m4v":  "video/mp4",
	"mov":  "video/quicktime",
	"mpeg": "video/mpeg",
	"mpg":  "video/mpeg",
	"avi":  "video/x-msvideo",
	"flv":  "video/x-flv",
	"wmv":  "video/x-ms-wmv",
	"webm": "video/webm",
	"mkv":  "video/x-matroska",
	"3gp":  "video/3gpp",
}

// ExtensionType returns the media type of a file name or URL path by its
// extension, or "" for unknown extensions.
func ExtensionType(name string) string {
	if i := strings.IndexAny(name, "?#"); i >= 0 && strings.Contains(name, "://") {
		name = name[:i]
	}
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(name)), ".")
	return extensionTypes[ext]
}

// BaseType returns a media type in lower case without parameters, so
// "Audio/L16; rate=24000" becomes "audio/l16".
func BaseType(mediaType string) string {
	mt, _, _ := strings.Cut(mediaType, ";")
	return strings.ToLower(strings.TrimSpace(mt))
}

// InlineData returns the content of a media part held inline, in Data or a
// data: URL, with its media type in lower case without parameters. The
// type is the part's MediaType, else that of the data: URL, else detected
// from the content. ok is false for parts referenced by URL or file ID.
func InlineData(p model.Part) (mediaType string, data []byte, ok bool) {
	data, mediaType = p.Data, p.MediaType
	if len(data) == 0 {
		if p.URL == "" {
			return "", nil, false
		}
		mt, d, err := dataurl.Decode(p.URL)
		if err != nil {
			return "", nil, false
		}
		data, mediaType = d, cmp.Or(mediaType, mt)
	}
	if mediaType == "" {
		mediaType = http.DetectContentType(data)
	}
	return BaseType(mediaType), data, true
}

// MediaType returns the media type of a part, in lower case without
// parameters: its MediaType, else that of its data: URL, else guessed
// from the extension of its file name or URL, else detected from its
// data. It returns "" when nothing tells the type.
func MediaType(p model.Part) string {
	if p.MediaType != "" {
		return BaseType(p.MediaType)
	}
	if mt, _, ok := InlineData(p); ok {
		return mt
	}
	return cmp.Or(ExtensionType(p.Filename), ExtensionType(p.URL))
}

// Filename returns the file name of a media part: its Filename, else the
// last element of its URL, else "file" with the extension of its media
// type, such as "file.pdf".
func Filename(p model.Part) string {
	if p.Filename != "" {
		return p.Filename
	}
	if p.URL != "" && !strings.HasPrefix(strings.ToLower(p.URL), "data:") {
		u := p.URL
		if i := strings.IndexAny(u, "?#"); i >= 0 {
			u = u[:i]
		}
		if base := path.Base(u); base != "." && base != "/" && strings.Contains(base, ".") {
			return base
		}
	}
	mt := MediaType(p)
	for _, ext := range []string{"pdf", "txt", "md", "html", "csv", "json", "xml", "docx", "xlsx", "doc", "xls", "png", "jpg", "gif", "webp", "wav", "mp3", "mp4"} {
		if extensionTypes[ext] == mt {
			return "file." + ext
		}
	}
	return "file"
}
