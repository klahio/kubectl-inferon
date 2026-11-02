package main

import (
	"os"

	// Registers client-go authentication plugins (GKE, AKS, OIDC, ...), as the krew best practices ask.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/klahio/kubectl-inferon/internal/cmd"
)

func main() {
	os.Exit(cmd.Execute())
}
