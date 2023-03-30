/*
Copyright 2023 The Kubernetes Authors.

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

// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	"context"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	labels "k8s.io/apimachinery/pkg/labels"
	schema "k8s.io/apimachinery/pkg/runtime/schema"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	testing "k8s.io/client-go/testing"
	v1beta1 "k8s.io/ingress-gce/pkg/apis/ingparams/v1beta1"
)

// FakeGCPIngressParams implements GCPIngressParamsInterface
type FakeGCPIngressParams struct {
	Fake *FakeNetworkingV1beta1
}

var gcpingressparamsResource = schema.GroupVersionResource{Group: "networking.gke.io", Version: "v1beta1", Resource: "gcpingressparams"}

var gcpingressparamsKind = schema.GroupVersionKind{Group: "networking.gke.io", Version: "v1beta1", Kind: "GCPIngressParams"}

// Get takes name of the gCPIngressParams, and returns the corresponding gCPIngressParams object, and an error if there is any.
func (c *FakeGCPIngressParams) Get(ctx context.Context, name string, options v1.GetOptions) (result *v1beta1.GCPIngressParams, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewRootGetAction(gcpingressparamsResource, name), &v1beta1.GCPIngressParams{})
	if obj == nil {
		return nil, err
	}
	return obj.(*v1beta1.GCPIngressParams), err
}

// List takes label and field selectors, and returns the list of GCPIngressParams that match those selectors.
func (c *FakeGCPIngressParams) List(ctx context.Context, opts v1.ListOptions) (result *v1beta1.GCPIngressParamsList, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewRootListAction(gcpingressparamsResource, gcpingressparamsKind, opts), &v1beta1.GCPIngressParamsList{})
	if obj == nil {
		return nil, err
	}

	label, _, _ := testing.ExtractFromListOptions(opts)
	if label == nil {
		label = labels.Everything()
	}
	list := &v1beta1.GCPIngressParamsList{ListMeta: obj.(*v1beta1.GCPIngressParamsList).ListMeta}
	for _, item := range obj.(*v1beta1.GCPIngressParamsList).Items {
		if label.Matches(labels.Set(item.Labels)) {
			list.Items = append(list.Items, item)
		}
	}
	return list, err
}

// Watch returns a watch.Interface that watches the requested gCPIngressParams.
func (c *FakeGCPIngressParams) Watch(ctx context.Context, opts v1.ListOptions) (watch.Interface, error) {
	return c.Fake.
		InvokesWatch(testing.NewRootWatchAction(gcpingressparamsResource, opts))
}

// Create takes the representation of a gCPIngressParams and creates it.  Returns the server's representation of the gCPIngressParams, and an error, if there is any.
func (c *FakeGCPIngressParams) Create(ctx context.Context, gCPIngressParams *v1beta1.GCPIngressParams, opts v1.CreateOptions) (result *v1beta1.GCPIngressParams, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewRootCreateAction(gcpingressparamsResource, gCPIngressParams), &v1beta1.GCPIngressParams{})
	if obj == nil {
		return nil, err
	}
	return obj.(*v1beta1.GCPIngressParams), err
}

// Update takes the representation of a gCPIngressParams and updates it. Returns the server's representation of the gCPIngressParams, and an error, if there is any.
func (c *FakeGCPIngressParams) Update(ctx context.Context, gCPIngressParams *v1beta1.GCPIngressParams, opts v1.UpdateOptions) (result *v1beta1.GCPIngressParams, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewRootUpdateAction(gcpingressparamsResource, gCPIngressParams), &v1beta1.GCPIngressParams{})
	if obj == nil {
		return nil, err
	}
	return obj.(*v1beta1.GCPIngressParams), err
}

// Delete takes name of the gCPIngressParams and deletes it. Returns an error if one occurs.
func (c *FakeGCPIngressParams) Delete(ctx context.Context, name string, opts v1.DeleteOptions) error {
	_, err := c.Fake.
		Invokes(testing.NewRootDeleteAction(gcpingressparamsResource, name), &v1beta1.GCPIngressParams{})
	return err
}

// DeleteCollection deletes a collection of objects.
func (c *FakeGCPIngressParams) DeleteCollection(ctx context.Context, opts v1.DeleteOptions, listOpts v1.ListOptions) error {
	action := testing.NewRootDeleteCollectionAction(gcpingressparamsResource, listOpts)

	_, err := c.Fake.Invokes(action, &v1beta1.GCPIngressParamsList{})
	return err
}

// Patch applies the patch and returns the patched gCPIngressParams.
func (c *FakeGCPIngressParams) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts v1.PatchOptions, subresources ...string) (result *v1beta1.GCPIngressParams, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewRootPatchSubresourceAction(gcpingressparamsResource, name, pt, data, subresources...), &v1beta1.GCPIngressParams{})
	if obj == nil {
		return nil, err
	}
	return obj.(*v1beta1.GCPIngressParams), err
}
