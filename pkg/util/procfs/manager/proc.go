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

package manager

import (
	"github.com/kubewharf/katalyst-core/pkg/util/machine"
	"github.com/prometheus/procfs"
)

func GetCPUInfo() ([]procfs.CPUInfo, error) {
	return GetProcFSManager().GetCPUInfo()
}

func GetProcStat() (procfs.Stat, error) {
	return GetProcFSManager().GetProcStat()
}

func GetPidComm(pid int) (string, error) {
	return GetProcFSManager().GetPidComm(pid)
}

func GetPidCmdline(pid int) ([]string, error) {
	return GetProcFSManager().GetPidCmdline(pid)
}

func GetPidCgroups(pid int) ([]procfs.Cgroup, error) {
	return GetProcFSManager().GetPidCgroups(pid)
}

func GetMounts() ([]*procfs.MountInfo, error) {
	return GetProcFSManager().GetMounts()
}

func GetProcMounts(pid int) ([]*procfs.MountInfo, error) {
	return GetProcFSManager().GetProcMounts(pid)
}

func GetIPVSStats() (procfs.IPVSStats, error) {
	return GetProcFSManager().GetIPVSStats()
}

func GetNetDev() (map[string]procfs.NetDevLine, error) {
	return GetProcFSManager().GetNetDev()
}

func GetNetStat() ([]procfs.NetStat, error) {
	return GetProcFSManager().GetNetStat()
}

func GetNetTCP() (procfs.NetTCP, error) {
	return GetProcFSManager().GetNetTCP()
}

func GetNetTCP6() (procfs.NetTCP, error) {
	return GetProcFSManager().GetNetTCP6()
}

func GetNetUDP() (procfs.NetUDP, error) {
	return GetProcFSManager().GetNetUDP()
}

func GetNetUDP6() (procfs.NetUDP, error) {
	return GetProcFSManager().GetNetUDP6()
}

func GetSoftirqs() (procfs.Softirqs, error) {
	return GetProcFSManager().GetSoftirqs()
}

func GetProcInterrupts() (procfs.Interrupts, error) {
	return GetProcFSManager().GetProcInterrupts()
}

func GetPSIStatsForResource(resourceName string) (procfs.PSIStats, error) {
	return GetProcFSManager().GetPSIStatsForResource(resourceName)
}

func GetSchedStat() (*procfs.Schedstat, error) {
	return GetProcFSManager().GetSchedStat()
}

func ApplyProcInterrupts(irqNumber int, cpuset machine.CPUSet) error {
	return GetProcFSManager().ApplyProcInterrupts(irqNumber, cpuset)
}
