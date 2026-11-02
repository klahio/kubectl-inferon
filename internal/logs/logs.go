// Package logs streams the logs of an InferenceService's pods (docs/mvp.md section 6).
package logs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/klahio/kubectl-inferon/internal/isvc"
	"github.com/klahio/kubectl-inferon/internal/llmisvc"
)

// ComponentAll selects the pods of every component together.
const ComponentAll = "all"

// Layout describes how one resource kind labels its pods and names its server container.
type Layout struct {
	// Kind is used in messages, for example "LLMInferenceService".
	Kind string
	// Selector returns the label selector for all pods of one resource.
	Selector func(name string) string
	// ComponentOf names a pod's component from its labels.
	ComponentOf func(labels map[string]string) string
	// Components are the --component values without "all"; the first one is the default.
	Components      []string
	ServerContainer string
}

// InferenceServiceLayout: pods carry serving.kserve.io/inferenceservice and component labels (docs/mvp.md section 6).
var InferenceServiceLayout = Layout{
	Kind:            "InferenceService",
	Selector:        func(name string) string { return isvc.LabelInferenceService + "=" + name },
	ComponentOf:     func(l map[string]string) string { return l[isvc.LabelComponent] },
	Components:      []string{"predictor", "transformer", "explainer"},
	ServerContainer: isvc.ContainerServer,
}

// LLMInferenceServiceLayout: components main (the primary workload), prefill, scheduler and worker (the non-leader
// pods of a multi-node LeaderWorkerSet), from docs/mvp.md section 6a.4.
var LLMInferenceServiceLayout = Layout{
	Kind:     "LLMInferenceService",
	Selector: llmisvc.Selector,
	ComponentOf: func(l map[string]string) string {
		if llmisvc.IsWorker(l) {
			return "worker"
		}
		if role := llmisvc.RoleOf(l); role != llmisvc.RolePrimary {
			return role
		}
		return "main"
	},
	Components:      []string{"main", "prefill", "scheduler", "worker"},
	ServerContainer: llmisvc.ContainerServer,
}

// Accepted returns the --component values of a layout, "all" included.
func (l Layout) Accepted() []string {
	return append(append([]string(nil), l.Components...), ComponentAll)
}

func (l Layout) rank(component string) int {
	for i, c := range l.Components {
		if c == component {
			return i
		}
	}
	return len(l.Components)
}

// Options mirror the logs command's flags.
type Options struct {
	// Layout is the resource kind; the zero value means InferenceServiceLayout.
	Layout        Layout
	Component     string
	Init          bool
	AllContainers bool
	Follow        bool
	Previous      bool
	Since         time.Duration
	// Tail is the number of lines per container; a negative value means all lines.
	Tail int64
	// Prefix forces the "[component/pod/container]" prefix on or off; nil means "on when there is more than one stream".
	Prefix *bool
	Color  bool
	// PollInterval is how often follow mode looks for new pods and restarted containers.
	PollInterval time.Duration
}

// Stream is one container whose log is shown.
type Stream struct {
	Component string
	Pod       string
	Container string
}

func (s Stream) key() string { return s.Pod + "/" + s.Container }

// Label is the prefix written before each line.
func (s Stream) Label() string {
	component := s.Component
	if component == "" {
		component = "-"
	}
	return component + "/" + s.Pod + "/" + s.Container
}

// Select returns the streams for the given pods, in a stable order: component, then pod, then container order in the spec.
func Select(pods []corev1.Pod, o Options) []Stream {
	l := o.layout()
	sorted := append([]corev1.Pod(nil), pods...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ci, cj := l.rank(l.ComponentOf(sorted[i].Labels)), l.rank(l.ComponentOf(sorted[j].Labels))
		if ci != cj {
			return ci < cj
		}
		return sorted[i].Name < sorted[j].Name
	})

	var out []Stream
	for _, p := range sorted {
		component := l.ComponentOf(p.Labels)
		if o.Component != ComponentAll && component != o.Component {
			continue
		}
		for _, name := range containerNames(p, o, l.ServerContainer) {
			out = append(out, Stream{Component: component, Pod: p.Name, Container: name})
		}
	}
	return out
}

func (o Options) layout() Layout {
	if o.Layout.Selector == nil {
		return InferenceServiceLayout
	}
	return o.Layout
}

