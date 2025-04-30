package util

import (
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/safchain/ethtool"
	"github.com/vishvananda/netns"
	"k8s.io/klog/v2"
)

const (
	NetNSRunDir        = "/var/run/netns"
	HostNetNSName      = ""
	DefaultNetNSSysDir = "/sys"
	TmpNetNSSysDir     = "/tmp/net_ns_sysfs"
	ClassNetBasePath   = "class/net"
	ClassNetFulPath    = "/sys/class/net"
	VirtioNetDriver    = "virtio_net"
	IrqRootPath        = "/proc/irq"
	InterruptsFile     = "/proc/interrupts"
	NetDevProcFile     = "/proc/net/dev"
	SoftnetStatFile    = "/proc/net/softnet_stat"
	SoftIrqsFile       = "/proc/softirqs"
)

const UnknownSocketBind = -1

type NicDriver string

const (
	NicDriverMLX       NicDriver = "mlx"
	NicDriverBNX       NicDriver = "bnxt"
	NicDriverVirtioNet NicDriver = "virtio_net"
	NicDriverI40E      NicDriver = "i40e"
	NicDriverIXGBE     NicDriver = "ixgbe"
	NicDriverUnknown   NicDriver = "unknown"
)

type NetNSInfo struct {
	NetNSName  string
	NetNSInode uint64 // used to compare with container's process's /proc/net/ns/net linked inode to get process's nents
}

type NicBasicInfo struct {
	NetNSInfo
	Name           string // generally use Name to locate nic's irq line, N.B., nics in different netns may have the same name
	IfIndex        int
	PCIAddr        string    // used to locate nic's irq line in some special scnerios
	Driver         NicDriver // used to filter queue stats of ethtool stats, different driver has different format
	SocketBind     int
	IsVirtioNetDev bool
	VirtioNetName  string // used to filter virtio nic's irqs in /proc/interrupts
	Irqs           []int  // store nic's all irqs including rx irqs, Irqs is used to resolve conflicts when there 2 active nics with the same name in /proc/interrupts
	QueueNum       int
	Queue2Irq      map[int]int
	Irq2Queue      map[int]int
}

type netnsSwitchContext struct {
	originalNetNSHdl netns.NsHandle
	newNetNSName     string
	newNetNSHdl      netns.NsHandle
	sysMountDir      string
	sysDirRemounted  bool
}

type SoftNetStat struct {
	ProcessedPackets   uint64 // /proc/net/softnet_stat 1st col
	TimeSqueezePackets uint64 // /proc/net/softnet_stat 3rd col
}

func SetIrqAffinityCPUs(irq int, cpus []int64) error {
	if irq <= 0 {
		return fmt.Errorf("invalid irq: %d", irq)
	}
	if len(cpus) == 0 {
		return fmt.Errorf("empty cpus")
	}

	irqProcDir := fmt.Sprintf("%s/%d", IrqRootPath, irq)
	if _, err := os.Stat(irqProcDir); err != nil && os.IsNotExist(err) {
		return fmt.Errorf("%d is not exist", irq)
	}

	smpAffinityListPath := path.Join(irqProcDir, "smp_affinity_list")
	cpuListStr := ConvertLinuxListToString(cpus)

	if err := os.WriteFile(smpAffinityListPath, []byte(cpuListStr), 0644); err != nil {
		return fmt.Errorf("failed to write %s to %s, err %v", cpuListStr, smpAffinityListPath, err)
	}
	return nil
}

func SetIrqAffinity(irq int, cpu int64) error {
	if irq <= 0 {
		return fmt.Errorf("invalid irq: %d", irq)
	}

	if cpu < 0 {
		return fmt.Errorf("invalid cpu: %d", cpu)
	}

	irqProcDir := fmt.Sprintf("%s/%d", IrqRootPath, irq)
	if _, err := os.Stat(irqProcDir); err != nil && os.IsNotExist(err) {
		return fmt.Errorf("%d is not exist", irq)
	}

	smpAffinityListPath := path.Join(irqProcDir, "smp_affinity_list")

	if err := os.WriteFile(smpAffinityListPath, []byte(strconv.Itoa(int(cpu))), 0644); err != nil {
		return fmt.Errorf("failed to write %d to %s, err %v", cpu, smpAffinityListPath, err)
	}
	return nil
}

func GetIrqAffinityCPUs(irq int) ([]int64, error) {
	if irq <= 0 {
		return nil, fmt.Errorf("invalid irq: %d", irq)
	}

	irqProcDir := fmt.Sprintf("%s/%d", IrqRootPath, irq)
	if _, err := os.Stat(irqProcDir); err != nil && os.IsNotExist(err) {
		return nil, fmt.Errorf("%d is not exist", irq)
	}

	smpAffinityListPath := path.Join(irqProcDir, "smp_affinity_list")
	cpuList, err := ParseLinuxListFormatFromFile(smpAffinityListPath)
	if err != nil {
		return nil, fmt.Errorf("failed to ParseLinuxListFormatFromFile(%s), err %v", smpAffinityListPath, err)
	}
	return cpuList, nil
}

func setNicRxQueueRPS(nic *NicBasicInfo, queue int, rpsConf string) error {
	if queue <= 0 {
		return fmt.Errorf("invalid rx queue %d", queue)
	}

	nsc, err := netnsEnter(nic.NetNSName)
	if err != nil {
		return fmt.Errorf("failed to netnsEnter(%s), err %v", nic.NetNSName, err)
	}
	defer nsc.netnsExit()

	nicSysDir := filepath.Join(nsc.sysMountDir, ClassNetBasePath, nic.Name)

	queueRPSPath := fmt.Sprintf("%s/queues/rx-%d/rps_cpus", nicSysDir, queue)
	if _, err := os.Stat(queueRPSPath); err != nil && os.IsNotExist(err) {
		return fmt.Errorf("%s is not exist", queueRPSPath)
	}

	if err := os.WriteFile(queueRPSPath, []byte(rpsConf), 0644); err != nil {
		return fmt.Errorf("failed to write %s to %s, err %v", rpsConf, queueRPSPath, err)
	}

	return nil
}

