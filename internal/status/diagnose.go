package status

import (
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klahio/kubectl-inferon/internal/isvc"
	"github.com/klahio/kubectl-inferon/internal/version"
)

// Severity of a finding.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
	// SeverityInfo marks a state that is not a problem yet, such as a model that is still loading.
	SeverityInfo Severity = "info"
)

// LogRef points at a container whose last log lines help explain a finding.
type LogRef struct {
	Pod       string `json:"pod"`
	Container string `json:"container"`
	// Previous selects the last terminated instance, for containers that already restarted.
	Previous bool `json:"previous,omitempty"`
	// Scan asks for a longer tail searched for a known root cause (ExplainLog) instead of just the last lines,
	// because a crashing server's last line rarely says why (docs/mvp.md section 6a.2).
	Scan bool `json:"scan,omitempty"`
}

// Finding is one diagnosed problem, with the evidence it is based on.
type Finding struct {
	Severity Severity `json:"severity"`
	Title    string   `json:"title"`
	Details  []string `json:"details,omitempty"`
	Object   string   `json:"object,omitempty"`
	Hint     string   `json:"hint,omitempty"`
	Logs     *LogRef  `json:"logs,omitempty"`
	LogLines []string `json:"logLines,omitempty"`
}

// Diagnose applies the rules of docs/mvp.md section 4.2 and returns the findings, most important first.
func Diagnose(s *Snapshot) []Finding {
	var out []Finding
	m := s.ISVC
	pods := podFindings(s)

	if f := m.ModelState.LastFailure; f != nil && f.Reason == "NoSupportingRuntime" {
		out = append(out, Finding{
			Severity: SeverityError,
			Title:    "No serving runtime supports this model",
			Details:  []string{fmt.Sprintf("model format %q: %s", m.Model.Format, f.Message)},
			Object:   "inferenceservice/" + m.Name,
			Hint:     "check spec.predictor.model.modelFormat, or install a ServingRuntime that lists this format with autoSelect: true",
		})
	} else if f != nil && f.Reason != "" && len(pods) == 0 {
		// KServe copies the failing container's termination message here (for example the whole storage-initializer
		// log), so a pod finding already says the same thing with better context. Use this only as a fallback.
		out = append(out, Finding{
			Severity: SeverityError,
			Title:    "Model failed to load (" + f.Reason + ")",
			Details:  nonEmpty(lastLine(f.Message), locationLine(f)),
			Object:   "inferenceservice/" + m.Name,
		})
	}
	if m.Model.Runtime != "" && s.Runtime != nil && !s.Runtime.Found && s.Unknown["runtime"] == "" {
		out = append(out, Finding{
			Severity: SeverityError,
			Title:    "Selected runtime does not exist",
			Details:  []string{fmt.Sprintf("%s %q was recorded in the status but cannot be found", m.Model.RuntimeKind, m.Model.Runtime)},
		})
	}

	out = append(out, pods...)
	out = append(out, deploymentFindings(s)...)

	if c, ok := m.Condition("CanaryPredictorReady"); ok && c.Status == "False" {
		out = append(out, Finding{
			Severity: SeverityError,
			Title:    "Canary predictor is not ready",
			Details:  nonEmpty(joinReason(c.Reason, c.Message)),
			Object:   "inferenceservice/" + m.Name,
		})
	}

	out = append(out, exposureFindings(s)...)

	// Nothing specific found, yet KServe says it is not Ready: show what KServe reports.
	if len(errorsOnly(out)) == 0 && !hasInfo(out) && m.Ready() != "True" {
		var details []string
		for _, c := range m.Conditions {
			if c.Status != "True" && c.Type != "Stopped" {
				details = append(details, fmt.Sprintf("%s=%s %s", c.Type, c.Status, joinReason(c.Reason, c.Message)))
			}
		}
		if len(details) == 0 {
			details = []string{"the controller has not reported any conditions yet"}
		}
		out = append(out, Finding{Severity: SeverityError, Title: "Not Ready", Details: details, Object: "inferenceservice/" + m.Name})
	}
	return out
}

