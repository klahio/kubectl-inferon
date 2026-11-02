package cmd

import (
	"errors"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/klahio/kubectl-inferon/internal/inference"
)

func TestDiscoverErrorExitCodes(t *testing.T) {
	_, badURL := inference.Discover(t.Context(), inference.Target{}, inference.DiscoverOptions{URL: "not a url"})
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "services"}, "", errors.New("no list"))
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"bad --url", badURL, ExitUsage},
		{"listing Services forbidden", fmt.Errorf("port-forward: %w", forbidden), ExitClusterOrNF},
		{"port-forward upgrade refused", errors.New(`error upgrading connection: pods "p" is forbidden: User "u" cannot create resource "pods/portforward"`), ExitClusterOrNF},
		{"no ready pod", errors.New("no ready pod behind svc/x"), ExitFailure},
	}
	for _, tt := range tests {
		var exitErr *ExitError
		if !errors.As(discoverError(tt.err), &exitErr) || exitErr.Code != tt.want {
			t.Errorf("%s: got %+v, want exit code %d", tt.name, exitErr, tt.want)
		}
	}
}
