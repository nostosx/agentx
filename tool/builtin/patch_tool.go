package builtin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/nostosx/agentx/tool"
)

const applyPatchDescription = `Edit files with a patch: add, delete, update and rename files in one call. The patch is plain text in this format:

*** Begin Patch
[one or more file operations]
*** End Patch

Each operation starts with one of these headers:
*** Add File: <path> - create a file, or replace an existing one. Every following line is a + line holding a line of the content.
*** Delete File: <path> - delete a file. Nothing follows.
*** Update File: <path> - change a file. It may be followed by "*** Move to: <new path>" to rename the file. Then come one or more changes. Each change starts with @@, optionally followed by a line of the file before the change, such as a class or function signature, that narrows down where it is. Every line of a change starts with ' ' for an unchanged line, '-' for a removed line or '+' for an added line. Show 3 unchanged lines before and after each change, enough to locate it uniquely. A change at the end of the file may end with "*** End of File". A change made only of + lines is added at the end of the file.

Example:
*** Begin Patch
*** Add File: hello.txt
+Hello world
*** Update File: src/app.py
*** Move to: src/main.py
@@ def greet():
-print("Hi")
+print("Hello, world!")
*** Delete File: obsolete.txt
*** End Patch

Paths are relative to the working directory. Read a file with read_file before updating, replacing or deleting it. The patch is applied as a whole: if any part fails, no file changes.`

// patchArgs holds the patch in the argument model.FreeformInput, which
// carries the text of freeform calls.
type patchArgs struct {
	Input string `json:"input" jsonschema:"The entire patch, from *** Begin Patch to *** End Patch."`
}

// ApplyPatch returns the apply_patch tool, which applies patches in the
// format of OpenAI's Codex, the format GPT models are trained to write. To
// providers that support freeform tools, such as the OpenAI Responses API,
// the tool is freeform, constrained by Codex's Lark grammar; others send
// the patch as the JSON argument "input".
//
// Like Codex, the tool locates changes by their lines with decreasing
// strictness: exactly, ignoring trailing whitespace, ignoring surrounding
// whitespace, and treating typographic punctuation as ASCII. Unlike Codex
// it checks every operation, including Config.Files for files it updates,
// replaces or deletes, before it changes anything, and then commits all
// files together; if a step fails, the files it already changed are
// restored. The result lists the changed files and shows the diff of each
// file whose old content was changed.
func ApplyPatch(cfg Config) tool.Tool {
	e := newEnv(cfg)
	opts := append(e.approval(), tool.Freeform("lark", applyPatchGrammar))
	return tool.Func("apply_patch", applyPatchDescription,
		func(ctx context.Context, in patchArgs) (*tool.Result, error) {
			return e.applyPatch(ctx, in.Input)
		}, opts...)
}

// patchFile is the state of a file during a patch: on disk before it, and
// after the operations so far.
type patchFile struct {
	rel string
	// onDisk reports whether the file existed before the patch, with
	// state st and content orig; link is set for a symbolic link.
	onDisk bool
	st     fs.FileInfo
	orig   []byte
	// verified is set once Config.Files allowed changing the file.
	verified bool
	// exists, content and perm are the state after the operations so far;
	// perm 0 is the default for new files.
	exists  bool
	content []byte
	perm    fs.FileMode
}

// patchRun applies the operations of a patch in memory.
type patchRun struct {
	e     *env
	root  *os.Root
	files map[string]*patchFile
	order []*patchFile
}

