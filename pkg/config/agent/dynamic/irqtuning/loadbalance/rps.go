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

package loadbalance

import "github.com/kubewharf/katalyst-core/pkg/config/agent/dynamic/crd"

type RPSConfig struct {
	// if enable RPS when some single irq's cpu demand exceeds one whole CPU
	EnableRPS bool `json:"enableRPS"`
	// if reset RPS dest cpus when irq cores adjusted, be careful, reset dest cpus may result in out-of-order packets, so limit the reset rate.
	ResetDestCPUsWhenIrqCoresAdjusted bool

	EnterThresh *RPSEnterThresholds

	ExitThresh *RPSExitThresholds
}

type RPSEnterThresholds struct {
	// generally this value greater-than IrqLoadBalanceTuningThresholds.IrqCoreCpuUtilThresh
	SingleIrqCPUUtil int
}

type RPSExitThresholds struct {
	// Eligible PPS means (rps enabled queue's pps) - (non-rps queues's max PPS)
	EligiblePPSDuration int
}

func NewRPSConfig() *RPSConfig {
	return &RPSConfig{
		EnableRPS:                         true,
		ResetDestCPUsWhenIrqCoresAdjusted: true,
		EnterThresh: &RPSEnterThresholds{
			SingleIrqCPUUtil: 80,
		},
		ExitThresh: &RPSExitThresholds{
			EligiblePPSDuration: 120,
		},
	}
}

func (c *RPSConfig) ApplyConfiguration(conf *crd.DynamicConfigCRD) {
	if itc := conf.IRQTuningConfiguration; itc != nil &&
		itc.Spec.Config.IRQLoadBalanceConf != nil &&
		itc.Spec.Config.IRQLoadBalanceConf.RPSConf != nil {
		config := itc.Spec.Config.IRQLoadBalanceConf.RPSConf
		if config.EnableRPS != nil {
			c.EnableRPS = *config.EnableRPS
		}
		if config.ResetDestCPUsWhenIrqCoresAdjusted != nil {
			c.ResetDestCPUsWhenIrqCoresAdjusted = *config.ResetDestCPUsWhenIrqCoresAdjusted
		}
		if config.EnterThresh != nil {
			if config.EnterThresh.SingleIrqCPUUtil != nil {
				c.EnterThresh.SingleIrqCPUUtil = *config.EnterThresh.SingleIrqCPUUtil
			}
		}
		if config.ExitThresh != nil {
			if config.ExitThresh.EligiblePPSDuration != nil {
				c.ExitThresh.EligiblePPSDuration = *config.ExitThresh.EligiblePPSDuration
			}
		}
	}
}
