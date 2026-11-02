package status

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/klahio/kubectl-inferon/internal/isvc"
	"github.com/klahio/kubectl-inferon/internal/kube"
	"github.com/klahio/kubectl-inferon/internal/llmisvc"
	"github.com/klahio/kubectl-inferon/internal/version"
)

var leaderWorkerSetGVR = schema.GroupVersionResource{Group: "leaderworkerset.x-k8s.io", Version: "v1", Resource: "leaderworkersets"}

// LLMSnapshot is everything `status llmisvc/NAME` needs, read once from the cluster (docs/mvp.md section 6a).
type LLMSnapshot struct {
	Service     *llmisvc.Service
	Pods        []corev1.Pod
	Deployments []appsv1.Deployment
	WorkerSets  []unstructured.Unstructured
	Services    []corev1.Service
	HTTPRoutes  []unstructured.Unstructured
	Gateways    []unstructured.Unstructured
	Events      []corev1.Event
	GPU         *GPU
	// LWSMissing is set when the service needs LeaderWorkerSet and the cluster does not serve it.
	LWSMissing bool
	Unknown    map[string]string
}

// CollectLLM reads the LLMInferenceService and the objects around it.
func CollectLLM(ctx context.Context, c kube.Clients, namespace, name string, withEvents bool) (*LLMSnapshot, error) {
	u, err := c.GetLLMInferenceService(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	svc, err := llmisvc.FromUnstructured(u)
	if err != nil {
		return nil, err
	}
	s := &LLMSnapshot{Service: svc, Unknown: map[string]string{}}
	selector := metav1.ListOptions{LabelSelector: llmisvc.Selector(name)}

	if pods, err := c.Kube.CoreV1().Pods(namespace).List(ctx, selector); err != nil {
		s.Unknown["pods"] = reason(err)
	} else {
		s.Pods = pods.Items
	}
	s.GPU = collectGPU(ctx, c, s.Pods, llmPodRole, s.Unknown)
	if deps, err := c.Kube.AppsV1().Deployments(namespace).List(ctx, selector); err != nil {
		s.Unknown["deployments"] = reason(err)
	} else {
		s.Deployments = deps.Items
	}
	if svcs, err := c.Kube.CoreV1().Services(namespace).List(ctx, selector); err != nil {
		s.Unknown["services"] = reason(err)
	} else {
		s.Services = svcs.Items
	}
	if svc.MultiNode {
		sets, err := c.Dynamic.Resource(leaderWorkerSetGVR).Namespace(namespace).List(ctx, selector)
		switch {
		case err != nil && isNotInstalled(err):
			s.LWSMissing = true
		case err != nil:
			s.Unknown["leaderworkersets"] = reason(err)
		default:
			s.WorkerSets = sets.Items
		}
	}
	if routes, err := c.Dynamic.Resource(httpRouteGVR).Namespace(namespace).List(ctx, metav1.ListOptions{}); err != nil {
		if !isNotInstalled(err) {
			s.Unknown["httproutes"] = reason(err)
		}
	} else {
		for _, r := range routes.Items {
			if ownedBy(r.GetOwnerReferences(), svc.UID) {
				s.HTTPRoutes = append(s.HTTPRoutes, r)
			}
		}
	}
	s.Gateways = collectGateways(ctx, c.Dynamic, s.HTTPRoutes, s.Unknown)

	if withEvents {
		s.Events = collectEvents(ctx, c.Kube, namespace, name, s.Pods, s.Unknown)
	}
	return s, nil
}

// DiagnoseLLM applies the pod rules of section 4.2 and the LLMInferenceService rules of section 6a.2.
func DiagnoseLLM(s *LLMSnapshot) []Finding {
	m := s.Service
	object := "llminferenceservice/" + m.Name
	var out []Finding

	if c, ok := m.Condition("PresetsCombined"); ok && c.Status == "False" {
		out = append(out, Finding{
			Severity: SeverityError,
			Title:    "Preset cannot be applied",
			Details:  nonEmpty(joinReason(c.Reason, c.Message)),
			Object:   object,
			Hint:     "check spec.baseRefs against the LLMInferenceServiceConfigs in the namespace and in the KServe namespace",
		})
	}
	if s.LWSMissing {
		out = append(out, Finding{
			Severity: SeverityError,
			Title:    "LeaderWorkerSet is not installed",
			Details:  []string{"spec.worker asks for a multi-node workload, which KServe runs as a LeaderWorkerSet"},
			Object:   object,
			Hint:     "install the LeaderWorkerSet operator (leaderworkerset.x-k8s.io)",
		})
	}

	for _, f := range diagnosePods(s.Pods, llmPodRole) {
		if f.Title == "Model download failed" {
			f.Hint = "check spec.model.uri; gated or private Hugging Face models need a token Secret"
		}
		out = append(out, f)
	}
	out = append(out, replicaFailures(s.Deployments)...)

	if c, ok := m.Condition("RouterReady"); ok && c.Status == "False" {
		out = append(out, Finding{
			Severity: SeverityError,
			Title:    "Router is not ready",
			Details:  nonEmpty(joinReason(c.Reason, c.Message)),
			Object:   object,
			Hint:     "the Gateway implementation must support the Gateway API Inference Extension for InferencePool backends",
		})
	}
	out = append(out, routeFindings(s.HTTPRoutes, s.Gateways, s.Unknown, func(o, detail string) Finding {
		return Finding{
			Severity: SeverityWarning,
			Title:    titleNotRoutable,
			Details:  []string{detail},
			Object:   o,
			Hint:     fmt.Sprintf("`%s test llmisvc/%s` falls back to port-forward", version.Command, m.Name),
		}
	})...)

	explainGPUShortage(out, s.GPU, s.Pods)

	if len(errorsOnly(out)) == 0 && !hasInfo(out) && m.Ready() != "True" {
		var details []string
		for _, c := range m.Conditions {
			if c.Status != "True" {
				details = append(details, fmt.Sprintf("%s=%s %s", c.Type, c.Status, joinReason(c.Reason, c.Message)))
			}
		}
		if len(details) == 0 {
			details = []string{"the controller has not reported any conditions yet"}
		}
		out = append(out, Finding{Severity: SeverityError, Title: "Not Ready", Details: details, Object: object})
	}
	return out
}

// llmPodRole names a pod's workload for the output: primary, prefill, scheduler, or worker for the non-leader pods
// of a multi-node LeaderWorkerSet.
func llmPodRole(p *corev1.Pod) string {
	if llmisvc.IsWorker(p.Labels) {
		return "worker"
	}
	return llmisvc.RoleOf(p.Labels)
}

// LLMSummary is the structured result of `status llmisvc/NAME`; `-o json|yaml` prints it.
type LLMSummary struct {
	Kind       string            `json:"kind"`
	Name       string            `json:"name"`
	Namespace  string            `json:"namespace"`
	APIVersion string            `json:"apiVersion"`
	Ready      string            `json:"ready"`
	Created    time.Time         `json:"created"`
	Endpoints  Endpoints         `json:"endpoints"`
	Model      LLMModel          `json:"model"`
	Workloads  []WorkloadState   `json:"workloads"`
	GPU        *GPU              `json:"gpu,omitempty"`
	Startup    *Startup          `json:"startup,omitempty"`
	Conditions []isvc.Condition  `json:"conditions"`
	Findings   []Finding         `json:"findings"`
	Unknown    map[string]string `json:"unknown,omitempty"`
}

// LLMModel describes what an LLMInferenceService serves.
type LLMModel struct {
	URI         string           `json:"uri,omitempty"`
	Name        string           `json:"name,omitempty"`
	LoRA        []string         `json:"lora,omitempty"`
	Image       string           `json:"image,omitempty"`
	Parallelism map[string]int64 `json:"parallelism,omitempty"`
	Presets     []string         `json:"presets,omitempty"`
}

// WorkloadState is one workload of an LLMInferenceService with its replicas.
type WorkloadState struct {
	Role          string `json:"role"`
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	ReadyReplicas int64  `json:"readyReplicas"`
	Replicas      int64  `json:"replicas"`
}

// SummarizeLLM turns a snapshot and its findings into the summary.
func SummarizeLLM(s *LLMSnapshot, findings []Finding) *LLMSummary {
	m := s.Service
	sum := &LLMSummary{
		Kind:       "LLMInferenceService",
		Name:       m.Name,
		Namespace:  m.Namespace,
		APIVersion: m.APIVersion,
		Ready:      m.Ready(),
		Created:    m.Created,
		Conditions: m.Conditions,
		Findings:   findings,
		Unknown:    s.Unknown,
		Model: LLMModel{
			URI:         m.ModelURI,
			Name:        m.ModelName,
			LoRA:        m.LoRA,
			Image:       m.Image,
			Parallelism: m.Parallel,
			Presets:     m.Applied,
		},
		Endpoints: Endpoints{External: m.URL, Routable: m.URL != ""},
	}
	if sum.Findings == nil {
		sum.Findings = []Finding{}
	}
	if len(m.Addresses) > 0 {
		sum.Endpoints.InCluster = m.Addresses[0]
	}
	if len(sum.Model.Presets) == 0 {
		sum.Model.Presets = m.BaseRefs
	}
	if sum.Model.Image == "" {
		sum.Model.Image = serverImage(s.Pods)
	}
	for _, f := range findings {
		if f.Title == titleNotRoutable {
			sum.Endpoints.Routable = false
		}
	}
	for i := range s.HTTPRoutes {
		r := &s.HTTPRoutes[i]
		for _, ref := range parentRefs(r) {
			e := Exposure{Kind: "HTTPRoute", Name: r.GetName(), Via: "Gateway " + ref.Namespace + "/" + ref.Name}
			for _, gw := range s.Gateways {
				if gw.GetNamespace() == ref.Namespace && gw.GetName() == ref.Name {
					e.Address = gatewayAddress(&gw)
				}
			}
			sum.Endpoints.Exposure = append(sum.Endpoints.Exposure, e)
		}
	}
	markBroken(sum.Endpoints.Exposure, findings)
	sum.Workloads = workloads(s)
	sum.GPU = s.GPU
	if p := newestPod(s.Pods, func(p *corev1.Pod) bool { return llmPodRole(p) == llmisvc.RolePrimary }); p != nil {
		sum.Startup = startupOf(p, isvc.ContainerStorageInitial, llmisvc.ContainerServer)
	}
	return sum
}

// workloads lists the Deployments and LeaderWorkerSets of the service. Without them (for example while the
// controller has not created them yet) it falls back to what status.workloads reports.
func workloads(s *LLMSnapshot) []WorkloadState {
	var out []WorkloadState
	for _, d := range s.Deployments {
		w := WorkloadState{Role: llmisvc.RoleOf(d.Labels), Kind: "Deployment", Name: d.Name, ReadyReplicas: int64(d.Status.ReadyReplicas), Replicas: 1}
		if d.Spec.Replicas != nil {
			w.Replicas = int64(*d.Spec.Replicas)
		}
		out = append(out, w)
	}
	for _, ws := range s.WorkerSets {
		out = append(out, WorkloadState{
			Role:          llmisvc.RoleOf(ws.GetLabels()),
			Kind:          "LeaderWorkerSet",
			Name:          ws.GetName(),
			ReadyReplicas: isvc.Integer(ws.Object, "status", "readyReplicas"),
			Replicas:      isvc.Integer(ws.Object, "spec", "replicas"),
		})
	}
	if len(out) == 0 {
		for _, w := range s.Service.Workloads {
			out = append(out, WorkloadState{Role: w.Role, Kind: w.Kind, Name: w.Name, ReadyReplicas: w.ReadyReplicas, Replicas: -1})
		}
	}
	rank := map[string]int{llmisvc.RolePrimary: 0, llmisvc.RolePrefill: 1, llmisvc.RoleScheduler: 2}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Role] < rank[out[j].Role] })
	return out
}

func serverImage(pods []corev1.Pod) string {
	for _, p := range pods {
		for _, c := range p.Spec.Containers {
			if c.Name == llmisvc.ContainerServer {
				return c.Image
			}
		}
	}
	return ""
}

// formatParallelism prints "tensor 2, data 4"; empty when nothing is set.
func formatParallelism(p map[string]int64) string {
	var parts []string
	for _, k := range []string{"tensor", "pipeline", "data", "dataLocal", "expert"} {
		if n, ok := p[k]; ok {
			parts = append(parts, fmt.Sprintf("%s %d", k, n))
		}
	}
	return strings.Join(parts, ", ")
}
