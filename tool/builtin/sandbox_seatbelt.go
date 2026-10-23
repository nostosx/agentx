package builtin

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// The Seatbelt profile follows Codex's (codex-rs/sandboxing/src/seatbelt.rs
// and its seatbelt_*.sbpl policies, which build on Chromium's): deny by
// default, let processes fork, exec and signal within the sandbox, allow
// the sysctls, Mach services and devices ordinary programs need, then add
// file, network and protection rules. Paths are passed to sandbox-exec as
// -D parameters and referenced with (param "..."), so they never need
// escaping inside the profile. Later rules take precedence, so the denials
// come last.

// seatbeltBase is Codex's seatbelt_base_policy.sbpl, with the standard
// streams writable and pgrep's process listing allowed.
const seatbeltBase = `(version 1)
(deny default)

; child processes inherit the policy of their parent
(allow process-exec)
(allow process-fork)
(allow signal (target same-sandbox))
(allow process-info* (target same-sandbox))

(allow file-write-data
  (require-all
    (path "/dev/null")
    (vnode-type CHARACTER-DEVICE)))

(allow sysctl-read
  (sysctl-name "hw.activecpu")
  (sysctl-name "hw.busfrequency_compat")
  (sysctl-name "hw.byteorder")
  (sysctl-name "hw.cacheconfig")
  (sysctl-name "hw.cachelinesize_compat")
  (sysctl-name "hw.cpufamily")
  (sysctl-name "hw.cpufrequency_compat")
  (sysctl-name "hw.cputype")
  (sysctl-name "hw.l1dcachesize_compat")
  (sysctl-name "hw.l1icachesize_compat")
  (sysctl-name "hw.l2cachesize_compat")
  (sysctl-name "hw.l3cachesize_compat")
  (sysctl-name "hw.logicalcpu_max")
  (sysctl-name "hw.machine")
  (sysctl-name "hw.model")
  (sysctl-name "hw.memsize")
  (sysctl-name "hw.ncpu")
  (sysctl-name "hw.nperflevels")
  (sysctl-name-prefix "hw.optional.arm.")
  (sysctl-name-prefix "hw.optional.armv8_")
  (sysctl-name "hw.packages")
  (sysctl-name "hw.pagesize_compat")
  (sysctl-name "hw.pagesize")
  (sysctl-name "hw.physicalcpu")
  (sysctl-name "hw.physicalcpu_max")
  (sysctl-name "hw.logicalcpu")
  (sysctl-name "hw.cpufrequency")
  (sysctl-name "hw.tbfrequency_compat")
  (sysctl-name "hw.vectorunit")
  (sysctl-name "machdep.cpu.brand_string")
  (sysctl-name "kern.argmax")
  (sysctl-name "kern.hostname")
  (sysctl-name "kern.maxfilesperproc")
  (sysctl-name "kern.maxproc")
  (sysctl-name "kern.osproductversion")
  (sysctl-name "kern.osrelease")
  (sysctl-name "kern.ostype")
  (sysctl-name "kern.osvariant_status")
  (sysctl-name "kern.osversion")
  (sysctl-name "kern.secure_kernel")
  (sysctl-name "kern.sysv.semmns")
  (sysctl-name "kern.usrstack64")
  (sysctl-name "kern.version")
  (sysctl-name "sysctl.proc_cputype")
  (sysctl-name "vm.loadavg")
  (sysctl-name-prefix "hw.perflevel")
  (sysctl-name-prefix "kern.proc.pgrp.")
  (sysctl-name-prefix "kern.proc.pid.")
  (sysctl-name-prefix "net.routetable."))
(allow sysctl-write
  (sysctl-name "kern.grade_cputype"))

(allow iokit-open
  (iokit-registry-entry-class "RootDomainUserClient"))
(allow mach-lookup
  (global-name "com.apple.system.opendirectoryd.libinfo")
  (global-name "com.apple.PowerManagement.control")
  ; process listing for pgrep, from Gemini CLI's profiles
  (global-name "com.apple.sysmond"))
(allow ipc-posix-sem)
(allow ipc-posix-shm-read-data
  ipc-posix-shm-write-create
  ipc-posix-shm-write-unlink
  (ipc-posix-name-regex #"^/__KMP_REGISTERED_LIB_[0-9]+$"))

(allow pseudo-tty)
(allow file-read* file-write* file-ioctl (literal "/dev/ptmx"))
(allow file-read* file-write*
  (require-all
    (regex #"^/dev/ttys[0-9]+")
    (extension "com.apple.sandbox.pty")))
(allow file-ioctl (regex #"^/dev/ttys[0-9]+"))

; standard streams and descriptors opened before the sandbox
(allow file-write-data
  (literal "/dev/stdout")
  (literal "/dev/stderr")
  (literal "/dev/tty")
  (literal "/dev/zero")
  (subpath "/dev/fd"))
`

// seatbeltPreferences is Codex's seatbelt_preferences_policy.sbpl, used
// when reads are unrestricted.
const seatbeltPreferences = `(allow ipc-posix-shm-read* (ipc-posix-name-prefix "apple.cfprefs."))
(allow mach-lookup
  (global-name "com.apple.cfprefsd.daemon")
  (global-name "com.apple.cfprefsd.agent")
  (local-name "com.apple.cfprefsd.agent"))
(allow user-preference-read)
`

