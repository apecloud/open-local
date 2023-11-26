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
	"fmt"
	"os"
	"path/filepath"
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
	volumeCapacityTag = "_hp_volumeCapacity"
)

// hostPathCsImpl implements the csi.ControllerServer interface for hostPath volume type
type hostPathCsImpl struct {
	baseControllerServer

	// TODO(x.zhou): needs further refactoring to move methods/fields
	//               from controllerServer to baseControllerServer.
	common *controllerServer
}

func (cs *hostPathCsImpl) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if volumeSource := req.GetVolumeContentSource(); volumeSource != nil {
		return nil, status.Error(codes.Unimplemented, "CreateVolume: volume content source is not supported")
	}

	reqCtx := getValue(ctx, ctxKeyCreateVolume)
	nodeName := reqCtx.nodeName
	volumeID := req.GetName()
	parameters := req.GetParameters()

	// verifying the hostPath parameter
	_, _, err := getVerifiedHostPath(volumeID, parameters)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "CreateVolume: getVerifiedHostPath error: %s", err.Error())
	}

	// TODO(x.zhou): interact with the scheduler to see if there is sufficient space to allocate

	parameters[pkg.AnnoSelectedNode] = nodeName
	parameters[volumeCapacityTag] = fmt.Sprintf("%d", req.GetCapacityRange().GetRequiredBytes())
	response := &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeID,
			CapacityBytes: req.GetCapacityRange().GetRequiredBytes(),
			VolumeContext: parameters,
			AccessibleTopology: []*csi.Topology{{
				Segments: map[string]string{
					pkg.KubernetesNodeIdentityKey: nodeName,
				},
			}},
		},
	}
	log.Infof("CreateVolume: create volume %s(size: %d) successfully", volumeID, req.GetCapacityRange().GetRequiredBytes())
	return response, nil
}

func (cs *hostPathCsImpl) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	reqCtx := getValue(ctx, ctxKeyDeleteVolume)
	volumeID := reqCtx.pv.Name
	parameters := reqCtx.pv.Spec.CSI.VolumeAttributes
	_, hostPath, err := getVerifiedHostPath(volumeID, parameters)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "DeleteVolume: getVerifiedHostPath error: %s", err.Error())
	}
	nodeName := reqCtx.nodeName

	conn, err := cs.common.getNodeConn(nodeName)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "DeleteVolume: failed to connect to node %s: %s", nodeName, err.Error())
	}
	defer conn.Close()
	if err := conn.CleanPath(ctx, hostPath, true); err != nil {
		return nil, status.Errorf(codes.Internal, "DeleteVolume: failed to delete hostPath %s: %s", hostPath, err.Error())
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

func (ns *hostPathNsImpl) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (resp *csi.NodePublishVolumeResponse, err error) {
	volumeID := req.GetVolumeId()
	targetPath := req.GetTargetPath()

	basePath, volumeHostPath, err := getVerifiedHostPath(volumeID, req.VolumeContext)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "NodePublishVolume: getVerifiedHostPath error: %s", err.Error())
	}

	// create volume directory on the host
	if err := ns.common.osTool.MkdirAll(volumeHostPath, 0777); err != nil {
		return nil, status.Errorf(codes.Internal, "NodePublishVolume: failed to create directory %s: %s", volumeHostPath, err.Error())
	}
	defer func() {
		// clean up volume directory when error
		if err != nil {
			_ = ns.common.osTool.Remove(volumeHostPath)
		}
	}()

	// set project quota
	setQuotaDone := false
	var setQuotaErr error
	tags := tags(req.VolumeContext)
	capacity, err := tags.GetInt64(volumeCapacityTag)
	if err != nil || capacity <= 0 {
		log.Warningf("NodePublishVolume: invalid tag %s='%s', err: %v",
			volumeCapacityTag, tags[volumeCapacityTag], err)
	} else {
		setQuotaErr = ns.setProjectQuota(ctx, basePath, volumeHostPath, capacity)
		if setQuotaErr != nil {
			log.Warningf("NodePublishVolume: failed to set project quota, err: %s", setQuotaErr)
		} else {
			setQuotaDone = true
		}
	}
	enforceCapacityLimit, _ := tags.GetBool(enforceCapacityLimitScTag)
	if enforceCapacityLimit && !setQuotaDone {
		return nil, status.Errorf(codes.Internal,
			"NodePublishVolume: failed to set project quota, err: %s", setQuotaErr)
	}

	// mount target path
	if err := ns.mountHostPathVolume(ctx, req, volumeHostPath); err != nil {
		return nil, status.Errorf(codes.Internal, "NodePublishVolume: %s", err.Error())
	}
	log.Infof("NodePublishVolume: mount HostPath volume %s to %s successfully", volumeID, targetPath)
	return &csi.NodePublishVolumeResponse{}, nil
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
  xfs_quota -x -c 'project -s -p  {{ .ProjectPath }}' $PID "{{ .BasePath }}"
  xfs_quota -x -c 'limit -p bsoft={{ .SoftLimit }} bhard={{ .HardLimit }}' $PID "{{ .BasePath }}"
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
		SoftLimit, UpperSoftLimit string
		HardLimit, UpperHardLimit string
	}{
		BasePath:       basePath,
		ProjectPath:    volumeHostPath,
		SoftLimit:      limit,
		HardLimit:      limit,
		UpperSoftLimit: strings.ToUpper(limit),
		UpperHardLimit: strings.ToUpper(limit),
	}); err != nil {
		return fmt.Errorf("failed to render template, err: %w", err)
	}

	const (
		lockTimeout  = "30" // seconds
		lockFileName = "setquota.lock"
	)
	lockfile := filepath.Join(basePath, lockFileName)
	out, err := ns.common.osTool.RunCommand("flock", []string{
		"-w", lockTimeout, "-x", lockfile, "-c", strBuilder.String(),
	})
	if err != nil {
		err = fmt.Errorf("setProjectQuota failed, error: %w, output: %s", err, out)
	}
	log.Infof("setProjectQuota: cmd output: %s, err: %v", out, err)
	return err
}

func getVerifiedHostPath(volumeID string, params map[string]string) (string, string, error) {
	basePath := params[hostPathScTag]
	if basePath == "" {
		return "", "", fmt.Errorf("missing %s tag", hostPathScTag)
	}
	if basePathPrefix := os.Getenv(hostPathBasePathEnv); basePathPrefix != "" {
		if !strings.HasPrefix(basePath, basePathPrefix) {
			return "", "", fmt.Errorf("invalid hostPath (%s), it should have prefix of '%s'", basePath, basePathPrefix)
		}
	}
	return basePath, filepath.Join(basePath, volumeID), nil
}