func SetNicRxQueueRPS(nic *NicBasicInfo, queue int, destCpus []int64) error {
	// calculate rps
	rpsConf, _ := ConvertIntSliceToBitmapString(destCpus)

	return setNicRxQueueRPS(nic, queue, rpsConf)
}

func ClearNicRxQueueRPS(nic *NicBasicInfo, queue int) error {
	return setNicRxQueueRPS(nic, queue, "0")
}

func IsZeroBitmap(bitmapStr string) bool {
	fields := strings.Split(bitmapStr, ",")
	for _, field := range fields {
		val, err := strconv.ParseInt(field, 16, 64)
		if err != nil {
			return false
		}
		if val != 0 {
			return false
		}
	}

	return true
}

func ComparesHexBitmapStrings(a string, b string) bool {
	bitmapAFields := strings.Split(a, ",")
	bitmapBFields := strings.Split(b, ",")

	cmpLen := len(bitmapAFields)
	if len(bitmapBFields) < cmpLen {
		cmpLen = len(bitmapBFields)
	}

	aLastIndex := len(bitmapAFields) - 1
	bLastIndex := len(bitmapBFields) - 1
	for i := 0; i < cmpLen; i++ {
		if bitmapAFields[aLastIndex-i] != bitmapBFields[bLastIndex-i] {
			return false
		}
	}

	if len(bitmapAFields) > cmpLen {
		remainder := len(bitmapAFields) - cmpLen
		for i := 0; i < remainder; i++ {
			val, err := strconv.ParseInt(bitmapAFields[i], 16, 64)
			if err != nil {
				return false
			}
			if val != 0 {
				return false
			}
		}
	}

	if len(bitmapBFields) > cmpLen {
		remainder := len(bitmapBFields) - cmpLen
		for i := 0; i < remainder; i++ {
			val, err := strconv.ParseInt(bitmapBFields[i], 16, 64)
			if err != nil {
				return false
			}
			if val != 0 {
				return false
			}
		}
	}

	return true
}

func getNicRxQueueRpsConf(nicSysDir string, queue int) (string, error) {
	queueRPSPath := fmt.Sprintf("%s/queues/rx-%d/rps_cpus", nicSysDir, queue)
	if _, err := os.Stat(queueRPSPath); err != nil && os.IsNotExist(err) {
		return "", fmt.Errorf("%s is not exist in nic %s", queueRPSPath, nicSysDir)
	}

	b, err := os.ReadFile(queueRPSPath)
	if err != nil {
		return "", fmt.Errorf("failed to ReadFile(%s), err %s", queueRPSPath, err)
	}

	return strings.TrimRight(string(b), "\n"), nil
}

func GetNicRxQueueRpsConf(nic *NicBasicInfo, queue int) (string, error) {
	nsc, err := netnsEnter(nic.NetNSName)
	if err != nil {
		return "", fmt.Errorf("failed to netnsEnter(%s), err %v", nic.NetNSName, err)
	}
	defer nsc.netnsExit()

	nicSysDir := filepath.Join(nsc.sysMountDir, ClassNetBasePath, nic.Name)
	return getNicRxQueueRpsConf(nicSysDir, queue)
}

func GetNicRxQueuesRpsConf(nic *NicBasicInfo) (map[int]string, error) {
	if nic.QueueNum <= 0 {
		return nil, fmt.Errorf("invalid queue number %d", nic.QueueNum)
	}

	nsc, err := netnsEnter(nic.NetNSName)
	if err != nil {
		return nil, fmt.Errorf("failed to netnsEnter(%s), err %v", nic.NetNSName, err)
	}
	defer nsc.netnsExit()

	nicSysDir := filepath.Join(nsc.sysMountDir, ClassNetBasePath, nic.Name)

	rxQueuesRpsConf := make(map[int]string)
	for i := 0; i < nic.QueueNum; i++ {
		conf, err := getNicRxQueueRpsConf(nicSysDir, i)
		if err != nil {
			return nil, err
		}
		rxQueuesRpsConf[i] = conf
	}

	return rxQueuesRpsConf, nil
}

// the "same" queue may match multiple irqs, e.g.,
// nics with the same name from different netns
// sriov vfs
func GetNicQueue2IrqWithQueueFilter(nicInfo *NicBasicInfo, queueFilter string, queueDelimeter string) (map[int]int, error) {
	nicAllIrqs := make(map[int]interface{})
	for _, irq := range nicInfo.Irqs {
		nicAllIrqs[irq] = nil
	}

	b, err := os.ReadFile(InterruptsFile)
	if err != nil {
		return nil, fmt.Errorf("failed to ReadFile(%s), err %s", InterruptsFile, err)
	}

	lines := strings.Split(string(b), "\n")
	queue2Irq := make(map[int]int)

	for _, line := range lines {
		if len(line) == 0 {
			continue
		}

		cols := strings.Fields(line)
		if len(cols) == 0 {
			continue
		}

		queueStr := cols[len(cols)-1]

		if !strings.Contains(queueStr, queueFilter) {
			continue
		}

		irq, err := strconv.Atoi(strings.TrimSuffix(cols[0], ":"))
		if err != nil {
			klog.Warningf("failed to parse irq number, err %s", err)
			continue
		}

		// some nics may not have /sys/class/net/<xxx>/device/msi_irqs, so its corresponding NicBasicInfo.Irqs is empty
		if len(nicAllIrqs) > 0 {
			if _, ok := nicAllIrqs[irq]; !ok {
				continue
			}
		}

		queueCols := strings.Split(queueStr, queueDelimeter)
		if len(queueCols) < 2 {
			continue
		}

		findQueue := -1
		for _, col := range queueCols {
			queue, err := strconv.Atoi(col)
			if err == nil {
				findQueue = queue
				break
			}
		}
		if findQueue >= 0 {
			if i, ok := queue2Irq[findQueue]; ok {
				klog.Errorf("%s: %d: %s queue %d has multiple irqs {%d, %d}", nicInfo.NetNSName, nicInfo.IfIndex, nicInfo.Name, findQueue, i, irq)
				continue
			}
			queue2Irq[findQueue] = irq
			continue
		}

		if queueFilter == nicInfo.PCIAddr {
			if !strings.HasPrefix(queueCols[0], "mlx5_comp") {
				continue
			}

			var numArray []byte
			byteArray := []byte(queueCols[0])

			for i := len(byteArray) - 1; i >= 0; i-- {
				b := byteArray[i]
				if b >= '0' && b <= '9' {
					numArray = append([]byte{b}, numArray...)
				} else {
					break
				}
			}

			if len(numArray) == 0 {
				continue
			}

			queue, err := strconv.Atoi(string(numArray))
			if err != nil {
				continue
			}

			if i, ok := queue2Irq[queue]; ok {
				klog.Errorf("%s: %d: %s queue %d has multiple irqs {%d, %d}", nicInfo.NetNSName, nicInfo.IfIndex, nicInfo.Name, queue, i, irq)
				continue
			}
			queue2Irq[queue] = irq
		}
	}

	return queue2Irq, nil
}