func containerNames(p corev1.Pod, o Options, server string) []string {
	var names []string
	switch {
	case o.AllContainers:
		for _, c := range p.Spec.InitContainers {
			names = append(names, c.Name)
		}
		for _, c := range p.Spec.Containers {
			names = append(names, c.Name)
		}
	case o.Init:
		for _, c := range p.Spec.InitContainers {
			if c.Name == isvc.ContainerStorageInitial {
				names = append(names, c.Name)
			}
		}
	default:
		for _, c := range p.Spec.Containers {
			if c.Name == server {
				return []string{c.Name}
			}
		}
		// A custom predictor may name its container differently; its first container is the server.
		if len(p.Spec.Containers) > 0 {
			names = append(names, p.Spec.Containers[0].Name)
		}
	}
	return names
}

// ErrNoPods means no pod matched the InferenceService and component.
var ErrNoPods = errors.New("no pods found")

// ErrNoneReadable means containers were selected but none of their logs could be read; the reasons were already
// written to Notes.
var ErrNoneReadable = errors.New("no container log could be read")

// Streamer writes log lines from the selected containers.
type Streamer struct {
	Kube      kubernetes.Interface
	Namespace string
	Name      string // InferenceService name
	Options   Options
	Out       io.Writer
	// Notes receives messages that are not log lines, such as "container not started yet".
	Notes io.Writer

	mu     sync.Mutex
	prefix bool
	colors map[string]string
}

// Run prints the logs once, or keeps following them with Options.Follow.
func (s *Streamer) Run(ctx context.Context) error {
	pods, err := s.listPods(ctx)
	if err != nil {
		return err
	}
	streams := Select(pods, s.Options)
	if len(streams) == 0 && !s.Options.Follow {
		return fmt.Errorf("%w for %s %s/%s (component %s%s)", ErrNoPods, s.Options.layout().Kind, s.Namespace, s.Name, s.Options.Component, initNote(s.Options))
	}
	s.prefix = len(streams) > 1 || s.Options.Component == ComponentAll
	if s.Options.Prefix != nil {
		s.prefix = *s.Options.Prefix
	}

	if !s.Options.Follow {
		// One container after the other, so the output does not interleave.
		failed := 0
		for _, st := range streams {
			if err := s.stream(ctx, st, podByName(pods, st.Pod), nil); err != nil {
				s.note(st, err.Error())
				failed++
			}
		}
		if failed == len(streams) {
			return ErrNoneReadable
		}
		return nil
	}
	return s.follow(ctx, pods)
}

// follow streams every selected container concurrently. It polls for new pods (for example during a rollout) and
// for restarted containers, and resumes them from where their previous stream ended.
func (s *Streamer) follow(ctx context.Context, pods []corev1.Pod) error {
	interval := s.Options.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		active  = map[string]bool{}
		ended   = map[string]time.Time{}
		waiting = map[string]string{}
		waited  bool
	)
	start := func(st Stream, pod *corev1.Pod) {
		mu.Lock()
		if active[st.key()] {
			mu.Unlock()
			return
		}
		var since *metav1.Time
		if t, ok := ended[st.key()]; ok {
			since = &metav1.Time{Time: t}
		}
		active[st.key()] = true
		mu.Unlock()

		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.stream(ctx, st, pod, since); err != nil && ctx.Err() == nil {
				s.note(st, err.Error())
			}
			mu.Lock()
			delete(active, st.key())
			ended[st.key()] = time.Now()
			mu.Unlock()
		}()
	}

	for {
		streams := Select(pods, s.Options)
		if len(streams) == 0 && !waited {
			fmt.Fprintf(s.Notes, "waiting for pods of %s %s/%s (component %s)…\n", s.Options.layout().Kind, s.Namespace, s.Name, s.Options.Component)
			waited = true
		}
		if len(streams) > 1 {
			s.mu.Lock()
			s.prefix = s.Options.Prefix == nil || *s.Options.Prefix
			s.mu.Unlock()
		}
		for _, st := range streams {
			pod := podByName(pods, st.Pod)
			if reason, ok := notStarted(pod, st.Container); ok {
				if waiting[st.key()] != reason {
					s.note(st, "not started yet: "+reason)
					waiting[st.key()] = reason
				}
				continue
			}
			delete(waiting, st.key())
			start(st, pod)
		}

		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		case <-time.After(interval):
		}
		next, err := s.listPods(ctx)
		if err != nil {
			if ctx.Err() != nil {
				wg.Wait()
				return nil
			}
			return err
		}
		pods = next
	}
}

