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
	"slices"
	"strconv"
	"strings"

	errors "github.com/pkg/errors"
	yaml "gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	hostpath "github.com/alibaba/open-local/pkg/provisioner/internal/hostpath"
)

const (
	//KeyPVBasePath defines base directory for hostpath volumes
	// can be configured via the StorageClass annotations.
	KeyPVBasePath = "BasePath"

	//KeyNodeAffinityLabels defines the label keys that should be
	//used in the nodeAffinitySpec.
	//
	//Example:
	//
	// kind: StorageClass
	// metadata:
	//   name: local-device
	//   annotations:
	//     local.csi.aliyun.com/config: |
	//       - name: NodeAffinityLabels
	//         list:
	//           - "topology.kubernetes.io/zone"
	//           - "topology.kubernetes.io/region"
	KeyNodeAffinityLabels = "NodeAffinityLabels"

	//KeyXFSQuota enables/sets parameters for XFS Quota.
	// Example StorageClass snippet:
	//    - name: XFSQuota
	//      enabled: true
	//      data:
	//        softLimitGrace: "80%"
	//        hardLimitGrace: "85%"
	KeyXFSQuota = "XFSQuota"

	//KeyEXT4Quota enables/sets parameters for EXT4 Quota.
	// Example StorageClass snippet:
	//    - name: EXT4Quota
	//      enabled: true
	//      data:
	//        softLimitGrace: "80%"
	//        hardLimitGrace: "85%"
	KeyEXT4Quota = "EXT4Quota"

	KeyQuotaSoftLimit = "softLimitGrace"
	KeyQuotaHardLimit = "hardLimitGrace"

	// TODO(x.zhou): change the domain
	ConfigAnnoKey = "local.csi.aliyun.com/config"
)

const (
	// k8sNodeLabelKeyHostname is the label key used by Kubernetes
	// to store the hostname on the node resource.
	k8sNodeLabelKeyHostname = "kubernetes.io/hostname"
)

const (
	// reserved lockfile name for incrementing project id serially
	// volumeDir should never be same with this.
	lockfileForProjectID = "openlocal_set_quota.lock"
)

// GetVolumeConfig creates a new VolumeConfig struct by
// parsing and merging the configuration provided in the PVC
// annotation - local.csi.aliyun.com/config with the
// default configuration of the provisioner.
func (p *Provisioner) GetVolumeConfig(ctx context.Context, pvName string, pvc *corev1.PersistentVolumeClaim) (*VolumeConfig, error) {

	pvConfig := p.defaultConfig

	//Fetch the SC
	scName := GetStorageClassName(pvc)
	sc, err := p.kubeClient.StorageV1().StorageClasses().Get(ctx, *scName, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get storageclass: missing sc name {%v}", scName)
	}

	// extract and merge the cas config from storageclass
	scCASConfigStr := sc.ObjectMeta.Annotations[string(ConfigAnnoKey)]
	klog.V(4).Infof("SC %v has config:%v", *scName, scCASConfigStr)
	if len(strings.TrimSpace(scCASConfigStr)) != 0 {
		scCASConfig, err := UnMarshallToConfig(scCASConfigStr)
		if err == nil {
			pvConfig = MergeConfig(scCASConfig, pvConfig)
		} else {
			return nil, errors.Wrapf(err, "failed to get config: invalid sc config {%v}", scCASConfigStr)
		}
	}

	//TODO : extract and merge the cas volume config from pvc
	//This block can be added once validation checks are added
	// as to the type of config that can be passed via PVC
	//pvcCASConfigStr := pvc.ObjectMeta.Annotations[string(mconfig.CASConfigKey)]
	//if len(strings.TrimSpace(pvcCASConfigStr)) != 0 {
	//	pvcCASConfig, err := cast.UnMarshallToConfig(pvcCASConfigStr)
	//	if err == nil {
	//		pvConfig = cast.MergeConfig(pvcCASConfig, pvConfig)
	//	}
	//}

	pvConfigMap, err := ConfigToMap(pvConfig)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to read volume config: pvc {%v}", pvc.ObjectMeta.Name)
	}

	dataPvConfigMap, err := dataConfigToMap(pvConfig)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to read volume config: pvc {%v}", pvc.ObjectMeta.Name)
	}

	listPvConfigMap, err := listConfigToMap(pvConfig)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to read volume config: pvc {%v}", pvc.ObjectMeta.Name)
	}

	c := &VolumeConfig{
		pvName:     pvName,
		pvcName:    pvc.ObjectMeta.Name,
		scName:     *scName,
		options:    pvConfigMap,
		configData: dataPvConfigMap,
		configList: listPvConfigMap,
	}
	return c, nil
}

