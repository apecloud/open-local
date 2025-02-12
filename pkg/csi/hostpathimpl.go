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

package csi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	log "k8s.io/klog/v2"

	"github.com/alibaba/open-local/pkg"
)

const (
	hostPathBasePathEnv = "HOSTPATH_BASE_PATH"

	// tags from StorageClass
	hostPathScTag             = "hostPath"
	enforceCapacityLimitScTag = "enforceCapacityLimit"

	// internal tags, the prefix is added to avoid conflicts with other public tags
	volumeContextTag = "_apecloud_volume_context"

	lockTimeout = "30" // seconds
)

// hostPathCsImpl implements the csi.ControllerServer interface for hostPath volume type
type hostPathCsImpl struct {
	baseControllerServer

	// TODO(x.zhou): needs further refactoring to move methods/fields
	//               from controllerServer to baseControllerServer.
	common *controllerServer
}

type volumeContext struct {
	HostPath, HostVolumePath string
	NodeName                 string
	VolumeCapacity           int64
	EnforceCapacityLimit     bool
	VolumeBps                uint64
	VolumeIops               uint64
	EnableIOThrottling       bool
}

func (cs *hostPathCsImpl) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if volumeSource := req.GetVolumeContentSource(); volumeSource != nil {
		return nil, status.Error(codes.Unimplemented, "CreateVolume: volume content source is not supported")
	}
	parameters := req.GetParameters()
	var err error
	var (
		volumeID                 string
		hostPath, hostVolumePath string
		nodeName                 string
		volumeCapacity           int64
		enforceCapacityLimit     bool
		volumeBps, volumeIops    uint64
		enableIOThrottling       bool
	)

	volumeID = req.GetName()
	// verifying the hostPath parameter
	hostPath, hostVolumePath, err = getVerifiedHostPath(volumeID, parameters)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "CreateVolume: getVerifiedHostPath error: %s", err.Error())
	}
	nodeName = getValue(ctx, ctxKeyCreateVolume).nodeName
	volumeCapacity = req.GetCapacityRange().GetRequiredBytes()
	if val, ok := parameters[enforceCapacityLimitScTag]; ok {
		enforceCapacityLimit, err = strconv.ParseBool(val)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "CreateVolume: invalid tag in parameters %s: %s", enforceCapacityLimitScTag, val)
		}
	}
	enableIOThrottling, volumeBps, volumeIops, err = requireThrottleIO(parameters)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "CreateVolume: invalid io throttling parameters: %v", err)
	}

	// TODO(x.zhou): interact with the scheduler to see if there is sufficient space to allocate

	vc := &volumeContext{
		HostPath:             hostPath,
		HostVolumePath:       hostVolumePath,
		NodeName:             nodeName,
		VolumeCapacity:       volumeCapacity,
		EnforceCapacityLimit: enforceCapacityLimit,
		VolumeBps:            volumeBps,
		VolumeIops:           volumeIops,
		EnableIOThrottling:   enableIOThrottling,
	}

	ctxStr, err := json.Marshal(vc)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "CreateVolume: failed to marshal volume context: %v", err)
	}

	parameters[volumeContextTag] = string(ctxStr)

	response := &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeID,
			CapacityBytes: volumeCapacity,
			VolumeContext: parameters,
			AccessibleTopology: []*csi.Topology{{
				Segments: map[string]string{
					pkg.KubernetesNodeIdentityKey: nodeName,
				},
			}},
		},
	}
	log.Infof("CreateVolume: create volume %s(size: %d) successfully", volumeID, volumeCapacity)
	return response, nil
}

