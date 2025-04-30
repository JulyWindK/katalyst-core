package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/cpuid/v2"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/dynamicpolicy/irqtuner"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/dynamicpolicy/irqtuner/config"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/dynamicpolicy/irqtuner/util"
	dynconfig "github.com/kubewharf/katalyst-core/pkg/config/agent/dynamic"
	"github.com/kubewharf/katalyst-core/pkg/util/machine"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

const (
	NicsSyncInterval      = 600 // seconds
	IrqBalanceNGProcComm  = "irqbalance-ng"
	IrqTuningLogPrefix    = "irq-tuning controller"
	KataRuntimeClassName  = "kata-clh"
	KataBMAnnotationName  = "bytedance.com/kata-bm"
	KataBMAnnotationValue = "true"
)

// specific errors
var ErrNotFoundProperDestIrqCore = errors.New("not found proper dest irq core for irq balance")

// [irq affinity policy transitions]
//
// init-tuning is new-created nic's init policy, which is not a real irq affinity policy, just an initial state, cannot be used to control nic's irq affinity,
// after successfully complete first round periodicTuning, irq affinity policy will be switched from init-tuning to a real irq affinity policy.
// Generally, irq affinity policy will be switched from init-tuning to either of irq-cores-exclusive and irq-cores-fair after complete first round periodicTuning,
// and then irq affinity policy switches between irq-cores-exclusive and irq-cores-fair as needed according to nic rx pps, configration and related policies.
// when irq affinity exceptions happened in a large scale nodes, we will use kcc to notify each node's katalyst (irq tuning manager) unconditionall roll back to irqbalance-ng.service
// supported irq affinity policy, and never switch back again unless reset kcc configuration and restart katalyst.
//
//
//	                         +---------------------+
//                    +----->|                     |-------+
//	                  |      | irq-cores-exclusive |       |
//	                  |   +--|                     |<-+    |
//	                  |   |  +---------------------+  |    |
//	 init state       |   |                           |    |     when irq affnitiy exceptions happened in a large scale nodes, use kcc to notify each node's katalyst to roll back to this policy
//	+-------------+   |   |                           |    |       +----------------+
//	| init-tuning |---+   |                           |    +------>| irq-balance-ng |
//	+-------------+   |   |                           |    |       +----------------+
//	       |          |   |                           |    |               ^
//	       |          |   |                           |    |               |
//         |          |   |  +---------------------+  |    |               |
//	       |          |   +->|                     |--+    |               |
//	       |          |      |   irq-cores-fair    |       |               |
//	       |          +----->|                     |-------+               |
//	       |                 +---------------------+                       |
//	       |                                                               |
//	       +---------------------------------------------------------------+
//
//
// [future plans]
// 1. we may support irq affinity policy switches back to either of irq-cores-exclusive and irq-cores-fair from irq-balance-ng if necessary.
// 2. we may support static irq affinity policy that different scenario or machine type can adopt appropriate static policy, as opposed to the current dynamic switching
// between mulitple irq affinity polices based on conditions and rules.

type IrqAffinityPolicy string

const (
	// InitTuning means this nic is new created and affinity policy has not been decided, affinity policy will be set to one of below
	// after successfully complete first round periodicTuning.
	InitTuning IrqAffinityPolicy = "init-tuning"
	// dedicate a small number of cores exclusively to handle packets reception, these cores will be excluded from shared-cores,
	// dedicated-core, reclaimed-cores, .etc, but dose not affect cpu capacity for allocation, only affects the actual cpuset of containers.
	IrqCoresExclusive IrqAffinityPolicy = "irq-cores-exclusive"
	// this policy considers socket irq balance, and sriov container's irq affinity, and avoid overlapping with exclusive irq cores, etc.
	IrqBalanceFair IrqAffinityPolicy = "irq-balance-fair"
	// irq-balance-ng.service supported policy, when irq affinity exceptions happened in a large scale nodes,
	// we will use kcc to notify each node's katalyst to roll back irq affinity policy to this one.
	IrqBalanceNG IrqAffinityPolicy = "irq-balance-ng"
)

type CpuNetLoad interface {
	GetLoad() int
	GetCpuID() int64
}

type CPUUtil struct {
	CpuID      int64
	IrqUtil    int
	ActiveUtil int
}

func (c *CPUUtil) GetLoad() int {
	return c.IrqUtil
}

func (c *CPUUtil) GetCpuID() int64 {
	return c.CpuID
}

type CPUUtilSlice []*CPUUtil

func (x CPUUtilSlice) Len() int           { return len(x) }
func (x CPUUtilSlice) Less(i, j int) bool { return x[j].IrqUtil < x[i].IrqUtil }
func (x CPUUtilSlice) Swap(i, j int)      { x[i], x[j] = x[j], x[i] }

// sortCpuUtilSliceByIrqUtilInDecOrder sorts a slice of CPUUtil in decreasing order.
func sortCpuUtilSliceByIrqUtilInDecOrder(x []*CPUUtil) {
	sort.Sort(CPUUtilSlice(x))
}

// return value:
// map[int64]*CPUUtil: cpu level util, cpu id as may key
// *CPUUtil: average cpu util
func calculateCpuUtils(oldCpuStats, newCpuStats map[int64]*util.CPUStat, cpus []int64) ([]*CPUUtil, *CPUUtil) {
	var cpuUtils []*CPUUtil

	var allCpuIrqTimeDiff uint64
	var allCpuActiveTimeDiff uint64
	var allCpuTotalTimeDiff uint64

	for _, cpu := range cpus {
		oldStat, ok := oldCpuStats[cpu]
		if !ok {
			klog.Warningf("cpu not in old cpu stats")
			continue
		}

		newStat, ok := newCpuStats[cpu]
		if !ok {
			klog.Warningf("cpu not in new cpu stats")
			continue
		}

		oldIrqTime := oldStat.Irq + oldStat.Softirq
		oldActiveTime := oldStat.User + oldStat.Nice + oldStat.System + oldIrqTime + oldStat.Steal + oldStat.Guest + oldStat.GuestNice
		oldTotalTime := oldActiveTime + oldStat.Idle + oldStat.Iowait

		newIrqTime := newStat.Irq + newStat.Softirq
		newActiveTime := newStat.User + newStat.Nice + newStat.System + newIrqTime + newStat.Steal + newStat.Guest + newStat.GuestNice
		newTotalTime := newActiveTime + newStat.Idle + newStat.Iowait

		irqTimeDiff := newIrqTime - oldIrqTime
		activeTimeDiff := newActiveTime - oldActiveTime
		totalTimeDiff := newTotalTime - oldTotalTime

		if totalTimeDiff == 0 {
			cpuUtils = append(cpuUtils, &CPUUtil{
				CpuID:      cpu,
				IrqUtil:    0,
				ActiveUtil: 0,
			})
		} else {
			cpuUtils = append(cpuUtils, &CPUUtil{
				CpuID:      cpu,
				IrqUtil:    int(irqTimeDiff * 100 / totalTimeDiff),
				ActiveUtil: int(activeTimeDiff * 100 / totalTimeDiff),
			})
		}

		allCpuIrqTimeDiff += irqTimeDiff
		allCpuActiveTimeDiff += activeTimeDiff
		allCpuTotalTimeDiff += totalTimeDiff
	}

	var cpuUtilAvg *CPUUtil
	if allCpuTotalTimeDiff == 0 {
		cpuUtilAvg = &CPUUtil{
			IrqUtil:    0,
			ActiveUtil: 0,
		}
	} else {
		cpuUtilAvg = &CPUUtil{
			IrqUtil:    int(allCpuIrqTimeDiff * 100 / allCpuTotalTimeDiff),
			ActiveUtil: int(allCpuActiveTimeDiff * 100 / allCpuTotalTimeDiff),
		}
	}

	return cpuUtils, cpuUtilAvg
}

type QueuePPS struct {
	QueueID int
	PPS     uint64
}

type QueuePPSSlice []*QueuePPS

func (x QueuePPSSlice) Len() int           { return len(x) }
func (x QueuePPSSlice) Less(i, j int) bool { return x[j].PPS < x[i].PPS }
func (x QueuePPSSlice) Swap(i, j int)      { x[i], x[j] = x[j], x[i] }

// sortQueueuPPSSliceInDecOrder sorts a slice of QueuePPS in decreasing order.
func sortQueuePPSSliceInDecOrder(x []*QueuePPS) {
	sort.Sort(QueuePPSSlice(x))
}

func calculateQueuePPS(oldNicStats, newNicStats *NicStats, timeDiff float64) []*QueuePPS {
	if timeDiff <= 0 {
		return nil
	}

	if len(oldNicStats.RxQueuePackets) != len(newNicStats.RxQueuePackets) {
		return nil
	}

	var queuesPPS []*QueuePPS
	queueCount := len(oldNicStats.RxQueuePackets)
	for i := 0; i < queueCount; i++ {
		oldPackets, ok := oldNicStats.RxQueuePackets[i]
		if !ok {
			klog.Warningf("impossible, failed to find queue %d in nic old stats", i)
			continue
		}

		newPackets, ok := newNicStats.RxQueuePackets[i]
		if !ok {
			klog.Warningf("impossible, failed to find queue %d in nic new stats", i)
			continue
		}

		if newPackets < oldPackets {
			klog.Warningf("impossible, queue %d new packets less than old packets", i)
			continue
		}

		queuesPPS = append(queuesPPS, &QueuePPS{
			QueueID: i,
			PPS:     (newPackets - oldPackets) / uint64(timeDiff),
		})
	}

	return queuesPPS
}

// before irq tuning initialization, one irq's smp_affinity_list may has multiple cpus, Irq2CPUs is only used during irq tuning initialization
// so during irq initialization, we can re-configure irqs's affinity according to irq's cpu assingment(kernel apic_set_affinity -> irq_matrix_alloc -> matrix_find_best_cpu),
// to make one irq affinity only one cpu, so we can convert Irq2CPUs to Irq2CPU, then start to tuning irq cores's affinity.
type NicInfo struct {
	*util.NicBasicInfo
	Irq2Core       map[int]int64   // convert util.GetNicIrq2CPUs()'s return value to Irq2CPU, if there is a irq affinity multiple cpus, then re-write this irq's affinity to one cpu.
	SocketIrqCores map[int][]int64 // socket id as map key
	// IrqCore2Socket map[int]int64 // core id as map key, dynamic get by NicInfo.getIrqCore2SocketMap
	// IrqCoreAffinitiedIrqs map[int64][]int // core id as map key, dynamic get by NicInfo.getIrqCoreAffinitiedIrqs
	// IrqCores []int64 // dynamic get by NicInfo.getIrqCores
}

type IrqCoresExclusionSwitchStat struct {
	IrqCoresExclusionLastSwitchTime time.Time // time of last enable/disable irq cores exclusion, interval of successive enable/disable irq cores exclusion MUST >= specified threshold
	EnableExclusionThreshSuccCount  int       // succssive count of rx pps >= irq cores exclusion enable threshold
	DisableExclusionThreshSuccCount int       // successive count of rx pps <= irq cores exclusion disable threshold
}

type IrqAffinityTuning struct {
	SourceCore int64
	DestCore   int64
}

type IrqLoadBalance struct {
	SourceCores []int64
	DestCores   []int64
	IrqTunings  map[int]*IrqAffinityTuning // irq as map key
	TimeStamp   time.Time                  // balance performing time
}

type ExclusiveIrqCoresAdjust struct {
	Number    int       // negtive value means irq cores decrease, positive value means irq cores increase
	Cores     []int64   // useless for now
	TimeStamp time.Time // adjustment performing time
}

type TuningRecords struct {
	LastIrqLoadBalance          *IrqLoadBalance          // only record last balance
	IrqLoadBalancePingPongCount int                      // record count of pingpong irq load balance
	LastExclusiveIrqCoresInc    *ExclusiveIrqCoresAdjust // only record last increase
	LastExclusiveIrqCoresDec    *ExclusiveIrqCoresAdjust // only record last decrease
}

type NicIrqTuningManager struct {
	Conf                  *config.IrqTuningConfig
	NicInfo               *NicInfo
	IrqAffinityPolicy     IrqAffinityPolicy
	FallbackToBalanceFair bool  // if server error happened in IrqCoresExclusive policy, then fallback to balance-fair policy, and cannot be changed back again.
	AssignedSockets       []int // assigned sockets which nic irqs should affinity to, which are determined by the number of active nics and nic's binded numa in physical topo
	IrqCoresExclusionSwitchStat
	TuningRecords
}

type NicStats struct {
	TotalRxPackets uint64         // /proc/net/dev
	RxQueuePackets map[int]uint64 // queue id as map key
}

// https://docs.kernel.org/scheduler/sched-stats.html
// /proc/PID/schedstat 2rd col (NS) is task schedwait, print/report irqcores ksoftirqd avg schedwait and non-irqcores ksoftirqd avg schedwait,
// in extreme cases, softirq usage maybe not high beacuse significant ksoftirqd schedwait.
// due to the interrupt suppression of NAPI, the frequency of NET_RX_SOFTIRQ on a CPU cannot represent the actual packet reception load,
// but it can serve as a reference.
type IndicatorsStats struct {
	NicStats           map[int]*NicStats           // nic ifindex as map key
	CPUStats           map[int64]*util.CPUStat     // core id as map key
	SoftNetStats       map[int64]*util.SoftNetStat // core id as map key
	KsoftirqdSchedWait map[int]uint64
	NetRxSoftirqCount  map[int64]uint64 // cpu id as map key, only care net rx softirq counts on irq cores
	UpdateTime         time.Time
}

// IrqAffinityChange includes 3 types of changes,
// 1) irq affinity policy change
// 2) exclusive irq cores number change
// 3) irqs affinity change but exclusive irq cores number not change,
// here mainly introduce 1) and 2).
// Generally, irq affinity policy change will also cause irq cores change, however this is not definite, because we cannot infer the truely policy
// before katalyst restart, irq affinity policy is always set to InitTuning after katalyst restart. Then after calculation and redecides, there
// is a high probability that the original policy will be restored, then it's possible needlees to change irq cores if network load has no noticeable
// change and specicial containers's cpuset has no change.

// [irq affinity changes]
// There are 7 kind of irq affinity policy changes, 4 of which will also cause exclusive irq cores change.
// 1. InitTuning -> IrqBalanceFair
// 2. InitTuning -> IrqCoresExclusive, will also cause exclusive irq cores change
// 3. IrqCoresExclusive -> IrqBalanceFair, will also cause exclusive irq cores change
// 4. IrqBalanceFair -> IrqCoresExclusive, will also cause exclusive irq cores change
// 5. InitTuning -> IrqBalanceNG
// 6. IrqBalanceFair -> IrqBalanceNG
// 7. IrqCoresExclusive -> IrqBalanceNG, will also cause exclusive irq cores change.
//
// [irq cores change]
// There are many kind of irq cores changes, but we only care about if exclusive irq cores changed,
// there are 6 kinds of exclusive irq cores change, 4 of which are caused by irq affinity policy changes mentioned above when explain irq affinity policy changes,
// 2 of which are caused by increase/decreas exclusive irq cores but irq affinity policy keep IrqCoresExclusive unchanged.
//  1. irq affinity policy change: InitTuning -> IrqCoresExclusive
//  2. irq affinity policy change: IrqBalanceFair -> IrqCoresExclusive
//  3. irq affinity policy change: IrqCoresExclusive -> IrqBalanceFair
//  4. irq affinity policy change: IrqCoresExclusive -> IrqBalanceNG
//  5. when nic already enable irq cores exclusive (nic.IrqAffinityPolicy == IrqCoresExclusive), increase exclusive irq cores
//     when exclusive irq cores totoal load excleeds configured thresholds of trigger increase cores.
//  6. when nic already enable irq cores exclusive (nic.IrqAffinityPolicy == IrqCoresExclusive), decrease exclusive irq cores
//     when exclusive irq cores totoal load under configured thresholds  of trigger decrease cores.
type IrqAffinityChange struct {
	Nic                  *NicIrqTuningManager
	OldIrqAffinityPolicy IrqAffinityPolicy
	NewIrqAffinityPolicy IrqAffinityPolicy
	OldIrqCores          []int64
	NewIrqCores          []int64
	IrqsBalanced         bool
}

// shared-cores(include snb) sriov containers's irq affinity will be tuned as balance-fair policy.
// dedicated-cores and reclaimed-cores sriov container's irqs will be affinitied to container self cpuset.
// dedicated-cores sriov container's cpus should be excluded from cpu allocation for other nic's balance-fair irq affinity,
// shared-cores(inlcue snb) and reclaimed-cores sriov container's irqs should be counted when calculating each core's irq count,
// which is used to select target cpu for balance-fair irq affinity.
type ContainerInfoWrapper struct {
	*irqtuner.ContainerInfo
	IsSriovContainer bool
	Nics             []*NicInfo // nics of sriov container
}

type IrqTuningController struct {
	dynamicConfHolder    *dynconfig.DynamicAgentConfiguration
	conf                 *config.IrqTuningConfig
	CPUInfo              *util.CPUInfo
	Ksoftirqds           map[int64]int // cpuid as map key, ksoftirqd pid as value
	IrqStateAdapter      irqtuner.StateAdapter
	Containers           map[string]*ContainerInfoWrapper // container id as map key
	IrqAffForbiddenCores []int64
	// MetricClient, used to report some metrics

	NicSyncInterval int // interval of sync nic interval, periodic sync for active nics change, like nic number changed, nic queue number changed
	LastNicSyncTime time.Time
	Nics            []*NicIrqTuningManager // nic level irq tuning manager
	*IndicatorsStats

	IrqAffinityChanges map[int]*IrqAffinityChange // nic ifindex as map key. used to record irq affinity changes in each periodicTuning, and will be reset at the beginning of periodicTuning
}

func NewNicIrqTuningManager(conf *config.IrqTuningConfig, nic *util.NicBasicInfo, irqAffSockets []int) (*NicIrqTuningManager, error) {
	nicInfo, err := GetNicInfo(nic)
	if err != nil {
		return nil, fmt.Errorf("failed to GetNicInfo for nic %s, err %v", nic, err)
	}

	return &NicIrqTuningManager{
		Conf:              conf,
		NicInfo:           nicInfo,
		IrqAffinityPolicy: InitTuning,
		AssignedSockets:   irqAffSockets,
		IrqCoresExclusionSwitchStat: IrqCoresExclusionSwitchStat{
			IrqCoresExclusionLastSwitchTime: time.Now(),
		},
	}, nil
}

func NewIrqTuningController(dynamicConfHolder *dynconfig.DynamicAgentConfiguration, irqStateAdapter irqtuner.StateAdapter) (*IrqTuningController, error) {
	if isIrqBalanceNGServiceRuning() {
		return nil, fmt.Errorf("irqbalance-ng service is running")
	}

	conf := config.ConvertDynamicConfigToIrqTuningConfig(dynamicConfHolder.GetDynamicConfiguration())

	cpuInfo, err := util.GetCPUInfoWithTopo()
	if err != nil {
		return nil, fmt.Errorf("failed to GetCPUInfoWithTopo, err %v", err)
	}

	if len(cpuInfo.Sockets) == 0 {
		return nil, fmt.Errorf("invalid cpuinfo with 0 socket")
	}

	if len(cpuInfo.Sockets) > 2 {
		return nil, fmt.Errorf("invalid cpuinfo has %d sockets", len(cpuInfo.Sockets))
	}

	ksoftirqds, err := util.ListKsoftirqdProcesses()
	if err != nil {
		return nil, fmt.Errorf("failed to ListKsoftirqdProcesses, err %v", err)
	}

	for cpuID, _ := range cpuInfo.CPUOnline {
		if _, ok := ksoftirqds[cpuID]; !ok {
			return nil, fmt.Errorf("cpu%d's ksoftirqd not exists", cpuID)
		}
	}

	nics, err := listActiveUplinkNicsExcludeSriovVFs()
	if err != nil {
		return nil, err
	}

	nicIrqsAffSockets, err := AssignSocketsForNicIrqs(nics, cpuInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to AssignSocketsForNicIrqs, err %v", err)
	}

	var nicManagers []*NicIrqTuningManager
	for _, n := range nics {
		mng, err := NewNicIrqTuningManager(conf, n, nicIrqsAffSockets[n.IfIndex])
		if err != nil {
			return nil, fmt.Errorf("failed to NewNicIrqTuningManager for nic %s, err %v", n, err)
		}
		nicManagers = append(nicManagers, mng)
	}

	controller := &IrqTuningController{
		dynamicConfHolder:  dynamicConfHolder,
		conf:               conf,
		CPUInfo:            cpuInfo,
		Ksoftirqds:         ksoftirqds,
		IrqStateAdapter:    irqStateAdapter,
		Containers:         make(map[string]*ContainerInfoWrapper),
		NicSyncInterval:    NicsSyncInterval,
		LastNicSyncTime:    time.Now(),
		Nics:               nicManagers,
		IrqAffinityChanges: make(map[int]*IrqAffinityChange),
	}

	controllerBytes, err := json.Marshal(controller)
	if err != nil {
		klog.ErrorS(err, "%s failed to marshal controller", IrqTuningLogPrefix)
	} else {
		klog.Infof("%s controller: %s", IrqTuningLogPrefix, string(controllerBytes))
	}

	return controller, nil
}

func isIrqBalanceNGServiceRuning() bool {
	running, err := util.CheckIfProcCommRunning(IrqBalanceNGProcComm)
	if err != nil { // just to be sure, if failed to check if irq balance service is running, consider it as running
		return true
	}
	return running
}

func getIrqsAffinityCPUs(nic *util.NicBasicInfo, irqs []int) (map[int]int64, error) {
	irq2CPUs, err := util.GetIrqsAffinityCPUs(irqs)
	if err != nil {
		return nil, fmt.Errorf("failed to GetIrqsAffinityCPUs(%+v), err %v", irqs, err)
	}

	irq2Core := make(map[int]int64)
	var hasIrqAffinityMultiCPUs bool
	for irq, cpus := range irq2CPUs {
		if len(cpus) > 1 {
			hasIrqAffinityMultiCPUs = true
			klog.Warningf("%s nic %s has irq(%d) affinity multipule cpus(%+v)", IrqTuningLogPrefix, nic, irq, cpus)
			break
		}
		irq2Core[irq] = cpus[0]
	}

	if hasIrqAffinityMultiCPUs {
		irq2Core, err = util.TidyUpNicIrqsAffinityCPUs(irq2CPUs)
		if err != nil {
			return nil, fmt.Errorf("failed to TidyUpIrqsAffinityCPUs, err %v", err)
		}
	}
	return irq2Core, nil
}

