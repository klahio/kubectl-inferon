package status

import (
	"fmt"
	"slices"
	"strings"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klahio/kubectl-inferon/internal/isvc"
)

// Summary is the structured result of the status command. It is what `-o json|yaml` prints, and the text
// renderer prints nothing that is not in it.
type Summary struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Mode        string            `json:"mode"`
	Ready       string            `json:"ready"`
	Created     time.Time         `json:"created"`
	Endpoints   Endpoints         `json:"endpoints"`
	Model       isvc.Model        `json:"model"`
	Runtime     *Runtime          `json:"runtime,omitempty"`
	ModelStatus isvc.ModelStatus  `json:"modelStatus"`
	Components  []ComponentState  `json:"components"`
	Canary      *CanarySplit      `json:"canary,omitempty"`
	GPU         *GPU              `json:"gpu,omitempty"`
	Startup     *Startup          `json:"startup,omitempty"`
	Conditions  []isvc.Condition  `json:"conditions"`
	Findings    []Finding         `json:"findings"`
	Warnings    []string          `json:"warnings,omitempty"`
	Unknown     map[string]string `json:"unknown,omitempty"`
}

// Endpoints are the addresses of the InferenceService and how it is exposed.
type Endpoints struct {
	External  string     `json:"external,omitempty"`
	InCluster string     `json:"inCluster,omitempty"`
	Exposure  []Exposure `json:"exposure,omitempty"`
	// Routable is false when a finding says the external address cannot be reached.
	Routable bool `json:"routable"`
}

// Exposure is one Ingress or HTTPRoute that publishes the InferenceService.
type Exposure struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Via     string `json:"via,omitempty"` // IngressClass name, or Gateway namespace/name
	Address string `json:"address,omitempty"`
	// Leftover marks an Ingress that no longer publishes the external address (see IsLeftoverIngress).
	Leftover bool `json:"leftover,omitempty"`
	// Broken marks an exposure that a finding says cannot route (for example a rejected HTTPRoute).
	Broken bool `json:"broken,omitempty"`
}

// Usable reports whether requests may be sent through the exposure.
func (e Exposure) Usable() bool { return e.Address != "" && !e.Leftover && !e.Broken }

// markBroken flags each exposure that a not-routable finding names: the Ingress, the HTTPRoute, or its Gateway.
func markBroken(exposures []Exposure, findings []Finding) {
	for i := range exposures {
		e := &exposures[i]
		objects := []string{strings.ToLower(e.Kind) + "/" + e.Name}
		if gw, ok := strings.CutPrefix(e.Via, "Gateway "); ok {
			objects = append(objects, "gateway/"+gw)
		}
		for _, f := range findings {
			if f.Title == titleNotRoutable && slices.Contains(objects, f.Object) {
				e.Broken = true
			}
		}
	}
}

// ComponentState is the state of a predictor, transformer or explainer.
type ComponentState struct {
	Name          string       `json:"name"`
	Ready         string       `json:"ready"`
	Reason        string       `json:"reason,omitempty"`
	ReadyReplicas int32        `json:"readyReplicas"`
	Replicas      int32        `json:"replicas"`
	Autoscaling   *Autoscaling `json:"autoscaling,omitempty"`
}

// Autoscaling is the HorizontalPodAutoscaler range of a component.
type Autoscaling struct {
	Min    int32  `json:"min"`
	Max    int32  `json:"max"`
	Target string `json:"target,omitempty"`
}

// CanarySplit shows how traffic is shared between the stable predictor and the canaries.
type CanarySplit struct {
	StablePercent int64         `json:"stablePercent"`
	Canaries      []CanaryState `json:"canaries"`
	// Ready is the CanaryPredictorReady condition, covering all canaries.
	Ready string `json:"ready"`
}

// CanaryState is one canary predictor with its replicas.
type CanaryState struct {
	isvc.Canary
	ReadyReplicas int32 `json:"readyReplicas"`
	Replicas      int32 `json:"replicas"`
}

