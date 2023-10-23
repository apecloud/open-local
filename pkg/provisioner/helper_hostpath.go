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
	"path/filepath"
	"time"

	errors "github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	hostpath "github.com/alibaba/open-local/pkg/provisioner/internal/hostpath"
)

type podConfig struct {
	pOpts                         *HelperPodOptions
	parentDir, volumeDir, podName string
	taints                        []corev1.Taint
}

var (
	//CmdTimeoutCounts specifies the duration to wait for cleanup pod
	//to be launched.
	CmdTimeoutCounts = 120
)

// HelperPodOptions contains the options that
// will launch a Pod on a specific node (nodeHostname)
// to execute a command (cmdsForPath) on a given
// volume path (path)
type HelperPodOptions struct {
	//nodeAffinityLabels represents the labels of the node where pod should be launched.
	nodeAffinityLabels map[string]string

	//name is the name of the PV for which the pod is being launched
	name string

	//cmdsForPath represent either create (mkdir) or delete(rm)
	//commands that need to be executed on the volume path.
	cmdsForPath []string

	//path is the volume hostpath directory
	path string

	//serviceAccountName is the service account with which the pod should be launched
	serviceAccountName string

	selectedNodeTaints []corev1.Taint

	imagePullSecrets []corev1.LocalObjectReference
}

// validate checks that the required fields to launch
// helper pods are valid. helper pods are used to either
// create or delete a directory (path) on a given node hostname (nodeHostname).
// name refers to the volume being created or deleted.
func (pOpts *HelperPodOptions) validate() error {
	if pOpts.name == "" ||
		pOpts.path == "" ||
		len(pOpts.nodeAffinityLabels) == 0 ||
		pOpts.serviceAccountName == "" {
		return errors.Errorf("invalid empty name or hostpath or hostname or service account name, %+v", pOpts)
	}
	return nil
}

// createInitPod launches a helper(busybox) pod, to create the host path.
//
//	The local pv expect the hostpath to be already present before mounting
//	into pod. Validate that the local pv host path is not created under root.
func (p *Provisioner) createInitPod(ctx context.Context, pOpts *HelperPodOptions) error {
	var config podConfig
	config.pOpts, config.podName = pOpts, "init"
	//err := pOpts.validate()
	if err := pOpts.validate(); err != nil {
		return err
	}

	// Initialize HostPath builder and validate that
	// volume directory is not directly under root.
	// Extract the base path and the volume unique path.
	var vErr error
	config.parentDir, config.volumeDir, vErr = hostpath.NewBuilder().WithPath(pOpts.path).
		WithCheckf(hostpath.IsNonRoot(), "volume directory {%v} should not be under root directory", pOpts.path).
		ExtractSubPath()
	if vErr != nil {
		return vErr
	}

	//Pass on the taints, to create tolerations.
	config.taints = pOpts.selectedNodeTaints

	config.pOpts.cmdsForPath = append(config.pOpts.cmdsForPath, filepath.Join("/data/", config.volumeDir))

	iPod, err := p.launchPod(ctx, config)
	if err != nil {
		return err
	}

	if err := p.exitPod(ctx, iPod); err != nil {
		return err
	}

	return nil
}

// createCleanupPod launches a helper(busybox) pod, to delete the host path.
//
//	This provisioner expects that the host paths are created using
//	an unique PV path - under a given BasePath. From the absolute path,
//	it extracts the base path and the PV path. The helper pod is then launched
//	by mounting the base path - and performing a delete on the unique PV path.
func (p *Provisioner) createCleanupPod(ctx context.Context, pOpts *HelperPodOptions) error {
	var config podConfig
	config.pOpts, config.podName = pOpts, "cleanup"
	//err := pOpts.validate()
	if err := pOpts.validate(); err != nil {
		return err
	}

	// Initialize HostPath builder and validate that
	// volume directory is not directly under root.
	// Extract the base path and the volume unique path.
	var vErr error
	config.parentDir, config.volumeDir, vErr = hostpath.NewBuilder().WithPath(pOpts.path).
		WithCheckf(hostpath.IsNonRoot(), "volume directory {%v} should not be under root directory", pOpts.path).
		ExtractSubPath()
	if vErr != nil {
		return vErr
	}

	config.taints = pOpts.selectedNodeTaints

	config.pOpts.cmdsForPath = append(config.pOpts.cmdsForPath, filepath.Join("/data/", config.volumeDir))

	cPod, err := p.launchPod(ctx, config)
	if err != nil {
		return err
	}

	if err := p.exitPod(ctx, cPod); err != nil {
		return err
	}
	return nil
}