// podFindings reports at most one finding per pod: the first problem along init containers, then containers.
// Identical findings across replicas are merged into one with a pod count.
func podFindings(s *Snapshot) []Finding {
	return diagnosePods(s.Pods, func(p *corev1.Pod) string {
		if canary := s.ISVC.CanaryOf(p.Labels); canary != "" {
			return "canary " + canary
		}
		return p.Labels[isvc.LabelComponent]
	})
}

// diagnosePods applies the pod rules to any set of pods; describe names a pod's component for the output.
func diagnosePods(all []corev1.Pod, describe func(*corev1.Pod) string) []Finding {
	type keyed struct {
		f     Finding
		count int
	}
	var order []string
	byKey := map[string]*keyed{}

	pods := append([]corev1.Pod(nil), all...)
	sort.SliceStable(pods, func(i, j int) bool { return componentRank(pods[i]) < componentRank(pods[j]) })

	for i := range pods {
		// A pod being deleted (the old pod of a rolling update) exits with SIGTERM; that is not a failure.
		if pods[i].DeletionTimestamp != nil {
			continue
		}
		f, ok := podFinding(&pods[i], describe(&pods[i]))
		if !ok {
			continue
		}
		key := f.Title + "|" + strings.Join(f.Details, "|")
		if k, seen := byKey[key]; seen {
			k.count++
			continue
		}
		byKey[key] = &keyed{f: f, count: 1}
		order = append(order, key)
	}

	out := make([]Finding, 0, len(order))
	for _, key := range order {
		k := byKey[key]
		if k.count > 1 {
			k.f.Details = append(k.f.Details, fmt.Sprintf("same problem in %d pods", k.count))
		}
		out = append(out, k.f)
	}
	return out
}

func podFinding(p *corev1.Pod, component string) (Finding, bool) {
	object := "pod/" + p.Name
	if component != "" {
		object = component + " " + object
	}

	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return Finding{
				Severity: SeverityError,
				Title:    "Pod cannot be scheduled",
				Details:  nonEmpty(c.Message),
				Object:   object,
				Hint:     "lower the resource requests, or free capacity on the nodes",
			}, true
		}
	}

	for _, cs := range p.Status.InitContainerStatuses {
		if cs.Name != isvc.ContainerStorageInitial {
			continue
		}
		// An out-of-memory kill also exits non-zero (137); containerFinding reports it as what it is.
		if t := failedTermination(cs); t != nil && t.Reason != reasonOOMKilled {
			return Finding{
				Severity: SeverityError,
				Title:    "Model download failed",
				Details:  nonEmpty(fmt.Sprintf("%s exited with code %d (restarts: %d)", cs.Name, t.ExitCode, cs.RestartCount), lastLine(t.Message)),
				Object:   object,
				Hint:     "check spec.predictor.model.storageUri and the storage credentials",
				Logs:     &LogRef{Pod: p.Name, Container: cs.Name, Previous: cs.State.Terminated == nil},
			}, true
		}
	}

	for _, cs := range append(append([]corev1.ContainerStatus(nil), p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		if f, ok := containerFinding(p, cs, object); ok {
			return f, true
		}
	}

	// The model download is still running: not a failure, and the usual reason a new service is not Ready yet.
	for _, cs := range p.Status.InitContainerStatuses {
		if cs.Name == isvc.ContainerStorageInitial && cs.State.Running != nil {
			return Finding{
				Severity: SeverityInfo,
				Title:    "Downloading the model",
				Details:  []string{fmt.Sprintf("%s has been running for %s", cs.Name, since(cs.State.Running.StartedAt.Time))},
				Object:   object,
				Logs:     &LogRef{Pod: p.Name, Container: cs.Name},
			}, true
		}
	}

	// Running but not ready, and never restarted: a large model can take minutes to load, which is not a failure.
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Running != nil && !cs.Ready && cs.RestartCount == 0 {
			return Finding{
				Severity: SeverityInfo,
				Title:    "Starting",
				Details:  []string{fmt.Sprintf("container %s has been starting for %s; loading a model can take minutes", cs.Name, since(cs.State.Running.StartedAt.Time))},
				Object:   object,
			}, true
		}
	}
	return Finding{}, false
}

