package permission_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/permission"
	"github.com/nostosx/agentx/tool"
)

// Tool infos like those of the built-in tools.
var (
	bashInfo = tool.Info{Name: "bash", RequiresApproval: true, Parameters: &tool.Schema{Type: "object", Properties: map[string]*tool.Schema{
		"command": {Type: "string"}, "timeout_ms": {Type: "integer"},
	}}}
	readInfo  = tool.Info{Name: "read_file", ReadOnly: true}
	writeInfo = tool.Info{Name: "write_file", RequiresApproval: true}
	editInfo  = tool.Info{Name: "edit_file", RequiresApproval: true}
	fetchInfo = tool.Info{Name: "web_fetch", ReadOnly: true}
	mcpInfo   = tool.Info{Name: "mcp", RequiresApproval: false}
)

type env struct {
	dir, home string
}

func newEnv(t *testing.T) env {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return env{dir: filepath.Join(dir, "project"), home: filepath.Join(dir, "home")}
}

func (e env) policy(t *testing.T, mode permission.Mode, rules ...permission.Rules) *permission.Policy {
	t.Helper()
	for _, d := range []string{e.dir, e.home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p, err := permission.New(permission.Config{Mode: mode, Dir: e.dir, Home: e.home, Rules: rules})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func args(kv ...string) json.RawMessage {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	b, _ := json.Marshal(m)
	return b
}

func decide(p *permission.Policy, mode permission.Mode, info tool.Info, a json.RawMessage) permission.Verdict {
	return p.Decide(&permission.Request{Mode: mode, Name: info.Name, Info: info, Known: true, Arguments: a})
}

func bash(p *permission.Policy, mode permission.Mode, cmd string) loop.Decision {
	return decide(p, mode, bashInfo, args("command", cmd)).Decision
}

func name(d loop.Decision) string {
	return [...]string{"none", "allow", "ask", "deny"}[d]
}

const (
	none  = loop.NoDecision
	allow = loop.Allowed
	ask   = loop.Asked
	deny  = loop.Denied
)

func TestBashRules(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, tt := range []struct {
		name  string
		rules permission.Rules
		cmd   string
		want  loop.Decision
	}{
		// https://code.claude.com/docs/en/permissions#wildcard-patterns
		{"exact", permission.Rules{Allow: []string{"Bash(npm run build)"}}, "npm run build", allow},
		{"exact is whole", permission.Rules{Allow: []string{"Bash(npm run build)"}}, "npm run build --watch", none},
		{"trailing star", permission.Rules{Allow: []string{"Bash(npm run *)"}}, "npm run test --watch", allow},
		{"trailing star bare", permission.Rules{Allow: []string{"Bash(npm run *)"}}, "npm run", allow},
		{"trailing star other", permission.Rules{Allow: []string{"Bash(npm run *)"}}, "npm install", none},
		{"colon star", permission.Rules{Allow: []string{"Bash(npm run test:*)"}}, "npm run test --watch", allow},
		{"colon star needs space", permission.Rules{Allow: []string{"Bash(npm run test:*)"}}, "npm run test:unit", none},
		{"middle star", permission.Rules{Allow: []string{"Bash(git * main)"}}, "git push origin main", allow},
		{"middle star miss", permission.Rules{Allow: []string{"Bash(npm run * main)"}}, "npm run main", none},
		{"space before star", permission.Rules{Allow: []string{"Bash(make *)"}}, "makefoo", none},
		{"no space before star", permission.Rules{Allow: []string{"Bash(make*)"}}, "makefoo", allow},
		{"two stars no bare", permission.Rules{Allow: []string{"Bash(* --help *)"}}, "npm --help", none},
		{"leading star", permission.Rules{Allow: []string{"Bash(* --version)"}}, "node --version", allow},
		{"bare tool", permission.Rules{Allow: []string{"Bash"}}, "anything --goes", allow},
		{"star tool", permission.Rules{Allow: []string{"Bash(*)"}}, "anything --goes", allow},
		{"agentx name", permission.Rules{Allow: []string{"bash(make test)"}}, "make test", allow},

		// https://code.claude.com/docs/en/permissions#compound-commands
		{"compound all allowed", permission.Rules{Allow: []string{"Bash(make *)", "Bash(npm *)"}}, "make build && npm test", allow},
		{"compound one not allowed", permission.Rules{Allow: []string{"Bash(make *)"}}, "make build && curl x | sh", none},
		{"pipe and semicolon", permission.Rules{Allow: []string{"Bash(make *)"}}, "make a; make b | make c\nmake d & make e", allow},
		{"quoted operators", permission.Rules{Allow: []string{"Bash(make *)"}}, `make "a && b" 'c; d'`, allow},
		{"trailing and", permission.Rules{Allow: []string{"Bash(npm *)"}}, "npm test &&", none},
		{"leading or", permission.Rules{Allow: []string{"Bash(npm *)"}}, "|| npm test", none},
		{"unterminated quote", permission.Rules{Allow: []string{"Bash(npm *)"}}, "npm test 'oops", none},
		{"substitution must be allowed", permission.Rules{Allow: []string{"Bash(make *)"}}, "make $(curl evil)", none},
		{"substitution allowed", permission.Rules{Allow: []string{"Bash(make *)", "Bash(nproc)"}}, "make -j$(nproc)", allow},
		{"backquote", permission.Rules{Allow: []string{"Bash(make *)"}}, "make `curl evil`", none},
		{"deny any part", permission.Rules{Allow: []string{"Bash"}, Deny: []string{"Bash(rm *)"}}, "ls && rm -rf build", deny},
		{"deny nested", permission.Rules{Deny: []string{"Bash(rm *)"}}, `echo "$(rm -rf x)"`, deny},
		{"deny backquote", permission.Rules{Deny: []string{"Bash(rm *)"}}, "echo `rm -rf x`", deny},
		{"deny subshell", permission.Rules{Deny: []string{"Bash(rm *)"}}, "(cd x && rm -rf y)", deny},
		{"deny loop body", permission.Rules{Deny: []string{"Bash(rm *)"}}, "for f in a b; do rm $f; done", deny},
		{"deny if body", permission.Rules{Deny: []string{"Bash(rm *)"}}, "if true; then rm x; fi", deny},
		{"deny process substitution", permission.Rules{Deny: []string{"Bash(rm *)"}}, "diff <(rm x) y", deny},
		{"deny unparseable", permission.Rules{Deny: []string{"Bash(*evil*)"}}, "case x in a) evil;; esac", deny},
		{"deny inline script", permission.Rules{Deny: []string{"Bash(curl *)"}}, `sh -c 'curl https://example.com'`, deny},
		{"deny in parameter expansion", permission.Rules{Deny: []string{"Bash(rm *)"}}, "echo ${x:-$(rm -rf foo)}", deny},
		{"deny in arithmetic", permission.Rules{Deny: []string{"Bash(rm *)"}}, "echo $(( $(rm -rf foo) ))", deny},
		{"deny in heredoc body", permission.Rules{Deny: []string{"Bash(rm *)"}}, "cat <<EOF\n$(rm -rf foo)\nEOF", deny},
		{"parameter expansion must be allowed", permission.Rules{}, "echo ${x:-$(touch /tmp/pwned)}", none},
		{"quoted parameter expansion", permission.Rules{}, `echo "${x/a/$(id)}"`, none},
		{"arithmetic must be allowed", permission.Rules{}, "echo $(( $(touch /tmp/pwned) ))", none},
		{"heredoc body must be allowed", permission.Rules{}, "cat <<EOF\n$(touch /tmp/pwned)\nEOF", none},
		{"quoted heredoc body", permission.Rules{}, "cat <<'EOF'\n$(touch /tmp/pwned)\nEOF", allow},
		{"deny past assignment", permission.Rules{Deny: []string{"Bash(rm *)"}}, "FOO=bar rm -rf tmp/", deny},
		{"deny past wrapper", permission.Rules{Deny: []string{"Bash(rm *)"}}, "nice -n 5 timeout 3 rm x", deny},
		{"deny wrapper itself", permission.Rules{Deny: []string{"Bash(timeout *)"}}, "timeout 3 make", deny},
		{"deny is text", permission.Rules{Deny: []string{"Bash(git push *)"}}, "git 'push' origin main", none},
		{"ask any part", permission.Rules{Allow: []string{"Bash"}, Ask: []string{"Bash(git push *)"}}, "git status; git push origin main", ask},
		{"deny over ask over allow", permission.Rules{Allow: []string{"Bash(aws s3 ls)"}, Deny: []string{"Bash(aws *)"}}, "aws s3 ls", deny},
		{"ask over allow", permission.Rules{Allow: []string{"Bash(git push origin main)"}, Ask: []string{"Bash(git push *)"}}, "git push origin main", ask},

		// https://code.claude.com/docs/en/permissions#wrappers
		{"wrapper stripped", permission.Rules{Allow: []string{"Bash(npm test *)"}}, "timeout 30 npm test", allow},
		{"wrappers stripped", permission.Rules{Allow: []string{"Bash(npm test *)"}}, "nohup nice -n 10 stdbuf -oL time command npm test", allow},
		{"command -v kept", permission.Rules{Allow: []string{"Bash(npm *)"}}, "command -v npm", none},
		{"safe env stripped", permission.Rules{Allow: []string{"Bash(npm test *)"}}, "NODE_ENV=test npm test", allow},
		{"unsafe env kept", permission.Rules{Allow: []string{"Bash(npm test *)"}}, "LD_PRELOAD=x.so npm test", none},
		{"bare xargs stripped", permission.Rules{Allow: []string{"Bash(make *)"}}, "xargs make", allow},
		{"xargs flags kept", permission.Rules{Allow: []string{"Bash(make *)"}}, "xargs -n1 make", none},
		{"exec wrapper wildcard", permission.Rules{Allow: []string{"Bash(watch *)"}}, "watch make", none},
		{"exec wrapper exact", permission.Rules{Allow: []string{"Bash(watch make)"}}, "watch make", allow},
		{"find action wildcard", permission.Rules{Allow: []string{"Bash(find *)"}}, "find build -type f -delete", none},
		{"find action exact", permission.Rules{Allow: []string{"Bash(find build -type f -delete)"}}, "find build -type f -delete", allow},

		// https://code.claude.com/docs/en/permissions#read-only-commands
		{"read-only", permission.Rules{}, "ls -la | grep foo | wc -l", allow},
		{"read-only cat outside", permission.Rules{}, "cat /etc/hosts", none},
		{"read-only cat home", permission.Rules{}, "cat ~/.ssh/id_rsa", none},
		{"read-only cat variable", permission.Rules{}, "cat $HOME/.ssh/id_rsa", none},
		{"read-only cat parent", permission.Rules{}, "cat ../secret", none},
		{"read-only cat inside", permission.Rules{}, "cat notes.txt sub/a.go", allow},
		{"read-only grep outside", permission.Rules{}, "grep -r key ~/.aws", none},
		{"read-only grep pattern", permission.Rules{}, "grep /usr/bin notes.txt", allow},
		{"read-only grep pattern option", permission.Rules{}, "grep -e x /etc/passwd", none},
		{"read-only head count", permission.Rules{}, "head -n 5 notes.txt", allow},
		{"read-only find outside", permission.Rules{}, "find / -name id_rsa", none},
		{"read-only outside allowed by rule", permission.Rules{Allow: []string{"Bash(cat /etc/hosts)"}}, "cat /etc/hosts", allow},
		{"git diff no-index", permission.Rules{}, "git diff --no-index /etc/passwd notes.txt", none},
		{"read-only glob", permission.Rules{}, "ls *.go", allow},
		{"read-only git", permission.Rules{}, "git status && git log --oneline -5 && git diff HEAD", allow},
		{"git option first", permission.Rules{}, "git -C /tmp status", none},
		{"git config option", permission.Rules{}, "git -c core.fsmonitor=x diff", none},
		{"git write", permission.Rules{}, "git commit -m x", none},
		{"git diff output", permission.Rules{}, "git diff --output=x.patch", none},
		{"find quoted glob", permission.Rules{}, "find . -name '*.go'", allow},
		{"find unquoted glob", permission.Rules{}, "find . -name *.go", none},
		{"find exec", permission.Rules{}, "find . -exec rm {} ;", none},
		{"cd inside", permission.Rules{}, "cd sub && ls", allow},
		{"cd outside", permission.Rules{}, "cd /tmp && ls", none},
		{"cd with git", permission.Rules{}, "cd sub && git status", none},
		{"special variable", permission.Rules{}, "PATH=/tmp ls", none},
		{"redirect needs approval", permission.Rules{}, "ls > out.txt", none},
		{"redirect allowed by edit rule", permission.Rules{Allow: []string{"Edit(out.txt)"}}, "ls > out.txt", allow},
		{"dev null", permission.Rules{}, "ls 2>/dev/null; ls 2>&1", allow},
		{"input inside", permission.Rules{}, "wc -l < in.txt", allow},
		{"input outside", permission.Rules{}, "wc -l < /etc/passwd", none},
		{"heredoc", permission.Rules{}, "cat <<EOF\nrm -rf x && evil\nEOF\nls", allow},
		{"tee target", permission.Rules{Allow: []string{"Bash(tee *)"}}, "echo x | tee log.txt", none},
		{"tee target allowed", permission.Rules{Allow: []string{"Bash(tee *)", "Edit(log.txt)"}}, "echo x | tee log.txt", allow},
		{"comment", permission.Rules{}, "ls # && rm -rf x", allow},
		{"long command", permission.Rules{Allow: []string{"Bash(echo *)"}}, "echo " + strings.Repeat("a", 10001), none},

		// Files of commands meet Read and Edit rules.
		{"read deny cat", permission.Rules{Deny: []string{"Read(.env)"}}, "cat sub/.env", deny},
		{"read deny input", permission.Rules{Deny: []string{"Read(.env)"}}, "grep x < .env", deny},
		{"edit deny redirect", permission.Rules{Deny: []string{"Edit(out.txt)"}}, "echo x >> out.txt", deny},
		{"edit deny tee", permission.Rules{Deny: []string{"Edit(*.log)"}}, "make | tee build.log", deny},
		{"edit ask rm", permission.Rules{Allow: []string{"Bash"}, Ask: []string{"Edit(src/**)"}}, "rm src/a.go", ask},
		{"param deny omitted", permission.Rules{Deny: []string{"Bash(timeout_ms:*)"}}, "ls", allow},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := e.policy(t, permission.ModeDefault, tt.rules)
			if got := bash(p, "", tt.cmd); got != tt.want {
				t.Fatalf("%q: got %s, want %s", tt.cmd, name(got), name(tt.want))
			}
		})
	}
}

