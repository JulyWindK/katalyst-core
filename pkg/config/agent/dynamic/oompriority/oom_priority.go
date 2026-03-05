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

package oompriority

import (
	"github.com/kubewharf/katalyst-core/pkg/config/agent/dynamic/crd"
)

type OOMPriorityConfiguration struct {
	EnableOOMPriority                     bool
	Interval                              int
	LowPriorityWartermarkRatioWithSwapOn  int
	LowPriorityWartermarkRatioWithSwapOff int
	HighPriorityKatalystQoS               []string
	LowPriorityKatalystQoS                []string
	HighPriorityCgroupPaths               []string
	LowPriorityCgroupPaths                []string
}

func NewOOMPriorityConfiguration() *OOMPriorityConfiguration {
	return &OOMPriorityConfiguration{
		Interval:                              60,
		LowPriorityWartermarkRatioWithSwapOn:  50,
		LowPriorityWartermarkRatioWithSwapOff: 30,
		HighPriorityKatalystQoS:               []string{},
		LowPriorityKatalystQoS:                []string{},
		HighPriorityCgroupPaths:               []string{},
		LowPriorityCgroupPaths:                []string{},
	}
}

func (sg *OOMPriorityConfiguration) ApplyConfiguration(conf *crd.DynamicConfigCRD) {
	if dynamicOOMPriority := conf.OOMPriority; dynamicOOMPriority != nil {
		if dynamicOOMPriority.Spec.Config.EnableOOMPriority != nil {
			sg.EnableOOMPriority = *dynamicOOMPriority.Spec.Config.EnableOOMPriority
		}
		if dynamicOOMPriority.Spec.Config.Interval != nil {
			sg.Interval = *dynamicOOMPriority.Spec.Config.Interval
		}
		if dynamicOOMPriority.Spec.Config.LowPriorityWartermarkRatioWithSwapOn != nil {
			sg.LowPriorityWartermarkRatioWithSwapOn = *dynamicOOMPriority.Spec.Config.LowPriorityWartermarkRatioWithSwapOn
		}
		if dynamicOOMPriority.Spec.Config.LowPriorityWartermarkRatioWithSwapOff != nil {
			sg.LowPriorityWartermarkRatioWithSwapOff = *dynamicOOMPriority.Spec.Config.LowPriorityWartermarkRatioWithSwapOff
		}
		if dynamicOOMPriority.Spec.Config.HighPriorityKatalystQoS != nil {
			for _, qos := range dynamicOOMPriority.Spec.Config.HighPriorityKatalystQoS {
				sg.HighPriorityKatalystQoS = append(sg.HighPriorityKatalystQoS, qos)
			}
		}
		if dynamicOOMPriority.Spec.Config.LowPriorityKatalystQoS != nil {
			for _, qos := range dynamicOOMPriority.Spec.Config.LowPriorityKatalystQoS {
				sg.LowPriorityKatalystQoS = append(sg.LowPriorityKatalystQoS, qos)
			}
		}
		if dynamicOOMPriority.Spec.Config.HighPriorityCgroupPaths != nil {
			for _, path := range dynamicOOMPriority.Spec.Config.HighPriorityCgroupPaths {
				sg.HighPriorityCgroupPaths = append(sg.HighPriorityCgroupPaths, path)
			}
		}
		if dynamicOOMPriority.Spec.Config.LowPriorityCgroupPaths != nil {
			for _, path := range dynamicOOMPriority.Spec.Config.LowPriorityCgroupPaths {
				sg.LowPriorityCgroupPaths = append(sg.LowPriorityCgroupPaths, path)
			}
		}
	}
}
