package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Kube is what kubectl says about a cluster right now.
type Kube struct {
	At      time.Time
	Err     string
	Context string // the current context, when the agent named none
	Nodes   []Node
	Jobs    []Job
	Deploys []Deploy
	Pods    []Pod
	AllNS   bool // jobs were listed in every namespace, without pods
}

// Node is one Kubernetes node.
type Node struct {
	Name  string
	Pool  string // AKS pool, or the auto provisioning node pool
	Size  string
	Spot  bool
	Ready bool
	GPU   int
	Auto  bool // made by node auto provisioning
	Born  time.Time
}

// Job is one Kubernetes job.
type Job struct {
	Name, NS                  string
	Active, Succeeded, Failed int
	Want                      int
	Start, End                time.Time
	Done, Broken, Suspended   bool
	Reason                    string // why it failed
}

// State is the job's status in a word.
func (j Job) State() string {
	switch {
	case j.Broken:
		return "failed"
	case j.Done:
		return "complete"
	case j.Suspended:
		return "suspended"
	case j.Active > 0:
		return "running"
	}
	return "waiting"
}

// Deploy is one Kubernetes deployment.
type Deploy struct {
	Name, NS    string
	Ready, Want int
}

// Pod is one pod that belongs to a job or is not running.
type Pod struct {
	Name, NS, Node, Phase string
	Reason                string // why a container is waiting, or why it is unschedulable
	Job                   string
	GPU                   int
}

// Kubectl runs kubectl with the given arguments and returns its output.
type Kubectl func(ctx context.Context, args ...string) ([]byte, error)

// RealKubectl runs the kubectl on PATH.
func RealKubectl() Kubectl {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "kubectl", append(args, "--request-timeout=20s")...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = 5 * time.Second
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(errb.String())
			if i := strings.IndexByte(msg, '\n'); i > 0 {
				msg = msg[:i]
			}
			if msg == "" {
				msg = err.Error()
			}
			return nil, fmt.Errorf("%s", msg)
		}
		return out.Bytes(), nil
	}
}

// Namespaces lists the namespaces in a cluster row's scope. all is true when
// the agent listed every namespace.
func Namespaces(scope string) (ns []string, all bool) {
	for _, s := range strings.Split(scope, ", ") {
		switch s {
		case "":
		case "all namespaces":
			all = true
		default:
			ns = append(ns, s)
		}
	}
	return ns, all
}

// FetchKube asks a cluster about its nodes and about the workloads in the
// namespaces agents used. access is the kubectl flags that select it.
func FetchKube(ctx context.Context, run Kubectl, access []string, namespaces []string, all bool) Kube {
	k := Kube{}
	if !hasContext(access) {
		if b, err := run(ctx, append(append([]string{}, access...), "config", "current-context")...); err == nil {
			k.Context = strings.TrimSpace(string(b))
		}
	}
	b, err := run(ctx, append(append([]string{}, access...), "get", "nodes", "-o", "json")...)
	if err != nil {
		k.Err = err.Error()
		return k
	}
	k.Nodes, err = readNodes(b)
	if err != nil {
		k.Err = err.Error()
		return k
	}
	if len(namespaces) == 0 && all {
		k.AllNS = true
		b, err := run(ctx, append(append([]string{}, access...), "get", "jobs,deployments", "-A", "-o", "json")...)
		if err == nil {
			err = k.readWork(b)
		}
		if err != nil {
			k.Err = err.Error()
		}
		return k
	}
	if len(namespaces) > 4 {
		namespaces = namespaces[:4]
	}
	for _, ns := range namespaces {
		b, err := run(ctx, append(append([]string{}, access...), "get", "jobs,deployments,pods", "-n", ns, "-o", "json")...)
		if err == nil {
			err = k.readWork(b)
		}
		if err != nil {
			k.Err = err.Error()
		}
	}
	return k
}

func hasContext(access []string) bool {
	for _, a := range access {
		if a == "--context" {
			return true
		}
	}
	return false
}

type kubeList struct {
	Items []json.RawMessage `json:"items"`
}

type kubeMeta struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Labels    map[string]string `json:"labels"`
	Created   time.Time         `json:"creationTimestamp"`
}