// GetNodeAffinityLabelKey returns the custom node affinity
// label keys as configured in StorageClass.
//
// Default is nil.
func (c *VolumeConfig) GetNodeAffinityLabelKeys() []string {
	nodeAffinityLabelKeys := c.getList(KeyNodeAffinityLabels)
	if nodeAffinityLabelKeys == nil {
		return nil
	}
	return nodeAffinityLabelKeys
}

// GetPath returns a valid PV path based on the configuration
// or an error. The Path is constructed using the following rules:
// If AbsolutePath is specified return it. (Future)
// If PVPath is specified, suffix it with BasePath and return it. (Future)
// If neither of above are specified, suffix the PVName to BasePath
//
//	and return it
//
// Also before returning the path, validate that path is safe
//
//	and matches the filters specified in StorageClass.
func (c *VolumeConfig) GetPath() (string, error) {
	//This feature need to be supported with some more
	// security checks are in place, so that rouge pods
	// don't get access to node directories.
	//absolutePath := c.getValue(KeyPVAbsolutePath)
	//if len(strings.TrimSpace(absolutePath)) != 0 {
	//	return c.validatePath(absolutePath)
	//}

	basePath := c.getValue(KeyPVBasePath)
	if strings.TrimSpace(basePath) == "" {
		return "", errors.Errorf("failed to get path: base path is empty")
	}

	//This feature need to be supported after the
	// security checks are in place.
	//pvRelPath := c.getValue(KeyPVRelativePath)
	//if len(strings.TrimSpace(pvRelPath)) == 0 {
	//	pvRelPath = c.pvName
	//}

	pvRelPath := c.pvName
	//path := filepath.Join(basePath, pvRelPath)

	return hostpath.NewBuilder().
		WithPathJoin(basePath, pvRelPath).
		WithCheckf(hostpath.IsNonRoot(), "path should not be a root directory: %s/%s", basePath, pvRelPath).
		ValidateAndBuild()
}

func (c *VolumeConfig) IsXfsQuotaEnabled() bool {
	xfsQuotaEnabled := c.getEnabled(KeyXFSQuota)
	xfsQuotaEnabled = strings.TrimSpace(xfsQuotaEnabled)

	enableXfsQuotaBool, err := strconv.ParseBool(xfsQuotaEnabled)
	// Default case
	// this means that we have hit either of the two cases below:
	//     i. The value was something other than a straightforward
	//        true or false
	//    ii. The value was empty
	if err != nil {
		return false
	}

	return enableXfsQuotaBool
}

func (c *VolumeConfig) IsExt4QuotaEnabled() bool {
	ext4QuotaEnabled := c.getEnabled(KeyEXT4Quota)
	ext4QuotaEnabled = strings.TrimSpace(ext4QuotaEnabled)

	enableExt4QuotaBool, err := strconv.ParseBool(ext4QuotaEnabled)
	// Default case
	// this means that we have hit either of the two cases below:
	//     i. The value was something other than a straightforward
	//        true or false
	//    ii. The value was empty
	if err != nil {
		return false
	}

	return enableExt4QuotaBool
}

// getValue is a utility function to extract the value
// of the `key` from the ConfigMap object - which is
// map[string]interface{map[string][string]}
// Example:
//
//	{
//	    key1: {
//	            value: value1
//	            enabled: true
//	          }
//	}
//
// In the above example, if `key1` is passed as input,
//
//	`value1` will be returned.
func (c *VolumeConfig) getValue(key string) string {
	if configObj, ok := GetNestedField(c.options, key).(map[string]string); ok {
		if val, p := configObj["value"]; p {
			return val
		}
	}
	return ""
}

// Similar to getValue() above. Returns value of the
// 'Enabled' parameter.
func (c *VolumeConfig) getEnabled(key string) string {
	if configObj, ok := GetNestedField(c.options, key).(map[string]string); ok {
		if val, p := configObj["enabled"]; p {
			return val
		}
	}
	return ""
}

// This is similar to getValue() and getEnabled().
// This gets the value for a specific
// 'Data' parameter key-value pair.
func (c *VolumeConfig) getDataField(key string, dataKey string) string {
	if configData, ok := GetNestedField(c.configData, key).(map[string]string); ok {
		if val, p := configData[dataKey]; p {
			return val
		}
	}
	//Default case
	return ""
}

// This gets the list of values for the 'List' parameter.
func (c *VolumeConfig) getList(key string) []string {
	if listValues, ok := GetNestedField(c.configList, key).([]string); ok {
		return listValues
	}
	//Default case
	return nil
}

// GetStorageClassName extracts the StorageClass name from PVC
func GetStorageClassName(pvc *corev1.PersistentVolumeClaim) *string {
	return pvc.Spec.StorageClassName
}

// GetNodeHostname extracts the Hostname from the labels on the Node
// If hostname label `kubernetes.io/hostname` is not present
// an empty string is returned.
func GetNodeHostname(n *corev1.Node) string {
	hostname, found := n.Labels[k8sNodeLabelKeyHostname]
	if !found {
		return ""
	}
	return hostname
}

