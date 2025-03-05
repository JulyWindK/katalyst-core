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

import (
	"github.com/kubewharf/katalyst-core/pkg/config/agent/dynamic/irqtuning/loadbalance"
	cliflag "k8s.io/component-base/cli/flag"
)

type RPSOptions struct {
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

func NewRPSOptions() *RPSOptions {
	return &RPSOptions{
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

func (o *RPSOptions) AddFlags(fss *cliflag.NamedFlagSets) {
	fs := fss.FlagSet("rps")
	fs.BoolVar(&o.EnableRPS, "enable-rps", o.EnableRPS, "if enable RPS when some single irq's cpu demand exceeds one whole CPU")
	fs.BoolVar(&o.ResetDestCPUsWhenIrqCoresAdjusted, "reset-dest-cpus-when-irq-cores-adjusted", o.ResetDestCPUsWhenIrqCoresAdjusted, "if reset RPS dest cpus when irq cores adjusted, be careful, reset dest cpus may result in out-of-order packets, so limit the reset rate.")
	fs.IntVar(&o.EnterThresh.SingleIrqCPUUtil, "rps-enter-thresh-single-irq-cpu-util", o.EnterThresh.SingleIrqCPUUtil, "generally this value greater-than IrqLoadBalanceTuningThresholds.IrqCoreCpuUtilThresh")
	fs.IntVar(&o.ExitThresh.EligiblePPSDuration, "rps-exit-thresh-eligible-pps-duration", o.ExitThresh.EligiblePPSDuration, "Eligible PPS means (rps enabled queue's pps) - (non-rps queues's max PPS)")
}

func (o *RPSOptions) ApplyTo(c *loadbalance.RPSConfig) error {
	c.EnableRPS = o.EnableRPS
	c.ResetDestCPUsWhenIrqCoresAdjusted = o.ResetDestCPUsWhenIrqCoresAdjusted
	c.EnterThresh.SingleIrqCPUUtil = o.EnterThresh.SingleIrqCPUUtil
	c.ExitThresh.EligiblePPSDuration = o.ExitThresh.EligiblePPSDuration

	return nil
}
