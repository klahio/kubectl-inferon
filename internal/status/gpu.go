package status

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/klahio/kubectl-inferon/internal/kube"
)

// LabelGPUProduct is set by NVIDIA GPU Feature Discovery on GPU nodes.
const LabelGPUProduct = "nvidia.com/gpu.product"

// GPU is what the service's pods ask for in GPU resources and what the nodes that offer them have left
// (docs/mvp.md section 4).
type GPU struct {
	Requests []GPURequest `json:"requests"`
	Nodes    []GPUNode    `json:"nodes,omitempty"`
	// UsageUnknown is set when the pods of the cluster could not be read: the nodes show what they offer, but not
	// what is taken, so nothing is said about free GPUs. NodesUnknown means the nodes could not be read at all.
	UsageUnknown bool `json:"usageUnknown,omitempty"`
	NodesUnknown bool `json:"nodesUnknown,omitempty"`
}

// GPURequest is one component's GPU request.
type GPURequest struct {
	Component string `json:"component"`
	Resource  string `json:"resource"`
	PerPod    int64  `json:"perPod"`
	Pods      int    `json:"pods"`
}

// GPUNode is one node that offers a requested GPU resource.
type GPUNode struct {
	Name        string `json:"name"`
	Resource    string `json:"resource"`
	Product     string `json:"product,omitempty"`
	Allocatable int64  `json:"allocatable"`
	// Requested sums the requests of every running or pending pod on the node, in all namespaces.
	Requested int64 `json:"requested"`
	// ThisService is the part of Requested that belongs to this service's pods.
	ThisService int64 `json:"thisService"`
}

// Free is what is left on the node; a time-sliced or over-committed node can go below zero.
func (n GPUNode) Free() int64 { return n.Allocatable - n.Requested }

// isGPUResource recognizes extended resources for accelerators: nvidia.com/gpu, amd.com/gpu, MIG slices such as
// nvidia.com/mig-1g.10gb, and similar vendor names.
func isGPUResource(name corev1.ResourceName) bool {
	n := string(name)
	return strings.Contains(n, "gpu") || strings.HasPrefix(n, "nvidia.com/mig-")
}

// gpuRequests sums the GPU requests of the live pods per component; describe names a pod's component.
func gpuRequests(pods []corev1.Pod, describe func(*corev1.Pod) string) []GPURequest {
	byKey := map[string]*GPURequest{}
	var order []string
	for _, p := range livePods(pods) {
		for res, n := range podGPU(&p) {
			// The amount is part of the key: during a rollout old and new pods of a component can ask for different amounts.
			key := fmt.Sprintf("%s|%s|%d", describe(&p), res, n)
			r, ok := byKey[key]
			if !ok {
				r = &GPURequest{Component: describe(&p), Resource: string(res), PerPod: n}
				byKey[key] = r
				order = append(order, key)
			}
			r.Pods++
		}
	}
	sort.Strings(order)
	out := make([]GPURequest, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	return out
}

// podGPU is the pod's effective GPU request per resource, computed as the scheduler does. Sidecars (init containers
// with restartPolicy Always) keep running, so they add to the containers and to every init container that starts
// after them; a regular init container runs alone. Extended resources must set limits, and requests default to them.
func podGPU(p *corev1.Pod) map[corev1.ResourceName]int64 {
	amount := func(c corev1.Container, res corev1.ResourceName) int64 {
		if q, ok := c.Resources.Requests[res]; ok {
			return q.Value()
		}
		if q, ok := c.Resources.Limits[res]; ok {
			return q.Value()
		}
		return 0
	}
	names := map[corev1.ResourceName]bool{}
	for _, c := range append(append([]corev1.Container(nil), p.Spec.Containers...), p.Spec.InitContainers...) {
		for res := range c.Resources.Limits {
			if isGPUResource(res) {
				names[res] = true
			}
		}
		for res := range c.Resources.Requests {
			if isGPUResource(res) {
				names[res] = true
			}
		}
	}
	out := map[corev1.ResourceName]int64{}
	for res := range names {
		var sum, sidecars, maxInit int64
		for _, c := range p.Spec.InitContainers {
			if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
				sidecars += amount(c, res)
				maxInit = max(maxInit, sidecars)
				continue
			}
			maxInit = max(maxInit, sidecars+amount(c, res))
		}
		for _, c := range p.Spec.Containers {
			sum += amount(c, res)
		}
		if n := max(sum+sidecars, maxInit); n > 0 {
			out[res] = n
		}
	}
	return out
}

