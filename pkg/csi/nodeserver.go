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
	"encoding/json"
	"fmt"
	"os"
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	log "k8s.io/klog/v2"
	mountutils "k8s.io/mount-utils"
	utilexec "k8s.io/utils/exec"
)

type nodeServer struct {
	k8smounter           *mountutils.SafeFormatAndMount
	ephemeralVolumeStore Store
	inFlight             *InFlight
	osTool               OSTool

	options       *driverOptions
	cgroupVersion string
	cgroupDriver  utils.CgroupDriverType
}

func newNodeServer(options *driverOptions) *nodeServer {
	store, err := NewVolumeStore(DefaultEphemeralVolumeDataFilePath)
	if err != nil {
		log.Fatalf("fail to initialize ephemeral volume store: %s", err.Error())
	}

	ns := &nodeServer{
		k8smounter: &mountutils.SafeFormatAndMount{
			Interface: mountutils.New(""),
			Exec:      utilexec.New(),
		},
		ephemeralVolumeStore: store,
		inFlight:             NewInFlight(),
		osTool:               NewOSTool(),
		options:              options,
	}

	ns.cgroupVersion = ns.detectCgroupVersion()
	ns.cgroupDriver = ns.detectCgroupDriver()

	log.Infof("detected cgroup version: %s, driver: %s", ns.cgroupVersion, ns.cgroupDriver)

	return ns
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
		var devicePath string
		var err error
		switch volCap.GetAccessType().(type) {
		case *csi.VolumeCapability_Block:
			devicePath, err = ns.mountLvmBlock(ctx, req)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "NodePublishVolume(mountLvmBlock): fail to mount lvm volume %s with path %s: %s", volumeID, targetPath, err.Error())
			}
		case *csi.VolumeCapability_Mount:
			devicePath, err = ns.mountLvmFS(ctx, req)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "NodePublishVolume(mountLvmFS): fail to mount lvm volume %s with path %s: %s", volumeID, targetPath, err.Error())
			}
		}
		stat := syscall.Stat_t{}
		err = syscall.Stat(devicePath, &stat)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "NodePublishVolume: fail to stat device path volume %s with path %s, error: %s", volumeID, devicePath, err.Error())
		}
		maj := stat.Rdev / 256
		min := stat.Rdev % 256
		if err := ns.setIOThrottling(ctx, req, uint64(maj), uint64(min)); err != nil {
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

	return ns.resizeVolume(ctx, req, volumeID, targetPath)
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

func (ns *nodeServer) resizeVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest, volumeID, targetPath string) (*csi.NodeExpandVolumeResponse, error) {
	// Get volumeType
	volumeType := string(pkg.VolumeTypeLVM)
	_, _, pv, err := getPvInfo(ns.options.kubeclient, volumeID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "ExpandVolume failed: %v", err)
	}
	if pv != nil && pv.Spec.CSI != nil {
		if value, ok := pv.Spec.CSI.VolumeAttributes["volumeType"]; ok {
			volumeType = value
		}
	} else {
		return nil, status.Errorf(codes.Internal, "ExpandVolume: local volume get pv info error %s", volumeID)
	}

	switch volumeType {
	case string(pkg.VolumeTypeLVM):
		// Get lvm info
		vgName := utils.GetVGNameFromCsiPV(pv)
		if vgName == "" {
			return nil, status.Errorf(codes.Internal, "ExpandVolume: Volume %s with vgname empty", pv.Name)
		}

		devicePath := filepath.Join("/dev", vgName, volumeID)
		log.Infof("NodeExpandVolume:: volumeId: %s, devicePath: %s", volumeID, devicePath)

		ok, err := ns.osTool.ResizeFS(devicePath, targetPath)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "NodeExpandVolume: Lvm Resize Error, volumeId: %s, devicePath: %s, volumePath: %s, err: %s", volumeID, devicePath, targetPath, err.Error())
		}
		if !ok {
			return nil, status.Errorf(codes.Internal, "NodeExpandVolume:: Lvm Resize failed, volumeId: %s, devicePath: %s, volumePath: %s", volumeID, devicePath, targetPath)
		}
		log.Infof("NodeExpandVolume:: lvm resizefs successful volumeId: %s, devicePath: %s, volumePath: %s", volumeID, devicePath, targetPath)
	case string(pkg.VolumeTypeHostPath):
		reqCtx := &expandVolumeContext{
			pv: pv,
		}
		ctx = ctxWithValue(ctx, ctxKeyExpandVolume, reqCtx)
		impl := &hostPathNsImpl{common: ns}
		return impl.NodeExpandVolume(ctx, req)
	}
	return &csi.NodeExpandVolumeResponse{}, nil
}