func TestParamRule(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.policy(t, "", permission.Rules{Deny: []string{"Bash(timeout_ms:6*)"}})
	if got := decide(p, "", bashInfo, json.RawMessage(`{"command":"ls","timeout_ms":60000}`)).Decision; got != deny {
		t.Fatalf("matching parameter: got %s", name(got))
	}
	if got := decide(p, "", bashInfo, json.RawMessage(`{"command":"ls","timeout_ms":1000}`)).Decision; got != allow {
		t.Fatalf("other value: got %s", name(got))
	}
	if got := decide(p, "", bashInfo, json.RawMessage(`{"command":"ls"}`)).Decision; got != allow {
		t.Fatalf("omitted parameter: got %s", name(got))
	}
}

func TestCriticalRemovals(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, tt := range []struct {
		cmd  string
		mode permission.Mode
		want loop.Decision
	}{
		{"rm -rf ~", permission.ModeBypassPermissions, ask},
		{"rm -rf ~", permission.ModeDontAsk, deny},
		{"rm -rf /", "", ask},
		{"rm -rf /usr", "", ask},
		{"rm -rf $HOME", "", ask},
		{"rm -rf .", "", ask},
		{"rm -rf *", "", ask},
		{"rm -rf ..", "", ask},
		{`rm -rf "$DIR"/*`, "", ask},
		{`rm -rf "${DIR:?}"/*`, "", allow},
		{`rm -rf "$TMPDIR/usr"`, "", ask},
		{`rm -rf "$(pwd)"`, "", ask},
		{`rm -rf ~/$(echo x)`, "", ask},
		{`rm -rf logs/*/*`, "", ask},
		{`bash -c 'rm -rf ~'`, permission.ModeBypassPermissions, ask},
		{"rm -rf build", "", allow},
		{"rmdir /", permission.ModeAcceptEdits, ask},
	} {
		p := e.policy(t, permission.ModeDefault, permission.Rules{Allow: []string{"Bash(rm *)", "Bash(bash *)", "Bash(rmdir *)"}})
		if got := bash(p, tt.mode, tt.cmd); got != tt.want {
			t.Errorf("%q in %q: got %s, want %s", tt.cmd, tt.mode, name(got), name(tt.want))
		}
	}
}

