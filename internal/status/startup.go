package status

import (
	"bytes"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/duration"
)

// sandboxRestartGap tells a pod whose sandbox was recreated (after a node restart, for example) from a pod that just
// took a moment to get its network: the first sandbox is ready seconds after scheduling.
const sandboxRestartGap = time.Minute

// readySkew is how much earlier than the container's start the ready time may be stamped and still count as clock skew.
const readySkew = 2 * time.Second

// Startup splits the start of one pod into phases, to answer "why does my model take so long to come up":
// scheduling, pulling and starting the download container, the model download, pulling and starting the server, and
// loading the model until the server reports ready (docs/mvp.md section 4).
type Startup struct {
	Pod string `json:"pod"`
	// Restarts of the server container; the phases then describe its current instance.
	Restarts int32 `json:"restarts,omitempty"`
	// SandboxRestarted is set when the pod's sandbox was recreated long after scheduling, so the phases describe
	// that run and not the pod's creation.
	SandboxRestarted *time.Time `json:"sandboxRestarted,omitempty"`
	// Failed is set when the last phase ended in a failure; the start never completed.
	Failed bool    `json:"failed,omitempty"`
	Phases []Phase `json:"phases"`
}

// Phase is one step of a pod's start. End is nil while the step is still running, or when its end is not
// recorded (Untimed).
type Phase struct {
	Name    string     `json:"name"`
	Start   time.Time  `json:"start"`
	End     *time.Time `json:"end,omitempty"`
	Note    string     `json:"note,omitempty"`
	Untimed bool       `json:"untimed,omitempty"`
}

