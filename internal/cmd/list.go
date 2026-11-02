package cmd

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/klahio/kubectl-inferon/internal/status"
)

type listOptions struct {
	*rootOptions
	allNamespaces bool
	output        string
}

var listOutputs = []string{"name", "json", "yaml"}

func newListCommand(root *rootOptions) *cobra.Command {
	o := &listOptions{rootOptions: root}
	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List InferenceServices and LLMInferenceServices as TYPE/NAME",
		Long: "List InferenceServices and LLMInferenceServices with their readiness and the first problem found in their pods.\n" +
			"The NAME column is the argument for status, test and logs.",
		Example: fmt.Sprintf(`  %[1]s%[2]s list -n models
  %[1]s%[2]s list -A
  %[1]s%[2]s list -n models -o name     # one TYPE/NAME per line, for scripts`, commandPrefix(), PluginName),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.run(cmd.Context())
		},
	}
	c.Flags().BoolVarP(&o.allNamespaces, "all-namespaces", "A", false, "List in every namespace")
	c.Flags().StringVarP(&o.output, "output", "o", "", "Output format: name, json or yaml. Default is a table")
	return c
}

func (o *listOptions) run(ctx context.Context) error {
	if o.output != "" && !slices.Contains(listOutputs, o.output) {
		return usage("unknown output format %q (use %s)", o.output, strings.Join(listOutputs, ", "))
	}
	if ctx == nil {
		ctx = context.Background()
	}
	clients, namespace, err := o.clients()
	if err != nil {
		return err
	}
	if o.allNamespaces {
		namespace = ""
	}
	l, err := status.List(ctx, clients, namespace)
	if err != nil {
		return clusterError(err)
	}

	switch o.output {
	case "name":
		for _, it := range l.Items {
			fmt.Fprintln(o.streams.Out, it.Ref())
		}
	case "json", "yaml":
		data, err := marshal(o.output, l)
		if err != nil {
			return err
		}
		if _, err := o.streams.Out.Write(data); err != nil {
			return err
		}
	default:
		if len(l.Items) == 0 {
			where := "in namespace " + namespace
			if o.allNamespaces {
				where = "in any namespace"
			}
			fmt.Fprintf(o.streams.ErrOut, "No inference services found %s.\n", where)
		} else {
			r := status.TextRenderer{Color: o.useColor(o.streams.Out)}
			if err := r.RenderList(o.streams.Out, l, o.allNamespaces); err != nil {
				return err
			}
		}
	}

	parts := make([]string, 0, len(l.Unknown))
	for part := range l.Unknown {
		parts = append(parts, part)
	}
	sort.Strings(parts)
	for _, part := range parts {
		fmt.Fprintf(o.streams.ErrOut, "not checked: %s: %s\n", part, l.Unknown[part])
	}
	return nil
}