func (p *Provisioner) launchPod(ctx context.Context, config podConfig) (*corev1.Pod, error) {
	// the helper pod need to be launched in privileged mode. This is because in CoreOS
	// nodes, pods without privileged access cannot write to the host directory.
	// Helper pods need to create and delete directories on the host.
	privileged := true

	container := corev1.Container{
		Name:    "local-path-" + config.podName,
		Image:   p.helperImage,
		Command: append([]string(nil), config.pOpts.cmdsForPath...),
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "data",
				ReadOnly:  false,
				MountPath: "/data/",
			},
			{
				Name:      "dev",
				ReadOnly:  false,
				MountPath: "/dev/",
			},
		},
		SecurityContext: &corev1.SecurityContext{
			Privileged: &privileged,
		},
	}

	volumes := []corev1.Volume{
		{
			Name: "data",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: config.parentDir,
				},
			},
		},
		{
			Name: "dev",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/dev/",
				},
			},
		},
	}

	helperPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: config.podName + "-" + config.pOpts.name,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Affinity: &corev1.Affinity{
				NodeAffinity: buildNodeAffinity(config.pOpts.nodeAffinityLabels),
			},
			ServiceAccountName: config.pOpts.serviceAccountName,
			Tolerations:        buildTolerationsForTaints(config.taints...),
			Containers:         []corev1.Container{container},
			ImagePullSecrets:   append([]corev1.LocalObjectReference(nil), config.pOpts.imagePullSecrets...),
			Volumes:            volumes,
		},
	}

	var hPod *corev1.Pod

	//Launch the helper pod.
	hPod, err := p.kubeClient.CoreV1().Pods(p.namespace).Create(ctx, helperPod, metav1.CreateOptions{})
	return hPod, err
}

func (p *Provisioner) exitPod(ctx context.Context, hPod *corev1.Pod) error {
	defer func() {
		e := p.kubeClient.CoreV1().Pods(p.namespace).Delete(ctx, hPod.Name, metav1.DeleteOptions{})
		if e != nil {
			klog.Errorf("unable to delete the helper pod: %v", e)
		}
	}()

	//Wait for the helper pod to complete it job and exit
	completed := false
	for i := 0; i < CmdTimeoutCounts; i++ {
		checkPod, err := p.kubeClient.CoreV1().Pods(p.namespace).Get(ctx, hPod.Name, metav1.GetOptions{})
		if err != nil {
			return err
		} else if checkPod.Status.Phase == corev1.PodSucceeded {
			completed = true
			break
		}
		time.Sleep(1 * time.Second)
	}
	if !completed {
		return errors.Errorf("create process timeout after %v seconds", CmdTimeoutCounts)
	}
	return nil
}

func buildNodeSelector(labels map[string]string) *corev1.NodeSelector {
	var matchExpressions []corev1.NodeSelectorRequirement

	for key, value := range labels {
		if value != "" {
			matchExpressions = append(matchExpressions, corev1.NodeSelectorRequirement{
				Key:      key,
				Operator: corev1.NodeSelectorOpIn,
				Values: []string{
					value,
				},
			})
		}
	}

	if len(matchExpressions) == 0 {
		return nil
	}
	return &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{
			{MatchExpressions: matchExpressions},
		},
	}
}

func buildVolumeNodeAffinity(labels map[string]string) *corev1.VolumeNodeAffinity {
	selector := buildNodeSelector(labels)
	if selector == nil {
		return nil
	}
	return &corev1.VolumeNodeAffinity{
		Required: selector,
	}
}

func buildNodeAffinity(labels map[string]string) *corev1.NodeAffinity {
	selector := buildNodeSelector(labels)
	if selector == nil {
		return nil
	}
	return &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: selector,
	}
}

func buildTolerationsForTaints(taints ...corev1.Taint) []corev1.Toleration {
	tolerations := []corev1.Toleration{}
	for i := range taints {
		var toleration corev1.Toleration
		toleration.Key = taints[i].Key
		toleration.Effect = taints[i].Effect
		if len(taints[i].Value) == 0 {
			toleration.Operator = corev1.TolerationOpExists
		} else {
			toleration.Value = taints[i].Value
			toleration.Operator = corev1.TolerationOpEqual
		}
		tolerations = append(tolerations, toleration)
	}
	return tolerations
}
