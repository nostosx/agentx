package builtin

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// Sandbox confines the commands of the bash tool, foreground and
// background, at the operating-system level, so that what a command can
// write and reach no longer depends on what the model chose to run. See
// NewSandbox for the sandbox of the operating system; other
// implementations may run commands in a container or a virtual machine.
type Sandbox interface {
	// Available returns nil if the sandbox can run commands on this
	// system, or an error saying why it cannot. bash asks before every
	// command, to decide whether the command needs approval and whether to
	// apply Config.SandboxFallback, so it should be cheap.
	Available() error
	// Wrap rewrites cmd, which has not been started, to run inside the
	// sandbox. dir is the working directory of the tools, Config.Dir,
	// which the sandbox should let the command write to; cmd.Dir is where
	// the command starts. Wrap may change cmd.Path, cmd.Args, cmd.Env and
	// cmd.SysProcAttr, but must keep cmd's descriptors, including
	// cmd.ExtraFiles, and run its program with its arguments. It returns
	// an error wrapping ErrSandboxUnavailable if the sandbox cannot run
	// commands.
	Wrap(cmd *exec.Cmd, dir string) error
}

// ErrSandboxUnavailable reports that a sandbox cannot run commands on
// this system.
var ErrSandboxUnavailable = errors.New("sandbox unavailable")

// SandboxFallback selects what bash does when Config.Sandbox is set but
// not available.
type SandboxFallback int

const (
	// SandboxRequired, the default, fails every command with an error
	// result naming the reason, so that a missing sandbox never widens
	// what commands can do. It matches Claude Code's failIfUnavailable
	// setting, applied per command.
	SandboxRequired SandboxFallback = iota
	// SandboxBestEffort runs commands without the sandbox, but only after
	// approval, even with Config.NoApproval.
	SandboxBestEffort
)

// SandboxPolicy describes what the commands in a sandbox of NewSandbox
// may access. The zero value lets commands read everything, write to the
// working directory and the temporary directories except the
// DefaultProtectedNames entries in them, and use no network.
//
// Paths may start with "~/" for the home directory; other relative paths
// are relative to the working directory passed to Wrap.
type SandboxPolicy struct {
	// WritableRoots are directories commands may write to besides the
	// working directory and the temporary directories. A root whose path
	// passes through a symbolic link inside a writable root is refused,
	// since a command could retarget the link and widen the next
	// command's access.
	WritableRoots []string
	// ReadableRoots, if set, restricts reading to them, the writable
	// roots and the system directories programs need to run. If empty,
	// commands may read everything except DenyRead.
	ReadableRoots []string
	// DenyRead are files and directories commands may neither read nor
	// write, such as "~/.ssh" or "~/.aws/credentials". On Linux only
	// paths that exist when the command starts are hidden.
	DenyRead []string
	// ProtectedNames are entries at the top of each writable root that
	// stay read-only, so commands cannot change version control state or
	// agent configuration. On macOS they are protected even when missing,
	// so commands cannot create them either; on Linux only existing ones
	// are. A ".git" file pointing to a git directory protects that
	// directory too. Nil means DefaultProtectedNames; an empty non-nil
	// slice protects nothing.
	ProtectedNames []string
	// NoTempDirs keeps the temporary directories, $TMPDIR and /tmp,
	// read-only.
	NoTempDirs bool
	// AllowNetwork gives commands full network access.
	AllowNetwork bool
	// AllowedDomains are hosts commands may reach, unless AllowNetwork is
	// set, through an HTTP proxy that the sandbox starts on the loopback
	// interface and announces in HTTP_PROXY, HTTPS_PROXY and ALL_PROXY.
	// The proxy forwards plain HTTP requests and HTTPS CONNECT tunnels to
	// allowed hosts and refuses the rest. An entry is a host name, "*."
	// and a domain for its subdomains, "*" for every host, or an IP
	// address, IPv6 in brackets, each optionally followed by ":port". A
	// host name that resolves only to loopback or link-local addresses,
	// or to an address of this machine, is refused unless it is
	// "localhost" or ends in ".localhost". Programs that ignore the proxy
	// variables, and traffic other than TCP, cannot reach the network. On
	// Linux the proxy is bridged into the sandbox with socat, which must
	// be on PATH.
	AllowedDomains []string
	// AllowLocalBinding lets commands on macOS listen on ports and talk to
	// servers on the loopback interface. On Linux commands always have a
	// loopback interface of their own, isolated from the host's, unless
	// AllowNetwork is set.
	AllowLocalBinding bool
}

// DefaultProtectedNames returns the entries that stay read-only at the
// top of each writable root by default: those Codex protects (.git,
// .agents, .codex and .aws, whose profiles can name credential helpers)
// and Claude Code's .claude configuration directory.
func DefaultProtectedNames() []string {
	return []string{".git", ".agents", ".codex", ".aws", ".claude"}
}

