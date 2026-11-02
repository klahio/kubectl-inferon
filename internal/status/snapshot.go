// Package status gathers everything around one InferenceService, diagnoses problems and renders the summary
// described in docs/mvp.md section 4.
package status

import (
	"context"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/klahio/kubectl-inferon/internal/isvc"
	"github.com/klahio/kubectl-inferon/internal/kube"
)

var (
	httpRouteGVR = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}
	gatewayGVR   = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"}
)

// Runtime is the ServingRuntime or ClusterServingRuntime selected for the model.
type Runtime struct {
	Name             string   `json:"name"`
	Kind             string   `json:"kind"`
	ProtocolVersions []string `json:"protocolVersions,omitempty"`
	Found            bool     `json:"found"`
}

// Snapshot is everything the status command needs, read once from the cluster.
type Snapshot struct {
	ISVC           *isvc.InferenceService
	Runtime        *Runtime
	Pods           []corev1.Pod
	Deployments    []appsv1.Deployment
	Autoscalers    []autoscalingv2.HorizontalPodAutoscaler
	Ingresses      []networkingv1.Ingress
	IngressClasses []networkingv1.IngressClass
	HTTPRoutes     []unstructured.Unstructured
	Gateways       []unstructured.Unstructured
	Events         []corev1.Event
	GPU            *GPU
	// Unknown lists parts that could not be read (no permission, API not installed) with the reason.
	// Those parts are shown as unknown instead of failing the command (docs/mvp.md section 3).
	Unknown map[string]string
}

// Collect reads the InferenceService and the objects around it.
func Collect(ctx context.Context, c kube.Clients, namespace, name string, withEvents bool) (*Snapshot, error) {
	u, err := c.GetInferenceService(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	model, err := isvc.FromUnstructured(u)
	if err != nil {
		return nil, err
	}

	s := &Snapshot{ISVC: model, Unknown: map[string]string{}}
	selector := metav1.ListOptions{LabelSelector: isvc.LabelInferenceService + "=" + name}

	s.Runtime = collectRuntime(ctx, c.Dynamic, namespace, model, s.Unknown)

	if pods, err := c.Kube.CoreV1().Pods(namespace).List(ctx, selector); err != nil {
		s.markUnknown("pods", err)
	} else {
		s.Pods = pods.Items
	}
	s.GPU = collectGPU(ctx, c, s.Pods, s.podComponent, s.Unknown)
	if deps, err := c.Kube.AppsV1().Deployments(namespace).List(ctx, selector); err != nil {
		s.markUnknown("deployments", err)
	} else {
		s.Deployments = deps.Items
	}
	if hpas, err := c.Kube.AutoscalingV2().HorizontalPodAutoscalers(namespace).List(ctx, selector); err != nil {
		s.markUnknown("autoscalers", err)
	} else {
		s.Autoscalers = hpas.Items
	}

	// Ingresses and HTTPRoutes are matched by owner reference, so naming conventions do not matter (docs/mvp.md 2.2).
	if ings, err := c.Kube.NetworkingV1().Ingresses(namespace).List(ctx, metav1.ListOptions{}); err != nil {
		s.markUnknown("ingresses", err)
	} else {
		for _, ing := range ings.Items {
			if ownedBy(ing.OwnerReferences, model.UID) {
				s.Ingresses = append(s.Ingresses, ing)
			}
		}
	}
	if len(s.Ingresses) > 0 {
		if classes, err := c.Kube.NetworkingV1().IngressClasses().List(ctx, metav1.ListOptions{}); err != nil {
			s.markUnknown("ingressclasses", err)
		} else {
			s.IngressClasses = classes.Items
		}
	}

	if routes, err := c.Dynamic.Resource(httpRouteGVR).Namespace(namespace).List(ctx, metav1.ListOptions{}); err != nil {
		// Without the Gateway API CustomResourceDefinitions there is nothing to report (docs/mvp.md section 10).
		if !isNotInstalled(err) {
			s.markUnknown("httproutes", err)
		}
	} else {
		for _, r := range routes.Items {
			if ownedBy(ownerRefs(&r), model.UID) {
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

func collectRuntime(ctx context.Context, d dynamic.Interface, namespace string, m *isvc.InferenceService, unknown map[string]string) *Runtime {
	if m.Model.Runtime == "" {
		return nil
	}
	rt := &Runtime{Name: m.Model.Runtime, Kind: m.Model.RuntimeKind}
	var (
		obj *unstructured.Unstructured
		err error
	)
	if rt.Kind == "ClusterServingRuntime" {
		obj, err = d.Resource(isvc.ClusterServingRuntimeGVR).Get(ctx, rt.Name, metav1.GetOptions{})
	} else {
		obj, err = d.Resource(isvc.ServingRuntimeGVR).Namespace(namespace).Get(ctx, rt.Name, metav1.GetOptions{})
	}
	switch {
	case apierrors.IsNotFound(err):
		return rt
	case err != nil:
		unknown["runtime"] = reason(err)
		return rt
	}
	rt.Found = true
	rt.ProtocolVersions, _, _ = unstructured.NestedStringSlice(obj.Object, "spec", "protocolVersions")
	return rt
}

func collectGateways(ctx context.Context, d dynamic.Interface, routes []unstructured.Unstructured, unknown map[string]string) []unstructured.Unstructured {
	seen := map[string]bool{}
	var out []unstructured.Unstructured
	for _, r := range routes {
		for _, ref := range parentRefs(&r) {
			key := ref.Namespace + "/" + ref.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			gw, err := d.Resource(gatewayGVR).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				continue // reported as "Gateway missing" by the diagnosis
			}
			if err != nil {
				unknown["gateway "+key] = reason(err)
				continue
			}
			out = append(out, *gw)
		}
	}
	return out
}

func collectEvents(ctx context.Context, k kubernetes.Interface, namespace, name string, pods []corev1.Pod, unknown map[string]string) []corev1.Event {
	names := []string{name}
	for _, p := range pods {
		names = append(names, p.Name)
	}
	var out []corev1.Event
	for _, n := range names {
		list, err := k.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
			FieldSelector: fields.OneTermEqualSelector("involvedObject.name", n).String(),
		})
		if err != nil {
			unknown["events"] = reason(err)
			return out
		}
		out = append(out, list.Items...)
	}
	sort.Slice(out, func(i, j int) bool { return eventTime(out[i]).Time.Before(eventTime(out[j]).Time) })
	return out
}

func eventTime(e corev1.Event) metav1.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp
	}
	if !e.EventTime.IsZero() {
		return metav1.NewTime(e.EventTime.Time)
	}
	return e.CreationTimestamp
}

