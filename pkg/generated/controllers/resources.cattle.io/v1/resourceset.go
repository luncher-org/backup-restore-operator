/*
Copyright 2024 Rancher Labs, Inc.

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

// Code generated by main. DO NOT EDIT.

package v1

import (
	v1 "github.com/rancher/backup-restore-operator/pkg/apis/resources.cattle.io/v1"
	"github.com/rancher/wrangler/v2/pkg/generic"
)

// ResourceSetController interface for managing ResourceSet resources.
type ResourceSetController interface {
	generic.NonNamespacedControllerInterface[*v1.ResourceSet, *v1.ResourceSetList]
}

// ResourceSetClient interface for managing ResourceSet resources in Kubernetes.
type ResourceSetClient interface {
	generic.NonNamespacedClientInterface[*v1.ResourceSet, *v1.ResourceSetList]
}

// ResourceSetCache interface for retrieving ResourceSet resources in memory.
type ResourceSetCache interface {
	generic.NonNamespacedCacheInterface[*v1.ResourceSet]
}
