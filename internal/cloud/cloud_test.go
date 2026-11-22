package cloud

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func touches(cmd string) []string {
	var out []string
	for _, t := range Parse(cmd) {
		s := fmt.Sprintf("%s|%s|%s|%d", t.Kind, t.Name, t.Scope, t.Verb)
		if t.Sub != "" {
			s += "|" + t.Sub
		}
		out = append(out, s)
	}
	return out
}

func TestParse(t *testing.T) {
	cases := []struct {
		cmd  string
		want []string
	}{
		{`RG=rg-gpu; S=sub1
for size in A B; do
  az vm create -g $RG -n probe1 --subscription "$S" --size $size -o none && echo ok
done`, []string{"VM|probe1|rg-gpu|1|sub1"}},
		{`az vm delete -g rg -n "vm-$(date +%s)" --yes; az vm create -n p$((i+1)) -g rg`, nil},
		{`az account get-access-token --query accessToken; az rest --method get --url x; az boards work-item show --id 1`, nil},
		{`az aks get-credentials -g rg -n spexp-aks && az aks nodepool add --cluster-name spexp-aks -n gpu -g rg`,
			[]string{"AKS cluster|spexp-aks|rg|0", "AKS node pool|gpu|spexp-aks|0"}},
		{`az cognitiveservices account deployment create -n acct -g rg --deployment-name kimi`, []string{"Model deployment|kimi|acct|1"}},
		{`az network vnet subnet create -g rg --vnet-name v -n s0`, []string{"Azure network vnet subnet|s0|rg|1"}},
		{`kubectl --kubeconfig ~/.kube/ring1-aks.yaml -n training logs job/sft-run --tail=80 2>&1 | tail`,
			[]string{"Kubernetes|ring1-aks|training|0", "Kubernetes job|sft-run|ring1-aks/training|0"}},
		{`KUBECONFIG=/x/ring0.yaml kubectl delete job a b -n exp`,
			[]string{"Kubernetes|ring0|exp|0", "Kubernetes job|a|ring0/exp|2", "Kubernetes job|b|ring0/exp|2"}},
		{`kubectl get pods -A`, []string{"Kubernetes|default context|all namespaces|0"}},
		{`ssh -i ~/.ssh/id -o BatchMode=yes azureuser@20.1.2.3 'kubectl get pods'; git push git@github.com:a/b`,
			[]string{"SSH host|20.1.2.3||0"}},
		{`scp -r out azureuser@vm1:/data/`, []string{"SSH host|vm1||0"}},
		{`cat > x.sh <<'EOF'
az vm create -n inside-heredoc -g rg
EOF
bash -c "az group create -n rg-new -l eastus"`, []string{"Resource group|rg-new||1"}},
		{`echo "az vm create -n quoted -g rg" # az vm create -n comment`, nil},
		{`timeout 60 sudo az acr build -r myacr -t img .`, []string{"Container registry|myacr||0"}},
		{`helm upgrade --install web ./chart -n apps --kube-context prod`,
			[]string{"Kubernetes|prod|apps|0", "Helm release|web|prod/apps|0"}},
	}
	for _, c := range cases {
		got := touches(c.cmd)
		if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("Parse(%q)\n got  %q\n want %q", c.cmd, got, c.want)
		}
	}
}

func copilotLine(ts, cmd string) string {
	return fmt.Sprintf(`{"type":"tool.execution_start","data":{"toolName":"bash","arguments":{"command":%q}},"timestamp":%q}`, cmd, ts) + "\n"
}