// nic queue naming in /proc/interrrupts, refer to below files
// https://bytedance.larkoffice.com/docx/QbVGdhk5LoIRYaxJlficHLZJnHe
// https://bytedance.larkoffice.com/docx/HN10digxVoviElxy6nXcChBqnJc
// https://bytedance.larkoffice.com/wiki/ILS7wV43Ji0DMiknRsOcOwZUnwg
// https://bytedance.larkoffice.com/wiki/wikcnMfA9FDb2V5ZrcHc1GVnL6m
func GetNicQueue2Irq(nicInfo *NicBasicInfo) (map[int]int, error) {
	queueFilter := nicInfo.Name
	queueDelimeter := "-"
	if nicInfo.IsVirtioNetDev {
		queueFilter = fmt.Sprintf("%s-input", nicInfo.VirtioNetName)
		queueDelimeter = "."
	}

	queue2Irq, err := GetNicQueue2IrqWithQueueFilter(nicInfo, queueFilter, queueDelimeter)
	if err != nil {
		return nil, fmt.Errorf("GetNicQueue2Irqs(%s, %s), err %v", queueFilter, queueDelimeter, err)
	}

	if len(queue2Irq) > 0 {
		return queue2Irq, nil
	}

	// below fallback filters are not for virito-net device
	if nicInfo.IsVirtioNetDev {
		return nil, fmt.Errorf("failed to find matched queue in %s for virtio nic %d: %s, virtio name: %s", InterruptsFile, nicInfo.IfIndex, nicInfo.Name, nicInfo.VirtioNetName)
	}

	// some mlx driver version naming queue in /proc/interrupts as format mlx5_comp<queue number>@pci:<pci addr>, like mlx5_comp52@pci:0000:5e:00.0
	if nicInfo.PCIAddr != "" {
		queueFilter = nicInfo.PCIAddr
		queueDelimeter = "@"

		queue2Irq, err := GetNicQueue2IrqWithQueueFilter(nicInfo, queueFilter, queueDelimeter)
		if err != nil {
			return nil, fmt.Errorf("failed to GetNicQueue2IrqWithQueueFilter(%s, %s), err %v", queueFilter, queueDelimeter, err)
		}

		if len(queue2Irq) > 0 {
			return queue2Irq, nil
		}
	}

	// mlx/bnx sriov naming vf name as ethx_y, like eth0_6, vf's queue name still use ethx, but the same PF's all VFs share the same queue name in /proc/interrupts
	cols := strings.Split(nicInfo.Name, "_")
	if len(cols) == 2 {
		queueFilter = cols[0]
		queueDelimeter = "-"

		queue2Irq, err := GetNicQueue2IrqWithQueueFilter(nicInfo, queueFilter, queueDelimeter)
		if err != nil {
			return nil, fmt.Errorf("failed to GetNicQueue2IrqWithQueueFilter(%s, %s), err %v", queueFilter, queueDelimeter, err)
		}

		if len(queue2Irq) > 0 {
			return queue2Irq, nil
		}
	}

	return nil, fmt.Errorf("failed to find matched queue in %s for %d: %s", InterruptsFile, nicInfo.IfIndex, nicInfo.Name)
}

// before irq tuning initialization, one irq's smp_affinity_list may has multiple cpus
// after irq tuning initialization, one irq only affinity one cpu.
func GetIrqsAffinityCPUs(irqs []int) (map[int][]int64, error) {
	irq2CPUs := make(map[int][]int64)

	for _, irq := range irqs {
		cpus, err := GetIrqAffinityCPUs(irq)
		if err != nil {
			return nil, fmt.Errorf("failed to GetIrqAffinityCPUs(%d), err %v", irq, err)
		}
		irq2CPUs[irq] = cpus
	}
	return irq2CPUs, nil
}

