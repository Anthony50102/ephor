package cloud

import (
	"path/filepath"

	"strings"
)

// Agents pass many ssh options that only matter to scripts. These are the
// ones a person needs to log in the same way.
var sshKeepOpts = map[string]bool{
	"proxyjump": true, "proxycommand": true, "user": true, "port": true,
	"identityfile": true, "hostkeyalias": true, "certificatefile": true,
}

func sshCommand(keep []string, dest string) string {
	ws := append([]string{"ssh"}, keep...)
	return quoteWords(append(ws, dest))
}

// homeTilde turns $HOME into ~ so the command reads and runs the same.
func homeTilde(s string) string {
	for _, p := range []string{"$HOME", "${HOME}"} {
		if s == p || strings.HasPrefix(s, p+"/") {
			return "~" + s[len(p):]
		}
	}
	return s
}

func isLocal(dest string) bool {
	if i := strings.LastIndexByte(dest, '@'); i >= 0 {
		dest = dest[i+1:]
	}
	switch strings.ToLower(dest) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

var azSSHKeep = []string{"--resource-group", "-g", "--name", "-n", "--vm-name", "--ip", "--local-user",
	"--private-key-file", "--port", "--subscription"}

var bastionKeep = []string{"--name", "-n", "--resource-group", "-g", "--subscription", "--target-resource-id",
	"--resource-port", "--port", "--auth-type", "--username", "--ssh-key"}

// azLine rebuilds an az command from the flags worth keeping, in a fixed
// order, or returns "" when a value is not a plain resolved word.
func azLine(head string, fl map[string]string, keep []string) string {
	ws := strings.Fields(head)
	for _, k := range keep {
		v, ok := fl[k]
		if !ok {
			continue
		}
		v = homeTilde(v)
		if !usable(v) && !strings.HasPrefix(v, "~") {
			return ""
		}
		ws = append(ws, k, v)
	}
	return quoteWords(ws)
}

// armParts reads the subscription, group, type and name from a resource ID.
func armParts(id string) (sub, group, typ, name string) {
	p := strings.Split(strings.Trim(id, "/"), "/")
	for i := 0; i+1 < len(p); i++ {
		switch strings.ToLower(p[i]) {
		case "subscriptions":
			sub = p[i+1]
		case "resourcegroups":
			group = p[i+1]
		}
	}
	if len(p) >= 2 {
		typ, name = p[len(p)-2], p[len(p)-1]
	}
	return
}

// parseBastion reads az network bastion tunnel and az network bastion ssh.
// A tunnel is remembered by its local port, so a later ssh to that port on
// localhost is known to reach the VM.
func parseBastion(verb string, fl map[string]string, st *State) []Touch {
	id := fl["--target-resource-id"]
	sub, group, typ, vm := armParts(id)
	if !strings.EqualFold(typ, "virtualMachines") || !usable(id) || !usable(vm) {
		return nil
	}
	if s := first(fl, "--subscription"); s != "" {
		sub = s
	}
	t := Touch{Kind: "VM", Name: vm, Scope: group, Sub: sub, Verb: Used}
	switch verb {
	case "tunnel":
		port := fl["--port"]
		t.Tunnel = azLine("az network bastion tunnel", fl, bastionKeep)
		if port == "" || !isDigits(port) || t.Tunnel == "" {
			return []Touch{t}
		}
		if st.Ports == nil {
			st.Ports = map[string]Target{}
		}
		st.Ports[port] = Target{VM: vm, Group: group, Sub: sub, Tunnel: t.Tunnel}
	case "ssh":
		t.SSH = azLine("az network bastion ssh", fl, bastionKeep)
	default:
		return nil
	}
	return []Touch{t}
}

// kubeAccess is the kubectl flags that reach the same cluster.
func kubeAccess(fl, env, vars map[string]string, ctxFlag string) string {
	kc := fl["--kubeconfig"]
	if kc == "" {
		kc = env["KUBECONFIG"]
	}
	if kc == "" {
		kc = vars["KUBECONFIG"]
	}
	kc = homeTilde(strings.Split(kc, ":")[0])
	var ws []string
	if kc != "" && !strings.ContainsAny(kc, "$`") {
		ws = append(ws, "--kubeconfig", kc)
	}
	if c := fl[ctxFlag]; usable(c) {
		ws = append(ws, "--context", c)
	}
	return quoteWords(ws)
}

// KubeArgs splits Kube flags back into arguments, expanding ~.
func KubeArgs(kube string, home string) []string {
	ws := strings.Fields(kube)
	for i, w := range ws {
		w = strings.Trim(w, "'")
		if strings.HasPrefix(w, "~/") && home != "" {
			w = filepath.Join(home, w[2:])
		}
		ws[i] = w
	}
	return ws
}

// quoteWords joins words into a command line, quoting where needed. ~ at
// the start of a word stays bare so the shell expands it.
func quoteWords(ws []string) string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = quoteWord(w)
	}
	return strings.Join(out, " ")
}

func quoteWord(w string) string {
	if w == "" {
		return "''"
	}
	safe := true
	for _, r := range w {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:@=,+%~", r)) {
			safe = false
			break
		}
	}
	if safe {
		return w
	}
	if strings.HasPrefix(w, "~/") {
		return "~/" + quoteWord(w[2:])
	}
	return "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
}

// newer copies how-to-reach fields from b into a when b is newer or a has none.
func (a *Resource) takeAccess(b Resource, bNewer bool) {
	if b.SSH != "" && (bNewer || a.SSH == "") {
		a.SSH = b.SSH
	}
	if b.Tunnel != "" && (bNewer || a.Tunnel == "") {
		a.Tunnel = b.Tunnel
	}
	if b.Kube != "" && (bNewer || a.Kube == "") {
		a.Kube = b.Kube
	}
}
