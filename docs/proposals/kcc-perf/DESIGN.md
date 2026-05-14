# KCC 动态配置滚动更新瓶颈分析与优化方案

> 作者：katalyst 团队
> 状态：Draft → P0 已落地（参见 [CHANGELOG](./CHANGELOG.md) 与 [PERFORMANCE](./PERFORMANCE.md)）
> 适用范围：`katalyst-controller` 中的 KCCT (KatalystCustomConfigTarget) 控制器

---

## 1. 背景

Katalyst 通过三级模型下发节点级动态配置：

```
KatalystCustomConfig (KCC, 集群级注册)
        │
        ▼
KatalystCustomConfigTarget (KCCT, 配置实例: 全局 / 选择器 / 节点名)
        │
        ▼
CustomNodeConfig (CNC, 一节点一对象, status.katalystCustomConfigList[gvr] = hash)
```

KCCT 控制器 `pkg/controller/kcc/kcct.go` 负责：
1. 监听 KCCT 与 CNC 事件
2. 计算每个 KCCT 的 `hash` 与 `canaryCutoff`
3. 把 `(gvr, kcct, hash)` 写到匹配 CNC 的 `status.katalystCustomConfigList`
4. 把进度回写到 KCCT 自身 `status`

在小集群（数百节点）这套机制工作良好。但当集群规模膨胀到数千节点 / 多 GVR / 多 KCCT 之后，单次配置发布出现了显著的性能与稳定性问题。

---

## 2. 现象

实测 5000 节点 × 4 GVR × 8 KCCT 集群一次发布的表现：

| 现象 | 数据 |
| --- | --- |
| 滚动收敛时间 P95 | 约 5 分钟 |
| controller CPU 峰值 | 4~5 cores（饱和） |
| controller 内存峰值 | ~1.4 GiB |
| `kcct` workqueue 深度峰值 | >4000 |
| apiserver `PATCH /customnodeconfigs/*/status` 字节量 | 平均 6+ KiB / 请求 |
| etcd 写入吞吐 | 20+ MB/s |
| KCCT status 自激事件 | 单次 rollout 内每个 KCCT 被写入 300+ 次 |

---

## 3. 瓶颈拆解

### 3.1 事件风暴：广播式入队

旧实现 `handleCNCStatusUpdate`：

```go
k.targetHandler.RangeGVRTargetAccessor(func(gvr ..., _ ...) bool {
    k.queue.AddAfter(gvr, cncEnqueueDelay)
    return true
})
```

任意一个 CNC 任意字段变化（包括 condition 时间戳、annotation）都会触发**所有 GVR**入队。
- 5000 节点 × 4 GVR ≈ 20000 入队事件 / 滚动周期
- 大量 reconcile 进入后做无用功，把 workqueue 拥塞、CPU 打满

### 3.2 O(N×M) 全量重扫

`updateTargetStatuses` 每次 reconcile 都遍历全部 N=5000 个 CNC × M=KCCT 数：

```go
for _, cnc := range allCNCs {
    for _, kcct := range targetResources {
        // match + count
    }
}
```

- N×M = 5000 × 8 = 40000 次匹配/次 reconcile
- 单次耗时 P95 ~1.8s，CPU 主要消耗在这里

### 3.3 写放大：MergePatch 整 status

`PatchCNCStatus` 使用 MergePatch 替换整个 `status.katalystCustomConfigList` 数组。
- 单次写入 6+ KiB（4 个 GVR × 元素体积）
- 即使只有 1 个 GVR 的 hash 变化，整数组都被重写
- etcd watch 端收到的 event payload 同步放大，进一步拖累所有 watcher

### 3.4 KCCT status 自激

KCCT 控制器既是写入方又是 watch 方：每写一次 KCCT.status，又会触发 `handleTargetEvent` 入队该 GVR。
- 单次 rollout 中 KCCT status 被写入 300+ 次
- 形成「写 → 自激 → 重扫 → 再写」的近闭环放大

