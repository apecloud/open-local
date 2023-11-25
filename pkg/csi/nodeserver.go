/*
Copyright © 2021 Alibaba Group Holding Ltd.

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

package csi

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/alibaba/open-local/pkg"
	localtype "github.com/alibaba/open-local/pkg"
	"github.com/alibaba/open-local/pkg/utils"
	"github.com/container-storage-interface/spec/lib/go/csi"
	volume "github.com/kata-containers/kata-containers/src/runtime/pkg/direct-volume"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	log "k8s.io/klog/v2"
	mountutils "k8s.io/mount-utils"
	utilexec "k8s.io/utils/exec"
)

type cgroupVersion int

const (
	cgroupUnknown cgroupVersion = iota
	cgroupV1
	cgroupV2
)

type cgroupDriver string

const (
	cgroupDriverSystemd  = "systemd"
	cgroupDriverCgroupfs = "cgroupfs"
)

type nodeServer struct {
	k8smounter           *mountutils.SafeFormatAndMount
	ephemeralVolumeStore Store
	inFlight             *InFlight
	osTool               OSTool

	cgroupFsRoot  string
	cgroupVersion cgroupVersion
	cgroupDriver  cgroupDriver

	options *driverOptions
}

func newNodeServer(options *driverOptions) *nodeServer {
	store, err := NewVolumeStore(DefaultEphemeralVolumeDataFilePath)
	if err != nil {
		log.Fatalf("fail to initialize ephemeral volume store: %s", err.Error())
	}

	cgroupFsRoot, cgroupVersion, err := detectCgroupFsRootDir()
	if err != nil {
		log.Fatalf("fail to detect cgroup fs root directory: %v", err)
	}

	cgroupDriver, err := detectCgroupDriver()
	if err != nil {
		log.Fatalf("fail to detect cgroup driver: %v", err)
	}

	log.Infof("Node info: cgroup root(%s), cgroup version(%d), cgroupDriver(%s)", cgroupFsRoot, cgroupVersion, cgroupDriver)

	ns := &nodeServer{
		k8smounter: &mountutils.SafeFormatAndMount{
			Interface: mountutils.New(""),
			Exec:      utilexec.New(),
		},
		ephemeralVolumeStore: store,
		inFlight:             NewInFlight(),
		osTool:               NewOSTool(),
		cgroupFsRoot:         cgroupFsRoot,
		cgroupVersion:        cgroupVersion,
		cgroupDriver:         cgroupDriver,
		options:              options,
	}

	return ns
}

func detectCgroupDriver() (cgroupDriver, error) {
	return cgroupDriverSystemd, nil
	// TODO(gufeijun) do not have permission to access nodes/proxy
	// nodeName := os.Getenv("KUBE_NODE_NAME")
	// if len(nodeName) == 0 {
	// 	return "", errors.New("can not get node's name")
	// }
	// log.Infof("NodeName: %s", nodeName)

	// req := ns.common.options.kubeclient.CoreV1().RESTClient().Get().
	// 	Resource("nodes").
	// 	Name(nodeName).
	// 	Suffix("proxy/configz")

	// result := req.Do(ctx)
	// if result.Error() != nil {
	// 	return "", result.Error()
	// }
	// resp, err := result.Raw()
	// if err != nil {
	// 	return "", err
	// }

	// log.Infof("getCgroupDriver: api response: %s", resp)
	// m := make(map[interface{}]interface{})
	// if err := json.Unmarshal(resp, &m); err != nil {
	// 	return "", err
	// }
	// kubeletCfg, ok := m["kubeletconfig"]
	// if !ok {
	// 	return "", errors.New("can not find kubelete config")
	// }
	// m, ok = kubeletCfg.(map[interface{}]interface{})
	// if !ok {
	// 	return "", errors.New("can not find kubelete config")
	// }
	// driver, ok := m["cgroupDriver"]
	// if !ok {
	// 	return cgroupDriverSystemd, nil
	// }
	// driverStr, ok := driver.(string)
	// if !ok {
	// 	return "", errors.New("invalid cgroup driver")
	// }
	// return cgroupDriver(driverStr), nil
}

func detectCgroupFsRootDir() (path string, version cgroupVersion, err error) {
	file, err := os.Open("/proc/mounts")
	if err != nil {
		err = fmt.Errorf("fail to open /proc/mounts: %v", err)
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)

		if len(fields) < 2 {
			continue
		}

		if fields[0] == "cgroup2" {
			version = cgroupV2
			path = fields[1]
			break
		}

		if fields[0] == "cgroup1" {
			if version == cgroupV2 {
				continue
			}
			version = cgroupV1
			path = fields[1]
		}
	}
	if err = scanner.Err(); err != nil {
		err = fmt.Errorf("fail to read /proc/mounts: %v", err)
		return
	}
	if version == cgroupUnknown {
		err = errors.New("no mounted cgroup file system found")
	}
	return
}

// volume_id: yoda-70597cb6-c08b-4bbb-8d41-c4afcfa91866
// staging_target_path: /var/lib/kubelet/plugins/kubernetes.io/csi/pv/yoda-70597cb6-c08b-4bbb-8d41-c4afcfa91866/globalmount
// target_path: /var/lib/kubelet/pods/2a7bbb9c-c915-4006-84d7-0e3ac9d8d70f/volumes/kubernetes.io~csi/yoda-70597cb6-c08b-4bbb-8d41-c4afcfa91866/mount
func (ns *nodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	log.V(4).Infof("NodePublishVolume: called with args %+v", *req)
	// Step 1: check
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodePublishVolume: Volume ID not provided")
	}
	targetPath := req.GetTargetPath()
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "NodePublishVolume: targetPath is empty")
	}
	log.Infof("NodePublishVolume: start to mount volume %s to target path %s", volumeID, targetPath)

	// Step 2: get volumeType
	volumeType := ""
	if _, ok := req.VolumeContext[VolumeTypeTag]; ok {
		volumeType = req.VolumeContext[VolumeTypeTag]
	}
	ephemeralVolume := req.GetVolumeContext()[pkg.Ephemeral] == "true"
	if ephemeralVolume {
		_, vgNameExist := req.VolumeContext[localtype.ParamVGName]
		if !vgNameExist {
			return nil, status.Error(codes.InvalidArgument, "NodePublishVolume: must set vgName in volumeAttributes when creating ephemeral local volume")
		}
		volumeType = string(pkg.VolumeTypeLVM)
	}

	// check if the volume is a direct-assigned volume, direct volume will be used as virtio-blk
	direct := false
	if val, ok := req.VolumeContext[DirectTag]; ok {
		var err error
		direct, err = strconv.ParseBool(val)
		if err != nil {
			direct = false
		}
	}

	// Step 3: direct
	if ok := ns.inFlight.Insert(volumeID); !ok {
		return nil, status.Errorf(codes.Aborted, VolumeOperationAlreadyExists, volumeID)
	}
	defer func() {
		ns.inFlight.Delete(volumeID)
	}()
	if direct {
		if volumeType == string(pkg.VolumeTypeMountPoint) || volumeType == string(pkg.VolumeTypeDevice) {
			return nil, status.Errorf(codes.InvalidArgument, "The volume type should not be %s or %s", string(pkg.VolumeTypeMountPoint), string(pkg.VolumeTypeDevice))
		}

		err := ns.publishDirectVolume(ctx, req, volumeType)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "NodePublishVolume: volume %s with error: %s", volumeID, err.Error())
		}
		log.Infof("NodePublishVolume: add local volume %s to %s successfully", volumeID, targetPath)
		return &csi.NodePublishVolumeResponse{}, nil
	}

	// check targetPath
	if _, err := ns.osTool.Stat(targetPath); os.IsNotExist(err) {
		if err := ns.osTool.MkdirAll(targetPath, 0750); err != nil {
			return &csi.NodePublishVolumeResponse{}, fmt.Errorf("mountLvmFS: fail to mkdir target path %s: %s", targetPath, err.Error())
		}
	}

	// Step 4: mount
	volCap := req.GetVolumeCapability()
	switch volumeType {
	case string(pkg.VolumeTypeLVM):
		switch volCap.GetAccessType().(type) {
		case *csi.VolumeCapability_Block:
			err := ns.mountLvmBlock(ctx, req)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "NodePublishVolume(mountLvmBlock): fail to mount lvm volume %s with path %s: %s", volumeID, targetPath, err.Error())
			}
		case *csi.VolumeCapability_Mount:
			err := ns.mountLvmFS(ctx, req)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "NodePublishVolume(mountLvmFS): fail to mount lvm volume %s with path %s: %s", volumeID, targetPath, err.Error())
			}
		}
		if err := ns.setIOThrottling(ctx, req); err != nil {
			return nil, err
		}
	case string(pkg.VolumeTypeMountPoint):
		err := ns.mountMountPointVolume(ctx, req)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "NodePublishVolume: fail to mount mountpoint volume %s with path %s: %s", volumeID, targetPath, err.Error())
		}
	case string(pkg.VolumeTypeDevice):
		switch volCap.GetAccessType().(type) {
		case *csi.VolumeCapability_Block:
			err := ns.mountDeviceVolumeBlock(ctx, req)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "NodePublishVolume(Block): fail to mount device volume %s with path %s: %s", volumeID, targetPath, err.Error())
			}
		case *csi.VolumeCapability_Mount:
			err := ns.mountDeviceVolumeFS(ctx, req)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "NodePublishVolume(FileSystem): fail to mount device volume %s with path %s: %s", volumeID, targetPath, err.Error())
			}
		}
	case string(pkg.VolumeTypeHostPath):
		impl := &hostPathNsImpl{common: ns}
		return impl.NodePublishVolume(ctx, req)
	default:
		return nil, status.Errorf(codes.Internal, "NodePublishVolume: unsupported volume %s with type %s", volumeID, volumeType)
	}

	log.Infof("NodePublishVolume: mount local volume %s to %s successfully", volumeID, targetPath)
	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	log.V(4).Infof("NodeUnpublishVolume: called with args %+v", *req)
	// Step 1: check
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeUnpublishVolume: Volume ID not provided")
	}
	targetPath := req.GetTargetPath()
	if targetPath == "" {
		return nil, status.Error(codes.Internal, "NodeUnpublishVolume: targetPath is empty")
	}
	log.Infof("NodeUnpublishVolume: start to umount target path %s for volume %s", targetPath, volumeID)

	// Step 2: umount
	if ok := ns.inFlight.Insert(volumeID); !ok {
		return nil, status.Errorf(codes.Aborted, VolumeOperationAlreadyExists, volumeID)
	}
	defer func() {
		ns.inFlight.Delete(volumeID)
	}()
	if err := ns.osTool.CleanupMountPoint(targetPath, ns.k8smounter, true /*extensiveMountPointCheck*/); err != nil {
		return nil, status.Errorf(codes.Internal, "NodeUnpublishVolume: fail to umount volume %s for path %s: %s", volumeID, targetPath, err.Error())
	}

	// Step 3: delete ephemeral device
	var err error
	ephemeralDevice, exist := ns.ephemeralVolumeStore.GetDevice(volumeID)
	if exist && ephemeralDevice != "" {
		// /dev/mapper/yoda--pool0-yoda--5c523416--7288--4138--95e0--f9392995959f
		err = ns.removeLVMByDevicePath(ephemeralDevice)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "NodeUnpublishVolume: fail to remove ephemeral volume: %s", err.Error())
		}

		err = ns.ephemeralVolumeStore.DeleteVolume(volumeID)
		if err != nil {
			log.Errorf("NodeUnpublishVolume: failed to remove volume in volume store: %s", err.Error())
		}
	}

	log.Infof("NodeUnpublishVolume: umount target path %s for volume %s successfully", targetPath, volumeID)
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	log.V(4).Infof("NodeStageVolume: called with args %+v", *req)
	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	log.V(4).Infof("NodeUnstageVolume: called with args %+v", *req)
	return &csi.NodeUnstageVolumeResponse{}, nil
}

