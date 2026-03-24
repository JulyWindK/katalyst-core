/*
Copyright 2022 The Katalyst Authors.

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

package validator

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/util/validator"
	"github.com/kubewharf/katalyst-core/pkg/config"
	"github.com/kubewharf/katalyst-core/pkg/metaserver/agent/pod"
)

type nicAnnotationValidator struct {
	conf       *config.Configuration
	client     kubernetes.Interface
	podFetcher pod.PodFetcher
}

func NewNICAnnotationValidator(conf *config.Configuration, client kubernetes.Interface, podFetcher pod.PodFetcher) validator.AnnotationValidator {
	return &nicAnnotationValidator{
		conf:       conf,
		client:     client,
		podFetcher: podFetcher,
	}
}

func (n *nicAnnotationValidator) ValidatePodAnnotation(ctx context.Context, podUID, podNameSpace, podName string) (bool, error) {
	annoList := map[string]string{
		n.conf.NetworkQRMPluginConfig.IPv4ResourceAllocationAnnotationKey:             "",
		n.conf.NetworkQRMPluginConfig.IPv6ResourceAllocationAnnotationKey:             "",
		n.conf.NetworkQRMPluginConfig.NetNSPathResourceAllocationAnnotationKey:        "",
		n.conf.NetworkQRMPluginConfig.NetInterfaceNameResourceAllocationAnnotationKey: "",
		n.conf.NetworkQRMPluginConfig.NetClassIDResourceAllocationAnnotationKey:       "",
		n.conf.NetworkQRMPluginConfig.NetBandwidthResourceAllocationAnnotationKey:     "",
	}

	getPod, err := n.podFetcher.GetPod(context.WithValue(ctx, pod.BypassCacheKey, pod.BypassCacheTrue), podUID)
	if err != nil {
		getPod, err = n.client.CoreV1().Pods(podNameSpace).Get(ctx, podName, metav1.GetOptions{ResourceVersion: "0"})
		if err != nil {
			return false, err
		}
	}

	return validator.ValidatePodAnnotations(getPod, annoList)
}
