package cloud

import (
	"context"
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
	if strings.Contains(q, "union (Resources") || !strings.Contains(q, "'gpu'") || !strings.Contains(q, "ResourceContainers") ||
		!strings.Contains(q, "tostring(tags['karpenter.azure.com_cluster']) in~ ('aks1','kubeconfig-ring1')") {
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
		          {"name":"nap","mode":"Machines","powerState":{"code":"Running"}}]},
		{"name":"aks-nap-a","type":"microsoft.compute/virtualmachines","vmSize":"Standard_NC40ads_H100_v5","power":"PowerState/running","aksCluster":"AKS1","priority":"Spot"},
		{"name":"aks-nap-b","type":"microsoft.compute/virtualmachines","vmSize":"Standard_NC40ads_H100_v5","power":"PowerState/running","aksCluster":"aks1","priority":"Spot"},
		{"name":"aks-nap-c","type":"microsoft.compute/virtualmachines","vmSize":"Standard_D8s_v5","power":"PowerState/deallocated","aksCluster":"aks1"}]}`
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
		"running|2× D4as_v5 + 2× NC40ads_H100_v5 spot|rg-k", // auto-provisioned machines are counted; stopped ones are not
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

func TestAccessCommands(t *testing.T) {
	st := &State{}
	id := "/subscriptions/s1/resourceGroups/rg-vh/providers/Microsoft.Compute/virtualMachines/vm-a"
	tun := ParseWith(`az network bastion tunnel --name bast -g rg-vh --target-resource-id `+id+` --resource-port 22 --port 2222 >/dev/null 2>&1 &`, st)
	if len(tun) != 1 || tun[0].Name != "vm-a" || tun[0].Scope != "rg-vh" || tun[0].Sub != "s1" {
		t.Fatalf("tunnel: %+v", tun)
	}
	wantTun := "az network bastion tunnel --name bast -g rg-vh --target-resource-id " + id + " --resource-port 22 --port 2222"
	if tun[0].Tunnel != wantTun {
		t.Errorf("tunnel line:\n got  %s\n want %s", tun[0].Tunnel, wantTun)
	}
	// A later ssh to that local port reaches the VM, with script-only
	// options dropped.
	got := ParseWith(`ssh -n -i $HOME/.ssh/id_ed25519 -o StrictHostKeyChecking=no -o ConnectTimeout=8 -o BatchMode=yes -p 2222 azureuser@127.0.0.1 'nvidia-smi'`, st)
	if len(got) != 1 || got[0].Kind != "VM" || got[0].Name != "vm-a" || got[0].Tunnel != wantTun {
		t.Fatalf("ssh through tunnel: %+v", got)
	}
	if want := "ssh -i ~/.ssh/id_ed25519 -p 2222 azureuser@127.0.0.1"; got[0].SSH != want {
		t.Errorf("ssh line %q, want %q", got[0].SSH, want)
	}
	if got := ParseWith(`ssh -p 2299 azureuser@localhost true`, st); got != nil {
		t.Errorf("unknown local port should name nothing: %+v", got)
	}
	got = Parse(`ssh -o ProxyJump=jump@1.2.3.4 -o ControlPath=/tmp/x azureuser@10.0.0.5 'ls'`)
	if len(got) != 1 || got[0].SSH != "ssh -o ProxyJump=jump@1.2.3.4 azureuser@10.0.0.5" {
		t.Errorf("proxyjump: %+v", got)
	}
	got = Parse(`az ssh vm -g rg -n vm-b --local-user azureuser -- -L 8000:localhost:8000`)
	if len(got) != 1 || got[0].SSH != "az ssh vm -g rg -n vm-b --local-user azureuser" {
		t.Errorf("az ssh: %+v", got)
	}
	got = Parse(`kubectl --kubeconfig ~/.kube/ring1.yaml --context admin get nodes`)
	if len(got) != 1 || got[0].Kube != "--kubeconfig ~/.kube/ring1.yaml --context admin" {
		t.Errorf("kube access: %+v", got)
	}
	if a := KubeArgs(got[0].Kube, "/h"); strings.Join(a, " ") != "--kubeconfig /h/.kube/ring1.yaml --context admin" {
		t.Errorf("KubeArgs: %q", a)
	}
}

func TestTunnelSurvivesRescan(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "events.jsonl")
	id := "/subscriptions/s1/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm-t"
	os.WriteFile(log, []byte(copilotLine("2026-01-01T00:00:00Z", "az network bastion tunnel -n b -g rg --target-resource-id "+id+" --port 2230 &")), 0o600)
	ix := LoadIndex("")
	ix.Scan(log)
	path := filepath.Join(dir, "ix.json")
	ix.Save(path)
	ix = LoadIndex(path)
	f, _ := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(copilotLine("2026-01-01T01:00:00Z", "ssh -p 2230 azureuser@127.0.0.1 uptime"))
	f.Close()
	ix.Scan(log)
	rs := ix.Resources(log)
	if len(rs) != 1 || rs[0].SSH != "ssh -p 2230 azureuser@127.0.0.1" || rs[0].Tunnel == "" || rs[0].Count != 2 {
		t.Fatalf("got %+v", rs)
	}
}

func TestHostsPoolsAndPrices(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	rs := []Resource{
		{Kind: "SSH host", Name: "20.1.2.3", SSH: "ssh azureuser@20.1.2.3", Last: now, Count: 2},
		{Kind: "VM", Name: "box", Scope: "rg-a", Last: now.Add(-time.Hour), Count: 1},
		{Kind: "SSH host", Name: "20.9.9.9", Last: now, Count: 1},
		{Kind: "AKS cluster", Name: "aks1", Scope: "rg-k", Last: now, Count: 1},
	}
	if !strings.Contains(Query(rs), "'20.1.2.3'") {
		t.Fatalf("query does not ask about the address: %s", Query(rs))
	}
	reply := `{"data":[
		{"name":"box","type":"microsoft.compute/virtualmachines","resourceGroup":"rg-a","location":"westus2","vmSize":"Standard_D64as_v6","power":"PowerState/running"},
		{"name":"box","type":"microsoft.compute/virtualmachines","resourceGroup":"rg-a","ip":"20.1.2.3"},
		{"name":"other","type":"microsoft.compute/virtualmachines","resourceGroup":"rg-b","location":"westus3","vmSize":"Standard_E32s_v5","power":"PowerState/running","ip":"20.9.9.9"},
		{"name":"aks1","type":"microsoft.containerservice/managedclusters","resourceGroup":"rg-k","location":"westus2","power":"Running","k8s":"1.36.5",
		 "pools":[{"name":"sys","count":2,"vmSize":"Standard_D4as_v5","mode":"System","enableAutoScaling":true,"minCount":1,"maxCount":3,"powerState":{"code":"Running"}},
		          {"name":"nap","mode":"Machines","powerState":{"code":"Running"}}]},
		{"name":"aks-nap-a","type":"microsoft.compute/virtualmachines","vmSize":"Standard_NC40ads_H100_v5","power":"PowerState/running","aksCluster":"aks1","priority":"Spot"}]}`
	live, err := Match(rs, []byte(reply))
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := Rows(rs, live)
	byName := map[string]Row{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	// The address an agent ssh'd to folds into the VM it belongs to, keeping
	// the agent's login; an address with no VM row becomes one.
	box := byName["box"]
	if len(rows) != 3 || box.SSH != "ssh azureuser@20.1.2.3" || strings.Join(box.Hosts, ",") != "20.1.2.3" || box.Count != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	if o := byName["other"]; o.Kind != "VM" || o.Group() != "rg-b" || strings.Join(o.Hosts, ",") != "20.9.9.9" {
		t.Fatalf("other = %+v", o)
	}
	aks := byName["aks1"].Live
	if aks.Version != "1.36.5" || len(aks.Pools) != 2 || !aks.Pools[0].Auto || aks.Pools[0].Max != 3 || len(aks.Machines) != 1 || !aks.Machines[0].Spot {
		t.Fatalf("aks1 = %+v", aks)
	}

	p := &Prices{USD: map[string]float64{}, Fetched: map[string]time.Time{}}
	miss := p.Missing(rows, now)
	if len(miss) != 4 { // D64as_v6, E32s_v5, D4as_v5, and the spot H100
		t.Fatalf("missing = %+v", miss)
	}
	page := `{"Items":[
		{"armSkuName":"Standard_NC40ads_H100_v5","skuName":"NC40ads H100 v5","productName":"Virtual Machines NCads H100 v5 Series","retailPrice":6.98,"unitOfMeasure":"1 Hour"},
		{"armSkuName":"Standard_NC40ads_H100_v5","skuName":"NC40ads H100 v5 Spot","productName":"Virtual Machines NCads H100 v5 Series","retailPrice":3.5,"unitOfMeasure":"1 Hour"},
		{"armSkuName":"Standard_NC40ads_H100_v5","skuName":"NC40ads H100 v5 Low Priority","productName":"Virtual Machines NCads H100 v5 Series","retailPrice":0.1,"unitOfMeasure":"1 Hour"},
		{"armSkuName":"Standard_NC40ads_H100_v5","skuName":"NC40ads H100 v5 Spot","productName":"Virtual Machines NCads H100 v5 Series Windows","retailPrice":9,"unitOfMeasure":"1 Hour"},
		{"armSkuName":"Standard_D4as_v5","skuName":"D4as v5","productName":"Virtual Machines Dasv5 Series","retailPrice":0.25,"unitOfMeasure":"1 Hour"}],
		"NextPageLink":""}`
	asked := 0
	got, err := FetchPrices(context.Background(), func(_ context.Context, u string) ([]byte, error) {
		asked++
		if !strings.Contains(u, "westus2") {
			return []byte(`{"Items":[]}`), nil
		}
		return []byte(page), nil
	}, miss)
	if err != nil || asked != 2 {
		t.Fatalf("asked %d times, err %v", asked, err)
	}
	p.Merge(miss, got, now)
	if usd, ok := byName["aks1"].Hourly(p); !ok || usd != 4 { // 2 x 0.25 + 3.5 spot
		t.Fatalf("aks1 costs %v (%v)", usd, ok)
	}
	if _, ok := byName["box"].Hourly(p); ok {
		t.Fatal("a size the price list lacks should count as unknown")
	}
	if len(p.Missing(rows, now.Add(time.Hour))) != 0 {
		t.Fatal("sizes with no listed price should not be asked about again right away")
	}
}

func TestFetchKube(t *testing.T) {
	nodes := `{"items":[
		{"metadata":{"name":"aks-sys-0","labels":{"agentpool":"sys","node.kubernetes.io/instance-type":"Standard_D4as_v5"}},
		 "status":{"conditions":[{"type":"Ready","status":"True"}]}},
		{"metadata":{"name":"aks-nap-gpu","labels":{"agentpool":"aksmanagedap","karpenter.sh/nodepool":"gpu-spot","karpenter.sh/capacity-type":"spot"}},
		 "status":{"capacity":{"nvidia.com/gpu":"1"},"conditions":[{"type":"Ready","status":"True"}]}}]}`
	work := `{"items":[
		{"kind":"Job","metadata":{"name":"train","namespace":"exp"},"status":{"active":1,"startTime":"2026-05-01T10:00:00Z"}},
		{"kind":"Job","metadata":{"name":"bad","namespace":"exp"},"status":{"failed":3,"conditions":[{"type":"Failed","status":"True","reason":"BackoffLimitExceeded"}]}},
		{"kind":"Deployment","metadata":{"name":"api","namespace":"exp"},"spec":{"replicas":2},"status":{"readyReplicas":1}},
		{"kind":"Pod","metadata":{"name":"train-x","namespace":"exp","labels":{"batch.kubernetes.io/job-name":"train"}},"spec":{"nodeName":"aks-nap-gpu"},"status":{"phase":"Running"}},
		{"kind":"Pod","metadata":{"name":"web-1","namespace":"exp"},"spec":{"nodeName":"aks-sys-0"},"status":{"phase":"Running"}},
		{"kind":"Pod","metadata":{"name":"wait-1","namespace":"exp"},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable"}]}}]}`
	var calls []string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		c := strings.Join(args, " ")
		calls = append(calls, c)
		switch {
		case strings.Contains(c, "get nodes"):
			return []byte(nodes), nil
		case strings.Contains(c, "-n exp"):
			return []byte(work), nil
		}
		return nil, fmt.Errorf("unexpected %s", c)
	}
	k := FetchKube(context.Background(), run, []string{"--context", "aks1"}, []string{"exp"}, false)
	if k.Err != "" || len(calls) != 2 || !strings.HasPrefix(calls[0], "--context aks1 get nodes") {
		t.Fatalf("err %q, calls %q", k.Err, calls)
	}
	if n := k.Nodes[0]; n.Name != "aks-nap-gpu" || !n.Auto || !n.Spot || n.GPU != 1 || n.Pool != "gpu-spot" {
		t.Fatalf("nodes = %+v", k.Nodes)
	}
	if j, _ := k.JobFor("train", "exp"); j.State() != "running" {
		t.Fatalf("train is %s", j.State())
	}
	if j, _ := k.JobFor("bad", ""); j.State() != "failed" || j.Reason != "BackoffLimitExceeded" {
		t.Fatalf("bad = %+v", j)
	}
	if d, ok := k.DeployFor("api", "exp"); !ok || d.Ready != 1 || d.Want != 2 {
		t.Fatalf("api = %+v", d)
	}
	// Plain running pods are dropped; job pods and stuck pods are kept.
	if len(k.Pods) != 2 || len(k.PodsOn("aks-nap-gpu")) != 1 || len(k.Waiting()) != 1 || k.Waiting()[0].Reason != "Unschedulable" {
		t.Fatalf("pods = %+v", k.Pods)
	}

	// Without a context the current one is named, and a failure is kept.
	calls = nil
	k = FetchKube(context.Background(), func(_ context.Context, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "current-context") {
			return []byte("ring1\n"), nil
		}
		return nil, fmt.Errorf("Unable to connect")
	}, nil, nil, true)
	if k.Context != "ring1" || k.Err != "Unable to connect" {
		t.Fatalf("k = %+v", k)
	}
}