func TestScanIsIncremental(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "events.jsonl")
	ix := LoadIndex(filepath.Join(dir, "cloud.json"))
	write := func(s string) {
		f, _ := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		_, _ = f.WriteString(s)
		_ = f.Close()
	}
	full := copilotLine("2026-11-20T10:00:00Z", "az vm create -n vm1 -g rg")
	write(full + full[:30]) // the second line is still being written
	if changed, err := ix.Scan(log); err != nil || !changed {
		t.Fatalf("scan: %v %v", changed, err)
	}
	if rs := ix.Resources(log); len(rs) != 1 || !rs[0].Created || rs[0].Count != 1 {
		t.Fatalf("resources = %+v", rs)
	}
	if changed, _ := ix.Scan(log); changed {
		t.Fatal("a partial line must wait for the rest")
	}
	write(full[30:] + copilotLine("2026-11-20T11:00:00Z", "az vm delete -n vm1 --yes"))
	_, _ = ix.Scan(log)
	rs := ix.Resources(log)
	if len(rs) != 1 || rs[0].Count != 3 || !rs[0].Deleted || rs[0].Scope != "rg" || !rs[0].Last.Equal(time.Date(2026, 11, 20, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("after append = %+v", rs)
	}

	// A Claude transcript in a rewritten log starts over.
	_ = os.WriteFile(log, []byte(`{"type":"assistant","timestamp":"2026-11-21T09:00:00Z","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"kubectl -n ml get job/train"}}]}}`+"\n"), 0o600)
	_, _ = ix.Scan(log)
	if rs := ix.Resources(log); len(rs) != 2 || rs[0].Name != "default context" || rs[1].Name != "train" {
		t.Fatalf("after rewrite = %+v", rs)
	}

	if err := ix.Save(filepath.Join(dir, "cloud.json")); err != nil {
		t.Fatal(err)
	}
	if again := LoadIndex(filepath.Join(dir, "cloud.json")); again.Logs[log] == nil || again.Logs[log].Offset == 0 {
		t.Fatal("offsets must persist")
	}
}

func TestMergeAndRows(t *testing.T) {
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }
	a := []Resource{
		{Kind: "Kubernetes", Name: "aks1", Scope: "training", Last: at(1), Count: 1},
		{Kind: "Kubernetes job", Name: "j1", Scope: "aks1/training", Last: at(1), Count: 1},
		{Kind: "Kubernetes job", Name: "j2", Scope: "aks1/training", Last: at(5), Created: true, Count: 1},
		{Kind: "VM", Name: "old", Created: true, Deleted: true, Last: at(9), Count: 2},
		{Kind: "Azure network vnet", Name: "v", Created: true, Last: at(2), Count: 1},
	}
	b := []Resource{
		{Kind: "Kubernetes", Name: "aks1", Scope: "exp", Last: at(3), Count: 1},
		{Kind: "VM", Name: "gpu", Scope: "rg", Created: true, Last: at(4), Count: 1},
		{Kind: "VM", Name: "old", Created: true, Last: at(2), Count: 1}, // older than the delete
	}
	merged := Merge(map[string][]Resource{"alpha": a, "beta": b})
	rows, note := Rows(merged, nil)
	var got []string
	for _, r := range rows {
		got = append(got, fmt.Sprintf("%s %s [%s] %s %s", r.Kind, r.Name, r.Detail, r.Status(), strings.Join(r.By, ",")))
	}
	want := []string{
		"Kubernetes aks1 [2 jobs, latest j2 · ns training, exp] used alpha,beta",
		"VM gpu [] created beta",
		"VM old [] deleted alpha,beta",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rows\n got  %q\n want %q", got, want)
	}
	if !rows[0].Last.Equal(at(5)) {
		t.Fatalf("a cluster is as recent as its latest job: %v", rows[0].Last)
	}
	if note != "1 network resource" {
		t.Fatalf("note = %q", note)
	}

	// The same cluster reached by az and by kubectl is one row.
	rows, _ = Rows(Merge(map[string][]Resource{
		"alpha": {{Kind: "AKS cluster", Name: "AKS1", Scope: "rg", Created: true, Last: at(1), Count: 1}},
		"beta":  {{Kind: "Kubernetes", Name: "aks1", Scope: "training", Last: at(3), Count: 2}},
	}), nil)
	if len(rows) != 1 || rows[0].Kind != "AKS cluster" || !rows[0].Created || rows[0].Count != 3 || !rows[0].Last.Equal(at(3)) || len(rows[0].By) != 2 {
		t.Fatalf("folded rows = %+v", rows)
	}
}

