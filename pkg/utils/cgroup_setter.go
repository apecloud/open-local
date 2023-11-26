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

package utils

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	// for cgroup v1
	bpsReadFile   = "blkio.throttle.read_bps_device"
	bpsWriteFile  = "blkio.throttle.write_bps_device"
	iopsReadFile  = "blkio.throttle.read_iops_device"
	iopsWriteFile = "blkio.throttle.write_iops_device"

	// for cgroup v2
	ioFile = "io.max"
)

type CgroupSetter interface {
	SetBlkio(major uint64, minor uint64, iops uint64, bps uint64) error
}

func NewCgroupSetter(cgroupVersion string, path string) CgroupSetter {
	switch cgroupVersion {
	case CgroupV1:
		return &cgroupV1Setter{
			path: path,
		}
	case CgroupV2:
		return &cgroupV2Setter{
			path: path,
		}
	default:
		return &unsupportedSetter{}
	}
}

type unsupportedSetter struct{}

func (c *unsupportedSetter) SetBlkio(major uint64, minor uint64, iops uint64, bps uint64) (err error) {
	return fmt.Errorf("unsupported SetBlkio")
}

type cgroupV1Setter struct {
	path string
}

func (c *cgroupV1Setter) SetBlkio(major uint64, minor uint64, iops uint64, bps uint64) (err error) {
	defer func() {
		if obj := recover(); obj != nil {
			err = fmt.Errorf("%s", obj)
		}
	}()
	if iops > 0 {
		mustWriteFile(filepath.Join(c.path, iopsReadFile), fmt.Sprintf("%d:%d %d", major, minor, iops))
		mustWriteFile(filepath.Join(c.path, iopsWriteFile), fmt.Sprintf("%d:%d %d", major, minor, iops))
	}
	if bps > 0 {
		mustWriteFile(filepath.Join(c.path, bpsReadFile), fmt.Sprintf("%d:%d %d", major, minor, bps))
		mustWriteFile(filepath.Join(c.path, bpsWriteFile), fmt.Sprintf("%d:%d %d", major, minor, bps))
	}
	return nil
}

type cgroupV2Setter struct {
	path string
}

func (c *cgroupV2Setter) SetBlkio(major uint64, minor uint64, iops uint64, bps uint64) (err error) {
	defer func() {
		if obj := recover(); obj != nil {
			err = fmt.Errorf("%s", obj)
		}
	}()
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
		mustWriteFile(filepath.Join(c.path, ioFile), fmt.Sprintf("%d:%d %s %s", major, minor, iopsstr, bpsstr))
	}
	return nil
}

func mustWriteFile(path string, content string) {
	err := os.WriteFile(path, []byte(content), 0777)
	if err != nil {
		panic(fmt.Sprintf("write to file %s failed, content: %s, err: %s",
			path, content, err.Error()))
	}
}
