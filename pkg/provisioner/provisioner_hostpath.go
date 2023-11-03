/*
Copyright 2019 The OpenEBS Authors.
Portions Copyright (c) 2023 ApeCloud, Inc. All rights reserved.

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

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	klog "k8s.io/klog/v2"
	pvController "sigs.k8s.io/sig-storage-lib-external-provisioner/v9/controller"
)

const (
	EnableXfsQuota  string = "enableXfsQuota"
	EnableExt4Quota string = "enableExt4Quota"
	SoftLimitGrace  string = "softLimitGrace"
	HardLimitGrace  string = "hardLimitGrace"
)

// ProvisionHostPath is invoked by the Provisioner which expect HostPath PV
//
//	to be provisioned and a valid PV spec returned.
func (p *Provisioner) ProvisionHostPath(ctx context.Context, opts pvController.ProvisionOptions, volumeConfig *VolumeConfig) (*corev1.PersistentVolume, pvController.ProvisioningState, error) {
	pvc := opts.PVC
	taints := GetTaints(opts.SelectedNode)
	name := opts.PVName
	saName := getServiceAccountName()

	// nodeAffinityLabels
	nodeAffinityLabels := make(map[string]string)

	nodeAffinityKeys := volumeConfig.GetNodeAffinityLabelKeys()
	if nodeAffinityKeys == nil {
		nodeAffinityLabels[k8sNodeLabelKeyHostname] = GetNodeLabelValue(opts.SelectedNode, k8sNodeLabelKeyHostname)
	} else {
		for _, nodeAffinityKey := range nodeAffinityKeys {
			nodeAffinityLabels[nodeAffinityKey] = GetNodeLabelValue(opts.SelectedNode, nodeAffinityKey)
		}
	}

	path, err := volumeConfig.GetPath()
	if err != nil {
		klog.Errorf("Failed to provision Local PV %s, unable to get path from volume config, err: %v", opts.PVName, err)
		return nil, pvController.ProvisioningFinished, err
	}

	imagePullSecrets := GetImagePullSecrets(getImagePullSecrets())

	klog.Infof("Creating volume %v at node with labels {%v}, path:%v,ImagePullSecrets:%v", name, nodeAffinityLabels, path, imagePullSecrets)

	//Before using the path for local PV, make sure it is created.
	initCmdsForPath := []string{"mkdir", "-m", "0777", "-p"}
	podOpts := &HelperPodOptions{
		cmdsForPath:        initCmdsForPath,
		name:               name,
		path:               path,
		nodeAffinityLabels: nodeAffinityLabels,
		serviceAccountName: saName,
		selectedNodeTaints: taints,
		imagePullSecrets:   imagePullSecrets,
	}
	iErr := p.createInitPod(ctx, podOpts)
	if iErr != nil {
		klog.Errorf("Initialize volume %v failed: %v", name, iErr)
		return nil, pvController.ProvisioningFinished, iErr
	}

	if volumeConfig.IsXfsQuotaEnabled() {
		softLimitGrace := volumeConfig.getDataField(KeyXFSQuota, KeyQuotaSoftLimit)
		hardLimitGrace := volumeConfig.getDataField(KeyXFSQuota, KeyQuotaHardLimit)
		pvcStorage := opts.PVC.Spec.Resources.Requests.Storage().Value()

		podOpts := &HelperPodOptions{
			name:               name,
			path:               path,
			nodeAffinityLabels: nodeAffinityLabels,
			serviceAccountName: saName,
			selectedNodeTaints: taints,
			imagePullSecrets:   imagePullSecrets,
			softLimitGrace:     softLimitGrace,
			hardLimitGrace:     hardLimitGrace,
			pvcStorage:         pvcStorage,
		}
		iErr := p.createQuotaPod(ctx, podOpts)
		if iErr != nil {
			klog.Infof("Applying quota failed: %v", iErr)
			return nil, pvController.ProvisioningFinished, iErr
		}
	}

	if volumeConfig.IsExt4QuotaEnabled() {
		softLimitGrace := volumeConfig.getDataField(KeyEXT4Quota, KeyQuotaSoftLimit)
		hardLimitGrace := volumeConfig.getDataField(KeyEXT4Quota, KeyQuotaHardLimit)
		pvcStorage := opts.PVC.Spec.Resources.Requests.Storage().Value()

		podOpts := &HelperPodOptions{
			name:               name,
			path:               path,
			nodeAffinityLabels: nodeAffinityLabels,
			serviceAccountName: saName,
			selectedNodeTaints: taints,
			imagePullSecrets:   imagePullSecrets,
			softLimitGrace:     softLimitGrace,
			hardLimitGrace:     hardLimitGrace,
			pvcStorage:         pvcStorage,
		}
		iErr := p.createQuotaPod(ctx, podOpts)
		if iErr != nil {
			klog.Infof("Applying quota failed: %v", iErr)
			return nil, pvController.ProvisioningFinished, iErr
		}
	}

	// VolumeMode will always be specified as Filesystem for host path volume,
	// and the value passed in from the PVC spec will be ignored.
	fs := corev1.PersistentVolumeFilesystem

	// It is possible that the HostPath doesn't already exist on the node.
	// Set the Local PV to create it.
	//hostPathType := corev1.HostPathDirectoryOrCreate

	// TODO initialize the Labels and annotations
	// Use annotations to specify the context using which the PV was created.
	//volAnnotations := make(map[string]string)
	//volAnnotations[string(v1alpha1.CASTypeKey)] = casVolume.Spec.CasType
	//fstype := casVolume.Spec.FSType

	labels := make(map[string]string)
	//labels[string(v1alpha1.StorageClassKey)] = *className

	pvObj := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: *opts.StorageClass.ReclaimPolicy,
			AccessModes:                   pvc.Spec.AccessModes,
			VolumeMode:                    &fs,
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: pvc.Spec.Resources.Requests[corev1.ResourceStorage],
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				Local: &corev1.LocalVolumeSource{
					Path: path,
				},
			},
			NodeAffinity: buildVolumeNodeAffinity(nodeAffinityLabels),
		},
	}

	klog.Infof("Successfully provisioned Local PV %s", opts.PVName)
	return pvObj, pvController.ProvisioningFinished, nil
}

// GetNodeObjectFromLabels returns the Node Object with matching label key and value
func (p *Provisioner) GetNodeObjectFromLabels(nodeLabels map[string]string) (*corev1.Node, error) {
	labelSelector := metav1.LabelSelector{MatchLabels: nodeLabels}
	listOptions := metav1.ListOptions{
		LabelSelector: labels.Set(labelSelector.MatchLabels).String(),
	}
	nodeList, err := p.kubeClient.CoreV1().Nodes().List(context.TODO(), listOptions)
	if err != nil || len(nodeList.Items) == 0 {
		// After the PV is created and node affinity is set
		// based on kubernetes.io/hostname label, either:
		// - hostname label changed on the node or
		// - the node is deleted from the cluster.
		return nil, errors.Errorf("Unable to get the Node with the Node Labels {%v}", nodeLabels)
	}
	if len(nodeList.Items) != 1 {
		// After the PV is created and node affinity is set
		// on a custom affinity label, there may be a transitory state
		// with two nodes matching (old and new) label.
		return nil, errors.Errorf("Unable to determine the Node. Found multiple nodes matching the labels {%v}", nodeLabels)
	}
	return &nodeList.Items[0], nil

}

// DeleteHostPath is invoked by the PVC controller to perform clean-up
//
//	activities before deleteing the PV object. If reclaim policy is
//	set to not-retain, then this function will create a helper pod
//	to delete the host path from the node.
func (p *Provisioner) DeleteHostPath(ctx context.Context, pv *corev1.PersistentVolume) (err error) {
	defer func() {
		err = errors.Wrapf(err, "failed to delete volume %v", pv.Name)
	}()

	saName := getServiceAccountName()
	//Determine the path and node of the Local PV.
	path := ""
	if local := pv.Spec.PersistentVolumeSource.Local; local != nil {
		path = local.Path
	}
	if path == "" {
		return errors.Errorf("no HostPath set")
	}

	nodeAffinityLabels := getAffinitedNodeLabels(pv)
	if len(nodeAffinityLabels) == 0 {
		return errors.Errorf("cannot find affinited node details")
	}
	klog.Infof("Get the Node Object with label {%v}", nodeAffinityLabels)

	//Get the node Object once again to get updated Taints.
	nodeObject, err := p.GetNodeObjectFromLabels(nodeAffinityLabels)
	if err != nil {
		return err
	}
	taints := GetTaints(nodeObject)

	imagePullSecrets := GetImagePullSecrets(getImagePullSecrets())

	//Initiate clean up only when reclaim policy is not retain.
	klog.Infof("Deleting volume %v at %v:%v", pv.Name, GetNodeHostname(nodeObject), path)
	cleanupCmdsForPath := []string{"rm", "-rf"}
	podOpts := &HelperPodOptions{
		cmdsForPath:        cleanupCmdsForPath,
		name:               pv.Name,
		path:               path,
		nodeAffinityLabels: nodeAffinityLabels,
		serviceAccountName: saName,
		selectedNodeTaints: taints,
		imagePullSecrets:   imagePullSecrets,
	}

	if err := p.createCleanupPod(ctx, podOpts); err != nil {
		return errors.Wrapf(err, "clean up volume %v failed", pv.Name)
	}
	return nil
}

func getAffinitedNodeLabels(pv *corev1.PersistentVolume) map[string]string {
	nodeAffinity := pv.Spec.NodeAffinity
	if nodeAffinity == nil {
		return nil
	}
	required := nodeAffinity.Required
	if required == nil {
		return nil
	}

	labels := make(map[string]string)

	for _, selectorTerm := range required.NodeSelectorTerms {
		for _, expression := range selectorTerm.MatchExpressions {
			if expression.Operator == corev1.NodeSelectorOpIn {
				if len(expression.Values) != 1 {
					return nil
				}
				labels[expression.Key] = expression.Values[0]
			}
		}
	}
	return labels
}
