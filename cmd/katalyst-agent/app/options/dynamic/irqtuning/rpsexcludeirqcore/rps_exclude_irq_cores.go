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

package rpsexcludeirqcore

import (
	"github.com/kubewharf/katalyst-core/pkg/config/agent/dynamic/irqtuning/rpsexcludeirqcore"
	cliflag "k8s.io/component-base/cli/flag"
)

type RPSExcludeIRQCoresThreshold struct {
	// RPSCoresVSIRQCoresRatio is the ratio of rps qualified cores count versus irq cores count of rps qualified cores.
	RPSCoresVSIRQCoresRatio float64
}

func NewRPSExcludeIRQCoresThreshold() *RPSExcludeIRQCoresThreshold {
	return &RPSExcludeIRQCoresThreshold{
		RPSCoresVSIRQCoresRatio: 4,
	}
}

func (o *RPSExcludeIRQCoresThreshold) AddFlags(fss *cliflag.NamedFlagSets) {
	fs := fss.FlagSet("rps-exclude-irq-cores-thresholds")
	fs.Float64Var(&o.RPSCoresVSIRQCoresRatio, "rps-cores-vs-irq-cores-ratio", o.RPSCoresVSIRQCoresRatio, "ratio of rps qualified cores count versus irq cores count of rps qualified cores")
}

func (o *RPSExcludeIRQCoresThreshold) ApplyTo(c *rpsexcludeirqcore.RPSExcludeIRQCoresThreshold) error {
	c.RPSCoresVSIRQCoresRatio = o.RPSCoresVSIRQCoresRatio
	return nil
}
