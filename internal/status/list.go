package status

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"

	"github.com/klahio/kubectl-inferon/internal/isvc"
	"github.com/klahio/kubectl-inferon/internal/kube"
	"github.com/klahio/kubectl-inferon/internal/llmisvc"
)

// Resource types as users write them in TYPE/NAME. `list` prints them, so its output can be passed to the other
// commands as it is.
const (
	TypeInferenceService    = "isvc"
	TypeLLMInferenceService = "llmisvc"
)

// Item is one inference service as `list` shows it.
type Item struct {
	Type      string    `json:"type"`
	Kind      string    `json:"kind"`
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	Ready     string    `json:"ready"`
	Model     string    `json:"model,omitempty"`
	Problem   string    `json:"problem,omitempty"`
	Created   time.Time `json:"created"`
}

// Ref is the TYPE/NAME argument of the other commands.
func (i Item) Ref() string { return i.Type + "/" + i.Name }

// Listing is the result of List.
type Listing struct {
	Items []Item `json:"items"`
	// Unknown lists the types or parts that could not be read, with the reason.
	Unknown map[string]string `json:"unknown,omitempty"`
}

// ErrNothingInstalled means the cluster serves none of the resource types the plugin handles.
var ErrNothingInstalled = errors.New("none of the supported inference service types (isvc, llmisvc) is installed in this cluster")