func TestModes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	edit := args("path", "src/a.go")
	for _, tt := range []struct {
		name  string
		mode  permission.Mode
		rules permission.Rules
		info  tool.Info
		args  json.RawMessage
		prior loop.Decision
		want  loop.Decision
	}{
		{"default edit", permission.ModeDefault, permission.Rules{}, writeInfo, edit, none, none},
		{"default read-only bash", permission.ModeDefault, permission.Rules{}, bashInfo, args("command", "ls"), none, allow},
		{"accept edits inside", permission.ModeAcceptEdits, permission.Rules{}, writeInfo, edit, none, allow},
		{"accept edits outside", permission.ModeAcceptEdits, permission.Rules{}, writeInfo, args("path", "/etc/hosts"), none, none},
		{"accept edits mkdir", permission.ModeAcceptEdits, permission.Rules{}, bashInfo, args("command", "mkdir -p a/b && touch a/b/c && cp a/b/c d && mv d e"), none, allow},
		{"accept edits mkdir outside", permission.ModeAcceptEdits, permission.Rules{}, bashInfo, args("command", "mkdir /tmp/x"), none, none},
		{"accept edits redirect", permission.ModeAcceptEdits, permission.Rules{}, bashInfo, args("command", "echo x > notes.txt"), none, allow},
		{"accept edits sed", permission.ModeAcceptEdits, permission.Rules{}, bashInfo, args("command", "sed -i s/a/b/ f.txt"), none, none},
		{"accept edits other command", permission.ModeAcceptEdits, permission.Rules{}, bashInfo, args("command", "npm install"), none, none},
		{"plan read", permission.ModePlan, permission.Rules{}, readInfo, args("path", "a.go"), none, none},
		{"plan read-only bash", permission.ModePlan, permission.Rules{}, bashInfo, args("command", "grep -r x . | head"), none, allow},
		{"plan bash", permission.ModePlan, permission.Rules{Allow: []string{"Bash(npm test)"}}, bashInfo, args("command", "npm test"), none, deny},
		{"plan redirect", permission.ModePlan, permission.Rules{}, bashInfo, args("command", "echo x > f"), none, deny},
		{"plan edit", permission.ModePlan, permission.Rules{Allow: []string{"Edit"}}, writeInfo, edit, none, deny},
		{"plan hook allow", permission.ModePlan, permission.Rules{}, writeInfo, edit, allow, deny},
		{"plan fetch", permission.ModePlan, permission.Rules{}, fetchInfo, args("url", "https://example.com"), none, none},
		{"plan bash_output", permission.ModePlan, permission.Rules{}, tool.Info{Name: "bash_output", Concurrent: true}, args("shell_id", "bash_1"), none, none},
		{"plan kill_shell", permission.ModePlan, permission.Rules{}, tool.Info{Name: "kill_shell"}, args("shell_id", "bash_1"), none, none},
		{"plan todo_write", permission.ModePlan, permission.Rules{}, tool.Info{Name: "todo_write"}, json.RawMessage(`{"todos":[]}`), none, none},
		{"plan other tool", permission.ModePlan, permission.Rules{}, tool.Info{Name: "send_mail"}, json.RawMessage(`{}`), none, deny},
		{"deny kill_shell", permission.ModeDefault, permission.Rules{Deny: []string{"KillShell"}}, tool.Info{Name: "kill_shell"}, args("shell_id", "bash_1"), none, deny},
		{"dont ask edit", permission.ModeDontAsk, permission.Rules{}, writeInfo, edit, none, deny},
		{"dont ask allowed edit", permission.ModeDontAsk, permission.Rules{Allow: []string{"Edit(src/**)"}}, writeInfo, edit, none, allow},
		{"dont ask read", permission.ModeDontAsk, permission.Rules{}, readInfo, args("path", "a.go"), none, none},
		{"dont ask read-only bash", permission.ModeDontAsk, permission.Rules{}, bashInfo, args("command", "ls"), none, allow},
		{"dont ask hook allow", permission.ModeDontAsk, permission.Rules{}, writeInfo, edit, allow, none},
		{"dont ask hook ask", permission.ModeDontAsk, permission.Rules{}, readInfo, args("path", "a.go"), ask, deny},
		{"dont ask ask rule", permission.ModeDontAsk, permission.Rules{Allow: []string{"Edit"}, Ask: []string{"Edit(src/**)"}}, writeInfo, edit, none, deny},
		{"bypass", permission.ModeBypassPermissions, permission.Rules{}, writeInfo, edit, none, allow},
		{"bypass deny", permission.ModeBypassPermissions, permission.Rules{Deny: []string{"Edit(src/**)"}}, writeInfo, edit, none, deny},
		{"bypass ask", permission.ModeBypassPermissions, permission.Rules{Ask: []string{"Edit(src/**)"}}, writeInfo, edit, none, ask},
		{"bypass protected", permission.ModeBypassPermissions, permission.Rules{}, writeInfo, args("path", ".git/config"), none, allow},
		{"protected default", permission.ModeDefault, permission.Rules{Allow: []string{"Edit"}}, writeInfo, args("path", ".git/config"), none, ask},
		{"protected accept edits", permission.ModeAcceptEdits, permission.Rules{}, editInfo, args("path", ".bashrc"), none, ask},
		{"protected dont ask", permission.ModeDontAsk, permission.Rules{Allow: []string{"Edit"}}, editInfo, args("path", ".claude/settings.json"), none, deny},
		{"protected bash", permission.ModeAcceptEdits, permission.Rules{}, bashInfo, args("command", "echo x > .husky/pre-commit"), none, ask},
		{"worktree not protected", permission.ModeAcceptEdits, permission.Rules{}, writeInfo, args("path", ".claude/worktrees/x/a.go"), none, allow},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := e.policy(t, permission.ModeDefault, tt.rules)
			v := p.Decide(&permission.Request{Mode: tt.mode, Name: tt.info.Name, Info: tt.info, Known: true, Arguments: tt.args, Prior: tt.prior})
			if v.Decision != tt.want {
				t.Fatalf("got %s (%s), want %s", name(v.Decision), v.Reason, name(tt.want))
			}
			if tt.mode == permission.ModePlan && v.Decision == deny && !strings.Contains(v.Reason, "Plan mode") {
				t.Fatalf("plan reason %q", v.Reason)
			}
		})
	}
}