// GetNodeLabelValue extracts the value from the given label on the Node
// If specificed label is not present an empty string is returned.
func GetNodeLabelValue(n *corev1.Node, labelKey string) string {
	labelValue, found := n.Labels[labelKey]
	if !found {
		return ""
	}
	return labelValue
}

// GetTaints extracts the Taints from the Spec on the node
// If Taints are empty, it just returns empty structure of corev1.Taints
func GetTaints(n *corev1.Node) []corev1.Taint {
	return n.Spec.Taints
}

// GetImagePullSecrets  parse image pull secrets from env
// transform  string to corev1.LocalObjectReference
// multiple secrets are separated by commas
func GetImagePullSecrets(s string) []corev1.LocalObjectReference {
	s = strings.TrimSpace(s)
	list := make([]corev1.LocalObjectReference, 0)
	if len(s) == 0 {
		return list
	}
	arr := strings.Split(s, ",")
	for _, item := range arr {
		if len(item) > 0 {
			l := corev1.LocalObjectReference{Name: strings.TrimSpace(item)}
			list = append(list, l)
		}
	}
	return list
}

// UnMarshallToConfig un-marshals the provided
// cas template config in a yaml string format
// to a typed list of cas template config
func UnMarshallToConfig(config string) (configs []Config, err error) {
	err = yaml.Unmarshal([]byte(config), &configs)
	return
}

// MergeConfig will merge configuration fields
// from lowPriority that are not present in
// highPriority configuration and return the
// resulting config
func MergeConfig(highPriority, lowPriority []Config) (final []Config) {
	var book []string
	for _, h := range highPriority {
		final = append(final, h)
		book = append(book, strings.TrimSpace(h.Name))
	}
	for _, l := range lowPriority {
		// include only if the config was not present
		// earlier in high priority configuration
		if !slices.Contains(book, strings.TrimSpace(l.Name)) {
			final = append(final, l)
		}
	}
	return
}

// ConfigToMap transforms CAS template config type
// to a nested map
func ConfigToMap(all []Config) (m map[string]interface{}, err error) {
	var configName string
	m = map[string]interface{}{}
	for _, config := range all {
		configName = strings.TrimSpace(config.Name)
		if len(configName) == 0 {
			err = errors.Errorf("failed to transform cas config to map: missing config name: %s", config)
			return nil, err
		}
		confHierarchy := map[string]interface{}{
			configName: map[string]string{
				"enabled": config.Enabled,
				"value":   config.Value,
			},
		}
		isMerged := MergeMapOfObjects(m, confHierarchy)
		if !isMerged {
			err = errors.Errorf("failed to transform cas config to map: failed to merge: %s", config)
			return nil, err
		}
	}
	return
}

// MergeMapOfObjects will merge the map from src to dest. It will override
// existing keys of the destination
func MergeMapOfObjects(dest map[string]interface{}, src map[string]interface{}) bool {
	// nil check as storing into a nil map panics
	if dest == nil {
		return false
	}

	for k, v := range src {
		dest[k] = v
	}

	return true
}

// GetNestedField returns a nested field from the provided map
func GetNestedField(obj map[string]interface{}, fields ...string) interface{} {
	var val interface{} = obj
	for _, field := range fields {
		if _, ok := val.(map[string]interface{}); !ok {
			return nil
		}
		val = val.(map[string]interface{})[field]
	}
	return val
}

func dataConfigToMap(pvConfig []Config) (map[string]interface{}, error) {
	m := map[string]interface{}{}

	for _, configObj := range pvConfig {
		//No Data Parameter
		if configObj.Data == nil {
			continue
		}

		configName := strings.TrimSpace(configObj.Name)
		confHierarchy := map[string]interface{}{
			configName: configObj.Data,
		}
		isMerged := MergeMapOfObjects(m, confHierarchy)
		if !isMerged {
			return nil, errors.Errorf("failed to transform cas config 'Data' for configName '%s' to map: failed to merge: %s", configName, configObj)
		}
	}

	return m, nil
}

func listConfigToMap(pvConfig []Config) (map[string]interface{}, error) {
	m := map[string]interface{}{}

	for _, configObj := range pvConfig {
		//No List Parameter
		if len(configObj.List) == 0 {
			continue
		}

		configName := strings.TrimSpace(configObj.Name)
		confHierarchy := map[string]interface{}{
			configName: configObj.List,
		}
		isMerged := MergeMapOfObjects(m, confHierarchy)
		if !isMerged {
			return nil, errors.Errorf("failed to transform cas config 'List' for configName '%s' to map: failed to merge: %s", configName, configObj)
		}
	}

	return m, nil
}
