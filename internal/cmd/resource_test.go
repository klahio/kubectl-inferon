package cmd

import (
	"errors"
	"strings"
	"testing"
)

func TestResourceArg(t *testing.T) {
	tests := []struct {
		arg      string
		kind     Kind
		name     string
		errorHas string
	}{
		{arg: "isvc/sklearn-iris", kind: KindInferenceService, name: "sklearn-iris"},
		{arg: "InferenceService/sklearn-iris", kind: KindInferenceService, name: "sklearn-iris"},
		{arg: "inferenceservices.serving.kserve.io/sklearn-iris", kind: KindInferenceService, name: "sklearn-iris"},
		{arg: "llmisvc/smollm", kind: KindLLMInferenceService, name: "smollm"},
		{arg: "LLMInferenceService/smollm", kind: KindLLMInferenceService, name: "smollm"},
		{arg: "llminferenceservices.serving.kserve.io/smollm", kind: KindLLMInferenceService, name: "smollm"},
		{arg: "ig/graph", errorHas: "InferenceGraph is not supported yet"},
		{arg: "deploy/x", errorHas: `unknown resource type "deploy"`},
		{arg: "sklearn-iris", errorHas: `"sklearn-iris" has no resource type; use TYPE/NAME, for example isvc/sklearn-iris`},
		{arg: "isvc/", errorHas: "is not a resource name"},
		{arg: "/sklearn-iris", errorHas: "is not a resource name"},
		{arg: "llmisvc/a/b", errorHas: "is not a resource name"},
	}
	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			kind, name, err := resourceArg(tt.arg)
			if tt.errorHas == "" {
				if err != nil || kind != tt.kind || name != tt.name {
					t.Fatalf("got %s %q, %v; want %s %q", kind, name, err, tt.kind, tt.name)
				}
				return
			}
			var exitErr *ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != ExitUsage || !strings.Contains(err.Error(), tt.errorHas) {
				t.Fatalf("err = %v, want a usage error mentioning %q", err, tt.errorHas)
			}
		})
	}
}