// called with args {VolumeId:yoda-7825af6f-ea0a-4047-8704-7576dfe8d201 VolumePath:/var/lib/kubelet/pods/2ba91d3a-1b97-4d72-9dd9-be98aebbfe62/volumes/kubernetes.io~csi/yoda-7825af6f-ea0a-4047-8704-7576dfe8d201/mount CapacityRange:required_bytes:21474836480  StagingTargetPath:/var/lib/kubelet/plugins/kubernetes.io/csi/pv/yoda-7825af6f-ea0a-4047-8704-7576dfe8d201/globalmount VolumeCapability:mount:<fs_type:"ext4" > access_mode:<mode:SINGLE_NODE_WRITER >  XXX_NoUnkeyedLiteral:{} XXX_unrecognized:[] XXX_sizecache:0}
func (ns *nodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (
	*csi.NodeExpandVolumeResponse, error) {
	log.V(4).Infof("NodeExpandVolume: called with args %+v", *req)
	volumeID := req.VolumeId
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeExpandVolume: Volume ID not provided")
	}
	targetPath := req.VolumePath
	if len(targetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeExpandVolume: Target path not provided")
	}
	expectSize := req.CapacityRange.RequiredBytes
	if err := ns.resizeVolume(ctx, volumeID, targetPath); err != nil {
		return nil, status.Errorf(codes.Internal, "NodeExpandVolume: Resize local volume %s with error: %s", volumeID, err.Error())
	}

	log.Infof("NodeExpandVolume: Successful expand local volume: %v to %d", req.VolumeId, expectSize)
	return &csi.NodeExpandVolumeResponse{}, nil
}