// restarted reports whether any container of the pod has restarted.
func restarted(p *corev1.Pod) bool {
	for _, cs := range append(append([]corev1.ContainerStatus(nil), p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		if cs.RestartCount > 0 {
			return true
		}
	}
	return false
}

// Phase names.
const (
	PhaseScheduling = "scheduling"
	PhasePodStart   = "pod start"
	PhaseDownload   = "model download"
	PhaseServer     = "server start"
	PhaseLoad       = "model load"
)

const notePull = "image pull and container start"

// startupOf reads the phases from the pod's conditions and container states. initName is the model download init
// container, server the model server container.
func startupOf(p *corev1.Pod, initName, server string) *Startup {
	st := &Startup{Pod: p.Name}
	add := func(name string, start time.Time, end *time.Time, note string) bool {
		st.Phases = append(st.Phases, Phase{Name: name, Start: start, End: end, Note: note})
		return end != nil
	}
	created := p.CreationTimestamp.Time
	scheduled, isScheduled := podCondition(p, corev1.PodScheduled)
	if !isScheduled {
		add(PhaseScheduling, created, nil, "")
		return st
	}

	begin := scheduled
	// A late sandbox alone could be a slow network setup on the first run; a restarted sandbox also restarts the
	// containers, so a restart count is required too.
	if sandbox, ok := podCondition(p, corev1.PodReadyToStartContainers); ok && sandbox.Sub(scheduled) > sandboxRestartGap && restarted(p) {
		st.SandboxRestarted = &sandbox
		begin = sandbox
	} else {
		add(PhaseScheduling, created, &scheduled, "")
	}

	prev := begin
	if cs, ok := containerStatus(p.Status.InitContainerStatuses, initName); ok {
		start, end, running := containerTimes(cs)
		if !running && end == nil {
			if cs.State.Waiting == nil && cs.LastTerminationState.Terminated == nil {
				add(PhasePodStart, begin, nil, notePull)
			}
			return st
		}
		if t := failedTermination(cs); t != nil && !running {
			// A failing download is retried with back-off; only the last attempt has a meaningful duration.
			add(PhaseDownload, start, end, fmt.Sprintf("failed with exit code %d, restarts: %d", t.ExitCode, cs.RestartCount))
			st.Failed = true
			return st
		}
		if last := cs.LastTerminationState.Terminated; last != nil && last.ExitCode != 0 {
			// A retry after failed attempts: the time before it is back-off, not image pulling. Restarts after a
			// successful run (a node restart re-runs init containers) are not retries.
			if !add(PhaseDownload, start, end, fmt.Sprintf("attempt %d", cs.RestartCount+1)) {
				return st
			}
			prev = *end
			goto server
		}
		add(PhasePodStart, begin, &start, notePull)
		if !add(PhaseDownload, start, end, "") {
			return st
		}
		prev = *end
	}

server:
	cs, ok := containerStatus(p.Status.ContainerStatuses, server)
	if !ok && len(p.Spec.Containers) > 0 {
		// A custom predictor may name its container differently; its first container is the server, as for logs.
		cs, ok = containerStatus(p.Status.ContainerStatuses, p.Spec.Containers[0].Name)
	}
	if !ok {
		return st
	}
	start, end, running := containerTimes(cs)
	if t := failedTermination(cs); t != nil && !running {
		st.Restarts = cs.RestartCount
		add(PhaseLoad, start, end, fmt.Sprintf("crashed with exit code %d, restarts: %d", t.ExitCode, cs.RestartCount))
		st.Failed = true
		return st
	}
	if !running {
		if cs.RestartCount == 0 && cs.State.Waiting != nil {
			add(PhaseServer, prev, nil, notePull)
		}
		return st
	}
	note := "until ready"
	if cs.RestartCount > 0 {
		// The gap before the current instance includes crash back-off, so "server start" would mislead.
		st.Restarts = cs.RestartCount
		note = fmt.Sprintf("current instance, after %d restarts", cs.RestartCount)
	} else {
		add(PhaseServer, prev, &start, notePull)
	}
	if cs.Ready {
		// Kubernetes records when all containers became ready, not each one. When another container (a service
		// mesh proxy, say) is not ready yet, the server's own ready time is unknown.
		c, ok := podCondition(p, corev1.ContainersReady)
		// The runtime and the kubelet stamp times separately; a few seconds of disagreement is clock skew.
		if ok && c.Before(start) && !c.Before(start.Add(-readySkew)) {
			c = start
		}
		if !ok || c.Before(start) {
			st.Phases = append(st.Phases, Phase{Name: PhaseLoad, Start: start, Note: "ready; the time is not recorded while other containers are not ready", Untimed: true})
			return st
		}
		add(PhaseLoad, start, &c, note)
		return st
	}
	add(PhaseLoad, start, nil, note)
	return st
}

func podCondition(p *corev1.Pod, t corev1.PodConditionType) (time.Time, bool) {
	for _, c := range p.Status.Conditions {
		if c.Type == t && c.Status == corev1.ConditionTrue && !c.LastTransitionTime.IsZero() {
			return c.LastTransitionTime.Time, true
		}
	}
	return time.Time{}, false
}

func containerStatus(list []corev1.ContainerStatus, name string) (corev1.ContainerStatus, bool) {
	for _, cs := range list {
		if cs.Name == name {
			return cs, true
		}
	}
	return corev1.ContainerStatus{}, false
}

// containerTimes returns when the current instance started, when it finished (nil while running) and whether it
// runs. A container waiting to be restarted reports its last instance.
func containerTimes(cs corev1.ContainerStatus) (start time.Time, end *time.Time, running bool) {
	switch {
	case cs.State.Running != nil:
		return cs.State.Running.StartedAt.Time, nil, true
	case cs.State.Terminated != nil:
		return terminatedTimes(cs.State.Terminated)
	case cs.State.Waiting != nil && cs.LastTerminationState.Terminated != nil:
		return terminatedTimes(cs.LastTerminationState.Terminated)
	}
	return time.Time{}, nil, false
}

// terminatedTimes leaves the end out when the runtime did not record it; a zero time is "unknown", not year 1.
func terminatedTimes(t *corev1.ContainerStateTerminated) (time.Time, *time.Time, bool) {
	if t.FinishedAt.IsZero() || t.StartedAt.IsZero() {
		return t.StartedAt.Time, nil, false
	}
	end := t.FinishedAt.Time
	return t.StartedAt.Time, &end, false
}

// newestPod picks the pod whose start is most worth explaining: the newest one that is not being deleted.
func newestPod(pods []corev1.Pod, keep func(*corev1.Pod) bool) *corev1.Pod {
	var mine []corev1.Pod
	for _, p := range pods {
		if keep(&p) {
			mine = append(mine, p)
		}
	}
	var best *corev1.Pod
	live := livePods(mine)
	for i := range live {
		p := &live[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if best == nil || p.CreationTimestamp.After(best.CreationTimestamp.Time) {
			best = p
		}
	}
	return best
}

// renderStartup writes the Startup section; now dates the phases that are still running.
func (r TextRenderer) renderStartup(p *printer, s *Startup, now time.Time) {
	if s == nil || len(s.Phases) == 0 {
		return
	}
	p.section("Startup   " + r.paint(colorDim, "pod/"+s.Pod))
	if s.SandboxRestarted != nil {
		p.line("  the pod's sandbox was recreated %s ago (for example by a node restart); times are for that run", duration.HumanDuration(now.Sub(*s.SandboxRestarted)))
	}
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 3, ' ', 0)
	first := s.Phases[0].Start
	last := s.Phases[len(s.Phases)-1]
	for _, ph := range s.Phases {
		took := "so far " + duration.HumanDuration(now.Sub(ph.Start))
		switch {
		case ph.Untimed:
			took = "-"
		case ph.End != nil:
			took = humanSpan(ph.End.Sub(ph.Start))
		}
		// Every row has the note cell, even empty, so tabwriter aligns the column across all rows.
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", ph.Name, took, r.paint(colorDim, ph.Note))
	}
	if len(s.Phases) > 1 {
		if last.End != nil && s.Restarts == 0 && !s.Failed {
			fmt.Fprintf(tw, "  total\t%s\t\n", humanSpan(last.End.Sub(first)))
		} else if last.End == nil && !last.Untimed && s.Restarts == 0 && !s.Failed {
			fmt.Fprintf(tw, "  total\tso far %s\t\n", duration.HumanDuration(now.Sub(first)))
		}
	}
	tw.Flush()
	// An empty note cell leaves padding behind; trim it.
	for _, l := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		p.line("%s", strings.TrimRight(l, " "))
	}
}

// humanSpan prints a duration like kubectl does, with "0s" for the sub-second steps that timestamps cannot resolve.
func humanSpan(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}
	return duration.HumanDuration(d)
}
