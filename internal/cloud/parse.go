// Package cloud works out which cloud resources an agent session touched by
// reading the shell commands it ran.
//
// It reads commands, not cloud state: a resource listed here was named in a
// command the agent ran. Commands that used unresolved shell variables for
// names, or that ran on a remote host, are invisible to it.
package cloud

import (
	"path/filepath"
	"strings"
)

// Verb is what a command did to a resource.
type Verb int

const (
	Used Verb = iota
	Created
	Deleted
)

// Touch is one resource named by one command.
type Touch struct {
	Kind  string // display label, for example "AKS cluster"
	Name  string
	Scope string // resource group, cluster/namespace, or account
	Sub   string // Azure subscription, when given
	Verb  Verb
}

// Parse returns the resources named in a shell command line.
func Parse(cmdline string) []Touch {
	var out []Touch
	vars := map[string]string{}
	for _, words := range commands(cmdline) {
		out = append(out, parseSimple(words, vars, 0)...)
	}
	return out
}

func parseSimple(words []string, vars map[string]string, depth int) []Touch {
	env := map[string]string{}
	i := 0
	for i < len(words) && isAssign(words[i]) {
		k, v, _ := strings.Cut(words[i], "=")
		env[k] = expand(v, vars)
		i++
	}
	words = words[i:]
	if len(words) == 0 {
		for k, v := range env {
			vars[k] = v
		}
		return nil
	}
	if words[0] == "export" {
		for _, w := range words[1:] {
			if isAssign(w) {
				k, v, _ := strings.Cut(w, "=")
				vars[k] = expand(v, vars)
			}
		}
		return nil
	}
	words = unwrap(words)
	if len(words) == 0 {
		return nil
	}
	for i := range words {
		words[i] = expand(words[i], vars)
	}
	switch filepath.Base(words[0]) {
	case "az":
		return parseAz(words[1:])
	case "kubectl":
		return parseKubectl(words[1:], env, vars)
	case "helm":
		return parseHelm(words[1:], env, vars)
	case "ssh":
		return parseSSH(words[1:])
	case "scp", "rsync":
		return parseCopy(words[1:])
	case "bash", "sh", "zsh":
		if depth < 2 && len(words) >= 3 && (words[1] == "-c" || words[1] == "-lc") {
			var out []Touch
			for _, ws := range commands(words[2]) {
				out = append(out, parseSimple(ws, vars, depth+1)...)
			}
			return out
		}
	}
	return nil
}

// unwrap drops command prefixes that run another command.
func unwrap(w []string) []string {
	for len(w) > 0 {
		switch w[0] {
		case "do", "then", "else", "elif", "if", "while", "until", "!", "{":
			w = w[1:]
		case "sudo", "time", "nohup", "exec", "command", "builtin", "stdbuf", "nice":
			w = w[1:]
			for len(w) > 0 && strings.HasPrefix(w[0], "-") {
				w = w[1:]
			}
		case "env":
			w = w[1:]
			for len(w) > 0 && (strings.HasPrefix(w[0], "-") || isAssign(w[0])) {
				w = w[1:]
			}
		case "timeout":
			w = w[1:]
			for len(w) > 0 && strings.HasPrefix(w[0], "-") {
				w = w[1:]
			}
			if len(w) > 0 {
				w = w[1:] // duration
			}
		default:
			return w
		}
	}
	return w
}