func (ns *nodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	log.V(4).Infof("NodeGetCapabilities: called with args %+v", *req)
	return &csi.NodeGetCapabilitiesResponse{Capabilities: NodeCaps}, nil
}

func (ns *nodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	log.V(4).Infof("NodeGetInfo: called with args %+v", *req)
	return &csi.NodeGetInfoResponse{
		NodeId: ns.options.nodeID,
		// make sure that the driver works on this particular node only
		AccessibleTopology: &csi.Topology{
			Segments: map[string]string{
				pkg.KubernetesNodeIdentityKey: ns.options.nodeID,
			},
		},
	}, nil
}

// NodeGetVolumeStats used for csi metrics
func (ns *nodeServer) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	log.V(4).Infof("NodeGetVolumeStats: called with args %+v", *req)
	targetPath := req.GetVolumePath()
	if targetPath == "" {
		return nil, status.Errorf(codes.InvalidArgument, "NodeGetVolumeStats target local path %v is empty", targetPath)
	}

	return utils.GetMetrics(targetPath)
}

func (ns *nodeServer) addDirectVolume(volumePath, device, fsType string) error {
	mountInfo := struct {
		VolumeType string            `json:"volume-type"`
		Device     string            `json:"device"`
		FsType     string            `json:"fstype"`
		Metadata   map[string]string `json:"metadata,omitempty"`
		Options    []string          `json:"options,omitempty"`
	}{
		VolumeType: "block",
		Device:     device,
		FsType:     fsType,
	}

	mi, err := json.Marshal(mountInfo)
	if err != nil {
		log.Error("addDirectVolume - json.Marshal failed: ", err.Error())
		return status.Errorf(codes.Internal, "json.Marshal failed: %s", err.Error())
	}

	if err := volume.Add(volumePath, string(mi)); err != nil {
		log.Error("addDirectVolume - add direct volume failed: ", err.Error())
		return status.Errorf(codes.Internal, "add direct volume failed: %s", err.Error())
	}

	log.Infof("add direct volume done: %s %s", volumePath, string(mi))
	return nil
}

