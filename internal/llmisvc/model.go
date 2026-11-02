// Package llmisvc turns an unstructured KServe LLMInferenceService into a plugin-side model (docs/mvp.md section 6a).
//
// Like package isvc, it detects fields instead of KServe versions: v1alpha1 (KServe v0.16) has no status.workloads
// and no status.appliedConfigs, so callers find the workloads through owner references and labels there.
package llmisvc

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/klahio/kubectl-inferon/internal/isvc"
)

// Group and resource; the version is chosen at run time (PreferredVersions).
const (
	Group    = "serving.kserve.io"
	Resource = "llminferenceservices"
)

// PreferredVersions lists the served API versions, newest first. v1alpha2 is the stored version from KServe v0.17.
var PreferredVersions = []string{"v1alpha2", "v1alpha1"}

// GVR returns the resource for one API version.
func GVR(version string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: Group, Version: version, Resource: Resource}
}

// Labels KServe puts on the objects of an LLMInferenceService (observed on KServe v0.21).
const (
	LabelPartOf         = "app.kubernetes.io/part-of"
	PartOfValue         = "llminferenceservice"
	LabelName           = "app.kubernetes.io/name"
	LabelComponent      = "kserve.io/component"
	LabelRole           = "llm-d.ai/role" // both, prefill or decode
	LabelLWSWorkerIndex = "leaderworkerset.sigs.k8s.io/worker-index"
)

// ContainerServer is the vLLM container in workload pods.
const ContainerServer = "main"

// Selector selects every object of one LLMInferenceService. app.kubernetes.io/name alone could match unrelated
// objects, so part-of is always included.
func Selector(name string) string {
	return LabelPartOf + "=" + PartOfValue + "," + LabelName + "=" + name
}

// Workload roles.
const (
	RolePrimary   = "primary" // decode, or both when prefill is not split out
	RolePrefill   = "prefill"
	RoleScheduler = "scheduler"
)

// WorkloadRef is one entry of status.workloads.
type WorkloadRef struct {
	Role          string `json:"role"`
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	ReadyReplicas int64  `json:"readyReplicas"`
}

// Service is the plugin's view of one LLMInferenceService.
type Service struct {
	Name       string            `json:"name"`
	Namespace  string            `json:"namespace"`
	UID        string            `json:"-"`
	APIVersion string            `json:"apiVersion"`
	Created    time.Time         `json:"created"`
	URL        string            `json:"url,omitempty"`
	Addresses  []string          `json:"addresses,omitempty"`
	Conditions []isvc.Condition  `json:"conditions,omitempty"`
	ModelURI   string            `json:"modelUri,omitempty"`
	ModelName  string            `json:"modelName,omitempty"`
	LoRA       []string          `json:"lora,omitempty"`
	Replicas   int64             `json:"replicas"`
	Parallel   map[string]int64  `json:"parallelism,omitempty"`
	Image      string            `json:"image,omitempty"`
	BaseRefs   []string          `json:"baseRefs,omitempty"`
	Applied    []string          `json:"appliedConfigs,omitempty"`
	Prefill    bool              `json:"prefill,omitempty"`
	MultiNode  bool              `json:"multiNode,omitempty"`
	Router     bool              `json:"router,omitempty"`
	Workloads  []WorkloadRef     `json:"workloads,omitempty"`
	Labels     map[string]string `json:"-"`
	// WorkloadsReported is false on v1alpha1, which has no status.workloads.
	WorkloadsReported bool `json:"-"`
}

// Condition returns the condition of the given type, if present.
func (s *Service) Condition(t string) (isvc.Condition, bool) {
	for _, c := range s.Conditions {
		if c.Type == t {
			return c, true
		}
	}
	return isvc.Condition{}, false
}

// Ready reports the Ready condition: "True", "False" or "Unknown".
func (s *Service) Ready() string {
	if c, ok := s.Condition("Ready"); ok && c.Status != "" {
		return c.Status
	}
	return "Unknown"
}

