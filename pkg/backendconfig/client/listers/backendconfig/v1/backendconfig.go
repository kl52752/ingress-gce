/*
Copyright 2021 The Kubernetes Authors.

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

// Code generated by lister-gen. DO NOT EDIT.

package v1

import (
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	v1 "k8s.io/ingress-gce/pkg/apis/backendconfig/v1"
)

// BackendConfigLister helps list BackendConfigs.
type BackendConfigLister interface {
	// List lists all BackendConfigs in the indexer.
	List(selector labels.Selector) (ret []*v1.BackendConfig, err error)
	// BackendConfigs returns an object that can list and get BackendConfigs.
	BackendConfigs(namespace string) BackendConfigNamespaceLister
	BackendConfigListerExpansion
}

// backendConfigLister implements the BackendConfigLister interface.
type backendConfigLister struct {
	indexer cache.Indexer
}

// NewBackendConfigLister returns a new BackendConfigLister.
func NewBackendConfigLister(indexer cache.Indexer) BackendConfigLister {
	return &backendConfigLister{indexer: indexer}
}

// List lists all BackendConfigs in the indexer.
func (s *backendConfigLister) List(selector labels.Selector) (ret []*v1.BackendConfig, err error) {
	err = cache.ListAll(s.indexer, selector, func(m interface{}) {
		ret = append(ret, m.(*v1.BackendConfig))
	})
	return ret, err
}

// BackendConfigs returns an object that can list and get BackendConfigs.
func (s *backendConfigLister) BackendConfigs(namespace string) BackendConfigNamespaceLister {
	return backendConfigNamespaceLister{indexer: s.indexer, namespace: namespace}
}

// BackendConfigNamespaceLister helps list and get BackendConfigs.
type BackendConfigNamespaceLister interface {
	// List lists all BackendConfigs in the indexer for a given namespace.
	List(selector labels.Selector) (ret []*v1.BackendConfig, err error)
	// Get retrieves the BackendConfig from the indexer for a given namespace and name.
	Get(name string) (*v1.BackendConfig, error)
	BackendConfigNamespaceListerExpansion
}

// backendConfigNamespaceLister implements the BackendConfigNamespaceLister
// interface.
type backendConfigNamespaceLister struct {
	indexer   cache.Indexer
	namespace string
}

// List lists all BackendConfigs in the indexer for a given namespace.
func (s backendConfigNamespaceLister) List(selector labels.Selector) (ret []*v1.BackendConfig, err error) {
	err = cache.ListAllByNamespace(s.indexer, s.namespace, selector, func(m interface{}) {
		ret = append(ret, m.(*v1.BackendConfig))
	})
	return ret, err
}

// Get retrieves the BackendConfig from the indexer for a given namespace and name.
func (s backendConfigNamespaceLister) Get(name string) (*v1.BackendConfig, error) {
	obj, exists, err := s.indexer.GetByKey(s.namespace + "/" + name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.NewNotFound(v1.Resource("backendconfig"), name)
	}
	return obj.(*v1.BackendConfig), nil
}