// seatbeltNetwork is Codex's seatbelt_network_policy.sbpl and TLS trust
// rule, added whenever commands may use the network or the proxy.
const seatbeltNetwork = `(allow system-socket
  (require-all
    (socket-domain AF_SYSTEM)
    (socket-protocol 2)))
(allow mach-lookup
  (global-name "com.apple.bsd.dirhelper")
  (global-name "com.apple.system.opendirectoryd.membership")
  (global-name "com.apple.SecurityServer")
  (global-name "com.apple.networkd")
  (global-name "com.apple.ocspd")
  (global-name "com.apple.trustd.agent")
  (global-name "com.apple.SystemConfiguration.DNSConfiguration")
  (global-name "com.apple.SystemConfiguration.configd")
  (global-name "com.apple.TrustEvaluationAgent"))
(allow sysctl-read
  (sysctl-name-regex #"^net.routetable"))
`

// seatbeltSystemReads are the directories programs need to run when reads
// are restricted to ReadableRoots, after Codex's
// seatbelt_read_only_platform_defaults.sbpl, plus the package managers'
// usual prefixes.
var seatbeltSystemReads = []string{
	"/bin", "/sbin", "/usr", "/System", "/Library", "/Applications",
	"/private/etc", "/private/var/db", "/private/var/select", "/dev",
	"/opt/homebrew", "/opt/local", "/nix",
}

// sbParam is a -D parameter of sandbox-exec.
type sbParam struct{ key, value string }

// seatbeltProfile returns the profile for spec and the parameters it
// references.
func seatbeltProfile(sp *sandboxSpec) (string, []sbParam) {
	var params []sbParam
	param := func(prefix string, value string) string {
		key := fmt.Sprintf("%s_%d", prefix, len(params))
		params = append(params, sbParam{key, value})
		return fmt.Sprintf("(param %q)", key)
	}
	var b strings.Builder
	b.WriteString(seatbeltBase)

	// Reads.
	if sp.readable == nil {
		b.WriteString("\n; reads are unrestricted\n(allow file-read*)\n")
		b.WriteString(seatbeltPreferences)
	} else {
		b.WriteString("\n; reads are restricted; metadata stays readable so paths resolve\n(allow file-read-metadata)\n(allow file-read*\n  (literal \"/\")")
		for _, p := range seatbeltSystemReads {
			fmt.Fprintf(&b, "\n  (subpath %s)", param("SYSTEM_READ", p))
		}
		var roots []string
		roots = append(roots, sp.readable...)
		for _, w := range sp.writable {
			roots = append(roots, w.path)
		}
		for _, r := range roots {
			fmt.Fprintf(&b, "\n  (subpath %s)", param("READABLE_ROOT", r))
		}
		b.WriteString(")\n")
	}

	// Writes: each root except its protected paths, which neither exist
	// as a literal nor have anything beneath them, so they cannot be
	// created either.
	var anchors, protectedAncestors []string
	if len(sp.writable) > 0 {
		b.WriteString("\n; writable roots\n(allow file-write*")
		for _, w := range sp.writable {
			root := param("WRITABLE_ROOT", w.path)
			anchors = append(anchors, root)
			if len(w.protected) == 0 {
				fmt.Fprintf(&b, "\n  (subpath %s)", root)
				continue
			}
			fmt.Fprintf(&b, "\n  (require-all (subpath %s)", root)
			for _, p := range w.protected {
				pp := param("PROTECTED", p)
				fmt.Fprintf(&b, "\n    (require-not (literal %s)) (require-not (subpath %s))", pp, pp)
				// Renaming a directory between the root and a protected
				// path would move the path out of its exclusion.
				for a := filepath.Dir(p); within(w.path, a) && a != w.path; a = filepath.Dir(a) {
					if !slices.Contains(protectedAncestors, a) {
						protectedAncestors = append(protectedAncestors, a)
					}
				}
			}
			b.WriteString(")")
		}
		b.WriteString(")\n")
	}

	// Network.
	switch sp.network {
	case networkFull:
		b.WriteString("\n; full network\n(allow network-outbound)\n(allow network-inbound)\n(allow network-bind)\n")
		b.WriteString(seatbeltNetwork)
	case networkProxy:
		if sp.proxyPort != 0 {
			fmt.Fprintf(&b, "\n; only the proxy\n(allow network-outbound (remote ip \"localhost:%d\"))\n", sp.proxyPort)
			b.WriteString(seatbeltNetwork)
		}
	}
	if sp.localBinding && sp.network != networkFull {
		b.WriteString("\n; loopback servers\n(allow network-bind (local ip \"*:*\"))\n(allow network-inbound (local ip \"localhost:*\"))\n(allow network-outbound (remote ip \"localhost:*\"))\n")
	}

	// Denials, last so that no allowance above reopens them.
	b.WriteString("\n; denials\n(deny mach-lookup (xpc-service-name-prefix \"\"))\n")
	for _, a := range anchors {
		// A command must not replace a root the next policy is built on.
		fmt.Fprintf(&b, "(deny file-write-unlink (require-all (literal %s) (vnode-type DIRECTORY)))\n", a)
	}
	for _, a := range protectedAncestors {
		fmt.Fprintf(&b, "(deny file-write-unlink (require-all (literal %s) (vnode-type DIRECTORY)))\n", param("PROTECTED_ANCESTOR", a))
	}
	for _, d := range sp.denyRead {
		dp := param("DENY", d)
		fmt.Fprintf(&b, "(deny file-read* file-write* (literal %s) (subpath %s))\n", dp, dp)
	}
	// These fcntls change files through read-only descriptors.
	b.WriteString("(deny system-fcntl (fcntl-command 80 110))\n")
	return b.String(), params
}