func (s *Snapshot) markUnknown(part string, err error) { s.Unknown[part] = reason(err) }

func reason(err error) string {
	switch {
	case apierrors.IsForbidden(err):
		return "no permission"
	case isNotInstalled(err):
		return "not installed in this cluster"
	default:
		return err.Error()
	}
}

func isNotInstalled(err error) bool {
	return apierrors.IsNotFound(err) || meta.IsNoMatchError(err) || strings.Contains(err.Error(), "the server could not find the requested resource")
}

// ownedBy matches by UID, which is unique, so it works for every owner kind.
func ownedBy(refs []metav1.OwnerReference, uid string) bool {
	for _, r := range refs {
		if string(r.UID) == uid {
			return true
		}
	}
	return false
}

func ownerRefs(u *unstructured.Unstructured) []metav1.OwnerReference { return u.GetOwnerReferences() }

// ParentRef is a Gateway an HTTPRoute attaches to.
type ParentRef struct {
	Namespace string
	Name      string
}

func parentRefs(route *unstructured.Unstructured) []ParentRef {
	raw, _, _ := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
	var out []ParentRef
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if kind, _, _ := unstructured.NestedString(m, "kind"); kind != "" && kind != "Gateway" {
			continue
		}
		name, _, _ := unstructured.NestedString(m, "name")
		ns, _, _ := unstructured.NestedString(m, "namespace")
		if ns == "" {
			ns = route.GetNamespace()
		}
		out = append(out, ParentRef{Namespace: ns, Name: name})
	}
	return out
}
