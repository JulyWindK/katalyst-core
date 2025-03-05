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

type IRQCoresIncConfig struct {
	// interval of two successive irq cores increase MUST greater-equal this interval
	SuccessiveIncInterval int
	// scale factor of irq cores cpu usage to calculate expected irq cores number when irq cores cpu util nearly full, default 1.5
	IRQCoresFullScaleFactor float64

	Thresholds *IRQCoresIncThresholds
}

type IRQCoresIncThresholds struct {
	// threshold of increasing irq cores, generally this thresh equal to or a litter greater-than IrqCoresExpectedCpuUtil
	IRQCoresAvgCPUUtilThresh int
}

func NewIRQCoresIncConfig() *IRQCoresIncConfig {
	return &IRQCoresIncConfig{
		SuccessiveIncInterval:   5,
		IRQCoresFullScaleFactor: 1.5,
		Thresholds: &IRQCoresIncThresholds{
			IRQCoresAvgCPUUtilThresh: 60,
		},
	}
}

func (c *IRQCoresIncConfig) ApplyConfiguration(conf *crd.DynamicConfigCRD) {
	if itc := conf.IRQTuningConfiguration; itc != nil &&
		itc.Spec.Config.IRQCoresAdjustConf != nil &&
		itc.Spec.Config.IRQCoresAdjustConf.IRQCoresIncConf != nil {
		config := itc.Spec.Config.IRQCoresAdjustConf.IRQCoresIncConf
		if config.SuccessiveIncInterval != nil {
			c.SuccessiveIncInterval = *config.SuccessiveIncInterval
		}
		if config.IRQCoresFullScaleFactor != nil {
			c.IRQCoresFullScaleFactor = *config.IRQCoresFullScaleFactor
		}
		if config.Thresholds != nil {
			if config.Thresholds.IRQCoresAvgCPUUtilThresh != nil {
				c.Thresholds.IRQCoresAvgCPUUtilThresh = *config.Thresholds.IRQCoresAvgCPUUtilThresh
			}
		}
	}
}