// Sandbox backends of NewSandbox.
const (
	BackendSeatbelt   = "seatbelt"
	BackendBubblewrap = "bubblewrap"
)

// seatbeltPath is the only sandbox-exec used, like Codex, so a sandbox-exec
// earlier on PATH cannot replace it.
const seatbeltPath = "/usr/bin/sandbox-exec"

// probeTimeout bounds the check that the backend works.
const probeTimeout = 10 * time.Second

// SystemSandbox is the sandbox of the operating system: Seatbelt through
// sandbox-exec on macOS and bubblewrap on Linux. Create it with NewSandbox
// and Close it when done, which stops its proxy. It is safe for concurrent
// use and may be shared by several tool sets.
type SystemSandbox struct {
	policy  SandboxPolicy
	backend string
	// bwrap and socat are the resolved programs on Linux.
	bwrap, socat string
	unavailable  error

	probeOnce sync.Once
	probeErr  error

	mu     sync.Mutex
	proxy  *domainProxy
	closed bool
}

// NewSandbox returns the sandbox of the operating system with policy p:
// Seatbelt on macOS, through /usr/bin/sandbox-exec with a profile modeled
// on Codex's, and bubblewrap on Linux when bwrap is on PATH, with the
// mounts Codex's Linux sandbox uses. Elsewhere, or when the backend is
// missing or cannot create its sandbox, for example inside another
// sandbox, Available reports why and Wrap fails.
//
// Landlock and seccomp filters are not used on Linux: the sandbox relies
// on bubblewrap's mount, user, PID, IPC and network namespaces alone. A
// command can therefore still connect to Unix sockets it can reach in the
// filesystem; the sockets of Docker, containerd, Podman and CRI-O are
// hidden unless the network is allowed, but others, such as a desktop
// session bus, are not. Hide them with DenyRead where that matters.
func NewSandbox(p SandboxPolicy) *SystemSandbox {
	s := &SystemSandbox{policy: p}
	switch runtime.GOOS {
	case "darwin":
		s.backend = BackendSeatbelt
		if _, err := os.Stat(seatbeltPath); err != nil {
			s.unavailable = fmt.Errorf("%w: %s not found", ErrSandboxUnavailable, seatbeltPath)
		}
	case "linux":
		s.backend = BackendBubblewrap
		bwrap, err := exec.LookPath("bwrap")
		if err != nil {
			s.unavailable = fmt.Errorf("%w: bubblewrap (bwrap) is not on PATH", ErrSandboxUnavailable)
			break
		}
		s.bwrap = bwrap
		if s.usesProxy() {
			socat, err := exec.LookPath("socat")
			if err != nil {
				s.unavailable = fmt.Errorf("%w: AllowedDomains needs socat on PATH to reach the proxy", ErrSandboxUnavailable)
				break
			}
			s.socat = socat
		}
	default:
		s.unavailable = fmt.Errorf("%w: no sandbox backend for %s", ErrSandboxUnavailable, runtime.GOOS)
	}
	return s
}

// Backend returns BackendSeatbelt, BackendBubblewrap or "" if the system
// has no backend.
func (s *SystemSandbox) Backend() string { return s.backend }

// Available reports whether the backend exists and can create a sandbox.
// The first call runs a trivial command in a sandbox to find out.
func (s *SystemSandbox) Available() error {
	if s.unavailable != nil {
		return s.unavailable
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return fmt.Errorf("%w: the sandbox was closed", ErrSandboxUnavailable)
	}
	s.probeOnce.Do(func() { s.probeErr = s.probe() })
	return s.probeErr
}

