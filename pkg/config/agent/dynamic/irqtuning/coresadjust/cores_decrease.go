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

package coresadjust

import "github.com/kubewharf/katalyst-core/pkg/config/agent/dynamic/crd"

type IRQCoresDecConfig struct {
	// interval of two successive irq cores decrease MUST greater-equal this interval
	SuccessiveDecInterval int
	Thresholds            *IRQCoresDecThresholds
	// max cores to decrease each time, deault 1
	DecCoresMaxEachTime int
}

type IRQCoresDecThresholds struct {
	// threshold of decreasing irq cores, generally this thresh should be less-than IrqCoresExpectedCpuUtil
	IRQCoresAvgCPUUtilThresh int
}

func NewIRQCoresDecConfig() *IRQCoresDecConfig {
	return &IRQCoresDecConfig{
		SuccessiveDecInterval: 120,
		Thresholds: &IRQCoresDecThresholds{
			IRQCoresAvgCPUUtilThresh: 35,
		},
		DecCoresMaxEachTime: 1,
	}
}

func (c *IRQCoresDecConfig) ApplyConfiguration(conf *crd.DynamicConfigCRD) {
	if itc := conf.IRQTuningConfiguration; itc != nil &&
		itc.Spec.Config.IRQCoresAdjustConf != nil &&
		itc.Spec.Config.IRQCoresAdjustConf.IRQCoresDecConf != nil {
		config := itc.Spec.Config.IRQCoresAdjustConf.IRQCoresDecConf
		if config.SuccessiveDecInterval != nil {
			c.SuccessiveDecInterval = *config.SuccessiveDecInterval
		}
		if config.DecCoresMaxEachTime != nil {
			c.DecCoresMaxEachTime = *config.DecCoresMaxEachTime
		}
		if config.Thresholds != nil {
			if config.Thresholds.IRQCoresAvgCPUUtilThresh != nil {
				c.Thresholds.IRQCoresAvgCPUUtilThresh = *config.Thresholds.IRQCoresAvgCPUUtilThresh
			}
		}
	}
}
