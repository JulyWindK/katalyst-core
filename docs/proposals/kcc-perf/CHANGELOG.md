# KCC 滚动更新性能优化 - 代码变更说明

## 概述

本次变更针对大规模集群下 `KatalystCustomConfig`（KCC）→ `KatalystCustomConfigTarget`（KCCT）→ `CustomNodeConfig`（CNC）三级配置下发链路的性能瓶颈，落地了 P0 级别的四项优化。

变更目标：
1. 消除 updatedTargetNodes 的全量重算，显著降低 KCCT status 计算成本
2. 消除每次 CNC 事件触发全量 GVR 入队导致的 reconcile 风暴
3. 降低 CNC `status` 写入幅度（MergePatch 全量 → JSON Patch 增量）
4. 增加可观测性指标，便于运营态监控滚动状态

---

## 1. 变更文件

| 文件 | 变更类型 | 说明 |
| --- | --- | --- |
| `pkg/config/controller/kcc.go` | 增 | 扩展 KCCConfig，新增 7 个性能参数 + 3 个 feature toggle |
| `pkg/client/control/cnc.go` | 改 | 新增 `PatchCNCTargetConfig` 接口与 RFC6902 JSON Patch fast path |
| `pkg/client/control/cnc_test.go` | 新增 | 单元测试覆盖 JSON Patch / 回退路径 / Dummy 实现 |
| `pkg/controller/kcc/kcct.go` | 重构 | 精确事件分发 / 增量进度统计 / metrics 埋点 / 状态写入节流 |
| `pkg/controller/kcc/kcct_test.go` | 增 | 12 个新测试用例，覆盖核心 helper 与状态机 |

---

## 2. 详细改动

### 2.1 KCCConfig 扩展

新增以下 tunables（全部支持「不配置即默认值」语义，保证向后兼容）：

| 字段 | 默认值 | 作用 |
| --- | --- | --- |
| `KCCTWorkerCount` | 1 | KCCT 队列并发度 |
| `CNCWorkerCount` | 16 | 单次 reconcile 内 patch CNC 的并行度 |
| `CNCEnqueueDelay` | 20s | CNC 事件入队 debounce |
| `KCCTEnqueueDelay` | 10s | KCCT 事件入队 debounce |
| `CNCUpdateQPS` | 10 | 每 GVR 写 CNC status 的限速 QPS |
| `CNCUpdateBurst` | 100 | 每 GVR 写 CNC status 的突发上限 |
| `CNCStatusFullReconcileInterval` | 5min | 全量 status 重扫的兜底周期 |

新增 feature toggles（`*bool` 类型，区分「未设置」与「显式禁用」，缺省为启用）：

| 字段 | 缺省 | 含义 |
| --- | --- | --- |
| `EnablePreciseCNCDispatch` | true | 关闭则走广播分发路径 |
| `EnableCNCJSONPatch` | true | 关闭则全部回退 MergePatch 整 status |
| `EnableIncrementalProgress` | true | 关闭则禁用进度缓存，每次 reconcile 全量重扫 |

### 2.2 改动 1：精确 CNC 事件分发

旧实现 `handleCNCStatusUpdate` 在任意 CNC 事件上对所有已注册 GVR 入队，导致 KCCT 即使没有相关变化也会被无谓重扫。

新实现细分三条路径：
- `handleCNCAdd`：基于 CNC 当前 `status.katalystCustomConfigList` 提取 GVR 集合，仅入队这些 GVR；空列表才回退到广播
- `handleCNCUpdate`：
  - 若 `Labels` 或 `Spec` 变化（可能影响选中关系）→ 广播
  - 否则按 `oldList` 与 `newList` 的 `(ConfigType, ConfigName, ConfigNamespace, Hash)` 差集入队
- `handleCNCDelete`：仅入队该 CNC 的 GVR 集合

并新增 `enqueueGVRs`、`configTypesOf`、`diffTargetConfigGVRs` 辅助函数，写入 `kcct_event_dispatch_total{reason}` 指标。

### 2.3 改动 2：CNC Status JSON Patch fast path

`PatchCNCTargetConfig` 构造 RFC6902 JSON Patch，只 `replace` 变更条目对应的 array index：
- 仅命中 hash 变化时：1 个 op，path = `/status/katalystCustomConfigList/<idx>`
- 出现新条目（数组膨胀，可能改变排序）→ 自动回退 MergePatch
- 出现删除条目 → 自动回退 MergePatch
- 完全无差异 → 不发请求

