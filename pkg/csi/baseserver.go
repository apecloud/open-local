package csi

import (
	. "github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
)

var (
	errUnimplemented = status.Errorf(codes.Unimplemented, "unimplemented")
)

// baseControllerServer implements csi.ControllerServer
type baseControllerServer struct{}

var _ ControllerServer = (*baseControllerServer)(nil)

func (*baseControllerServer) CreateVolume(context.Context, *CreateVolumeRequest) (*CreateVolumeResponse, error) {
	return nil, errUnimplemented
}
func (*baseControllerServer) DeleteVolume(context.Context, *DeleteVolumeRequest) (*DeleteVolumeResponse, error) {
	return nil, errUnimplemented
}
func (*baseControllerServer) ControllerPublishVolume(context.Context, *ControllerPublishVolumeRequest) (*ControllerPublishVolumeResponse, error) {
	return nil, errUnimplemented
}
func (*baseControllerServer) ControllerUnpublishVolume(context.Context, *ControllerUnpublishVolumeRequest) (*ControllerUnpublishVolumeResponse, error) {
	return nil, errUnimplemented
}
func (*baseControllerServer) ValidateVolumeCapabilities(context.Context, *ValidateVolumeCapabilitiesRequest) (*ValidateVolumeCapabilitiesResponse, error) {
	return nil, errUnimplemented
}
func (*baseControllerServer) ListVolumes(context.Context, *ListVolumesRequest) (*ListVolumesResponse, error) {
	return nil, errUnimplemented
}
func (*baseControllerServer) GetCapacity(context.Context, *GetCapacityRequest) (*GetCapacityResponse, error) {
	return nil, errUnimplemented
}
func (*baseControllerServer) ControllerGetCapabilities(context.Context, *ControllerGetCapabilitiesRequest) (*ControllerGetCapabilitiesResponse, error) {
	return nil, errUnimplemented
}
func (*baseControllerServer) CreateSnapshot(context.Context, *CreateSnapshotRequest) (*CreateSnapshotResponse, error) {
	return nil, errUnimplemented
}
func (*baseControllerServer) DeleteSnapshot(context.Context, *DeleteSnapshotRequest) (*DeleteSnapshotResponse, error) {
	return nil, errUnimplemented
}
func (*baseControllerServer) ListSnapshots(context.Context, *ListSnapshotsRequest) (*ListSnapshotsResponse, error) {
	return nil, errUnimplemented
}
func (*baseControllerServer) ControllerExpandVolume(context.Context, *ControllerExpandVolumeRequest) (*ControllerExpandVolumeResponse, error) {
	return nil, errUnimplemented
}
func (*baseControllerServer) ControllerGetVolume(context.Context, *ControllerGetVolumeRequest) (*ControllerGetVolumeResponse, error) {
	return nil, errUnimplemented
}

// baseNodeServer implements csi.NodeServer
type baseNodeServer struct{}

var _ NodeServer = (*baseNodeServer)(nil)

func (*baseNodeServer) NodeStageVolume(context.Context, *NodeStageVolumeRequest) (*NodeStageVolumeResponse, error) {
	return nil, errUnimplemented
}
func (*baseNodeServer) NodeUnstageVolume(context.Context, *NodeUnstageVolumeRequest) (*NodeUnstageVolumeResponse, error) {
	return nil, errUnimplemented
}
func (*baseNodeServer) NodePublishVolume(context.Context, *NodePublishVolumeRequest) (*NodePublishVolumeResponse, error) {
	return nil, errUnimplemented
}
func (*baseNodeServer) NodeUnpublishVolume(context.Context, *NodeUnpublishVolumeRequest) (*NodeUnpublishVolumeResponse, error) {
	return nil, errUnimplemented
}
func (*baseNodeServer) NodeGetVolumeStats(context.Context, *NodeGetVolumeStatsRequest) (*NodeGetVolumeStatsResponse, error) {
	return nil, errUnimplemented
}
func (*baseNodeServer) NodeExpandVolume(context.Context, *NodeExpandVolumeRequest) (*NodeExpandVolumeResponse, error) {
	return nil, errUnimplemented
}
func (*baseNodeServer) NodeGetCapabilities(context.Context, *NodeGetCapabilitiesRequest) (*NodeGetCapabilitiesResponse, error) {
	return nil, errUnimplemented
}
func (*baseNodeServer) NodeGetInfo(context.Context, *NodeGetInfoRequest) (*NodeGetInfoResponse, error) {
	return nil, errUnimplemented
}

// Define context structures for each kind of request.

type contextKey[T any] struct {
	string
}

func ctxWithValue[T any](ctx context.Context, key contextKey[T], value *T) context.Context {
	return context.WithValue(ctx, key, value)
}

func getValue[T any](ctx context.Context, key contextKey[T]) *T {
	val, _ := ctx.Value(key).(*T)
	return val
}

var (
	ctxKeyCreateVolume = contextKey[createVolumeContext]{"createVolume"}
	ctxKeyDeleteVolume = contextKey[deleteVolumeContext]{"deleteVolume"}
)

type createVolumeContext struct {
	pvc      *corev1.PersistentVolumeClaim
	nodeName string
}

type deleteVolumeContext struct {
	pv       *corev1.PersistentVolume
	nodeName string
}
