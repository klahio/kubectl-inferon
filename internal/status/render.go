package status

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"k8s.io/apimachinery/pkg/util/duration"

	"github.com/klahio/kubectl-inferon/internal/isvc"
)

// Text colors, used only when the output is a terminal (docs/mvp.md section 3).
const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBold   = "\033[1m"
	colorDim    = "\033[2m"
)

// TextRenderer prints a summary for people.
type TextRenderer struct {
	Color bool
	// Now is injected so tests get a stable "age"; zero means time.Now().
	Now time.Time
}

func (r TextRenderer) paint(color, s string) string {
	if !r.Color || s == "" {
		return s
	}
	return color + s + colorReset
}

func (r TextRenderer) state(s string) string {
	switch s {
	case "True":
		return r.paint(colorGreen, "Ready")
	case "False":
		return r.paint(colorRed, "Not Ready")
	default:
		return r.paint(colorYellow, "Unknown")
	}
}

func (r TextRenderer) conditionStatus(s string) string {
	switch s {
	case "True":
		return r.paint(colorGreen, s)
	case "False":
		return r.paint(colorRed, s)
	default:
		return r.paint(colorYellow, s)
	}
}

// Render writes the summary.
func (r TextRenderer) Render(w io.Writer, s *Summary) error {
	now := r.Now
	if now.IsZero() {
		now = time.Now()
	}
	p := &printer{w: w}

	age := "-"
	if !s.Created.IsZero() {
		age = duration.HumanDuration(now.Sub(s.Created))
	}
	p.line("%s   namespace: %s   mode: %s   %s   age: %s",
		r.paint(colorBold, s.Name), s.Namespace, dash(s.Mode), r.state(s.Ready), age)
	for _, warn := range s.Warnings {
		p.line("%s", r.paint(colorYellow, "⚠ "+warn))
	}

	p.section("Endpoints")
	tw := p.table()
	external := dash(s.Endpoints.External)
	if s.Endpoints.External != "" && !s.Endpoints.Routable {
		external += "   " + r.paint(colorYellow, "⚠ not routable")
	}
	fmt.Fprintf(tw, "  external\t%s\n", external)
	fmt.Fprintf(tw, "  in-cluster\t%s\n", dash(s.Endpoints.InCluster))
	for _, e := range s.Endpoints.Exposure {
		line := fmt.Sprintf("  %s\t%s   via %s   address: %s", strings.ToLower(e.Kind), e.Name, dash(e.Via), dash(e.Address))
		if e.Leftover {
			line += "   " + r.paint(colorYellow, "leftover")
		} else if e.Broken {
			line += "   " + r.paint(colorYellow, "not routable")
		}
		fmt.Fprintln(tw, line)
	}
	tw.Flush()

	p.section("Model")
	tw = p.table()
	if s.Model.CustomContainer {
		fmt.Fprintf(tw, "  format\tcustom container\n")
	} else {
		fmt.Fprintf(tw, "  format\t%s\n", dash(s.Model.Format))
		fmt.Fprintf(tw, "  storage\t%s\n", dash(s.Model.StorageURI))
	}
	if s.Runtime != nil {
		rt := fmt.Sprintf("%s (%s)", s.Runtime.Name, s.Runtime.Kind)
		if len(s.Runtime.ProtocolVersions) > 0 {
			rt += "   protocols: " + strings.Join(s.Runtime.ProtocolVersions, ", ")
		}
		if !s.Runtime.Found {
			rt += "   " + r.paint(colorRed, "not found")
		}
		fmt.Fprintf(tw, "  runtime\t%s\n", rt)
	} else if !s.Model.CustomContainer {
		fmt.Fprintf(tw, "  runtime\t%s\n", r.paint(colorYellow, "none selected"))
	}
	if s.Model.ProtocolVersion != "" {
		fmt.Fprintf(tw, "  protocol\t%s\n", s.Model.ProtocolVersion)
	}
	state := dash(s.ModelStatus.ActiveState)
	if s.ModelStatus.TargetState != "" && s.ModelStatus.TargetState != s.ModelStatus.ActiveState {
		state = fmt.Sprintf("%s → %s", dash(s.ModelStatus.ActiveState), s.ModelStatus.TargetState)
	}
	if s.ModelStatus.TransitionStatus != "" {
		state += " (" + s.ModelStatus.TransitionStatus + ")"
	}
	fmt.Fprintf(tw, "  state\t%s\n", state)
	tw.Flush()

	p.section("Components")
	tw = p.table()
	for _, c := range s.Components {
		line := fmt.Sprintf("  %s\t%s\treplicas %d/%d", c.Name, r.state(c.Ready), c.ReadyReplicas, c.Replicas)
		if a := c.Autoscaling; a != nil {
			line += fmt.Sprintf("\tautoscaling %d–%d", a.Min, a.Max)
			if a.Target != "" {
				line += " (" + a.Target + ")"
			}
		}
		fmt.Fprintln(tw, line)
	}
	tw.Flush()

	if s.Canary != nil {
		p.section("Canary")
		tw = p.table()
		fmt.Fprintf(tw, "  stable\t%d%%\n", s.Canary.StablePercent)
		for _, c := range s.Canary.Canaries {
			fmt.Fprintf(tw, "  %s\t%d%%\t%s\treplicas %d/%d\n", c.Name, c.TrafficPercent, r.state(c.Ready), c.ReadyReplicas, c.Replicas)
		}
		tw.Flush()
	}
	r.renderGPU(p, s.GPU)
	r.renderStartup(p, s.Startup, now)

	r.renderTail(p, s.Conditions, s.Findings, s.Unknown)
	return p.err
}