// re-configure irqs's affinity according to irq's cpu assingment(kernel apic_set_affinity -> irq_matrix_alloc ->  matrix_find_best_cpu)
// to make one irq affinity only one cpu.
// TidyUpNicIrqsAffinityCPUs may change irqs's actual affinitied cpu, because we donnot know these cpus's affinitied irqs of other devices, include irqs of other nics,
// it's corner case that need to tidy up irqs affinity.
func TidyUpNicIrqsAffinityCPUs(irq2CPUs map[int][]int64) (map[int]int64, error) {
	var irqs []int
	for irq := range irq2CPUs {
		irqs = append(irqs, irq)
	}
	// irq-balance service configure irqs's affinity generally in irq's ascending order
	sort.Ints(irqs)

	cpuAffinitiedIrqsCount := make(map[int64]int)
	irq2CPU := make(map[int]int64)

	for _, irq := range irqs {
		affinityCPU := int64(-1)
		affinityCPUIrqsCount := -1
		cpus := irq2CPUs[irq]
		for _, cpu := range cpus {
			count, ok := cpuAffinitiedIrqsCount[cpu]
			if !ok {
				affinityCPU = cpu
				break
			}
			if affinityCPUIrqsCount == -1 || count < affinityCPUIrqsCount {
				affinityCPU = cpu
				affinityCPUIrqsCount = count
			}
		}

		err := SetIrqAffinity(irq, affinityCPU)
		if err != nil {
			return nil, fmt.Errorf("failed to SetIrqAffinity(%d, %d), err: %v", irq, affinityCPU, err)
		}

		if _, ok := cpuAffinitiedIrqsCount[affinityCPU]; ok {
			cpuAffinitiedIrqsCount[affinityCPU]++
		} else {
			cpuAffinitiedIrqsCount[affinityCPU] = 1
		}
		irq2CPU[irq] = affinityCPU
	}

	return irq2CPU, nil
}

func getNicDriver(nicSysPath string) (NicDriver, error) {
	linkPath := filepath.Join(nicSysPath, "device/driver")

	target, err := os.Readlink(linkPath)
	if err != nil {
		return NicDriverUnknown, fmt.Errorf("failed to read symlink for %s: err %v", linkPath, err)
	}

	driverName := filepath.Base(target)

	var driver NicDriver
	if strings.HasPrefix(driverName, string(NicDriverMLX)) {
		driver = NicDriverMLX
	} else if strings.HasPrefix(driverName, string(NicDriverBNX)) {
		driver = NicDriverBNX
	} else if strings.HasPrefix(driverName, string(NicDriverVirtioNet)) {
		driver = NicDriverVirtioNet
	} else if strings.HasPrefix(driverName, string(NicDriverI40E)) {
		driver = NicDriverI40E
	} else if strings.HasPrefix(driverName, string(NicDriverIXGBE)) {
		driver = NicDriverIXGBE
	} else {
		driver = NicDriverUnknown
	}

	return driver, nil
}

// get nic's all irqs, inlcuding rx-tx irqs, and some irqs used for control
func GetNicIrqs(nicSysPath string) ([]int, error) {
	msiIrqsDir := filepath.Join(nicSysPath, "device/msi_irqs")

	dirEnts, err := os.ReadDir(msiIrqsDir)
	if err != nil {
		return nil, fmt.Errorf("failed to ReadDir(%s), err %v", msiIrqsDir, err)
	}

	var irqs []int
	for _, d := range dirEnts {
		irq, err := strconv.Atoi(d.Name())
		if err != nil {
			klog.Warningf("failed to Atoi(%s), err %v", d.Name(), err)
			continue
		}
		irqs = append(irqs, irq)
	}
	sort.Ints(irqs)
	return irqs, nil
}

// nicSysPath like "/sys/class/net/eth0"
// N.B., bnx sriov vf dose not hava corresponding /sys/class/net/ethx_y/device file
func IsPCIDevice(nicSysPath string) bool {
	devRealPath, err := filepath.EvalSymlinks(nicSysPath)
	if err != nil {
		klog.Warningf("failed to EvalSymlinks(%s), err %v", nicSysPath, err)
		return false
	}

	if strings.Contains(devRealPath, "devices/pci") {
		return true
	}
	return false
}

func GetNicPCIAddr(nicSysPath string) (string, error) {
	nicDevSysPath := filepath.Join(nicSysPath, "device")
	devRealPath, err := filepath.EvalSymlinks(nicDevSysPath)
	if err != nil {
		return "", fmt.Errorf("failed to EvalSymlinks(%s), err %v", nicDevSysPath, err)
	}
	if IsVirtioNetDevice(nicSysPath) {
		return filepath.Base(filepath.Dir(devRealPath)), nil
	}
	return filepath.Base(devRealPath), nil
}

func IsVirtioNetDevice(nicSysPath string) bool {
	nicDrvSysPath := filepath.Join(nicSysPath, "device/driver")
	nicDrvSysPath, err := filepath.EvalSymlinks(nicDrvSysPath)
	if err != nil {
		klog.Warningf("failed to EvalSymlinks(%s), err %v", nicDrvSysPath, err)
		return false
	}
	if filepath.Base(nicDrvSysPath) == VirtioNetDriver {
		return true
	}

	return false
}

func GetNicVirtioName(nicSysPath string) (string, error) {
	nicDevSysPath := filepath.Join(nicSysPath, "device")
	devRealPath, err := filepath.EvalSymlinks(nicDevSysPath)
	if err != nil {
		return "", fmt.Errorf("failed to EvalSymlinks(%s), err %v", nicDevSysPath, err)
	}
	return filepath.Base(devRealPath), nil
}

