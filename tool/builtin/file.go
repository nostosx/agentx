package builtin

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/nostosx/kitx/utf8text"

	"github.com/nostosx/agentx/internal/diff"
	"github.com/nostosx/agentx/tool"
)

const (
	defaultReadLimit = 2000
	maxLineLength    = 2000
	// sniffLen is the prefix inspected for NUL bytes to detect binary files.
	sniffLen = 8000
)

type readArgs struct {
	Path   string `json:"path" jsonschema:"Path of the file, relative to the working directory or absolute."`
	Offset int    `json:"offset,omitempty" jsonschema:"1-based line number to start reading from."`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum number of lines to read (default 2000)."`
}

// ReadFile returns the read_file tool.
func ReadFile(cfg Config) tool.Tool {
	e := newEnv(cfg)
	return tool.Func("read_file",
		"Read a text file. Output lines are prefixed with their 1-based line numbers and a tab, like cat -n; the prefix is not part of the file. "+
			"Long files are read in pages of up to 2000 lines; use offset and limit to read further. Lines longer than 2000 characters are truncated. "+
			"Images (PNG, JPEG, GIF and WebP, up to 5 MB) and PDF documents (up to 20 MB) are returned whole for you to view.",
		func(ctx context.Context, in readArgs) (*tool.Result, error) {
			return e.readFile(in)
		}, tool.ReadOnly())
}