func TestPathRules(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	other := filepath.Join(filepath.Dir(e.dir), "other")
	for _, tt := range []struct {
		name  string
		rules permission.Rules
		info  tool.Info
		path  string
		want  loop.Decision
	}{
		// https://code.claude.com/docs/en/permissions#read-and-edit
		{"allow dir", permission.Rules{Allow: []string{"Edit(src/**)"}}, writeInfo, "src/app.ts", allow},
		{"allow dir nested copy", permission.Rules{Allow: []string{"Edit(src/**)"}}, writeInfo, "vendor/pkg/src/lib.js", none},
		{"deny dir nested copy", permission.Rules{Allow: []string{"Edit"}, Deny: []string{"Edit(src/**)"}}, writeInfo, "vendor/pkg/src/lib.js", deny},
		{"anchored deny", permission.Rules{Allow: []string{"Edit"}, Deny: []string{"Edit(/src/**)"}}, writeInfo, "vendor/pkg/src/lib.js", allow},
		{"any depth", permission.Rules{Allow: []string{"Edit(**/src/**)"}}, writeInfo, "vendor/pkg/src/lib.js", allow},
		{"dot slash", permission.Rules{Deny: []string{"Read(./.env)"}}, readInfo, ".env", deny},
		{"dot slash is anchored", permission.Rules{Deny: []string{"Read(./.env)"}}, readInfo, "sub/.env", none},
		{"bare name any depth", permission.Rules{Deny: []string{"Read(.env)"}}, readInfo, "a/b/.env", deny},
		{"bare name star", permission.Rules{Deny: []string{"Read(*.env)"}}, readInfo, "a/prod.env", deny},
		{"absolute", permission.Rules{Deny: []string{"Read(//" + strings.TrimPrefix(filepath.ToSlash(other), "/") + "/**)"}}, readInfo, filepath.Join(other, "x"), deny},
		{"single slash is not absolute", permission.Rules{Deny: []string{"Read(" + filepath.ToSlash(other) + "/**)"}}, readInfo, filepath.Join(other, "x"), none},
		{"home", permission.Rules{Deny: []string{"Read(~/.ssh/**)"}}, readInfo, filepath.Join(e.home, ".ssh", "id_rsa"), deny},
		{"outside anchor", permission.Rules{Deny: []string{"Read(.env)"}}, readInfo, filepath.Join(other, ".env"), none},
		{"negation", permission.Rules{Deny: []string{"Read(*.env)", "Read(!sample.env)"}}, readInfo, "a/sample.env", none},
		{"negation keeps others", permission.Rules{Deny: []string{"Read(*.env)", "Read(!sample.env)"}}, readInfo, "a/prod.env", deny},
		{"negation first carves nothing", permission.Rules{Deny: []string{"Read(!sample.env)", "Read(*.env)"}}, readInfo, "sample.env", deny},
		{"negation cannot reopen dir", permission.Rules{Deny: []string{"Read(secrets/**)", "Read(!secrets/public/**)"}}, readInfo, "secrets/public/x", deny},
		{"directory name", permission.Rules{Deny: []string{"Read(secrets)"}}, readInfo, "secrets/key", deny},
		{"read deny blocks edit", permission.Rules{Allow: []string{"Edit"}, Deny: []string{"Read(.env)"}}, editInfo, ".env", deny},
		{"edit deny not read", permission.Rules{Deny: []string{"Edit(.env)"}}, readInfo, ".env", none},
		{"glob tool", permission.Rules{Deny: []string{"Read(secrets/**)"}}, tool.Info{Name: "grep", ReadOnly: true}, "secrets/x", deny},
		{"agentx tool name", permission.Rules{Deny: []string{"read_file(./x)"}}, readInfo, "x", deny},
		{"agentx tool name other tool", permission.Rules{Deny: []string{"read_file(./x)"}}, tool.Info{Name: "glob", ReadOnly: true}, "x", none},
		{"write bare", permission.Rules{Deny: []string{"Write"}}, writeInfo, "x", deny},
		{"write bare not edit", permission.Rules{Deny: []string{"Write"}}, editInfo, "x", none},
		{"parentheses literal", permission.Rules{Allow: []string{"Edit(./Finance (2024)/**)"}}, writeInfo, "Finance (2024)/a.xlsx", allow},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := e.policy(t, permission.ModeDefault, tt.rules)
			if got := decide(p, "", tt.info, args("path", tt.path)).Decision; got != tt.want {
				t.Fatalf("%s: got %s, want %s", tt.path, name(got), name(tt.want))
			}
		})
	}
}