// /sys/class/net/ethX/device/numa_node not exist, or contains negtive value, then return -1 as unknown socket bind
func GetNicBindedSocket(nicSysPath string) (int, error) {
	nicBindNumaPath := filepath.Join(nicSysPath, "device/numa_node")
	if _, err := os.Stat(nicBindNumaPath); err != nil {
		// no /sys/class/net/ethX/device/numa_node file for virtio-net device, but numa_node can be acquired by general method
		// that read pci device's numa_node, like "/sys/devices/pci0000:16/0000:16:02.0/numa_node", which is available for all PCI devices.
		if os.IsNotExist(err) {
			nicDevPath := filepath.Join(nicSysPath, "device")
			devRealPath, err := filepath.EvalSymlinks(nicDevPath)
			if err != nil {
				return UnknownSocketBind, fmt.Errorf("failed to EvalSymlinks(%s), err %s", nicDevPath, err)
			}

			nicBindNumaPath = filepath.Join(filepath.Dir(devRealPath), "numa_node")
			if _, err := os.Stat(nicBindNumaPath); err != nil {
				return UnknownSocketBind, err
			}
		} else {
			return UnknownSocketBind, fmt.Errorf("failed to Stat(%s), err %v", nicBindNumaPath, err)
		}
	}

	b, err := os.ReadFile(nicBindNumaPath)
	if err != nil {
		return UnknownSocketBind, fmt.Errorf("failed to ReadFile(%s), err %v", nicBindNumaPath, err)
	}

	numaStr := strings.TrimSpace(strings.TrimRight(string(b), "\n"))
	numa, err := strconv.Atoi(numaStr)
	if err != nil {
		return UnknownSocketBind, fmt.Errorf("failed to Atoi(%s), err %v", numaStr, err)
	}

	// -1 storeed in /sys/class/net/device/numa_node for passthrough non-virtio device (e.g. mellanox SRIOV VF)
	if numa < 0 {
		return UnknownSocketBind, nil
	}

	socketID, err := GetNumaPackageID(numa)
	if err != nil {
		return UnknownSocketBind, err
	}
	return socketID, nil
}

func GetIfIndex(nicName string, nicSysPath string) (int, error) {
	iface, err := net.InterfaceByName(nicName)
	if err != nil {
		klog.Warningf("failed to InterfaceByName(%s), err %v", nicName, err)
		ifIndex, err := GetIfIndexFromSys(nicSysPath)
		if err != nil {
			return -1, fmt.Errorf("failed to GetIfIndexFromSys(%s), err %v", nicSysPath, err)
		}
		return ifIndex, err
	}
	return iface.Index, nil
}

func GetIfIndexFromSys(nicSysPath string) (int, error) {
	ifIndexPath := filepath.Join(nicSysPath, "ifindex")
	b, err := os.ReadFile(ifIndexPath)
	if err != nil {
		return 0, err
	}

	ifindex, err := strconv.Atoi(strings.TrimSpace(strings.TrimRight(string(b), "\n")))
	if err != nil {
		return 0, err
	}

	return ifindex, nil
}

func IsBondingNetDevice(nicSysPath string) bool {
	if stat, err := os.Stat(filepath.Join(nicSysPath, "bonding")); err == nil && stat.IsDir() {
		return true
	}
	return false
}

func IsNetDevLinkUp(nicSysPath string) bool {
	nicLinkStatusFile := filepath.Join(nicSysPath, "operstate")
	b, err := os.ReadFile(nicLinkStatusFile)
	if err != nil {
		klog.Warningf("failed to ReadFile(%s), err %v", nicLinkStatusFile, err)
		return false
	}

	status := strings.TrimSpace(strings.TrimRight(string(b), "\n"))

	return status == "up"
}

func ListBondNetDevSlaves(nicSysPath string) ([]string, error) {
	if !IsBondingNetDevice(nicSysPath) {
		return nil, fmt.Errorf("%s is not bonding netdev", nicSysPath)
	}

	bondSlavesPath := filepath.Join(nicSysPath, "bonding/slaves")
	b, err := os.ReadFile(bondSlavesPath)
	if err != nil {
		return nil, fmt.Errorf("failed to ReadFile(%s), err %v", bondSlavesPath, err)
	}

	slaves := strings.Fields(strings.TrimRight(string(b), "\n"))

	return slaves, nil
}

func ListNetNS() ([]NetNSInfo, error) {
	hostNetNSInode, err := GetProcessNameSpaceInode(1, NetNS)
	if err != nil {
		return nil, fmt.Errorf("failed to GetProcessNameSpaceInode(1, %s), err %v", NetNS, err)
	}

	nsList := []NetNSInfo{
		{
			NetNSName:  HostNetNSName,
			NetNSInode: hostNetNSInode,
		},
	}

	dirEnts, err := os.ReadDir(NetNSRunDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nsList, nil
		}
		return nil, err
	}

	for _, d := range dirEnts {
		if !d.IsDir() {
			netnsPath := filepath.Join(NetNSRunDir, d.Name())
			inode, err := GetFileInode(netnsPath)
			if err != nil {
				return nil, fmt.Errorf("failed to GetFileInode(%s), err %v", netnsPath, err)
			}

			nsList = append(nsList, NetNSInfo{
				NetNSName:  d.Name(),
				NetNSInode: inode,
			})
		}
	}

	return nsList, nil
}

