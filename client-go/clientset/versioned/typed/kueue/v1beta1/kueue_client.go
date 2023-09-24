/*
Copyright 2022 The Kubernetes Authors.

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

package v1beta1

import (
	"net/http"

	rest "k8s.io/client-go/rest"
	v1beta1 "sigs.k8s.io/kueue/apis/kueue/v1beta1"
	"sigs.k8s.io/kueue/client-go/clientset/versioned/scheme"
)

type KueueV1beta1Interface interface {
	RESTClient() rest.Interface
	AdmissionChecksGetter
	ClusterQueuesGetter
	LocalQueuesGetter
	ResourceFlavorsGetter
	WorkloadsGetter
	WorkloadPriorityClassesGetter
}

// KueueV1beta1Client is used to interact with features provided by the kueue.x-k8s.io group.
type KueueV1beta1Client struct {
	restClient rest.Interface
}

func (c *KueueV1beta1Client) AdmissionChecks(namespace string) AdmissionCheckInterface {
	return newAdmissionChecks(c, namespace)
}

func (c *KueueV1beta1Client) ClusterQueues(namespace string) ClusterQueueInterface {
	return newClusterQueues(c, namespace)
}

func (c *KueueV1beta1Client) LocalQueues(namespace string) LocalQueueInterface {
	return newLocalQueues(c, namespace)
}

func (c *KueueV1beta1Client) ResourceFlavors(namespace string) ResourceFlavorInterface {
	return newResourceFlavors(c, namespace)
}

func (c *KueueV1beta1Client) Workloads(namespace string) WorkloadInterface {
	return newWorkloads(c, namespace)
}

func (c *KueueV1beta1Client) WorkloadPriorityClasses(namespace string) WorkloadPriorityClassInterface {
	return newWorkloadPriorityClasses(c, namespace)
}

// NewForConfig creates a new KueueV1beta1Client for the given config.
// NewForConfig is equivalent to NewForConfigAndClient(c, httpClient),
// where httpClient was generated with rest.HTTPClientFor(c).
func NewForConfig(c *rest.Config) (*KueueV1beta1Client, error) {
	config := *c
	if err := setConfigDefaults(&config); err != nil {
		return nil, err
	}
	httpClient, err := rest.HTTPClientFor(&config)
	if err != nil {
		return nil, err
	}
	return NewForConfigAndClient(&config, httpClient)
}

// NewForConfigAndClient creates a new KueueV1beta1Client for the given config and http client.
// Note the http client provided takes precedence over the configured transport values.
func NewForConfigAndClient(c *rest.Config, h *http.Client) (*KueueV1beta1Client, error) {
	config := *c
	if err := setConfigDefaults(&config); err != nil {
		return nil, err
	}
	client, err := rest.RESTClientForConfigAndClient(&config, h)
	if err != nil {
		return nil, err
	}
	return &KueueV1beta1Client{client}, nil
}

// NewForConfigOrDie creates a new KueueV1beta1Client for the given config and
// panics if there is an error in the config.
func NewForConfigOrDie(c *rest.Config) *KueueV1beta1Client {
	client, err := NewForConfig(c)
	if err != nil {
		panic(err)
	}
	return client
}

// New creates a new KueueV1beta1Client for the given RESTClient.
func New(c rest.Interface) *KueueV1beta1Client {
	return &KueueV1beta1Client{c}
}

func setConfigDefaults(config *rest.Config) error {
	gv := v1beta1.SchemeGroupVersion
	config.GroupVersion = &gv
	config.APIPath = "/apis"
	config.NegotiatedSerializer = scheme.Codecs.WithoutConversion()

	if config.UserAgent == "" {
		config.UserAgent = rest.DefaultKubernetesUserAgent()
	}

	return nil
}

// RESTClient returns a RESTClient that is used to communicate
// with API server by this client implementation.
func (c *KueueV1beta1Client) RESTClient() rest.Interface {
	if c == nil {
		return nil
	}
	return c.restClient
}