func (cs *hostPathCsImpl) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	reqCtx := getValue(ctx, ctxKeyDeleteVolume)
	volumeID := reqCtx.pv.Name
	parameters := reqCtx.pv.Spec.CSI.VolumeAttributes
	hostPath, hostVolumePath, err := getVerifiedHostPath(volumeID, parameters)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "DeleteVolume: getVerifiedHostPath error: %s", err.Error())
	}

	cmd := `
set -ex
trap 'rm -rf {{ .ProjectPath }}' EXIT

# fs stores the file system of mount
FS=$(stat -f -c %T "{{ .BasePath }}")
# check if fs is xfs
if [[ "$FS" == "xfs" ]]; then
  PID=$(cat {{ .ProjectIDFile }})
  xfs_quota -x -c "limit -p bsoft=0 bhard=0 $PID" "{{ .BasePath }}"
  rm {{ .ProjectIDFile }}
fi
	`
	tmpl, err := template.New("").Parse(cmd)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "DeleteVolume: failed to parse template, err: %v", err)
	}
	var strBuilder strings.Builder
	if err = tmpl.Execute(&strBuilder, struct {
		BasePath      string
		ProjectPath   string
		ProjectIDFile string
	}{
		BasePath:      hostPath,
		ProjectPath:   hostVolumePath,
		ProjectIDFile: getProjectIDFilePath(hostPath, hostVolumePath),
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "DeleteVolume: failed to render template, err: %v", err)
	}
	cmd = strBuilder.String()
	log.Infof("DeleteVolume: execute command: %s", cmd)
	nodeName := reqCtx.nodeName

	conn, err := cs.common.getNodeConn(nodeName)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "DeleteVolume: failed to connect to node %s: %s", nodeName, err.Error())
	}
	defer conn.Close()

	cmds := []string{"flock", "-w", lockTimeout, "-x", getLockfilePath(hostPath), "-c", cmd}

	out, err := conn.DoCommand(ctx, cmds)
	if err != nil {
		err = status.Errorf(codes.Internal, "DeleteVolume: fail to execute commands, output: %s, err: %v", out, err)
		log.Error(err)
		return nil, err
	}
	log.Infof("DeleteVolume: delete HostPath volume(%s) successfully", volumeID)
	return &csi.DeleteVolumeResponse{}, nil
}

// hostPathNsImpl implements the csi.NodeServer interface for hostPath volume type
type hostPathNsImpl struct {
	baseNodeServer

	// TODO(x.zhou): needs further refactoring
	common *nodeServer
}

func (ns *hostPathNsImpl) getMajorAndMinorNumberOfHostPath(hostPath string) (uint32, uint32, error) {
	// find major and minor device type of the hostPath
	cmd := fmt.Sprintf("findmnt --target %s -o MAJ:MIN -r -n", hostPath)
	output, err := ns.common.osTool.RunShellCommand(cmd)
	if err != nil {
		return 0, 0, fmt.Errorf("exec cmd '%s' failed: %s, output: %s", cmd, err.Error(), output)
	}
	// check if it is a real block device
	majMin := strings.TrimSpace(output)
	_, err = ns.common.osTool.Stat(fmt.Sprintf("%s/dev/block/%s", ns.common.options.sysPath, majMin))
	if os.IsNotExist(err) {
		return 0, 0, fmt.Errorf("the underlay device type for '%s' is %s, but it's not a real block device", hostPath, majMin)
	}
	parts := strings.Split(majMin, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid output of cmd '%s': %s", cmd, majMin)
	}
	majorNumberOfDevice, err1 := strconv.ParseUint(parts[0], 10, 64)
	minorNumberOfDevice, err2 := strconv.ParseUint(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("invalid output of cmd '%s': %s, err1: %v, err2: %v",
			cmd, output, err1, err2)
	}
	return uint32(majorNumberOfDevice), uint32(minorNumberOfDevice), nil
}