// Summarize turns a snapshot and its findings into the summary.
func Summarize(s *Snapshot, findings []Finding) *Summary {
	m := s.ISVC
	sum := &Summary{
		Name:        m.Name,
		Namespace:   m.Namespace,
		Mode:        m.Mode,
		Ready:       m.Ready(),
		Created:     m.Created,
		Model:       m.Model,
		Runtime:     s.Runtime,
		ModelStatus: m.ModelState,
		Conditions:  m.Conditions,
		Findings:    findings,
		Unknown:     s.Unknown,
		GPU:         s.GPU,
		Endpoints: Endpoints{
			External:  m.URL,
			InCluster: m.AddressURL,
			Exposure:  exposures(s),
			Routable:  true,
		},
	}
	if sum.Findings == nil {
		sum.Findings = []Finding{}
	}
	markBroken(sum.Endpoints.Exposure, findings)
	for _, f := range findings {
		if f.Title == titleNotRoutable {
			sum.Endpoints.Routable = false
		}
	}
	if m.URL == "" {
		sum.Endpoints.Routable = false
	}
	if m.OlderThanSupported {
		sum.Warnings = append(sum.Warnings, "KServe older than v0.16 is not supported; some fields may be missing")
	}

	for _, c := range m.Components {
		sum.Components = append(sum.Components, componentState(s, c.Name))
	}

	if p := newestPod(s.Pods, func(p *corev1.Pod) bool {
		return p.Labels[isvc.LabelComponent] == "predictor" && m.CanaryOf(p.Labels) == ""
	}); p != nil {
		sum.Startup = startupOf(p, isvc.ContainerStorageInitial, isvc.ContainerServer)
	}

	if len(m.Canaries) > 0 {
		split := &CanarySplit{StablePercent: 100, Ready: "Unknown"}
		for _, c := range m.Canaries {
			split.StablePercent -= c.TrafficPercent
			cs := CanaryState{Canary: c}
			cs.ReadyReplicas, cs.Replicas = replicas(s, "predictor", c.Name)
			split.Canaries = append(split.Canaries, cs)
		}
		if c, ok := m.Condition("CanaryPredictorReady"); ok {
			split.Ready = c.Status
		}
		sum.Canary = split
	}
	return sum
}

func componentState(s *Snapshot, name string) ComponentState {
	cs := ComponentState{Name: name, Ready: "Unknown"}
	if c, ok := s.ISVC.Condition(strings.ToUpper(name[:1]) + name[1:] + "Ready"); ok {
		cs.Ready, cs.Reason = c.Status, c.Reason
	}
	cs.ReadyReplicas, cs.Replicas = replicas(s, name, "")
	for _, h := range s.Autoscalers {
		if h.Labels[isvc.LabelComponent] != name || s.ISVC.CanaryOf(h.Labels) != "" {
			continue
		}
		a := &Autoscaling{Max: h.Spec.MaxReplicas, Min: 1, Target: metricTarget(h)}
		if h.Spec.MinReplicas != nil {
			a.Min = *h.Spec.MinReplicas
		}
		cs.Autoscaling = a
	}
	return cs
}

// replicas sums the Deployments of a component, or of one of its canaries; stable and canary objects share the
// component label and differ only in the app label.
func replicas(s *Snapshot, component, canary string) (ready, desired int32) {
	for _, d := range s.Deployments {
		if d.Labels[isvc.LabelComponent] != component || s.ISVC.CanaryOf(d.Labels) != canary {
			continue
		}
		ready += d.Status.ReadyReplicas
		if d.Spec.Replicas != nil {
			desired += *d.Spec.Replicas
		}
	}
	return ready, desired
}

func metricTarget(h autoscalingv2.HorizontalPodAutoscaler) string {
	var parts []string
	for _, m := range h.Spec.Metrics {
		if m.Resource == nil {
			parts = append(parts, string(m.Type))
			continue
		}
		t := m.Resource.Target
		switch {
		case t.AverageUtilization != nil:
			parts = append(parts, fmt.Sprintf("%s %d%%", m.Resource.Name, *t.AverageUtilization))
		case t.AverageValue != nil:
			parts = append(parts, fmt.Sprintf("%s %s", m.Resource.Name, t.AverageValue.String()))
		default:
			parts = append(parts, string(m.Resource.Name))
		}
	}
	return strings.Join(parts, ", ")
}

func exposures(s *Snapshot) []Exposure {
	var out []Exposure
	for _, ing := range s.Ingresses {
		e := Exposure{Kind: "Ingress", Name: ing.Name, Leftover: IsLeftoverIngress(ing, s.ISVC.URL)}
		if ing.Spec.IngressClassName != nil {
			e.Via = "IngressClass " + *ing.Spec.IngressClassName
		}
		for _, lb := range ing.Status.LoadBalancer.Ingress {
			e.Address = firstNonEmpty(lb.IP, lb.Hostname)
			break
		}
		out = append(out, e)
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
			out = append(out, e)
		}
	}
	return out
}

// GatewayAddress returns the first address of a Gateway, or "".
func gatewayAddress(gw *unstructured.Unstructured) string {
	addrs, _, _ := unstructured.NestedSlice(gw.Object, "status", "addresses")
	for _, a := range addrs {
		if m, ok := a.(map[string]any); ok {
			if v, _, _ := unstructured.NestedString(m, "value"); v != "" {
				return v
			}
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
