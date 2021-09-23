/*
Copyright 2018 The Kubernetes Authors.
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

package backends

import (
	"fmt"
	"net/http"

	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud/meta"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/ingress-gce/pkg/composite"
	"k8s.io/ingress-gce/pkg/instances"
	"k8s.io/ingress-gce/pkg/utils"
	"k8s.io/klog"
)

// regionalInstanceGroupLinker handles linking backends to InstanceGroup's.
type regionalInstanceGroupLinker struct {
	instancePool instances.NodePool
	backendPool  Pool
}

// regionalInstanceGroupLinker is a Linker
var _ Linker = (*regionalInstanceGroupLinker)(nil)

func NewRegionalInstanceGroupLinker(instancePool instances.NodePool, backendPool Pool) Linker {
	return &regionalInstanceGroupLinker{
		instancePool: instancePool,
		backendPool:  backendPool,
	}
}

// Link performs linking instance groups to regional backend service
func (linker *regionalInstanceGroupLinker) Link(sp utils.ServicePort, groups []GroupKey) error {
	var igLinks []string

	for _, group := range groups {
		ig, err := linker.instancePool.Get(sp.IGName(), group.Zone)
		if err != nil {
			klog.V(2).Infof("Error linking IG %s Zone: %s", sp.IGName(), group.Zone)
			return fmt.Errorf("error retrieving IG for linking with backend %+v: %w", sp, err)
		}
		igLinks = append(igLinks, ig.SelfLink)
	}

	addIGs := sets.String{}
	for _, igLink := range igLinks {
		path, err := utils.RelativeResourceName(igLink)
		if err != nil {
			return fmt.Errorf("failed to parse instance group: %w", err)
		}
		addIGs.Insert(path)
	}
	klog.V(2).Infof("IG to Add %v", addIGs)
	if len(addIGs) == 0 {
		return nil
	}

	var newBackends []*composite.Backend
	for _, igLink := range igLinks {
		b := &composite.Backend{
			Group: igLink,
		}
		newBackends = append(newBackends, b)
	}
	be, err := linker.backendPool.Get(sp.BackendName(), meta.VersionGA, meta.Regional)
	if err != nil {
		return err
	}
	be.Backends = newBackends

	if err := linker.backendPool.Update(be); err != nil {
		if utils.IsHTTPErrorCode(err, http.StatusBadRequest) {
			klog.V(2).Infof("Updating backend service for Ig failed, err:%v", err)
			return err
		}
	}
	return nil
}