func getSocketIrqCores(irq2Core map[int]int64) (map[int][]int64, error) {
	socketIrqCores := make(map[int][]int64)
	coresMap := make(map[int64]interface{})

	for _, core := range irq2Core {
		if _, ok := coresMap[core]; ok {
			continue
		}
		coresMap[core] = nil

		socketID, err := util.GetCPUPackageID(core)
		if err != nil {
			return nil, fmt.Errorf("failed to GetCPUPackageID(%d), err %v", core, err)
		}

		socketIrqCores[socketID] = append(socketIrqCores[socketID], core)
	}

	return socketIrqCores, nil
}

func GetNicInfo(nic *util.NicBasicInfo) (*NicInfo, error) {
	var irqs []int
	for _, irq := range nic.Queue2Irq {
		irqs = append(irqs, irq)
	}

	irq2Core, err := getIrqsAffinityCPUs(nic, irqs)
	if err != nil {
		return nil, fmt.Errorf("failed to getIrqsAffinityCPUs(%+v), err %v", irqs, err)
	}

	socketIrqCores, err := getSocketIrqCores(irq2Core)
	if err != nil {
		return nil, fmt.Errorf("failed to getSocketIrqCores, err %s", err)
	}

	return &NicInfo{
		NicBasicInfo:   nic,
		Irq2Core:       irq2Core,
		SocketIrqCores: socketIrqCores,
	}, nil
}

func (n *NicInfo) getIrqCore2SocketMap() map[int64]int {
	irqCore2Socket := make(map[int64]int)

	// irq affinity cpus MUST be in nic.SocketIrqCores, which is guaranteed in GetNicInfo
	for socket, cores := range n.SocketIrqCores {
		for _, core := range cores {
			irqCore2Socket[core] = socket
		}
	}
	return irqCore2Socket
}

func (n *NicInfo) getIrqs() []int {
	var irqs []int
	for _, irq := range n.Queue2Irq {
		irqs = append(irqs, irq)
	}

	return irqs
}

func (n *NicInfo) getQueues() []int {
	var queues []int
	for queue, _ := range n.Queue2Irq {
		queues = append(queues, queue)
	}

	return queues
}

func (n *NicInfo) getIrqCoreAffinitiedIrqs() map[int64][]int {
	irqCoreAffinitiedIrqs := make(map[int64][]int)

	for irq, core := range n.Irq2Core {
		irqCoreAffinitiedIrqs[core] = append(irqCoreAffinitiedIrqs[core], irq)
	}
	return irqCoreAffinitiedIrqs
}

func (n *NicInfo) filterCoresAffinitiedIrqs(coresList []int64) []int {
	// uniq cores
	coresMap := make(map[int64]interface{})
	for _, core := range coresList {
		coresMap[core] = nil
	}

	coresAffinitiedIrqs := n.getIrqCoreAffinitiedIrqs()

	var irqs []int
	for core, _ := range coresMap {
		if coreIrqs, ok := coresAffinitiedIrqs[core]; ok && len(coreIrqs) > 0 {
			irqs = append(irqs, coreIrqs...)
		}
	}
	return irqs
}

func (n *NicInfo) filterCoresAffinitiedQueues(coreList []int64) []int {
	irqs := n.filterCoresAffinitiedIrqs(coreList)

	var queues []int
	for _, irq := range irqs {
		queue, ok := n.Irq2Queue[irq]
		if !ok {
			klog.Warningf("failed to find irq %d in nic %s Irq2Queue", irq, n)
			continue
		}
		queues = append(queues, queue)
	}

	return queues
}

func (n *NicInfo) getSocketAffinitiedIrqs(socket int) []int {
	var socketAffinitiedIrqs []int

	cores, ok := n.SocketIrqCores[socket]
	if !ok {
		return socketAffinitiedIrqs
	}

	coreIrqs := n.getIrqCoreAffinitiedIrqs()

	for _, core := range cores {
		irqs, ok := coreIrqs[core]
		if !ok {
			klog.Errorf("failed to find core %d in getIrqCoreAffinitiedIrqs return value", core)
			continue
		}
		socketAffinitiedIrqs = append(socketAffinitiedIrqs, irqs...)
	}

	return socketAffinitiedIrqs
}

func (n *NicInfo) getIrqCores() []int64 {
	var irqCores []int64
	coresMap := make(map[int64]interface{})

	for _, core := range n.Irq2Core {
		if _, ok := coresMap[core]; ok {
			continue
		}

		coresMap[core] = nil
		irqCores = append(irqCores, core)
	}
	return irqCores
}

func (n *NicInfo) filterIrqCores(coresList []int64) []int64 {
	irqCores := n.getIrqCores()
	irqCoresMap := make(map[int64]interface{})
	for _, core := range irqCores {
		irqCoresMap[core] = nil
	}

	var filteredIrqCores []int64
	filteredIrqCoresMap := make(map[int64]interface{})
	for _, core := range coresList {
		if _, ok := irqCoresMap[core]; ok {
			if _, ok := filteredIrqCoresMap[core]; ok {
				continue
			}

			filteredIrqCoresMap[core] = nil
			filteredIrqCores = append(filteredIrqCores, core)
		}
	}

	return filteredIrqCores
}

func (n *NicInfo) sync() error {
	nicInfo, err := GetNicInfo(n.NicBasicInfo)
	if err != nil {
		return fmt.Errorf("failed to GetNicInfo for nic %s, err %v", n, err)
	}

	n.Irq2Core = nicInfo.Irq2Core
	n.SocketIrqCores = nicInfo.SocketIrqCores

	return nil
}

func listActiveUplinkNicsExcludeSriovVFs() ([]*util.NicBasicInfo, error) {
	nics, err := util.ListActiveUplinkNics()
	if err != nil {
		return nil, err
	}

	// filter out nics which are dedicated to sriov dedicated-cores containers from nics
	// sriov dedicated-cores container's nic's irq affinity will be tuned in initialize tuning and periodic tuning
	var tmpNics []*util.NicBasicInfo
	for _, nic := range nics {
		// all sriov netns's names hava prefix "cni-", sriov netns is managed by cni plugin
		if !strings.HasPrefix(nic.NetNSName, "cni-") {
			tmpNics = append(tmpNics, nic)
		}
	}
	nics = tmpNics

	if len(nics) == 0 {
		return nil, fmt.Errorf("no active uplink nics after filtering out sriov nics, it's impossible")
	}

	if len(nics) > 2 {
		return nil, fmt.Errorf("still has %d active uplink nics after filtering out sriov nics, at most 2 active uplink nics are supported now", len(nics))
	}

	return nics, nil
}

// map[int]int : nic ifindex as map key, nic irqs should affinitied socket slice as value
func AssignSocketsForNicIrqs(nics []*util.NicBasicInfo, cpuInfo *util.CPUInfo) (map[int][]int, error) {
	ifIndex2Sockets := make(map[int][]int)

	if len(nics) == 0 {
		return nil, fmt.Errorf("no nic")
	}

	if len(nics) > 2 {
		return nil, fmt.Errorf("too many nics(%d), now at mostly support 2 nics", len(nics))
	}

	if len(cpuInfo.Sockets) == 0 {
		return nil, fmt.Errorf("no sockets to assign")
	}

	if len(cpuInfo.Sockets) > 2 {
		return nil, fmt.Errorf("two many sockets(%d), now only support 2 sockets", len(cpuInfo.Sockets))
	}

	if len(cpuInfo.Sockets) == 1 {
		for _, nic := range nics {
			ifIndex2Sockets[nic.IfIndex] = []int{0}
		}
		return ifIndex2Sockets, nil
	}

	if len(nics) == 1 {
		ifIndex2Sockets[nics[0].IfIndex] = []int{0, 1}
		return ifIndex2Sockets, nil
	}

	if nics[0].SocketBind != nics[1].SocketBind {
		var nicIndexWithKnownSocketBind int
		var otherNicIndex int

		if nics[0].SocketBind != util.UnknownSocketBind {
			nicIndexWithKnownSocketBind = 0
			otherNicIndex = 1
		} else {
			nicIndexWithKnownSocketBind = 1
			otherNicIndex = 0
		}
		ifIndex2Sockets[nics[nicIndexWithKnownSocketBind].IfIndex] = []int{nics[nicIndexWithKnownSocketBind].SocketBind}

		var otherNicAffSocket int
		if nics[nicIndexWithKnownSocketBind].SocketBind == 0 {
			otherNicAffSocket = 1
		} else {
			otherNicAffSocket = 0
		}
		ifIndex2Sockets[nics[otherNicIndex].IfIndex] = []int{otherNicAffSocket}
	} else {
		var leastIfIndexNicIndex int
		var otherNicIndex int
		if nics[0].IfIndex < nics[1].IfIndex {
			leastIfIndexNicIndex = 0
			otherNicIndex = 1
		} else {
			leastIfIndexNicIndex = 1
			otherNicIndex = 0
		}
		ifIndex2Sockets[nics[leastIfIndexNicIndex].IfIndex] = []int{0}
		ifIndex2Sockets[nics[otherNicIndex].IfIndex] = []int{1}
	}

	return ifIndex2Sockets, nil
}

