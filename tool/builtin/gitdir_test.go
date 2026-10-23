package builtin

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// globAll lists the files Glob reports under dir, sorted.
func globAll(t *testing.T, dir string) []string {
	t.Helper()
	res := call(t, Glob(Config{Dir: dir}), map[string]any{"pattern": "**"})
	if res.IsError {
		t.Fatalf("glob %s: %q", dir, res.Content)
	}
	got := strings.Split(res.Content, "\n")
	slices.Sort(got)
	return got
}

// TestIgnoreGitFile checks work trees whose .git is a file naming the git
// directory, as in linked work trees and submodules: info/exclude is read
// from that directory, or from its commondir when it has one.
func TestIgnoreGitFile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		files func(root string) map[string]string
		want  []string
	}{
		{
			// A submodule's .git names its git directory relative to the
			// work tree.
			name: "relative gitdir",
			files: func(string) map[string]string {
				return map[string]string{
					".git/modules/sub/HEAD":         "ref: refs/heads/main\n",
					".git/modules/sub/info/exclude": "secret.txt\n",
					"sub/.git":                      "gitdir: ../.git/modules/sub\n",
				}
			},
			want: []string{"keep.txt"},
		},
		{
			// A linked work tree names its git directory with an absolute
			// path; info/exclude lives in the common directory, and the
			// work tree's own info directory is not used.
			name: "absolute gitdir with relative commondir",
			files: func(root string) map[string]string {
				return map[string]string{
					"main/.git/HEAD":                        "ref: refs/heads/main\n",
					"main/.git/info/exclude":                "secret.txt\n",
					"main/.git/worktrees/sub/commondir":     "../..\n",
					"main/.git/worktrees/sub/info/exclude":  "keep.txt\n",
					"main/.git/worktrees/sub/gitdir":        filepath.Join(root, "sub", ".git") + "\n",
					"sub/.git":                              "gitdir: " + filepath.Join(root, "main", ".git", "worktrees", "sub") + "\n",
					"main/.git/worktrees/sub/HEAD":          "ref: refs/heads/sub\n",
					"main/.git/worktrees/sub/info/.keepdir": "",
				}
			},
			want: []string{"keep.txt"},
		},
		{
			name: "absolute commondir",
			files: func(root string) map[string]string {
				return map[string]string{
					"common/HEAD":         "ref: refs/heads/main\n",
					"common/info/exclude": "secret.txt\n",
					"private/HEAD":        "ref: refs/heads/main\n",
					"private/commondir":   filepath.Join(root, "common") + "\n",
					"sub/.git":            "gitdir: ../private\n",
				}
			},
			want: []string{"keep.txt"},
		},
		{
			// Without HEAD the target is not a git directory.
			name: "gitdir without HEAD",
			files: func(string) map[string]string {
				return map[string]string{
					".git/modules/sub/info/exclude": "secret.txt\n",
					"sub/.git":                      "gitdir: ../.git/modules/sub\n",
				}
			},
			want: []string{"keep.txt", "secret.txt"},
		},
		{
			name: "commondir without HEAD",
			files: func(root string) map[string]string {
				return map[string]string{
					"common/info/exclude": "secret.txt\n",
					"private/HEAD":        "ref: refs/heads/main\n",
					"private/commondir":   filepath.Join(root, "common") + "\n",
					"sub/.git":            "gitdir: ../private\n",
				}
			},
			want: []string{"keep.txt", "secret.txt"},
		},
		{
			name: "network gitdir",
			files: func(string) map[string]string {
				return map[string]string{"sub/.git": "gitdir: //attacker.invalid/share\n"}
			},
			want: []string{"keep.txt", "secret.txt"},
		},
		{
			name: "malformed git file",
			files: func(string) map[string]string {
				return map[string]string{
					".git/modules/sub/info/exclude": "secret.txt\n",
					"sub/.git":                      "../.git/modules/sub\n",
				}
			},
			want: []string{"keep.txt", "secret.txt"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			files := tc.files(root)
			files["sub/keep.txt"] = "x"
			files["sub/secret.txt"] = "x"
			writeFiles(t, root, files)
			if got := globAll(t, filepath.Join(root, "sub")); !slices.Equal(got, tc.want) {
				t.Fatalf("glob = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIgnoreLinkedWorktreeMatchesGit(t *testing.T) {
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main, wt := filepath.Join(root, "main"), filepath.Join(root, "wt")
	writeFiles(t, main, map[string]string{"README": "x"})
	gitCmd(t, main, "init", "-q")
	gitCmd(t, main, "add", "README")
	gitCmd(t, main, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "init")
	gitCmd(t, main, "worktree", "add", "-q", "--detach", wt)
	writeFiles(t, main, map[string]string{".git/info/exclude": "secret.txt\n*.tmp\n"})
	writeFiles(t, wt, map[string]string{
		"secret.txt":     "x",
		"keep.txt":       "x",
		"dir/build.tmp":  "x",
		"dir/secret.txt": "x",
		"dir/keep.go":    "x",
	})
	common := strings.TrimSpace(gitCmd(t, wt, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	commonInfo, err := os.Stat(common)
	if err != nil {
		t.Fatal(err)
	}
	mainInfo, err := os.Stat(filepath.Join(main, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(commonInfo, mainInfo) {
		t.Fatal("work tree does not share the main repository")
	}
	for _, dir := range []string{wt, filepath.Join(wt, "dir")} {
		want := strings.Fields(gitCmd(t, dir, "ls-files", "--cached", "--others", "--exclude-standard"))
		got := globAll(t, dir)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s:\nglob %v\ngit  %v", dir, got, want)
		}
	}
	if got := globAll(t, wt); !slices.Equal(got, []string{"README", "dir/keep.go", "keep.txt"}) {
		t.Errorf("work tree glob = %v", got)
	}
	// The work tree's .git file is skipped like a .git directory.
	if res := call(t, Grep(Config{Dir: wt}), map[string]any{"pattern": "gitdir"}); res.Content != "No matches found." {
		t.Errorf("grep searched the .git file: %q", res.Content)
	}
}

func TestNetworkPath(t *testing.T) {
	t.Parallel()
	network := []string{`\\host\share`, "//host/share", `\\?\UNC\host\share`, `\\.\pipe\x`}
	local := []string{"relative/dir", "../.git/modules/sub"}
	if runtime.GOOS == "windows" {
		network = append(network, `\/host/share`)
		local = append(local, `C:\repo\.git`)
	} else {
		network = append(network, "/net/host/share", "/Network/Servers/host")
		local = append(local, "/repo/.git", "/home/net", "net/x")
	}
	for _, p := range network {
		if !networkPath(p) {
			t.Errorf("networkPath(%q) = false", p)
		}
	}
	for _, p := range local {
		if networkPath(p) {
			t.Errorf("networkPath(%q) = true", p)
		}
	}
}

// TestLocalGitDirRefusesLinkToNetwork checks that a symbolic link is not
// followed to a network path, which merely looking up could mount.
func TestLocalGitDirRefusesLinkToNetwork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows")
	}
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFiles(t, root, map[string]string{"real/HEAD": "ref: refs/heads/main\n"})
	for name, target := range map[string]string{
		"net":      "/net/attacker.invalid/share",
		"unc":      "//attacker.invalid/share",
		"relative": "../../../../../../../../../../net/attacker.invalid",
		"local":    "real",
	} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"net", "unc", "relative"} {
		if dir, ok := localGitDir(root, name); ok {
			t.Errorf("localGitDir(%s) = %q, want refusal", name, dir)
		}
	}
	if dir, ok := localGitDir(root, "local"); !ok || dir != filepath.Join(root, "real") {
		t.Errorf("localGitDir(local) = %q, %v", dir, ok)
	}
}