// FromUnstructured converts an LLMInferenceService read through the dynamic client.
func FromUnstructured(u *unstructured.Unstructured) (*Service, error) {
	if u.GetKind() != "LLMInferenceService" {
		return nil, fmt.Errorf("expected kind LLMInferenceService, got %q", u.GetKind())
	}
	obj := u.Object
	s := &Service{
		Name:       u.GetName(),
		Namespace:  u.GetNamespace(),
		UID:        string(u.GetUID()),
		APIVersion: u.GetAPIVersion(),
		Created:    u.GetCreationTimestamp().Time,
		Labels:     u.GetLabels(),
		URL:        isvc.Str(obj, "status", "url"),
		Conditions: isvc.Conditions(obj),
		ModelURI:   isvc.Str(obj, "spec", "model", "uri"),
		ModelName:  isvc.Str(obj, "spec", "model", "name"),
		Replicas:   1,
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(obj, "spec", "replicas"); found {
		s.Replicas = isvc.Integer(obj, "spec", "replicas")
	}

	addrs, _, _ := unstructured.NestedSlice(obj, "status", "addresses")
	for _, a := range addrs {
		if m, ok := a.(map[string]any); ok {
			if u := isvc.Str(m, "url"); u != "" {
				s.Addresses = append(s.Addresses, u)
			}
		}
	}
	adapters, _, _ := unstructured.NestedFieldNoCopy(obj, "spec", "model", "lora", "adapters")
	if list, ok := adapters.([]any); ok {
		for _, a := range list {
			switch v := a.(type) {
			case map[string]any:
				s.LoRA = append(s.LoRA, firstNonEmpty(isvc.Str(v, "name"), isvc.Str(v, "uri")))
			case string:
				s.LoRA = append(s.LoRA, v)
			}
		}
	}

	if p, found, _ := unstructured.NestedMap(obj, "spec", "parallelism"); found {
		s.Parallel = map[string]int64{}
		for _, k := range []string{"tensor", "data", "dataLocal", "pipeline", "expert"} {
			if n := isvc.Integer(p, k); n > 0 {
				s.Parallel[k] = n
			}
		}
	}

	containers, _, _ := unstructured.NestedSlice(obj, "spec", "template", "containers")
	for _, c := range containers {
		if m, ok := c.(map[string]any); ok && isvc.Str(m, "name") == ContainerServer {
			s.Image = isvc.Str(m, "image")
		}
	}

	refs, _, _ := unstructured.NestedSlice(obj, "spec", "baseRefs")
	for _, r := range refs {
		if m, ok := r.(map[string]any); ok {
			s.BaseRefs = append(s.BaseRefs, isvc.Str(m, "name"))
		}
	}
	applied, _, _ := unstructured.NestedSlice(obj, "status", "appliedConfigs")
	for _, a := range applied {
		if m, ok := a.(map[string]any); ok {
			s.Applied = append(s.Applied, isvc.Str(m, "name"))
		}
	}

	_, s.Prefill, _ = unstructured.NestedMap(obj, "spec", "prefill")
	_, s.MultiNode, _ = unstructured.NestedMap(obj, "spec", "worker")
	if router, found, _ := unstructured.NestedMap(obj, "spec", "router"); found && len(router) > 0 {
		s.Router = true
	}

	if w, found, _ := unstructured.NestedMap(obj, "status", "workloads"); found {
		s.WorkloadsReported = true
		for _, role := range []string{RolePrimary, RolePrefill, RoleScheduler} {
			m, ok := w[role].(map[string]any)
			if !ok {
				continue
			}
			s.Workloads = append(s.Workloads, WorkloadRef{
				Role:          role,
				Kind:          isvc.Str(m, "kind"),
				Name:          isvc.Str(m, "name"),
				ReadyReplicas: isvc.Integer(m, "readyReplicas"),
			})
		}
	}
	return s, nil
}

// RoleOf tells which workload a pod or Deployment belongs to, from its labels: scheduler pods by kserve.io/component,
// prefill pods by llm-d.ai/role, everything else is the primary (decode) workload.
func RoleOf(labels map[string]string) string {
	if strings.Contains(labels[LabelComponent], "scheduler") || strings.Contains(labels[LabelComponent], "router") {
		return RoleScheduler
	}
	if labels[LabelRole] == "prefill" {
		return RolePrefill
	}
	return RolePrimary
}

// IsWorker reports a non-leader pod of a multi-node LeaderWorkerSet.
func IsWorker(labels map[string]string) bool {
	idx, ok := labels[LabelLWSWorkerIndex]
	if !ok {
		return false
	}
	n, err := strconv.Atoi(idx)
	return err == nil && n > 0
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
