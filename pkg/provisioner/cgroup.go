package provisioner

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"text/template"

	"github.com/alibaba/open-local/pkg/provisioner/internal/hostpath"
	"k8s.io/klog/v2"
)

func getIOLimitDesc(limit int) string {
	if limit == 0 {
		return "max"
	}
	return strconv.Itoa(int(limit))
}

func (p *Provisioner) createIOLimitPod(ctx context.Context, pOpts *HelperPodOptions) error {
	wbps, err := strconv.Atoi(pOpts.wbps)
	if err != nil {
		return fmt.Errorf("invalid wbps: %s", pOpts.wbps)
	}
	wiops, err := strconv.Atoi(pOpts.wiops)
	if err != nil {
		return fmt.Errorf("invalid wiops: %s", pOpts.wiops)
	}
	rbps, err := strconv.Atoi(pOpts.rbps)
	if err != nil {
		return fmt.Errorf("invalid rbps: %s", pOpts.rbps)
	}
	riops, err := strconv.Atoi(pOpts.riops)
	if err != nil {
		return fmt.Errorf("invalid riops: %s", pOpts.riops)
	}
	if wbps <= 0 && riops <= 0 && rbps <= 0 && wiops <= 0 {
		return nil
	}
	parentDir, volumeDir, err := hostpath.NewBuilder().WithPath(pOpts.path).
		WithCheckf(hostpath.IsNonRoot(), "volume directory {%v} should not be under root directory", pOpts.path).
		WithCheckf(notReservedVolumeDir(), "volume directory is a reserved name").
		ExtractSubPath()
	if err != nil {
		return err
	}
	config := podConfig{
		pOpts:     pOpts,
		podName:   "cgroup_io_limit",
		parentDir: parentDir,
		volumeDir: volumeDir,
		taints:    pOpts.selectedNodeTaints,
	}

	// TODO(gufeijun) support both cgroup v1 and v2, for now noly v2 is supported
	cmd := `
		#!/bin/sh

        cgroup2_root=$(awk '$1 == "cgroup2" {print $2}' /proc/mounts)
		cgroup1_root=$(awk '$1 == "cgroup" {print $2}' /proc/mounts)

		if [ -z $cgroup2_root ] && [ -z $cgroup1_root ]; then
			echo "can not find any mounted cgroup"
			exit 1
		fi

		# get device major and minor version
		device_file=$(df -P "/data" | awk 'NR==2 {print $1}')
		major=$(stat -c "%t" "$device_file")
		minor=$(stat -c "%T" "$device_file")
		if [ $major -eq 0 ] && [ $minor -eq 0 ]; then
		    echo "can not get major and minor version of device."
		    exit 1
		fi

		# prefer cgroup v2
		if [ -n $cgroup2_root ]; then
            echo "$major:$minor wbps={{ .WBPS }} wiops={{ .WIOPS }} rbps={{ .RBPS }} riops={{ .RIOPS }}" \
              > ${cgroup2_root}/{{ .CgroupDir }}/io.max
		else	
			echo "not implemented yet"
	        exit 1
		fi
	`
	tmpl, err := template.New("").Parse(cmd)
	if err != nil {
		return err
	}
	var strBuilder strings.Builder
	if err = tmpl.Execute(&strBuilder, struct {
		WBPS, WIOPS string
		RBPS, RIOPS string
		CgroupDir   string
	}{
		WBPS:  getIOLimitDesc(wbps),
		WIOPS: getIOLimitDesc(wiops),
		RBPS:  getIOLimitDesc(rbps),
		RIOPS: getIOLimitDesc(riops),
		// TODO(gufeijun)
		CgroupDir: "",
	}); err != nil {
		return err
	}
	cmd = strBuilder.String()
	klog.Infof("io limit pod commands: %s", cmd)
	config.pOpts.cmdsForPath = []string{"sh", "-c", cmd}

	qPod, err := p.launchPod(ctx, config)
	if err != nil {
		return err
	}
	return p.exitPod(ctx, qPod)
}