// On file systems that ignore case, a path written in another case is the
// same file, so deny rules and protected paths still apply to it.
func TestCaseInsensitivePaths(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("file names are case-sensitive here")
	}
	e := newEnv(t)
	for _, tt := range []struct {
		name  string
		mode  permission.Mode
		rules permission.Rules
		info  tool.Info
		args  json.RawMessage
		want  loop.Decision
	}{
		{"read deny", permission.ModeDefault, permission.Rules{Deny: []string{"Read(./.env)"}}, readInfo, args("path", ".ENV"), deny},
		{"edit deny dir", permission.ModeDefault, permission.Rules{Allow: []string{"Edit"}, Deny: []string{"Edit(./secrets/**)"}}, writeInfo, args("path", "Secrets/x"), deny},
		{"bash read deny", permission.ModeDefault, permission.Rules{Deny: []string{"Read(./.env)"}}, bashInfo, args("command", "cat .ENV"), deny},
		{"allow stays exact", permission.ModeDefault, permission.Rules{Allow: []string{"Edit(src/**)"}}, writeInfo, args("path", "SRC/a.go"), none},
		{"protected", permission.ModeAcceptEdits, permission.Rules{}, writeInfo, args("path", ".GIT/hooks/pre-commit"), ask},
		{"protected bash", permission.ModeAcceptEdits, permission.Rules{}, bashInfo, args("command", "echo hi > .GIT/hooks/pre-commit"), ask},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := e.policy(t, tt.mode, tt.rules)
			if got := decide(p, tt.mode, tt.info, tt.args).Decision; got != tt.want {
				t.Fatalf("got %s, want %s", name(got), name(tt.want))
			}
		})
	}
}