// renderTail writes the sections every resource kind shares: conditions, diagnosis and the parts not checked.
func (r TextRenderer) renderTail(p *printer, conditions []isvc.Condition, findings []Finding, unknown map[string]string) {
	p.section("Conditions")
	tw := p.table()
	for _, c := range conditions {
		row := fmt.Sprintf("  %s\t%s", c.Type, r.conditionStatus(c.Status))
		if reason := joinReason(c.Reason, c.Message); reason != "" {
			row += "\t" + reason
		}
		fmt.Fprintln(tw, row)
	}
	if len(conditions) == 0 {
		fmt.Fprintln(tw, "  (none reported yet)")
	}
	tw.Flush()

	if len(findings) > 0 {
		p.section("Diagnosis")
		for _, f := range findings {
			mark := r.paint(colorRed, "✗")
			switch f.Severity {
			case SeverityWarning:
				mark = r.paint(colorYellow, "⚠")
			case SeverityInfo:
				mark = r.paint(colorDim, "…")
			}
			title := r.paint(colorBold, f.Title)
			if f.Object != "" {
				title += "   " + r.paint(colorDim, f.Object)
			}
			p.line("  %s %s", mark, title)
			for _, d := range f.Details {
				p.line("      %s", d)
			}
			if len(f.LogLines) > 0 {
				label := "last log lines of %s:"
				if f.Logs.Scan {
					label = "from the log of %s:"
				}
				p.line("      "+label, f.Logs.Container)
				for _, l := range f.LogLines {
					p.line("        %s", r.paint(colorDim, l))
				}
			}
			if f.Hint != "" {
				p.line("      hint: %s", f.Hint)
			}
		}
	}

	if len(unknown) > 0 {
		p.section("Not checked")
		parts := make([]string, 0, len(unknown))
		for part := range unknown {
			parts = append(parts, part)
		}
		sort.Strings(parts)
		for _, part := range parts {
			p.line("  %s: %s", part, unknown[part])
		}
	}
}