// file returns the state of the file at path, as named in the patch.
func (r *patchRun) file(path string) (*patchFile, error) {
	rel, err := r.e.rel(path)
	if err != nil {
		return nil, err
	}
	if f, ok := r.files[rel]; ok {
		return f, nil
	}
	f := &patchFile{rel: rel}
	st, err := r.root.Stat(rel)
	switch {
	case err == nil && st.IsDir():
		return nil, fmt.Errorf("%s is a directory", path)
	case err == nil:
		data, err := r.root.ReadFile(rel)
		if err != nil {
			return nil, err
		}
		f.onDisk, f.st, f.orig = true, st, data
		f.exists, f.content, f.perm = true, data, st.Mode().Perm()
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	r.files[rel] = f
	r.order = append(r.order, f)
	return f, nil
}

// verify checks Config.Files before a file that existed is changed.
func (r *patchRun) verify(f *patchFile, path string) error {
	if !f.onDisk || f.verified {
		return nil
	}
	if err := r.e.Files.check(r.e.abs(f.rel), path, f.st, f.orig); err != nil {
		return err
	}
	f.verified = true
	return nil
}

// run applies a hunk in memory and returns its summary line.
func (r *patchRun) run(h patchHunk) (string, error) {
	f, err := r.file(h.path)
	if err != nil {
		return "", err
	}
	switch h.kind {
	case addHunk:
		if f.exists {
			if err := r.verify(f, h.path); err != nil {
				return "", err
			}
		}
		f.exists, f.content = true, []byte(h.contents)
		return "A " + h.path, nil
	case deleteHunk:
		if !f.exists {
			return "", fmt.Errorf("failed to delete file %s: it does not exist", h.path)
		}
		if err := r.verify(f, h.path); err != nil {
			return "", err
		}
		f.exists, f.content = false, nil
		return "D " + h.path, nil
	}
	if !f.exists {
		return "", fmt.Errorf("failed to read file to update %s: it does not exist", h.path)
	}
	if err := r.verify(f, h.path); err != nil {
		return "", err
	}
	if !utf8.Valid(f.content) {
		return "", fmt.Errorf("%s is not valid UTF-8 text", h.path)
	}
	updated, err := applyChunks(string(f.content), h.path, h.chunks)
	if err != nil {
		return "", err
	}
	if h.movePath == "" {
		f.content = []byte(updated)
		return "M " + h.path, nil
	}
	dest, err := r.file(h.movePath)
	if err != nil {
		return "", err
	}
	if dest != f {
		if dest.exists {
			if err := r.verify(dest, h.movePath); err != nil {
				return "", err
			}
		} else {
			dest.perm = f.perm
		}
		f.exists, f.content = false, nil
	}
	dest.exists, dest.content = true, []byte(updated)
	return "M " + h.movePath, nil
}

func (e *env) applyPatch(ctx context.Context, text string) (*tool.Result, error) {
	hunks, err := parsePatch(text)
	if err != nil {
		return tool.Errorf("%v", err), nil
	}
	if len(hunks) == 0 {
		return tool.Errorf("No files were modified."), nil
	}
	root, err := os.OpenRoot(e.dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	r := &patchRun{e: e, root: root, files: map[string]*patchFile{}}
	// Codex lists added, then modified, then deleted files.
	var added, modified, deleted []string
	for _, h := range hunks {
		line, err := r.run(h)
		if err != nil {
			return tool.Errorf("%v", err), nil
		}
		switch line[0] {
		case 'A':
			added = append(added, line)
		case 'M':
			modified = append(modified, line)
		default:
			deleted = append(deleted, line)
		}
	}

	var changes []fileChange
	var diffs strings.Builder
	moved := map[string]string{} // the old path of each moved file, by its new path
	movedTo := map[string]string{}
	for _, h := range hunks {
		if h.kind == updateHunk && h.movePath != "" {
			from, _ := e.rel(h.path)
			to, _ := e.rel(h.movePath)
			if from != to {
				moved[to], movedTo[from] = from, to
			}
		}
	}
	for _, f := range r.order {
		switch {
		case f.onDisk && !f.exists:
			c, err := statChange(root, f.rel, nil, f.orig)
			if err != nil {
				return tool.Errorf("%v", err), nil
			}
			c.remove = true
			changes = append(changes, c)
		case f.exists && (!f.onDisk || !bytes.Equal(f.content, f.orig)):
			c, err := statChange(root, f.rel, f.content, f.orig)
			if err != nil {
				return tool.Errorf("%v", err), nil
			}
			c.perm = f.perm
			changes = append(changes, c)
			// A diff shows changes to existing content; that of a new file
			// would repeat the patch.
			from, old := f.rel, f.orig
			if src, ok := moved[f.rel]; ok && !f.onDisk {
				from, old = src, r.files[src].orig
			} else if !f.onDisk {
				continue
			}
			diffs.WriteString(e.fileDiff(from, f.rel, old, f.content))
		}
	}
	edits := make([]FileEdit, len(changes))
	for i, c := range changes {
		edits[i] = FileEdit{Path: c.rel, Kind: FileModified, From: moved[c.rel], To: movedTo[c.rel]}
		switch {
		case c.remove:
			edits[i].Kind = FileDeleted
		case !c.existed:
			edits[i].Kind = FileCreated
		}
	}
	if res := e.saveHistory(ctx, "apply_patch", edits); res != nil {
		return res, nil
	}
	if err := commit(root, changes); err != nil {
		return tool.Errorf("applying the patch failed, and no file was changed: %v", err), nil
	}
	for _, c := range changes {
		if c.remove {
			e.Files.forget(e.abs(c.rel))
		} else {
			e.recordWrite(root, c.rel, c.data)
		}
	}

	var b strings.Builder
	b.WriteString("Success. Updated the following files:\n")
	for _, list := range [][]string{added, modified, deleted} {
		for _, line := range list {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	if diffs.Len() > 0 {
		b.WriteByte('\n')
		b.WriteString(diffs.String())
	}
	return tool.Text(tool.Truncate(strings.TrimSuffix(b.String(), "\n"), e.MaxOutput)), nil
}