func TestSettingsBase(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	base := filepath.Join(e.home, ".claude")
	p := e.policy(t, "", permission.Rules{Deny: []string{"Read(/secrets/**)"}, Base: base})
	if got := decide(p, "", readInfo, args("path", filepath.Join(base, "secrets", "x"))).Decision; got != deny {
		t.Fatalf("base: got %s", name(got))
	}
	if got := decide(p, "", readInfo, args("path", "secrets/x")).Decision; got != none {
		t.Fatalf("project: got %s", name(got))
	}
}

func TestSymlinks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.policy(t, "", permission.Rules{Allow: []string{"Edit(proj/**)"}, Deny: []string{"Read(secret/**)"}})
	for _, d := range []string{"proj", "secret", "outside"} {
		if err := os.MkdirAll(filepath.Join(e.dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(e.dir, "outside"), filepath.Join(e.dir, "proj", "out")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if err := os.Symlink(filepath.Join(e.dir, "secret"), filepath.Join(e.dir, "pub")); err != nil {
		t.Fatal(err)
	}
	if got := decide(p, "", writeInfo, args("path", "proj/a.go")).Decision; got != allow {
		t.Fatalf("plain: got %s", name(got))
	}
	// Allow rules must match the path and its target.
	if got := decide(p, "", writeInfo, args("path", "proj/out/a.go")).Decision; got != none {
		t.Fatalf("link out of allowed dir: got %s", name(got))
	}
	// Deny rules match either.
	if got := decide(p, "", readInfo, args("path", "pub/key")).Decision; got != deny {
		t.Fatalf("link into denied dir: got %s", name(got))
	}
}

func TestToolNames(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	call := func(rules permission.Rules, tool string) loop.Decision {
		p := e.policy(t, "", rules)
		info := mcpInfo
		info.Name = tool
		return decide(p, "", info, args()).Decision
	}
	// https://code.claude.com/docs/en/permissions#mcp
	for _, tt := range []struct {
		name  string
		rules permission.Rules
		tool  string
		want  loop.Decision
	}{
		{"server", permission.Rules{Allow: []string{"mcp__puppeteer"}}, "mcp__puppeteer__navigate", allow},
		{"server glob", permission.Rules{Allow: []string{"mcp__puppeteer__*"}}, "mcp__puppeteer__navigate", allow},
		{"tool", permission.Rules{Allow: []string{"mcp__puppeteer__navigate"}}, "mcp__puppeteer__navigate", allow},
		{"tool other", permission.Rules{Allow: []string{"mcp__puppeteer__navigate"}}, "mcp__puppeteer__click", none},
		{"prefix glob", permission.Rules{Allow: []string{"mcp__github__get_*"}}, "mcp__github__get_issue", allow},
		{"prefix glob other", permission.Rules{Allow: []string{"mcp__github__get_*"}}, "mcp__github__create_issue", none},
		{"server is whole", permission.Rules{Allow: []string{"mcp__git"}}, "mcp__github__get_issue", none},
		{"allow server with underscores", permission.Rules{Allow: []string{"mcp__reports"}}, "mcp__reports__v2__list", none},
		{"deny server with underscores", permission.Rules{Deny: []string{"mcp__reports"}}, "mcp__reports__v2__list", deny},
		{"deny glob", permission.Rules{Deny: []string{"mcp__*"}}, "mcp__a__b", deny},
		{"deny all", permission.Rules{Deny: []string{"*"}}, "todo_write", deny},
		{"alias", permission.Rules{Deny: []string{"TodoWrite"}}, "todo_write", deny},
		{"custom", permission.Rules{Ask: []string{"deploy"}}, "deploy", ask},
	} {
		if got := call(tt.rules, tt.tool); got != tt.want {
			t.Errorf("%s: got %s, want %s", tt.name, name(got), name(tt.want))
		}
	}
}

func TestInvalidRules(t *testing.T) {
	t.Parallel()
	for _, rs := range []permission.Rules{
		{Allow: []string{"*"}},
		{Allow: []string{"mcp__*"}},
		{Allow: []string{"B*"}},
		{Allow: []string{"mcp__x*__y"}},
		{Deny: []string{"mcp__github__x(y)"}},
		{Allow: []string{"Write(docs/**)"}},
		{Deny: []string{"Glob(docs/**)"}},
		{Deny: []string{"Bash(command:rm *)"}},
		{Deny: []string{"Read(file_path:x)"}},
		{Allow: []string{"WebFetch(https://example.com)"}},
		{Allow: []string{"WebFetch(domain:example.com/x)"}},
		{Allow: []string{"Read(!x)"}},
		{Allow: []string{"Edit([)"}},
		{Allow: []string{"Bash(ls"}},
		{Allow: []string{""}},
		{Allow: []string{"Bash()"}},
	} {
		if _, err := permission.New(permission.Config{Rules: []permission.Rules{rs}}); err == nil {
			t.Errorf("%v: no error", rs)
		}
	}
	if _, err := permission.New(permission.Config{Mode: "auto"}); err == nil {
		t.Error("auto mode: no error")
	}
	if _, err := permission.New(permission.Config{Mode: permission.ModeBypassPermissions, DisableBypassPermissionsMode: true}); err == nil {
		t.Error("disabled bypass: no error")
	}
	// A deny rule with an unusable pattern still guards the exact path.
	p := permission.MustNew(permission.Config{Dir: t.TempDir(), Rules: []permission.Rules{{Deny: []string{"Read([)"}}}})
	if got := decide(p, "", readInfo, args("path", "[")).Decision; got != deny {
		t.Fatalf("literal deny: got %s", name(got))
	}
	if m, err := permission.ParseMode("manual"); err != nil || m != permission.ModeDefault {
		t.Fatalf("manual: %v %v", m, err)
	}
}

func TestWebFetchRules(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	// https://code.claude.com/docs/en/permissions#webfetch
	for _, tt := range []struct {
		rule, url string
		want      bool
	}{
		{"example.com", "https://example.com/x", true},
		{"example.com", "https://EXAMPLE.com./x", true},
		{"example.com.", "https://example.com", true},
		{"example.com", "https://api.example.com", false},
		{"*.example.com", "https://a.b.example.com", true},
		{"*.example.com", "https://example.com", false},
		{"example.*", "https://example.org", true},
		{"example.*", "https://example.evil.com", false},
		{"*", "https://anything.test", true},
		{"example.com", "not a url", false},
	} {
		p := e.policy(t, "", permission.Rules{Deny: []string{"WebFetch(domain:" + tt.rule + ")"}})
		got := decide(p, "", fetchInfo, args("url", tt.url)).Decision == deny
		if got != tt.want {
			t.Errorf("%s on %s: got %v", tt.rule, tt.url, got)
		}
	}
	p := e.policy(t, permission.ModeDontAsk, permission.Rules{Allow: []string{"WebFetch(domain:example.com)"}, Ask: []string{"WebFetch(domain:ask.test)"}})
	if got := decide(p, "", fetchInfo, args("url", "https://example.com")).Decision; got != allow {
		t.Fatalf("allow: got %s", name(got))
	}
	if got := decide(p, "", fetchInfo, args("url", "https://ask.test")).Decision; got != deny {
		t.Fatalf("ask in dontAsk: got %s", name(got))
	}
}

func TestClassify(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := permission.MustNew(permission.Config{Dir: dir, Mode: permission.ModeAcceptEdits,
		Rules: []permission.Rules{{Deny: []string{"Edit(*.lock)"}}},
		Classify: func(name string, args json.RawMessage) (permission.Access, bool) {
			if name != "apply_patch" {
				return permission.Access{}, false
			}
			return permission.Access{Kind: permission.KindEdit, Paths: []string{"a.go", "b.lock"}}, true
		}})
	info := tool.Info{Name: "apply_patch", RequiresApproval: true}
	if got := decide(p, "", info, nil).Decision; got != deny {
		t.Fatalf("classified edit: got %s", name(got))
	}
}

func TestSuggest(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.policy(t, "", permission.Rules{Allow: []string{"Bash(make *)"}})
	got := p.Suggest(&permission.Request{Name: "bash", Info: bashInfo, Known: true,
		Arguments: args("command", "make && npm test > /tmp/out.txt && ls && rm *.o")})
	want := []string{"Bash(npm test)", "Edit(//tmp/out.txt)"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
	got = p.Suggest(&permission.Request{Name: "web_fetch", Info: fetchInfo, Known: true, Arguments: args("url", "https://Docs.Example.com/x")})
	if len(got) != 1 || got[0] != "WebFetch(domain:docs.example.com)" {
		t.Fatalf("fetch: %q", got)
	}
	got = p.Suggest(&permission.Request{Name: "write_file", Info: writeInfo, Known: true, Arguments: args("path", "a[1].go")})
	if len(got) != 1 || !strings.HasSuffix(got[0], `/a\[1\].go)`) || !strings.HasPrefix(got[0], "Edit(//") {
		t.Fatalf("file: %q", got)
	}
	// The suggestion approves the call and only it.
	q := e.policy(t, "", permission.Rules{Allow: got})
	if d := decide(q, "", writeInfo, args("path", "a[1].go")).Decision; d != allow {
		t.Fatalf("suggested rule: got %s", name(d))
	}
	if d := decide(q, "", writeInfo, args("path", "a1.go")).Decision; d != none {
		t.Fatalf("suggested rule on other path: got %s", name(d))
	}
}

func TestApplyPatch(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	patchInfo := tool.Info{Name: "apply_patch", RequiresApproval: true}
	patch := func(body string) json.RawMessage { return args("input", "*** Begin Patch\n"+body+"*** End Patch\n") }
	update := patch("*** Update File: src/a.go\n@@\n-x\n+y\n")
	for _, tt := range []struct {
		name  string
		mode  permission.Mode
		rules permission.Rules
		args  json.RawMessage
		want  loop.Decision
	}{
		{"default", permission.ModeDefault, permission.Rules{}, update, none},
		{"accept edits inside", permission.ModeAcceptEdits, permission.Rules{}, update, allow},
		{"accept edits outside", permission.ModeAcceptEdits, permission.Rules{}, patch("*** Add File: /etc/evil\n+x\n"), none},
		{"accept edits move outside", permission.ModeAcceptEdits, permission.Rules{}, patch("*** Update File: src/a.go\n*** Move to: ../out.go\n@@\n-x\n+y\n"), none},
		{"deny any file", permission.ModeAcceptEdits, permission.Rules{Deny: []string{"Edit(.env)"}}, patch("*** Update File: src/a.go\n@@\n-x\n+y\n  *** Delete File: .env\n"), deny},
		{"read deny", permission.ModeDefault, permission.Rules{Deny: []string{"Read(secrets/**)"}}, patch("*** Delete File: secrets/key\n"), deny},
		{"allow rule", permission.ModeDefault, permission.Rules{Allow: []string{"Edit(src/**)"}}, update, allow},
		{"protected", permission.ModeAcceptEdits, permission.Rules{}, patch("*** Add File: .git/hooks/pre-commit\n+x\n"), ask},
		{"plan", permission.ModePlan, permission.Rules{}, update, deny},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := e.policy(t, permission.ModeDefault, tt.rules)
			if v := decide(p, tt.mode, patchInfo, tt.args); v.Decision != tt.want {
				t.Fatalf("got %s (%s), want %s", name(v.Decision), v.Reason, name(tt.want))
			}
		})
	}
}