func isAssign(w string) bool {
	k, _, ok := strings.Cut(w, "=")
	if !ok || k == "" {
		return false
	}
	for i, r := range k {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func isNameByte(c byte, firstByte bool) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || !firstByte && c >= '0' && c <= '9'
}

// expand substitutes $NAME and ${NAME} for variables set earlier in the same
// command line. Unknown variables are left alone so callers can reject them.
func expand(s string, vars map[string]string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		j, name := i+1, ""
		if s[j] == '{' {
			end := strings.IndexByte(s[j:], '}')
			if end < 0 {
				b.WriteByte(s[i])
				continue
			}
			name, j = s[j+1:j+end], j+end+1
		} else {
			k := j
			for k < len(s) && isNameByte(s[k], k == j) {
				k++
			}
			name, j = s[j:k], k
		}
		if v, ok := vars[name]; ok && name != "" {
			b.WriteString(v)
			i = j - 1
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// usable reports whether a word is a real, resolved name.
func usable(s string) bool {
	return s != "" && !strings.ContainsAny(s, "$`*{}()<>|;\"' \t\n") && !strings.HasPrefix(s, "-")
}

// flags splits arguments into positionals and flag values. When valued is
// nil, any flag followed by a non-flag word takes it as its value, which is
// how az commands are written. Otherwise only the listed flags take a value.
func flags(args []string, valued map[string]bool) (pos []string, fl map[string]string) {
	fl = map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		if k, v, ok := strings.Cut(a, "="); ok {
			fl[k] = v
			continue
		}
		if (valued == nil || valued[a]) && i+1 < len(args) && (valued != nil || !strings.HasPrefix(args[i+1], "-")) {
			fl[a] = args[i+1]
			i++
			continue
		}
		fl[a] = ""
	}
	return pos, fl
}

func first(fl map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := fl[k]; usable(v) {
			return v
		}
	}
	return ""
}

// ---- az ----

type azKind struct {
	group string   // az command group, for example "aks nodepool"
	kind  string   // display label
	name  []string // flags that hold the resource name
	scope []string // flags that hold the parent
	sub   bool     // deeper subcommands of this group touch the same resource
}

// Ordered so that longer groups win.
var azKinds = []azKind{
	{"aks nodepool", "AKS node pool", []string{"--nodepool-name", "--name", "-n"}, []string{"--cluster-name"}, false},
	{"aks", "AKS cluster", []string{"--name", "-n"}, []string{"--resource-group", "-g"}, true},
	{"vmss", "VM scale set", []string{"--vmss-name", "--name", "-n"}, []string{"--resource-group", "-g"}, true},
	{"vm", "VM", []string{"--vm-name", "--name", "-n"}, []string{"--resource-group", "-g"}, true},
	{"group", "Resource group", []string{"--name", "-n", "--resource-group", "-g"}, nil, false},
	{"cognitiveservices account deployment", "Model deployment", []string{"--deployment-name"}, []string{"--name", "-n"}, false},
	{"cognitiveservices account", "Azure AI account", []string{"--name", "-n"}, []string{"--resource-group", "-g"}, true},
	{"ml compute", "ML compute", []string{"--name", "-n"}, []string{"--workspace-name", "-w"}, false},
	{"ml workspace", "ML workspace", []string{"--name", "-n"}, []string{"--resource-group", "-g"}, false},
	{"acr", "Container registry", []string{"--registry", "-r", "--name", "-n"}, nil, true},
	{"containerapp env", "Container apps env", []string{"--name", "-n"}, []string{"--resource-group", "-g"}, false},
	{"containerapp job", "Container apps job", []string{"--name", "-n"}, []string{"--resource-group", "-g"}, false},
	{"containerapp", "Container app", []string{"--name", "-n"}, []string{"--resource-group", "-g"}, true},
	{"storage account", "Storage account", []string{"--name", "-n", "--account-name"}, []string{"--resource-group", "-g"}, true},
	{"storage", "Storage account", []string{"--account-name"}, nil, true},
	{"batch pool", "Batch pool", []string{"--pool-id", "--id"}, []string{"--account-name"}, false},
	{"batch job", "Batch job", []string{"--job-id", "--id"}, []string{"--account-name"}, false},
	{"batch task", "Batch job", []string{"--job-id"}, []string{"--account-name"}, false},
	{"batch account", "Batch account", []string{"--name", "-n"}, []string{"--resource-group", "-g"}, false},
	{"keyvault", "Key vault", []string{"--vault-name", "--name", "-n"}, nil, true},
	{"postgres flexible-server", "Postgres server", []string{"--name", "-n"}, []string{"--resource-group", "-g"}, true},
	{"webapp", "Web app", []string{"--name", "-n"}, []string{"--resource-group", "-g"}, true},
	{"functionapp", "Function app", []string{"--name", "-n"}, []string{"--resource-group", "-g"}, true},
	{"identity", "Managed identity", []string{"--name", "-n"}, []string{"--resource-group", "-g"}, false},
	{"network public-ip", "Public IP", []string{"--name", "-n"}, []string{"--resource-group", "-g"}, false},
}

// Groups that never name a resource worth showing.
var azSkip = map[string]bool{
	"account": true, "rest": true, "boards": true, "repos": true, "pipelines": true, "devops": true,
	"login": true, "logout": true, "extension": true, "config": true, "ad": true, "role": true,
	"monitor": true, "graph": true, "quota": true, "cloud": true, "upgrade": true, "version": true,
	"feedback": true, "find": true, "bicep": true, "provider": true, "policy": true, "tag": true,
	"artifacts": true, "lock": true, "resource": true, "deployment": true, "interactive": true,
}

func parseAz(args []string) []Touch {
	pos, fl := flags(args, nil)
	if len(pos) < 2 || azSkip[pos[0]] {
		return nil
	}
	verb := pos[len(pos)-1]
	group := strings.Join(pos[:len(pos)-1], " ")
	if pos[0] == "ssh" {
		if pos[1] != "vm" {
			return nil
		}
		name := first(fl, "--name", "-n", "--vm-name")
		if name == "" {
			return nil
		}
		return []Touch{{Kind: "VM", Name: name, Scope: first(fl, "--resource-group", "-g"), Sub: first(fl, "--subscription"), Verb: Used}}
	}
	if verb == "list" {
		return nil
	}
	v := Used
	switch verb {
	case "create", "up":
		v = Created
	case "delete", "purge":
		v = Deleted
	}
	sub := first(fl, "--subscription")
	for _, k := range azKinds {
		exact := group == k.group
		under := k.sub && strings.HasPrefix(group, k.group+" ")
		if !exact && !under {
			continue
		}
		if under {
			v = Used // a subcommand changes part of the resource, not the resource
		}
		name := first(fl, k.name...)
		if name == "" {
			return nil
		}
		return []Touch{{Kind: k.kind, Name: name, Scope: first(fl, k.scope...), Sub: sub, Verb: v}}
	}
	// Unknown group: only creations and deletions are worth showing.
	name := first(fl, "--name", "-n")
	if v == Used || name == "" {
		return nil
	}
	return []Touch{{Kind: "Azure " + group, Name: name, Scope: first(fl, "--resource-group", "-g"), Sub: sub, Verb: v}}
}

// ---- kubernetes ----

var kubeValued = map[string]bool{
	"-n": true, "--namespace": true, "--context": true, "--kubeconfig": true, "-l": true, "--selector": true,
	"-o": true, "--output": true, "-f": true, "--filename": true, "-c": true, "--container": true,
	"--tail": true, "--since": true, "--image": true, "--for": true, "--timeout": true, "--cluster": true,
	"--user": true, "--field-selector": true, "-p": true, "--patch": true, "--type": true, "-k": true,
	"--kube-context": true, "--set": true, "--values": true, "--version": true, "--replicas": true,
	"--template": true, "-L": true, "--sort-by": true, "--from": true, "--to-revision": true,
}

var kubeWorkloads = map[string]string{
	"job": "Kubernetes job", "jobs": "Kubernetes job", "job.batch": "Kubernetes job",
	"cronjob": "Kubernetes cronjob", "cronjobs": "Kubernetes cronjob", "cj": "Kubernetes cronjob",
	"deployment": "Kubernetes deployment", "deployments": "Kubernetes deployment", "deploy": "Kubernetes deployment",
	"statefulset": "Kubernetes statefulset", "statefulsets": "Kubernetes statefulset", "sts": "Kubernetes statefulset",
}

func cluster(fl, env, vars map[string]string, ctxFlag string) string {
	if c := fl[ctxFlag]; usable(c) {
		return c
	}
	kc := fl["--kubeconfig"]
	if kc == "" {
		kc = env["KUBECONFIG"]
	}
	if kc == "" {
		kc = vars["KUBECONFIG"]
	}
	if kc != "" && !strings.Contains(kc, "$") {
		b := filepath.Base(strings.Split(kc, ":")[0])
		return strings.TrimSuffix(strings.TrimSuffix(b, filepath.Ext(b)), ".kubeconfig")
	}
	return "default context"
}

func namespace(fl map[string]string) string {
	if _, ok := fl["-A"]; ok {
		return "all namespaces"
	}
	if _, ok := fl["--all-namespaces"]; ok {
		return "all namespaces"
	}
	return first(fl, "-n", "--namespace")
}

func parseKubectl(args []string, env, vars map[string]string) []Touch {
	pos, fl := flags(args, kubeValued)
	if len(pos) == 0 {
		return nil
	}
	switch pos[0] {
	case "config", "version", "api-resources", "api-versions", "explain", "completion", "plugin", "kustomize":
		return nil
	}
	cl, ns := cluster(fl, env, vars, "--context"), namespace(fl)
	out := []Touch{{Kind: "Kubernetes", Name: cl, Scope: ns, Verb: Used}}
	v := Used
	switch pos[0] {
	case "delete":
		v = Deleted
	case "create", "run":
		v = Created
	}
	scope := cl
	if ns != "" {
		scope += "/" + ns
	}
	rest := pos[1:]
	if pos[0] == "rollout" && len(rest) > 0 {
		rest = rest[1:]
	}
	for i, w := range rest {
		if t, n, ok := strings.Cut(w, "/"); ok {
			if k := kubeWorkloads[t]; k != "" && usable(n) {
				out = append(out, Touch{Kind: k, Name: n, Scope: scope, Verb: v})
			}
			continue
		}
		if k := kubeWorkloads[w]; k != "" && i == 0 {
			for _, n := range rest[1:] {
				if usable(n) && !strings.Contains(n, "/") {
					out = append(out, Touch{Kind: k, Name: n, Scope: scope, Verb: v})
				}
			}
			break
		}
	}
	return out
}

func parseHelm(args []string, env, vars map[string]string) []Touch {
	pos, fl := flags(args, kubeValued)
	if len(pos) < 2 {
		return nil
	}
	v := Used
	switch pos[0] {
	case "install":
		v = Created
	case "uninstall", "delete":
		v = Deleted
	case "upgrade", "status", "rollback", "history", "test":
	default:
		return nil
	}
	name := pos[1]
	if !usable(name) {
		return nil
	}
	cl, ns := cluster(fl, env, vars, "--kube-context"), namespace(fl)
	scope := cl
	if ns != "" {
		scope += "/" + ns
	}
	return []Touch{{Kind: "Kubernetes", Name: cl, Scope: ns, Verb: Used}, {Kind: "Helm release", Name: name, Scope: scope, Verb: v}}
}

// ---- ssh ----

var sshValued = map[string]bool{}

func init() {
	for _, c := range "bcDEeFIiJLlmOopQRSWw" {
		sshValued["-"+string(c)] = true
	}
}

var notHosts = map[string]bool{
	"localhost": true, "127.0.0.1": true, "::1": true, "github.com": true, "ssh.github.com": true,
	"ssh.dev.azure.com": true, "vs-ssh.visualstudio.com": true, "gitlab.com": true, "bitbucket.org": true,
}

func host(dest string) string {
	dest = strings.TrimPrefix(dest, "ssh://")
	if i := strings.LastIndexByte(dest, '@'); i >= 0 {
		dest = dest[i+1:]
	}
	if h, _, ok := strings.Cut(dest, ":"); ok && strings.Count(dest, ":") == 1 {
		dest = h
	}
	if !usable(dest) || notHosts[strings.ToLower(dest)] || strings.Contains(dest, "/") {
		return ""
	}
	return dest
}

func parseSSH(args []string) []Touch {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			if sshValued[a] {
				i++
			}
			continue
		}
		if h := host(a); h != "" {
			return []Touch{{Kind: "SSH host", Name: h, Verb: Used}}
		}
		return nil
	}
	return nil
}

func parseCopy(args []string) []Touch {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		at, colon := strings.IndexByte(a, '@'), strings.IndexByte(a, ':')
		if at > 0 && colon > at {
			if h := host(a[:colon]); h != "" {
				return []Touch{{Kind: "SSH host", Name: h, Verb: Used}}
			}
		}
	}
	return nil
}