func netnsEnter(nsName string) (*netnsSwitchContext, error) {
	var retErr error

	// need to LockOSThread during switch netns
	runtime.LockOSThread()

	defer func() {
		if retErr != nil {
			runtime.UnlockOSThread()
		}
	}()

	if nsName == HostNetNSName {
		return &netnsSwitchContext{
			newNetNSName: nsName,
			sysMountDir:  DefaultNetNSSysDir,
		}, nil
	}

	originalNetNSHdl, err := netns.Get()
	if err != nil {
		retErr = fmt.Errorf("failed to netns.Get, err %v", err)
		return nil, retErr
	}

	defer func() {
		if retErr != nil {
			originalNetNSHdl.Close()
		}
	}()

	newNetNSPath := path.Join(NetNSRunDir, nsName)
	newNetNSHdl, err := netns.GetFromPath(newNetNSPath)
	if err != nil {
		retErr = fmt.Errorf("failed to GetFromPath(%s), err %v", newNetNSPath, err)
		return nil, retErr
	}
	defer func() {
		if retErr != nil {
			newNetNSHdl.Close()
		}
	}()

	if err := netns.Set(newNetNSHdl); err != nil {
		retErr = fmt.Errorf("failed to setns to %s, err %v", nsName, retErr)
		return nil, retErr
	}

	defer func() {
		if retErr != nil {
			// switch back to original netns
			if err := netns.Set(originalNetNSHdl); err != nil {
				klog.Fatalf("failed to set netns to host netns, err %v", err)
			}
		}
	}()

	// create the target directory if it doesn't exist
	if _, err := os.Stat(TmpNetNSSysDir); err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(TmpNetNSSysDir, os.FileMode(0755)); err != nil {
				retErr = fmt.Errorf("failed to MkdirAll(%s), err %v", TmpNetNSSysDir, err)
				return nil, retErr
			}
		} else {
			retErr = fmt.Errorf("failed to Stat(%s), err %v", TmpNetNSSysDir, err)
			return nil, retErr
		}
	}

	if err := syscall.Mount("sysfs", TmpNetNSSysDir, "sysfs", 0, ""); err != nil {
		retErr = fmt.Errorf("failed to Mount(%s), err %v", TmpNetNSSysDir, retErr)
		return nil, retErr
	}

	return &netnsSwitchContext{
		originalNetNSHdl: originalNetNSHdl,
		newNetNSName:     nsName,
		newNetNSHdl:      newNetNSHdl,
		sysMountDir:      TmpNetNSSysDir,
		sysDirRemounted:  true,
	}, nil
}

// netnsExit MUST be called after netnsEnter susccessfully
func (nsc *netnsSwitchContext) netnsExit() {
	defer runtime.UnlockOSThread()

	if nsc.newNetNSName == HostNetNSName {
		return
	}

	if nsc.sysDirRemounted && nsc.sysMountDir != "" {
		if err := syscall.Unmount(nsc.sysMountDir, 0); err != nil {
			klog.Fatalf("failed to Unmount(%s), err %v", nsc.sysMountDir, err)
		}
	}

	nsc.newNetNSHdl.Close()

	if err := netns.Set(nsc.originalNetNSHdl); err != nil {
		klog.Fatalf("failed to set netns to host netns, err %v", err)
	}
	nsc.originalNetNSHdl.Close()
}

func ListActiveUplinkNicsFromNetNS(netnsInfo NetNSInfo) ([]*NicBasicInfo, error) {
	nsc, err := netnsEnter(netnsInfo.NetNSName)
	if err != nil {
		return nil, fmt.Errorf("failed to netnsEnter(%s), err %v", netnsInfo.NetNSName, err)
	}
	defer nsc.netnsExit()

	interfacesWithIPAddr, err := ListInterfacesWithIPAddr()
	interfacesWithIPAddrMap := make(map[string]interface{})
	for _, i := range interfacesWithIPAddr {
		interfacesWithIPAddrMap[i] = nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed ListInterfacesWithIPAddr, err %v", err)
	}

	netSysDir := filepath.Join(nsc.sysMountDir, ClassNetBasePath)
	dirEnts, err := os.ReadDir(netSysDir)
	if err != nil {
		return nil, fmt.Errorf("failed to ReadDir(%s), err %v", netSysDir, err)
	}

	var activeUplinkNics []string
	for _, d := range dirEnts {
		nicName := d.Name()
		nicSysPath := filepath.Join(netSysDir, nicName)

		if _, ok := interfacesWithIPAddrMap[nicName]; !ok {
			continue
		}

		if IsPCIDevice(nicSysPath) && IsNetDevLinkUp(nicSysPath) {
			activeUplinkNics = append(activeUplinkNics, nicName)
			continue
		}

		if IsBondingNetDevice(nicSysPath) {
			slaves, err := ListBondNetDevSlaves(nicSysPath)
			if err != nil {
				klog.Warningf("failed to ListBondNetDevSlaves(%s), err %v", nicSysPath, err)
				continue
			}

			// bonding's all link-up slaves are considered as avtive uplink nics regardless of bonding's mode
			for _, slave := range slaves {
				slaveSysPath := filepath.Join(netSysDir, slave)
				if IsPCIDevice(slaveSysPath) && IsNetDevLinkUp(slaveSysPath) {
					activeUplinkNics = append(activeUplinkNics, slave)
				}
			}
			continue
		}
	}

	var nics []*NicBasicInfo
	for _, n := range activeUplinkNics {
		nicSysPath := filepath.Join(netSysDir, n)

		ifIndex, err := GetIfIndex(n, nicSysPath)
		if err != nil {
			return nil, fmt.Errorf("failed to GetIfIndex(%s), err %v", n, err)
		}

		pciAddr, err := GetNicPCIAddr(nicSysPath)
		if err != nil {
			return nil, fmt.Errorf("failed to GetNicPCIAddr(%s), err %v", nicSysPath, err)
		}

		driver, err := getNicDriver(nicSysPath)
		if err != nil {
			return nil, fmt.Errorf("failed to getNicDriver(%s), err %v", nicSysPath, err)
		}

		bindedSocket, err := GetNicBindedSocket(nicSysPath)
		if err != nil {
			return nil, fmt.Errorf("failed to GetNicBindedSocket(%s), err %v", nicSysPath, err)
		}

		isVirtioNetDev := IsVirtioNetDevice(nicSysPath)
		virtioNetDevName := ""
		if isVirtioNetDev {
			virtioNetDevName, err = GetNicVirtioName(nicSysPath)
			if err != nil {
				return nil, fmt.Errorf("failed to GetNicVirtioName(%s), err %v", nicSysPath, err)
			}
		}

		irqs, err := GetNicIrqs(nicSysPath)
		if err != nil {
			// if there are two active uplink nics from different netns with the same nic name,
			// then we cannot tell apart two nic's irqs in /proc/interrupts,
			// we can use irqs from /sys/class/net/ethX/device/msi_irqs to resolve above confilicts in /proc/interrupts,
			// e.g., two nics with the same name "eth0", we can filter all eth0 irqs in /proc/interrupts, then further filter irq based on nic's msi irqs.
			// so even though get nic's msi irqs failed, needless to exit, print warning log is enough
			if !isVirtioNetDev {
				klog.Warningf("failed to GetNicIrqs(%s), err %v", nicSysPath, err)
			}
		}

		nicInfo := &NicBasicInfo{
			NetNSInfo:      netnsInfo,
			Name:           n,
			IfIndex:        ifIndex,
			PCIAddr:        pciAddr,
			Driver:         driver,
			SocketBind:     bindedSocket,
			IsVirtioNetDev: isVirtioNetDev,
			VirtioNetName:  virtioNetDevName,
			Irqs:           irqs,
		}
		nics = append(nics, nicInfo)
	}

	return nics, nil
}

