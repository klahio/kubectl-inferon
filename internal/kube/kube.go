// Package kube holds the Kubernetes clients shared by all commands and the lookups they have in common.
package kube

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/klahio/kubectl-inferon/internal/isvc"
	"github.com/klahio/kubectl-inferon/internal/llmisvc"
)

// Clients are the Kubernetes clients the commands read from.
type Clients struct {
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
	// Config is needed for port-forward, which talks to the API server outside the typed clients.
	Config *rest.Config
}

// ErrNotFound means the requested resource does not exist.
var ErrNotFound = errors.New("not found")

// GetInferenceService reads one InferenceService; a missing one is reported as ErrNotFound.
func (c Clients) GetInferenceService(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	u, err := c.Dynamic.Resource(isvc.InferenceServiceGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("InferenceService %s/%s %w", namespace, name, ErrNotFound)
	}
	return u, err
}

// ErrNotInstalled means the cluster does not serve a resource at all (its CustomResourceDefinition is missing).
var ErrNotInstalled = errors.New("not installed in this cluster")

// ServedVersion returns the first of the preferred API versions under which the API server serves the resource.
func (c Clients) ServedVersion(group, resource string, preferred []string) (string, error) {
	for _, v := range preferred {
		list, err := c.Kube.Discovery().ServerResourcesForGroupVersion(group + "/" + v)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return "", err
		}
		for _, r := range list.APIResources {
			if r.Name == resource {
				return v, nil
			}
		}
	}
	return "", fmt.Errorf("%s.%s is %w", resource, group, ErrNotInstalled)
}

// GetLLMInferenceService reads one LLMInferenceService through the newest API version the cluster serves.
func (c Clients) GetLLMInferenceService(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	v, err := c.ServedVersion(llmisvc.Group, llmisvc.Resource, llmisvc.PreferredVersions)
	if err != nil {
		return nil, err
	}
	u, err := c.Dynamic.Resource(llmisvc.GVR(v)).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("LLMInferenceService %s/%s %w", namespace, name, ErrNotFound)
	}
	return u, err
}
