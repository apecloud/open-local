package provisioner

import (
	"context"
	"errors"
	"os"
	"path"

	"sigs.k8s.io/sig-storage-lib-external-provisioner/v9/controller"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klog "k8s.io/klog/v2"
)

const (
	AnnotationNameOfPvIdentity = "KBHostPathProvisionerIdentity"
)

type hostPathProvisioner struct {
	// The directory to create PV-backing directories in
	pvBaseDir string

	// Identity of this hostPathProvisioner, set to node's name. Used to identify
	// "this" provisioner's PVs.
	identity string
}

// NewHostPathProvisioner creates a new hostpath provisioner
func NewHostPathProvisioner(pvBaseDir string) controller.Provisioner {
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		klog.Fatal("env variable NODE_NAME must be set so that this provisioner can identify itself")
	}
	return &hostPathProvisioner{
		pvBaseDir: pvBaseDir,
		identity:  nodeName,
	}
}

// Provision creates a storage asset and returns a PV object representing it.
func (p *hostPathProvisioner) Provision(ctx context.Context, options controller.ProvisionOptions) (*v1.PersistentVolume, controller.ProvisioningState, error) {
	path := path.Join(p.pvBaseDir, options.PVName)

	if err := os.MkdirAll(path, 0777); err != nil {
		return nil, controller.ProvisioningFinished, err
	}

	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: options.PVName,
			Annotations: map[string]string{
				AnnotationNameOfPvIdentity: p.identity,
			},
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: *options.StorageClass.ReclaimPolicy,
			AccessModes:                   options.PVC.Spec.AccessModes,
			Capacity: v1.ResourceList{
				v1.ResourceName(v1.ResourceStorage): options.PVC.Spec.Resources.Requests[v1.ResourceName(v1.ResourceStorage)],
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: path,
				},
			},
		},
	}

	return pv, controller.ProvisioningFinished, nil
}

// Delete removes the storage asset that was created by Provision represented
// by the given PV.
func (p *hostPathProvisioner) Delete(ctx context.Context, volume *v1.PersistentVolume) error {
	ann, ok := volume.Annotations[AnnotationNameOfPvIdentity]
	if !ok {
		return errors.New("identity annotation not found on PV")
	}
	if ann != p.identity {
		return &controller.IgnoredError{Reason: "identity annotation on PV does not match ours"}
	}

	path := path.Join(p.pvBaseDir, volume.Name)
	if err := os.RemoveAll(path); err != nil {
		return err
	}

	return nil
}
