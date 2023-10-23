/*
Copyright (c) 2023 ApeCloud, Inc. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package provisioner

import (
	"context"
	"os"
	"strings"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	klog "k8s.io/klog/v2"
	pvController "sigs.k8s.io/sig-storage-lib-external-provisioner/v9/controller"

	"github.com/alibaba/open-local/pkg/provisioner"
	"github.com/alibaba/open-local/pkg/signals"
)

const (
	// TODO(x.zhou): change the domain
	defaultProvisionerName = "local.csi.aliyun.com/hostpath"
	// LeaderElectionKey represents ENV for disable/enable leaderElection for
	// localpv provisioner
	LeaderElectionKey = "LEADER_ELECTION_ENABLED"
)

var (
	opt = provisionerOptions{}
)

var Cmd = &cobra.Command{
	Use:   "provisioner",
	Short: "Dynamic Host Path PV Provisioner",
	Long: `Manage the Host Path PVs that includes: validating, creating,
		deleting and cleanup tasks. Host Path PVs are setup with
		node affinity`,
	Run: func(cmd *cobra.Command, args []string) {
		err := Start(cmd)
		if err != nil {
			klog.Fatalf("error :%s, quitting now\n", err.Error())
		}
	},
}

func init() {
	opt.addFlags(Cmd.Flags())
}

// Start will initialize and run the dynamic provisioner daemon
func Start(cmd *cobra.Command) error {
	klog.Infof("Starting Provisioner...")

	cfg, err := clientcmd.BuildConfigFromFlags(opt.Master, opt.Kubeconfig)
	if err != nil {
		klog.Fatalf("fail to build kubeconfig: %s", err.Error())
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return errors.Wrap(err, "unable to get k8s client")
	}

	//Create an instance of ProvisionerHandler to handle PV
	// create and delete events.
	provisioner, err := provisioner.NewProvisioner(kubeClient)
	if err != nil {
		return err
	}

	//Create an instance of the Dynamic Provisioner Controller
	// that has the reconciliation loops for PVC create and delete
	// events and invokes the Provisioner Handler.
	pc := pvController.NewProvisionController(
		kubeClient,
		opt.ProvisionerName,
		provisioner,
		pvController.LeaderElection(isLeaderElectionEnabled()),
	)

	// Set up signals so we handle the first shutdown signal gracefully
	stopCh := signals.SetupSignalHandler()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stopCh
		cancel()
	}()

	klog.V(4).Info("Provisioner started")
	//Run the provisioner till a shutdown signal is received.
	pc.Run(ctx)
	klog.V(4).Info("Provisioner stopped")

	return nil
}

// isLeaderElectionEnabled returns true/false based on the ENV
// LEADER_ELECTION_ENABLED set via provisioner deployment.
// Defaults to true, means leaderElection enabled by default.
func isLeaderElectionEnabled() bool {
	leaderElection := os.Getenv(LeaderElectionKey)

	var leader bool
	switch strings.ToLower(leaderElection) {
	default:
		klog.Info("Leader election enabled for localpv-provisioner")
		leader = true
	case "y", "yes", "true":
		klog.Info("Leader election enabled for localpv-provisioner via leaderElectionKey")
		leader = true
	case "n", "no", "false":
		klog.Info("Leader election disabled for localpv-provisioner via leaderElectionKey")
		leader = false
	}
	return leader
}
