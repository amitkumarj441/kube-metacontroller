/*
Copyright 2017 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"

	"github.com/golang/glog"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/metacontroller/apis/metacontroller/v1alpha1"
	k8s "k8s.io/metacontroller/third_party/kubernetes"
)

func syncAllInitializerControllers(clientset *dynamicClientset) error {
	icClient, err := clientset.Resource(v1alpha1.SchemeGroupVersion.String(), "initializercontrollers", "")
	if err != nil {
		return err
	}
	obj, err := icClient.List(metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("can't list InitializerControllers: %v", err)
	}
	icList := obj.(*unstructured.UnstructuredList)

	for i := range icList.Items {
		data, err := json.Marshal(&icList.Items[i])
		if err != nil {
			glog.Errorf("can't marshal InitializerController: %v")
			continue
		}
		ic := &v1alpha1.InitializerController{}
		if err := json.Unmarshal(data, ic); err != nil {
			glog.Errorf("can't unmarshal InitializerController: %v", err)
			continue
		}
		if err := syncInitializerController(clientset, ic); err != nil {
			glog.Errorf("sync InitializerController %v: %v", ic.Name, err)
			continue
		}
	}
	return nil
}

func syncInitializerController(clientset *dynamicClientset, ic *v1alpha1.InitializerController) error {
	var errs []error
	// Find all uninitialized objects of the requested kinds.
	for _, group := range ic.Spec.UninitializedResources {
		// Within each group/version, there can be multiple resources requested.
		for _, resourceName := range group.Resources {
			if err := initializeResource(clientset, ic, group.APIVersion, resourceName); err != nil {
				errs = append(errs, err)
				continue
			}
		}
	}
	return utilerrors.NewAggregate(errs)
}

func initializeResource(clientset *dynamicClientset, ic *v1alpha1.InitializerController, apiVersion, resourceName string) error {
	// List all objects of the given kind in all namespaces.
	client, err := clientset.Resource(apiVersion, resourceName, "")
	if err != nil {
		return err
	}
	obj, err := client.List(metav1.ListOptions{IncludeUninitialized: true})
	if err != nil {
		return fmt.Errorf("can't list uninitialized %v objects: %v", client.Kind(), err)
	}
	list := obj.(*unstructured.UnstructuredList)

	var errs []error
	for i := range list.Items {
		uninitialized := &list.Items[i]

		// Check if this initializer is next in the pending list.
		pending := k8s.GetNestedArray(uninitialized.UnstructuredContent(), "metadata", "initializers", "pending")
		if len(pending) < 1 {
			continue
		}
		first, ok := pending[0].(map[string]interface{})
		if !ok {
			continue
		}
		if k8s.GetNestedString(first, "name") == ic.Spec.InitializerName {
			resp, err := callInitHook(ic, &initHookRequest{Object: uninitialized})
			if err != nil {
				// TODO(enisoc): Add this as an event on the uninitialized object?
				errs = append(errs, fmt.Errorf("can't initialize %v %v/%v: %v", uninitialized.GetKind(), uninitialized.GetNamespace(), uninitialized.GetName(), err))
				continue
			}
			initialized := resp.Object

			// Remove this initializer from pending.
			pending = pending[1:]
			if len(pending) == 0 {
				// This is a workaround for a bug in 1.7.x, which does not allow setting 'pending'
				// to an empty list.
				k8s.DeleteNestedField(initialized.UnstructuredContent(), "metadata", "initializers")
			} else {
				k8s.SetNestedField(initialized.UnstructuredContent(), pending, "metadata", "initializers", "pending")
			}

			// Set initializer result if provided.
			if resp.Result != nil {
				k8s.SetNestedField(initialized.UnstructuredContent(), resp.Result, "metadata", "initializers", "result")
			}

			glog.Infof("InitializerController %v: updating %v %v/%v", ic.Name, initialized.GetKind(), initialized.GetNamespace(), initialized.GetName())
			nsClient, err := clientset.Resource(apiVersion, resourceName, initialized.GetNamespace())
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if _, err := nsClient.Update(initialized); err != nil {
				errs = append(errs, fmt.Errorf("can't update %v %v/%v: %v", initialized.GetKind(), initialized.GetNamespace(), initialized.GetName(), err))
				continue
			}
		}
	}
	return utilerrors.NewAggregate(errs)
}