func containerFinding(p *corev1.Pod, cs corev1.ContainerStatus, object string) (Finding, bool) {
	if w := cs.State.Waiting; w != nil {
		switch w.Reason {
		case "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
			return Finding{
				Severity: SeverityError,
				Title:    "Image cannot be pulled",
				Details:  nonEmpty("image "+cs.Image, shorten(w.Message, 300)),
				Object:   object + " container " + cs.Name,
				Hint:     "check the image name and tag (for built-in runtimes: spec.predictor.model.runtimeVersion)",
			}, true
		case "CreateContainerConfigError", "CreateContainerError", "RunContainerError":
			return Finding{
				Severity: SeverityError,
				Title:    "Container cannot be created",
				Details:  nonEmpty(w.Reason + ": " + w.Message),
				Object:   object + " container " + cs.Name,
			}, true
		}
	}

	if t := failedTermination(cs); t != nil && t.Reason == reasonOOMKilled {
		hint := "raise resources.limits.memory"
		if cs.Name == isvc.ContainerStorageInitial {
			hint = "raise storageInitializer.memoryLimit in the inferenceservice-config ConfigMap of the KServe namespace"
		}
		return Finding{
			Severity: SeverityError,
			Title:    "Out of memory",
			Details:  []string{fmt.Sprintf("container %s was killed for exceeding its memory limit (%s), restarts: %d", cs.Name, memoryLimit(p, cs.Name), cs.RestartCount)},
			Object:   object,
			Hint:     hint,
		}, true
	}
	if t := failedTermination(cs); t != nil {
		return Finding{
			Severity: SeverityError,
			Title:    "Server crashes on start",
			Details:  nonEmpty(fmt.Sprintf("container %s exited with code %d (%s), restarts: %d", cs.Name, t.ExitCode, t.Reason, cs.RestartCount), lastLine(t.Message)),
			Object:   object,
			Logs:     &LogRef{Pod: p.Name, Container: cs.Name, Previous: cs.State.Terminated == nil, Scan: true},
		}, true
	}
	return Finding{}, false
}

const reasonOOMKilled = "OOMKilled"

// failedTermination returns the current or last termination when it ended with a non-zero exit code. A last
// termination counts only while the container is not ready again: one that recovered is history, not a failure.
func failedTermination(cs corev1.ContainerStatus) *corev1.ContainerStateTerminated {
	if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
		return t
	}
	if t := cs.LastTerminationState.Terminated; t != nil && t.ExitCode != 0 && !cs.Ready {
		return t
	}
	return nil
}

func deploymentFindings(s *Snapshot) []Finding { return replicaFailures(s.Deployments) }

// replicaFailures reports Deployments whose pods the API server refuses to create.
func replicaFailures(deployments []appsv1.Deployment) []Finding {
	var out []Finding
	for _, d := range deployments {
		for _, c := range d.Status.Conditions {
			if c.Type == "ReplicaFailure" && c.Status == corev1.ConditionTrue {
				out = append(out, Finding{
					Severity: SeverityError,
					Title:    "Pods cannot be created",
					Details:  nonEmpty(joinReason(c.Reason, c.Message)),
					Object:   "deployment/" + d.Name,
					Hint:     "often a ResourceQuota, a LimitRange or an admission policy rejecting the pod",
				})
			}
		}
	}
	return out
}

