#!/usr/bin/env bash

# Copyright 2017 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -x

set -o errexit
set -o nounset
set -o pipefail

SCRIPT_ROOT="$(cd "$(dirname $0)/../" && pwd -P)"
if [ -d "${SCRIPT_ROOT}/vendor" ]; then
  export GOFLAGS="-mod=readonly"
fi
CODEGEN_PKG=${CODEGEN_PKG:-$(go list -f '{{.Dir}}' -m k8s.io/code-generator)}
CODE_REPO=github.com/alibaba/open-local
GROUP=storage
VERSION=v1alpha1
OUTPUT_PACKAGE="${CODE_REPO}/pkg/generated"
OUTPUT_BASE="${SCRIPT_ROOT}/hack"
export GOBIN=${SCRIPT_ROOT}/bin

# generate the code to hack/github.com/alibaba/open-local/...
bash ${CODEGEN_PKG}/generate-groups.sh "deepcopy,client,informer,lister" \
  ${OUTPUT_PACKAGE} ${CODE_REPO}/pkg/apis \
  ${GROUP}:${VERSION} \
  --output-base "${OUTPUT_BASE}" \
  --go-header-file ${SCRIPT_ROOT}/hack/boilerplate.go.txt

# move the code to the right place
rm -rf "${SCRIPT_ROOT}/pkg/generated"
mv "${OUTPUT_BASE}/${OUTPUT_PACKAGE}" "${SCRIPT_ROOT}/pkg/generated"
cp -r "${OUTPUT_BASE}/${CODE_REPO}/pkg/apis/" "${SCRIPT_ROOT}/pkg/apis/"
rm -rf "${OUTPUT_BASE}/github.com"