func (ns *nodeServer) mountBlockDevice(device, targetPath string) error {
	notMounted, err := ns.k8smounter.IsLikelyNotMountPoint(targetPath)
	if err != nil {
		log.Errorf("mountBlockDevice - check if %s is mounted failed: %s", targetPath, err.Error())
		return status.Errorf(codes.Internal, "check if %s is mounted failed: %s", targetPath, err.Error())
	}

	mountOptions := []string{"bind"}
	if !notMounted {
		log.Infof("Target path %s is already mounted", targetPath)
	} else {
		log.Infof("mounting %s at %s", device, targetPath)

		if err := ns.osTool.MountBlock(device, targetPath, mountOptions...); err != nil {
			if removeErr := ns.osTool.Remove(targetPath); removeErr != nil {
				log.Errorf("Remove mount target %s failed: %s", targetPath, removeErr.Error())
				return status.Errorf(codes.Internal, "Could not remove mount target %q: %v", targetPath, removeErr)
			}
			log.Errorf("mount block %s at %s failed: %s", device, targetPath, err.Error())
			return status.Errorf(codes.Internal, "Could not mount block %s at %s: %s", device, targetPath, err.Error())
		}
	}

	return nil
}

func (ns *nodeServer) publishDirectVolume(ctx context.Context, req *csi.NodePublishVolumeRequest, volumeType string) (err error) {
	// in case publish volume failed, clean up the resources
	defer func() {
		if err != nil {
			log.Error("publishDirectVolume failed: ", err.Error())

			ephemeralDevice, exist := ns.ephemeralVolumeStore.GetDevice(req.VolumeId)
			if exist && ephemeralDevice != "" {
				if err := ns.removeLVMByDevicePath(ephemeralDevice); err != nil {
					log.Errorf("fail to remove lvm device (%s): %s", ephemeralDevice, err.Error())
				}
			}

			if err := volume.Remove(req.GetTargetPath()); err != nil {
				log.Errorf("NodePublishVolume - direct volume remove failed: %s", err.Error())
			}
		}
	}()

	device := ""
	if volumeType != string(pkg.VolumeTypeLVM) {
		if value, ok := req.VolumeContext[pkg.VolumeTypeKey]; ok {
			device = value
		} else {
			log.Error("source device is empty")
			return status.Error(codes.Internal, "publishDirectVolume: source device is empty")
		}
	} else {
		var err error
		device, err = ns.createLV(ctx, req)
		if err != nil {
			log.Error("publishDirectVolume - create logical volume failed: ", err.Error())
			return status.Errorf(codes.Internal, "publishDirectVolume - create logical volume failed: %s", err.Error())
		}

		ephemeralVolume := req.GetVolumeContext()[pkg.Ephemeral] == "true"
		if ephemeralVolume {
			if err := ns.ephemeralVolumeStore.AddVolume(req.VolumeId, device); err != nil {
				log.Errorf("fail to add volume: %s", err.Error())
			}
		}
	}

	mount := false
	volCap := req.GetVolumeCapability()
	if volumeType == string(pkg.VolumeTypeMountPoint) {
		mount = true
	}

	if _, ok := volCap.GetAccessType().(*csi.VolumeCapability_Mount); ok {
		mount = true
	}

	if mount {
		fsType := volCap.GetMount().FsType
		if len(fsType) == 0 {
			fsType = DefaultFs
		}

		if err := ns.addDirectVolume(req.GetTargetPath(), device, fsType); err != nil {
			log.Error("addDirectVolume failed: ", err.Error())
			return status.Errorf(codes.Internal, "addDirectVolume failed: %s", err.Error())
		}

		if err := utils.FormatBlockDevice(device, fsType); err != nil {
			log.Error("FormatBlockDevice failed: ", err.Error())
			return status.Errorf(codes.Internal, "FormatBlockDevice failed: %s", err.Error())
		}
	} else {
		if err := ns.mountBlockDevice(device, req.GetTargetPath()); err != nil {
			log.Error("mountBlockDevice failed: ", err.Error())
			return status.Errorf(codes.Internal, "mountBlockDevice failed: %s", err.Error())
		}
	}

	return nil
}

