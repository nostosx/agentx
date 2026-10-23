package builtin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nostosx/agentx/model"
)

var (
	pngFile = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x02\x00\x00\x00")
	pdfFile = []byte("%PDF-1.7\n1 0 obj\n<< /Type /Page >>\nendobj\n%%EOF\n")
)

func TestReadFileMedia(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"img/shot.png": string(pngFile),
		// The content decides, not the extension.
		"doc.bin":   string(pdfFile),
		"anim.gif":  "GIF89a\x01\x00\x01\x00\x00\x00\x00",
		"pic.webp":  "RIFF\x1a\x00\x00\x00WEBPVP8 ",
		"photo.jpg": "\xff\xd8\xff\xe0\x00\x10JFIF\x00",
		"notes.txt": "text\n",
		"icon.bmp":  "BM\x00\x00\x00\x00",
	})
	files := &FileTracker{}
	rf := ReadFile(Config{Dir: dir, Files: files})

	res := call(t, rf, map[string]any{"path": "img/shot.png", "offset": 5})
	if res.IsError || len(res.Parts) != 1 || !strings.Contains(res.Content, "Image img/shot.png (image/png, 29 bytes)") {
		t.Fatalf("image result = %+v", res)
	}
	if p := res.Parts[0]; p.Type != model.PartImage || p.MediaType != "image/png" || !bytes.Equal(p.Data, pngFile) || p.Filename != "shot.png" {
		t.Fatalf("image part = %+v", p)
	}
	res = call(t, rf, map[string]any{"path": "doc.bin"})
	if p := res.Parts[0]; res.IsError || p.Type != model.PartDocument || p.MediaType != "application/pdf" || p.Filename != "doc.bin" || !bytes.Equal(p.Data, pdfFile) || !strings.HasPrefix(res.Content, "PDF document doc.bin") {
		t.Fatalf("PDF result = %+v", res)
	}
	for name, mt := range map[string]string{"anim.gif": "image/gif", "pic.webp": "image/webp", "photo.jpg": "image/jpeg"} {
		if res := call(t, rf, map[string]any{"path": name}); res.IsError || len(res.Parts) != 1 || res.Parts[0].MediaType != mt {
			t.Errorf("%s: %+v", name, res)
		}
	}
	// Text is unchanged, and other binary formats are still refused.
	if res := call(t, rf, map[string]any{"path": "notes.txt"}); res.Parts != nil || res.Content != "     1\ttext" {
		t.Errorf("text result = %+v", res)
	}
	if res := call(t, rf, map[string]any{"path": "icon.bmp"}); !res.IsError || res.Parts != nil {
		t.Errorf("bmp result = %+v", res)
	}
	// A media file read counts as read for writes.
	st, err := os.Stat(filepath.Join(dir, "img", "shot.png"))
	if err != nil {
		t.Fatal(err)
	}
	if err := files.check(filepath.Join(dir, "img", "shot.png"), "img/shot.png", st, pngFile); err != nil {
		t.Errorf("read not recorded: %v", err)
	}
}

func TestReadFileMediaLimits(t *testing.T) {
	dir := t.TempDir()
	big := append(bytes.Clone(pngFile), make([]byte, maxImageBytes)...)
	if err := os.WriteFile(filepath.Join(dir, "big.png"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	pdf := append(bytes.Clone(pdfFile), make([]byte, maxPDFBytes)...)
	if err := os.WriteFile(filepath.Join(dir, "big.pdf"), pdf, 0o644); err != nil {
		t.Fatal(err)
	}
	rf := ReadFile(Config{Dir: dir})
	if res := call(t, rf, map[string]any{"path": "big.png"}); !res.IsError || res.Parts != nil || !strings.Contains(res.Content, "is an image of 5.0 MB, larger than the 5.0 MB read_file returns") {
		t.Errorf("big image = %q", res.Content)
	}
	if res := call(t, rf, map[string]any{"path": "big.pdf"}); !res.IsError || !strings.Contains(res.Content, "is a PDF document of 20.0 MB, larger than the 20.0 MB") {
		t.Errorf("big PDF = %q", res.Content)
	}
}

func TestMediaSize(t *testing.T) {
	for n, want := range map[int64]string{12: "12 bytes", 2048: "2.0 KB", 3 << 20: "3.0 MB"} {
		if got := mediaSize(n); got != want {
			t.Errorf("mediaSize(%d) = %q, want %q", n, got, want)
		}
	}
}