// probe runs true in a sandbox with the namespaces bash commands use.
func (s *SystemSandbox) probe() error {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	var cmd *exec.Cmd
	switch s.backend {
	case BackendSeatbelt:
		cmd = exec.CommandContext(ctx, seatbeltPath, "-p", "(version 1)(allow default)", "--", "/usr/bin/true")
	case BackendBubblewrap:
		args := []string{"--ro-bind", "/", "/", "--dev", "/dev", "--unshare-user", "--unshare-pid", "--unshare-ipc", "--proc", "/proc"}
		if !s.policy.AllowNetwork {
			args = append(args, "--unshare-net")
		}
		cmd = exec.CommandContext(ctx, s.bwrap, append(args, "--", "true")...)
	default:
		return fmt.Errorf("%w: no backend", ErrSandboxUnavailable)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s cannot create a sandbox here: %w: %s", ErrSandboxUnavailable, s.backend, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// usesProxy reports whether commands reach the network through the proxy.
func (s *SystemSandbox) usesProxy() bool {
	return !s.policy.AllowNetwork && len(s.policy.AllowedDomains) > 0
}

// Wrap rewrites cmd to run in the sandbox; see Sandbox.
func (s *SystemSandbox) Wrap(cmd *exec.Cmd, dir string) error {
	if err := s.Available(); err != nil {
		return err
	}
	spec, err := s.spec(dir)
	if err != nil {
		return err
	}
	if s.usesProxy() {
		p, err := s.startProxy()
		if err != nil {
			return err
		}
		spec.proxyPort = p.port()
		spec.proxySocket = p.socket
	}
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	argv := append([]string{cmd.Path}, cmd.Args[1:]...)
	switch s.backend {
	case BackendSeatbelt:
		profile, params := seatbeltProfile(spec)
		args := []string{"sandbox-exec", "-p", profile}
		for _, p := range params {
			args = append(args, "-D"+p.key+"="+p.value)
		}
		cmd.Path, cmd.Args = seatbeltPath, append(append(args, "--"), argv...)
		if spec.proxyPort != 0 {
			cmd.Env = append(cmd.Env, proxyEnv(fmt.Sprintf("http://127.0.0.1:%d", spec.proxyPort))...)
		}
	case BackendBubblewrap:
		cwd := cmd.Dir
		if cwd == "" {
			cwd = dir
		}
		if spec.proxySocket != "" {
			argv = append([]string{"/bin/sh", "-c", bwrapBridgeScript(s.socat, spec.proxySocket), "agentx-sandbox"}, argv...)
			cmd.Env = append(cmd.Env, proxyEnv(fmt.Sprintf("http://127.0.0.1:%d", bwrapProxyPort))...)
		}
		cmd.Path, cmd.Args = s.bwrap, append([]string{"bwrap"}, bwrapArgs(spec, cwd, argv)...)
	}
	return nil
}

// DeniedHosts returns the hosts, with ports, that the proxy refused since
// the given time, so that a command's result can name what the sandbox
// blocked. Hosts refused to concurrent commands are included too.
func (s *SystemSandbox) DeniedHosts(since time.Time) []string {
	s.mu.Lock()
	p := s.proxy
	s.mu.Unlock()
	if p == nil {
		return nil
	}
	return p.deniedSince(since)
}

// Close stops the proxy. Later commands are refused.
func (s *SystemSandbox) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.proxy == nil {
		return nil
	}
	err := s.proxy.close()
	s.proxy = nil
	return err
}

// startProxy starts the proxy on first use.
func (s *SystemSandbox) startProxy() (*domainProxy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("%w: the sandbox was closed", ErrSandboxUnavailable)
	}
	if s.proxy != nil {
		return s.proxy, nil
	}
	p, err := newDomainProxy(s.policy.AllowedDomains, s.backend == BackendBubblewrap)
	if err != nil {
		return nil, fmt.Errorf("start the sandbox proxy: %w", err)
	}
	s.proxy = p
	return p, nil
}

// proxyEnv returns the variables that direct clients to the proxy at u.
// NO_PROXY is cleared: a direct connection fails in the sandbox anyway.
func proxyEnv(u string) []string {
	var env []string
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		env = append(env, k+"="+u, strings.ToLower(k)+"="+u)
	}
	return append(env, "NO_PROXY=", "no_proxy=")
}

// sandboxSpec is a SandboxPolicy resolved for one command. Its paths are
// absolute, with symbolic links resolved, as Seatbelt matches resolved
// paths.
type sandboxSpec struct {
	writable []writableRoot
	// readable is nil when commands may read everything.
	readable []string
	denyRead []string
	network  networkMode
	// proxyPort is the port of the proxy on macOS and proxySocket its Unix
	// socket on Linux, when the network goes through the proxy.
	proxyPort    int
	proxySocket  string
	localBinding bool
	// sockets are the container daemon sockets bubblewrap hides.
	sockets []string
}

type networkMode int

const (
	networkNone networkMode = iota
	networkFull
	networkProxy
)

// writableRoot is a directory commands may write to except for its
// protected paths.
type writableRoot struct {
	path      string
	protected []string
}