func readNodes(b []byte) ([]Node, error) {
	var l struct {
		Items []struct {
			Meta   kubeMeta `json:"metadata"`
			Status struct {
				Capacity   map[string]string `json:"capacity"`
				Conditions []struct {
					Type, Status string
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("unreadable node list: %w", err)
	}
	var out []Node
	for _, it := range l.Items {
		lb := it.Meta.Labels
		n := Node{Name: it.Meta.Name, Pool: lb["agentpool"], Size: lb["node.kubernetes.io/instance-type"], Born: it.Meta.Created}
		if np := lb["karpenter.sh/nodepool"]; np != "" {
			n.Pool, n.Auto = np, true
		}
		n.Spot = lb["kubernetes.azure.com/scalesetpriority"] == "spot" || lb["karpenter.sh/capacity-type"] == "spot"
		fmt.Sscan(it.Status.Capacity["nvidia.com/gpu"], &n.GPU)
		for _, c := range it.Status.Conditions {
			if c.Type == "Ready" {
				n.Ready = c.Status == "True"
			}
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pool != out[j].Pool {
			return out[i].Pool < out[j].Pool
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (k *Kube) readWork(b []byte) error {
	var l kubeList
	if err := json.Unmarshal(b, &l); err != nil {
		return fmt.Errorf("unreadable workload list: %w", err)
	}
	for _, raw := range l.Items {
		var head struct {
			Kind string   `json:"kind"`
			Meta kubeMeta `json:"metadata"`
		}
		if json.Unmarshal(raw, &head) != nil {
			continue
		}
		switch head.Kind {
		case "Job":
			k.Jobs = append(k.Jobs, readJob(raw, head.Meta))
		case "Deployment":
			var d struct {
				Spec struct {
					Replicas *int `json:"replicas"`
				} `json:"spec"`
				Status struct {
					Ready int `json:"readyReplicas"`
				} `json:"status"`
			}
			if json.Unmarshal(raw, &d) == nil {
				want := 1
				if d.Spec.Replicas != nil {
					want = *d.Spec.Replicas
				}
				k.Deploys = append(k.Deploys, Deploy{Name: head.Meta.Name, NS: head.Meta.Namespace, Ready: d.Status.Ready, Want: want})
			}
		case "Pod":
			if p, ok := readPod(raw, head.Meta); ok {
				k.Pods = append(k.Pods, p)
			}
		}
	}
	return nil
}

func readJob(raw []byte, m kubeMeta) Job {
	var j struct {
		Spec struct {
			Completions *int  `json:"completions"`
			Suspend     *bool `json:"suspend"`
		} `json:"spec"`
		Status struct {
			Active     int        `json:"active"`
			Succeeded  int        `json:"succeeded"`
			Failed     int        `json:"failed"`
			Start      *time.Time `json:"startTime"`
			End        *time.Time `json:"completionTime"`
			Conditions []struct {
				Type, Status, Reason string
			} `json:"conditions"`
		} `json:"status"`
	}
	_ = json.Unmarshal(raw, &j)
	out := Job{Name: m.Name, NS: m.Namespace, Active: j.Status.Active, Succeeded: j.Status.Succeeded, Failed: j.Status.Failed, Want: 1}
	if j.Spec.Completions != nil {
		out.Want = *j.Spec.Completions
	}
	out.Suspended = j.Spec.Suspend != nil && *j.Spec.Suspend
	if j.Status.Start != nil {
		out.Start = *j.Status.Start
	} else {
		out.Start = m.Created
	}
	if j.Status.End != nil {
		out.End = *j.Status.End
	}
	for _, c := range j.Status.Conditions {
		if c.Status != "True" {
			continue
		}
		switch c.Type {
		case "Complete":
			out.Done = true
		case "Failed":
			out.Broken, out.Reason = true, c.Reason
		}
	}
	return out
}

// readPod keeps pods worth showing: those of a job, and any that are stuck.
func readPod(raw []byte, m kubeMeta) (Pod, bool) {
	var p struct {
		Spec struct {
			Node       string `json:"nodeName"`
			Containers []struct {
				Resources struct {
					Limits map[string]string `json:"limits"`
				} `json:"resources"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			Phase      string `json:"phase"`
			Conditions []struct {
				Type, Status, Reason string
			} `json:"conditions"`
			Containers []struct {
				State struct {
					Waiting *struct {
						Reason string `json:"reason"`
					} `json:"waiting"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return Pod{}, false
	}
	out := Pod{Name: m.Name, NS: m.Namespace, Node: p.Spec.Node, Phase: p.Status.Phase}
	out.Job = m.Labels["batch.kubernetes.io/job-name"]
	if out.Job == "" {
		out.Job = m.Labels["job-name"]
	}
	for _, c := range p.Spec.Containers {
		var n int
		fmt.Sscan(c.Resources.Limits["nvidia.com/gpu"], &n)
		out.GPU += n
	}
	for _, c := range p.Status.Containers {
		if c.State.Waiting != nil && c.State.Waiting.Reason != "" {
			out.Reason = c.State.Waiting.Reason
		}
	}
	for _, c := range p.Status.Conditions {
		if c.Type == "PodScheduled" && c.Status == "False" && c.Reason != "" {
			out.Reason = c.Reason
		}
	}
	stuck := out.Reason != "" || out.Phase == "Pending" || out.Phase == "Failed"
	if out.Job == "" && !stuck && out.GPU == 0 {
		return Pod{}, false
	}
	return out, true
}

// JobFor finds a job by name and namespace; ns "" matches any.
func (k Kube) JobFor(name, ns string) (Job, bool) {
	for _, j := range k.Jobs {
		if j.Name == name && (ns == "" || j.NS == ns) {
			return j, true
		}
	}
	return Job{}, false
}

// DeployFor finds a deployment by name and namespace; ns "" matches any.
func (k Kube) DeployFor(name, ns string) (Deploy, bool) {
	for _, d := range k.Deploys {
		if d.Name == name && (ns == "" || d.NS == ns) {
			return d, true
		}
	}
	return Deploy{}, false
}

// PodsOn lists the pods shown on a node.
func (k Kube) PodsOn(node string) []Pod {
	var out []Pod
	for _, p := range k.Pods {
		if p.Node == node {
			out = append(out, p)
		}
	}
	return out
}

// Waiting lists pods not yet placed on a node, usually for want of capacity.
func (k Kube) Waiting() []Pod {
	var out []Pod
	for _, p := range k.Pods {
		if p.Node == "" && p.Phase == "Pending" {
			out = append(out, p)
		}
	}
	return out
}