apiserver 端写入字节量从 `O(len(katalystCustomConfigList))` 下降到 `O(1)`，进入 etcd 后产生的 watch event payload 同步缩小。

### 2.4 改动 3：增量进度 + 全量校准兜底

新增 `progressCache map[string]*kcctProgress`，按 `(GVR, KCCT)` 维度记忆：
- `hash`：上一次发布的配置 hash
- `updatedTargetNodes` / `updatedNodes` / `targetNodes` / `canaryNodes`
- `rolloutStartedAt` / `rolloutDone`
- `lastFullReconcileAt`

`updateTargetStatuses` 现在接收 `deltaByKCCT map[string]int`（来自 `updateCNCs` 增量回写）：
- `shouldFullReconcileLocked` 决定是否执行全量校准
- 增量路径：`updatedTargetNodes` 基于 `deltaByKCCT` 做局部增量维护，显著降低 KCCT status 计算成本
- `updatedNodes` 仍保留对 `allCNCs` 的全量扫描，作为精度与稳妥性的折中
- hash 变化时 `observeHashLocked` 重置进度并埋点 `kcct_rollout_duration_ms`
- `shouldEmitStatusLocked` 节流（默认 5s）以避免 KCCT 自激事件循环

### 2.5 改动 4：Metrics 埋点

| 指标 | 类型 | 标签 | 含义 |
| --- | --- | --- | --- |
| `kcct_reconcile_duration_ms` | Raw | `gvr` | 单次 reconcile 耗时 |
| `kcct_cnc_patch_total` | Count | `gvr`, `mode={json,merge}`, `result={ok,err}` | CNC 写入次数（按 patch 模式与结果） |
| `kcct_queue_depth` | Raw | - | workqueue 当前深度（10s 周期采样） |
| `kcct_event_dispatch_total` | Count | `reason` | CNC 事件触发的 GVR 入队次数 |
| `kcct_rollout_duration_ms` | Raw | `gvr`, `kcct` | 单次滚动从 hash 翻转到 100% 完成的耗时 |

---

## 3. 兼容性

- 全部新字段在 `KCCConfig` 中以 0 值/`nil` 兜底，旧部署无需任何配置变更
- `cnc.go` 在接口层新增 `PatchCNCTargetConfig`，所有现有实现（`DummyCNCControl`、`RealCNCControl`）已同步实现，无外部破坏性变更
- 数据面（agent 侧 `cnc.Status.KatalystCustomConfigList` 消费逻辑）完全未改动

## 4. 回滚方案

按风险递减排序：

1. **整体回滚**：保留旧二进制即可，无 CRD/数据格式变更
2. **关闭 JSON Patch**：将 `EnableCNCJSONPatch` 显式置为 `false` → 回到 MergePatch 老路径
3. **关闭精确分发**：将 `EnablePreciseCNCDispatch` 置为 `false` → 回到广播全量入队
4. **关闭增量进度**：将 `EnableIncrementalProgress` 置为 `false` → 每次 reconcile 都做全量 O(N×M) 校准

## 5. 单元测试

新增测试用例：

`pkg/client/control/cnc_test.go`
- `TestPrepareJSONPatchForCNCTargetConfig_Replace`
- `TestPrepareJSONPatchForCNCTargetConfig_NoOp`
- `TestPrepareJSONPatchForCNCTargetConfig_NewEntryFallback`
- `TestPrepareJSONPatchForCNCTargetConfig_DeletionFallback`
- `TestPrepareJSONPatchForCNCTargetConfig_NilArgs`
- `TestDummyCNCControl_PatchCNCTargetConfig`

`pkg/controller/kcc/kcct_test.go`
- `TestResolveInt`、`TestResolveDuration`、`TestResolveBool`
- `TestClampInt32`
- `TestProgressKey`
- `TestConfigTypesOf`
- `TestDiffTargetConfigGVRs`（5 个 sub-case）
- `TestObserveHashLocked`
- `TestShouldFullReconcileLocked`
- `TestShouldEmitStatusLocked`
- `TestGetOrInitProgressLocked`

执行结果：
```
ok    github.com/kubewharf/katalyst-core/pkg/controller/kcc      4.519s
ok    github.com/kubewharf/katalyst-core/pkg/controller/kcc/util 3.764s
ok    github.com/kubewharf/katalyst-core/pkg/client/control      8.536s
```

新增分支覆盖到所有 helper 与关键状态机；JSON Patch 4 条主路径（replace/no-op/new entry fallback/deletion fallback）全部覆盖。