func TestLive(t *testing.T) {
	rs := []Resource{
		{Kind: "VM", Name: "gpu", Scope: "rg-a", Created: true},
		{Kind: "VM", Name: "old", Deleted: true},
		{Kind: "Storage account", Name: "gonestore", Created: true},
		{Kind: "Resource group", Name: "rg-a", Created: true},
		{Kind: "Kubernetes", Name: "kubeconfig-ring1"},
		{Kind: "Kubernetes", Name: "aks1"},
		{Kind: "VM", Name: "x') | union (Resources"}, // never reaches the query
		{Kind: "SSH host", Name: "10.0.0.1"},
	}
	q := Query(rs)
	if strings.Contains(q, "union (Resources") || !strings.Contains(q, "'gpu'") || !strings.Contains(q, "ResourceContainers") {
		t.Fatalf("query = %s", q)
	}
	reply := `{"data":[
		{"name":"GPU","type":"microsoft.compute/virtualmachines","resourceGroup":"rg-b","vmSize":"Standard_ND96","power":"PowerState/running"},
		{"name":"gpu","type":"microsoft.compute/virtualmachines","resourceGroup":"rg-a","vmSize":"Standard_ND40","power":"PowerState/stopped"},
		{"name":"old","type":"microsoft.compute/virtualmachines","resourceGroup":"rg-a","power":"PowerState/deallocated"},
		{"name":"rg-a","type":"microsoft.resources/subscriptions/resourcegroups","resourceGroup":"rg-a"},
		{"name":"aks1","type":"microsoft.containerservice/managedclusters","resourceGroup":"rg-k","power":"Running",
		 "pools":[{"name":"sys","count":2,"vmSize":"Standard_D4as_v5","mode":"System","powerState":{"code":"Running"}},
		          {"name":"gpu","count":0,"vmSize":"Standard_NC24","mode":"User","powerState":{"code":"Running"}},
		          {"name":"nap","mode":"Machines","powerState":{"code":"Running"}}]}]}`
	live, err := Match(rs, []byte(reply))
	if err != nil {
		t.Fatal(err)
	}
	get := func(r Resource) string {
		l, ok := live[r.ID()]
		if !ok {
			return "unchecked"
		}
		return fmt.Sprintf("%s|%s|%s", l.Word(), l.Size, l.Group)
	}
	want := []string{
		"stopped, still billed|ND40|rg-a", // the copy in the logged group wins
		"deallocated||rg-a",
		"gone||",
		"exists||rg-a",
		"unchecked", // a kubeconfig name that is not an AKS name proves nothing
		"running|2× D4as_v5 + auto nodes|rg-k",
		"unchecked",
		"unchecked",
	}
	for i, r := range rs {
		if got := get(r); got != want[i] {
			t.Errorf("%s %s: got %q, want %q", r.Kind, r.Name, got, want[i])
		}
	}

	rows, _ := Rows(rs[:4], live)
	var got []string
	for _, r := range rows {
		got = append(got, r.Name+" "+r.Status())
	}
	if w := "gpu stopped, still billed|old delete failed?|rg-a created|gonestore gone"; strings.Join(got, "|") != w {
		t.Fatalf("rows = %q, want %q", strings.Join(got, "|"), w)
	}
	if l := rows[2].Line(); l != "Resource group rg-a, created by an agent, Azure says now: exists" {
		t.Fatalf("line = %q", l)
	}

	// A full page may be missing matches, so nothing is called gone.
	page := `{"data":[` + strings.TrimSuffix(strings.Repeat(`{"name":"z","type":"t"},`, pageSize), ",") + `]}`
	live, _ = Match(rs, []byte(page))
	if _, ok := live[rs[2].ID()]; ok {
		t.Fatal("a truncated reply must not mark anything gone")
	}
}