func (ns *hostPathNsImpl) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (resp *csi.NodePublishVolumeResponse, err error) {
	volumeID := req.GetVolumeId()
	targetPath := req.GetTargetPath()

	vcStr, ok := req.VolumeContext[volumeContextTag]
	if !ok {
		return nil, status.Errorf(codes.Internal, "NodePublishVolume: can not find volume context")
	}
	vc := new(volumeContext)
	if err := json.Unmarshal([]byte(vcStr), vc); err != nil {
		return nil, status.Errorf(codes.Internal, "NodePublishVolume: fail to unmarshal volume context: %v", err)
	}

	var majorNumberOfDevice, minorNumberOfDevice uint32

	if vc.EnableIOThrottling {
		majorNumberOfDevice, minorNumberOfDevice, err = ns.getMajorAndMinorNumberOfHostPath(vc.HostPath)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "NodePublishVolume: hostpath seems not to be backed by a block device: %v", err)
		}
	}

	// TODO(gufeijun): if io quota is enabled, check if the filesystem of basepath supports quota.

	// create volume directory on the host
	if err := ns.common.osTool.MkdirAll(vc.HostVolumePath, 0777); err != nil {
		return nil, status.Errorf(codes.Internal, "NodePublishVolume: failed to create directory %s: %s", vc.HostVolumePath, err.Error())
	}
	defer func() {
		// clean up volume directory when error
		if err != nil {
			_ = ns.common.osTool.Remove(vc.HostVolumePath)
		}
	}()

	// set project quota
	err = ns.setProjectQuota(ctx, vc.HostPath, vc.HostVolumePath, vc.VolumeCapacity)
	if err != nil && vc.EnforceCapacityLimit {
		log.Errorf("NodePublishVolume: failed to set project quota, err: %s", err)
		return nil, status.Errorf(codes.Internal,
			"NodePublishVolume: failed to set project quota, err: %v", err)
	}

	// set IO throttling
	if vc.EnableIOThrottling {
		err = ns.common.setIOThrottling(ctx, req, vc.VolumeBps, vc.VolumeIops, majorNumberOfDevice, minorNumberOfDevice)
		if err != nil {
			log.Errorf("NodePublishVolume: failed to set IO throttling, err: %s", err)
			return nil, status.Errorf(codes.Internal,
				"NodePublishVolume: failed to set IO throttling, err: %s", err)
		}
	}

	// mount target path
	if err := ns.mountHostPathVolume(ctx, req, vc.HostVolumePath); err != nil {
		return nil, status.Errorf(codes.Internal, "NodePublishVolume: %s", err.Error())
	}
	log.Infof("NodePublishVolume: mount HostPath volume %s to %s successfully", volumeID, targetPath)
	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *hostPathNsImpl) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	enforceCapacityLimit := false
	reqCtx := getValue(ctx, ctxKeyExpandVolume)
	volumeID := reqCtx.pv.Name
	parameters := reqCtx.pv.Spec.CSI.VolumeAttributes
	hostPath, hostVolumePath, err := getVerifiedHostPath(volumeID, parameters)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "NodeExpandVolume: %v", err)
	}
	if val, ok := parameters[enforceCapacityLimitScTag]; ok {
		enforceCapacityLimit, err = strconv.ParseBool(val)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "CreateVolume: invalid tag in parameters %s: %s", enforceCapacityLimitScTag, val)
		}
	}

	cmd := `
set -ex
# fs stores the file system of mount
FS=$(stat -f -c %T "{{ .BasePath }}")
# check if fs is xfs or ext4 (output of stat is ext2/ext3)
if [[ "$FS" == "xfs" ]]; then
  PID=$(cat {{ .ProjectIDFile }})
  xfs_quota -x -c "limit -p bsoft={{ .SoftLimit }} bhard={{ .HardLimit }} $PID" "{{ .BasePath }}"
elif [[ "$FS" == "ext2/ext3" ]]; then
  PID=$(lsattr -p {{ .ProjectPath }} -d | awk '{print $1}')
  setquota -P $PID {{ .UpperSoftLimit }} {{ .UpperHardLimit }} 0 0 "{{ .BasePath }}"
fi
	`
	tmpl, err := template.New("").Parse(cmd)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "NodeExpandVolume: failed to parse template, err: %v", err)
	}
	capacityBytes := (req.CapacityRange.RequiredBytes + 1023) / 1024
	limit := fmt.Sprintf("%dk", capacityBytes)
	var strBuilder strings.Builder
	if err = tmpl.Execute(&strBuilder, struct {
		BasePath                  string
		ProjectPath               string
		ProjectIDFile             string
		SoftLimit, UpperSoftLimit string
		HardLimit, UpperHardLimit string
	}{
		BasePath:       hostPath,
		ProjectPath:    hostVolumePath,
		ProjectIDFile:  getProjectIDFilePath(hostPath, hostVolumePath),
		SoftLimit:      limit,
		HardLimit:      limit,
		UpperSoftLimit: strings.ToUpper(limit),
		UpperHardLimit: strings.ToUpper(limit),
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "NodeExpandVolume: failed to render template, err: %v", err)
	}

	cmd = strBuilder.String()
	log.Infof("NodeExpandVolume: execute command: %s", cmd)
	out, err := ns.common.osTool.RunCommand("flock", []string{
		"-w", lockTimeout, "-x", getLockfilePath(hostPath), "-c", cmd,
	})
	if err != nil && enforceCapacityLimit {
		return nil, status.Errorf(codes.Internal, "NodeExpandVolume: excute command failed, err=%v, output=%v", err, out)
	}
	log.Infof("NodeExpandVolume success")
	return &csi.NodeExpandVolumeResponse{
		CapacityBytes: capacityBytes,
	}, nil
}

func (ns *hostPathNsImpl) mountHostPathVolume(ctx context.Context, req *csi.NodePublishVolumeRequest, hostPath string) error {
	sourcePath := hostPath
	targetPath := req.TargetPath

	notMounted, err := ns.common.k8smounter.IsLikelyNotMountPoint(targetPath)
	if err != nil {
		return fmt.Errorf("mountHostPathVolume: check if targetPath %s is mounted: %s", targetPath, err.Error())
	}
	if !notMounted {
		log.Infof("mountHostPathVolume: volume %s(%s) is already mounted", req.VolumeId, targetPath)
		return nil
	}

	// start to mount
	mnt := req.VolumeCapability.GetMount()
	options := append(mnt.MountFlags, "bind")
	if req.Readonly {
		options = append(options, "ro")
	}
	fsType := "ext4"
	if mnt.FsType != "" {
		fsType = mnt.FsType
	}
	log.Infof("mountHostPathVolume: mount volume %s to %s with flags %v and fsType %s", req.VolumeId, targetPath, options, fsType)
	if err = ns.common.k8smounter.Mount(sourcePath, targetPath, fsType, options); err != nil {
		return fmt.Errorf("mountHostPathVolume: fail to mount %s to %s: %s", sourcePath, targetPath, err.Error())
	}
	return nil
}

