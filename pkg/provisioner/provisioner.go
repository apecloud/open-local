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
	"fmt"
	"strings"

	errors "github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	pvController "sigs.k8s.io/sig-storage-lib-external-provisioner/v9/controller"
)

// NewProvisioner will create a new Provisioner object and initialize
//
//	it with global information used across PV create and delete operations.
func NewProvisioner(kubeClient *clientset.Clientset) (*Provisioner, error) {

	namespace := getNamespace()
	if len(strings.TrimSpace(namespace)) == 0 {
		return nil, fmt.Errorf("Cannot start Provisioner: failed to get namespace")
	}

	p := &Provisioner{
		kubeClient:  kubeClient,
		namespace:   namespace,
		helperImage: getDefaultHelperImage(),
		defaultConfig: []Config{
			{
				Name:  KeyPVBasePath,
				Value: getDefaultBasePath(),
			},
		},
	}
	p.getVolumeConfig = p.GetVolumeConfig

	return p, nil
}

// SupportsBlock will be used by controller to determine if block mode is
//
//	supported by the host path provisioner.
func (p *Provisioner) SupportsBlock(_ context.Context) bool {
	return true
}

// Provision is invoked by the PVC controller which expect the PV
//
//	to be provisioned and a valid PV spec returned.
func (p *Provisioner) Provision(ctx context.Context, opts pvController.ProvisionOptions) (*corev1.PersistentVolume, pvController.ProvisioningState, error) {
	pvc := opts.PVC

	// validate pvc dataSource
	if err := validateVolumeSource(*pvc); err != nil {
		return nil, pvController.ProvisioningFinished, err
	}

	if pvc.Spec.Selector != nil {
		return nil, pvController.ProvisioningFinished, fmt.Errorf("claim.Spec.Selector is not supported")
	}
	for _, accessMode := range pvc.Spec.AccessModes {
		if accessMode != corev1.ReadWriteOnce {
			return nil, pvController.ProvisioningFinished, fmt.Errorf("Only support ReadWriteOnce access mode")
		}
	}

	if opts.SelectedNode == nil {
		return nil, pvController.ProvisioningReschedule, fmt.Errorf("configuration error, no node was specified")
	}

	if GetNodeHostname(opts.SelectedNode) == "" {
		return nil, pvController.ProvisioningFinished, fmt.Errorf("configuration error, node{%v} hostname is empty", opts.SelectedNode.Name)
	}

	name := opts.PVName

	// Create a new Config instance for the PV by merging the
	// default configuration with configuration provided
	// via PVC and the associated StorageClass
	pvCASConfig, err := p.getVolumeConfig(ctx, name, pvc)
	if err != nil {
		return nil, pvController.ProvisioningFinished, err
	}

	return p.ProvisionHostPath(ctx, opts, pvCASConfig)
}

// Delete is invoked by the PVC controller to perform clean-up
//
//	activities before deleteing the PV object. If reclaim policy is
//	set to not-retain, then this function will create a helper pod
//	to delete the host path from the node.
func (p *Provisioner) Delete(ctx context.Context, pv *corev1.PersistentVolume) (err error) {
	defer func() {
		err = errors.Wrapf(err, "failed to delete volume %v", pv.Name)
	}()
	//Initiate clean up only when reclaim policy is not retain.
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		err = p.DeleteHostPath(ctx, pv)
		if err != nil {
			klog.Errorf("Failed to delete Local PV %v, err: %s", pv.Name, err)
		}
		return err
	}
	klog.Infof("Retained volume %v", pv.Name)
	return nil
}

// validateVolumeSource validates datasource field of the pvc.
// - clone - not handled by this provisioner
// - snapshot - not handled by this provisioner
// - volume populator - not handled by this provisioner
func validateVolumeSource(pvc corev1.PersistentVolumeClaim) error {
	if pvc.Spec.DataSource != nil {
		// PVC.Spec.DataSource.Name is the name of the VolumeSnapshot or PVC or populator
		if pvc.Spec.DataSource.Name == "" {
			return fmt.Errorf("dataSource name not found for PVC `%s`", pvc.Name)
		}
		switch pvc.Spec.DataSource.Kind {

		// DataSource is snapshot
		case SnapshotKind:
			if *(pvc.Spec.DataSource.APIGroup) != SnapshotAPIGroup {
				return fmt.Errorf("snapshot feature not supported by this provisioner")
			}
			return fmt.Errorf("datasource `%s` of group `%s` is not handled by the provisioner",
				pvc.Spec.DataSource.Kind, *pvc.Spec.DataSource.APIGroup)

		// DataSource is pvc
		case PVCKind:
			return fmt.Errorf("clone feature not supported by this provisioner")

		// Custom DataSource (volume populator)
		default:
			return fmt.Errorf("datasource `%s` of group `%s` is not handled by the provisioner",
				pvc.Spec.DataSource.Kind, *pvc.Spec.DataSource.APIGroup)
		}
	}
	return nil
}