// exposureFindings implements the "external address not routable" rules for Ingress and Gateway API.
func exposureFindings(s *Snapshot) []Finding {
	var out []Finding
	for _, ing := range s.Ingresses {
		object := "ingress/" + ing.Name
		if IsLeftoverIngress(ing, s.ISVC.URL) {
			// Seen on the lab cluster: after KServe switches to Gateway API it creates HTTPRoutes but keeps the old
			// Ingress. It no longer serves the external address, so its problems say nothing about routability.
			out = append(out, Finding{
				Severity: SeverityWarning,
				Title:    "Leftover Ingress",
				Details: []string{
					fmt.Sprintf("its hosts (%s) do not include the external address host %s", strings.Join(ingressHosts(ing), ", "), hostOf(s.ISVC.URL)),
					"KServe keeps the old Ingress after a switch to Gateway API",
				},
				Object: object,
				Hint:   fmt.Sprintf("delete it when nothing uses it any more: kubectl delete ingress %s -n %s", ing.Name, ing.Namespace),
			})
			continue
		}
		className := ""
		if ing.Spec.IngressClassName != nil {
			className = *ing.Spec.IngressClassName
		}
		if _, unknown := s.Unknown["ingressclasses"]; !unknown && !ingressClassExists(s.IngressClasses, className) {
			detail := fmt.Sprintf("IngressClass %q does not exist", className)
			if className == "" {
				detail = "no ingressClassName is set and there is no default IngressClass"
			}
			out = append(out, notRoutable(s, object, detail))
			continue
		}
		if len(ing.Status.LoadBalancer.Ingress) == 0 {
			out = append(out, notRoutable(s, object, "the ingress controller has not assigned an address"))
		}
	}

	out = append(out, routeFindings(s.HTTPRoutes, s.Gateways, s.Unknown, func(object, detail string) Finding {
		return notRoutable(s, object, detail)
	})...)
	return out
}

// routeFindings checks HTTPRoutes and the Gateways they attach to, for any resource kind. notRoutable builds the
// finding, so each kind can add its own hint.
func routeFindings(routes, gatewayList []unstructured.Unstructured, unknown map[string]string, notRoutable func(object, detail string) Finding) []Finding {
	var out []Finding
	gateways := map[string]*unstructured.Unstructured{}
	for i := range gatewayList {
		gateways[gatewayList[i].GetNamespace()+"/"+gatewayList[i].GetName()] = &gatewayList[i]
	}
	for i := range routes {
		r := &routes[i]
		object := "httproute/" + r.GetName()
		for _, problem := range routeProblems(r) {
			out = append(out, notRoutable(object, problem))
		}
		for _, ref := range parentRefs(r) {
			key := ref.Namespace + "/" + ref.Name
			if _, unknown := unknown["gateway "+key]; unknown {
				continue
			}
			gw, ok := gateways[key]
			if !ok {
				out = append(out, notRoutable(object, fmt.Sprintf("Gateway %s does not exist", key)))
				continue
			}
			if c, ok := findCondition(gw.Object, "Programmed"); !ok || c.Status != "True" {
				out = append(out, notRoutable("gateway/"+key, "Gateway is not Programmed "+joinReason(c.Reason, c.Message)))
				continue
			}
			if addrs, _, _ := unstructured.NestedSlice(gw.Object, "status", "addresses"); len(addrs) == 0 {
				out = append(out, notRoutable("gateway/"+key, "Gateway has no address"))
			}
		}
	}
	return out
}

// IsLeftoverIngress reports an Ingress whose hosts do not include the InferenceService's external host, so it
// cannot be what publishes the external address.
func IsLeftoverIngress(ing networkingv1.Ingress, externalURL string) bool {
	host := hostOf(externalURL)
	hosts := ingressHosts(ing)
	return host != "" && len(hosts) > 0 && !slices.Contains(hosts, host)
}