func (ns *nodeServer) resizeVolume(ctx context.Context, volumeID, targetPath string) error {
	// Get volumeType
	volumeType := string(pkg.VolumeTypeLVM)
	_, _, pv, err := getPvInfo(ns.options.kubeclient, volumeID)
	if err != nil {
		return err
	}
	if pv != nil && pv.Spec.CSI != nil {
		if value, ok := pv.Spec.CSI.VolumeAttributes["volumeType"]; ok {
			volumeType = value
		}
	} else {
		return status.Errorf(codes.Internal, "resizeVolume: local volume get pv info error %s", volumeID)
	}

	switch volumeType {
	case string(pkg.VolumeTypeLVM):
		// Get lvm info
		vgName := utils.GetVGNameFromCsiPV(pv)
		if vgName == "" {
			return status.Errorf(codes.Internal, "resizeVolume: Volume %s with vgname empty", pv.Name)
		}

		devicePath := filepath.Join("/dev", vgName, volumeID)

		log.Infof("NodeExpandVolume:: volumeId: %s, devicePath: %s", volumeID, devicePath)

		ok, err := ns.osTool.ResizeFS(devicePath, targetPath)
		if err != nil {
			return fmt.Errorf("NodeExpandVolume: Lvm Resize Error, volumeId: %s, devicePath: %s, volumePath: %s, err: %s", volumeID, devicePath, targetPath, err.Error())
		}
		if !ok {
			return status.Errorf(codes.Internal, "NodeExpandVolume:: Lvm Resize failed, volumeId: %s, devicePath: %s, volumePath: %s", volumeID, devicePath, targetPath)
		}
		log.Infof("NodeExpandVolume:: lvm resizefs successful volumeId: %s, devicePath: %s, volumePath: %s", volumeID, devicePath, targetPath)
		return nil
	}
	return nil
}

