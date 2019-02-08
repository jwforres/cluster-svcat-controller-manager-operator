package operator

import (
	"github.com/spf13/cobra"

	"github.com/openshift/cluster-svcat-apiserver-operator/pkg/operator"
	"github.com/openshift/cluster-svcat-apiserver-operator/pkg/version"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
)

func NewOperator() *cobra.Command {
	cmd := controllercmd.
		NewControllerCommandConfig("openshift-cluster-svcat-apiserver-operator", version.Get(), operator.RunOperator).
		NewCommand()
	cmd.Use = "operator"
	cmd.Short = "Start the Cluster svcat-apiserver Operator"

	return cmd
}
