// Package process implements agentx tool and hook command execution, including
// platform-specific process-tree cleanup. Commands execute caller-supplied scripts;
// this package is not a sandbox or an authorization boundary. Tree setup and Start
// must complete before concurrent cancellation; exec.Cmd still requires one waiter.
package process
