package utils

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

type CgroupDriverType string

const (
	Cgroupfs CgroupDriverType = "cgroupfs"
	Systemd  CgroupDriverType = "systemd"

	KubeRootNameSystemd       = "kubepods.slice/"
	KubeBurstableNameSystemd  = "kubepods-burstable.slice/"
	KubeBesteffortNameSystemd = "kubepods-besteffort.slice/"

	KubeRootNameCgroupfs       = "kubepods/"
	KubeBurstableNameCgroupfs  = "burstable/"
	KubeBesteffortNameCgroupfs = "besteffort/"

	CgroupV1 = "cgroupv1"
	CgroupV2 = "cgroupv2"
)

var (
	CgroupDriverTypes = []CgroupDriverType{Cgroupfs, Systemd}

	QOSClasses = []corev1.PodQOSClass{
		corev1.PodQOSGuaranteed,
		corev1.PodQOSBurstable,
		corev1.PodQOSBestEffort,
	}
)

func (c CgroupDriverType) Validate() bool {
	for _, t := range CgroupDriverTypes {
		if c == t {
			return true
		}
	}
	return false
}

type formatter struct {
	ParentDir string
	QOSDirFn  func(qos corev1.PodQOSClass) string
	PodDirFn  func(qos corev1.PodQOSClass, podUID string) string
	// containerID format: "containerd://..." or "docker://..."
	ContainerDirFn func(id string) (string, error)

	PodIDParser       func(basename string) (string, error)
	ContainerIDParser func(basename string) (string, error)
}

func (f *formatter) GetBlkioPath(cgroupVersion string, cgroupFsPath string, qos corev1.PodQOSClass, podUID string) string {
	switch cgroupVersion {
	case CgroupV1:
		return fmt.Sprintf("%s/blkio/%s%s%s", cgroupFsPath, f.ParentDir, f.QOSDirFn(qos), f.PodDirFn(qos, podUID))
	case CgroupV2:
		return fmt.Sprintf("%s/%s%s%s", cgroupFsPath, f.ParentDir, f.QOSDirFn(qos), f.PodDirFn(qos, podUID))
	}
	return ""
}

var cgroupPathFormatterInSystemd = formatter{
	ParentDir: KubeRootNameSystemd,
	QOSDirFn: func(qos corev1.PodQOSClass) string {
		switch qos {
		case corev1.PodQOSBurstable:
			return KubeBurstableNameSystemd
		case corev1.PodQOSBestEffort:
			return KubeBesteffortNameSystemd
		case corev1.PodQOSGuaranteed:
			return "/"
		}
		return "/"
	},
	PodDirFn: func(qos corev1.PodQOSClass, podUID string) string {
		id := strings.ReplaceAll(podUID, "-", "_")
		switch qos {
		case corev1.PodQOSBurstable:
			return fmt.Sprintf("kubepods-burstable-pod%s.slice/", id)
		case corev1.PodQOSBestEffort:
			return fmt.Sprintf("kubepods-besteffort-pod%s.slice/", id)
		case corev1.PodQOSGuaranteed:
			return fmt.Sprintf("kubepods-pod%s.slice/", id)
		}
		return "/"
	},
	ContainerDirFn: func(id string) (string, error) {
		hashID := strings.Split(id, "://")
		if len(hashID) < 2 {
			return "", fmt.Errorf("parse container id %s failed", id)
		}

		switch hashID[0] {
		case "docker":
			return fmt.Sprintf("docker-%s.scope/", hashID[1]), nil
		case "containerd":
			return fmt.Sprintf("cri-containerd-%s.scope/", hashID[1]), nil
		default:
			return "", fmt.Errorf("unknown container protocol %s", id)
		}
	},
	PodIDParser: func(basename string) (string, error) {
		patterns := []struct {
			prefix string
			suffix string
		}{
			{
				prefix: "kubepods-besteffort-pod",
				suffix: ".slice",
			},
			{
				prefix: "kubepods-burstable-pod",
				suffix: ".slice",
			},

			{
				prefix: "kubepods-pod",
				suffix: ".slice",
			},
		}

		for i := range patterns {
			if strings.HasPrefix(basename, patterns[i].prefix) && strings.HasSuffix(basename, patterns[i].suffix) {
				return basename[len(patterns[i].prefix) : len(basename)-len(patterns[i].suffix)], nil
			}
		}
		return "", fmt.Errorf("fail to parse pod id: %v", basename)
	},
	ContainerIDParser: func(basename string) (string, error) {
		patterns := []struct {
			prefix string
			suffix string
		}{
			{
				prefix: "docker-",
				suffix: ".scope",
			},
			{
				prefix: "cri-containerd-",
				suffix: ".scope",
			},
		}

		for i := range patterns {
			if strings.HasPrefix(basename, patterns[i].prefix) && strings.HasSuffix(basename, patterns[i].suffix) {
				return basename[len(patterns[i].prefix) : len(basename)-len(patterns[i].suffix)], nil
			}
		}
		return "", fmt.Errorf("fail to parse pod id: %v", basename)
	},
}

var cgroupPathFormatterInCgroupfs = formatter{
	ParentDir: KubeRootNameCgroupfs,
	QOSDirFn: func(qos corev1.PodQOSClass) string {
		switch qos {
		case corev1.PodQOSBurstable:
			return KubeBurstableNameCgroupfs
		case corev1.PodQOSBestEffort:
			return KubeBesteffortNameCgroupfs
		case corev1.PodQOSGuaranteed:
			return "/"
		}
		return "/"
	},
	PodDirFn: func(qos corev1.PodQOSClass, podUID string) string {
		return fmt.Sprintf("pod%s/", podUID)
	},
	ContainerDirFn: func(id string) (string, error) {
		hashID := strings.Split(id, "://")
		if len(hashID) < 2 {
			return "", fmt.Errorf("parse container id %s failed", id)
		}
		if hashID[0] == "docker" || hashID[0] == "containerd" {
			return fmt.Sprintf("%s/", hashID[1]), nil
		} else {
			return "", fmt.Errorf("unknown container protocol %s", id)
		}
	},
	PodIDParser: func(basename string) (string, error) {
		if strings.HasPrefix(basename, "pod") {
			return basename[len("pod"):], nil
		}
		return "", fmt.Errorf("fail to parse pod id: %v", basename)
	},
	ContainerIDParser: func(basename string) (string, error) {
		return basename, nil
	},
}

func GetCgroupPathFormatter(driver CgroupDriverType) *formatter {
	switch driver {
	case Systemd:
		return &cgroupPathFormatterInSystemd
	case Cgroupfs:
		return &cgroupPathFormatterInCgroupfs
	default:
		return nil
	}
}