func (ns *nodeServer) setIOThrottling(ctx context.Context, req *csi.NodePublishVolumeRequest) (err error) {
	volumeID := req.VolumeId

	containsValue, bps, iops, err := requireThrottleIO(req.VolumeContext)

	if err != nil {
		log.Errorf("invalid bps or iops parameter in storage class: %s", err)
		return err
	}
	if !containsValue {
		log.Infof("no need to set throttle for volume %s", volumeID)
		return nil
	}
	volCap := req.GetVolumeCapability()
	targetPath := req.GetTargetPath()
	// get pod
	var pod v1.Pod
	var podUID string
	switch volCap.GetAccessType().(type) {
	case *csi.VolumeCapability_Block:
		// /var/lib/kubelet/plugins/kubernetes.io/csi/volumeDevices/publish/yoda-c018ff81-d346-452e-b7b8-a45f1d1c230e/76cf946e-d074-4455-a272-4d3a81264fab
		podUID = strings.Split(targetPath, "/")[10]
	case *csi.VolumeCapability_Mount:
		// /var/lib/kubelet/pods/2a7bbb9c-c915-4006-84d7-0e3ac9d8d70f/volumes/kubernetes.io~csi/yoda-70597cb6-c08b-4bbb-8d41-c4afcfa91866/mount
		podUID = strings.Split(targetPath, "/")[5]
	}
	log.Infof("pod(volume id %s) uuid is %s", volumeID, podUID)
	namespace := req.VolumeContext[localtype.PVCNameSpace]
	// set ResourceVersion to 0
	// https://arthurchiao.art/blog/k8s-reliability-list-data-zh/
	pods, err := ns.options.kubeclient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
	for _, podItem := range pods.Items {
		if podItem.UID == types.UID(podUID) {
			pod = podItem
		}
	}
	if err != nil {
		return status.Errorf(codes.Internal, "NodePublishVolume: failed to get pod(uuid: %s): %s", podUID, err.Error())
	}
	// pod qosClass and blkioPath
	qosClass := pod.Status.QOSClass

	// get cgroup version
	getCgroupVersion := func() string {
		var fsPath string = "/proc/filesystems"
		// by default, return "v1"
		var cgroupVersion string = "v1"

		fs, err := os.Open(fsPath)
		if err != nil {
			log.Errorf("get cgroup version from %s failed: %s", fsPath, err.Error())
			return cgroupVersion
		}
		defer fs.Close()

		fsScanner := bufio.NewScanner(fs)
		fsScanner.Split(bufio.ScanLines)

		cgroup2 := "cgroup2"

		for fsScanner.Scan() {
			if strings.Contains(fsScanner.Text(), cgroup2) {
				cgroupVersion = "v2"
				break
			}
		}

		if err := fsScanner.Err(); err != nil {
			log.Errorf("scan for cgroup version from %s err:%s", fsPath, err.Error())
		}

		return cgroupVersion
	}

	getBlkioPath := func(cgroupVersion string) string {
		var blkioPath string
		if cgroupVersion == "v1" { // for cgroup v1
			blkioPath = fmt.Sprintf("%s/fs/cgroup/blkio/%s%s%s", ns.options.sysPath, utils.CgroupPathFormatter.ParentDir, utils.CgroupPathFormatter.QOSDirFn(qosClass), utils.CgroupPathFormatter.PodDirFn(qosClass, podUID))
		} else { // for higher cgroup version, current version = v2
			blkioPath = fmt.Sprintf("%s/fs/cgroup/%s%s%s", ns.options.sysPath, utils.CgroupPathFormatter.ParentDir, utils.CgroupPathFormatter.QOSDirFn(qosClass), utils.CgroupPathFormatter.PodDirFn(qosClass, podUID))
		}
		return blkioPath
	}

	makeBlkioCmdstr := func(cgroupVersion string, blkioPath string, maj uint64, min uint64, iops int64, bps int64) []string {
		var cmdArray []string
		var cmdstr string

		if cgroupVersion == "v1" {
			if iops > 0 {
				cmdstr = fmt.Sprintf("echo %s > %s", fmt.Sprintf("%d:%d %d", maj, min, iops), fmt.Sprintf("%s/%s", blkioPath, localtype.IOPSReadFile))
				cmdArray = append(cmdArray, cmdstr)
				cmdstr = fmt.Sprintf("echo %s > %s", fmt.Sprintf("%d:%d %d", maj, min, iops), fmt.Sprintf("%s%s", blkioPath, localtype.IOPSWriteFile))
				cmdArray = append(cmdArray, cmdstr)
			}

			if bps > 0 {
				cmdstr = fmt.Sprintf("echo %s > %s", fmt.Sprintf("%d:%d %d", maj, min, bps), fmt.Sprintf("%s%s", blkioPath, localtype.BPSReadFile))
				cmdArray = append(cmdArray, cmdstr)
				cmdstr = fmt.Sprintf("echo %s > %s", fmt.Sprintf("%d:%d %d", maj, min, bps), fmt.Sprintf("%s%s", blkioPath, localtype.BPSWriteFile))
				cmdArray = append(cmdArray, cmdstr)
			}
		} else {
			var iopsstr string
			var bpsstr string
			if iops > 0 {
				iopsstr = fmt.Sprintf("riops=%d wiops=%d", iops, iops)
			} else {
				iopsstr = "riops=max wiops=max"
			}

			if bps > 0 {
				bpsstr = fmt.Sprintf("rbps=%d wbps=%d", bps, bps)
			} else {
				bpsstr = "rbps=max wbps=max"
			}

			if iops > 0 || bps > 0 {
				cmdstr = fmt.Sprintf("echo %s > %s", fmt.Sprintf("%d:%d %s %s", maj, min, iopsstr, bpsstr), fmt.Sprintf("%s/%s", blkioPath, localtype.V2_IOFILE))
				cmdArray = append(cmdArray, cmdstr)
			}
		}

		return cmdArray
	}

	cgroupVersion := getCgroupVersion()
	blkioPath := getBlkioPath(cgroupVersion)

	log.Infof("pod(volume id %s) qosClass: %s", volumeID, qosClass)
	log.Infof("pod(volume id %s) blkio path: %s", volumeID, blkioPath)
	// get lv lvpath
	// todo: not support device kind
	lvpath, err := ns.createLV(ctx, req)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to get lv path %s: %s", volumeID, err.Error())
	}
	stat := syscall.Stat_t{}
	_ = syscall.Stat(lvpath, &stat)
	maj := uint64(stat.Rdev / 256)
	min := uint64(stat.Rdev % 256)
	log.Infof("volume %s maj:min: %d:%d", volumeID, maj, min)
	log.Infof("volume %s path: %s", volumeID, lvpath)
	if iops > 0 || bps > 0 {
		log.Infof("volume %s iops: %d bps: %d", volumeID, iops, bps)
		cmdArray := makeBlkioCmdstr(cgroupVersion, blkioPath, maj, min, iops, bps)
		for _, cmd := range cmdArray {
			_, err := exec.Command("sh", "-c", cmd).CombinedOutput()
			if err != nil {
				return status.Errorf(codes.Internal, "failed to write blkio file cmd:%s error:%s", cmd, err.Error())
			}
		}
	}

	return nil
}
