package common

type InterruptInfo struct {
	Info       string
	DeviceName string
	Counters   []string
}

// Interrupts models the content of /proc/interrupts. Key is the IRQ number.
type Interrupts map[string]InterruptInfo

// CPUStat shows how much time the cpu spend in various stages.
type CPUStat struct {
	User      float64
	Nice      float64
	System    float64
	Idle      float64
	Iowait    float64
	IRQ       float64
	SoftIRQ   float64
	Steal     float64
	Guest     float64
	GuestNice float64
}

// SoftIRQStat represent the softirq statistics as exported in the procfs stat file.
type SoftIRQStat struct {
	Hi          uint64
	Timer       uint64
	NetTx       uint64
	NetRx       uint64
	Block       uint64
	BlockIoPoll uint64
	Tasklet     uint64
	Sched       uint64
	Hrtimer     uint64
	Rcu         uint64
}

type Stat struct {
	// Boot time in seconds since the Epoch.
	BootTime uint64
	// Summed up cpu statistics.
	CPUTotal CPUStat
	// Per-CPU statistics.
	CPU map[int64]CPUStat
	// Number of times interrupts were handled, which contains numbered and unnumbered IRQs.
	IRQTotal uint64
	// Number of times a numbered IRQ was triggered.
	IRQ []uint64
	// Number of times a context switch happened.
	ContextSwitches uint64
	// Number of times a process was created.
	ProcessCreated uint64
	// Number of processes currently running.
	ProcessesRunning uint64
	// Number of processes currently blocked (waiting for IO).
	ProcessesBlocked uint64
	// Number of times a softirq was scheduled.
	SoftIRQTotal uint64
	// Detailed softirq statistics.
	SoftIRQ SoftIRQStat
}