func (e *env) readFile(in readArgs) (*tool.Result, error) {
	rel, err := e.rel(in.Path)
	if err != nil {
		return tool.Errorf("%v", err), nil
	}
	root, err := os.OpenRoot(e.dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(rel)
	if err != nil {
		return tool.Errorf("%v", err), nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return tool.Errorf("%s is a directory; use glob to list files", in.Path), nil
	}

	br := bufio.NewReaderSize(f, 64*1024)
	head, _ := br.Peek(sniffLen)
	if mt := sniffMedia(head); mt != "" {
		return e.readMedia(in.Path, rel, st, br, mt)
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return tool.Errorf("%s appears to be a binary file", in.Path), nil
	}
	offset := max(in.Offset, 1)
	limit := in.Limit
	if limit <= 0 {
		limit = defaultReadLimit
	}

	// The content hash strengthens the read-before-write check; it covers
	// the whole file, also past the lines shown.
	var h hash.Hash
	var hashed int64
	if e.Files.tracks() && st.Size() <= maxStampHash {
		h = sha256.New()
	}
	var b strings.Builder
	n, shown := 0, 0
	for {
		line, err := br.ReadString('\n')
		if h != nil {
			_, _ = io.WriteString(h, line) // hashes never fail
			hashed += int64(len(line))
		}
		if line != "" {
			n++
			if n >= offset {
				if shown == limit || b.Len() >= e.MaxOutput {
					fmt.Fprintf(&b, "... (file continues; use offset=%d to read more)\n", n)
					break
				}
				line = strings.TrimRight(line, "\r\n")
				fmt.Fprintf(&b, "%6d\t%s\n", n, utf8text.Truncate(line, maxLineLength, "..."))
				shown++
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	// Only a file whose content the model has seen counts as read; an
	// empty file is seen entirely.
	if n == 0 || shown > 0 {
		e.recordRead(rel, st, br, h, hashed)
	}
	switch {
	case n == 0:
		return tool.Text("(empty file)"), nil
	case shown == 0:
		return tool.Errorf("offset %d is past the end of the file, which has %d lines", offset, n), nil
	}
	return tool.Text(strings.TrimSuffix(b.String(), "\n")), nil
}

// recordRead records a file read_file has shown. h has hashed the first
// hashed bytes of the file and br holds the rest.
func (e *env) recordRead(rel string, st fs.FileInfo, br io.Reader, h hash.Hash, hashed int64) {
	if h != nil {
		// Read one byte past the size from Stat: a file that keeps growing,
		// such as a log, must not keep the read going, and is not recorded
		// with a hash.
		rest, err := io.CopyN(h, br, max(st.Size()-hashed+1, 0))
		if (err == nil || errors.Is(err, io.EOF)) && hashed+rest == st.Size() {
			e.Files.recordSum(e.abs(rel), st, [sha256.Size]byte(h.Sum(nil)))
			return
		}
	}
	e.Files.record(e.abs(rel), st, nil)
}

type writeArgs struct {
	Path    string `json:"path" jsonschema:"Path of the file, relative to the working directory or absolute."`
	Content string `json:"content" jsonschema:"The complete new content of the file."`
}

const (
	// diffContext is the number of unchanged lines around each change in
	// the diffs the tools return.
	diffContext = 3
	// maxDiffOutput caps the diff of one file in a tool result.
	maxDiffOutput = 10000
	// maxDiffInput is the largest file the tools compute a diff for.
	maxDiffInput = 4 << 20
)

// WriteFile returns the write_file tool. It replaces the file, keeping the
// permissions of an existing file, and returns the diff of an existing
// file's change. See the package documentation for replacement semantics.
func WriteFile(cfg Config) tool.Tool {
	e := newEnv(cfg)
	return tool.Func("write_file",
		"Create a file or overwrite it completely, creating parent directories as needed. "+
			"Read an existing file with read_file before overwriting it. Prefer edit_file for changes to existing files. "+
			"Overwriting a file returns the diff of the change.",
		func(ctx context.Context, in writeArgs) (*tool.Result, error) {
			rel, err := e.rel(in.Path)
			if err != nil {
				return tool.Errorf("%v", err), nil
			}
			root, err := os.OpenRoot(e.dir)
			if err != nil {
				return nil, err
			}
			defer root.Close()
			var old []byte
			st, err := root.Stat(rel)
			switch {
			case err == nil && st.IsDir():
				return tool.Errorf("%s is a directory", in.Path), nil
			case err == nil:
				if (e.Files.tracks() && st.Size() <= maxStampHash) || st.Size() <= maxDiffInput {
					old, _ = root.ReadFile(rel)
				}
				if err := e.Files.check(e.abs(rel), in.Path, st, old); err != nil {
					return tool.Errorf("%v", err), nil
				}
			case !errors.Is(err, fs.ErrNotExist):
				return tool.Errorf("%v", err), nil
			}
			data := []byte(in.Content)
			c, err := statChange(root, rel, data, old)
			if err != nil {
				return tool.Errorf("%v", err), nil
			}
			kind := FileModified
			if !c.existed {
				kind = FileCreated
			}
			if res := e.saveHistory(ctx, "write_file", []FileEdit{{Path: rel, Kind: kind}}); res != nil {
				return res, nil
			}
			if err := commit(root, []fileChange{c}); err != nil {
				return tool.Errorf("%v", err), nil
			}
			e.recordWrite(root, rel, data)
			name := filepath.ToSlash(rel)
			if !c.existed {
				return tool.Text(fmt.Sprintf("Created %s (%d bytes)", name, len(data))), nil
			}
			out := fmt.Sprintf("Updated %s (%d bytes)", name, len(data))
			switch {
			case old == nil && st.Size() > 0:
				out += "\n(diff omitted: the file is too large)"
			case bytes.Equal(old, data):
				out += "\n(no changes)"
			default:
				out += "\n" + strings.TrimSuffix(e.fileDiff(rel, rel, old, data), "\n")
			}
			return tool.Text(out), nil
		}, e.approval()...)
}

// fileDiff returns the unified diff of a file's change, labeled with the
// slash-separated paths before and after it and shortened for a tool
// result.
func (e *env) fileDiff(from, to string, old, new []byte) string {
	if len(old) > maxDiffInput || len(new) > maxDiffInput {
		return fmt.Sprintf("--- a/%s\n+++ b/%s\n(diff omitted: the file is too large)\n", filepath.ToSlash(from), filepath.ToSlash(to))
	}
	d := diff.Unified("a/"+filepath.ToSlash(from), "b/"+filepath.ToSlash(to), string(old), string(new), diffContext)
	if d == "" {
		return ""
	}
	d = strings.ToValidUTF8(d, "\uFFFD")
	if limit := min(e.MaxOutput, maxDiffOutput); len(d) > limit {
		d = tool.Truncate(d, limit) + "\n"
	}
	return d
}

// editOp is one replacement of edit_file.
type editOp struct {
	OldString  string `json:"old_string" jsonschema:"The exact text to replace, including whitespace and indentation. It must occur exactly once unless replace_all is set."`
	NewString  string `json:"new_string" jsonschema:"The replacement text."`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"Replace every occurrence of old_string."`
}

type editArgs struct {
	Path       string   `json:"path" jsonschema:"Path of the file, relative to the working directory or absolute."`
	OldString  string   `json:"old_string,omitempty" jsonschema:"The exact text to replace, including whitespace and indentation. It must occur exactly once unless replace_all is set; include surrounding lines to make it unique. Required unless edits is set."`
	NewString  string   `json:"new_string,omitempty" jsonschema:"The replacement text."`
	ReplaceAll bool     `json:"replace_all,omitempty" jsonschema:"Replace every occurrence of old_string."`
	Edits      []editOp `json:"edits,omitempty" jsonschema:"Several replacements to make instead of old_string and new_string. They apply in order, each to the result of the previous ones, and either all succeed or the file is left unchanged."`
}

// EditFile returns the edit_file tool. It makes one exact-text replacement,
// or several in order with the edits argument, and returns the diff. See the
// package documentation for replacement semantics.
func EditFile(cfg Config) tool.Tool {
	e := newEnv(cfg)
	return tool.Func("edit_file",
		"Replace exact text in an existing file and return the diff. Read the file first. When copying text from read_file output, omit the line-number prefix. "+
			"To make several changes to one file, pass them in edits instead of old_string and new_string.",
		func(ctx context.Context, in editArgs) (*tool.Result, error) {
			ops := in.Edits
			switch {
			case len(ops) == 0:
				ops = []editOp{{OldString: in.OldString, NewString: in.NewString, ReplaceAll: in.ReplaceAll}}
			case in.OldString != "" || in.NewString != "" || in.ReplaceAll:
				return tool.Errorf("set either old_string and new_string or edits, not both"), nil
			}
			for i, op := range ops {
				label := ""
				if len(in.Edits) > 0 {
					label = fmt.Sprintf("edit %d: ", i+1)
				}
				if op.OldString == "" {
					return tool.Errorf("%sold_string is empty; use write_file to create files", label), nil
				}
				if op.OldString == op.NewString {
					return tool.Errorf("%sold_string and new_string are identical", label), nil
				}
			}
			rel, err := e.rel(in.Path)
			if err != nil {
				return tool.Errorf("%v", err), nil
			}
			root, err := os.OpenRoot(e.dir)
			if err != nil {
				return nil, err
			}
			defer root.Close()
			st, err := root.Stat(rel)
			if err != nil {
				return tool.Errorf("%v", err), nil
			}
			if st.IsDir() {
				return tool.Errorf("%s is a directory", in.Path), nil
			}
			data, err := root.ReadFile(rel)
			if err != nil {
				return tool.Errorf("%v", err), nil
			}
			if err := e.Files.check(e.abs(rel), in.Path, st, data); err != nil {
				return tool.Errorf("%v", err), nil
			}
			content, n, res := applyEdits(string(data), in.Path, ops, len(in.Edits) > 0)
			if res != nil {
				return res, nil
			}
			c, err := statChange(root, rel, []byte(content), data)
			if err != nil {
				return tool.Errorf("%v", err), nil
			}
			if res := e.saveHistory(ctx, "edit_file", []FileEdit{{Path: rel, Kind: FileModified}}); res != nil {
				return res, nil
			}
			if err := commit(root, []fileChange{c}); err != nil {
				return tool.Errorf("%v", err), nil
			}
			e.recordWrite(root, rel, []byte(content))
			summary := fmt.Sprintf("%d replacement%s", n, plural(n))
			if len(in.Edits) > 0 {
				summary = fmt.Sprintf("%d edit%s, %s", len(ops), plural(len(ops)), summary)
			}
			out := fmt.Sprintf("Edited %s (%s)", filepath.ToSlash(rel), summary)
			if d := e.fileDiff(rel, rel, data, []byte(content)); d != "" {
				out += "\n" + strings.TrimSuffix(d, "\n")
			}
			return tool.Text(out), nil
		}, e.approval()...)
}

// applyEdits applies ops to content in order and returns the result and the
// number of replacements, or an error result.
func applyEdits(content, path string, ops []editOp, numbered bool) (string, int, *tool.Result) {
	total := 0
	for i, op := range ops {
		label, after := "", ""
		if numbered {
			label = fmt.Sprintf("edit %d: ", i+1)
			if i > 0 {
				after = " after the previous edits"
			}
		}
		count := strings.Count(content, op.OldString)
		switch {
		case count == 0:
			return "", 0, tool.Errorf("%sold_string was not found in %s%s", label, path, after)
		case count > 1 && !op.ReplaceAll:
			return "", 0, tool.Errorf("%sold_string occurs %d times in %s%s; add surrounding context to make it unique or set replace_all", label, count, path, after)
		}
		if op.ReplaceAll {
			content = strings.ReplaceAll(content, op.OldString, op.NewString)
			total += count
		} else {
			content = strings.Replace(content, op.OldString, op.NewString, 1)
			total++
		}
	}
	return content, total, nil
}

// recordWrite notes the state of a file the tools just wrote, whose new
// content the model knows, so it can be edited again without a re-read.
func (e *env) recordWrite(root *os.Root, rel string, content []byte) {
	if st, err := root.Stat(rel); err == nil {
		e.Files.record(e.abs(rel), st, content)
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
