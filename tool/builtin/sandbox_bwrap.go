package builtin

import (
	"fmt"
	"os"
	"strings"
)

// The bubblewrap arguments follow Codex's Linux sandbox
// (codex-rs/linux-sandbox/README.md and src/bwrap.rs): a read-only view of
// / with a minimal /dev, writable roots bound read-write over it, protected
// paths bound read-only again, fresh user, PID and IPC namespaces, a fresh
// /proc, a network namespace of its own unless the network is allowed, a
// new session against terminal injection, and death with the parent.
// Unlike Codex it installs no seccomp filter and does not mask paths that
// do not exist yet.

// bwrapProxyPort is the port on which the bridge to the proxy listens
// inside the sandbox's network namespace.
const bwrapProxyPort = 3128

// bwrapSystemReads are the directories bound read-only when reads are
// restricted to ReadableRoots.
var bwrapSystemReads = []string{
	"/bin", "/sbin", "/usr", "/lib", "/lib32", "/lib64", "/libx32", "/etc",
	"/opt", "/nix", "/run/current-system", "/var/lib/alternatives",
}

// bwrapArgs returns the arguments of bwrap, without the program name, that
// run argv in cwd in the sandbox described by spec.
func bwrapArgs(sp *sandboxSpec, cwd string, argv []string) []string {
	args := []string{"--new-session", "--die-with-parent"}
	if sp.readable == nil {
		args = append(args, "--ro-bind", "/", "/")
	} else {
		args = append(args, "--tmpfs", "/")
		for _, p := range bwrapSystemReads {
			args = append(args, "--ro-bind-try", p, p)
		}
		for _, p := range sp.readable {
			args = append(args, "--ro-bind-try", p, p)
		}
	}
	args = append(args, "--dev", "/dev")
	if sp.readable != nil {
		// Programs resolve cwd and read their temporary files here.
		args = append(args, "--tmpfs", "/tmp")
	}
	for _, w := range sp.writable {
		// bwrap needs mount points to exist.
		if !exists(w.path) {
			continue
		}
		args = append(args, "--bind", w.path, w.path)
	}
	if sp.proxySocket != "" {
		// The socket's directory may lie outside the writable roots.
		dir, _, _ := strings.CutLast(sp.proxySocket, "/")
		args = append(args, "--bind", dir, dir)
	}
	// A later writable bind can cover an earlier read-only mount, even
	// when it binds an ancestor (for example /tmp over a working directory
	// below /tmp). Apply protections only after all writable mounts.
	for _, w := range sp.writable {
		if !exists(w.path) {
			continue
		}
		for _, p := range w.protected {
			if exists(p) {
				args = append(args, "--ro-bind", p, p)
			}
		}
	}
	for _, d := range sp.denyRead {
		fi, err := os.Stat(d)
		switch {
		case err != nil:
		case fi.IsDir():
			args = append(args, "--tmpfs", d, "--remount-ro", d)
		default:
			args = append(args, "--ro-bind", "/dev/null", d)
		}
	}
	for _, sock := range sp.sockets {
		args = append(args, "--ro-bind", "/dev/null", sock)
	}
	args = append(args, "--unshare-user", "--unshare-pid", "--unshare-ipc")
	if sp.network != networkFull {
		args = append(args, "--unshare-net")
	}
	args = append(args, "--proc", "/proc", "--chdir", cwd, "--cap-drop", "ALL", "--")
	return append(args, argv...)
}

// bwrapBridgeScript returns a sh script that relays the proxy's port
// inside the sandbox to its Unix socket with socat, waits until the relay
// accepts connections and then runs its arguments.
func bwrapBridgeScript(socat, socket string) string {
	listen := fmt.Sprintf("TCP-LISTEN:%d,bind=127.0.0.1,fork,reuseaddr", bwrapProxyPort)
	connect := fmt.Sprintf("TCP:127.0.0.1:%d", bwrapProxyPort)
	return fmt.Sprintf(`%s %s %s >/dev/null 2>&1 &
i=0
while [ "$i" -lt 200 ] && ! %s -u OPEN:/dev/null %s >/dev/null 2>&1; do sleep 0.01; i=$((i+1)); done
exec "$@"`, shellQuote(socat), shellQuote(listen), shellQuote("UNIX-CONNECT:"+socket), shellQuote(socat), shellQuote(connect))
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
