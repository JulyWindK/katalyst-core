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

package irqtuning

import (
	"github.com/kubewharf/katalyst-core/pkg/config/agent/dynamic/crd"
	"github.com/kubewharf/katalyst-core/pkg/config/agent/dynamic/irqtuning/coresadjust"
	"github.com/kubewharf/katalyst-core/pkg/config/agent/dynamic/irqtuning/coresexclusion"
	"github.com/kubewharf/katalyst-core/pkg/config/agent/dynamic/irqtuning/loadbalance"
	"github.com/kubewharf/katalyst-core/pkg/config/agent/dynamic/irqtuning/netoverload"
	"github.com/kubewharf/katalyst-core/pkg/util/general"
)

// IRQTuningConfiguration is the configuration for irq tuner.
type IRQTuningConfiguration struct {
	EnableIRQTuner          bool
	EnableIRQCoresExclusion bool
	// irq tuning periodic interval
	Interval                 int
	IRQCoresExpectedCPUUtil  int
	IRQCoreNetOverLoadThresh *netoverload.IRQCoreNetOverloadThresholds
	IRQLoadBalanceConf       *loadbalance.IRQLoadBalanceConfig
	IRQCoresAdjustConf       *coresadjust.IRQCoresAdjustConfig
	IRQCoresExclusionConf    *coresexclusion.IRQCoresExclusionConfig
}

func NewIRQTuningConfiguration() *IRQTuningConfiguration {
	return &IRQTuningConfiguration{
		EnableIRQTuner:           false,
		EnableIRQCoresExclusion:  false,
		Interval:                 5,
		IRQCoresExpectedCPUUtil:  50,
		IRQCoreNetOverLoadThresh: netoverload.NewIRQCoreNetOverloadThresholds(),
		IRQLoadBalanceConf:       loadbalance.NewIRQLoadBalanceConfig(),
		IRQCoresAdjustConf:       coresadjust.NewIRQCoresAdjustConfig(),
		IRQCoresExclusionConf:    coresexclusion.NewIRQCoresExclusionConfig(),
	}
}

func (c *IRQTuningConfiguration) ApplyConfiguration(conf *crd.DynamicConfigCRD) {
	general.Infof("[DEBUG]IRQTuningConfiguration ApplyConfiguration crd conf: %+v", conf)
	if itc := conf.IRQTuningConfiguration; itc != nil {
		if itc.Spec.Config.EnableIRQTuner != nil {
			c.EnableIRQTuner = *itc.Spec.Config.EnableIRQTuner
		}
		if itc.Spec.Config.EnableIRQCoresExclusion != nil {
			c.EnableIRQCoresExclusion = *itc.Spec.Config.EnableIRQCoresExclusion
		}
		if itc.Spec.Config.Interval != nil {
			c.Interval = *itc.Spec.Config.Interval
		}
		if itc.Spec.Config.IRQCoresExpectedCPUUtil != nil {
			c.IRQCoresExpectedCPUUtil = *itc.Spec.Config.IRQCoresExpectedCPUUtil
		}
		c.IRQCoreNetOverLoadThresh.ApplyConfiguration(conf)
		c.IRQLoadBalanceConf.ApplyConfiguration(conf)
		c.IRQCoresAdjustConf.ApplyConfiguration(conf)
		c.IRQCoresExclusionConf.ApplyConfiguration(conf)
	}
	general.Infof("[DEBUG]IRQTuningConfiguration ApplyConfiguration irq conf: %+v", c)

}