func (ns *hostPathNsImpl) setProjectQuota(ctx context.Context, basePath string, volumeHostPath string, capacity int64) error {
	if notMountPoint, err := ns.common.k8smounter.IsLikelyNotMountPoint(basePath); err != nil {
		return fmt.Errorf("setProjectQuota: failed to check if basePath %s is mounted, err: %s", basePath, err.Error())
	} else if notMountPoint {
		return fmt.Errorf("setProjectQuota: basePath %s is not a mount point, unable to set quota", basePath)
	}

	cmd := `
set -ex
# fs stores the file system of mount
FS=$(stat -f -c %T "{{ .BasePath }}")
# check if fs is xfs or ext4 (output of stat is ext2/ext3)
# PID is the last project Id in the directory
# xfs_quota project(xfs) or chattr +P (ext4) initializes project with new project id
# xfs_quota limit(xfs) or repquota (ext4) sets the quota according to limits defined
if [[ "$FS" == "xfs" ]]; then
  PID=$(xfs_quota -x -c 'report -h' "{{ .BasePath }}" | tail -2 | awk 'NR==1{print substr ($1,2)}+0')
  PID=$(expr $PID + 1)
  # save project id
  echo $PID > {{ .ProjectIDFile }}
  xfs_quota -x -c "project -s -p  {{ .ProjectPath }} $PID" "{{ .BasePath }}"
  xfs_quota -x -c "limit -p bsoft={{ .SoftLimit }} bhard={{ .HardLimit }} $PID" "{{ .BasePath }}"
elif [[ "$FS" == "ext2/ext3" ]]; then
  PID=$(repquota -P "{{ .BasePath }}" | tail -3 | awk 'NR==1{print substr ($1,2)}+0')
  PID=$(expr $PID + 1)
  chattr +P -p $PID "{{ .ProjectPath }}"
  setquota -P $PID {{ .UpperSoftLimit }} {{ .UpperHardLimit }} 0 0 "{{ .BasePath }}"
fi
    `
	tmpl, err := template.New("").Parse(cmd)
	if err != nil {
		return fmt.Errorf("failed to parse template, err: %w", err)
	}
	capacityKB := (capacity + 1023) / 1024
	limit := fmt.Sprintf("%dk", capacityKB)
	var strBuilder strings.Builder
	if err = tmpl.Execute(&strBuilder, struct {
		BasePath                  string
		ProjectPath               string
		ProjectIDFile             string
		SoftLimit, UpperSoftLimit string
		HardLimit, UpperHardLimit string
	}{
		BasePath:       basePath,
		ProjectPath:    volumeHostPath,
		ProjectIDFile:  getProjectIDFilePath(basePath, volumeHostPath),
		SoftLimit:      limit,
		HardLimit:      limit,
		UpperSoftLimit: strings.ToUpper(limit),
		UpperHardLimit: strings.ToUpper(limit),
	}); err != nil {
		return fmt.Errorf("failed to render template, err: %w", err)
	}

	out, err := ns.common.osTool.RunCommand("flock", []string{
		"-w", lockTimeout, "-x", getLockfilePath(basePath), "-c", strBuilder.String(),
	})
	if err != nil {
		err = fmt.Errorf("setProjectQuota failed, error: %w, output: %s", err, out)
		log.Errorf("setProjectQuota failed, err: %v, output: %s", err)
	}
	return err
}

func getVerifiedHostPath(volumeID string, params map[string]string) (string, string, error) {
	hostPath := params[hostPathScTag]
	if hostPath == "" {
		return "", "", fmt.Errorf("missing %s tag", hostPathScTag)
	}
	if hostPathPrefix := os.Getenv(hostPathBasePathEnv); hostPathPrefix != "" {
		if !strings.HasPrefix(hostPath, hostPathPrefix) {
			return "", "", fmt.Errorf("invalid hostPath (%s), it should have prefix of '%s'", hostPath, hostPathPrefix)
		}
	}
	return hostPath, filepath.Join(hostPath, volumeID), nil
}

func getLockfilePath(basepath string) string {
	return filepath.Join(basepath, ".setquota.lock")
}

func getProjectIDFilePath(basepath string, projectPath string) string {
	return filepath.Join(basepath, fmt.Sprintf(".apecloud_project_%s", filepath.Base(projectPath)))
}