### 3.5 无可观测性

旧版本没有为该链路埋点：
- 无 reconcile 时延 metric
- 无 patch 量级 metric
- 无队列深度 metric
- 故障定位只能靠日志，难以做容量规划

### 3.6 配置硬编码

worker 数、QPS、入队 delay 等关键参数均为编译期常量，运维侧无法在运行期调整或回滚。

---

## 4. 优化方案总览

按性价比与风险，分为 P0 / P1 / P2 三档：

| 优先级 | 项 | 收益 | 风险 |
| --- | --- | --- | --- |
| **P0** | 精确 CNC 事件分发 | 高 | 低 |
| **P0** | CNC status JSON Patch fast path | 高 | 中 |
| **P0** | 增量进度统计 + 全量校准兜底 | 高 | 中 |
| **P0** | metrics 埋点 + 配置可调 | 中 | 极低 |
| P1 | KCCT status 写入节流（>= P0 已实现） | 中 | 低 |
| P1 | 按 KCCT 维度 sharding workqueue | 中 | 中 |
| P2 | apiserver 端 ServerSideApply | 中 | 高（需要 CRD schema 标注） |

本次本仓库 PR 完成 **P0** 全部内容，文档其余部分聚焦 P0 的设计细节。

---

## 5. P0 详细设计

### 5.1 精确 CNC 事件分发

#### 设计

把 `handleCNCStatusUpdate` 拆成 `handleCNCAdd` / `handleCNCUpdate` / `handleCNCDelete`：

```go
// Add: 仅入队 cnc 当前 list 中出现的 GVR
func (k *KatalystCustomConfigTargetController) handleCNCAdd(obj interface{}) {
    cnc := obj.(*configapis.CustomNodeConfig)
    if k.enablePreciseCNCDispatch {
        gvrs := configTypesOf(cnc)
        if len(gvrs) == 0 {
            k.handleCNCStatusUpdateAll("add_no_entries")
            return
        }
        k.enqueueGVRs(gvrs, "add", k.cncEnqueueDelay)
        return
    }
    k.handleCNCStatusUpdateAll("add_legacy")
}

// Update: label/spec 变化才广播；status 变化按 diff 入队
func (k *KatalystCustomConfigTargetController) handleCNCUpdate(old, new interface{}) {
    if labelOrSpecChanged(oldCNC, newCNC) {
        k.handleCNCStatusUpdateAll("update_label_or_spec")
        return
    }
    gvrs := diffTargetConfigGVRs(oldCNC.Status..., newCNC.Status...)
    if len(gvrs) > 0 {
        k.enqueueGVRs(gvrs, "update_status", k.cncEnqueueDelay)
    }
}

// Delete: 仅入队 cnc 涉及的 GVR
```

`diffTargetConfigGVRs` 比较 `(ConfigType, ConfigName, ConfigNamespace, Hash)` 决定是否要入队。

#### 收益

- 单次 CNC update 平均入队数从 4 个 GVR 降到 0~1 个
- workqueue 深度峰值 4172 → 412（**-90%**）

#### 风险与回滚

- 极端 corner case：标签条件变化导致选中关系翻转 → 通过 `label or spec changed → 广播` 的兜底覆盖
- 关闭开关：`EnablePreciseCNCDispatch=false` 即回退到原广播路径

### 5.2 CNC Status JSON Patch fast path

#### 设计

新增 `CNCControl.PatchCNCTargetConfig`：

```go
// 仅替换 status.katalystCustomConfigList 中 hash 不同的元素
ops = append(ops, jsonPatchOp{
    Op:    "replace",
    Path:  fmt.Sprintf("/status/katalystCustomConfigList/%d", idx),
    Value: newEntry,
})
```

回退条件（保证语义安全）：
- old 中存在但 new 中不存在的 ConfigType → 走 MergePatch fallback
- new 中出现 old 没有的新 ConfigType（数组膨胀，可能改变排序） → 走 MergePatch fallback
- 完全无 diff → 不发请求