// List reads every inference service the plugin handles, in one namespace or, when namespace is "", in all of them.
// The problem column comes from the same pod rules as `status`, but pods are listed once per type for the whole
// scope, so a long list costs a few calls, not a few per service. Deeper checks (routes, logs) stay with `status`.
func List(ctx context.Context, c kube.Clients, namespace string) (*Listing, error) {
	l := &Listing{Items: []Item{}, Unknown: map[string]string{}}
	installed := 0

	isvcs, err := c.Dynamic.Resource(isvc.InferenceServiceGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	switch {
	case err == nil:
		installed++
		pods := listPods(ctx, c, namespace, isvc.LabelInferenceService, TypeInferenceService, len(isvcs.Items), l.Unknown)
		for i := range isvcs.Items {
			m, err := isvc.FromUnstructured(&isvcs.Items[i])
			if err != nil {
				return nil, err
			}
			l.Items = append(l.Items, InferenceServiceItem(m, podsOf(pods, m.Namespace, isvc.LabelInferenceService, m.Name)))
		}
	case isNotInstalled(err):
	case apierrors.IsForbidden(err):
		installed++
		l.Unknown[TypeInferenceService] = reason(err)
	default:
		return nil, err
	}

	switch v, err := c.ServedVersion(llmisvc.Group, llmisvc.Resource, llmisvc.PreferredVersions); {
	case errors.Is(err, kube.ErrNotInstalled):
	case apierrors.IsForbidden(err):
		installed++
		l.Unknown[TypeLLMInferenceService] = reason(err)
	case err != nil:
		return nil, err
	default:
		installed++
		list, err := c.Dynamic.Resource(llmisvc.GVR(v)).Namespace(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			if !apierrors.IsForbidden(err) {
				return nil, err
			}
			l.Unknown[TypeLLMInferenceService] = reason(err)
			break
		}
		pods := listPods(ctx, c, namespace, llmisvc.LabelPartOf+"="+llmisvc.PartOfValue, TypeLLMInferenceService, len(list.Items), l.Unknown)
		for i := range list.Items {
			s, err := llmisvc.FromUnstructured(&list.Items[i])
			if err != nil {
				return nil, err
			}
			l.Items = append(l.Items, LLMInferenceServiceItem(s, podsOf(pods, s.Namespace, llmisvc.LabelName, s.Name)))
		}
	}

	if installed == 0 {
		return nil, ErrNothingInstalled
	}
	// Every installed type was refused: an empty table would look like "nothing there", so fail like kubectl get.
	if refused := len(typesOnly(l.Unknown)); refused == installed {
		return nil, fmt.Errorf("cannot list inference services: no permission (%s)", strings.Join(typesOnly(l.Unknown), ", "))
	}
	sort.SliceStable(l.Items, func(i, j int) bool {
		a, b := l.Items[i], l.Items[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Type < b.Type
	})
	return l, nil
}

// typesOnly returns the refused resource types among the unknown parts, leaving out their pods.
func typesOnly(unknown map[string]string) []string {
	var out []string
	for k := range unknown {
		if !strings.HasSuffix(k, " pods") {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// listPods lists the pods of one resource type in the scope; it skips the call when there is nothing to match.
func listPods(ctx context.Context, c kube.Clients, namespace, selector, typ string, services int, unknown map[string]string) []corev1.Pod {
	if services == 0 {
		return nil
	}
	pods, err := c.Kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		unknown[typ+" pods"] = reason(err)
		return nil
	}
	return pods.Items
}

func podsOf(pods []corev1.Pod, namespace, label, name string) []corev1.Pod {
	var out []corev1.Pod
	for _, p := range pods {
		if p.Namespace == namespace && p.Labels[label] == name {
			out = append(out, p)
		}
	}
	return out
}

// InferenceServiceItem builds the list row of an InferenceService from the service and its pods.
func InferenceServiceItem(m *isvc.InferenceService, pods []corev1.Pod) Item {
	it := Item{
		Type:      TypeInferenceService,
		Kind:      "InferenceService",
		Namespace: m.Namespace,
		Name:      m.Name,
		Ready:     m.Ready(),
		Model:     m.Model.Format,
		Created:   m.Created,
	}
	if m.Model.CustomContainer {
		it.Model = "custom container"
	}
	switch {
	case m.Mode != "" && !m.ModeSupported():
		it.Problem = fmt.Sprintf("deployment mode %s is not supported", m.Mode)
	case m.ModelState.LastFailure != nil && m.ModelState.LastFailure.Reason == "NoSupportingRuntime":
		it.Problem = "No serving runtime supports this model"
	default:
		it.Problem = firstProblem(podFindings(&Snapshot{ISVC: m, Pods: pods}))
	}
	return it
}

// LLMInferenceServiceItem builds the list row of an LLMInferenceService from the service and its pods.
func LLMInferenceServiceItem(s *llmisvc.Service, pods []corev1.Pod) Item {
	it := Item{
		Type:      TypeLLMInferenceService,
		Kind:      "LLMInferenceService",
		Namespace: s.Namespace,
		Name:      s.Name,
		Ready:     s.Ready(),
		Model:     s.ModelName,
		Created:   s.Created,
	}
	if it.Model == "" {
		it.Model = s.ModelURI
	}
	if c, ok := s.Condition("PresetsCombined"); ok && c.Status == "False" {
		it.Problem = "Preset cannot be applied"
	} else {
		it.Problem = firstProblem(diagnosePods(pods, llmPodRole))
	}
	return it
}

// problemIsError tells a real problem from a state such as "Starting", for the color of the column.
func (i Item) problemIsError() bool {
	return i.Problem != "" && i.Problem != "Starting" && i.Problem != "Downloading the model"
}

// firstProblem names the first error, or else the first finding ("Starting", say), so a replica that is still
// starting does not hide one that crashes.
func firstProblem(findings []Finding) string {
	if errs := errorsOnly(findings); len(errs) > 0 {
		return errs[0].Title
	}
	if len(findings) > 0 {
		return findings[0].Title
	}
	return ""
}

// RenderList writes the list as a table. withNamespace adds the namespace column, for --all-namespaces.
func (r TextRenderer) RenderList(w io.Writer, l *Listing, withNamespace bool) error {
	now := r.Now
	if now.IsZero() {
		now = time.Now()
	}
	p := &printer{w: w}
	tw := p.table()
	header := "NAME\tREADY\tMODEL\tPROBLEM\tAGE"
	if withNamespace {
		header = "NAMESPACE\t" + header
	}
	fmt.Fprintln(tw, header)
	for _, it := range l.Items {
		age := "-"
		if !it.Created.IsZero() {
			age = duration.HumanDuration(now.Sub(it.Created))
		}
		row := fmt.Sprintf("%s\t%s\t%s\t%s\t%s", it.Ref(), r.conditionStatus(it.Ready), dash(it.Model), r.problem(it), age)
		if withNamespace {
			row = it.Namespace + "\t" + row
		}
		fmt.Fprintln(tw, row)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	return p.err
}

func (r TextRenderer) problem(it Item) string {
	switch {
	case it.Problem == "":
		return "-"
	case it.problemIsError():
		return r.paint(colorRed, it.Problem)
	default:
		return r.paint(colorDim, it.Problem)
	}
}