// collectGPU reads the nodes that offer the requested GPU resources and how much of them is taken. It needs
// cluster-wide read access to nodes and pods; without it the section shows only the requests.
func collectGPU(ctx context.Context, c kube.Clients, pods []corev1.Pod, describe func(*corev1.Pod) string, unknown map[string]string) *GPU {
	reqs := gpuRequests(pods, describe)
	if len(reqs) == 0 {
		return nil
	}
	g := &GPU{Requests: reqs}
	wanted := map[string]bool{}
	for _, r := range reqs {
		wanted[r.Resource] = true
	}

	nodes, err := c.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		unknown["gpu nodes"] = reason(err)
		g.NodesUnknown = true
		return g
	}
	all, err := c.Kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase!=" + string(corev1.PodSucceeded) + ",status.phase!=" + string(corev1.PodFailed),
	})
	if err != nil {
		unknown["gpu usage"] = reason(err)
		g.UsageUnknown = true
	}
	ours := map[string]bool{}
	for _, p := range pods {
		ours[p.Namespace+"/"+p.Name] = true
	}

	for _, n := range nodes.Items {
		for res, q := range n.Status.Allocatable {
			if !wanted[string(res)] || q.Value() == 0 {
				continue
			}
			gn := GPUNode{Name: n.Name, Resource: string(res), Product: n.Labels[LabelGPUProduct], Allocatable: q.Value()}
			if all != nil {
				for i := range all.Items {
					p := &all.Items[i]
					if p.Spec.NodeName != n.Name || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
						continue
					}
					amount := podGPU(p)[res]
					gn.Requested += amount
					if ours[p.Namespace+"/"+p.Name] {
						gn.ThisService += amount
					}
				}
			}
			g.Nodes = append(g.Nodes, gn)
		}
	}
	sort.Slice(g.Nodes, func(i, j int) bool {
		if g.Nodes[i].Resource != g.Nodes[j].Resource {
			return g.Nodes[i].Resource < g.Nodes[j].Resource
		}
		return g.Nodes[i].Name < g.Nodes[j].Name
	})
	return g
}

// explainGPUShortage adds the free GPU amounts to "cannot be scheduled" findings that name a GPU resource, so the
// user sees how far off the request is without running kubectl describe node. The request is the pending pod's own,
// which can differ from its running siblings' during a rollout.
func explainGPUShortage(findings []Finding, g *GPU, pods []corev1.Pod) {
	if g == nil || g.NodesUnknown || g.UsageUnknown {
		return
	}
	for i := range findings {
		f := &findings[i]
		if f.Title != titleUnschedulable {
			continue
		}
		_, name, _ := strings.Cut(f.Object, "pod/")
		var pod *corev1.Pod
		for j := range pods {
			if pods[j].Name == name {
				pod = &pods[j]
			}
		}
		if pod == nil {
			continue
		}
		details := strings.Join(f.Details, " ")
		for res, want := range podGPU(pod) {
			if !mentionsInsufficient(details, string(res)) {
				continue
			}
			var free []string
			var most int64
			for _, n := range g.Nodes {
				if n.Resource != string(res) {
					continue
				}
				entry := fmt.Sprintf("%s %d of %d", n.Name, max(n.Free(), 0), n.Allocatable)
				if n.Free() < 0 {
					entry += fmt.Sprintf(" (over-committed by %d)", -n.Free())
				}
				free = append(free, entry)
				most = max(most, n.Free())
			}
			if len(free) == 0 {
				f.Details = append(f.Details, fmt.Sprintf("no node offers %s", res))
				f.Hint = fmt.Sprintf("the pod asks for %d %s, which no node offers; check the GPU device plugin on the nodes", want, res)
				continue
			}
			f.Details = append(f.Details, fmt.Sprintf("free %s: %s", res, strings.Join(free, ", ")))
			f.Hint = fmt.Sprintf("the pod asks for %d %s, and no node has more than %d free; lower the request or free GPUs", want, res, max(most, 0))
		}
	}
}

// mentionsInsufficient finds "Insufficient <resource>" as a whole resource name, so nvidia.com/gpu does not match
// "Insufficient nvidia.com/gpumem".
func mentionsInsufficient(message, resource string) bool {
	needle := "Insufficient " + resource
	for rest := message; ; {
		i := strings.Index(rest, needle)
		if i < 0 {
			return false
		}
		rest = rest[i+len(needle):]
		if rest == "" || !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_/", rune(rest[0])) {
			return true
		}
	}
}

// renderGPU writes the GPU section.
func (r TextRenderer) renderGPU(p *printer, g *GPU) {
	if g == nil {
		return
	}
	p.section("GPU")
	tw := p.table()
	for _, q := range g.Requests {
		pods := "1 pod"
		if q.Pods != 1 {
			pods = fmt.Sprintf("%d pods", q.Pods)
		}
		fmt.Fprintf(tw, "  request\t%s\t%s %d per pod (%s)\n", q.Component, q.Resource, q.PerPod, pods)
	}
	for _, n := range g.Nodes {
		line := fmt.Sprintf("  node\t%s\t%s %d allocatable, %d requested, %d free", n.Name, n.Resource, n.Allocatable, n.Requested, n.Free())
		if g.UsageUnknown {
			line = fmt.Sprintf("  node\t%s\t%s %d allocatable, usage not checked", n.Name, n.Resource, n.Allocatable)
		}
		if n.ThisService > 0 {
			line += fmt.Sprintf(" (%d by this service)", n.ThisService)
		}
		if n.Product != "" {
			line += "   " + r.paint(colorDim, n.Product)
		}
		fmt.Fprintln(tw, line)
	}
	tw.Flush()
}
