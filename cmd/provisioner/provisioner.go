package provisioner

import (
	"context"
	"log"

	"github.com/alibaba/open-local/pkg/provisioner"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/sig-storage-lib-external-provisioner/v9/controller"
)

var (
	opt = provisionerOption{}
)

const (
	provisionerName = "kubeblocks.io/hostpath"
)

var Cmd = &cobra.Command{
	Use:   "host path provisioner",
	Short: "command for provisioning local pv",
	Run: func(cmd *cobra.Command, args []string) {
		err := Start(&opt)
		if err != nil {
			log.Fatalf("error :%s, quitting now\n", err.Error())
		}
	},
}

func init() {
	opt.addFlags(Cmd.Flags())
}

// Start will start agent
func Start(opt *provisionerOption) error {
	// TODO(@cnut): support exit on receiving a signal

	// Create an InClusterConfig and use it to create a client for the controller
	// to use to communicate with Kubernetes
	config, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("Failed to create config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to create client: %v", err)
	}

	// Create the provisioner: it implements the Provisioner interface expected by
	// the controller
	hostPathProvisioner := provisioner.NewHostPathProvisioner(opt.BasePath)

	// Start the provision controller which will dynamically provision hostPath
	// PVs
	pc := controller.NewProvisionController(clientset, provisionerName, hostPathProvisioner)

	// Never stops.
	pc.Run(context.Background())
	return nil
}
