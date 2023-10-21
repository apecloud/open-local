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

package provisioner

import (
	"os"
	"strings"
)

const (
	// ProvisionerNamespace is the environment variable that provides the
	// namespace where the provisioner is deployed.
	ProvisionerNamespace = "NAMESPACE"

	// ProvisionerServiceAccount is the environment variable that provides the
	// service account to be used by the provisioner.
	ProvisionerServiceAccount = "SERVICE_ACCOUNT"

	// ProvisionerHelperImage is the environment variable that provides the
	// container image to be used to launch the help pods managing the
	// host path
	ProvisionerHelperImage = "HELPER_IMAGE"

	// ProvisionerBasePath is the environment variable that provides the
	// default base path on the node where host-path PVs will be provisioned.
	ProvisionerBasePath = "BASE_PATH"

	// ProvisionerImagePullSecrets is the environment variable that provides the
	// init pod to use as authentication when pulling helper image, it is used in the scene where authentication is required
	ProvisionerImagePullSecrets = "IMAGE_PULL_SECRETS"
)

var (
	// TODO(x.zhou): use our own image
	defaultHelperImage = "openebs/linux-utils:latest"
	defaultBasePath    = "/var/open-local/local"
)

func getEnv(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

func getOrDefault(key string, defaultValue string) string {
	envValue := getEnv(key)
	if len(envValue) == 0 {
		// ENV not defined or set to ""
		return defaultValue
	}
	return envValue
}

func getNamespace() string {
	return getEnv(ProvisionerNamespace)
}

func getDefaultHelperImage() string {
	return getOrDefault(ProvisionerHelperImage, string(defaultHelperImage))
}

func getDefaultBasePath() string {
	return getOrDefault(ProvisionerBasePath, string(defaultBasePath))
}

func getServiceAccountName() string {
	return getEnv(ProvisionerServiceAccount)
}

func getImagePullSecrets() string {
	return getEnv(ProvisionerImagePullSecrets)
}
