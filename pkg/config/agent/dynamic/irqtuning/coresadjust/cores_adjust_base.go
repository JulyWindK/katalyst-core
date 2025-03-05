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

// IRQCoresAdjustConfig is the configuration for IRQCoresAdjustConfig.
type IRQCoresAdjustConfig struct {
	// minimum percent of (100 * irq cores/total(or socket) cores), valid value [0,100], default 2
	IRQCoresPercentMin int

	// maximum percent of (100 * irq cores/total(or socket) cores), valid value [0,100], default 30
	IRQCoresPercentMax int

	IRQCoresIncConf *IRQCoresIncConfig
	IRQCoresDecConf *IRQCoresDecConfig
}

func NewIRQCoresAdjustConfig() *IRQCoresAdjustConfig {
	return &IRQCoresAdjustConfig{
		IRQCoresPercentMin: 2,
		IRQCoresPercentMax: 30,
		IRQCoresIncConf:    NewIRQCoresIncConfig(),
		IRQCoresDecConf:    NewIRQCoresDecConfig(),
	}
}

func (c *IRQCoresAdjustConfig) ApplyConfiguration(conf *crd.DynamicConfigCRD) {
	if itc := conf.IRQTuningConfiguration; itc != nil &&
		itc.Spec.Config.IRQCoresAdjustConf != nil {
		config := itc.Spec.Config.IRQCoresAdjustConf
		if config.IRQCoresPercentMin != nil {
			c.IRQCoresPercentMin = *config.IRQCoresPercentMin
		}
		if config.IRQCoresPercentMax != nil {
			c.IRQCoresPercentMax = *config.IRQCoresPercentMax
		}
		if config.IRQCoresIncConf != nil {
			c.IRQCoresIncConf.ApplyConfiguration(conf)
		}
		if config.IRQCoresDecConf != nil {
			c.IRQCoresDecConf.ApplyConfiguration(conf)
		}
	}
}