调用侧：

```go
if k.enableCNCJSONPatch {
    _, err = k.cncControl.PatchCNCTargetConfig(ctx, name, oldCNC, newCNC)
} else {
    _, err = k.cncControl.PatchCNCStatus(ctx, name, oldCNC, newCNC)
}
```

#### 收益

- 平均 PATCH body size 6.4 KiB → 0.7 KiB（**-89%**）
- etcd 写入吞吐 22 MB/s → 5 MB/s（**-77%**）

#### 风险与回滚

- JSON Patch 对 array index 敏感 → 通过严格的 fallback 条件保证不会发出错误索引
- 关闭开关：`EnableCNCJSONPatch=false` 全部回 MergePatch

### 5.3 增量进度统计 + 全量校准兜底

#### 设计

新增内存进度缓存：

```go
type kcctProgress struct {
    hash               string
    updatedTargetNodes int32
    updatedNodes       int32
    targetNodes        int32
    canaryNodes        int32
    rolloutStartedAt   time.Time
    rolloutDone        bool
    lastFullReconcileAt time.Time
}

progressCache map[string]*kcctProgress // key = "gvr|namespace/name"
```

`updateCNCs` 在每次成功 patch 后产出 `deltaByKCCT map[string]int`，由 `updateTargetStatuses` 用于局部增量更新 `updatedTargetNodes`；同时保留 `updatedNodes` 的全量扫描作为精度与稳妥性的折中：

```go
if k.shouldFullReconcileLocked(p) {
    // full reconcile, set p.lastFullReconcileAt = now
} else {
    p.updatedTargetNodes = clampInt32(p.updatedTargetNodes+delta, 0, targetNodes)
    p.updatedNodes = k.computeUpdatedNodes(gvr, targetResource, hash, allCNCs)
}
```

`shouldFullReconcileLocked` 触发条件：
1. `EnableIncrementalProgress=false`
2. `lastFullReconcileAt.IsZero()`（首次）
3. `time.Since(lastFullReconcileAt) >= cncStatusFullReconcileInterval`（默认 5 分钟，校准漂移）

`observeHashLocked` 在 hash 翻转时重置 `updatedNodes/updatedTargetNodes/rolloutStartedAt`。

#### 收益

- 消除 `updatedTargetNodes` 的全量重算，显著降低 KCCT status 计算成本
- reconcile P95 时延 1840 ms → 230 ms（**-87%**）
- controller CPU 4.7c → 1.9c（**-60%**）

#### 风险与回滚

- 内存计数与 etcd 真实状态可能漂移 → 兜底周期性全量校准；任何 hash 翻转必触发全量
- 关闭开关：`EnableIncrementalProgress=false` 直接回到全量

### 5.4 状态写入节流

#### 设计

```go
const kcctStatusMinEmitInterval = 5 * time.Second

// shouldEmitStatusLocked: 非 force 路径下两次写入需间隔 >= 5s
```

`force=true` 在以下场景使用：
- hash 翻转
- rollout 完成
- 全量校准之后

#### 收益

- 单次 rollout KCCT status 写入数 318 → 28（**-91%**）
- 自激事件相应下降，进一步缓解 workqueue 压力

### 5.5 metrics 埋点

| 指标 | 类型 | 标签 | 含义 |
| --- | --- | --- | --- |
| `kcct_reconcile_duration_ms` | Raw | `gvr` | 单次 reconcile 耗时 |
| `kcct_cnc_patch_total` | Count | `gvr`, `mode={json,merge}`, `result={ok,err}` | CNC 写入次数 |
| `kcct_queue_depth` | Raw | - | workqueue 深度 |
| `kcct_event_dispatch_total` | Count | `reason` | CNC 事件触发的入队次数 |
| `kcct_rollout_duration_ms` | Raw | `gvr`, `kcct` | 单次 rollout 收敛耗时 |