// spec resolves the policy for a command of the tools working in dir.
func (s *SystemSandbox) spec(dir string) (*sandboxSpec, error) {
	p := s.policy
	home, _ := os.UserHomeDir()
	resolve := func(path string) string {
		switch {
		case path == "~":
			path = home
		case strings.HasPrefix(path, "~/"):
			path = filepath.Join(home, path[2:])
		case !filepath.IsAbs(path):
			path = filepath.Join(dir, path)
		}
		return filepath.Clean(path)
	}
	roots := []string{resolve(dir)}
	for _, r := range p.WritableRoots {
		roots = append(roots, resolve(r))
	}
	if !p.NoTempDirs {
		roots = append(roots, os.TempDir())
		if runtime.GOOS != "windows" {
			roots = append(roots, "/tmp")
		}
	}
	sp := &sandboxSpec{localBinding: p.AllowLocalBinding}
	var canon []string
	for _, r := range roots {
		c := canonicalPath(r)
		if !slices.Contains(canon, c) {
			canon = append(canon, c)
		}
	}
	// Check the user's roots against all roots: a symbolic link a
	// command can write could point the next command's root elsewhere.
	for i, r := range roots {
		if link := writableLink(r, canon); link != "" {
			return nil, fmt.Errorf("writable root %s passes through the symbolic link %s, which sandboxed commands could change", roots[i], link)
		}
	}
	names := p.ProtectedNames
	if names == nil {
		names = DefaultProtectedNames()
	}
	for _, c := range canon {
		sp.writable = append(sp.writable, writableRoot{path: c, protected: protectedPaths(c, names)})
	}
	if len(p.ReadableRoots) > 0 {
		for _, r := range p.ReadableRoots {
			sp.readable = append(sp.readable, canonicalPath(resolve(r)))
		}
	}
	for _, d := range p.DenyRead {
		r := resolve(d)
		for _, v := range []string{r, canonicalPath(r)} {
			if !slices.Contains(sp.denyRead, v) {
				sp.denyRead = append(sp.denyRead, v)
			}
		}
	}
	if runtime.GOOS == "linux" && !p.AllowNetwork {
		sp.sockets = daemonSockets()
	}
	switch {
	case p.AllowNetwork:
		sp.network = networkFull
	case len(p.AllowedDomains) > 0:
		sp.network = networkProxy
	}
	return sp, nil
}

// daemonSockets returns the sockets of container daemons that exist on
// this system. A command that could connect to one could start a
// container with the host's filesystem, so bubblewrap hides them; Seatbelt
// denies Unix sockets unless the network is allowed.
func daemonSockets() []string {
	paths := []string{
		"/var/run/docker.sock", "/run/docker.sock", "/run/containerd/containerd.sock",
		"/run/podman/podman.sock", "/var/run/crio/crio.sock", "/run/k3s/containerd/containerd.sock",
	}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		paths = append(paths, filepath.Join(xdg, "docker.sock"), filepath.Join(xdg, "podman", "podman.sock"))
	}
	var out []string
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && fi.Mode()&os.ModeSocket != 0 {
			out = append(out, p)
		}
	}
	return out
}

// canonicalPath returns the absolute path p with symbolic links resolved.
// For a path that does not exist, the links of its existing ancestors are
// resolved.
func canonicalPath(p string) string {
	p = filepath.Clean(p)
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(canonicalPath(parent), filepath.Base(p))
}

// writableLink returns a symbolic link on the path to root that lies in a
// directory under one of the canonical writable roots, or "".
func writableLink(root string, canon []string) string {
	for p := filepath.Clean(root); ; {
		parent := filepath.Dir(p)
		if parent == p {
			return ""
		}
		if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			cp := canonicalPath(parent)
			for _, c := range canon {
				if within(c, cp) {
					return p
				}
			}
		}
		p = parent
	}
}

// within reports whether p is root or below it.
func within(root, p string) bool {
	r, err := filepath.Rel(root, p)
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) && !filepath.IsAbs(r)
}

// protectedPaths returns the paths that stay read-only in the canonical
// writable root: each name, whether or not it exists, the target of a
// name that is a symbolic link, and the git directory a .git file points
// to.
func protectedPaths(root string, names []string) []string {
	var out []string
	add := func(p string) {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	for _, name := range names {
		if name == "" || strings.ContainsRune(name, filepath.Separator) || name == "." || name == ".." {
			continue
		}
		p := filepath.Join(root, name)
		add(p)
		fi, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			add(canonicalPath(p))
			continue
		}
		if name == ".git" && fi.Mode().IsRegular() {
			if gitdir := readGitdir(p); gitdir != "" {
				add(canonicalPath(gitdir))
			}
		}
	}
	return out
}

// readGitdir returns the directory named by the "gitdir:" line of a .git
// file, or "".
func readGitdir(gitFile string) string {
	f, err := os.Open(gitFile)
	if err != nil {
		return ""
	}
	defer f.Close()
	line, _ := bufio.NewReader(f).ReadString('\n')
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok {
		return ""
	}
	gitdir = strings.TrimSpace(gitdir)
	if gitdir == "" {
		return ""
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(filepath.Dir(gitFile), gitdir)
	}
	return filepath.Clean(gitdir)
}