func ingressHosts(ing networkingv1.Ingress) []string {
	var hosts []string
	for _, r := range ing.Spec.Rules {
		if r.Host != "" {
			hosts = append(hosts, r.Host)
		}
	}
	return hosts
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

const titleNotRoutable = "External address not routable"

func notRoutable(s *Snapshot, object, detail string) Finding {
	f := Finding{
		Severity: SeverityWarning,
		Title:    titleNotRoutable,
		Details:  []string{detail},
		Object:   object,
		Hint:     fmt.Sprintf("`%s test isvc/%s` falls back to port-forward", version.Command, s.ISVC.Name),
	}
	if c, ok := s.ISVC.Condition("IngressReady"); ok && c.IsTrue() {
		f.Details = append(f.Details, "KServe still reports IngressReady=True")
	}
	return f
}

// routeProblems checks every parent's Accepted and ResolvedRefs conditions in status.parents[].
func routeProblems(r *unstructured.Unstructured) []string {
	parents, _, _ := unstructured.NestedSlice(r.Object, "status", "parents")
	if len(parents) == 0 {
		return []string{"no Gateway controller has accepted the route yet"}
	}
	var out []string
	for _, p := range parents {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		gw, _, _ := unstructured.NestedString(pm, "parentRef", "name")
		for _, t := range []string{"Accepted", "ResolvedRefs"} {
			c, found := conditionIn(pm, t)
			if !found || c.Status != "True" {
				out = append(out, fmt.Sprintf("%s is not True for Gateway %q %s", t, gw, joinReason(c.Reason, c.Message)))
			}
		}
	}
	return out
}

func findCondition(obj map[string]any, t string) (isvc.Condition, bool) {
	status, _, _ := unstructured.NestedMap(obj, "status")
	return conditionIn(status, t)
}

func conditionIn(m map[string]any, t string) (isvc.Condition, bool) {
	raw, _, _ := unstructured.NestedSlice(m, "conditions")
	for _, r := range raw {
		cm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if ct, _, _ := unstructured.NestedString(cm, "type"); ct == t {
			c := isvc.Condition{Type: t}
			c.Status, _, _ = unstructured.NestedString(cm, "status")
			c.Reason, _, _ = unstructured.NestedString(cm, "reason")
			c.Message, _, _ = unstructured.NestedString(cm, "message")
			return c, true
		}
	}
	return isvc.Condition{}, false
}

func ingressClassExists(classes []networkingv1.IngressClass, name string) bool {
	for _, c := range classes {
		if name == "" && c.Annotations["ingressclass.kubernetes.io/is-default-class"] == "true" {
			return true
		}
		if name != "" && c.Name == name {
			return true
		}
	}
	return false
}

// IsNoise reports events that are not real problems: the controller's optimistic-locking retries (docs/mvp.md 4.2).
func IsNoise(e corev1.Event) bool {
	return e.Type == corev1.EventTypeWarning &&
		(e.Reason == "UpdateFailed" || e.Reason == "InternalError") &&
		strings.Contains(e.Message, "the object has been modified")
}

func componentRank(p corev1.Pod) int {
	switch p.Labels[isvc.LabelComponent] {
	case "transformer":
		return 1
	case "explainer":
		return 2
	default:
		return 0
	}
}

func memoryLimit(p *corev1.Pod, container string) string {
	for _, c := range append(append([]corev1.Container(nil), p.Spec.InitContainers...), p.Spec.Containers...) {
		if c.Name == container {
			if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
				return q.String()
			}
		}
	}
	return "no limit set"
}

func hasInfo(fs []Finding) bool {
	for _, f := range fs {
		if f.Severity == SeverityInfo {
			return true
		}
	}
	return false
}

func errorsOnly(fs []Finding) []Finding {
	var out []Finding
	for _, f := range fs {
		if f.Severity == SeverityError {
			out = append(out, f)
		}
	}
	return out
}

func locationLine(f *isvc.Failure) string {
	if f.Location == "" {
		return ""
	}
	return "location: " + f.Location
}

func joinReason(reason, message string) string {
	switch {
	case reason == "" && message == "":
		return ""
	case reason == "":
		return message
	case message == "":
		return reason
	default:
		return reason + ": " + message
	}
}

// lastLine keeps the last non-empty line of a multi-line termination message, which is usually the actual error.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return shorten(l, 300)
		}
	}
	return ""
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func nonEmpty(lines ...string) []string {
	var out []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