// RenderLLM writes the summary of an LLMInferenceService (docs/mvp.md section 6a.2).
func (r TextRenderer) RenderLLM(w io.Writer, s *LLMSummary) error {
	now := r.Now
	if now.IsZero() {
		now = time.Now()
	}
	p := &printer{w: w}
	age := "-"
	if !s.Created.IsZero() {
		age = duration.HumanDuration(now.Sub(s.Created))
	}
	p.line("%s   llminferenceservice   namespace: %s   %s   age: %s   api: %s",
		r.paint(colorBold, s.Name), s.Namespace, r.state(s.Ready), age, dash(s.APIVersion))

	p.section("Endpoints")
	tw := p.table()
	external := dash(s.Endpoints.External)
	if s.Endpoints.External == "" {
		external = "- (no router)"
	} else if !s.Endpoints.Routable {
		external += "   " + r.paint(colorYellow, "⚠ not routable")
	}
	fmt.Fprintf(tw, "  external\t%s\n", external)
	fmt.Fprintf(tw, "  in-cluster\t%s\n", dash(s.Endpoints.InCluster))
	for _, e := range s.Endpoints.Exposure {
		line := fmt.Sprintf("  %s\t%s   via %s   address: %s", strings.ToLower(e.Kind), e.Name, dash(e.Via), dash(e.Address))
		if e.Broken {
			line += "   " + r.paint(colorYellow, "not routable")
		}
		fmt.Fprintln(tw, line)
	}
	tw.Flush()

	p.section("Model")
	tw = p.table()
	fmt.Fprintf(tw, "  name\t%s\n", dash(s.Model.Name))
	fmt.Fprintf(tw, "  uri\t%s\n", dash(s.Model.URI))
	if len(s.Model.LoRA) > 0 {
		fmt.Fprintf(tw, "  lora\t%s\n", strings.Join(s.Model.LoRA, ", "))
	}
	fmt.Fprintf(tw, "  image\t%s\n", dash(s.Model.Image))
	if par := formatParallelism(s.Model.Parallelism); par != "" {
		fmt.Fprintf(tw, "  parallelism\t%s\n", par)
	}
	if len(s.Model.Presets) > 0 {
		fmt.Fprintf(tw, "  presets\t%s\n", strings.Join(s.Model.Presets, ", "))
	}
	tw.Flush()

	p.section("Workloads")
	tw = p.table()
	for _, wl := range s.Workloads {
		replicas := fmt.Sprintf("replicas %d/%d", wl.ReadyReplicas, wl.Replicas)
		if wl.Replicas < 0 {
			replicas = fmt.Sprintf("ready replicas %d", wl.ReadyReplicas)
		}
		fmt.Fprintf(tw, "  %s\t%s/%s\t%s\n", wl.Role, strings.ToLower(wl.Kind), wl.Name, replicas)
	}
	if len(s.Workloads) == 0 {
		fmt.Fprintln(tw, "  (none created yet)")
	}
	tw.Flush()
	r.renderGPU(p, s.GPU)
	r.renderStartup(p, s.Startup, now)

	r.renderTail(p, s.Conditions, s.Findings, s.Unknown)
	return p.err
}

// RenderEvents writes the --events section. Noise events are dropped.
func (r TextRenderer) RenderEvents(w io.Writer, events []EventLine) error {
	p := &printer{w: w}
	p.section("Events")
	if len(events) == 0 {
		p.line("  (no events)")
		return p.err
	}
	tw := p.table()
	for _, e := range events {
		t := e.Type
		if t == "Warning" {
			t = r.paint(colorYellow, t)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", e.Age, t, e.Reason, e.Object, e.Message)
	}
	tw.Flush()
	return p.err
}

// EventLine is one event ready for display.
type EventLine struct {
	Age     string `json:"age"`
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Object  string `json:"object"`
	Message string `json:"message"`
}

type printer struct {
	w   io.Writer
	err error
}

func (p *printer) line(format string, args ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format+"\n", args...)
	}
}

func (p *printer) section(title string) {
	p.line("")
	p.line("%s", title)
}

func (p *printer) table() *tabwriter.Writer {
	return tabwriter.NewWriter(p.w, 0, 0, 3, ' ', 0)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