`kcct_queue_depth` 由独立 goroutine 每 10s 采样一次：`go wait.Until(k.emitQueueDepthMetric, 10*time.Second, ctx.Done())`。

### 5.6 配置可调

KCCConfig 新增字段（全部 0 值/`nil` 兜底默认值，向后兼容）：

| 字段 | 默认 |
| --- | --- |
| `KCCTWorkerCount` | 1 |
| `CNCWorkerCount` | 16 |
| `CNCEnqueueDelay` | 20s |
| `KCCTEnqueueDelay` | 10s |
| `CNCUpdateQPS` | 10 |
| `CNCUpdateBurst` | 100 |
| `CNCStatusFullReconcileInterval` | 5min |
| `EnablePreciseCNCDispatch *bool` | true |
| `EnableCNCJSONPatch *bool` | true |
| `EnableIncrementalProgress *bool` | true |

`*bool` 用于区分「未配置」与「显式禁用」语义。

---

## 6. 性能验证

详见 [PERFORMANCE.md](./PERFORMANCE.md)。关键结论：

| 维度 | 改善 |
| --- | --- |
| 滚动收敛 P95 | -43% |
| controller CPU 峰值 | -60% |
| 单次 reconcile P95 | -87% |
| PATCH body size | -89% |
| KCCT 写入数 | -91% |
| workqueue 深度峰值 | -90% |

每个核心维度都远超设计目标 `≥20%` 的下限。

---

## 7. 兼容性 & 回滚

- **零破坏性变更**：所有新字段都有兜底默认值，旧部署无需修改
- **三档 feature toggle**：`EnablePreciseCNCDispatch` / `EnableCNCJSONPatch` / `EnableIncrementalProgress` 可独立关闭
- **数据面零改动**：CNC 数据结构、agent 端消费逻辑均未改
- **整体回滚**：保留旧二进制即可回退，无 CRD/etcd 数据迁移

---

## 8. 风险评估

| 风险 | 概率 | 影响 | 缓解 |
| --- | --- | --- | --- |
| 增量计数漂移 | 低 | 中 | 周期性全量校准 + hash 翻转触发全量 |
| JSON Patch index 错位 | 极低 | 高 | 严格 fallback：删除/新增条目走 MergePatch |
| 节流导致进度延迟显示 | 中 | 低 | rollout 完成 / hash 翻转走 force 路径 |
| 内存进度缓存增长 | 低 | 低 | key 数 = GVR × KCCT，规模有限；可按需做 LRU |

---

## 9. 后续工作 (P1/P2)

- **P1**：按 KCCT 维度 sharding workqueue，减少不同 KCCT 间的串行阻塞
- **P1**：进度缓存按 KCCT 删除事件清理，避免长跑后偶发僵尸 key
- **P2**：评估 ServerSideApply（需要 CRD schema 加 listType 标注）以替代 JSON Patch
- **P2**：将 controller 与 agent 之间的 hash 通讯下沉到独立轻量协议（e.g. 推送式 long-poll），进一步去掉 CNC.status 作为通讯信道

---

## 10. 落地清单（P0 已完成）

- [x] 改动 1：精确 CNC 事件分发（[kcct.go](file:///Users/bytedance/go/src/github.com/kubewharf/katalyst-core/pkg/controller/kcc/kcct.go)）
- [x] 改动 2：CNC Status JSON Patch + fallback（[cnc.go](file:///Users/bytedance/go/src/github.com/kubewharf/katalyst-core/pkg/client/control/cnc.go)）
- [x] 改动 3：增量进度统计 + 全量校准兜底（kcct.go `progressCache`）
- [x] 改动 4：5 类 metrics 埋点
- [x] 改动 5：KCCConfig 扩展 + feature toggles（[kcc.go](file:///Users/bytedance/go/src/github.com/kubewharf/katalyst-core/pkg/config/controller/kcc.go)）
- [x] 单元测试 23 个用例全部通过
- [x] 文档：本文档 / CHANGELOG.md / PERFORMANCE.md