// Detect cgroup version by using stat cmd
func (ns *nodeServer) detectCgroupVersion() string {
	cgroupVersion := utils.CgroupV1
	// https://kubernetes.io/docs/concepts/architecture/cgroups/#check-cgroup-version
	cmd := fmt.Sprintf("stat -fc %%T %s/fs/cgroup", ns.options.sysPath)
	output, err := ns.osTool.RunShellCommand(cmd)
	if err != nil {
		log.Errorf("detectCgroupVersion exec cmd '%s' failed: %s, output: %s, falling back to use %s",
			cmd, err.Error(), output, cgroupVersion)
		return cgroupVersion
	}
	// For cgroup v2, the output is cgroup2fs.
	// For cgroup v1, the output is tmpfs.
	if strings.Contains(output, "tmpfs") {
		cgroupVersion = utils.CgroupV1
	} else if strings.Contains(output, "cgroup2fs") {
		cgroupVersion = utils.CgroupV2
	} else {
		log.Errorf("detectCgroupVersion unrecognized device type: %s, falling back to use %s",
			output, cgroupVersion)
	}
	return cgroupVersion
}

// Detect cgroup driver by searching the group path of the current Pod
// in the cgroup directory tree
func (ns *nodeServer) detectCgroupDriver() utils.CgroupDriverType {
	var candidates []utils.CgroupDriverType
	for _, driver := range utils.CgroupDriverTypes {
		f := utils.GetCgroupPathFormatter(driver)
		if f == nil {
			log.Warningf("no CgroupPathFormatter available for driver %s", driver)
			continue
		}
		for _, qosClass := range utils.QOSClasses {
			// Pattern of cgroupv1: /sys/fs/cgroup/$(resType)/$(parentDir)/$(qosDir)/$(podDir)/...
			// Pattern of cgroupv2: /sys/fs/cgroup/$(parentDir)/$(qosDir)/$(podDir)/...
			// So the common pattern is $(parentDir)/$(qosDir)/$(podDir)
			commonPattern := f.ParentDir + f.QOSDirFn(qosClass) + f.PodDirFn(qosClass, ns.options.selfPodUID)
			cmd := fmt.Sprintf(`find %s/fs/cgroup -maxdepth 4 -type d | grep -q "%s"; echo "$?"`,
				ns.options.sysPath, commonPattern)
			output, err := ns.osTool.RunShellCommand(cmd)
			if err != nil {
				log.Errorf("detectCgroupDriver exec cmd '%s' failed: %s, output: %s",
					cmd, err.Error(), output)
				continue
			}
			if strings.TrimSpace(output) == "0" {
				candidates = append(candidates, driver)
				break
			}
		}
	}
	cgroupDriver := utils.Systemd
	if suggested := utils.CgroupDriverType(ns.options.driverName); suggested.Validate() {
		cgroupDriver = suggested
	}
	if len(candidates) != 1 {
		log.Warningf("detectCgroupDriver cgroup driver candidate (%q) is not unique, "+
			"falling back to use \"%s\" as suggested", candidates, cgroupDriver)
	} else {
		cgroupDriver = candidates[0]
	}
	return cgroupDriver
}

func (ns *nodeServer) setIOThrottling(ctx context.Context, req *csi.NodePublishVolumeRequest, maj, min uint64) (err error) {
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

	// get pod
	podUID := req.VolumeContext[pkg.PodUID]
	podName := req.VolumeContext[pkg.PodName]
	podNamespace := req.VolumeContext[pkg.PodNamespace]
	if podUID == "" || podName == "" || podNamespace == "" {
		return fmt.Errorf("pod uid, name or namespace is empty, please make sure CSIDriver.spec.podInfoOnMount is set to true")
	}
	log.Infof("pod(volume id %s) %s/%s uid: %s", volumeID, podNamespace, podName, podUID)
	// set ResourceVersion to 0
	// https://arthurchiao.art/blog/k8s-reliability-list-data-zh/
	pod, err := ns.options.kubeclient.CoreV1().Pods(podNamespace).Get(ctx, podName, metav1.GetOptions{ResourceVersion: "0"})
	if err != nil {
		return fmt.Errorf("failed to get pod %s/%s, error: %s", podNamespace, podName, err.Error())
	}

	// get blkioPath
	qosClass := pod.Status.QOSClass
	cgroupVersion := ns.cgroupVersion
	formatter := utils.GetCgroupPathFormatter(ns.cgroupDriver)
	blkioPath := formatter.GetBlkioPath(cgroupVersion, fmt.Sprintf("%s/fs/cgroup", ns.options.sysPath), qosClass, podUID)
	if blkioPath == "" {
		return fmt.Errorf("failed to get blkio path for cgroup version %s", cgroupVersion)
	}
	log.Infof("pod(volume id %s) blkio path: %s", volumeID, blkioPath)

	if iops > 0 || bps > 0 {
		log.Infof("volume %s maj:min: %d:%d iops: %d bps: %d", volumeID, maj, min, iops, bps)
		setter := utils.NewCgroupSetter(cgroupVersion, blkioPath)
		err := setter.SetBlkio(maj, min, uint64(iops), uint64(bps))
		if err != nil {
			return status.Errorf(codes.Internal, "set blkio error:%s", err.Error())
		}
	}

	return nil
}
