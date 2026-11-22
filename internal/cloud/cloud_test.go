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
	rows, note := Rows(merged)
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
}