// copied from https://github.com/kubewharf/katalyst-core/blob/main/pkg/util/machine/network_linux.go#L248
func ListInterfacesWithIPAddr() ([]string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	var intfs []string

	for _, i := range interfaces {
		// if the interface is down or a loopback interface, we just skip it
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback > 0 {
			continue
		}

		addrs, err := i.Addrs()
		if err != nil {
			continue
		}

		if len(addrs) == 0 {
			continue
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			default:
				continue
			}

			// filter out ips that are not global uni-cast
			if !ip.IsGlobalUnicast() {
				continue
			}

			intfs = append(intfs, i.Name)
			break
		}
	}

	return intfs, nil
}

func ListActiveUplinkNics() ([]*NicBasicInfo, error) {
	netnsList, err := ListNetNS()
	if err != nil {
		return nil, fmt.Errorf("failed to ListNetNS, err %v", err)
	}

	var nics []*NicBasicInfo
	for _, ns := range netnsList {
		netnsNics, err := ListActiveUplinkNicsFromNetNS(ns)
		if err != nil {
			return nil, fmt.Errorf("failed to ListActiveUplinkNicsFromNetNS(%s), err %v", ns.NetNSName, err)
		}

		for _, n := range netnsNics {
			queue2Irq, err := GetNicQueue2Irq(n)
			if err != nil {
				return nil, fmt.Errorf("failed to GetNicQueue2Irq for %d: %s, err %v", n.IfIndex, n.Name, err)
			}

			irq2Queue := make(map[int]int)
			for queue, irq := range queue2Irq {
				irq2Queue[irq] = queue
			}

			n.QueueNum = len(queue2Irq)
			n.Queue2Irq = queue2Irq
			n.Irq2Queue = irq2Queue
			nics = append(nics, n)
		}
	}

	return nics, nil
}

func GetNetDevRxPackets(netns string, nicName string) (uint64, error) {
	nsc, err := netnsEnter(netns)
	if err != nil {
		return 0, fmt.Errorf("failed to netnsEnter(%s), err %v", netns, err)
	}
	defer nsc.netnsExit()

	sysRxPacketsFile := filepath.Join(nsc.sysMountDir, ClassNetBasePath, nicName, "statistics/rx_packets")
	if _, err := os.Stat(sysRxPacketsFile); err == nil {
		rxPacket, err := ReadUint64FromFile(sysRxPacketsFile)
		if err == nil {
			return rxPacket, nil

		}
		klog.Errorf("failed to ReadUint64FromFile(%s), err %v", sysRxPacketsFile, err)
	} else {
		klog.Warningf("failed to stat(%s), err %v", sysRxPacketsFile, err)
	}

	// read rx packets from /proc/net/dev
	lines, err := ReadLines(NetDevProcFile)
	if err != nil {
		return 0, fmt.Errorf("failed to ReadLines(%s), err %v", NetDevProcFile, err)
	}

	if len(lines) < 3 {
		return 0, fmt.Errorf("%s lines count %d, less than 3", NetDevProcFile, len(lines))
	}

	nicNameFilter := fmt.Sprintf("%s:", nicName)

	for _, line := range lines {
		cols := strings.Fields(line)
		if len(cols) < 3 {
			continue
		}

		if cols[0] != nicNameFilter {
			continue
		}

		rxPackets, err := strconv.ParseUint(cols[2], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("failed to ParseUint, err: %v", err)
		}
		return rxPackets, nil
	}

	return 0, fmt.Errorf("failed to find %s in %s", nicName, NetDevProcFile)
}