func irqCoresEqual(a []int64, b []int64) bool {
	if len(a) != len(b) {
		return false
	}

	util.SortInt64Slice(a)
	util.SortInt64Slice(b)

	for i := 0; i < len(a); i++ {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

func calculateIncreasedIrqCores(oldIrqCores []int64, newIrqCores []int64) []int64 {
	var increasedIrqCores []int64

	for _, c1 := range newIrqCores {
		found := false
		for _, c2 := range oldIrqCores {
			if c1 == c2 {
				found = true
				break
			}
		}

		if !found {
			increasedIrqCores = append(increasedIrqCores, c1)
		}
	}

	return increasedIrqCores
}

func calculateDecreasedIrqCores(oldIrqCores []int64, newIrqCores []int64) []int64 {
	return calculateIncreasedIrqCores(newIrqCores, oldIrqCores)
}

func calculateOverlappedIrqCores(a []int64, b []int64) []int64 {
	var overlappedIrqCores []int64

	for _, coreA := range a {
		for _, coreB := range b {
			if coreA == coreB {
				overlappedIrqCores = append(overlappedIrqCores, coreA)
				break
			}
		}
	}

	return overlappedIrqCores
}

func calculateIrqCoresDiff(a []int64, b []int64) []int64 {
	var diff []int64

	for _, coreA := range a {
		found := false
		for _, coreB := range b {
			if coreB == coreA {
				found = true
				break
			}
		}
		if !found {
			diff = append(diff, coreA)
		}
	}

	return diff
}

func (nm *NicIrqTuningManager) collectNicStats() (*NicStats, error) {
	totalRxPackets, err := util.GetNetDevRxPackets(nm.NicInfo.NetNSName, nm.NicInfo.Name)
	if err != nil {
		return nil, err
	}

	rxQueuePackets, err := util.GetNicRxQueuePackets(nm.NicInfo.NicBasicInfo)
	if err != nil {
		return nil, err
	}

	return &NicStats{
		TotalRxPackets: totalRxPackets,
		RxQueuePackets: rxQueuePackets,
	}, nil
}

func (nm *NicIrqTuningManager) getRxQueuesPPSInDecOrder(queues []int, oldStats *IndicatorsStats, newStats *IndicatorsStats) []*QueuePPS {
	if len(queues) == 0 {
		return nil
	}

	timeDiff := newStats.UpdateTime.Sub(oldStats.UpdateTime).Seconds()

	rxQueuesPPS := calculateQueuePPS(oldStats.NicStats[nm.NicInfo.IfIndex], newStats.NicStats[nm.NicInfo.IfIndex], timeDiff)

	var coreRxQueuesPPS []*QueuePPS
	for _, queue := range queues {
		find := false
		for _, queuePPS := range rxQueuesPPS {
			if queue == queuePPS.QueueID {
				find = true
				coreRxQueuesPPS = append(coreRxQueuesPPS, queuePPS)
				break
			}
		}
		if !find {
			klog.Warningf("failed to find queue %d in nic %s rx queue pps", queue, nm.NicInfo)
		}
	}

	// sort queue pps in deceasing order
	sortQueuePPSSliceInDecOrder(coreRxQueuesPPS)

	return coreRxQueuesPPS
}

func (nm *NicIrqTuningManager) getCoresRxQueuesPPSInDecOrder(cores []int64, oldStats *IndicatorsStats, newStats *IndicatorsStats) []*QueuePPS {
	return nm.getRxQueuesPPSInDecOrder(nm.NicInfo.filterCoresAffinitiedQueues(cores), oldStats, newStats)
}

func (nm *NicIrqTuningManager) getIrqsCorrespondingRxQueuesPPSInDecOrder(irqs []int, oldStats *IndicatorsStats, newStats *IndicatorsStats) []*QueuePPS {
	var queues []int
	for _, irq := range irqs {
		queue, ok := nm.NicInfo.Irq2Queue[irq]
		if !ok {
			klog.Warningf("failed to find irq in nic %s Irq2Queue %+v", nm.NicInfo, nm.NicInfo.Irq2Queue)
			continue
		}
		queues = append(queues, queue)
	}

	return nm.getRxQueuesPPSInDecOrder(queues, oldStats, newStats)
}

func (ic *IrqTuningController) collectIndicatorsStats() (*IndicatorsStats, error) {
	nicStats := make(map[int]*NicStats)
	for _, nic := range ic.Nics {
		stats, err := nic.collectNicStats()
		if err != nil {
			return nil, fmt.Errorf("failed to collectNicStats, err %v", err)
		}
		nicStats[nic.NicInfo.IfIndex] = stats
	}

	cpuStats, err := util.CollectCpuStats()
	if err != nil {
		return nil, fmt.Errorf("failed to CollectCpuStats, err %v", err)
	}

	softNetStats, err := util.CollectSoftNetStats(ic.CPUInfo.CPUOnline)
	if err != nil {
		return nil, fmt.Errorf("failed to collectSoftNetStats, err %v", err)
	}

	var ksoftirqPids []int
	for _, pid := range ic.Ksoftirqds {
		ksoftirqPids = append(ksoftirqPids, pid)
	}

	ksoftirqdSchedWait, err := util.GetTaskSchedWait(ksoftirqPids)
	if err != nil {
		return nil, fmt.Errorf("failed to GetTaskSchedWait, err %v", err)
	}

	netRxSoftirqCount, err := util.CollectNetRxSoftirqStats()
	if err != nil {
		return nil, err
	}

	return &IndicatorsStats{
		NicStats:           nicStats,
		CPUStats:           cpuStats,
		SoftNetStats:       softNetStats,
		KsoftirqdSchedWait: ksoftirqdSchedWait,
		NetRxSoftirqCount:  netRxSoftirqCount,
		UpdateTime:         time.Now(),
	}, nil
}

// return value: old IndicatorStats
func (ic *IrqTuningController) updateIndicatorsStats() (*IndicatorsStats, error) {
	if ic.IndicatorsStats == nil {
		stats, err := ic.collectIndicatorsStats()
		if err != nil {
			return nil, fmt.Errorf("failed to updateStats, err %v", err)
		}
		ic.IndicatorsStats = stats
		time.Sleep(10 * time.Second)
	}

	stats, err := ic.collectIndicatorsStats()
	if err != nil {
		return nil, fmt.Errorf("failed to updateStats, err %v", err)
	}
	oldStats := ic.IndicatorsStats
	ic.IndicatorsStats = stats

	if stats.UpdateTime.Sub(oldStats.UpdateTime).Seconds() < 1 {
		return nil, fmt.Errorf("current IndicatorsStats update time(%s) sub last IndicatorsStats update time(%s) is less than 1 second",
			stats.UpdateTime, oldStats.UpdateTime)
	}

	return oldStats, nil
}

func (ic *IrqTuningController) updateLatestIndicatorsStats(seconds int) (*IndicatorsStats, error) {
	if _, err := ic.updateIndicatorsStats(); err != nil {
		return nil, fmt.Errorf("failed to updateIndicatorsStats, err %s", err)
	}

	time.Sleep(time.Duration(seconds) * time.Second)

	oldIndicatorsStats, err := ic.updateIndicatorsStats()
	if err != nil {
		return nil, fmt.Errorf("failed to updateIndicatorsStats, err %s", err)
	}

	return oldIndicatorsStats, nil
}

func (c *ContainerInfoWrapper) getContainerCPUs() []int64 {
	var cpus []int64

	for _, cpuset := range c.ActualCPUSet {
		cpus = append(cpus, cpuset.ToSliceInt64()...)
	}
	return cpus
}

func (c *ContainerInfoWrapper) isKataBMContainer() bool {
	if c.RuntimeClassName != KataRuntimeClassName {
		return false
	}

	if val, ok := c.Annotations[KataBMAnnotationName]; ok && val == KataBMAnnotationValue {
		return true
	}
	return false
}

func (ic *IrqTuningController) syncNics() error {
	klog.Infof("%s sync nics", IrqTuningLogPrefix)

	nics, err := listActiveUplinkNicsExcludeSriovVFs()
	if err != nil {
		return err
	}
	ic.LastNicSyncTime = time.Now()

	notChangedNicMap := make(map[int]interface{})
	for _, oldNic := range ic.Nics {
		for _, newNic := range nics {
			if newNic.Equal(oldNic.NicInfo.NicBasicInfo) {
				notChangedNicMap[newNic.IfIndex] = nil
				break
			}
		}
	}

	if len(notChangedNicMap) == len(ic.Nics) {
		klog.Infof("no nic changed")
		return nil
	}

	klog.Infof("%s %d nic changed", IrqTuningLogPrefix, len(ic.Nics)-len(notChangedNicMap))

	// if any nics changes happened, it's the simplest way to recalculate sockets assignment for nics's irq affinity and re-new
	// all nics's controller, regardless of unchanged nics's current configuration about irq affinity and assigned sockets,
	// just like katalyst restart.
	// There are the following reasons for handling it in this way,
	// 1) it's very simple and can keep consistent with irq-tuning manager plugin init
	// 2) the sockets assginemnts of nics's irq affinity is consistent, it's very important that sockets assginemnts result is consitent,
	//    because qrm use the same policy to assign nic for container in 2-nics machine to align with irq-tuning manager for best performance.
	// 3) there is an extremely low probability that any nic will change during node running.

	nicIrqsAffSockets, err := AssignSocketsForNicIrqs(nics, ic.CPUInfo)
	if err != nil {
		return fmt.Errorf("failed to AssignSocketsForNicIrqs, err %v", err)
	}

	var nicManagers []*NicIrqTuningManager
	for _, n := range nics {
		mng, err := NewNicIrqTuningManager(ic.conf, n, nicIrqsAffSockets[n.IfIndex])
		if err != nil {
			return fmt.Errorf("failed to NewNicIrqTuningManager for nic %s, err %v", n, err)
		}
		nicManagers = append(nicManagers, mng)
	}

	ic.Nics = nicManagers

	return nil
}

// katabm container's cpus MUST be excluded from cpu allocation for other nic's balance-fair irq affinity.
func (ic *IrqTuningController) getKataBMContainerCPUs() []int64 {
	var katabmCPUs []int64

	for _, cnt := range ic.Containers {
		if cnt.isKataBMContainer() {
			for _, cpuset := range cnt.ActualCPUSet {
				katabmCPUs = append(katabmCPUs, cpuset.ToSliceInt64()...)
			}
		}
	}
	return katabmCPUs
}

func (ic *IrqTuningController) getKataBMContainerNumas() []int {
	var numas []int

	for _, cnt := range ic.Containers {
		if cnt.isKataBMContainer() {
			for numaID, _ := range cnt.ActualCPUSet {
				numas = append(numas, numaID)
			}
		}
	}
	return numas
}

func (ic *IrqTuningController) isExclusiveIrqCoresNic(ifindex int) (bool, error) {
	for _, nic := range ic.Nics {
		if nic.NicInfo.IfIndex != ifindex {
			continue
		}

		if change, ok := ic.IrqAffinityChanges[ifindex]; ok {
			if change.NewIrqAffinityPolicy == IrqCoresExclusive {
				return true, nil
			} else {
				return false, nil
			}
		} else {
			if nic.IrqAffinityPolicy == IrqCoresExclusive {
				return true, nil
			} else {
				return false, nil
			}
		}
	}

	return false, fmt.Errorf("failed to find nic with ifindex: %d", ifindex)
}

// exclusive irq cores MUST be exlcuded from cpu allocation for other nic's balance-fair irq affinity
func (ic *IrqTuningController) getExclusiveIrqCores(excludedNicsIfIndex []int) []int64 {
	var exclusiveIrqCores []int64

	for _, nic := range ic.Nics {
		exclude := false
		for _, ifIndex := range excludedNicsIfIndex {
			if nic.NicInfo.IfIndex == ifIndex {
				exclude = true
				break
			}
		}
		if exclude {
			continue
		}

		if change, ok := ic.IrqAffinityChanges[nic.NicInfo.IfIndex]; ok {
			if change.NewIrqAffinityPolicy == IrqCoresExclusive {
				if len(change.NewIrqCores) > 0 {
					exclusiveIrqCores = append(exclusiveIrqCores, change.NewIrqCores...)
				}
			}
		} else {
			if nic.IrqAffinityPolicy == IrqCoresExclusive {
				for _, irqCores := range nic.NicInfo.SocketIrqCores {
					exclusiveIrqCores = append(exclusiveIrqCores, irqCores...)
				}
			}
		}
	}
	return exclusiveIrqCores
}

// dedicated-cores sriov container's cpus(NOT irq cores, sriov container's cpus containes its irq cores) should be excluded from
// cpu allocation for other nic's balance-fair irq affinity.
func (ic *IrqTuningController) getSRIOVContainerDedicatedCores() []int64 {
	var sriovDedicatedCores []int64

	for _, cnt := range ic.Containers {
		if !cnt.IsSriovContainer {
			continue
		}

		if cnt.CheckDedicated() {
			for _, cpuset := range cnt.ActualCPUSet {
				sriovDedicatedCores = append(sriovDedicatedCores, cpuset.ToSliceInt64()...)
			}
		}
	}
	return sriovDedicatedCores
}

func (ic *IrqTuningController) getUnqualifiedNumasForBalanceFairPolicy() []int {
	return ic.getKataBMContainerNumas()
}

func (ic *IrqTuningController) getSocketsQualifiedNumasForBalanceFairPolicy(sockets []int) []int {
	unqualifiedNumas := ic.getUnqualifiedNumasForBalanceFairPolicy()

	var qualifiedNumas []int
	for _, socket := range sockets {
		socketNumaIDs := ic.CPUInfo.Sockets[socket].NumaIDs
		for _, numaID := range socketNumaIDs {
			matched := false
			for _, n := range unqualifiedNumas {
				if numaID == n {
					matched = true
					break
				}
			}
			if !matched {
				qualifiedNumas = append(qualifiedNumas, numaID)
			}
		}
	}

	return qualifiedNumas
}

// get cores which are unqualified for defaut irq affinity
func (ic *IrqTuningController) getUnqualifiedCoresMapForBalanceFairPolicy() map[int64]interface{} {
	unqualifiedCores := ic.getKataBMContainerCPUs()

	exclusiveIrqCores := ic.getExclusiveIrqCores([]int{})
	unqualifiedCores = append(unqualifiedCores, exclusiveIrqCores...)

	// forbbiden cores can be affinitied by irqs of nics with balance-fair policy, but cannot be used as exclusive irq cores
	// so here we donnot acount forbidden cores

	unqualifiedCoresMap := make(map[int64]interface{})
	for _, core := range unqualifiedCores {
		unqualifiedCoresMap[core] = nil
	}
	return unqualifiedCoresMap
}

func (ic *IrqTuningController) getQualifiedCoresMap(destDomainCoresList []int64, unqualifiedCoresMap map[int64]interface{}) map[int64]interface{} {
	qualifiedCoresMap := make(map[int64]interface{})

	if len(unqualifiedCoresMap) > len(ic.CPUInfo.CPUOnline) {
		klog.Warningf("unqualified cores count %d > total online cpus count %d", len(unqualifiedCoresMap), len(ic.CPUInfo.CPUOnline))
		return qualifiedCoresMap
	}

	if len(unqualifiedCoresMap) == len(ic.CPUInfo.CPUOnline) {
		return qualifiedCoresMap
	}

	for _, cpu := range destDomainCoresList {
		if _, ok := unqualifiedCoresMap[cpu]; ok {
			continue
		}

		qualifiedCoresMap[cpu] = nil
	}

	return qualifiedCoresMap
}

// get cores which are qualified for defaut irq affinity
func (ic *IrqTuningController) getSocketsQualifiedCoresMapForBalanceFairPolicy(sockets []int) map[int64]interface{} {
	var cpuList []int64
	if len(sockets) == 0 {
		for _, socketCPUList := range ic.CPUInfo.SocketCPUs {
			cpuList = append(cpuList, socketCPUList...)
		}
	} else {
		for _, socket := range sockets {
			cpuList = append(cpuList, ic.CPUInfo.SocketCPUs[socket]...)
		}
	}

	return ic.getQualifiedCoresMap(cpuList, ic.getUnqualifiedCoresMapForBalanceFairPolicy())
}

// get cores which are qualified for defaut irq affinity
func (ic *IrqTuningController) getNumaQualifiedCoresMapForBalanceFairPolicy(numa int) map[int64]interface{} {
	cpuList := ic.CPUInfo.GetNodeCPUList(numa)
	return ic.getQualifiedCoresMap(cpuList, ic.getUnqualifiedCoresMapForBalanceFairPolicy())
}

func (ic *IrqTuningController) getCCDQualifiedCoresMapForBalanceFairPolicy(ccd *util.LLCDomain) map[int64]interface{} {
	cpuList := util.GetLLCDomainCPUList(ccd)
	return ic.getQualifiedCoresMap(cpuList, ic.getUnqualifiedCoresMapForBalanceFairPolicy())
}

func (ic *IrqTuningController) getNumaQualifiedCCDsForBalanceFairPolicy(numa int) []*util.LLCDomain {
	unqualifiedCoresMap := ic.getUnqualifiedCoresMapForBalanceFairPolicy()

	ccds, err := ic.CPUInfo.GetAMDNumaCCDs(numa)
	if err != nil {
		klog.Errorf("failed to GetAMDNumaCCDs(%d), err %s", numa, err)
		return nil
	}

	var qualifiedCCDs []*util.LLCDomain
	for _, ccd := range ccds {
		ccdCPUList := util.GetLLCDomainCPUList(ccd)
		qualified := true
		for _, cpu := range ccdCPUList {
			// if ccd has at least one cpu is unqualified, then this ccd is unqualified
			if _, ok := unqualifiedCoresMap[cpu]; ok {
				qualified = false
				break
			}
		}
		if qualified {
			qualifiedCCDs = append(qualifiedCCDs, ccd)
		}
	}

	return qualifiedCCDs
}

func (ic *IrqTuningController) getCoresIrqCount(includeSriovContainersNics bool) map[int64]int {
	coresIrqCount := make(map[int64]int)

	for _, nic := range ic.Nics {
		// need to filter irqs of nics whose irq affinity policy is IrqCoresExclusive,
		// because if a nic's irq affinity policy is changind from others to IrqCoresExclusive,
		// then its irqs affinitied cores may not be exclusive irq cores, and its irqs should not be counted,
		// or it will have impact on calculate avg core irq count in non-exclusive irq cores.
		exclusive, err := ic.isExclusiveIrqCoresNic(nic.NicInfo.IfIndex)
		if err != nil {
			klog.Errorf("failed to isExclusiveIrqCoresNic check for nic %s, err %s", nic.NicInfo, err)
			continue
		}

		if exclusive {
			continue
		}

		coreAffIrqs := nic.NicInfo.getIrqCoreAffinitiedIrqs()
		for core, irqs := range coreAffIrqs {
			coresIrqCount[core] += len(irqs)
		}
	}

	if includeSriovContainersNics {
		for _, cnt := range ic.Containers {
			if !cnt.IsSriovContainer {
				continue
			}

			for _, nic := range cnt.Nics {
				coreAffIrqs := nic.getIrqCoreAffinitiedIrqs()
				for core, irqs := range coreAffIrqs {
					coresIrqCount[core] += len(irqs)
				}
			}
		}
	}

	return coresIrqCount
}

func (ic *IrqTuningController) calculateCoresIrqSumCount(coresMap map[int64]interface{}, includeSriovContainersNics bool) int {
	coresIrqCount := ic.getCoresIrqCount(includeSriovContainersNics)

	irqSumCount := 0
	for core, _ := range coresMap {
		irqSumCount += coresIrqCount[core]
	}
	return irqSumCount
}

func (ic *IrqTuningController) selectPhysicalCoreWithLeastOrMostIrqs(coreIrqsCount map[int64]int, qualifiedCoresMap map[int64]interface{}, least bool) (int64, error) {
	if len(qualifiedCoresMap) == 0 {
		return 0, fmt.Errorf("qualifiedCoresMap length is zero")
	}

	var phyCores []util.PhyCore
	for socketID, _ := range ic.CPUInfo.Sockets {
		socketPhyCores := ic.CPUInfo.GetSocketPhysicalCores(socketID)
		phyCores = append(phyCores, socketPhyCores...)
	}

	if len(phyCores) == 0 {
		return 0, fmt.Errorf("it's impossible to have zero physical cores")
	}

	phyCoreIrqsCount := make(map[int]int) // physical core index in phyCores array as map key
	for index, phyCore := range phyCores {
		// needless to filter out unqualified cpu when counting irqs of physical core
		for _, cpu := range phyCore.CPUs {
			phyCoreIrqsCount[index] += coreIrqsCount[cpu]
		}
	}

	targetPhyCoreIndex := -1
	targetPhyCoreIrqsCount := 0

	for phyCoreIndex, irqsCount := range phyCoreIrqsCount {
		hasQualifiedCPU := false
		for _, cpu := range phyCores[phyCoreIndex].CPUs {
			if _, ok := qualifiedCoresMap[cpu]; ok {
				hasQualifiedCPU = true
				break
			}
		}
		if !hasQualifiedCPU {
			continue
		}

		if least {
			if targetPhyCoreIndex == -1 || irqsCount < targetPhyCoreIrqsCount {
				targetPhyCoreIndex = phyCoreIndex
				targetPhyCoreIrqsCount = irqsCount
			}
		} else {
			if targetPhyCoreIndex == -1 || irqsCount > targetPhyCoreIrqsCount {
				targetPhyCoreIndex = phyCoreIndex
				targetPhyCoreIrqsCount = irqsCount
			}
		}
	}

	targetPhyCore := phyCores[targetPhyCoreIndex]

	targetCore := int64(-1)
	targetCoreIrqsCount := 0
	for _, cpu := range targetPhyCore.CPUs {
		if _, ok := qualifiedCoresMap[cpu]; !ok {
			continue
		}

		cpuIrqsCount := coreIrqsCount[cpu]
		if least {
			if targetCore == -1 || cpuIrqsCount < targetCoreIrqsCount {
				targetCore = cpu
				targetCoreIrqsCount = cpuIrqsCount
			}
		} else {
			if targetCore == -1 || cpuIrqsCount > targetCoreIrqsCount {
				targetCore = cpu
				targetCoreIrqsCount = cpuIrqsCount
			}
		}
	}

	return targetCore, nil
}

func (ic *IrqTuningController) selectPhysicalCoreWithLeastIrqs(coreIrqsCount map[int64]int, qualifiedCoresMap map[int64]interface{}) (int64, error) {
	return ic.selectPhysicalCoreWithLeastOrMostIrqs(coreIrqsCount, qualifiedCoresMap, true)
}

func (ic *IrqTuningController) selectPhysicalCoreWithMostIrqs(coreIrqsCount map[int64]int, qualifiedCoresMap map[int64]interface{}) (int64, error) {
	return ic.selectPhysicalCoreWithLeastOrMostIrqs(coreIrqsCount, qualifiedCoresMap, false)
}

func (ic *IrqTuningController) tuneNicIrqsAffinityQualifiedCores(nic *NicInfo, irqs []int, qualifiedCoresMap map[int64]interface{}) error {
	isSriovContainerNic := true
	for _, nm := range ic.Nics {
		if nm.NicInfo.IfIndex == nic.IfIndex {
			isSriovContainerNic = false
			break
		}
	}

	includeSriovContainersNics := false
	if isSriovContainerNic {
		includeSriovContainersNics = true
	}

	coresIrqCount := ic.getCoresIrqCount(includeSriovContainersNics)
	hasIrqTuned := false

	for _, irq := range irqs {
		core, ok := nic.Irq2Core[irq]
		if !ok {
			klog.Errorf("failed to find irq %d in nic %s Irq2Core", irq, nic)
			continue
		}

		// needless to tune a irq when its affinitied core is qualified
		if _, ok := qualifiedCoresMap[core]; ok {
			continue
		}

		targetCore, err := ic.selectPhysicalCoreWithLeastIrqs(coresIrqCount, qualifiedCoresMap)
		if err != nil {
			klog.Errorf("failed to selectPhysicalCoreWithLeastIrqs, err %v", err)
			continue
		}

		if err := util.SetIrqAffinity(irq, targetCore); err != nil {
			klog.Errorf("failed to SetIrqAffinity(%d, %d) for nic %s, err %v",
				irq, targetCore, nic, err)
			continue
		}

		coresIrqCount[core]--
		coresIrqCount[targetCore]++
		hasIrqTuned = true
	}

	///////////////////////////////////////////////
	// update nic.Irq2Core and nic.SocketIrqCores
	///////////////////////////////////////////////
	if hasIrqTuned {
		if err := nic.sync(); err != nil {
			klog.Errorf("failed to sync for nic %s, err %s", nic, err)
		}
	}

	return nil
}

// balance this nic's irqs in its assigned sockets,
// the reason why choosing nic level irqs balance instead of all nics(with balance-fair policy)'s irqs balance,
//  1. the bandwidths of different nics may vary greatly, so nic level irqs balance in its assinged sockets will lead to
//     better total bandwidth balance in its assgined sockets.
//  2. nic level irq balance is simple than all nics(balance-fair policy)'s irqs balance.
func (ic *IrqTuningController) tuneNicIrqsAffinityNumasFairly_deprecated(nic *NicInfo, assingedSockets []int) error {
	sort.Ints(assingedSockets)
	irqs := nic.getIrqs()
	sort.Ints(irqs)

	avgSocketIrqCount := len(irqs) / len(assingedSockets)
	remainder := len(irqs) % len(assingedSockets)

	irqIndex := 0
	for _, socket := range assingedSockets {
		var socketAssignedIrqs []int
		if remainder > 0 {
			socketAssignedIrqs = irqs[irqIndex : irqIndex+avgSocketIrqCount+1]
			remainder--
		} else {
			socketAssignedIrqs = irqs[irqIndex : irqIndex+avgSocketIrqCount]
		}
		irqIndex += len(socketAssignedIrqs)

		if len(socketAssignedIrqs) == 0 {
			continue
		}

		// tune nic irqs affinity socket numas evenly
		socketNumas := ic.CPUInfo.Sockets[socket].NumaIDs // NumaIDs has been sorted in initializing
		avgNumaIrqCount := len(socketAssignedIrqs) / len(socketNumas)
		numaRemainder := len(socketAssignedIrqs) % len(socketNumas)

		numaIrqIndex := 0
		for _, numa := range socketNumas {
			var numaAssignedIrqs []int
			if numaRemainder > 0 {
				numaAssignedIrqs = socketAssignedIrqs[numaIrqIndex : numaIrqIndex+avgNumaIrqCount+1]
				numaRemainder--
			} else {
				numaAssignedIrqs = socketAssignedIrqs[numaIrqIndex : numaIrqIndex+avgNumaIrqCount]
			}
			numaIrqIndex += len(numaAssignedIrqs)

			if len(numaAssignedIrqs) == 0 {
				continue
			}

			qualifiedCoresMap := ic.getNumaQualifiedCoresMapForBalanceFairPolicy(numa)
			if len(qualifiedCoresMap) == 0 {
				klog.Errorf("failed to find qualified cores in numa %d for nic %s", numa, nic)
				continue
			}

			if err := ic.tuneNicIrqsAffinityQualifiedCores(nic, numaAssignedIrqs, qualifiedCoresMap); err != nil {
				klog.Errorf("failed to tuneNicIrqsAffinityQualifiedCores for nic %s, err %s", nic, err)
			}
		}
	}

	return nil
}

// balance this nic's irqs in its assigned sockets,
// the reason why choosing nic level irqs balance instead of all nics(with balance-fair policy)'s irqs balance,
//  1. the bandwidths of different nics may vary greatly, so nic level irqs balance in its assinged sockets will lead to
//     better total bandwidth balance in its assgined sockets.
//  2. nic level irq balance is simple than all nics(balance-fair policy)'s irqs balance.
func (ic *IrqTuningController) tuneNicIrqsAffinityNumasFairly(nic *NicInfo, assingedSockets []int, ccdsBalance bool) error {
retry:
	var numasWithNotEnoughQualifiedResource []int

	qualifiedNumas := ic.getSocketsQualifiedNumasForBalanceFairPolicy(assingedSockets)

	var tmpQualifiedNumas []int
	for _, numa := range qualifiedNumas {
		isNumaWithNotEnoughQualifiedCores := false
		for _, n := range numasWithNotEnoughQualifiedResource {
			if n == numa {
				isNumaWithNotEnoughQualifiedCores = true
				break
			}
		}

		if !isNumaWithNotEnoughQualifiedCores {
			tmpQualifiedNumas = append(tmpQualifiedNumas, numa)
		}
	}
	qualifiedNumas = tmpQualifiedNumas

	if len(qualifiedNumas) == 0 {
		return fmt.Errorf("no qualified numa for nic %s irq affinity with balance-fair policy", nic)
	}

	sort.Ints(qualifiedNumas)

	irqs := nic.getIrqs()
	sort.Ints(irqs)

	avgNumaIrqCount := len(irqs) / len(qualifiedNumas)
	remainder := len(irqs) % len(qualifiedNumas)

	// distribute the remainder evenly among all sockets, and then numas
	numasRemainder := make(map[int]int) // numaID as map key
	left, right := 0, len(qualifiedNumas)-1
	for left < right {
		if remainder <= 0 {
			break
		}
		leftNuma := qualifiedNumas[left]
		numasRemainder[leftNuma] = 1
		remainder--

		if remainder <= 0 {
			break
		}
		rightNuma := qualifiedNumas[right]
		numasRemainder[rightNuma] = 1
		remainder--

		left++
		right--
	}

	if remainder > 0 {
		return fmt.Errorf("impossible, remainder should be zero after distribute to qualified numas")
	}

	irqIndex := 0
	for _, numa := range qualifiedNumas {
		numaAssignedIrqCount := avgNumaIrqCount + numasRemainder[numa]
		numaAssignedIrqs := irqs[irqIndex:numaAssignedIrqCount]

		irqIndex += len(numaAssignedIrqs)

		if len(numaAssignedIrqs) == 0 {
			continue
		}

		if ic.CPUInfo.CPUVendor == cpuid.AMD && ccdsBalance {
			qualifiedCCDs := ic.getNumaQualifiedCCDsForBalanceFairPolicy(numa)
			if len(qualifiedCCDs) == 0 {
				klog.Warningf("failed to find qualified ccds in numa %d for nic %s", numa, nic)
				numasWithNotEnoughQualifiedResource = append(numasWithNotEnoughQualifiedResource, numa)
				goto retry
			}
			if err := ic.tuneNicIrqsAffinityCCDsFairly(nic, numaAssignedIrqs, qualifiedCCDs); err != nil {
				klog.Errorf("failed to tuneIrqsAffinityNumaCCDsFairly for nic %s in numa %d ccds, err %s", nic, numa, err)
			}
		} else {
			qualifiedCoresMap := ic.getNumaQualifiedCoresMapForBalanceFairPolicy(numa)
			if len(qualifiedCoresMap) == 0 {
				klog.Warningf("failed to find qualified cores in numa %d for nic %s", numa, nic)
				numasWithNotEnoughQualifiedResource = append(numasWithNotEnoughQualifiedResource, numa)
				goto retry
			}

			if err := ic.tuneNicIrqsAffinityQualifiedCores(nic, numaAssignedIrqs, qualifiedCoresMap); err != nil {
				klog.Errorf("failed to tuneNicIrqsAffinityQualifiedCores for nic %s, err %s", nic, err)
			}
		}
	}

	return nil
}

func (ic *IrqTuningController) tuneNicIrqsAffinityCCDsFairly(nic *NicInfo, irqs []int, ccds []*util.LLCDomain) error {
	avgCCDIrqCount := len(irqs) / len(ccds)
	remainder := len(irqs) % len(ccds)

	irqIndex := 0
	for _, ccd := range ccds {
		var ccdAssignedIrqs []int
		if remainder > 0 {
			ccdAssignedIrqs = irqs[irqIndex : irqIndex+avgCCDIrqCount+1]
			remainder--
		} else {
			ccdAssignedIrqs = irqs[irqIndex : irqIndex+avgCCDIrqCount]
		}
		irqIndex += len(ccdAssignedIrqs)

		if len(ccdAssignedIrqs) == 0 {
			continue
		}

		qualifiedCoresMap := ic.getCCDQualifiedCoresMapForBalanceFairPolicy(ccd)
		if len(qualifiedCoresMap) == 0 {
			klog.Errorf("failed to find qualified cores in ccd for nic %s", nic)
			continue
		}

		if err := ic.tuneNicIrqsAffinityQualifiedCores(nic, ccdAssignedIrqs, qualifiedCoresMap); err != nil {
			klog.Errorf("failed to tuneNicIrqsAffinityQualifiedCores for nic %s, err %s", nic, err)
		}
	}

	return nil
}

func (ic *IrqTuningController) tuneNicIrqsAffinityLLCDomainsFairly(nic *NicInfo, assingedSockets []int) error {
	if ic.CPUInfo.CPUVendor == cpuid.Intel {
		return ic.tuneNicIrqsAffinityNumasFairly(nic, assingedSockets, false)
	} else if ic.CPUInfo.CPUVendor == cpuid.AMD {
		return ic.tuneNicIrqsAffinityNumasFairly(nic, assingedSockets, true)
	} else {
		return fmt.Errorf("unsupport cpu arch: %s", ic.CPUInfo.CPUVendor)
	}
}

func (ic *IrqTuningController) tuneNicIrqsAffinityFairly(nic *NicInfo, assingedSockets []int) error {
	if ic.conf.IrqTuningPolicy == config.IrqTuningBalanceFair {
		return ic.tuneNicIrqsAffinityLLCDomainsFairly(nic, assingedSockets)
	} else {
		return ic.tuneNicIrqsAffinityNumasFairly(nic, assingedSockets, false)
	}
}

func (ic *IrqTuningController) balanceNicIrqsInCoresFairly(nic *NicInfo, irqs []int, qualifiedCoresMap map[int64]interface{}) error {
	if len(qualifiedCoresMap) == 0 {
		return fmt.Errorf("qualifiedCoresMap length is zero")
	}

	isSriovContainerNic := true
	for _, nm := range ic.Nics {
		if nm.NicInfo.IfIndex == nic.IfIndex {
			isSriovContainerNic = false
			break
		}
	}

	includeSriovContainersNics := false
	if isSriovContainerNic {
		includeSriovContainersNics = true
	}

	// balance irqs in qualified cpus based on all nics(with balance-fair policy)'s irq affinity.
	coresIrqCount := ic.getCoresIrqCount(includeSriovContainersNics)
	irqSumCount := ic.calculateCoresIrqSumCount(qualifiedCoresMap, includeSriovContainersNics)
	changedIrq2Core := make(map[int]int64)

	// make sure parameter irqs affinitied cores's irq count less-equal round up avg core irq count, if there is a irq of parameter irqs
	// affinitied cores's irq count greater-than roundUpAvgCoreIrqCount, then change this irq affinity to another core with least irqs in
	// parameter qualifiedCoresMap.
	roundUpAvgCoreIrqCount := (irqSumCount + len(qualifiedCoresMap) - 1) / len(qualifiedCoresMap)
	for _, irq := range irqs {
		oriCore, _ := nic.Irq2Core[irq]
		// if origin irq core is not qualified, then this irq's affinity MUST be changed to one of qualified cores with least irqs affinitied
		oriCoreQualified := false
		if _, ok := qualifiedCoresMap[oriCore]; ok {
			oriCoreQualified = true

			oriCoreIrqCount, _ := coresIrqCount[oriCore]
			if oriCoreIrqCount <= roundUpAvgCoreIrqCount {
				continue
			}
		} else {
			klog.Warningf("nic %s irq %d affinitied core %d is not qualified core, generally here nic's all irqs affinitied cores should be qualified",
				nic, irq, oriCore)
		}

		targetCore, err := ic.selectPhysicalCoreWithLeastIrqs(coresIrqCount, qualifiedCoresMap)
		if err != nil {
			klog.Errorf("failed to selectPhysicalCoreWithLeastIrqs, err %v", err)
			continue
		}

		// if irqs count diff of source core and dst core <= 1, then needless to change irq's affinity,
		// because if irq affinity change from source core to dst core, then dst core's irqs count >= source core's irq count.
		if oriCoreQualified && coresIrqCount[oriCore]-coresIrqCount[targetCore] <= 1 {
			klog.Warningf("irq count diff original irq core and selected target irq core is less-equal 1")
			continue
		}

		if oriCore == targetCore {
			continue
		}

		if err := util.SetIrqAffinity(irq, targetCore); err != nil {
			klog.Errorf("failed to SetIrqAffinity(%d, %d), err %v", irq, targetCore, err)
			continue
		}

		coresIrqCount[oriCore]--
		coresIrqCount[targetCore]++

		// changedIrq2Core is used to update nic.Irq2Core and nic.SocketIrqCores
		changedIrq2Core[irq] = targetCore
	}

	// make sure no qualified core's irq count less-than round down avg core irq count.
	// if there is a qualified core's irq count less-than round down avg core irq count, then find one qualified core from parameter
	// irqs affinitied cores whose irq count - this irq count greater-equal 2,
	// and cannot find qualified cores beyond paramter irqs affinitied cores, because we donn't know if other irqs can affinity cores in
	// parameter qualifiedCoresMap.

	// here we cannot use nic's all irqs affinitied cores (nic.getIrqCores), because other irqs cannot affinity cores of qualifiedCoresMap
	irqCoresMap := make(map[int64]interface{})
	for _, irq := range irqs {
		core, ok := nic.Irq2Core[irq]
		if !ok {
			klog.Warningf("nic %s irq %d not in Irq2Core %+v", nic, irq, nic.Irq2Core)
			continue
		}
		irqCoresMap[core] = nil
	}

	roundDownAvgCoreIrqCount := irqSumCount / len(qualifiedCoresMap)
	for core, _ := range qualifiedCoresMap {
		coreIrqCount := coresIrqCount[core]

		if coreIrqCount >= roundDownAvgCoreIrqCount {
			continue
		}

		srcCore, err := ic.selectPhysicalCoreWithMostIrqs(coresIrqCount, irqCoresMap)
		if err != nil {
			klog.Errorf("failed to selectPhysicalCoreWithMostIrqs, err %v", err)
			continue
		}

		// if irqs count diff of source core and dst core <= 1, then needless to change irq's affinity,
		// because if irq affinity change from source core to dst core, then dst core's irqs count >= source core's irq count.
		if coresIrqCount[srcCore]-coreIrqCount <= 1 {
			klog.Warningf("irq count diff of selected target irq core and current core is less-equal 1")
			continue
		}

		if core == srcCore {
			continue
		}

		coresIrqsMap := nic.getIrqCoreAffinitiedIrqs()
		srcCoreIrqs, ok := coresIrqsMap[srcCore]
		if !ok {
			klog.Warningf("failed to find target core %d in nic %s coresIrqsMap", srcCore, nic)
			continue
		}

		// pick any one irq is ok
		targetIrq := -1
		for _, irq := range srcCoreIrqs {
			matched := false
			for _, qualifiedIrq := range irqs {
				if irq == qualifiedIrq {
					targetIrq = irq
					matched = true
				}
			}
			if !matched {
				klog.Warningf("nic %s core %d irq %d is not in irqs %+v", nic, srcCore, irqs)
			}
		}

		if targetIrq == -1 {
			klog.Warningf("nic %s core %d affinitied no irq", nic, srcCore)
			continue
		}

		if err := util.SetIrqAffinity(targetIrq, core); err != nil {
			klog.Errorf("failed to SetIrqAffinity(%d, %d), err %v", targetIrq, core, err)
			continue
		}

		coresIrqCount[srcCore]--
		coresIrqCount[core]++

		// changedIrq2Core is used to update nic.Irq2Core and nic.SocketIrqCores
		changedIrq2Core[targetIrq] = core
	}

	if len(changedIrq2Core) == 0 {
		return nil
	}

	// update nic.Irq2Core and nic.SocketIrqCores, just in case nic.sync failed
	for irq, core := range changedIrq2Core {
		nic.Irq2Core[irq] = core
	}

	socketIrqCores, err := getSocketIrqCores(nic.Irq2Core)
	if err != nil {
		klog.Errorf("failed to getSocketIrqCores, err %s", err)
	} else {
		nic.SocketIrqCores = socketIrqCores
	}

	// update nic info
	if err := nic.sync(); err != nil {
		klog.Errorf("failed to sync nic %s, err %v", nic, err)
	}

	return nil
}

func (ic *IrqTuningController) balanceNicIrqsInNumaFairly(nic *NicInfo, assingedSockets []int) error {
	for _, socket := range assingedSockets {
		for _, numa := range ic.CPUInfo.Sockets[socket].NumaIDs {
			numaAffinitiedIrqs := nic.filterCoresAffinitiedIrqs(ic.CPUInfo.GetNodeCPUList(numa))
			if len(numaAffinitiedIrqs) == 0 {
				continue
			}

			qualifiedCoresMap := ic.getNumaQualifiedCoresMapForBalanceFairPolicy(numa)
			if len(qualifiedCoresMap) == 0 {
				klog.Errorf("found zero qualified core in numa %d for nic %s irq affinity", numa, nic)
				continue
			}

			if err := ic.balanceNicIrqsInCoresFairly(nic, numaAffinitiedIrqs, qualifiedCoresMap); err != nil {
				klog.Errorf("failed to balanceNicIrqsInCoresFairly for nic %s in numa %d, err %s", nic, numa, err)
			}
		}
	}

	return nil
}

func (ic *IrqTuningController) balanceNicIrqsInCCDFairly(nic *NicInfo, assingedSockets []int) error {
	if ic.CPUInfo.CPUVendor != cpuid.AMD {
		return fmt.Errorf("invalid cpu arch %s", ic.CPUInfo.CPUVendor)
	}

	for _, socket := range assingedSockets {
		for numaID, amdNuma := range ic.CPUInfo.Sockets[socket].AMDNumas {
			for _, ccd := range amdNuma.CCDs {
				ccdAffinitiedIrqs := nic.filterCoresAffinitiedIrqs(util.GetLLCDomainCPUList(ccd))
				if len(ccdAffinitiedIrqs) == 0 {
					continue
				}

				qualifiedCoresMap := ic.getCCDQualifiedCoresMapForBalanceFairPolicy(ccd)
				if len(qualifiedCoresMap) == 0 {
					klog.Errorf("found zero qualified core in numa %d ccd for nic %s irq affinity", numaID, nic)
					continue
				}

				if err := ic.balanceNicIrqsInCoresFairly(nic, ccdAffinitiedIrqs, qualifiedCoresMap); err != nil {
					klog.Errorf("failed to balanceNicIrqsInCoresFairly for nic %s in numa %d ccd, err %s", nic, numaID, err)
				}
			}
		}
	}

	return nil
}

func (ic *IrqTuningController) balanceNicIrqsInLLCDomainFairly(nic *NicInfo, assingedSockets []int) error {
	if ic.CPUInfo.CPUVendor == cpuid.Intel {
		return ic.balanceNicIrqsInNumaFairly(nic, assingedSockets)
	} else if ic.CPUInfo.CPUVendor == cpuid.AMD {
		return ic.balanceNicIrqsInCCDFairly(nic, assingedSockets)
	} else {
		return fmt.Errorf("unsupport cpu arch: %s", ic.CPUInfo.CPUVendor)
	}
}

func (ic *IrqTuningController) balanceNicIrqsFairly(nic *NicInfo, assingedSockets []int) error {
	if ic.conf.IrqTuningPolicy == config.IrqTuningBalanceFair {
		return ic.balanceNicIrqsInLLCDomainFairly(nic, assingedSockets)
	} else {
		return ic.balanceNicIrqsInNumaFairly(nic, assingedSockets)
	}
}

func (ic *IrqTuningController) tuneSriovContainerNicsIrqsAffinitySelfCores(cnt *ContainerInfoWrapper) error {
	rawQualifiedCoresMap := ic.getSocketsQualifiedCoresMapForBalanceFairPolicy([]int{})

	qualifiedCoresMap := make(map[int64]interface{})
	for _, cpuset := range cnt.ActualCPUSet {
		cpus := cpuset.ToSliceInt64()
		for _, cpu := range cpus {
			if _, ok := rawQualifiedCoresMap[cpu]; ok {
				qualifiedCoresMap[cpu] = nil
			}
		}
	}

	for _, nic := range cnt.Nics {
		if err := ic.tuneNicIrqsAffinityQualifiedCores(nic, nic.getIrqs(), qualifiedCoresMap); err != nil {
			klog.Errorf("failed to tuneNicIrqsAffinityQualifiedCores for container %s nic %s, err %v",
				cnt.ContainerID, nic, err)
		}
	}

	return nil
}

func (ic *IrqTuningController) TuneNicsIrqsAffinityQualifiedCoresFairly() error {
	for _, nic := range ic.Nics {
		if nic.IrqAffinityPolicy != IrqCoresExclusive {
			if err := ic.tuneNicIrqsAffinityFairly(nic.NicInfo, nic.AssignedSockets); err != nil {
				return err
			}
		}
	}

	for _, cnt := range ic.Containers {
		if !cnt.IsSriovContainer {
			continue
		}

		if err := ic.tuneSriovContainerNicsIrqsAffinitySelfCores(cnt); err != nil {
			klog.Errorf("failed to tuneSriovContainerNicsIrqsAffinitySelfCores for container %s, err %v", cnt.ContainerID, err)
		}
	}

	return nil
}

func (ic *IrqTuningController) balanceSriovContainerNicsIrqsInSelfCores(cnt *ContainerInfoWrapper) error {
	rawQualifiedCoresMap := ic.getSocketsQualifiedCoresMapForBalanceFairPolicy([]int{})

	qualifiedCoresMap := make(map[int64]interface{})
	for _, cpuset := range cnt.ActualCPUSet {
		cpus := cpuset.ToSliceInt64()
		for _, cpu := range cpus {
			if _, ok := rawQualifiedCoresMap[cpu]; ok {
				qualifiedCoresMap[cpu] = nil
			}
		}
	}

	for _, nic := range cnt.Nics {
		if err := ic.balanceNicIrqsInCoresFairly(nic, nic.getIrqs(), qualifiedCoresMap); err != nil {
			klog.Errorf("failed to balanceNicIrqsInCoresFairly for container %s nic %s, err %v",
				cnt.ContainerID, nic, err)
		}
	}

	return nil
}

func (ic *IrqTuningController) BalanceNicsIrqsInQualifiedCoresFairly() error {
	for _, nic := range ic.Nics {
		if nic.IrqAffinityPolicy != IrqCoresExclusive {
			if err := ic.balanceNicIrqsFairly(nic.NicInfo, nic.AssignedSockets); err != nil {
				klog.Errorf("failed to balanceNicIrqsFairly for nic %s, err %s", nic.NicInfo, err)
			}
		}
	}

	for _, cnt := range ic.Containers {
		if !cnt.IsSriovContainer {
			continue
		}

		if err := ic.balanceSriovContainerNicsIrqsInSelfCores(cnt); err != nil {
			klog.Errorf("failed to balanceSriovContainerNicsIrqsInSelfCores for container %s, err %v", cnt.ContainerID, err)
		}
	}

	return nil
}

// need to first put nics's irqs affinity their qualified cores
// second balance nics' irqs in their qualified cores
func (ic *IrqTuningController) TuneIrqAffinityForAllNicsWithBalanceFairPolicy() error {
	// put all irqs of nics with balance-fair policy to qualified cpus
	if err := ic.TuneNicsIrqsAffinityQualifiedCoresFairly(); err != nil {
		return fmt.Errorf("failed to TuneNicsIrqsAffinityQualifiedCoresFairly, err %s", err)
	}

	// balance nic irqs in corresponding qualified cpus
	if err := ic.BalanceNicsIrqsInQualifiedCoresFairly(); err != nil {
		return fmt.Errorf("failed to BalanceNicsIrqsInQualifiedCoresFairly, err %s", err)
	}

	return nil
}

// We can evaluate each nic's each irq core's load only when this nic no overlapped irq cores with other nics,
// note that the "nic" mentioned here refers to the logical nic, later will explain what the logical nic is.
// Theoretically, there is a possibility that ic.Nics have overlapped irq cores only when irq-tuning manager first initialize.
// but just in case, we still check if ic.Nics have overlapped irq cores in periodic tuning.
//
// We must make sure each nic's irq affinity its assigned sockets, because if one nic's irq affinity a socket which not assigned to this nic but
// assgined to other nics, then it's possible that this nic has overlapped irq cores with other nics.
//
// if one socket assigned multiple nics, a representative example is that a machine with only one socket, and has multiple nics, then nics irq cores
// can overlapped, because we allocate exclusive irq cores in socket level, multiple nics which assigned to the same socket shares the same exclusive
// irq cores pool in this socket, then these nics are considered as one aggregated logic nic to calculate irq affinities and irq cores assignment.
// So if a machine with one socket, needless to separate nics's irq cores.
// Additional explanation: now we only support atmost two uplink nics, so only when a machine with only one socket, and has 2 nics will result in one socket
// assigned multiple nics.
//
// if in machine with two sockets, there are two nics with overlapped irq cores, we need to set each nic's irqs to this nic's assigned socket's cpus
// in balance-fair policy, balance-fair policy assigns nic's irqs to specified sockets, in this case of two nics have overlapped irq cores in machine with two sockets,
// each nic's irqs will affinity socket which is assgined to this nic in NewIrqTuningController, so the two nic's irqs cores will completely has no overlap.
// then separation of two nics's irq cores is done, wait a while to settle down the softirq usage in new irq cores, then start to collect indicators's stats.
//
// N.B., there are corner cases that TidyUpNicIrqsAffinityCPUs happeded in NewIrqTuningController if there are irqs affinity multiple cores, and some
// irq's truely affinity irq cores may changed, then two nic's may have no overlapped irq cores here, and will not wait a while to settle down the nic's softirq
// usage in new irq cores, but it's not a big thing.
func (ic *IrqTuningController) separateNicsOverlappedIrqCores() {
	if len(ic.Nics) > 2 {
		klog.Errorf("has %d uplink nics", len(ic.Nics))
		for _, n := range ic.Nics {
			klog.Infof("nic: %s", n.NicInfo)
		}
		return
	}

	if len(ic.Nics) < 2 || len(ic.CPUInfo.Sockets) < 2 {
		return
	}

	hasOverlappedIrqCores := false

	for _, core1 := range ic.Nics[0].NicInfo.Irq2Core {
		for _, core2 := range ic.Nics[1].NicInfo.Irq2Core {
			if core2 == core1 {
				hasOverlappedIrqCores = true
				break
			}
		}
		if hasOverlappedIrqCores {
			break
		}
	}

	if hasOverlappedIrqCores {
		nicsAssignedSocketsOverlapped := false
		for i, nic := range ic.Nics {
			if i == 0 {
				continue
			}

			for _, n := range ic.Nics[:i] {
				for _, s1 := range nic.AssignedSockets {
					for _, s2 := range n.AssignedSockets {
						if s1 == s2 {
							klog.Warningf("both nic %s and %s assigned socket %d",
								nic.NicInfo, n.NicInfo, s1)
							nicsAssignedSocketsOverlapped = true
						}
					}
				}
			}
		}
		if nicsAssignedSocketsOverlapped {
			klog.Warningf("needless to separate nics's irq affinitied cores, because their assigned sockets overlapped")
			return
		}

		if ic.Nics[0].IrqAffinityPolicy != InitTuning && ic.Nics[1].IrqAffinityPolicy != InitTuning {
			klog.Errorf("2 nics initialize tuning already completed, nic %s and %s should not have overlapped irq cores",
				ic.Nics[0].NicInfo, ic.Nics[1].NicInfo)
		}

		if ic.Nics[0].IrqAffinityPolicy == IrqCoresExclusive {
			klog.Errorf("nic %s with exclusive irq cores, should not has overlapped irq cores with another nic %s",
				ic.Nics[0].NicInfo, ic.Nics[1].NicInfo)
		}

		if ic.Nics[1].IrqAffinityPolicy == IrqCoresExclusive {
			klog.Errorf("nic %s with exclusive irq cores, should not has overlapped irq cores with another nic %s",
				ic.Nics[1].NicInfo, ic.Nics[0].NicInfo)
		}

		// syncContainers here is for excluding unqualified cores, like exclusive irq cores, katambm cpus, and try to exclude sriov dedicated cores,
		// and srvio containers contribute to cores's irq count which is used to select cores with least irq count from optional cores.
		//
		// here we will remove the overlap between two ic.Nics's irq cores, but cannot make sure to remove the overlap between ic.Nic irq cores and SRIOV container's irq cores.
		if err := ic.syncContainers(); err != nil {
			klog.Errorf("failed to syncContainers, err %v", err)
		}

		for _, nic := range ic.Nics {
			if err := ic.tuneNicIrqsAffinityFairly(nic.NicInfo, nic.AssignedSockets); err != nil {
				klog.Errorf("[irq tuning] failed to tuneNicIrqsAffinityFairly for nic %s, socket: %+v, err %s", nic.NicInfo, nic.AssignedSockets, err)
			}
		}

		time.Sleep(time.Minute)
	}
}

// return value
// bool: if is sriov container
func (ic *IrqTuningController) getNicsIfSRIOVContainer(cnt *irqtuner.ContainerInfo) (bool, []*NicInfo) {
	// container maybe exited
	pids, err := util.GetCgroupPids(cnt.CgroupPath)
	if err != nil {
		klog.Errorf("failed to GetCgroupPids(%s), err %v", cnt.CgroupPath, err)
		return false, nil
	}

	// container maybe exited
	if len(pids) == 0 {
		klog.Warningf("container with id: %s, cgrouppath: %s has no pid", cnt.ContainerID, cnt.CgroupPath)
		return false, nil
	}

	var netnsInode uint64
	for _, pid := range pids {
		inode, err := util.GetProcessNameSpaceInode(pid, util.NetNS)
		if err == nil {
			netnsInode = inode
			break
		}
	}

	// container maybe exited
	if netnsInode == 0 {
		klog.Warningf("failed to GetProcessNameSpaceInode for container with id: %s, cgrouppath: %s has ", cnt.ContainerID, cnt.CgroupPath)
		return false, nil
	}

	// check this container's netns is shared netns for all containers, like hostnetns, ns2
	// N.B., shared netns for all containers maybe changed, so missed match in shared netns for all containers
	// dose not means this container's netns is not shared with other containers, so check if this container's
	// netns name has prefix "cni-" is neccessary.
	for _, nic := range ic.Nics {
		if netnsInode == nic.NicInfo.NetNSInode {
			return false, nil
		}
	}

	var netnsName string
	netnsList, err := util.ListNetNS()
	if err != nil {
		klog.Errorf("failed to ListNetNS, err %v", err)
		return false, nil
	}

	for _, netnsInfo := range netnsList {
		if netnsInfo.NetNSInode == netnsInode {
			netnsName = netnsInfo.NetNSName
		}
	}

	// container maybe exited
	if netnsName == "" {
		return false, nil
	}

	// all sriov netns's names hava prefix "cni-", sriov netns is managed by cni plugin
	if !strings.HasPrefix(netnsName, "cni-") {
		return false, nil
	}

	netnsInfo := util.NetNSInfo{
		NetNSName:  netnsName,
		NetNSInode: netnsInode,
	}
	activeUplinkNics, err := util.ListActiveUplinkNicsFromNetNS(netnsInfo)
	if err != nil {
		klog.Errorf("failed to ListActiveUplinkNicsFromNetNS for netns %s, err %v", netnsName, err)
		return false, nil
	}

	// bridge mode
	if len(activeUplinkNics) == 0 {
		return false, nil
	}

	if len(activeUplinkNics) > 1 {
		klog.Warningf("sriov container %s has %d nics, sriov container should has only 1 nic", cnt.ContainerID, len(activeUplinkNics))
	}

	var nics []*NicInfo

	for _, nic := range activeUplinkNics {
		nicInfo, err := GetNicInfo(nic)
		if err != nil {
			klog.Errorf("failed to GetNicInfo for nic %s, err %v", nic, err)
			continue
		}
		nics = append(nics, nicInfo)
	}

	// container maybe exited
	if len(nics) == 0 {
		return false, nil
	}

	return true, nics
}

func (ic *IrqTuningController) getNewContainers(containers []irqtuner.ContainerInfo) ([]*ContainerInfoWrapper, error) {
	var newContainers []*ContainerInfoWrapper
	for _, cnt := range containers {
		if _, ok := ic.Containers[cnt.ContainerID]; ok {
			continue
		}

		isSriovContainer, nics := ic.getNicsIfSRIOVContainer(&cnt)
		newContainers = append(newContainers, &ContainerInfoWrapper{
			ContainerInfo:    &cnt,
			IsSriovContainer: isSriovContainer,
			Nics:             nics,
		})
	}

	return newContainers, nil
}

func (ic *IrqTuningController) syncContainers() error {
	syncContainersRetryCount := 0
retry:
	containers, err := ic.IrqStateAdapter.ListContainers()
	if err != nil {
		klog.Errorf("failed to ListContainers, err %v", err)
		if syncContainersRetryCount < 2 {
			syncContainersRetryCount++
			time.Sleep(time.Second)
			goto retry
		}
		return fmt.Errorf("failed to ListContainers, err %v", err)
	}

	containersMap := make(map[string]*irqtuner.ContainerInfo)
	for _, cnt := range containers {
		containersMap[cnt.ContainerID] = &cnt
	}

	// filter out non-existed containers
	tmpContainers := make(map[string]*ContainerInfoWrapper)
	for containerID, cnt := range ic.Containers {
		if _, ok := containersMap[containerID]; ok {
			tmpContainers[containerID] = cnt
		}
	}
	ic.Containers = tmpContainers

	newContainers, err := ic.getNewContainers(containers)
	if err != nil {
		return fmt.Errorf("failed to get new containers, err %v", err)
	}

	for _, container := range newContainers {
		ic.Containers[container.ContainerID] = container
	}

	forbiddendCores, err := ic.IrqStateAdapter.GetIRQForbiddenCores()
	if err != nil {
		return fmt.Errorf("failed to GetIRQForbiddenCores, err %s", err)
	}

	ic.IrqAffForbiddenCores = forbiddendCores.ToSliceInt64()

	return nil
}

func (ic *IrqTuningController) fallbackToBalanceFairPolicyByError(nic *NicIrqTuningManager, err error) {
	klog.Infof("fallback to balance-fair policy for nic %s, by err %s", nic.NicInfo, err)

	nic.FallbackToBalanceFair = true

	// get IrqAffinityPolicy before TuneNicIrqAffinityWithBalanceFairPolicy
	irqAffinityPolicy := nic.IrqAffinityPolicy

	if err := ic.TuneNicIrqAffinityWithBalanceFairPolicy(nic); err != nil {
		klog.Errorf("failed to TuneNicIrqAffinityWithBalanceFairPolicy for nic %s, err %s", nic.NicInfo, err)
	}

	if _, ok := ic.IrqAffinityChanges[nic.NicInfo.IfIndex]; ok {
		delete(ic.IrqAffinityChanges, nic.NicInfo.IfIndex)
	}

	if irqAffinityPolicy == IrqCoresExclusive {
		totalExclusiveIrqCores, err := ic.getCurrentTotalExclusiveIrqCores()
		if err != nil {
			klog.Errorf("failed to getCurrentTotalExclusiveIrqCores, err %s", err)
			return
		}

		irqCores := nic.NicInfo.getIrqCores()
		totalExclusiveIrqCores = calculateIrqCoresDiff(totalExclusiveIrqCores, irqCores)
		if err := ic.IrqStateAdapter.SetExclusiveIRQCPUSet(machine.NewCPUSet(util.ConvertInt64SliceToIntSlice(totalExclusiveIrqCores)...)); err != nil {
			klog.Errorf("failed to decrease irq cores, err %s", err)
		}
	}
}

func buildNicIrqAffinityChange(nic *NicIrqTuningManager, newIrqAffinityPolicy IrqAffinityPolicy, newIrqCores []int64) *IrqAffinityChange {
	if (newIrqAffinityPolicy != nic.IrqAffinityPolicy) && (newIrqAffinityPolicy == IrqCoresExclusive || nic.IrqAffinityPolicy == IrqCoresExclusive) {
		nic.DisableExclusionThreshSuccCount = 0
		nic.EnableExclusionThreshSuccCount = 0
		nic.IrqCoresExclusionLastSwitchTime = time.Now()
	}

	return &IrqAffinityChange{
		Nic:                  nic,
		OldIrqAffinityPolicy: nic.IrqAffinityPolicy,
		NewIrqAffinityPolicy: newIrqAffinityPolicy,
		OldIrqCores:          nic.NicInfo.getIrqCores(),
		NewIrqCores:          newIrqCores,
	}
}

// nic.IrqAffinityPolicy will be changed when practically processing irqAffinityChangedNics later
// in adaptIrqAffinityPolicy only record IrqAffinityChange
func (ic *IrqTuningController) adaptIrqAffinityPolicy(oldIndicatorsStats *IndicatorsStats) {
	timeDiff := ic.IndicatorsStats.UpdateTime.Sub(oldIndicatorsStats.UpdateTime).Seconds()

	shouldFallbackToBalanceFairPolicy := false
	// if there are katabm container or sriov container, then fallback to balance-fair policy,
	// but needless to set nic.FallbackToBalanceFair = true
	for _, cnt := range ic.Containers {
		if cnt.isKataBMContainer() || cnt.IsSriovContainer {
			shouldFallbackToBalanceFairPolicy = true
			break
		}
	}

	oldNicStats := oldIndicatorsStats.NicStats
	for _, nic := range ic.Nics {
		if shouldFallbackToBalanceFairPolicy || nic.FallbackToBalanceFair {
			if nic.IrqAffinityPolicy != IrqBalanceFair {
				ic.IrqAffinityChanges[nic.NicInfo.IfIndex] = buildNicIrqAffinityChange(nic, IrqBalanceFair, nil)
			}
			continue
		}

		oldStats, ok := oldNicStats[nic.NicInfo.IfIndex]
		if !ok {
			klog.Errorf("impossible, failed to find nic %s in old nic stats", nic.NicInfo)
			continue
		}

		stats, ok := ic.NicStats[nic.NicInfo.IfIndex]
		if !ok {
			klog.Errorf("impossible, failed to find nic %s in nic stats", nic.NicInfo)
			continue
		}

		if stats.TotalRxPackets < oldStats.TotalRxPackets {
			klog.Errorf("nic %s current rx packets(%d) less than last rx packets(%d)", nic.NicInfo, stats.TotalRxPackets, oldStats.TotalRxPackets)
			continue
		}

		rxPPS := (stats.TotalRxPackets - oldStats.TotalRxPackets) / uint64(timeDiff)

		if nic.IrqAffinityPolicy == InitTuning {
			if rxPPS >= ic.conf.IrqCoresExclusionConf.Thresholds.EnableThresholds.RxPPSThresh {
				nic.IrqCoresExclusionLastSwitchTime = time.Now()
				// NewIrqCores will be populated after completing exclusive irq cores calculation and selection
				ic.IrqAffinityChanges[nic.NicInfo.IfIndex] = buildNicIrqAffinityChange(nic, IrqCoresExclusive, nil)
			} else {
				// needless to set NewIrqCores for IrqBalanceFair policy, irq cores will be calculated when practically set IrqBalanceFair policy for it.
				ic.IrqAffinityChanges[nic.NicInfo.IfIndex] = buildNicIrqAffinityChange(nic, IrqBalanceFair, nil)
			}
			continue
		}

		if nic.IrqAffinityPolicy == IrqCoresExclusive {
			if rxPPS <= ic.conf.IrqCoresExclusionConf.Thresholds.DisableThresholds.RxPPSThresh {
				nic.DisableExclusionThreshSuccCount++
				// after all exclusive irq cores pratically tuned,
				// will set balance-fair policy for nics whose irq cores exclusion switched from enable to disable
				if nic.DisableExclusionThreshSuccCount >= ic.conf.IrqCoresExclusionConf.Thresholds.DisableThresholds.SuccessiveCount &&
					time.Since(nic.IrqCoresExclusionLastSwitchTime).Seconds() > ic.conf.IrqCoresExclusionConf.SuccessiveSwitchInterval {
					// needless to set NewIrqCores for IrqBalanceFair policy, irq cores will be calculated when practically set IrqBalanceFair policy for it.
					ic.IrqAffinityChanges[nic.NicInfo.IfIndex] = buildNicIrqAffinityChange(nic, IrqBalanceFair, nil)
				}
			} else if rxPPS >= ic.conf.IrqCoresExclusionConf.Thresholds.EnableThresholds.RxPPSThresh {
				nic.DisableExclusionThreshSuccCount = 0
			} else {
				// nothing
			}
		} else {
			if rxPPS >= ic.conf.IrqCoresExclusionConf.Thresholds.EnableThresholds.RxPPSThresh {
				nic.EnableExclusionThreshSuccCount++
				if nic.EnableExclusionThreshSuccCount >= ic.conf.IrqCoresExclusionConf.Thresholds.EnableThresholds.SuccessiveCount &&
					time.Since(nic.IrqCoresExclusionLastSwitchTime).Seconds() > ic.conf.IrqCoresExclusionConf.SuccessiveSwitchInterval {
					// NewIrqCores will be populated after completing exclusive irq cores calculation and selection
					ic.IrqAffinityChanges[nic.NicInfo.IfIndex] = buildNicIrqAffinityChange(nic, IrqCoresExclusive, nil)
				}
			} else if rxPPS <= ic.conf.IrqCoresExclusionConf.Thresholds.DisableThresholds.RxPPSThresh {
				nic.EnableExclusionThreshSuccCount = 0
			} else {
				// nothing
			}
		}
	}

	return
}

// get cores which are unqualified for IrqCoreExclusive affinity policy
func (ic *IrqTuningController) getUnqualifiedCoresMapForAllNicsExclusiveIrqCores() map[int64]interface{} {
	unqualifiedCores := ic.getKataBMContainerCPUs()

	// forbbiden cores cannot be used as exclusive irq cores, but can be affinitied by irqs of nics with balance-fair policy
	if len(ic.IrqAffForbiddenCores) > 0 {
		unqualifiedCores = append(unqualifiedCores, ic.IrqAffForbiddenCores...)
	}

	unqualifiedCoresMap := make(map[int64]interface{})
	for _, core := range unqualifiedCores {
		unqualifiedCoresMap[core] = nil
	}
	return unqualifiedCoresMap
}

// get cores which are unqualified for IrqCoreExclusive affinity policy
func (ic *IrqTuningController) getUnqualifiedCoresMapForNicExclusiveIrqCores(nic *NicInfo) map[int64]interface{} {
	unqualifiedCoresMap := ic.getUnqualifiedCoresMapForAllNicsExclusiveIrqCores()

	// other nics's exclusive irq cores are unqualified for current nic's exclusive irq cores
	// of cousre current nic's exclusive irq cores is qualified
	var ifIndex []int
	if nic != nil {
		ifIndex = []int{nic.IfIndex}
	}
	exclusiveIrqCores := ic.getExclusiveIrqCores(ifIndex)

	for _, core := range exclusiveIrqCores {
		unqualifiedCoresMap[core] = nil
	}
	return unqualifiedCoresMap
}

func (ic *IrqTuningController) getNumaQualifiedCoresMapForExclusiveIrqCores(numa int) map[int64]interface{} {
	cpuList := ic.CPUInfo.GetNodeCPUList(numa)
	return ic.getQualifiedCoresMap(cpuList, ic.getUnqualifiedCoresMapForNicExclusiveIrqCores(nil))
}

func (ic *IrqTuningController) getNumaQualifiedCoresMapForNicExclusiveIrqCores(nic *NicInfo, numa int) map[int64]interface{} {
	cpuList := ic.CPUInfo.GetNodeCPUList(numa)
	return ic.getQualifiedCoresMap(cpuList, ic.getUnqualifiedCoresMapForNicExclusiveIrqCores(nic))
}

func (ic *IrqTuningController) getNicExclusiveIrqCoresMax(nic *NicIrqTuningManager) int {
	assignedSocketsCoresCount := 0
	for _, socket := range nic.AssignedSockets {
		assignedSocketsCoresCount += len(ic.CPUInfo.SocketCPUs[socket])
	}

	exclusiveIrqCoresMax := assignedSocketsCoresCount * ic.conf.IrqCoresAdjustConf.IrqCoresPercentMax / 100
	return exclusiveIrqCoresMax
}

func (ic *IrqTuningController) getNicExclusiveIrqCoresMin(nic *NicIrqTuningManager) int {
	assignedSocketsCoresCount := 0
	for _, socket := range nic.AssignedSockets {
		assignedSocketsCoresCount += len(ic.CPUInfo.SocketCPUs[socket])
	}

	exclusiveIrqCoresMin := assignedSocketsCoresCount * ic.conf.IrqCoresAdjustConf.IrqCoresPercentMin / 100

	if exclusiveIrqCoresMin < 1 {
		return 1
	}
	return exclusiveIrqCoresMin
}

func (ic *IrqTuningController) calculateExclusiveIrqCores(nic *NicIrqTuningManager, irqCoresUsage float64) int {
	expectedIrqCoresCount := int(math.Ceil(irqCoresUsage * 100 / float64(ic.conf.IrqCoresExpectedCpuUtil)))

	exclusiveIrqCoresMax := ic.getNicExclusiveIrqCoresMax(nic)
	if expectedIrqCoresCount > exclusiveIrqCoresMax {
		klog.Warningf("nic %s expected exclusive irq cores count %d is greater-than max limit %d", nic.NicInfo, expectedIrqCoresCount, exclusiveIrqCoresMax)
		expectedIrqCoresCount = exclusiveIrqCoresMax
	}

	exclusiveIrqCoresMin := ic.getNicExclusiveIrqCoresMin(nic)
	if expectedIrqCoresCount < exclusiveIrqCoresMin {
		expectedIrqCoresCount = exclusiveIrqCoresMax
	}

	return expectedIrqCoresCount
}

func (ic *IrqTuningController) selectExclusiveIrqCoresFromNuma(irqCoresNum int, socketID int, numaID int, qualifiedCoresMap map[int64]interface{}) ([]int64, error) {
	socket, ok := ic.CPUInfo.Sockets[socketID]
	if !ok {
		return nil, fmt.Errorf("invalid socket id %d", socketID)
	}

	if irqCoresNum <= 0 {
		return nil, fmt.Errorf("irqCoresNum %d less-equal 0", irqCoresNum)
	}

	var selectedIrqCores []int64
	selectedIrqCoresCount := 0
	if ic.CPUInfo.CPUVendor == cpuid.AMD {
		numa, ok := socket.AMDNumas[numaID]
		if !ok {
			return nil, fmt.Errorf("invalid numa id %d", numaID)
		}

		for _, ccd := range numa.CCDs {
			for _, phyCore := range ccd.PhyCores {
				for _, cpu := range phyCore.CPUs {
					if _, ok := qualifiedCoresMap[cpu]; ok {
						selectedIrqCores = append(selectedIrqCores, cpu)
						selectedIrqCoresCount++
						if selectedIrqCoresCount >= irqCoresNum {
							return selectedIrqCores, nil
						}
					}
				}
			}
		}
	} else if ic.CPUInfo.CPUVendor == cpuid.Intel {
		numa, ok := socket.IntelNumas[numaID]
		if !ok {
			return nil, fmt.Errorf("invalid numa id %d", numaID)
		}

		for _, phyCore := range numa.PhyCores {
			for _, cpu := range phyCore.CPUs {
				if _, ok := qualifiedCoresMap[cpu]; ok {
					selectedIrqCores = append(selectedIrqCores, cpu)
					selectedIrqCoresCount++
					if selectedIrqCoresCount >= irqCoresNum {
						return selectedIrqCores, nil
					}
				}
			}
		}
	}

	return nil, fmt.Errorf("selected irq cores count %d, less than irqCoresNum %d", len(selectedIrqCores), irqCoresNum)
}

// for nic irq affinity changed from non-IrqCoresExclusive to IrqCoresExclusive
func (ic *IrqTuningController) selectExclusiveIrqCoresForNic(nic *NicIrqTuningManager, irqCoresNum int) ([]int64, error) {
	var exclusiveIrqCores []int64

	// alloc exclusive irq cores evenly from nic's assigned sockets
	avgSocketIrqCoresCount := irqCoresNum / len(nic.AssignedSockets)
	remainder := irqCoresNum % len(nic.AssignedSockets)

	for _, socket := range nic.AssignedSockets {
		socketIrqCoresCount := 0
		if remainder > 0 {
			socketIrqCoresCount = avgSocketIrqCoresCount + 1
			remainder--
		} else {
			socketIrqCoresCount = avgSocketIrqCoresCount
		}

		if socketIrqCoresCount == 0 {
			continue
		}

		// alloc exclusive irq cores evenly from socket numas
		socketNumas := ic.CPUInfo.Sockets[socket].NumaIDs // NumaIDs has been sorted in initializing
		avgNumaIrqCoresCount := socketIrqCoresCount / len(socketNumas)
		numaRemainder := socketIrqCoresCount % len(socketNumas)

		for _, numa := range socketNumas {
			numaIrqCoresCount := 0
			if numaRemainder > 0 {
				numaIrqCoresCount = avgNumaIrqCoresCount + 1
				numaRemainder--
			} else {
				numaIrqCoresCount = avgNumaIrqCoresCount
			}

			if numaIrqCoresCount == 0 {
				continue
			}

			qualifiedCoresMap := ic.getNumaQualifiedCoresMapForNicExclusiveIrqCores(nic.NicInfo, numa)
			if len(qualifiedCoresMap) < numaIrqCoresCount {
				return nil, fmt.Errorf("numa %s with qualified cores count %d less than numa assigned exclusive irq cores count",
					len(qualifiedCoresMap), numaIrqCoresCount)
			}

			numaExclusiveIrqCores, err := ic.selectExclusiveIrqCoresFromNuma(numaIrqCoresCount, socket, numa, qualifiedCoresMap)
			if err != nil {
				return nil, fmt.Errorf("failed to selectExclusiveIrqCoresFromNuma(%d, %d, %d) for nic %s",
					numaIrqCoresCount, socket, numa, nic.NicInfo)
			}
			exclusiveIrqCores = append(exclusiveIrqCores, numaExclusiveIrqCores...)
		}
	}

	return exclusiveIrqCores, nil
}

func (ic *IrqTuningController) allocExclusiveIrqCoresForNic_deprecated(nic *NicIrqTuningManager, incIrqCoresCount int) ([]int64, error) {
	if incIrqCoresCount <= 0 {
		return nil, fmt.Errorf("increase non-positive number exclusive irq cores")
	}

	var exclusiveIrqCores []int64

	expectedIrqCoresCount := len(nic.NicInfo.getIrqCores()) + incIrqCoresCount
	// alloc exclusive irq cores evenly from nic's assigned sockets
	avgSocketIrqCoresCount := expectedIrqCoresCount / len(nic.AssignedSockets)
	remainder := expectedIrqCoresCount % len(nic.AssignedSockets)

	for _, socket := range nic.AssignedSockets {
		socketIrqCoresCount := 0
		if remainder > 0 {
			socketIrqCoresCount = avgSocketIrqCoresCount + 1
			remainder--
		} else {
			socketIrqCoresCount = avgSocketIrqCoresCount
		}

		if socketIrqCoresCount == 0 {
			continue
		}

		if socketIrqCoresCount == len(nic.NicInfo.SocketIrqCores[socket]) {
			continue
		}

		if socketIrqCoresCount < len(nic.NicInfo.SocketIrqCores[socket]) {
			klog.Warningf("socket %d new calculated irq cores count %d less-than original irq cores count %d",
				socket, socketIrqCoresCount, len(nic.NicInfo.SocketIrqCores[socket]))
			continue
		}

		// alloc exclusive irq cores evenly from socket numas
		socketNumas := ic.CPUInfo.Sockets[socket].NumaIDs // NumaIDs has been sorted in initializing
		avgNumaIrqCoresCount := socketIrqCoresCount / len(socketNumas)
		numaRemainder := socketIrqCoresCount % len(socketNumas)

		for _, numa := range socketNumas {
			numaIrqCoresCount := 0
			if numaRemainder > 0 {
				numaIrqCoresCount = avgNumaIrqCoresCount + 1
				numaRemainder--
			} else {
				numaIrqCoresCount = avgNumaIrqCoresCount
			}

			if numaIrqCoresCount == 0 {
				continue
			}

			oriNumaIrqCores := nic.NicInfo.filterIrqCores(ic.CPUInfo.GetNodeCPUList(numa))
			if numaIrqCoresCount == len(oriNumaIrqCores) {
				continue
			}

			if numaIrqCoresCount < len(oriNumaIrqCores) {
				klog.Warningf("numa %d new calculated irq cores count %d less-than original irq cores count %d",
					numa, numaIrqCoresCount, len(oriNumaIrqCores))
				continue
			}
			numaIncIrqCoresCount := numaIrqCoresCount - len(oriNumaIrqCores)

			qualifiedCoresMap := ic.getNumaQualifiedCoresMapForExclusiveIrqCores(numa)
			numaExclusiveIrqCores, err := ic.selectExclusiveIrqCoresFromNuma(numaIncIrqCoresCount, socket, numa, qualifiedCoresMap)
			if err != nil {
				return nil, fmt.Errorf("failed to selectExclusiveIrqCoresFromNuma(%d, %d, %d) for nic %s",
					numaIrqCoresCount, socket, numa, nic.NicInfo)
			}
			exclusiveIrqCores = append(exclusiveIrqCores, numaExclusiveIrqCores...)
		}
	}

	return exclusiveIrqCores, nil
}

func (ic *IrqTuningController) calculateNicExclusiveIrqCoresIncrease(nic *NicIrqTuningManager, oldIndicatorsStats *IndicatorsStats) ([]int64, error) {
	incConf := ic.conf.IrqCoresAdjustConf.IrqCoresIncConf
	lastInc := nic.LastExclusiveIrqCoresInc

	if lastInc != nil && time.Since(lastInc.TimeStamp).Seconds() < float64(incConf.SuccessiveIncInterval) {
		klog.Infof("two successive exclusive irq cores increase interval %d less than configured interval threshold %d",
			int(time.Since(lastInc.TimeStamp).Seconds()), incConf.SuccessiveIncInterval)
		return nil, nil
	}

	exclusiveIrqCoresMax := ic.getNicExclusiveIrqCoresMax(nic)
	if len(nic.NicInfo.getIrqCores()) >= exclusiveIrqCoresMax {
		klog.Warningf("nic %s exclusive irq cores count %d has already reached max limit %d, cannot increase any more",
			nic.NicInfo, len(nic.NicInfo.getIrqCores()), exclusiveIrqCoresMax)
		return nil, nil
	}

	_, cpuUtilAvg := calculateCpuUtils(oldIndicatorsStats.CPUStats, ic.IndicatorsStats.CPUStats, nic.NicInfo.getIrqCores())

	if cpuUtilAvg.IrqUtil < incConf.Thresholds.IrqCoresAvgCpuUtilThresh {
		return nil, nil
	}

	if cpuUtilAvg.IrqUtil >= incConf.IrqCoresCpuFullThresh {
		// fallback to balance-fair policy
		ic.IrqAffinityChanges[nic.NicInfo.IfIndex] = buildNicIrqAffinityChange(nic, IrqBalanceFair, nil)
		if err := ic.TuneNicIrqAffinityWithBalanceFairPolicy(nic); err != nil {
			klog.Errorf("failed to TuneNicIrqAffinityWithBalanceFairPolicy for nic %s, err %s", nic.NicInfo, err)
		}

		return nil, nil
	}

	irqCoresCpuUsage := float64(len(nic.NicInfo.getIrqCores())*cpuUtilAvg.IrqUtil) / 100

	expectedIrqCoresCount := ic.calculateExclusiveIrqCores(nic, irqCoresCpuUsage)

	oriIrqCoresCount := len(nic.NicInfo.getIrqCores())
	if expectedIrqCoresCount <= oriIrqCoresCount {
		klog.Warningf("needless to increase irq cores, new calculated irq cores count is %d, original irq cores count %d",
			expectedIrqCoresCount, oriIrqCoresCount)
		return nil, nil
	}

	newIrqCores, err := ic.selectExclusiveIrqCoresForNic(nic, expectedIrqCoresCount)
	if err != nil {
		return nil, fmt.Errorf("failed to selectExclusiveIrqCoresForNic for nic %s with exclusive irq core count %d", nic.NicInfo, expectedIrqCoresCount)
	}

	return newIrqCores, nil
}

func (ic *IrqTuningController) calculateNicIrqCoresWhenSwitchToIrqCoresExclusive(nic *NicIrqTuningManager, oldIndicatorsStats *IndicatorsStats) ([]int64, error) {
	_, cpuUtilAvg := calculateCpuUtils(oldIndicatorsStats.CPUStats, ic.IndicatorsStats.CPUStats, nic.NicInfo.getIrqCores())

	irqCoresCpuUsage := float64(len(nic.NicInfo.getIrqCores())*cpuUtilAvg.IrqUtil) / 100

	expectedIrqCoresCount := ic.calculateExclusiveIrqCores(nic, irqCoresCpuUsage)

	// scale up expected irq cores count with a factor(1.2) when nic's irq affinity policy switched from non-IrqCoresExclusive to IrqCoresExclusive
	expectedIrqCoresCount = expectedIrqCoresCount * 12 / 10
	if expectedIrqCoresCount > ic.getNicExclusiveIrqCoresMax(nic) {
		expectedIrqCoresCount = ic.getNicExclusiveIrqCoresMax(nic)
	}

	newIrqCores, err := ic.selectExclusiveIrqCoresForNic(nic, expectedIrqCoresCount)
	if err != nil {
		return nil, fmt.Errorf("failed to selectExclusiveIrqCoresForNic for nic %s with exclusive irq core count %d", nic.NicInfo, expectedIrqCoresCount)
	}

	return newIrqCores, nil
}

func (ic *IrqTuningController) calculateExclusiveIrqCoresIncrease(oldIndicatorsStats *IndicatorsStats) {
	// 1. calculate exclusive irq cores for nics whose IrqAffinityPolicy is IrqCoresExclusive and not changed this time
	for _, nic := range ic.Nics {
		if _, ok := ic.IrqAffinityChanges[nic.NicInfo.IfIndex]; ok {
			continue
		}

		if nic.IrqAffinityPolicy != IrqCoresExclusive {
			continue
		}

		newIrqCores, err := ic.calculateNicExclusiveIrqCoresIncrease(nic, oldIndicatorsStats)
		if err != nil {
			err := fmt.Errorf("failed to calculateNicExclusiveIrqCoresIncrease for nic %s, err %s",
				nic.NicInfo, err)
			ic.fallbackToBalanceFairPolicyByError(nic, err)
			continue
		}

		if len(newIrqCores) == 0 {
			continue
		}

		exclusiveIrqCoresChange := buildNicIrqAffinityChange(nic, nic.IrqAffinityPolicy, newIrqCores)
		ic.IrqAffinityChanges[nic.NicInfo.IfIndex] = exclusiveIrqCoresChange

		nic.LastExclusiveIrqCoresInc = &ExclusiveIrqCoresAdjust{
			Number:    len(newIrqCores) - len(nic.NicInfo.getIrqCores()),
			TimeStamp: time.Now(),
		}
	}

	// 2. calculate exclusive irq cores for nics whose IrqAffinityPolicy is changed to IrqCoresExclusive
	// should not range ic.IrqAffinityChanges, because range map cannot keep the consistence of range order.
	for _, nic := range ic.Nics {
		irqAffChange, ok := ic.IrqAffinityChanges[nic.NicInfo.IfIndex]
		if !ok {
			continue
		}

		if irqAffChange.NewIrqAffinityPolicy == IrqCoresExclusive && irqAffChange.OldIrqAffinityPolicy != IrqCoresExclusive {
			irqCores, err := ic.calculateNicIrqCoresWhenSwitchToIrqCoresExclusive(irqAffChange.Nic, oldIndicatorsStats)
			if err != nil {
				err := fmt.Errorf("failed to calculateNicIrqCoresWhenSwitchToIrqCoresExclusive for nic %s, err %s",
					irqAffChange.Nic.NicInfo, err)
				ic.fallbackToBalanceFairPolicyByError(nic, err)
				continue
			}
			irqAffChange.NewIrqCores = irqCores
		}
	}

	return
}

func (nm *NicIrqTuningManager) isPingPongIrqBalance(srcIrqCore int64, dstIrqCore int64, lbConf *config.IrqLoadBalanceConfig) bool {
	if nm.LastIrqLoadBalance == nil {
		return false
	}

	lastIrqLoadBalance := nm.LastIrqLoadBalance

	if time.Since(lastIrqLoadBalance.TimeStamp).Seconds() >= float64(lbConf.PingPongIntervalThresh) {
		return false
	}

	srcCoreMatched := false
	for _, cpu := range lastIrqLoadBalance.SourceCores {
		if srcIrqCore == cpu {
			srcCoreMatched = true
			break
		}
	}
	if !srcCoreMatched {
		return false
	}

	dstCoreMatched := false
	for _, cpu := range lastIrqLoadBalance.DestCores {
		if dstIrqCore == cpu {
			dstCoreMatched = true
			break
		}
	}

	return dstCoreMatched
}

func (ic *IrqTuningController) selectIrqsToBalance(nic *NicIrqTuningManager, srcIrqCore *CPUUtil, destIrqCore *CPUUtil, irqsTunedMax int, oldIndicatorsStats *IndicatorsStats) ([]int, error) {
	srcCoreQueuesPPSInDecOrder := nic.getCoresRxQueuesPPSInDecOrder([]int64{srcIrqCore.CpuID}, oldIndicatorsStats, ic.IndicatorsStats)

	if len(srcCoreQueuesPPSInDecOrder) == 0 {
		return nil, fmt.Errorf("nic %s src core %d has no queues", nic.NicInfo, srcIrqCore.CpuID)
	}

	if len(srcCoreQueuesPPSInDecOrder) == 1 {
		return nil, nil
	}

	srcCoreTotalPPS := uint64(0)
	for _, queuePPS := range srcCoreQueuesPPSInDecOrder {
		srcCoreTotalPPS += queuePPS.PPS
	}

	irqCpuUtilDiff := srcIrqCore.IrqUtil - destIrqCore.IrqUtil
	ppsNeedToShift := srcCoreTotalPPS * uint64(irqCpuUtilDiff/2) / uint64(srcIrqCore.IrqUtil)

	var targetIrqs []int
	var srcCoreIrqsInPPSDecOrder []int

	for _, queuePPS := range srcCoreQueuesPPSInDecOrder {
		irq, ok := nic.NicInfo.Queue2Irq[queuePPS.QueueID]
		if !ok {
			klog.Warningf("failed to find queue %d in nic %s Queue2Irq", queuePPS.QueueID, nic.NicInfo)
			continue
		}

		srcCoreIrqsInPPSDecOrder = append(srcCoreIrqsInPPSDecOrder, irq)

		if queuePPS.PPS <= ppsNeedToShift {
			targetIrqs = append(targetIrqs, irq)
			ppsNeedToShift -= queuePPS.PPS
		}

		if len(targetIrqs) >= irqsTunedMax {
			break
		}
	}

	// if src core has only one irq, then needless to balance src irq core
	if len(srcCoreIrqsInPPSDecOrder) <= 1 {
		return nil, nil
	}

	if len(targetIrqs) == 0 {
		coreIrqs := nic.NicInfo.getIrqCoreAffinitiedIrqs()
		// if dst cpu is new added, then move irq with second large pps to this cpu
		if irqs, ok := coreIrqs[destIrqCore.CpuID]; !ok || len(irqs) == 0 {
			secondLargePPSIrq := srcCoreIrqsInPPSDecOrder[1]
			targetIrqs = append(targetIrqs, secondLargePPSIrq)
		}

		if len(targetIrqs) == 0 {
			return nil, ErrNotFoundProperDestIrqCore
		}
	}

	return targetIrqs, nil
}

func (ic *IrqTuningController) balanceIrqs(nic *NicIrqTuningManager, srcIrqCore *CPUUtil, destIrqCore *CPUUtil, cpuUtilGapThresh int, irqsTunedMax int, oldIndicatorsStats *IndicatorsStats) (map[int]*IrqAffinityTuning, error) {
	coreIrqs := nic.NicInfo.getIrqCoreAffinitiedIrqs()

	srcCoreIrqs, ok := coreIrqs[srcIrqCore.CpuID]
	if !ok {
		return nil, fmt.Errorf("failed to find core %d in nic %s coreIrqs", srcIrqCore.CpuID, nic.NicInfo)
	}

	// if src irq core only has 1 irq, needless to balance
	if len(srcCoreIrqs) == 1 {
		return nil, nil
	}

	if srcIrqCore.IrqUtil < destIrqCore.IrqUtil {
		return nil, fmt.Errorf("source irq core's irq util(%d) less than dest irq core's irq util(%d)", srcIrqCore.IrqUtil, destIrqCore.IrqUtil)
	}

	irqUtilGap := srcIrqCore.IrqUtil - destIrqCore.IrqUtil
	if irqUtilGap < cpuUtilGapThresh {
		return nil, ErrNotFoundProperDestIrqCore
	}

	irqs, err := ic.selectIrqsToBalance(nic, srcIrqCore, destIrqCore, irqsTunedMax, oldIndicatorsStats)
	if err != nil {
		klog.Warningf("failed to selectIrqsToBalance, err %s", err)
		return nil, err
	}

	if len(irqs) == 0 {
		return nil, nil
	}

	irqsAffinityTuning := make(map[int]*IrqAffinityTuning)
	for _, irq := range irqs {
		if err := util.SetIrqAffinity(irq, destIrqCore.CpuID); err != nil {
			klog.Errorf("failed to SetIrqAffinity(%d, %d), err %v", irq, destIrqCore.CpuID, err)
			continue
		}
		nic.NicInfo.Irq2Core[irq] = destIrqCore.CpuID
		irqsAffinityTuning[irq] = &IrqAffinityTuning{
			SourceCore: srcIrqCore.CpuID,
			DestCore:   destIrqCore.CpuID,
		}
		klog.Infof("[irq load balance] tuning irq %d affinity from cpu %d to cpu %d", irq, srcIrqCore.CpuID, destIrqCore.CpuID)
	}

	if err := nic.NicInfo.sync(); err != nil {
		klog.Errorf("failed to sync for nic %s, err %s", nic.NicInfo, err)
	}

	return irqsAffinityTuning, nil
}

// balance irq load for nic whose IrqAffinityPolicy is IrqCoresExclusive
// return value:
// 1st, if need to increase irq cores, (if nic need to balance irq cores, but failed to find dest irq core to balance irq, then need to increase irq cores for balance)
// 2nd, if performed irq balance
func (ic *IrqTuningController) balanceIrqLoadBasedOnIrqUtil(nic *NicIrqTuningManager, oldIndicatorsStats *IndicatorsStats) (bool, bool) {
	lbConf := &ic.conf.IrqLoadBalanceConf
	lastBalance := nic.LastIrqLoadBalance
	if lastBalance != nil && time.Since(lastBalance.TimeStamp).Seconds() < float64(lbConf.SuccessiveTuningInterval) {
		return false, false
	}

	cpuUtils, cpuUtilAvg := calculateCpuUtils(oldIndicatorsStats.CPUStats, ic.IndicatorsStats.CPUStats, nic.NicInfo.getIrqCores())

	if cpuUtilAvg.IrqUtil > ic.conf.IrqCoresAdjustConf.IrqCoresIncConf.Thresholds.IrqCoresAvgCpuUtilThresh {
		return false, false
	}

	// sort irq cores cpu util by irq util in deceasing order
	sortCpuUtilSliceByIrqUtilInDecOrder(cpuUtils)

	var needToBalanceIrqCores []*CPUUtil
	for _, cpuUtil := range cpuUtils {
		if cpuUtil.IrqUtil >= lbConf.Thresholds.IrqCoreCpuUtilThresh {
			needToBalanceIrqCores = append(needToBalanceIrqCores, cpuUtil)
		} else {
			break
		}

	}

	if len(needToBalanceIrqCores) == 0 {
		return false, false
	}

	newLoadBalance := &IrqLoadBalance{
		TimeStamp: time.Now(),
	}

	balancedIrqCoresCount := 0
	hasIrqsBalanced := false
	needToIncIrqCores := false
	dstIrqCoreIndex := len(cpuUtils) - 1
	for i, srcIrqCore := range needToBalanceIrqCores {
		if dstIrqCoreIndex <= i {
			needToIncIrqCores = true
			break
		}

		if nic.isPingPongIrqBalance(srcIrqCore.CpuID, cpuUtils[dstIrqCoreIndex].CpuID, lbConf) {
			nic.TuningRecords.IrqLoadBalancePingPongCount++
			if nic.TuningRecords.IrqLoadBalancePingPongCount >= lbConf.PingPongCountThresh {
				needToIncIrqCores = true
				break
			}
		}

		irqTunings, err := ic.balanceIrqs(nic, srcIrqCore, cpuUtils[dstIrqCoreIndex], lbConf.Thresholds.IrqCoreCpuUtilGapThresh, lbConf.IrqsTunedNumMaxEachTime, oldIndicatorsStats)
		if err != nil {
			if err == ErrNotFoundProperDestIrqCore {
				needToIncIrqCores = true
				// need to reset IrqLoadBalancePingPongCount ???
				break // remainer needless to balance
			} else {
				klog.Errorf("failed to balanceIrqs for nic %s, err %s", nic.NicInfo, err)
				continue
			}
		}

		if irqTunings == nil {
			continue
		}

		hasIrqsBalanced = true
		dstIrqCoreIndex--

		// reset IrqLoadBalancePingPongCount after succeed to balance irq
		nic.TuningRecords.IrqLoadBalancePingPongCount = 0

		for irq, irqTune := range irqTunings {
			newLoadBalance.SourceCores = append(newLoadBalance.SourceCores, irqTune.SourceCore)
			newLoadBalance.DestCores = append(newLoadBalance.DestCores, irqTune.DestCore)
			newLoadBalance.IrqTunings[irq] = irqTune
		}

		balancedIrqCoresCount++
		if balancedIrqCoresCount >= lbConf.IrqCoresTunedNumMaxEachTime {
			break
		}
	}

	if hasIrqsBalanced {
		nic.LastIrqLoadBalance = newLoadBalance
	}

	return needToIncIrqCores, hasIrqsBalanced
}

func (ic *IrqTuningController) balanceIrqLoadBasedOnNetLoad(nic *NicIrqTuningManager, oldIndicatorsStats *IndicatorsStats) (bool, bool) {
	// support later
	return false, false
}

func (ic *IrqTuningController) balanceIrqLoad(nic *NicIrqTuningManager, oldIndicatorsStats *IndicatorsStats) (bool, bool) {
	needToIncIrqCores, hasIrqsBalanced := ic.balanceIrqLoadBasedOnIrqUtil(nic, oldIndicatorsStats)
	if needToIncIrqCores || hasIrqsBalanced {
		return needToIncIrqCores, hasIrqsBalanced
	}

	needToIncIrqCores, hasIrqsBalanced = ic.balanceIrqLoadBasedOnNetLoad(nic, oldIndicatorsStats)
	if needToIncIrqCores || hasIrqsBalanced {
		return needToIncIrqCores, hasIrqsBalanced
	}

	return false, false
}

func (ic *IrqTuningController) balanceIrqsForNicsWithExclusiveIrqCores(oldIndicatorsStats *IndicatorsStats) {
	for _, nic := range ic.Nics {
		if nic.IrqAffinityPolicy != IrqCoresExclusive {
			continue
		}

		if _, ok := ic.IrqAffinityChanges[nic.NicInfo.IfIndex]; ok {
			continue
		}

		var irqAffinityChange *IrqAffinityChange
		needToIncIrqCores, hasIrqsBalanced := ic.balanceIrqLoad(nic, oldIndicatorsStats)
		if needToIncIrqCores {
			newIrqCoresCount := len(nic.NicInfo.getIrqCores()) + 1
			newIrqCores, err := ic.selectExclusiveIrqCoresForNic(nic, newIrqCoresCount)
			if err != nil {
				err := fmt.Errorf("failed to selectExclusiveIrqCoresForNic, err %s", err)
				ic.fallbackToBalanceFairPolicyByError(nic, err)
				continue
			} else {
				irqAffinityChange = buildNicIrqAffinityChange(nic, nic.IrqAffinityPolicy, newIrqCores)
			}
		}
		if hasIrqsBalanced {
			if irqAffinityChange == nil {
				irqAffinityChange = buildNicIrqAffinityChange(nic, nic.IrqAffinityPolicy, nic.NicInfo.getIrqCores())
			}
			irqAffinityChange.IrqsBalanced = true
		}
		if irqAffinityChange != nil {
			ic.IrqAffinityChanges[nic.NicInfo.IfIndex] = irqAffinityChange
		}
	}

	return
}

func (ic *IrqTuningController) calculateNicExclusiveIrqCoresDecrease(nic *NicIrqTuningManager, oldIndicatorsStats *IndicatorsStats) ([]int64, error) {
	decConf := ic.conf.IrqCoresAdjustConf.IrqCoresDecConf

	lastDec := nic.LastExclusiveIrqCoresDec
	if lastDec != nil && time.Since(lastDec.TimeStamp).Seconds() < float64(decConf.SuccessiveDecInterval) {
		klog.Infof("two successive exclusive irq cores decrease interval %d less than configured interval threshold %d",
			int(time.Since(lastDec.TimeStamp).Seconds()), decConf.SuccessiveDecInterval)
		return nil, nil
	}

	lastInc := nic.LastExclusiveIrqCoresInc
	if lastInc != nil && time.Since(lastInc.TimeStamp).Seconds() < float64(decConf.PingPongAdjustInterval) {
		klog.Infof("since last exclusive irq cores increase interval %d less than configured pingpong interval threshold %d",
			int(time.Since(lastInc.TimeStamp).Seconds()), decConf.PingPongAdjustInterval)
		return nil, nil
	}

	lastBalance := nic.LastIrqLoadBalance
	if lastBalance != nil && time.Since(lastBalance.TimeStamp).Seconds() < float64(decConf.SinceLastBalanceInterval) {
		klog.Infof("since last irq balance interval %d less than configured SinceLastBalanceInterval threshold %d",
			int(time.Since(lastBalance.TimeStamp).Seconds()), decConf.SinceLastBalanceInterval)
		return nil, nil
	}

	_, cpuUtilAvg := calculateCpuUtils(oldIndicatorsStats.CPUStats, ic.IndicatorsStats.CPUStats, nic.NicInfo.getIrqCores())

	if cpuUtilAvg.IrqUtil > decConf.Thresholds.IrqCoresAvgCpuUtilThresh {
		return nil, nil
	}

	irqCoresCpuUsage := float64(len(nic.NicInfo.getIrqCores())*cpuUtilAvg.IrqUtil) / 100

	expectedIrqCoresCount := ic.calculateExclusiveIrqCores(nic, irqCoresCpuUsage)

	oriIrqCoresCount := len(nic.NicInfo.getIrqCores())
	if expectedIrqCoresCount >= oriIrqCoresCount {
		klog.Warningf("needless to decrease irq cores, new calculated irq cores count is %d, original irq cores count %d",
			expectedIrqCoresCount, oriIrqCoresCount)
		return nil, nil
	}

	if oriIrqCoresCount-expectedIrqCoresCount > decConf.DecCoresMaxEachTime {
		expectedIrqCoresCount = oriIrqCoresCount - decConf.DecCoresMaxEachTime
	}

	newIrqCores, err := ic.selectExclusiveIrqCoresForNic(nic, expectedIrqCoresCount)
	if err != nil {
		return nil, fmt.Errorf("failed to selectExclusiveIrqCoresForNic for nic %s with exclusive irq core count %d", nic.NicInfo, expectedIrqCoresCount)
	}

	return newIrqCores, nil
}

func (ic *IrqTuningController) calculateExclusiveIrqCoresDecrease(oldIndicatorsStats *IndicatorsStats) bool {
	// 1. calculate exclusive irq cores for nics whose IrqAffinityPolicy is IrqCoresExclusive and not changed this time
	hasNicExclusiveIrqCoresDec := false
	for _, nic := range ic.Nics {
		if _, ok := ic.IrqAffinityChanges[nic.NicInfo.IfIndex]; ok {
			continue
		}

		if nic.IrqAffinityPolicy != IrqCoresExclusive {
			continue
		}

		newIrqCores, err := ic.calculateNicExclusiveIrqCoresDecrease(nic, oldIndicatorsStats)
		if err != nil {
			err := fmt.Errorf("failed to calculateNicExclusiveIrqCoresDecrease for nic %s, err %s",
				nic.NicInfo, err)
			ic.fallbackToBalanceFairPolicyByError(nic, err)
			continue
		}

		// needless to decrease irq cores
		if len(newIrqCores) == 0 {
			continue
		}

		exclusiveIrqCoresChange := buildNicIrqAffinityChange(nic, nic.IrqAffinityPolicy, newIrqCores)
		ic.IrqAffinityChanges[nic.NicInfo.IfIndex] = exclusiveIrqCoresChange

		nic.LastExclusiveIrqCoresDec = &ExclusiveIrqCoresAdjust{
			Number:    len(nic.NicInfo.getIrqCores()) - len(newIrqCores),
			TimeStamp: time.Now(),
		}
		hasNicExclusiveIrqCoresDec = true
		// balance irqs here?
	}

	// 2. calculate exclusive irq cores for nics whose IrqAffinityPolicy is changed from IrqCoresExclusive to another
	// should not range ic.IrqAffinityChanges, because range map cannot keep the consistence of range order.
	for _, nic := range ic.Nics {
		irqAffChange, ok := ic.IrqAffinityChanges[nic.NicInfo.IfIndex]
		if !ok {
			continue
		}

		if irqAffChange.OldIrqAffinityPolicy == IrqCoresExclusive && irqAffChange.NewIrqAffinityPolicy != IrqCoresExclusive {
			// balance irqs here ?
		}
	}

	return hasNicExclusiveIrqCoresDec
}

func (ic *IrqTuningController) reAdjustAllNicsExclusiveIrqCores() {
	for _, nic := range ic.Nics {
		if change, ok := ic.IrqAffinityChanges[nic.NicInfo.IfIndex]; ok {
			if change.NewIrqAffinityPolicy == IrqCoresExclusive {
				newIrqCores, err := ic.selectExclusiveIrqCoresForNic(nic, len(change.NewIrqCores))
				if err != nil {
					err := fmt.Errorf("failed to selectExclusiveIrqCoresForNic for nic %s with expected irq cores count %d, err %s",
						nic.NicInfo, len(change.NewIrqCores), err)
					ic.fallbackToBalanceFairPolicyByError(nic, err)
					continue
				}
				change.NewIrqCores = newIrqCores
			}
		} else {
			if nic.IrqAffinityPolicy == IrqCoresExclusive {
				irqCores := nic.NicInfo.getIrqCores()
				newIrqCores, err := ic.selectExclusiveIrqCoresForNic(nic, len(irqCores))
				if err != nil {
					err := fmt.Errorf("failed to selectExclusiveIrqCoresForNic for nic %s with expected irq cores count %d, err %s",
						nic.NicInfo, len(change.NewIrqCores), err)
					ic.fallbackToBalanceFairPolicyByError(nic, err)
					continue
				}

				if irqCoresEqual(irqCores, newIrqCores) {
					continue
				}

				exclusiveIrqCoresChange := buildNicIrqAffinityChange(nic, nic.IrqAffinityPolicy, newIrqCores)
				ic.IrqAffinityChanges[nic.NicInfo.IfIndex] = exclusiveIrqCoresChange
			}
		}
	}
}

func (ic *IrqTuningController) handleUnqualifiedCoresChangeForExclusiveIrqCores() {
	unqualifiedCoresMap := ic.getUnqualifiedCoresMapForAllNicsExclusiveIrqCores()

	for _, nic := range ic.Nics {
		// if nic in ic.IrqAffinityChanges, this nic's new irq cores selection has exclude
		// unqualified cores.
		if _, ok := ic.IrqAffinityChanges[nic.NicInfo.IfIndex]; ok {
			continue
		}

		if nic.IrqAffinityPolicy != IrqCoresExclusive {
			continue
		}

		irqCores := nic.NicInfo.getIrqCores()
		needToAdjustIrqCores := false
		for _, core := range irqCores {
			if _, ok := unqualifiedCoresMap[core]; ok {
				needToAdjustIrqCores = true
				break
			}
		}

		if !needToAdjustIrqCores {
			continue
		}

		newIrqCores, err := ic.selectExclusiveIrqCoresForNic(nic, len(irqCores))
		if err != nil {
			err := fmt.Errorf("failed to selectExclusiveIrqCoresForNic for nic %s with exclusive irq core count %d", nic.NicInfo, len(irqCores))
			ic.fallbackToBalanceFairPolicyByError(nic, err)
			continue
		}

		if irqCoresEqual(irqCores, newIrqCores) {
			continue
		}

		exclusiveIrqCoresChange := buildNicIrqAffinityChange(nic, nic.IrqAffinityPolicy, newIrqCores)
		ic.IrqAffinityChanges[nic.NicInfo.IfIndex] = exclusiveIrqCoresChange
	}
}

func (ic *IrqTuningController) TuneNicIrqAffinityWithBalanceFairPolicy(nic *NicIrqTuningManager) error {
	if err := ic.tuneNicIrqsAffinityFairly(nic.NicInfo, nic.AssignedSockets); err != nil {
		return err
	}

	if err := ic.balanceNicIrqsFairly(nic.NicInfo, nic.AssignedSockets); err != nil {
		klog.Errorf("failed to balanceNicIrqsFairly for nic %s, err %s", nic.NicInfo, err)
	}

	nic.IrqAffinityPolicy = IrqBalanceFair
	return nil
}

func (ic *IrqTuningController) balanceIrqsToOtherExclusiveIrqCores(nic *NicIrqTuningManager, irqs []int, destCores []int64, oldIndicatorsStats *IndicatorsStats) error {
	if len(irqs) == 0 {
		return nil
	}

	if len(destCores) == 0 {
		return fmt.Errorf("dest cores length is zero")
	}

	srcCoresQueuesPPSInDecOrder := nic.getIrqsCorrespondingRxQueuesPPSInDecOrder(irqs, oldIndicatorsStats, ic.IndicatorsStats)

	if len(srcCoresQueuesPPSInDecOrder) == 0 {
		return nil
	}

	var totalPPS uint64
	for _, queuePPS := range srcCoresQueuesPPSInDecOrder {
		totalPPS += queuePPS.PPS
	}

	if totalPPS == 0 {
		return fmt.Errorf("irqs %+v corresponding queues's total pps is zero", irqs)
	}

	cpuUtils, _ := calculateCpuUtils(oldIndicatorsStats.CPUStats, ic.IndicatorsStats.CPUStats, nic.NicInfo.getIrqCores())

	cpuUtilsBuffer := make(map[int64]int)
	totalCpuUtilsBuffer := 0
	for _, cpuUtil := range cpuUtils {
		found := false
		for _, core := range destCores {
			if cpuUtil.CpuID == core {
				found = true
				break
			}
		}
		if !found {
			continue
		}

		cpuUtilBuffer := ic.conf.IrqCoresExpectedCpuUtil - cpuUtil.IrqUtil
		if cpuUtilBuffer > 0 {
			cpuUtilsBuffer[cpuUtil.CpuID] = cpuUtilBuffer
			totalCpuUtilsBuffer += cpuUtilBuffer
		}
	}

	if totalCpuUtilsBuffer == 0 {
		return fmt.Errorf("sum of the cpu util buffer of the cores other than the decreased cores is 0")
	}

	cpusPPSBuffer := make(map[int64]uint64)
	for cpu, cpuUtilBuffer := range cpuUtilsBuffer {
		ppsBuffer := int(totalPPS) * cpuUtilBuffer / totalCpuUtilsBuffer
		cpusPPSBuffer[cpu] = uint64(ppsBuffer)
	}

	for _, queuePPS := range srcCoresQueuesPPSInDecOrder {
		irq, ok := nic.NicInfo.Queue2Irq[queuePPS.QueueID]
		if !ok {
			klog.Warningf("failed to find queue %d in nic %s Queue2Irq", queuePPS.QueueID, nic.NicInfo)
			continue
		}

		maxPPSBufferCore := int64(-1)
		var maxPSSBuffer uint64
		for cpu, ppsBuffer := range cpusPPSBuffer {
			if maxPPSBufferCore == -1 || maxPSSBuffer < ppsBuffer {
				maxPPSBufferCore = cpu
				maxPSSBuffer = ppsBuffer
			}
		}

		if queuePPS.PPS > maxPSSBuffer*13/10 {
			klog.Warningf("irq %d with pps %d will be affinitied to core %d with pps buffer %d multiply 1.3", irq, queuePPS.PPS, maxPPSBufferCore, maxPSSBuffer)
		}

		if err := util.SetIrqAffinity(irq, maxPPSBufferCore); err != nil {
			klog.Errorf("failed to SetIrqAffinity(%d, %d), err %v", irq, maxPPSBufferCore, err)
			continue
		}

		cpusPPSBuffer[maxPPSBufferCore] = maxPSSBuffer - queuePPS.PPS
	}

	if err := nic.NicInfo.sync(); err != nil {
		klog.Errorf("failed to sync for nic %s, err %s", nic.NicInfo, err)
	}

	return nil
}

func (ic *IrqTuningController) balanceNicsIrqsAwayFromDecreasedCores(oldIndicatorsStats *IndicatorsStats) {
	for _, nic := range ic.Nics {
		irqAffinityChange, ok := ic.IrqAffinityChanges[nic.NicInfo.IfIndex]
		if !ok {
			continue
		}

		if irqAffinityChange.OldIrqAffinityPolicy == IrqCoresExclusive && irqAffinityChange.NewIrqAffinityPolicy != IrqCoresExclusive {
			if err := ic.TuneNicIrqAffinityWithBalanceFairPolicy(nic); err != nil {
				klog.Errorf("failed to TuneNicIrqAffinityWithBalanceFairPolicy for nic %s, err %s", nic.NicInfo, err)
				continue
			}
			continue
		}

		incIrqCores := calculateIncreasedIrqCores(irqAffinityChange.OldIrqCores, irqAffinityChange.NewIrqCores)
		decIrqCores := calculateDecreasedIrqCores(irqAffinityChange.OldIrqCores, irqAffinityChange.NewIrqCores)

		if len(decIrqCores) > 0 {
			decCoresAffinitiedIrqs := nic.NicInfo.filterCoresAffinitiedIrqs(decIrqCores)

			// if has decreased irq cores and no increased irq cores, directly balance irqs in decreased irq cores to other exclusive irq cores
			if len(incIrqCores) == 0 {
				if err := ic.balanceIrqsToOtherExclusiveIrqCores(nic, decCoresAffinitiedIrqs, irqAffinityChange.NewIrqCores, oldIndicatorsStats); err != nil {
					klog.Errorf("failed balanceIrqsToOtherExclusiveIrqCores for nic %s, err %s", nic.NicInfo, err)
				}
			} else {
				// temporarily balance decresed cores affinitied irqs to non-exclusive cores, after succeed to request new exclusive irq
				// cores, then let these irqs affinity new-requested exclusive irq cores.

				qualifiedCoresMap := ic.getSocketsQualifiedCoresMapForBalanceFairPolicy(nic.AssignedSockets)
				if len(qualifiedCoresMap) == 0 {
					klog.Errorf("failed to find qualified cores in sockets %+v for nic %s", nic.AssignedSockets, nic.NicInfo)
					continue
				}

				if err := ic.tuneNicIrqsAffinityQualifiedCores(nic.NicInfo, decCoresAffinitiedIrqs, qualifiedCoresMap); err != nil {
					klog.Errorf("failed to tuneNicIrqsAffinityQualifiedCores for nic %s, err %s", nic.NicInfo, err)
				}
			}
		}
	}
}

func (ic *IrqTuningController) getCurrentTotalExclusiveIrqCores() ([]int64, error) {
	retryCount := 0
retry:
	exclusiveIrqCPUSet, err := ic.IrqStateAdapter.GetExclusiveIRQCPUSet()
	if err != nil {
		if retryCount < 3 {
			klog.Errorf("failed to GetExclusiveIRQCPUSet, err %s", err)
			time.Sleep(time.Millisecond)
			goto retry
		}
		return nil, fmt.Errorf("failed to GetExclusiveIRQCPUSet, err %s", err)
	}

	return exclusiveIrqCPUSet.ToSliceInt64(), nil
}

func (ic *IrqTuningController) calcaulateIncExclusiveIrqCoresSteps(newIrqCores []int64) ([][]int64, error) {
	totalIrqCores, err := ic.getCurrentTotalExclusiveIrqCores()
	if err != nil {
		return nil, err
	}

	stepExpandableCPUMax := ic.IrqStateAdapter.GetStepExpandableCPUsMax()

	var steps [][]int64
	var stepIncIrqCores []int64
	for _, core := range newIrqCores {
		existed := false

		for _, c := range totalIrqCores {
			if core == c {
				existed = true
				break
			}
		}

		if existed {
			continue
		}

		stepIncIrqCores = append(stepIncIrqCores, core)

		if len(stepIncIrqCores) == stepExpandableCPUMax {
			steps = append(steps, stepIncIrqCores)
			stepIncIrqCores = []int64{}
		}
	}

	if len(stepIncIrqCores) > 0 {
		steps = append(steps, stepIncIrqCores)
	}

	return steps, nil
}

func (ic *IrqTuningController) waitContainersCpusetExcludeIrqCores(irqCores []int64) error {
	exclusionCompleted := false
	for i := 0; i < 600; i++ {
		containers, err := ic.IrqStateAdapter.ListContainers()
		if err != nil {
			klog.Errorf("failed to ListContainers, err %s", err)
			time.Sleep(time.Second)
			continue
		}

		hasOverlappedCores := false
		for _, cnt := range containers {
			var cntCpuSet []int64
			for _, cpuset := range cnt.ActualCPUSet {
				cntCpuSet = append(cntCpuSet, cpuset.ToSliceInt64()...)
			}

			overlappedCores := calculateOverlappedIrqCores(cntCpuSet, irqCores)
			if len(overlappedCores) > 0 {
				hasOverlappedCores = true
				break
			}
		}

		if !hasOverlappedCores {
			exclusionCompleted = true
			break
		}

		time.Sleep(time.Second)
	}

	if !exclusionCompleted {
		return fmt.Errorf("failed to exclude irq cores %+v from container's cpuset.cpus", irqCores)
	}

	return nil
}

func (ic *IrqTuningController) balanceNicIrqCoresLoad(nic *NicIrqTuningManager, irqCores []int64, oldIndicatorsStats *IndicatorsStats) error {
	if len(irqCores) <= 1 {
		return nil
	}

	cpuUtils, _ := calculateCpuUtils(oldIndicatorsStats.CPUStats, ic.IndicatorsStats.CPUStats, irqCores)

	// sort irq cores cpu util by irq util in deceasing order
	sortCpuUtilSliceByIrqUtilInDecOrder(cpuUtils)

	for i, srcCPUUtil := range cpuUtils {
		destCPUUtilIndex := len(cpuUtils) - 1 - i
		if i <= destCPUUtilIndex {
			break
		}
		destCPUUtil := cpuUtils[destCPUUtilIndex]

		srcCoreIrqs := nic.NicInfo.filterCoresAffinitiedIrqs([]int64{srcCPUUtil.CpuID})

		irqUtilGap := srcCPUUtil.IrqUtil - destCPUUtil.IrqUtil
		if irqUtilGap < 10 {
			continue
		}

		if _, err := ic.balanceIrqs(nic, srcCPUUtil, destCPUUtil, 10, len(srcCoreIrqs)-1, oldIndicatorsStats); err != nil {
			klog.Errorf("failed to balanceIrqs for nic %s from cpu %d to cpu %d, err", nic.NicInfo, srcCPUUtil.CpuID, destCPUUtil.CpuID)
		}
	}

	return nil
}

func (ic *IrqTuningController) balanceNicIrqsToNewIrqCores(nic *NicIrqTuningManager, newIrqCores []int64, oldIndicatorsStats *IndicatorsStats) error {
	steps, err := ic.calcaulateIncExclusiveIrqCoresSteps(newIrqCores)
	if err != nil {
		return err
	}

	if len(steps) == 0 {
		return nil
	}

	totalIrqCores, err := ic.getCurrentTotalExclusiveIrqCores()
	if err != nil {
		return err
	}

	for _, stepIncIrqCores := range steps {
		var tmpTotalIrqCores []int64
		tmpTotalIrqCores = append(tmpTotalIrqCores, totalIrqCores...)
		tmpTotalIrqCores = append(tmpTotalIrqCores, stepIncIrqCores...)

		if err := ic.IrqStateAdapter.SetExclusiveIRQCPUSet(machine.NewCPUSet(util.ConvertInt64SliceToIntSlice(tmpTotalIrqCores)...)); err != nil {
			return fmt.Errorf("failed to SetExclusiveIRQCPUSet, err %s", err)
		}

		// wait all containers's cpuset.cpus exclude exclusive irq cores
		if err := ic.waitContainersCpusetExcludeIrqCores(tmpTotalIrqCores); err != nil {
			return err
		}

		totalIrqCores = append(totalIrqCores, stepIncIrqCores...)
		nicCurrentIrqCores := calculateOverlappedIrqCores(totalIrqCores, newIrqCores)

		if err := ic.balanceNicIrqCoresLoad(nic, nicCurrentIrqCores, oldIndicatorsStats); err != nil {
			klog.Errorf("failed to balanceNicIrqCoresLoad for nic %s, err %s", nic.NicInfo, err)
			continue
		}

		// update indicators stats for latest 10s seconds
		oldStats, err := ic.updateLatestIndicatorsStats(10)
		if err != nil {
			klog.Errorf("failed to updateIndicatorsStats, err %s", err)
		} else {
			oldIndicatorsStats = oldStats
		}
	}

	// final overall balance in all irq cores
	if err := ic.balanceNicIrqCoresLoad(nic, newIrqCores, oldIndicatorsStats); err != nil {
		klog.Errorf("failed to balanceNicIrqCoresLoad for nic %s, err %s", nic.NicInfo, err)
	}

	// if this nic has decreased exclusive irq cores before, decreased cores affinitied irqs has been tune to other non-exclusive cores with
	// balance-fair policy in balanceNicsIrqsAwayFromDecreasedCores, here we need to find these irqs and balance them back to nic's exclusive irq cores.
	var irqsNotAffinityNewIrqCores []int
	for irq, core := range nic.NicInfo.Irq2Core {
		inNewIrqCores := false
		for _, c := range newIrqCores {
			if core == c {
				inNewIrqCores = true
				break
			}
		}
		if !inNewIrqCores {
			irqsNotAffinityNewIrqCores = append(irqsNotAffinityNewIrqCores, irq)
		}
	}

	if len(irqsNotAffinityNewIrqCores) > 0 {
		// update indicators stats for latest 10s seconds
		oldStats, err := ic.updateLatestIndicatorsStats(10)
		if err != nil {
			klog.Errorf("failed to updateIndicatorsStats, err %s", err)
		} else {
			oldIndicatorsStats = oldStats
		}

		// balance dec irq cores affinitied irqs to already requested irq cores
		if err := ic.balanceIrqsToOtherExclusiveIrqCores(nic, irqsNotAffinityNewIrqCores, newIrqCores, oldIndicatorsStats); err != nil {
			return err
		}
	}

	return nil
}

func (ic *IrqTuningController) tuneNicIrqAffinityPolicyToIrqCoresExclusive(nic *NicIrqTuningManager, newIrqCores []int64, oldIndicatorsStats *IndicatorsStats) error {
	steps, err := ic.calcaulateIncExclusiveIrqCoresSteps(newIrqCores)
	if err != nil {
		return err
	}

	if len(steps) == 0 {
		klog.Errorf("nic %s new irq cores length is zero", nic.NicInfo)
		return nil
	}

	totalIrqCores, err := ic.getCurrentTotalExclusiveIrqCores()
	if err != nil {
		return err
	}

	timeDiff := ic.IndicatorsStats.UpdateTime.Sub(oldIndicatorsStats.UpdateTime).Seconds()
	rxQueuesPPS := calculateQueuePPS(oldIndicatorsStats.NicStats[nic.NicInfo.IfIndex], ic.NicStats[nic.NicInfo.IfIndex], timeDiff)
	// sort queue pps in deceasing order
	sortQueuePPSSliceInDecOrder(rxQueuesPPS)

	totalPPS := uint64(0)
	for _, queuePPS := range rxQueuesPPS {
		totalPPS += queuePPS.PPS
	}
	ppsPerCore := totalPPS / uint64(len(newIrqCores))

	balancedIrqsMap := make(map[int]interface{})
	for i, stepIncIrqCores := range steps {
		var tmpTotalIrqCores []int64
		tmpTotalIrqCores = append(tmpTotalIrqCores, totalIrqCores...)
		tmpTotalIrqCores = append(tmpTotalIrqCores, stepIncIrqCores...)

		if err := ic.IrqStateAdapter.SetExclusiveIRQCPUSet(machine.NewCPUSet(util.ConvertInt64SliceToIntSlice(tmpTotalIrqCores)...)); err != nil {
			return fmt.Errorf("failed to SetExclusiveIRQCPUSet, err %s", err)
		}

		// wait all containers's cpuset.cpus exclude exclusive irq cores
		if err := ic.waitContainersCpusetExcludeIrqCores(tmpTotalIrqCores); err != nil {
			return err
		}

		totalIrqCores = append(totalIrqCores, stepIncIrqCores...)

		var stepBalanceIrqs []int
		if i == len(steps)-1 {
			for irq, _ := range nic.NicInfo.Irq2Queue {
				if _, ok := balancedIrqsMap[irq]; !ok {
					stepBalanceIrqs = append(stepBalanceIrqs, irq)
					balancedIrqsMap[irq] = nil
				}
			}
		} else {
			ppsToIncIrqCores := uint64(len(stepIncIrqCores)) * ppsPerCore

			for _, queuePPS := range rxQueuesPPS {
				irq, ok := nic.NicInfo.Queue2Irq[queuePPS.QueueID]
				if !ok {
					klog.Warningf("failed to find queue %d in nic %s Queue2Irq", queuePPS.QueueID, nic.NicInfo)
					continue
				}

				if _, ok := balancedIrqsMap[irq]; ok {
					continue
				}

				if queuePPS.PPS <= ppsToIncIrqCores {
					stepBalanceIrqs = append(stepBalanceIrqs, irq)
					balancedIrqsMap[irq] = nil
					ppsToIncIrqCores -= queuePPS.PPS
				}
			}
		}

		if len(stepBalanceIrqs) == 0 {
			klog.Warningf("stepBalanceIrqs is empty")
			continue
		}

		if err := ic.balanceIrqsToOtherExclusiveIrqCores(nic, stepBalanceIrqs, stepIncIrqCores, oldIndicatorsStats); err != nil {
			klog.Errorf("failed to balanceIrqsToOtherExclusiveIrqCores for nic %s, err %s", nic.NicInfo, err)
			continue
		}
	}

	// final overall balance in all irq cores
	// update indicators stats for latest 10s seconds
	oldStats, err := ic.updateLatestIndicatorsStats(10)
	if err != nil {
		klog.Errorf("failed to updateIndicatorsStats, err %s", err)
	} else {
		oldIndicatorsStats = oldStats
	}

	if err := ic.balanceNicIrqCoresLoad(nic, newIrqCores, oldIndicatorsStats); err != nil {
		klog.Errorf("failed to balanceNicIrqCoresLoad for nic %s, err %s", nic.NicInfo, err)
	}

	nic.IrqAffinityPolicy = IrqCoresExclusive
	return nil
}

func (ic *IrqTuningController) balanceNicsIrqsToNewIrqCores(oldIndicatorsStats *IndicatorsStats) error {
	totalExclusiveIrqCores, err := ic.getCurrentTotalExclusiveIrqCores()
	if err != nil {
		return err
	}

	var oldIrqCores []int64
	var newIrqCores []int64
	for _, nic := range ic.Nics {
		if change, ok := ic.IrqAffinityChanges[nic.NicInfo.IfIndex]; ok {
			if change.NewIrqAffinityPolicy == IrqCoresExclusive {
				newIrqCores = append(newIrqCores, change.NewIrqCores...)
			}

			if change.OldIrqAffinityPolicy == IrqCoresExclusive {
				oldIrqCores = append(oldIrqCores, change.OldIrqCores...)
			}
		} else {
			if nic.IrqAffinityPolicy == IrqCoresExclusive {
				irqCores := nic.NicInfo.getIrqCores()
				newIrqCores = append(newIrqCores, irqCores...)
				oldIrqCores = append(oldIrqCores, irqCores...)
			}
		}
	}

	if !irqCoresEqual(oldIrqCores, totalExclusiveIrqCores) {
		klog.Errorf("old irq cores %+v not equal to irq cores %+v get by GetExclusiveIRQCPUSet", oldIrqCores, totalExclusiveIrqCores)
	}

	if irqCoresEqual(newIrqCores, oldIrqCores) && irqCoresEqual(oldIrqCores, totalExclusiveIrqCores) {
		return nil
	}

	// calculate decreased irq cores based on final total irq cores and current total irq cores, and request qrm to decrease
	needToDecreasedIrqCores := calculateIrqCoresDiff(totalExclusiveIrqCores, newIrqCores)
	if len(needToDecreasedIrqCores) > 0 {
		totalExclusiveIrqCores = calculateIrqCoresDiff(totalExclusiveIrqCores, needToDecreasedIrqCores)
		if err := ic.IrqStateAdapter.SetExclusiveIRQCPUSet(machine.NewCPUSet(util.ConvertInt64SliceToIntSlice(totalExclusiveIrqCores)...)); err != nil {
			klog.Errorf("failed to decrease irq cores, err %s", err)
		}
	}

	// balance irqs of nic whose irq affinity policy not changed to new irq cores
	// alloc new increase irq cores in mulitple step with step limit, wait succeed to allocate,
	// and then balance irqs, wait 30s, collect indicators stats.
	for _, nic := range ic.Nics {
		change, ok := ic.IrqAffinityChanges[nic.NicInfo.IfIndex]
		if !ok {
			continue
		}

		if change.OldIrqAffinityPolicy != IrqCoresExclusive || change.NewIrqAffinityPolicy != IrqCoresExclusive {
			continue
		}

		if err := ic.balanceNicIrqsToNewIrqCores(nic, change.NewIrqCores, oldIndicatorsStats); err != nil {
			err := fmt.Errorf("failed to balanceNicIrqsToNewIrqCores for nic %s, err %s", nic.NicInfo, err)
			ic.fallbackToBalanceFairPolicyByError(nic, err)
		}
	}

	// balance irqs of nic whose irq affinity policy changed to IrqCoresExclusive
	// alloc new increase irq cores in mulitple step with step limit, wait succeed to allocate,
	// and then balance irqs, wait 30s, collect indicators stats.
	// N.B., when qrm restarted, nic original irq affinity policy may be IrqCoresExclusive, but we dont know,
	// we need to set original exclusive irq cores in one step.
	for _, nic := range ic.Nics {
		change, ok := ic.IrqAffinityChanges[nic.NicInfo.IfIndex]
		if !ok {
			continue
		}

		if change.OldIrqAffinityPolicy != IrqCoresExclusive && change.NewIrqAffinityPolicy == IrqCoresExclusive {
			if err := ic.tuneNicIrqAffinityPolicyToIrqCoresExclusive(nic, change.NewIrqCores, oldIndicatorsStats); err != nil {
				err := fmt.Errorf("failed to tuneNicIrqAffinityPolicyToIrqCoresExclusive for nic %s, err %s", nic.NicInfo, err)
				ic.fallbackToBalanceFairPolicyByError(nic, err)
			}
		}
	}

	return nil
}

func (ic *IrqTuningController) setNicQueuesRPS(nic *NicInfo, queues []int, destCores []int64, oldRPSConf map[int]string) error {
	newQueueRPSConf, err := util.ConvertIntSliceToBitmapString(destCores)
	if err != nil {
		return fmt.Errorf("failed to ConvertIntSliceToBitmapString(%+v), err %s", destCores, err)
	}

	for _, queue := range queues {
		oldQueueRPSConf, ok := oldRPSConf[queue]
		if !ok {
			klog.Warningf("failed to find queue %d in nic %s rps conf", queue, nic)
		}

		if ok {
			if util.ComparesHexBitmapStrings(oldQueueRPSConf, newQueueRPSConf) {
				continue
			}
		}

		if err := util.SetNicRxQueueRPS(nic.NicBasicInfo, queue, destCores); err != nil {
			klog.Errorf("failed to SetNicRxQueueRPS for nic %s queue %d, err %v", nic, queue, err)
			continue
		}
	}

	return nil
}

func (ic *IrqTuningController) setRPSInNumaForNic(nic *NicIrqTuningManager, assingedSockets []int) error {
	oldRPSConf, err := util.GetNicRxQueuesRpsConf(nic.NicInfo.NicBasicInfo)
	if err != nil {
		return err
	}

	for _, socket := range assingedSockets {
		for _, numa := range ic.CPUInfo.Sockets[socket].NumaIDs {
			numaCoresList := ic.CPUInfo.GetNodeCPUList(numa)
			numaAffinitiedQueues := nic.NicInfo.filterCoresAffinitiedQueues(numaCoresList)
			if len(numaAffinitiedQueues) == 0 {
				continue
			}

			qualifiedCoresMap := ic.getNumaQualifiedCoresMapForBalanceFairPolicy(numa)
			if len(qualifiedCoresMap) == 0 {
				klog.Errorf("found zero qualified core in numa %d for nic %s rps balance", numa, nic.NicInfo)
				continue
			}

			numaIrqCores := nic.NicInfo.filterIrqCores(numaCoresList)
			// if number of non-irq-cores is more than twice number of irq-cores, then rps dest cpus exclude irq cores
			if len(qualifiedCoresMap) >= 3*len(numaIrqCores) {
				for _, core := range numaIrqCores {
					if _, ok := qualifiedCoresMap[core]; ok {
						delete(qualifiedCoresMap, core)
					}
				}
			}

			var destCores []int64
			for core, _ := range qualifiedCoresMap {
				destCores = append(destCores, core)
			}

			if err := ic.setNicQueuesRPS(nic.NicInfo, numaAffinitiedQueues, destCores, oldRPSConf); err != nil {
				klog.Errorf("failed to setNicQueuesRPS, err %s", err)
				continue
			}
		}
	}

	return nil
}

func (ic *IrqTuningController) setRPSInCCDForNic(nic *NicIrqTuningManager, assingedSockets []int) error {
	oldNicRPSConf, err := util.GetNicRxQueuesRpsConf(nic.NicInfo.NicBasicInfo)
	if err != nil {
		return err
	}

	for _, socket := range assingedSockets {
		for numaID, amdNuma := range ic.CPUInfo.Sockets[socket].AMDNumas {
			for _, ccd := range amdNuma.CCDs {
				ccdCoresList := util.GetLLCDomainCPUList(ccd)
				ccdAffinitiedQueues := nic.NicInfo.filterCoresAffinitiedQueues(ccdCoresList)
				if len(ccdAffinitiedQueues) == 0 {
					continue
				}

				qualifiedCoresMap := ic.getCCDQualifiedCoresMapForBalanceFairPolicy(ccd)
				if len(qualifiedCoresMap) == 0 {
					klog.Errorf("found zero qualified core in numa %d ccd for nic %s rps balance", numaID, nic.NicInfo)
					continue
				}

				ccdIrqCores := nic.NicInfo.filterIrqCores(ccdCoresList)
				// if number of non-irq-cores is more than twice number of irq-cores, then rps dest cpus exclude irq cores
				if len(qualifiedCoresMap) >= 3*len(ccdIrqCores) {
					for _, core := range ccdIrqCores {
						if _, ok := qualifiedCoresMap[core]; ok {
							delete(qualifiedCoresMap, core)
						}
					}
				}

				var destCores []int64
				for core, _ := range qualifiedCoresMap {
					destCores = append(destCores, core)
				}

				if err := ic.setNicQueuesRPS(nic.NicInfo, ccdAffinitiedQueues, destCores, oldNicRPSConf); err != nil {
					klog.Errorf("failed to setNicQueuesRPS, err %s", err)
					continue
				}
			}
		}
	}

	return nil
}

func (ic *IrqTuningController) setRPSForNic(nic *NicIrqTuningManager) error {
	if ic.CPUInfo.CPUVendor == cpuid.Intel {
		return ic.setRPSInNumaForNic(nic, nic.AssignedSockets)
	} else if ic.CPUInfo.CPUVendor == cpuid.AMD {
		return ic.setRPSInCCDForNic(nic, nic.AssignedSockets)
	} else {
		return fmt.Errorf("unsupport cpu arch: %s", ic.CPUInfo.CPUVendor)
	}
}

func (ic *IrqTuningController) setRPSForNics() error {
	if ic.conf.IrqTuningPolicy != config.IrqTuningBalanceFair {
		return fmt.Errorf("irq tuing policy is %s, only support enable rps for fair-balance policy", ic.conf.IrqTuningPolicy)
	}

	for _, nic := range ic.Nics {
		if err := ic.setRPSForNic(nic); err != nil {
			klog.Errorf("failed to setRPSForNic for nic %s, err %s", nic.NicInfo, err)
		}
	}

	return nil
}

func (ic *IrqTuningController) clearRPSForNic(nic *NicIrqTuningManager) error {
	oldNicRPSConf, err := util.GetNicRxQueuesRpsConf(nic.NicInfo.NicBasicInfo)
	if err != nil {
		return err
	}

	queues := nic.NicInfo.getQueues()

	for _, queue := range queues {
		oldQueueRPSConf, ok := oldNicRPSConf[queue]
		if ok {
			if util.IsZeroBitmap(oldQueueRPSConf) {
				continue
			}
		}

		if err := util.ClearNicRxQueueRPS(nic.NicInfo.NicBasicInfo, queue); err != nil {
			klog.Errorf("failed to ClearNicRxQueueRPS for nic %s, queue: %d, err %s", nic.NicInfo, queue, err)
		}
	}

	return nil
}

func (ic *IrqTuningController) clearRPSForNics() error {
	for _, nic := range ic.Nics {
		if err := ic.clearRPSForNic(nic); err != nil {
			klog.Errorf("failed to clearRPSForNic for nic %s, err %s", nic.NicInfo, err)
		}
	}

	return nil
}

func (ic *IrqTuningController) nicRPSCleared(nic *NicInfo) bool {
	rpsConf, err := util.GetNicRxQueuesRpsConf(nic.NicBasicInfo)
	if err != nil {
		klog.Errorf("failed to GetNicRxQueuesRpsConf for nic %s, err %s", nic, err)
		return false
	}

	queues := nic.getQueues()
	for _, queue := range queues {
		queueRPSConf, ok := rpsConf[queue]
		if !ok {
			klog.Warningf("failed to find queue %d in nic %s rpc conf", queue, nic)
			return false
		}

		if !util.IsZeroBitmap(queueRPSConf) {
			return false
		}
	}

	return true
}

func (ic *IrqTuningController) nicsRPSCleared() bool {
	for _, nic := range ic.Nics {
		if !ic.nicRPSCleared(nic.NicInfo) {
			return false
		}
	}

	return true
}

func (ic *IrqTuningController) adjustKsoftirqdsNice() error {
	ksoftirqdsNice := make(map[int]int)
	for _, pid := range ic.Ksoftirqds {
		nice, err := util.GetProcessNice(pid)
		if err != nil {
			klog.Errorf("failed to GetProcessNice, err %s", err)
			continue
		}
		ksoftirqdsNice[pid] = nice
	}

	if ic.conf.IrqTuningPolicy != config.IrqTuningIrqCoresExclusive || !ic.conf.ReniceIrqCoresKsoftirqd {
		for _, pid := range ic.Ksoftirqds {
			nice, ok := ksoftirqdsNice[pid]
			if !ok || nice == 0 {
				continue
			}

			if err := util.SetProcessNice(pid, 0); err != nil {
				klog.Errorf("failed to SetProcessNice(%d, %d), err %s", pid, ic.conf.IrqCoresKsoftirqdNice, err)
			}
		}

		return nil
	}

	totalIrqCores, err := ic.getCurrentTotalExclusiveIrqCores()
	if err != nil {
		return fmt.Errorf("failed to getCurrentTotalExclusiveIrqCores, err %s", err)
	}

	if len(totalIrqCores) == 0 {
		return nil
	}

	for core, pid := range ic.Ksoftirqds {
		nice, ok := ksoftirqdsNice[pid]
		if !ok {
			continue
		}

		isExclusiveIrqCore := false
		for _, c := range totalIrqCores {
			if core == c {
				isExclusiveIrqCore = true
				break
			}
		}

		if isExclusiveIrqCore {
			if nice != ic.conf.IrqCoresKsoftirqdNice {
				if err := util.SetProcessNice(pid, ic.conf.IrqCoresKsoftirqdNice); err != nil {
					klog.Errorf("failed to SetProcessNice(%d, %d), err %s", pid, ic.conf.IrqCoresKsoftirqdNice)
				}
			}
		} else {
			if nice != 0 {
				if err := util.SetProcessNice(pid, 0); err != nil {
					klog.Errorf("failed to SetProcessNice(%d, %d), err %s", pid, ic.conf.IrqCoresKsoftirqdNice)
				}
			}
		}
	}

	return nil
}

func (ic *IrqTuningController) periodicTuningIrqBalanceFair() {
	klog.Infof("periodic irq tuning for periodicTuningIrqBalanceFair")

	if time.Since(ic.LastNicSyncTime).Seconds() >= float64(ic.NicSyncInterval) {
		if err := ic.syncNics(); err != nil {
			klog.Errorf("failed to syncNics, err %v", err)
			return
		}
	}

	if err := ic.syncContainers(); err != nil {
		klog.Errorf("failed to syncContainers, err %s", err)
	}

	// set each nic's IrqAffinityPolicy to IrqBalanceFair
	for _, nic := range ic.Nics {
		if nic.IrqAffinityPolicy != IrqBalanceFair {
			nic.IrqAffinityPolicy = IrqBalanceFair
		}
	}

	if err := ic.TuneIrqAffinityForAllNicsWithBalanceFairPolicy(); err != nil {
		klog.Errorf("failed to TuneIrqAffinityForAllNicsWithBalanceFairPolicy, err %v", err)
	}

	totalIrqCores, err := ic.getCurrentTotalExclusiveIrqCores()
	if err != nil || len(totalIrqCores) > 0 {
		if err := ic.IrqStateAdapter.SetExclusiveIRQCPUSet(machine.NewCPUSet()); err != nil {
			klog.Errorf("failed to SetExclusiveIRQCPUSet, err %s", err)
		}
	}

	// rps reconcile
	if ic.conf.EnableRPS {
		if err := ic.setRPSForNics(); err != nil {
			klog.Errorf("failed to setRPSForNics, err %s", err)
		}
	} else {
		if err := ic.clearRPSForNics(); err != nil {
			klog.Errorf("failed to clearRPSForNics, err %s", err)
		}
	}

	// restore ksoftirqd default nice
	if err := ic.adjustKsoftirqdsNice(); err != nil {
		klog.Errorf("failed to adjustKsoftirqdsNice, err %s", err)
	}
}

func (ic *IrqTuningController) periodicTuningIrqCoresExclusive() {
	klog.Infof("periodic irq tuning for periodicTuningIrqCoresExclusive")

	defer func() {
		// make sure IrqAffinityChanges was cleared after exit periodicTuningIrqCoresExclusive
		ic.IrqAffinityChanges = make(map[int]*IrqAffinityChange)
	}()

	if time.Since(ic.LastNicSyncTime).Seconds() >= float64(ic.NicSyncInterval) {
		if err := ic.syncNics(); err != nil {
			klog.Errorf("failed to syncNics, err %v", err)
			return
		}
	}

	if !ic.nicsRPSCleared() {
		if err := ic.clearRPSForNics(); err != nil {
			klog.Errorf("failed to makeSureNicsRPSCleared, err %s", err)
			return
		}

		// wait a while to settle down the net-rx softirq usage
		time.Sleep(time.Minute)
	}

	////////////////////////////////////////////////////////
	// [1] make sure two nic's has no overlapped irq cores before collect indicators stats in machine with two sockets.
	////////////////////////////////////////////////////////
	ic.separateNicsOverlappedIrqCores()

	////////////////////////////////////////////
	// [2] update stats, N.B, if ic.IndicatorsStats == nil, need to do first collect, then wait 1min, then do next collect, then calculator indicators.
	///////////////////////////////////////////
	oldStats, err := ic.updateIndicatorsStats()
	if err != nil {
		klog.Errorf("failed to updateIndicatorsStats, err %v", err)
		return
	}

	//////////////////////////////////////////
	//[3] syncContainers for later possible irq cores selection, and specical containers process
	/////////////////////////////////////////

	// after collect stats then syncContainers
	if err := ic.syncContainers(); err != nil {
		klog.Errorf("failed to syncContainers, err %v", err)
		return
	}

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// [4] evaluate each inic in ic.nics, decide which nic needless to exclude irq cores, than perform balance-fair irq affinity
	//
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	ic.adaptIrqAffinityPolicy(oldStats)

	///////////////////////////////////////////////////////////////////////////////////////
	// [5] caculate if need to do irq balance or irq cores adjustment.
	//
	// irq balance is performed in this step if need,
	// and exclusive irq cores decrease is performed in this step too, exclusive irq cores decrease MUST before SetExclusiveIrqCores to qrm,
	// but exlcusive irq cores increase is practically performed after qrm-state manager has moved all containers's cpus away from exclusive irq cores.
	///////////////////////////////////////////////////////////////////////////////////////

	// calculate exclusive irq cores for nics
	// 1) whose IrqAffinityPolicy is changed to IrqCoresExclusive
	// 2) whose IrqAffinityPolicy is IrqCoresExclusive before and unchanged
	// N.B., update new exclusive irq cores in irqAffinityPolicyChangedNics
	ic.calculateExclusiveIrqCoresIncrease(oldStats)

	ic.balanceIrqsForNicsWithExclusiveIrqCores(oldStats)

	// handle exclusive irq cores decrease, include two types of decrease
	// 1. nic's IrqAffinityPolicy switched from IrqCoresExclusive to another one
	// 2. nic's IrqAffinityPolicy is IrqCoresExclusive and not changed, but exclusive irq cores need to decrease
	// N.B.
	// if nic's exclusive irq cores need to decrease, and also balanced irq cores, then give up decrease this time,
	// if nic's last irq balance time is not before 5min, then give up decease this time.
	// if nic's last increase irq cores time is not before 10 min, then give up decrease this time.
	// use decrease exclusive irq cores to update increased irq cores, try to make the final exclusive irqs cores in a small cpu topology.
	hasNicExclusiveIrqCoresDec := ic.calculateExclusiveIrqCoresDecrease(oldStats)

	// if any one nic's exclusive irq cores decreased, all nic's exclusive irq cores need to be adjusted, this is because the exclusive cores
	// decreased by that nic may be allocated to other nics.
	if hasNicExclusiveIrqCoresDec {
		ic.reAdjustAllNicsExclusiveIrqCores()
	}

	// If there are changes in the forbidden cores or katabm cores, the exclusive cores of the NICs must be adjusted accordingly.
	ic.handleUnqualifiedCoresChangeForExclusiveIrqCores()

	// need to balance nics' irqs away from decreased irq cores, because decreased irq cores will be assigend to applications
	// or other nic as exclusive irq cores.
	ic.balanceNicsIrqsAwayFromDecreasedCores(oldStats)

	///////////////////////////////////////////////////////////////////////////////////
	// [8] if exclusive irq cores changed by other nics, re-adjust irq affinity for nic whose irq IrqAffinityPolicy
	// changed from IrqCoresExclusive to other one, and whose assigned sockets has overlap.
	// [9] handle special containers, like SRIOV containers and katabm containers, .etc
	// SRIOV containers process should NOT before ic.Nics process, because if katalyst restart, special containers's irqs's allocated irq cores may have overlap
	// with ic.Nics's irq cores, and ic.Nics's irq cores may truely be exlcuded before restart.
	///////////////////////////////////////////////////////////////////////////////////

	// sriov container's irq affinity tuning must before practically perfrom new irq cores exclusion if exclusiveIrqCoresAdjusted, but should
	// after ic.Nics irq cores's exclusion calculation, because if it is initialize tuning after katalyst restart,
	// some ic.Nics's irq cores may has been excluded before katalyst restart, but in initialze tuning before irq cores exclusion calculation,
	// cannot identify the truly excluded irq cores, and sriov containers's irqs cores allocation cannot exclude i.Nics's truly excluded irq cores.

	// we should tuning irqs affinity of nics(include ic.Nics and SRIOV VFs) with balance-fair policy here, before balance irqs to new allocated
	// exclusive irq cores for other nics with IrqCoresExclusive policy, and after all nics's exclusive irq cores has completed calculation and has balanced
	// decreased irq cores affinitied irqs away from original exclusive irq cores.

	if err := ic.TuneIrqAffinityForAllNicsWithBalanceFairPolicy(); err != nil {
		klog.Errorf("failed to TuneIrqAffinityForAllNicsWithBalanceFairPolicy, err %v", err)
	}

	////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// [6] if adjust exclusive irq cores, then need to notify qrm state-manager,
	// if add some new exclusive irq cores, then need to wait for completion by retry syncContainers and check if all container's cpuset exclude irqcores
	// if nic.IrqAffinityPolicy changed from InitTuning to IrqCoresExclusive, MUST tune all exclusive irq cores and irqs in one time, because we cannot infer nic's
	// old IrqAffinityPolicy before katalyst restart, maybe old IrqAffinityPolicy is IrqCoresExclusive, if we notify all exclusive irq cores to qrm-stage, qrm-stage may
	// move containers's cpuset to part of nic's exclusive irq cores.
	// so if nic.IrqAffinityPolicy changed from InitTuning to IrqCoresExclusive, cannot call SetExclusiveIrqCores multiple times to set this nic's exclusive irq cores.
	//
	// [7] update nic.IrqAffinityPolicy and nic's exclusive irq cores if irq affinity policy changed or exclusive irq cores changed,
	// because need to filter out exclusive irq cores when TuneIrqAffinityForAllNicsWithBalanceFairPolicy later.
	// N.B., this update dose not really set sysfs irq affinity, just update NicIrqTuningManager structure.
	// after later practically update exclusive irq cores failed, need to update truly exclusive irq cores by read from sysfs.

	// merge [6] and [7]
	// because there is exclusive irq cores increase limit in each SetExclusiveIRQCPUSet, so we cannot completely split irq cores increase operation and operation of
	// balance irqs to increased irq cores. We have to split to multiple steps to complete irq cores increase, and each step include increase exclusive cores and
	// then balance irqs to new increased irq cores.
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

	if err := ic.balanceNicsIrqsToNewIrqCores(oldStats); err != nil {
		klog.Errorf("failed to requestExclusiveIrqCores, err %s", err)
		return
	}

	/////////////////////////////////////////////////////////////
	// [11] set or restore ksoftirqd's nice based on new irqcores, only renice exclusive irq cores's ksoftirqd according to config.
	/////////////////////////////////////////////////////////////
	if err := ic.adjustKsoftirqdsNice(); err != nil {
		klog.Errorf("failed to adjustKsoftirqdsNice, err %s", err)
	}

	return
}

func (ic *IrqTuningController) disableIrqTuning() {
	// set each nic's IrqAffinityPolicy to IrqBalanceFair
	for _, nic := range ic.Nics {
		if nic.IrqAffinityPolicy != IrqBalanceFair {
			if err := ic.TuneNicIrqAffinityWithBalanceFairPolicy(nic); err != nil {
				klog.Errorf("failed to TuneNicIrqAffinityWithBalanceFairPolicy for nic %s, err %s", nic.NicInfo, err)
				continue
			}
		}
	}

	totalIrqCores, err := ic.getCurrentTotalExclusiveIrqCores()
	if err != nil || len(totalIrqCores) > 0 {
		if err := ic.IrqStateAdapter.SetExclusiveIRQCPUSet(machine.NewCPUSet()); err != nil {
			klog.Errorf("failed to SetExclusiveIRQCPUSet, err %s", err)
		}
	}
}

func (ic *IrqTuningController) syncDynamicConfig() {
	dynConf := ic.dynamicConfHolder.GetDynamicConfiguration()
	if dynConf != nil {
		ic.conf = config.ConvertDynamicConfigToIrqTuningConfig(dynConf)
	}
}

func (ic *IrqTuningController) periodicTuning() {
	ic.syncDynamicConfig()

	if !ic.conf.EnableIrqTuning {
		ic.disableIrqTuning()
		return
	}

	switch ic.conf.IrqTuningPolicy {
	case config.IrqTuningIrqCoresExclusive:
		fallthrough
	case config.IrqTuningAuto:
		ic.periodicTuningIrqCoresExclusive()
	case config.IrqTuningBalanceFair:
		fallthrough
	default:
		ic.periodicTuningIrqBalanceFair()
	}
}

func (ic *IrqTuningController) Run(stopCh <-chan struct{}) {
	klog.Infof("Irq tuning controller run")

	go wait.Until(ic.periodicTuning, time.Second*time.Duration(ic.conf.Interval), stopCh)
}

func (ic *IrqTuningController) Stop() {
	klog.Infof("Irq tuning controller stop")
}
