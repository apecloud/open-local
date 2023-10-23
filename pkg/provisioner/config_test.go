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
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestGetImagePullSecrets(t *testing.T) {
	testCases := map[string]struct {
		value         string
		expectedValue []corev1.LocalObjectReference
	}{
		"empty variable": {
			value:         "",
			expectedValue: []corev1.LocalObjectReference{},
		},
		"single value": {
			value:         "image-pull-secret",
			expectedValue: []corev1.LocalObjectReference{{Name: "image-pull-secret"}},
		},
		"multiple value": {
			value:         "image-pull-secret,secret-1",
			expectedValue: []corev1.LocalObjectReference{{Name: "image-pull-secret"}, {Name: "secret-1"}},
		},
		"whitespaces": {
			value:         " ",
			expectedValue: []corev1.LocalObjectReference{},
		},
		"single value with whitespaces": {
			value:         " docker-secret ",
			expectedValue: []corev1.LocalObjectReference{{Name: "docker-secret"}},
		},
		"multiple value with whitespaces": {
			value:         " docker-secret, image-pull-secret ",
			expectedValue: []corev1.LocalObjectReference{{Name: "docker-secret"}, {Name: "image-pull-secret"}},
		},
	}
	for k, v := range testCases {
		v := v
		t.Run(k, func(t *testing.T) {
			actualValue := GetImagePullSecrets(v.value)
			if !reflect.DeepEqual(actualValue, v.expectedValue) {
				t.Errorf("expected %s got %s", v.expectedValue, actualValue)
			}
		})
	}
}

func TestDataConfigToMap(t *testing.T) {
	hostpathConfig := Config{Name: "StorageType", Value: "hostpath"}
	xfsQuotaConfig := Config{Name: "XFSQuota", Enabled: "true",
		Data: map[string]string{
			"SoftLimitGrace": "20%",
			"HardLimitGrace": "80%",
		},
	}

	testCases := map[string]struct {
		config        []Config
		expectedValue map[string]interface{}
	}{
		"nil 'Data' map": {
			config: []Config{hostpathConfig, xfsQuotaConfig},
			expectedValue: map[string]interface{}{
				"XFSQuota": map[string]string{
					"SoftLimitGrace": "20%",
					"HardLimitGrace": "80%",
				},
			},
		},
	}

	for k, v := range testCases {
		v := v
		k := k
		t.Run(k, func(t *testing.T) {
			actualValue, err := dataConfigToMap(v.config)
			if err != nil {
				t.Errorf("expected error to be nil, but got %v", err)
			}
			if !reflect.DeepEqual(actualValue, v.expectedValue) {
				t.Errorf("expected %v, but got %v", v.expectedValue, actualValue)
			}
		})
	}
}

func Test_listConfigToMap(t *testing.T) {
	tests := map[string]struct {
		pvConfig      []Config
		expectedValue map[string]interface{}
		wantErr       bool
	}{
		"Valid list parameter": {
			pvConfig: []Config{
				{Name: "StorageType", Value: "hostpath"},
				{Name: "NodeAffinityLabels", List: []string{"fake-node-label-key"}},
			},
			expectedValue: map[string]interface{}{
				"NodeAffinityLabels": []string{"fake-node-label-key"},
			},
			wantErr: false,
		},
	}
	for k, v := range tests {
		t.Run(k, func(t *testing.T) {
			got, err := listConfigToMap(v.pvConfig)
			if (err != nil) != v.wantErr {
				t.Errorf("listConfigToMap() error = %v, wantErr %v", err, v.wantErr)
				return
			}
			if !reflect.DeepEqual(got, v.expectedValue) {
				t.Errorf("listConfigToMap() got = %v, want %v", got, v.expectedValue)
			}
		})
	}
}

func Test_UnMarshallToConfig(t *testing.T) {
	tests := map[string]struct {
		text     string
		expected []Config
		wantErr  bool
	}{
		"basic": {
			text: `
- name: config
  enabled: true
  value: foo
  data:
    key: value
  list:
  - "item1"
  - "item2"`,
			expected: []Config{
				{
					Name:    "config",
					Enabled: "true",
					Value:   "foo",
					Data:    map[string]string{"key": "value"},
					List:    []string{"item1", "item2"},
				},
			},
			wantErr: false,
		},
		"invalid yaml": {
			text:     `bad yaml`,
			expected: nil,
			wantErr:  true,
		},
	}
	for k, v := range tests {
		t.Run(k, func(t *testing.T) {
			got, err := UnMarshallToConfig(v.text)
			if (err != nil) != v.wantErr {
				t.Errorf("UnMarshallToConfig() error = %v, wantErr %v", err, v.wantErr)
				return
			}
			if !reflect.DeepEqual(got, v.expected) {
				t.Errorf("UnMarshallToConfig() got = %v, want %v", got, v.expected)
			}
		})
	}
}