func GetNicRxQueuePackets(nic *NicBasicInfo) (map[int]uint64, error) {
	driver := nic.Driver
	if driver != NicDriverMLX && driver != NicDriverBNX && driver != NicDriverVirtioNet &&
		driver != NicDriverI40E && driver != NicDriverIXGBE {
		return nil, fmt.Errorf("unknow driver: %s", driver)
	}

	nsc, err := netnsEnter(nic.NetNSName)
	if err != nil {
		return nil, fmt.Errorf("failed to netnsEnter(%s), err %v", nic.NetNSName, err)
	}
	defer nsc.netnsExit()

	// Create a new Ethtool instance
	ethHandler, err := ethtool.NewEthtool()
	if err != nil {
		return nil, err
	}
	defer ethHandler.Close()

	// Get statistics for the interface
	stats, err := ethHandler.Stats(nic.Name)
	if err != nil {
		return nil, err
	}

	rxQueuePackets := make(map[int]uint64)

	for key, val := range stats {
		var queueStr string
		key = strings.TrimSpace(key)

		if driver == NicDriverMLX {
			// mlx nic rx queue packets key:val format
			// key: rx60_packets
			// val: 89462756888
			if !strings.HasPrefix(key, "rx") {
				continue
			}

			if !strings.HasSuffix(key, "_packets") {
				continue
			}

			queueStr = strings.TrimSuffix(strings.TrimPrefix(key, "rx"), "_packets")
		} else if driver == NicDriverBNX {
			// bnxt nic rx queue packets key:val format
			// key: [6]: rx_ucast_packets
			// val: 8304
			cols := strings.Fields(key)
			if len(cols) != 2 {
				continue
			}

			if cols[1] != "rx_ucast_packets" {
				continue
			}

			queueStr = strings.TrimSuffix(strings.TrimPrefix(cols[0], "["), "]:")
		} else if driver == NicDriverVirtioNet {
			// virtio_net nic rx queue packets key:val format
			// key: rx_queue_6_packets
			// val: 51991567584
			if !strings.HasPrefix(key, "rx_queue_") {
				continue
			}

			if !strings.HasSuffix(key, "_packets") {
				continue
			}

			queueStr = strings.TrimSuffix(strings.TrimPrefix(key, "rx_queue_"), "_packets")
		} else if driver == NicDriverI40E {
			// i40e nic rx queue packets key:val format
			// key: rx-39.packets
			// val: 2769830722
			if !strings.HasPrefix(key, "rx-") {
				continue
			}

			if !strings.HasSuffix(key, ".packets") {
				continue
			}

			queueStr = strings.TrimSuffix(strings.TrimPrefix(key, "rx-"), ".packets")
		} else if driver == NicDriverIXGBE {
			// ixgbe nic rx queue packets key:val format
			// key: rx_queue_19_packets
			// val: 3994706599
			if !strings.HasPrefix(key, "rx_queue_") {
				continue
			}

			if !strings.HasSuffix(key, "_packets") {
				continue
			}

			queueStr = strings.TrimSuffix(strings.TrimPrefix(key, "rx_queue_"), "_packets")
		}

		queue, err := strconv.Atoi(queueStr)
		if err != nil {
			continue
		}

		if queue < 0 || queue >= nic.QueueNum {
			continue
		}
		rxQueuePackets[queue] = val
	}

	return rxQueuePackets, nil
}

func (n *NicBasicInfo) String() string {
	if n.NetNSName == "" {
		return fmt.Sprintf("%d: %s", n.IfIndex, n.Name)
	} else {
		return fmt.Sprintf("%s %d: %s", n.NetNSName, n.IfIndex, n.Name)
	}
}

func (n *NicBasicInfo) Equal(other *NicBasicInfo) bool {
	if other == nil {
		return false
	}

	if n.NetNSName != other.NetNSName || n.NetNSInode != other.NetNSInode || n.Name != other.Name ||
		n.IfIndex != other.IfIndex || n.PCIAddr != other.PCIAddr || n.SocketBind != other.SocketBind ||
		n.IsVirtioNetDev != other.IsVirtioNetDev || n.VirtioNetName != other.VirtioNetName || n.QueueNum != other.QueueNum {
		return false
	}

	if len(n.Irqs) != len(other.Irqs) {
		return false
	}

	sort.Ints(n.Irqs)
	sort.Ints(other.Irqs)
	for i := range n.Irqs {
		if n.Irqs[i] != other.Irqs[i] {
			return false
		}
	}

	if len(n.Queue2Irq) != len(other.Queue2Irq) {
		return false
	}

	for queue, irq := range other.Queue2Irq {
		if i, ok := n.Queue2Irq[queue]; !ok || i != irq {
			return false
		}
	}

	if len(n.Irq2Queue) != len(other.Irq2Queue) {
		return false
	}

	for irq, queue := range other.Irq2Queue {
		if q, ok := n.Irq2Queue[irq]; !ok || q != queue {
			return false
		}
	}

	return true
}

func CollectSoftNetStats(onlineCpus map[int64]bool) (map[int64]*SoftNetStat, error) {
	lines, err := ReadLines(SoftnetStatFile)
	if err != nil {
		return nil, err
	}

	onlineCpuIDMax := int64(-1)
	for cpu := range onlineCpus {
		if onlineCpuIDMax == -1 || cpu > onlineCpuIDMax {
			onlineCpuIDMax = cpu
		}
	}

	softnetStats := make(map[int64]*SoftNetStat)

	cpu := int64(0)
	for _, line := range lines {
		cols := strings.Fields(line)
		if len(cols) < 3 {
			continue
		}

		processed, err := strconv.ParseUint(cols[0], 16, 64)
		if err != nil {
			return nil, err
		}

		timeSqueeze, err := strconv.ParseUint(cols[2], 16, 64)
		if err != nil {
			return nil, err
		}

		matchedCpu := int64(-1)
		for ; cpu <= onlineCpuIDMax; cpu++ {
			if _, ok := onlineCpus[cpu]; ok {
				matchedCpu = cpu
				cpu++
				break
			}
		}

		if matchedCpu == -1 {
			klog.Warningf("failed to find matched cpu")
			continue
		}

		softnetStats[matchedCpu] = &SoftNetStat{
			ProcessedPackets:   processed,
			TimeSqueezePackets: timeSqueeze,
		}
	}

	return softnetStats, nil
}

func CollectNetRxSoftirqStats() (map[int64]uint64, error) {
	lines, err := ReadLines(SoftIrqsFile)
	if err != nil {
		return nil, err
	}

	var cpuSoftirqsStr []string
	for _, line := range lines {
		cols := strings.Fields(line)

		if len(cols) == 0 {
			continue
		}

		if cols[0] == "NET_RX:" {
			cpuSoftirqsStr = cols[1:]
			break
		}
	}

	if len(cpuSoftirqsStr) == 0 {
		return nil, fmt.Errorf("failed to find NET_RX softirq stats in %s", SoftIrqsFile)
	}

	cpuSoftirqCount := make(map[int64]uint64)
	for cpu, softirqStr := range cpuSoftirqsStr {
		softirqCount, err := strconv.ParseUint(softirqStr, 10, 64)
		if err != nil {
			klog.Errorf("failed to ParseUint, err %s", err)
			continue
		}
		cpuSoftirqCount[int64(cpu)] = softirqCount
	}

	return cpuSoftirqCount, nil
}