func (s *Streamer) stream(ctx context.Context, st Stream, pod *corev1.Pod, since *metav1.Time) error {
	if !s.Options.Previous {
		if reason, ok := notStarted(pod, st.Container); ok {
			return errors.New("not started yet: " + reason)
		}
	}
	opts := &corev1.PodLogOptions{
		Container: st.Container,
		Follow:    s.Options.Follow,
		Previous:  s.Options.Previous,
	}
	switch {
	case since != nil:
		opts.SinceTime = since
	case s.Options.Since > 0:
		seconds := int64(s.Options.Since.Seconds())
		opts.SinceSeconds = &seconds
	}
	if s.Options.Tail >= 0 && since == nil {
		tail := s.Options.Tail
		opts.TailLines = &tail
	}

	rc, err := s.Kube.CoreV1().Pods(s.Namespace).GetLogs(st.Pod, opts).Stream(ctx)
	if err != nil {
		return cleanError(err)
	}
	defer rc.Close()

	reader := bufio.NewReaderSize(rc, 64*1024)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			s.write(st, strings.TrimRight(line, "\r\n"))
		}
		if err == io.EOF || ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *Streamer) write(st Stream, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prefix {
		fmt.Fprintf(s.Out, "%s %s\n", s.paint(st, "["+st.Label()+"]"), line)
		return
	}
	fmt.Fprintln(s.Out, line)
}

func (s *Streamer) note(st Stream, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.Notes, "[%s] %s\n", st.Label(), msg)
}

var palette = []string{"\033[36m", "\033[35m", "\033[33m", "\033[32m", "\033[34m", "\033[31m"}

// paint gives each stream a stable color for its prefix. Callers hold s.mu.
func (s *Streamer) paint(st Stream, text string) string {
	if !s.Options.Color {
		return text
	}
	if s.colors == nil {
		s.colors = map[string]string{}
	}
	c, ok := s.colors[st.key()]
	if !ok {
		c = palette[len(s.colors)%len(palette)]
		s.colors[st.key()] = c
	}
	return c + text + "\033[0m"
}

func (s *Streamer) listPods(ctx context.Context) ([]corev1.Pod, error) {
	list, err := s.Kube.CoreV1().Pods(s.Namespace).List(ctx, metav1.ListOptions{LabelSelector: s.Options.layout().Selector(s.Name)})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// notStarted reports why a container has no log yet (for example PodInitializing or ImagePullBackOff).
func notStarted(pod *corev1.Pod, container string) (string, bool) {
	if pod == nil {
		return "pod is gone", true
	}
	statuses := append(append([]corev1.ContainerStatus(nil), pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
	for _, cs := range statuses {
		if cs.Name != container {
			continue
		}
		if cs.State.Running != nil || cs.State.Terminated != nil {
			return "", false
		}
		if w := cs.State.Waiting; w != nil {
			// A container waiting to restart still has the log of its last run.
			if cs.LastTerminationState.Terminated != nil && w.Reason == "CrashLoopBackOff" {
				return "", false
			}
			return w.Reason, true
		}
	}
	if pod.Status.Phase == corev1.PodPending {
		return "pod is " + strings.ToLower(string(pod.Status.Phase)), true
	}
	return "", false
}

func podByName(pods []corev1.Pod, name string) *corev1.Pod {
	for i := range pods {
		if pods[i].Name == name {
			return &pods[i]
		}
	}
	return nil
}

// cleanError shortens the API server's log errors, for example for --previous without a previous instance.
func cleanError(err error) error {
	msg := err.Error()
	if strings.Contains(msg, "previous terminated container") && strings.Contains(msg, "not found") {
		return errors.New("no previous instance (the container has not restarted)")
	}
	return err
}

func initNote(o Options) string {
	if o.Init {
		return ", init container " + isvc.ContainerStorageInitial
	}
	return ""
}
