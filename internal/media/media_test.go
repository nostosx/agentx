package media_test

import (
	"testing"

	"github.com/nostosx/agentx/internal/media"

	"github.com/nostosx/agentx/model"
)

func TestExtensionAndBaseType(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"a.PDF": "application/pdf", "dir/b.jpeg": "image/jpeg", "https://x.test/v.mp4?sig=1#t": "video/mp4",
		"s3://bucket/clip.mov": "video/quicktime", "a.unknown": "", "noext": "", "talk.m4a": "audio/mp4",
	} {
		if got := media.ExtensionType(name); got != want {
			t.Errorf("ExtensionType(%q) = %q, want %q", name, got, want)
		}
	}
	if got := media.BaseType(" Audio/L16; rate=24000"); got != "audio/l16" {
		t.Errorf("BaseType = %q", got)
	}
}

func TestInlineDataAndMediaType(t *testing.T) {
	t.Parallel()
	png := []byte("\x89PNG\r\n\x1a\n")
	for _, tc := range []struct {
		name   string
		part   model.Part
		inline bool
		mt     string
	}{
		{name: "data", part: model.Part{Type: model.PartImage, Data: png, MediaType: "Image/PNG"}, inline: true, mt: "image/png"},
		{name: "sniffed", part: model.Part{Type: model.PartImage, Data: png}, inline: true, mt: "image/png"},
		{name: "data URL", part: model.ImageURLPart("data:image/gif;base64,R0lGODlh"), inline: true, mt: "image/gif"},
		{name: "parameterized URL", part: model.ImageURLPart("data:image/svg+xml;charset=utf-8;base64,PHN2Zy8+"), inline: true, mt: "image/svg+xml"},
		{name: "invalid URL media type", part: model.ImageURLPart("data:invalid;base64,YQ==")},
		{name: "invalid URL parameter", part: model.ImageURLPart("data:image/png;name=%0D%0Aprivate,YQ==")},

		{name: "URL", part: model.DocumentURLPart("https://example.com/a.pdf"), mt: "application/pdf"},
		{name: "file name", part: model.Part{Type: model.PartDocument, FileID: "f", Filename: "a.csv"}, mt: "text/csv"},
		{name: "unknown", part: model.Part{Type: model.PartDocument, FileID: "f"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mt, data, ok := media.InlineData(tc.part)
			if ok != tc.inline || (ok && (mt != tc.mt || len(data) == 0)) {
				t.Errorf("InlineData = %q, %d bytes, %v", mt, len(data), ok)
			}
			if got := media.MediaType(tc.part); got != tc.mt {
				t.Errorf("MediaType = %q, want %q", got, tc.mt)
			}
		})
	}
}

func TestFilename(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		part model.Part
		want string
	}{
		{model.Part{Filename: "given.pdf", URL: "https://x.test/other.pdf"}, "given.pdf"},
		{model.Part{URL: "https://x.test/dir/paper.pdf?x=1"}, "paper.pdf"},
		{model.Part{URL: "https://x.test/download"}, "file"},
		{model.Part{MediaType: "application/pdf", Data: []byte("%PDF")}, "file.pdf"},
		{model.Part{URL: "data:text/csv;base64,YQ=="}, "file.csv"},
		{model.Part{MediaType: "application/zip"}, "file"},
	} {
		if got := media.Filename(tc.part); got != tc.want {
			t.Errorf("Filename(%+v) = %q, want %q", tc.part, got, tc.want)
		}
	}
}
