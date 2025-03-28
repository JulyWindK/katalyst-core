package manager

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/prometheus/procfs"
	"k8s.io/klog/v2"

	"github.com/kubewharf/katalyst-core/pkg/util/bitmask"
	"github.com/kubewharf/katalyst-core/pkg/util/machine"
	"github.com/kubewharf/katalyst-core/pkg/util/procfs/common"
)

type manager struct {
	procfs procfs.FS
}

// NewProcFSManager return a manager for procfs
func NewProcFSManager() *manager {
	fs, _ := procfs.NewDefaultFS()

	return &manager{fs}
}

func (m *manager) GetCPUInfo() ([]procfs.CPUInfo, error) {
	return m.procfs.CPUInfo()
}

func (m *manager) GetProcStat() (procfs.Stat, error) {
	return m.procfs.Stat()
}

func (m *manager) GetPidComm(pid int) (string, error) {
	proc, err := m.procfs.Proc(pid)
	if err != nil {
		klog.Errorf("[Porcfs] get pid %d proc failed, err: %v", pid, err)
		return "", err
	}
	comm, err := proc.Comm()
	if err != nil {
		klog.Errorf("[Porcfs] get pid %d comm failed, err: %v", pid, err)
		return "", err
	}

	return comm, nil
}

func (m *manager) GetPidCmdline(pid int) ([]string, error) {
	proc, err := m.procfs.Proc(pid)
	if err != nil {
		klog.Errorf("[Porcfs] get pid %d proc failed, err: %v", pid, err)
		return nil, err
	}

	data, err := proc.CmdLine()
	if err != nil {
		klog.Errorf("[Porcfs] get pid %d cmdline failed, err: %v", pid, err)
		return nil, err
	}

	return data, nil
}

func (m *manager) GetPidCgroups(pid int) ([]procfs.Cgroup, error) {
	proc, err := m.procfs.Proc(pid)
	if err != nil {
		klog.Errorf("[Porcfs] get pid %d proc failed, err: %v", pid, err)
		return nil, err
	}

	cgroups, err := proc.Cgroups()
	if err != nil {
		klog.Errorf("[Porcfs] get pid %d cgroup failed, err: %v", pid, err)
		return nil, err
	}

	return cgroups, nil
}

func (m *manager) GetMounts() ([]*procfs.MountInfo, error) {
	return procfs.GetMounts()
}

func (m *manager) GetProcMounts(pid int) ([]*procfs.MountInfo, error) {
	return procfs.GetProcMounts(pid)
}

func (m *manager) GetIPVSStats() (procfs.IPVSStats, error) {
	return m.procfs.IPVSStats()
}

func (m *manager) GetNetDev() (map[string]procfs.NetDevLine, error) {
	return m.procfs.NetDev()
}

func (m *manager) GetNetStat() ([]procfs.NetStat, error) {
	return m.procfs.NetStat()
}

func (m *manager) GetNetTCP() (procfs.NetTCP, error) {
	return m.procfs.NetTCP()
}

func (m *manager) GetNetTCP6() (procfs.NetTCP, error) {
	return m.procfs.NetTCP6()
}

func (m *manager) GetNetUDP() (procfs.NetUDP, error) {
	return m.procfs.NetUDP()
}

func (m *manager) GetNetUDP6() (procfs.NetUDP, error) {
	return m.procfs.NetUDP6()
}

func (m *manager) GetSoftirqs() (procfs.Softirqs, error) {
	return m.procfs.Softirqs()
}

func (m *manager) GetProcInterrupts() (procfs.Interrupts, error) {
	data, err := ReadFileNoStat("/proc/interrupts")
	if err != nil {
		klog.Errorf("[Porcfs] get /proc/interrupts failed, err: %v", err)
		return nil, err
	}
	return parseInterrupts(bytes.NewReader(data))
}

func (m *manager) GetPSIStatsForResource(resourceName string) (procfs.PSIStats, error) {
	return m.procfs.PSIStatsForResource(resourceName)
}

func (m *manager) GetSchedStat() (*procfs.Schedstat, error) {
	return m.procfs.Schedstat()
}

func (m *manager) ApplyProcInterrupts(irqNumber int, cpuset machine.CPUSet) error {
	// TODO: modify get max irq number
	if irqNumber < 0 || irqNumber > 10 {
		return fmt.Errorf("invalid IRQ number %d", irqNumber)
	}
	cpus := cpuset.ToSliceInt()
	// TODO: modify get max cpu number
	for cpu := range cpus {
		if cpu < 0 || cpu > 10 {
			return fmt.Errorf("invalid CPU number: %d", cpu)
		}
	}

	mask, err := bitmask.NewBitMask(cpus...)
	if err != nil {
		klog.Errorf("[Procfs] invalid cpuset: %v", err)
		return err
	}
	data := mask.String()

	if err, applied, oldData := common.InstrumentedWriteFileIfChange("/proc/irq", "smp_affinity", data); err != nil {
		return err
	} else if applied {
		klog.Infof("[Procfs] apply proc interrupts successfully, irq number: %v, data: %v, old data: %v\n", irqNumber, data, oldData)
	}

	return nil
}

// ReadFileNoStat uses io.ReadAll to read contents of entire file.
// This is similar to os.ReadFile but without the call to os.Stat, because
// many files in /proc and /sys report incorrect file sizes (either 0 or 4096).
// Reads a max file size of 1024kB.  For files larger than this, a scanner
// should be used.
func ReadFileNoStat(filename string) ([]byte, error) {
	const maxBufferSize = 1024 * 1024

	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	reader := io.LimitReader(f, maxBufferSize)
	return io.ReadAll(reader)
}

// copy from https://github.com/prometheus/procfs/proc_interrupts.go
func parseInterrupts(r io.Reader) (procfs.Interrupts, error) {
	var (
		interrupts = procfs.Interrupts{}
		scanner    = bufio.NewScanner(r)
	)

	if !scanner.Scan() {
		return nil, errors.New("interrupts empty")
	}
	cpuNum := len(strings.Fields(scanner.Text())) // one header per cpu

	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) == 0 { // skip empty lines
			continue
		}
		if len(parts) < 2 {
			return nil, fmt.Errorf("%w: Not enough fields in interrupts (expected 2+ fields but got %d): Error Parsing File", len(parts), parts)
		}
		intName := parts[0][:len(parts[0])-1] // remove trailing :

		if len(parts) == 2 {
			interrupts[intName] = procfs.Interrupt{
				Info:    "",
				Devices: "",
				Values: []string{
					parts[1],
				},
			}
			continue
		}

		intr := procfs.Interrupt{
			Values: parts[1 : cpuNum+1],
		}

		if _, err := strconv.Atoi(intName); err == nil { // numeral interrupt
			intr.Info = parts[cpuNum+1]
			intr.Devices = strings.Join(parts[cpuNum+2:], " ")
		} else {
			intr.Info = strings.Join(parts[cpuNum+1:], " ")
		}
		interrupts[intName] = intr
	}

	return interrupts, scanner.Err()
}
