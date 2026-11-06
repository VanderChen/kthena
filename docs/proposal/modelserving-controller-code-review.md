# ModelServing Controller 代码检视报告

## 0. 文档信息

| 项 | 内容 |
|---|---|
| 检视对象 | `pkg/model-serving-controller/`（controller / datastore / inplace / plugins / podgroupmanager / utils / webhook） |
| 检视基线 | `main` @ `2eff0aee`（2026-11-05，Merge PR #1774） |
| 检视分支 | `review/modelserving-controller-code-review` |
| 检视性质 | 只读检视，本分支只新增本文档，**不包含任何代码改动** |
| 检视背景 | 该模块经过多轮特性叠加（in-place 更新、Role 协同滚动、Headless Service 插件、版本历史、eviction、maxSurge、bin-pack 缩容等），累计 266 个非 merge 提交触达本目录，出现了"同一模块拆成多个独立文件"、"无效代码"、"写法晦涩"等问题 |

### 0.1 检视方法

1. 静态分析：`go vet`、`staticcheck`（全量检查，排除 ST1000/1003/1016/1020/1021/1022）、`deadcode`（入口 `./cmd/kthena-controller-manager`）、`gocyclo`（阈值 >20）。
2. 人工逐行阅读：`controller/` 全部非测试文件、`utils/`、`datastore/`、`podgroupmanager/`、`inplace/`、`webhook/`、`plugins/`。
3. 对关键结论做交叉验证（全仓 grep 引用、与调用方对照、与 `cmd/` 装配对照）。
4. 本次没有运行单测和 e2e，也没有对"可能的 bug"做复现；标注"不确定"的条目是读代码推断。

### 0.2 严重度定义

| 级别 | 含义 |
|---|---|
| **P0** | 可能 panic、死锁、资源泄漏或数据错误，应优先修复 |
| **P1** | 明显的正确性风险或严重影响维护性，需排期处理 |
| **P2** | 一般性缺陷或结构问题，随重构一并处理 |
| **P3** | 风格、命名、注释等轻微问题 |

### 0.3 编号规则

- controller 包：`C-<类别><序号>`，类别为 D=死代码、S=结构、T=生产代码中的测试专用逻辑、X=重复、R=正确性/健壮性、H=难以理解的写法、K=复杂度、N=风格与规范、Z=测试代码。
- 子包沿用各自前缀：`U-`（utils）、`D-`（datastore）、`G-`（podgroupmanager）、`I-`（inplace）、`W-`（webhook）、`P-`（plugins）；其中"难以理解的写法"加 `H` 标记，如 `U-H3`。

---

## 1. 总体结论

### 1.1 一句话结论

功能上已经比较完整，但代码组织已经落后于特性演进：**一个 3523 行的"上帝文件"加上 4 组按特性命名的补丁式文件**，大量 `XxxInRound` / `Xxx` 双层包装只为测试保留，生产代码里夹杂 `ResourceVersion != ""` 之类的"测试对象识别"分支，协同滚动（coordination）的判断散落在主流程各处。建议按本文第 11 章的路线分阶段治理，**先删死代码和修正确性问题，再做纯搬移式文件整理，最后才拆分大函数和抽象策略**。

### 1.2 规模概览

| 包 | 非测试代码 | 测试代码 |
|---|---|---|
| `controller` | 7167 行 / 11 文件 | 16560 行 / 20 文件 |
| `webhook` | 2320 行 / 5 文件 | 4019 行 / 7 文件 |
| `utils` | 1676 行 / 7 文件 | 2266 行 / 6 文件 |
| `plugins` | 1032 行 / 6 文件 | 1072 行 / 4 文件 |
| `podgroupmanager` | 675 行 / 1 文件 | 1941 行 / 1 文件 |
| `datastore` | 610 行 / 2 文件 | 494 行 / 2 文件 |
| `inplace` | 567 行 / 2 文件 | 709 行 / 2 文件 |

- `pkg/model-serving-controller/README.md` 是空文件（0 行），目录没有任何模块说明。
- `go vet` 无告警；`staticcheck` 无 U1000（未使用的非导出标识符）告警，但 `deadcode` 找到 **26 个生产不可达函数**，另有 `CreatePodsByRole`、`GetRunningPodNumByServingGroup` 等 `deadcode` 不报的导出符号只被测试使用（见第 3 章、附录 A.1）。
- 圈复杂度 >20 的非测试函数共 **21 个**，最高为 `updateModelServingStatusInRound`（82），其次是 `resolveRoleRolloutState`（44）、`rolesToDeleteForRoleRollingUpdateInRound`（40）、`scaleUpRoles`（38）。

### 1.3 Top 12（建议优先处理）

| # | 编号 | 级别 | 一句话描述 |
|---|---|---|---|
| 1 | C-R01 | P0 | `manageRoleReplicasPerGroup` 里 `pod.OwnerReferences[0].UID` 在 Pod 没有 ownerReferences 时会 panic |
| 2 | G-01 / G-02 | P0 | `podgroupmanager` 在持锁状态下 `WaitForCacheSync`，且 informer 的 ctx 与 `Run` 无关，存在阻塞 reconcile 和 goroutine 泄漏风险 |
| 3 | C-R02 | P1 | `deleteServingGroup` 的 `defer` 补偿入队只看局部 `err`，`getErr` 路径漏掉重试 |
| 4 | C-R03 | P1 | `createPodsByRoleWithContext` 在已创建 entry Pod 之后才校验 `WorkerTemplate`，且只打日志返回 nil，留下半成品 Role |
| 5 | C-R04 | P1 | `outdatedRolesInRound` 的排序比较器违反严格弱序，并原地修改入参 |
| 6 | C-S01 / C-S02 | P1 | `model_serving_controller.go` 3523 行混合 10+ 类职责；协同滚动特性散落在 4 个文件 + 11 个测试文件 |
| 7 | C-D 系列 + U-12 | P1 | 26 个生产不可达函数（多数是"为测试保留的包装"），应删除或下沉到 `_test.go` |
| 8 | C-T01 | P1 | 13 处 `ResourceVersion != ""`、多处 `c == nil` / `podsInformer == nil` 是为了让单测能跑而写进生产代码 |
| 9 | C-K01 | P1 | `updateModelServingStatusInRound`（约 270 行、复杂度 82）同时做状态计算、store 写入、API 写入、revision GC |
| 10 | U-01 / U-03 | P1 | v1 ControllerRevision 机制只接了一半：生产只写旧格式，v1 读取分支和 `RecordModelServingRevision` 只被测试触发，同时存在两套 "ModelServing at revision" 实现 |
| 11 | D-01 / D-04 | P1 | datastore 的顺序契约（调用方依赖按 ordinal 有序）没有文档，排序不稳定，"找不到"语义不一致 |
| 12 | I-02 | P1 | `OnlyRoleImagesChanged` 用 32 位 hash 判等，碰撞时会把"非镜像变更"误判成可 in-place 更新 |

> 两点说明：
> 1. `LWSController` 通过 `pkg/controller/controller.go:92` 装配，**不是死代码**，没有列入第 3 章。
> 2. `webhook.AllowEviction` 虽然在 `cmd/kthena-controller-manager/main.go:215` 注册（`deadcode` 因此不报），但注册发生在 `--enable-eviction-webhook=false` 分支；而 chart 只在 `evictionWebhook.enabled=true` 时才创建指向 `/validate-eviction` 的 webhook 配置（`validating-webhook.yaml:74-79`），同一开关也控制该启动参数（`deployment.yaml:27`）。所以 chart 部署下这条路由没有调用方，属于"代码可达、效果为零"，见 W-02。

---

## 2. 代码清单与职责

### 2.1 `controller/` 非测试文件

| 文件 | 行数 | 职责 | 备注 |
|---|---|---|---|
| `model_serving_controller.go` | 3523 | 构造与 informer、事件处理、`syncModelServing`、ServingGroup/Role 扩缩容、滚动更新、Pod 事件、状态汇总、Pod 创建、删除、revision 解析、debug 接口、测试包装 | **上帝文件** |
| `role_rolling_update_coordinator.go` | 1089 | 协同滚动：限额、策略、状态解析、依赖闭包、skew 计算 | 与下面 3 个文件属于同一特性 |
| `coordinated_role_observation.go` | 451 | 协同滚动：Role 实时观测、模板比较、预算、surge | 同上 |
| `coordinated_round.go` | 186 | 协同滚动：每轮缓存（context 里携带） | 同上 |
| `role_coordinated_expansion.go` | 150 | 协同滚动：扩容 gang 分组 | 同上 |
| `inplace_update.go` | 689 | in-place 更新决策、patch、完成/超时/回滚 | 另有 `inplace/` 包，职责切分不清 |
| `lws_controller.go` | 408 | LeaderWorkerSet → ModelServing 的另一个独立 controller | 与主 controller 共用包，但无共享状态 |
| `auxiliary_cleanup.go` | 230 | 删除重试、孤儿 Headless Service 清理 | 与 `deleteServingGroup` 强耦合 |
| `pod_deletion_targets.go` | 126 | 删除目标快照（UID 围栏） | 同上 |
| `binpack_scaledown.go` | 190 | 缩容优先级打分 | 命名与职责不符（见 C-N05） |
| `historical_plugin_context.go` | 125 | 历史 revision 的 plugin 上下文 | 与 `utils/controller_revision*.go` 职责交叠 |

### 2.2 子包文件规模

| 包 | 文件（行数） |
|---|---|
| `utils` | `utils.go`(652) `revision_data.go`(317) `controller_revision.go`(289) `controller_revision_lifecycle.go`(210) `coordination_partition.go`(91) `revision_util.go`(71) `coordination_comparison.go`(46)，另有 `test/helper.go` |
| `webhook` | `eviction_handler.go`(1021) `validator.go`(683) `role_coordination_validator.go`(485) `pod_template_validation.go`(79) `plugin_validation.go`(52) |
| `plugins` | `headless_service_plugin.go`(276) `manager.go`(264) `legacy_child.go`(208) `lws_labels_plugin.go`(125) `demo_plugin.go`(96) `types.go`(63) |
| `podgroupmanager` | `manager.go`(675) |
| `datastore` | `store.go`(527) `dump.go`(83) |
| `inplace` | `state.go`(472) `eligibility.go`(95) |

---

## 3. 死代码与"仅测试使用"代码

方法：`deadcode ./cmd/kthena-controller-manager` 得到生产不可达函数，再用全仓 grep 区分"完全无引用"和"仅测试引用"。

### 3.1 controller 包

| 编号 | 符号 | 位置 | 性质 | 建议 |
|---|---|---|---|---|
| C-D01 | `isServingGroupOutdated` | `model_serving_controller.go:2343` | 仅测试 | 删除，测试改用生产入口 |
| C-D02 | `findOutdatedRolesInServingGroups` | `model_serving_controller.go:3382` | 仅测试 | 同上 |
| C-D03 | `hasUpdateableOutdatedRole` | `model_serving_controller.go:3484` | 测试包装（调 `...InRound`） | 删除包装 |
| C-D04 | `outdatedRoles` | `model_serving_controller.go:3505` | 测试包装 | 同上 |
| C-D05 | `rolesToDeleteForRoleRollingUpdate` | `model_serving_controller.go:3509` | 测试包装 | 同上 |
| C-D06 | `manageRollingUpdate` | `model_serving_controller.go:3517` | 测试包装 | 同上 |
| C-D07 | `refreshCoordinatedAvailability` | `coordinated_role_observation.go:449` | 测试包装 | 同上 |
| C-D08 | `coordinatedPodGroupLayout` | `role_coordinated_expansion.go:148` | 测试包装 | 同上 |
| C-D09 | `stableRolePopulation` | `role_rolling_update_coordinator.go:1080` | 测试包装 | 同上 |
| C-D10 | `roleSpecsFromRevision` | `role_rolling_update_coordinator.go:1084` | 测试包装 | 同上 |
| C-D11 | `CreatePodsByRole`（导出） | `model_serving_controller.go:3014` | 导出包装，生产无调用，仅 7 处测试调用 | 取消导出或删除，测试直接调 `createPodsByRole` |

共同模式：每个函数有 `XxxInRound(ctx, …)` 与 `Xxx(…)`（内部 `context.Background()`）两个版本，后者只为测试保留（详见 C-S04）。

### 3.2 子包

| 编号 | 符号 | 位置 | 性质 |
|---|---|---|---|
| U-12 | `utils.CreateHeadlessService`、`CheckPodRevision`、`ContainerRestarted` | `utils.go:237 / 300 / 373` | **全仓零引用**，直接删除 |
| U-13 | `utils.SetConditionWithRolloutState`、`SetCondition`、`ExpectedPodNum` | `utils.go:429 / 414 / 362` | 前者零引用；后两者仅测试使用 |
| U-01 | `RecordModelServingRevision`、`controllerRevisionLess`、`updateControllerRevision`、`copyInt32`、`modelServingCollisionCount` | `controller_revision_lifecycle.go:38/152/162/196/204` | 仅测试，整套 v1 记录机制未接入生产 |
| U-04 | `utils.RevisionDataHash` | `revision_data.go:222` | 仅测试 |
| G-05 | `calculateRequiredRoleNames` | `podgroupmanager/manager.go:531` | 仅测试 |
| I-01 | 包级 `InProgress` / `Completed` / `OnTarget` | `inplace/state.go:135/141/146` | 仅测试和 e2e，生产使用同名方法 |
| D-08 | `GetRunningPodNumByServingGroup`；`runningPods` 除 `DumpCache` 外无生产读者 | `datastore/store.go` | 仅测试 / 疑似死状态 |

---

## 4. controller 包：结构与文件组织

### C-S01 `model_serving_controller.go` 是上帝文件（P1）

3523 行，包含以下职责（行号为大致区间）：

| 区间 | 职责 |
|---|---|
| 65–265 | 常量、`PodGroupManager` 接口、结构体、构造函数 |
| 267–548 | informer 事件处理（ModelServing / Pod / Service / PodGroup） |
| 548–780 | worker、`syncModelServing`、`syncServingGroupReplicas` |
| 782–1480 | ServingGroup 与 Role 的扩缩容（`scaleUpServingGroups`、`scaleDownRoles`、`scaleUpRoles`、`manageRoleReplicasPerGroup`） |
| 1482–1942 | Role 删除、滚动更新（ServingGroup 级与 Role 级） |
| 1944–2190 | Pod 事件处理（ready / error / grace） |
| 2193–2525 | 就绪判断、deletion 处理、owner 判断 |
| 2526–2791 | 状态汇总 `updateModelServingStatusInRound` |
| 2994–3200 | Pod 创建 |
| 3202–3400 | ServingGroup 删除、PodGroup、revision hash 解析 |
| 3400–3523 | debug 接口、测试包装 |

建议拆分（均在同一个 package 内做"纯搬移"，不改逻辑），目标布局见第 11.3 节。

### C-S02 协同滚动特性被切成 4 个源文件 + 11 个测试文件（P1）

- 源文件：`coordinated_role_observation.go`、`coordinated_round.go`、`role_coordinated_expansion.go`、`role_rolling_update_coordinator.go`；**前缀顺序不统一**（`coordinated_role_*` / `role_coordinated_*` / `role_rolling_update_*`），按名字难以发现它们属于同一特性。
- 同一特性的常量也分散：`coordinatedRoleReplicaBaselineAnnotation` 在 coordinator 文件，`surgeAnnotation` 在 observation 文件。
- 相关逻辑还泄漏到 `utils/coordination_comparison.go`、`utils/coordination_partition.go`，以及 `model_serving_controller.go` 中 34 处 `roleCoordination(ms) != nil` 判断。
- 测试：`coordinated_creation_test.go`、`coordinated_expansion_regression_test.go`、`coordinated_inplace_regression_test.go`、`coordinated_partial_creation_test.go`、`coordinated_role_fixture_test.go`、`coordinated_role_regression_test.go`、`coordinated_round_test.go`、`role_coordinated_expansion_test.go`、`role_coordination_boundaries_test.go`、`merge_ready_regression_test.go`、`role_rolling_update_coordinator_test.go`，共 11 个文件。

建议：以 `coordination_*.go` 统一前缀，或抽成独立子包（见 11.3）。

### C-S03 Revision / 历史版本逻辑分散（P1）

同一主题的代码分布在 `controller/historical_plugin_context.go`、`controller/model_serving_controller.go`（`resolveModelServingRevision`、`resolveRoleTemplateHash*`）、`controller/role_rolling_update_coordinator.go`（`persistCoordinatedRoleRevision`、`readCoordinatedRevision`）、`utils/controller_revision.go`、`utils/controller_revision_lifecycle.go`、`utils/revision_data.go`、`utils/revision_util.go`。同时存在两套"取某 revision 下的 ModelServing"实现且语义不同（见 U-03）。

### C-S04 `XxxInRound` 与 `Xxx` 双层包装（P1）

每个核心函数都有"带 ctx 的 InRound 版本"和"`context.Background()` 的无参版本"。后者生产不可达（见 3.1）。`UpdateModelServingStatus`（导出）→ `updateModelServingStatus` → `updateModelServingStatusInRound` 有三层。`InRound` 这个后缀也没有表达"ctx 里携带本轮缓存"的含义，读者必须去看 `coordinated_round.go` 才知道（见 C-H05）。

建议：只保留一个带 `ctx` 的版本，命名去掉 `InRound` 后缀；测试通过 `withCoordinatedRound(ctx, ms)` 自行构造 ctx。

### C-S05 `LWSController` 与主 controller 同包（P2）

`lws_controller.go`（408 行）是一个完全独立的 controller（自己的 informer factory、workqueue、worker），与 `ModelServingController` 没有共享状态，仅因同在 `controller` 包而被混在一起。导出的 `ResourceExists`（通用 discovery 工具）也放在这里。建议移到 `pkg/model-serving-controller/lws/`（或 `pkg/lws-controller/`）。

### C-S06 常量与类型散落（P3）

- `PodDeletionCostAnnotation`、`Priority*` 常量在 `binpack_scaledown.go`；`GroupNameKey` / `RoleIDKey` 在主文件；`groupNameKey = "GroupName"` 又在 `podgroupmanager/manager.go:56` 重复定义，两者必须相等（controller 在 manager 的 informer 上按此 index 查询）。
- 建议建立 `constants.go`，并让 `podgroupmanager` 引用同一个常量（或把 index 名放进 `utils`）。

---

## 5. controller 包：生产代码中的测试专用逻辑

### C-T01 用 `ResourceVersion != ""` 区分"真实对象 / 单测对象"（P1）

在非测试文件中以 `!= ""` / `== ""` 形式出现 13 次（`model_serving_controller.go` 5 处、`coordinated_round.go` 3 处、`coordinated_role_observation.go` 5 处），涉及 `withCoordinatedRound`、`coordinatedLiveRole`、`compareRoleTemplate`、`refreshCoordinatedAvailabilityInRound`、`surgePodsInRound`、`checkRoleReadyInRound`、`createPod`、`retireCoordinatedSurge`、`deleteOutdatedRoles`、`coordinatedInPlaceStarted` 等。语义是"没有 ResourceVersion 说明是单测里手工构造的对象，跳过 API 查询"。

问题：
1. 生产逻辑依赖一个与业务无关的字段，且这个约定没有文档；
2. 单测与生产走不同分支，测试覆盖不到生产路径；
3. 如果 informer 缓存对象的 `ResourceVersion` 被清空（例如某些 fake client 或 `DeepCopy` 后手工清理），行为会静默改变。

建议：测试使用 `fake clientset` + 真正创建对象（或在构造函数里注入 `observer` 接口），删除这些分支。

### C-T02 为单测保留的 nil 防御（P2）

`c == nil`、`c.kubeClientSet == nil`、`c.podsInformer == nil` 出现在 `resolveRoleTemplateHash`、`emitRoleStatusEvent`、`roleSpecsFromRevisionInRound`、`terminatingRoleReplicas`、`createPod`、`stableRolePopulationInRound`、`registerPodGroupHandler`（后者的 `c == nil` 完全没有意义）等处。方法 receiver 永远非 nil，这些检查只为让"没有装配完整的测试 controller"能跑。建议用 `newTestController` harness 装配完整依赖后删除。

### C-T03 生产结构为了测试而暴露（P3）

- `podgroupmanager.Manager` 导出的可变字段 `CrdInformer` / `PodGroupInformer` / `PodGroupLister` 只被测试直接赋值（G-04）。
- `utils/test/helper.go` 的 `CreatePodGroupCRD` 是测试夹具，却放在非 `_test` 包，会被编进生产二进制（U-25）。
- `datastore.ExportedRole` / `ExportedServingGroup` 仅供 debug endpoint 使用（D-09）。

---

## 6. controller 包：重复实现

| 编号 | 级别 | 重复内容 | 位置 | 建议 |
|---|---|---|---|---|
| C-X01 | P2 | ControllerRevision 归属校验写了 3 份：`utils.GetControllerRevision`、`historical_plugin_context.go` 的 `validateControllerRevisionOwner`、`createPodsByRoleWithContext` 内联检查（`model_serving_controller.go:3074-3079`）；其中一处用 `workloadv1alpha1.ModelServingKind.Kind`，`isOwnedByModelServing`（:2399）却写死字面量 `"ModelServing"` | 多处 | 统一为 `utils.IsControlledBy(obj, ms)`，见 U-07 |
| C-X02 | P2 | "对象归属于某 ModelServing"至少 6 种写法：`IsOwnedByModelServingWithUID`、`isOwnedByModelServing`、`podOwnedForDeletion`（`pod_deletion_targets.go:99`）、`stableCapacityFillsOrphanSlot` 内联一整行条件（`auxiliary_cleanup.go:~160`）、`metav1.IsControlledBy` 等 | 多处 | 同上 |
| C-X03 | P3 | tombstone 解包样板代码在 `deleteModelServing` / `deletePod` / `deleteService` / `deletePodGroup` 中重复 4 次 | 事件处理区 | 抽 `objectFromTombstone[T]` 泛型函数 |
| C-X04 | P3 | informer 过滤闭包重复 3 次；`AddFunc: func(obj){ c.addModelServing(obj) }` 之类无意义的闭包包装 | 构造函数 | 直接传方法值 |
| C-X05 | P3 | 构造 label selector 的代码在 `updateModelServingStatusInRound` 内重复（两个分支各一份），在其他处又有若干 `fmt.Sprintf("%s/%s", ns, group)` 拼 index key | `:2543-2552` 等 | 提供 `groupIndexKey(ns, group)`、`roleIndexKey(...)` |
| C-X06 | P3 | `calculateRoleScore` 与 `calculateServingGroupScore` 结构完全一致（取状态优先级 → 取 Pod → 求和删除成本）；`RoleWithScore` 与 `ServingGroupWithScore` 字段完全相同；`Priority*` 的 5 个 ServingGroup 常量中 4 个值都是 0 | `binpack_scaledown.go` | 合并为泛型打分函数，常量只保留"可优先删除/受保护"两个语义 |
| C-X07 | P3 | `forEachMissingOrdinal` 与 `forEachCoordinatedOrdinal` 相似 | 主文件 | 合并 |
| C-X08 | P2 | `scaleUpServingGroups` 先取一次 ControllerRevision，随后 `resolveModelServingRevision` 又取一次 | `model_serving_controller.go:~798-925` | 只取一次并传递 |
| C-X09 | P3 | `getPartition` 是不使用 `c` 的方法，且存在同名语义的 `modelServingPartition` / `rolePartition` 辅助函数 | 主文件 | 改成普通函数 |

---

## 7. controller 包：正确性与健壮性风险

> 以下均为读代码得出的结论。标注"已核实"的表示已对照源码复核触发条件。

### C-R01 `pod.OwnerReferences[0]` 可能越界 panic（P0，已核实）

`model_serving_controller.go:1362-1367`：

```go
if !utils.IsOwnedByModelServingWithUID(pod, ms.UID) {
    klog.Warningf("... (expected UID=%s, got UID=%s), re-enqueuing", ..., ms.UID, pod.OwnerReferences[0].UID)
    ...
}
```

`IsOwnedByModelServingWithUID` 在"没有任何 ownerReference"时同样返回 false，此时 `pod.OwnerReferences[0]` 越界 panic，而 `Run` 的 worker 只有 `utilruntime.HandleCrash`（会让进程重新 panic）。触发条件：存在带相同 Role 索引标签、但无 ownerRef 的 Pod（手工创建、ownerRef 被清理、孤儿化等）。
修复：使用 `metav1.GetControllerOf(pod)` 判空；日志里输出 `controllerOf` 的 UID 或 `<none>`；同时把 `1*time.Second` 换成已有常量 `enqueueAfter`（:67）。

### C-R02 `deleteServingGroup` 的补偿入队有遗漏（P1，已核实）

`model_serving_controller.go:3219-3223` 的 `defer` 只在局部变量 `err != nil` 时 `enqueueModelServing`。但 `:3246-3247` 的 `if names, getErr := ...replacements(...); getErr != nil { return getErr }` 不会赋值给 `err`，所以 `replacements` 的 API 错误路径**不会触发补偿入队**。`deleteServingGroup` 还会被 Pod 事件回调调用（`handleErrorPod` 等，错误只记日志），这些路径没有 workqueue 的重试兜底。另外 :3234 / :3243 使用 `%v` 而非 `%w`。
修复：改用具名返回值 `(retErr error)` 并在 defer 里判断 `retErr`；统一用 `%w`。

### C-R03 Worker 模板缺失时留下半成品 Role（P1，已核实）

`model_serving_controller.go:3099-3102`：`createPodsByRoleWithContext` 先创建 entry Pod（:3092，协同模式下还写入 store），然后才检查 `role.WorkerReplicas > 0 && role.WorkerTemplate == nil`，且仅 `klog.Errorf` 后 `return nil`。结果是返回成功，但只有 entry Pod，Role 永远凑不齐。
修复：校验提前到创建任何 Pod 之前，并返回 error；webhook 校验与此处的防御应当对应。

### C-R04 `outdatedRolesInRound` 的排序比较器不满足严格弱序，且修改入参（P1）

`model_serving_controller.go:1887-1942`：

- 比较器 `if roleCoordination(ms) == nil && a.Status != b.Status { if a.Status != RoleRunning { return -1 }; return 1 }`：当 `a`、`b` 都不是 Running 且状态不同（如 Creating vs Deleting）时，`cmp(a,b) = -1` 且 `cmp(b,a) = -1`，违反反对称性，`slices.SortFunc` 结果不确定。
- 比较器行为随 `roleCoordination(ms)` 变化，排序键不统一。
- 函数内 `roleList[i].Status = datastore.RoleDeleting` 修改了调用方传入的切片元素（`roleList` 来自 `GetRoleList` 的拷贝，当前没出问题，但契约不明）。
修复：把状态映射成 rank（Running=1，其余=0），比较 `(rank, -ordinal)`；用局部拷贝或返回新值。

### C-R05 `scaleUpRoles` 中 revision 选择不确定（P1）

`model_serving_controller.go:1090-1273`：在"带 partition 保护、非协同"的恢复路径里，当 `revisionToUse == newRevision` 时会 list 全部 ControllerRevision，并取"第一个与 newRevision 不同的"。`ControllerRevision` 列表没有稳定顺序，当存在多个历史 revision 时选择结果不确定，可能让恢复出的 Role 使用错误的历史模板。
修复：应当以被保护 ServingGroup / 同 Role 已有实例的 `Revision` 为准，而不是在全集里"挑一个"。

### C-R06 `handlePodAfterGraceTime` 的 goroutine + `time.Sleep`（P2）

`model_serving_controller.go:2085-2120`：每个 error Pod 启动一个 goroutine 睡眠 `RestartGracePeriodSeconds`，不响应 ctx、不随 controller 退出而终止（泄漏），内部用 `context.TODO()` 调 API（:2109、:2120、:2134、:2140）；`graceMap` 的清理依赖 goroutine 结束（`defer c.graceMap.Delete`）。建议改为"记录截止时间 + `AddAfter` 重新入队 + 到期后再判断"的 level-triggered 写法，与 `inplace_update.go` 的 deadline 处理保持一致。

### C-R07 `updateModelServingStatusInRound` 忽略 ctx 并吞掉错误（P2）

- 参数 ctx 完全没用，`UpdateStatus` 用 `context.TODO()`（:2572、:2766），revision GC 也用 `context.TODO()`（:2779）。
- `else if ok, err := c.checkServingGroupReady(...); ok && err == nil {...}`（约 :2623）静默丢弃 `err`。
- 在"计算状态"的函数里写 datastore（`UpdateServingGroupStatus`）和 API（`UpdateStatus`、`CleanupOldControllerRevisions`），副作用与计算混在一起。

### C-R08 `getPartition` 错误处理不一致（P2）

`scaleUpServingGroups`、`syncRoleReplicas`、`manageRollingUpdateWithPolicy`、`scaleDownServingGroups` 忽略错误，`rolesToDeleteForRoleRollingUpdateInRound`、状态汇总处理错误；`scaleDownRoles` 在出错时静默把 partition 设为 0（等于放弃保护）。partition 已在 webhook 校验，但 controller 作为最后防线不应静默降级。

### C-R09 nil 解引用风格不一致（P3）

`*role.Replicas` 在 `CreatePodsForServingGroup`、`checkServingGroupReady` 中直接解引用，`*copy.Spec.Replicas` 在状态汇总中直接解引用；而 nil 安全的 `roleReplicas()` / `modelServingReplicas()` 已存在。CRD 有 `default=1` 所以线上通常不为 nil，但应统一使用安全版本（`utils.GetMaxUnavailable` 也有同类问题，见 U-18）。

### C-R10 同步循环里的重复 API 调用（P2）

- `persistCoordinatedRoleRevision`（`role_rolling_update_coordinator.go:317`）在启用协同时每次 sync 都 GET ControllerRevision，必要时 Create/Update，且没有冲突重试。
- `resolveRoleRolloutPolicy` 在一次 reconcile 中被重复计算。
- `ModelServingRevision(ms)` 每次整体 `DeepCopy`，一次 reconcile 约 10 处调用（U-06）。
建议在 round 缓存里 memoize。

### C-R11 其他小问题（P3）

- `resolveRoleRolloutPolicy` 的错误信息打印 `servingGroup.Revision`，但查找 key 是 `baselineRevision`。
- `terminatingRoleReplicas`（`role_rolling_update_coordinator.go:689`）在 `getPodsByIndex` 失败时静默返回空 map，导致"没有终止中的 Role"的误判。
- `manageRollingUpdateWithPolicy`（:1551）在 RoleRollingUpdate 提前返回之后，仍判断 `RolloutStrategy == nil || Type == ServingGroupRollingUpdate`，该条件恒真；`allOutdatedGroups := append(runningOutdatedGroups, notRunningOutdatedGroups...)` 依赖"倒序遍历使 not-running 优先"，并可能与 `runningOutdatedGroups` 共享底层数组；:1642 仍有 `TODO(hzxuzhonghu): reuse calMaxScaleDown`。
- `scaleDownRoles` 在检查 `err` 之前就打印"Setting ... Scaling"。
- `Run` 里 `cache.WaitForCacheSync` 的返回值被忽略；worker goroutine 直接 `go c.worker(ctx)`，没有使用 `wait.UntilWithContext`（panic 后不会重启）。
- `pkg/controller/controller.go:115` 的 goroutine 里 `if err = lwsc.Run(ctx, 1); ...` 赋值给外层变量 `err`，存在数据竞争（装配代码，位于本目录之外，顺带指出）。
- 副作用：`shouldSkipHandling`（:2374）有未使用的 `servingGroupName` 参数，注释仍写"skip handling if pod revision mismatches serving group revision"，实际只剩 owner 校验；各调用点的注释同样过时。

### C-R12 对 datastore 顺序契约的隐式依赖（P1）

`syncRoleReplicas`、`scaleDown*`、状态汇总都假定 datastore 返回的 ServingGroup / Role 列表按 ordinal 升序（"partition 保护前 N 个"）。该契约在接口注释中没有声明，实现用不稳定排序 + `-1` 哨兵（D-01、D-H1）。

---

## 8. 难以理解的写法

本章收集"功能正确但读者很难一眼看懂"的写法，每条给出位置、难懂原因、建议。

### 8.1 controller 包

| 编号 | 位置 | 写法 | 为什么难懂 | 建议 |
|---|---|---|---|---|
| C-H01 | `role_rolling_update_coordinator.go:560-687` `resolveRoleRolloutState` | **10 个参数**，函数内有十几个相似命名的计数器：`stableTargetReady`、`hasTargetWork`、`hasTargetReady`、`hasCommittedReplacement`、`remainingOldToUpdate`、`versionReady`、`outsideEligibleStarted`、`versionOld`、`formalObserved`、`protectedObserved`、`startedCount`、`readyCount`…… | 概念（stable / formal / protected / target / version / eligible / committed）没有统一词汇表，同一个 Role 在循环里被同时归入多个桶；循环内 `role.Status = datastore.RoleCreating`"假装"状态（修改循环变量副本）来表达"in-place 已启动不算 Ready"；最后一行 `tailReady && formalObserved == desired && versionReady+versionOld == desired` 的含义只能靠推导 | 参数收敛为 `roleRolloutInput` 结构体；先在文件头写"概念表"；把"观测→分类→汇总"拆成 3 个函数；用枚举表示 Role 实例的分类而不是多个计数器 |
| C-H02 | `role_rolling_update_coordinator.go:451-512` `roleStablePopulation` | 用 `end + holes + extra` 表示"稀疏稳定集合"，`boundary(partition, offset)` 把"第 offset 个元素"换算回 ordinal | 算法本身需要 30 行注释才能读懂，现有只有 2 行；`contains/countFrom/upperBound/boundary` 四个方法之间没有空行、没有文档，`holes` 与 `extra` 的不变量（互斥、有序）没有说明；`stableRolePopulationInRound`（:514）里 `for ordinal := population.end - 1; population.holes.Len() < retainedOutsideDesired && ordinal >= 0; ordinal--` 的循环条件把"计数"和"遍历"揉在一起 | 补"不变量 + 例子"注释；为 `boundary` 写表驱动测试覆盖 `holes/extra` 交错的场景；考虑用显式 `[]int` 有序 ordinal 列表（O(n) 但 n 很小）替代稀疏表示 |
| C-H03 | `role_rolling_update_coordinator.go:854-977` `calculateRoleRolloutLimits` | 在一个循环里同时计算 `allowTargetStart`、`effectivePartition`、`rolloutEnd`、`remainingDeletions`、`retainOldReplica` 以及 3 个 blocker；"terminal rounding step" 例外分支 | 同一个变量 `effectivePartition` 先是"计数边界"，最后被改写成"ordinal 边界"（:960-964），含义在函数中途变化；`retentionIsBinding := limits.effectivePartition == 0` 先判断再 `max(...,1)`，顺序一错语义就变；blocker 的优先级靠最后 `for _, blocker := range []{dependency, retention, skew}` 的字面顺序隐含 | 拆成 `dependencyGate` / `skewGate` / `retentionGate` 三个返回 `limits` 修饰的纯函数，最后一次性把"计数边界"转换为"ordinal 边界"；把 blocker 优先级写成注释或常量表 |
| C-H04 | `role_rolling_update_coordinator.go:994-1013` `allowedStartedReplicas`、`:979-992` `slowestReadyProgress` | `bits.Mul64` / `bits.Div64` 做 128 位向上取整；`slowestReadyProgress` 用交叉相乘比较分数 | 数学意图（向上取整的 `ceil(total*(ready/baselineTotal + skew%))`）完全藏在位运算里，只有一行注释 "The product can exceed int64" | 写出公式注释；抽 `ceilMulDiv(a, b, d uint64)` 工具函数并单测 |
| C-H05 | `inplace_update.go:72`、`coordinated_round.go:62`、`model_serving_controller.go:1157` | 用 `context.WithValue` 在函数间传递"本轮缓存"（`inPlaceRoundKey`、`coordinatedRoundKey`）和"是否 surge"布尔标志（`coordinatedSurgeKey{}` → `true`） | 隐式参数：函数签名看不出依赖；`roundOf(ctx)` 在 ctx 没有 round 时返回"一次性"对象，缓存悄悄失效；"仅被单个 goroutine 使用"的约束只在注释里；`context.Value` 里存可变 map 是并发隐患 | 改为显式传递 `*rolloutRound` 参数，或作为结构体字段；surge 标志改为函数参数 |
| C-H06 | 全文 34 处 `roleCoordination(ms) != nil` | 在 `scaleUpRoles`、`manageRoleReplicasPerGroup`、`outdatedRolesInRound`、`rolesToDeleteForRoleRollingUpdateInRound`、`checkRoleReadyInRound`、`createPod`、`handleReadyPod`、`updatePod`、`syncModelServing` 的主流程里穿插分支 | 一个函数里"普通路径"和"协同路径"交织，读者必须在脑中分别过两遍；如 `rolesToDeleteForRoleRollingUpdateInRound` 的 `protected` 集合在两种模式下用**不同规则**构造（协同：`ordinal < partition`；普通：列表前 N 个）并只用一句"Preserve the existing ... semantics"带过 | 引入 `rolloutStrategy` 接口（`ordinaryStrategy` / `coordinatedStrategy`），主流程只调接口；至少把 `coordinated := roleCoordination(ms) != nil` 提到函数顶部一次计算 |
| C-H07 | `model_serving_controller.go:1887-1942` `outdatedRolesInRound` | `observedHash, ok := "", false` 后在 if/else 里被多分支覆盖；`if !ok { if coordination && status != Running { newUnavailable++ }; warn; continue }` | 同一个函数里 `ok` 在协同模式表示"比较结果已知"，在普通模式表示"hash 能解析"，语义不同；`newUnavailable++` 分散在 5 处 | 先分流成两个小函数 `outdatedByComparison` / `outdatedByHash`，各自返回 `(outdated, unavailable)` |
| C-H08 | `model_serving_controller.go:1759-1885` `rolesToDeleteForRoleRollingUpdateInRound` | `filtered := outdatedRoles[:0]` 原地过滤；三段连续判断 `if len(outdated)==0 { if len(protected)>0 {...} }` → `if len(outdated)>0 { has = true }` → `if len(outdated)==0 { continue }` | 原地过滤依赖"别名不再使用"；三段对 `len(outdatedRoles)` 的判断互相重叠，改一处容易漏另两处 | 合并为 `if len(outdated) == 0 { markProtectedOutdated(); continue }`；过滤用 `slices.DeleteFunc` |
| C-H09 | `model_serving_controller.go:2526-2791` `updateModelServingStatusInRound` | 首个分支条件 `err != nil \|\| (len(groups)==0 && roleCoordination(...) != nil && replicas == 0)`，内部再判断 `if err == nil \|\| errors.Is(err, ErrServingGroupNotFound) {...} return err`；`needsUpdate` 一长串 `\|\|`；`else if ok, err := c.checkServingGroupReady(...); ok && err == nil {` 在 else-if 条件里调用有副作用（之后写 store）的函数；局部变量 `copy` 遮蔽内建函数；`available = available + 1` | 把"错误兜底"和"零副本特例"混成一个分支；条件里夹带函数调用 + 写操作；同一函数既算 `current/updated/available` 又决定 `Progressing/Available` 条件，读者需要同时记住 20+ 个局部变量 | 拆为 `collectGroupStats` → `computeStatus`（纯函数）→ `applyStatus`（写 API）；变量改名为 `msCopy` |
| C-H10 | `inplace_update.go:~367-452` `reconcileInPlaceUnit` | 多个状态布尔：`inProgress`、`retargeted`、`unitSettled`、`unitCompleted`、`hasUnpatchedOutdatedPod`；`if deadline, remaining := ...; remaining <= 0 && policy != None { return ... } else if remaining > 0 { enqueueAfter }` | `if` 带 init 语句 + `else if` 把"超时处理"和"重新入队"写在一个条件里，`remaining<=0 && policy==None` 这一"什么都不做"的分支靠漏过去表达；`retargeted` 的含义需要读 5 行注释才明白 | 用显式状态枚举 `unitPhase{Idle, Patching, Retargeted, Completed}`，`switch` 分发；超时判断抽成 `deadlineState` |
| C-H11 | `auxiliary_cleanup.go:143-229` `stableCapacityFillsOrphanSlot` | 内部闭包 `stableRoles` 出错返回 0；`return false, nil // No usable membership observation: preserve the slot.` 把错误吞掉当成"不删"；:160 一行 200 字符的 owner 判断条件；`for n := 0; n <= int(desired.WorkerReplicas); n++` 里 `n` 是 Pod 序号（0=entry） | 吞错误是"安全优先"的隐式策略；一行条件复制了 `podOwnedForDeletion` 的逻辑（并多了 Ready/Updating 条件）；返回 `(bool, error)` 却在 `err != nil` 时返回 `(false, nil)` | 把 Pod 的"稳定可用"条件抽成 `isStablePod(pod, ms)`；吞错误的地方写明理由，或者向上返回由调用方决定 |
| C-H12 | `auxiliary_cleanup.go:~63-125` `cleanupOrphanServices` | 在 `needed := false` 后用嵌套 for 计算；`pods.List` 之后又对 store 状态做"二次确认"（`GetRoleStatus != NotFound`、`GetServingGroupStatus != groupStatus`） | 二次确认是在防 TOCTOU，但没有注释说明；`needed` 的含义（"这个孤儿 Service 对应一个仍在期望范围内的槽位"）要读完整个循环才知道 | 抽 `isDesiredSlot(ms, group, role, id)`；二次确认处加注释 |
| C-H13 | `lws_controller.go` `constructModelServing` | 局部变量 `roleReplicas := int32(1)` **遮蔽了同包函数 `roleReplicas(role)`**；末尾有一条悬空注释"Keep the plugin chain stable through size=1 transitions..."，位置在构造完成之后；`UpdateFunc: func(old, new interface{})` 遮蔽内建 `new` | 同名变量/函数遮蔽极易在后续改动中误用；注释与它描述的代码不相邻 | 改名为 `defaultRoleReplicas`；注释挪到 `Plugins:` 字面量旁；参数改名 `oldObj, newObj` |
| C-H14 | `model_serving_controller.go:3099` 等 | `if err, retryAfter := c.podGroupManager.CreateOrUpdatePodGroup(...); err != nil { if retryAfter > 0 { ...; return nil } ...}` | 返回 `(error, time.Duration)`：error 非 nil 但被"吞掉"，`retryAfter` 的"0 表示不重试"是魔法约定 | 见 G-03、G-H7：改为 `RequeueAfterError` 类型化错误 |
| C-H15 | `model_serving_controller.go:~798-925` `scaleUpServingGroups` | `if x, err := f(); err != nil { ... } else { ...长块... }` | 违反 Go 惯例（有 `return` 就不应 `else`），长块被缩进两层 | 提前 return，去掉 else |
| C-H16 | 全包 | `fmt.Errorf("...: %v", err)` 在 `model_serving_controller.go` 中 40 处，`%w` 仅 19 处 | 同一文件内两种风格；用 `%v` 丢失了 `errors.Is` 能力，调用方不得不比较字符串或用哨兵 | 统一 `%w` |
| C-H17 | 全包 | 导入别名不统一：`workloadv1alpha1`（24 个文件）、`workload`（4 个）、`api`（4 个）；`historical_plugin_context.go` 的 import 分组违反惯例；`pod_deletion_targets.go` 把第三方包与本地包混排 | 同一个包有三种叫法，grep/阅读成本高 | 统一用 `workloadv1alpha1`，并用 `goimports -local github.com/volcano-sh/kthena` |

### 8.2 子包

子包的"难以理解的写法"见第 9 章各小节中的 `-H` 条目（`U-H1`…`U-H14`、`D-H1`…`D-H5`、`G-H1`…`G-H8`、`I-H1`…`I-H5`、`W-H*`、`P-H*`）。

---

## 9. 子包问题清单

### 9.1 `utils`

| 编号 | 级别 | 位置 | 问题 | 建议 |
|---|---|---|---|---|
| U-01 | P1 | `controller_revision_lifecycle.go:38-148,152,162,196,204` | `RecordModelServingRevision` 及 4 个辅助函数生产不可达，仅测试使用。生产唯一的 ControllerRevision 写入者是旧路径 `CreateControllerRevision`（调用点 `inplace_update.go:372`、`model_serving_controller.go:875/896/3080`、`role_rolling_update_coordinator.go:330`）。`controller_revision.go:48-51` 的注释却说"新 v1 路径必须使用 `BuildRevisionData` 和 `RecordModelServingRevision`"，具有误导性 | 二选一：删除整套 v1 机制（连同 U-03 的 v1 读取分支和测试），或真正接入写路径并统一；决定之前先修正注释 |
| U-02 | P2 | `controller_revision_lifecycle.go:38-148` | 圈复杂度 30；无上限的 `for {}` 冲突重试；返回的 collision count 注释写"必须由调用方持久化"，但没有调用方；3 次调用 `modelServingCollisionCount`；修订号策略不一致（旧路径写死 `Revision: 1`，v1 路径 `maxRevision+1`） | 若保留则拆为 `findExistingByHash` / `nextRevisionNumber` / `createWithCollisionRetry`，并给循环加上限 |
| U-03 | P1 | `controller_revision.go:130-169`、`revision_data.go:233-282`、`controller/historical_plugin_context.go` | v1 读取分支只有测试夹具能触发（生产从不写 `ControllerRevisionDataVersionAnnotation`）。同时有两套"ModelServing at revision"：`utils.ModelServingAtRevision` 对旧 revision 整体替换 `Spec.Template.Roles`，`controller.modelServingFromControllerRevision` 则保留当前 replicas 与滚动配置 | 只保留一套，由 controller 调用 |
| U-04 | P2 | `revision_data.go:222` | `RevisionDataHash` 仅测试使用；其 FNV + `rand.SafeEncodeString` 流水线与 `revision_util.go` 的 `Revision` 相同，仅输入不同 | 随 U-01 删除，或共用 `hashBytes` |
| U-05 | P2 | `revision_util.go:28-75` | 两套哈希口径并存：`ModelServingRevision`/`CalRoleTemplateHash`（spew + fnv32，只含 roles）与 `BuildRevisionData`（canonical JSON，额外含 `SchedulerName` 和 `Plugins`）。`Revision`、`DeepHashObject` 仅内部使用却导出 | 取消导出，并在文档里写明两套口径各自的用途 |
| U-06 | P2 | `revision_util.go:43,54,60,68` | `removeRoleReplicasForRevision` 与 `removeRoleReplicasForRoleTemplateHash` 逻辑重复；局部变量 `copy` 遮蔽内建；`ModelServingRevision` 每次对整个 ModelServing 做 `DeepCopy`，一次 reconcile 约 10 处调用 | 合并，只拷贝 roles，变量改名 |
| U-07 | P2 | `controller_revision.go:286-289`、`utils.go:282-292` 等 | "属于某 ModelServing"至少 5 种写法（`controlledByModelServing`、`IsOwnedByModelServingWithUID`、`metav1.IsControlledBy`、lifecycle 中仅比 UID、`validateControllerRevisionOwner`/内联实现）。`IsOwnedByModelServingWithUID` 校验 `APIVersion+Kind+UID`（不校验 `Name`/controller 标志），且在谓词里 `klog.Warningf` | 统一成 `owner.go` 的 `IsControlledBy(obj, ms)`，谓词内不打日志 |
| U-08 | P3 | `controller_revision.go:60,78,103,141,150,189` 等 | `%v` 与 `%w` 混用（同包 lifecycle 与 `revision_data.go` 用 `%w`）；错误串首字母大写；`GetControllerRevision` NotFound 返回 `(nil, nil)` 并留有 `//TODO: get it from a informer's store`（生产 8 处实时 GET） | 统一 `%w`；NotFound 返回哨兵或改读 lister |
| U-09 | P2 | `controller_revision.go:174-~258` | `CleanupOldControllerRevisions`：全量 Pod `List` 是**有意为之**（注释写明用来封住 cache 空窗），但仍可加 label selector；NotFound 也计入 `deletedCount`；注释"preserve CurrentRevision and UpdateRevision"已过时（实际还保留被引用与 Pod 持有的 revision）；唯一调用方传入 `context.TODO()` | 抽 `collectProtectedRevisions`；NotFound 不计数；调用方传 ctx |
| U-10 | P3 | `controller_revision.go:52-108` | `CreateControllerRevision` 用 `string(existing.Data.Raw) != string(data)` 比较重新序列化的 JSON（不确定是否会误判） | 改语义比较或比较 hash annotation |
| U-11 | P2 | `controller_revision.go:264-283` | `unfinishedInPlaceRevision` 重新解析 in-place annotation，复制了 `inplace.StateOf` 及其旧格式迁移；原因是依赖环（`inplace` 导入 `utils`） | 把 State schema 与解码抽到叶子包，见 I-06 |
| U-12 | P1 | `utils.go:237,300,373` | `CreateHeadlessService`、`CheckPodRevision`、`ContainerRestarted` 全仓零引用 | 直接删除 |
| U-13 | P2 | `utils.go:362,414,429` | `ExpectedPodNum`（仅 controller 测试）、`SetCondition`（仅 `utils_test.go`）、`SetConditionWithRolloutState`（零引用）。`TestSetCondition` 测的是生产不会走的路径，因其自带的 partition 计算与 controller 的 ordinal 感知谓词不一致 | 删除后两者并把测试迁到唯一的生产入口 |
| U-14 | P1 | `utils.go:391-485` | 三个 `SetCondition*` 变体并存，只有 `SetConditionWithRolloutAndProgressState`（437-485）被生产使用（唯一调用点 `controller/model_serving_controller.go:2649`），6 个参数；`LastTransitionTime` 在 408 与 459 行重复设置；`newCondition` 里 `metav1.ConditionStatus(corev1.ConditionTrue)` 跨类型强转，switch 无 default | 收敛为单一入口，参数用结构体，移入 `condition.go` |
| U-15 | P3 | `utils.go:45-47,487-488` | 常量块注释"condition status"实际是 message；`AllGroupsIsReady` 命名/语法错误；487 行注释语法差。不确定：同状态路径不刷新 `Message`；`Progressing` 与 `UpdateInProgress` 未互斥（疑似沿用 LWS 设计） | 修正命名注释，补测试或文档 |
| U-16 | P1 | `utils.go`（652 行） | 杂物箱，混 8 类职责：命名序号（50-97）、Pod 生成与 env 注入（99-225）、ownerRef（226）、Pod 谓词（295-370）、condition（391-500）、admission HTTP（503-550）、informer index（552-595，引入 volcano `schedulingv1beta1`）、`GetMax*` 计算（597-652） | 按 9.7 的布局拆分 |
| U-17 | P2 | `utils.go:502-550` | `ParseModelServingFromRequest`：`admissionReview.Request == nil` 时 :527 空指针；`io.ReadAll` 无上限；`%v` 不是 `%w`；Content-Type 要求完全等于 `application/json`；注释写成 `ParseAdmissionRequest`；与 `pkg/kthena-router/webhook/utils.go:102-120` 重复 | 移到 webhook 包与 router 共用，加判空和大小上限 |
| U-18 | P2 | `utils.go:597-652` | `GetMaxUnavailable`（599）直接解引用 `*ms.Spec.Replicas`，`GetMaxSurge`（611）有 nil 检查；`GetMaxUnavailableForRole`（625）内联 `roleReplicas` 逻辑；`intstr.FromInt` 已弃用；返回形态 `(int,bool,error)` 与 `(int,error)` 不统一 | 统一 nil 安全取值与返回形态 |
| U-19 | P3 | `utils.go:99-125,189-203` | `GenerateEntryPod`/`GenerateWorkerPod` 的 `pod.Spec = role.EntryTemplate.Spec` 是浅拷贝，`AddPodEnvVars` 随后修改共享的 `Containers[i].Env`。现有调用方先 `DeepCopy` 所以安全，但函数本身不保护；`GenerateWorkerPod` 在 `WorkerTemplate == nil` 时返回 nil | 函数内部深拷贝，或在文档写明 |
| U-20 | P3 | `utils.go:42,57,63-77,93-94` | `Entry = "true"` 命名有歧义；57 行注释写 `ServingGroupRegex`，变量是 `servingGroupRegex`；`GenerateServingGroupName(miName, …)` 参数名过时；`GeneratePodName` 参数 `roleName` 实际接收 `roleID`；`GetParentNameAndOrdinal` 数字溢出 int32 时返回 ordinal `-1`；`GetNamespaceName` 与字面量 `types.NamespacedName{}` 混用 | 重命名并修正注释 |
| U-21 | P2 | `coordination_comparison.go`、`coordination_partition.go` | controller 级协调逻辑放在 `utils`；用别名 `api`；import 分组不符合 goimports；部分文件头许可证没有空行 | `EqualRoleTemplateForRevision` 并入 `revision.go`，`CoordinatedPartitionConflict` 移出 |
| U-22 | P2 | `coordination_partition.go:32-91` | `CoordinatedPartitionConflict` 圈复杂度 29；用 `-1` 作"无冲突"哨兵（调用方一个用下标 `webhook/role_coordination_validator.go:59`，一个忽略 `role_rolling_update_coordinator.go:826`）；百分比字符串解析两次；魔数 `1`/`100`。区间相交逻辑经追踪未发现错误 | 拆 `resolveBounds`/`intersect`/`formatConflict`，返回 `*Conflict` |
| U-23 | P3 | `revision_data.go:78-146` | `buildRevisionPatch` 约 70 行并深拷贝整个 ModelServing | 拆 `normalizePlugins`/`normalizeRoles` |
| U-24 | P3 | `revision_data.go:35,169` | `defaultSchedulerName = "volcano"` 同值字面量还出现在 `podgroupmanager/manager.go:279`、`plugins/legacy_child.go:112`、`controller/role_coordinated_expansion.go:88`；`headlessOwnsEntryAddress` 硬编码 `"headless-service"`，注释自己写着"Keep aligned with headless-service OnPodCreate... utils cannot import plugins" | 常量集中到叶子包，适用性判断由插件元数据提供 |
| U-25 | P3 | `utils/test/helper.go:25-52` | `CreatePodGroupCRD` 是测试夹具却放在非 `_test` 包，会进入生产二进制；硬编码 CRD 名 | 迁到 `testutil` |
| U-26 | P3 | `controller_revision.go` / `controller_revision_lifecycle.go` / `revision_data.go` / `revision_util.go` | 4 个文件本质是同一模块的不同层，拆分边界是历史遗留（如 `ModelServingAtRevision` 在前者，却依赖后者 `ApplyRevision`） | 见 9.7 |
| U-27 | P3 | `utils.go:552-595` | `GroupNameIndexFunc`/`RoleIDIndexFunc` 带来 volcano 依赖；controller 里大量 `fmt.Sprintf("%s/%s", ns, group)` 拼 index key | 移入 `index.go`，提供 `GroupIndexKey` |

**utils 的"难以理解的写法"**

| 编号 | 位置 | 说明 | 建议 |
|---|---|---|---|
| U-H1 | `utils.go:414-425` | `SetCondition` 内联算 partition：`if p.Type == Int {…} else if ms.Spec.Replicas != nil { partition, _ = … }`，错误被丢弃，`Replicas==nil` 时静默得 0；传给下层的 `len(progressingGroups) > 0 && len(currentGroups) > partition` 一行多条件 | 随 U-13 删除；若保留，先赋给命名变量 `rolloutInFlight` |
| U-H2 | `utils.go:437-441,446-457` | 两个布尔 `rolloutActive, progressActive` 编码三种结果，Available 分支写成 `else if !progressActive`（双重否定）；`SetConditionWithRolloutState`（429-433）用 `len(progressingGroups) > 0` 推导第二个布尔 | 用枚举 `rolloutPhase{Available, Progressing, Updating}` + `switch` |
| U-H3 | `utils.go:405-411,459,469,479` | `newCondition` 永远设 `Status=True`，所以 469、479 行的 `newCond.Status == True` 恒真；`LastTransitionTime` 设置两次 | 删掉恒真判断与重复赋值，注释说明"只会设置 True" |
| U-H4 | `utils.go:460-477` | `for` 套 `if/else` 套 `if` 共 4 层，循环里同时改 `Conditions[i]` 并累计 `found`、`shouldUpdate`；同类型同状态不刷新 `Message` 这一隐式行为无注释 | 拆成 `upsertCondition`、`clearExclusive` |
| U-H5 | `utils.go:488-499` | `exclusiveConditionTypes(condition1, condition2)` 手写 4 个 `\|\|` 的对称组合，参数名无含义；`Progressing` 与 `UpdateInProgress` 不互斥只能从"缺了什么"读出 | 用 `map[type]set` 表达互斥关系并注明 |
| U-H6 | `utils.go:282-292` | `IsOwnedByModelServingWithUID` 名字只提 UID，实际还校验 APIVersion 与 Kind；`Is*` 谓词在返回 false 时有日志副作用，下游（如 `ensureService`）会被刷屏 | 去掉日志，改名 `HasModelServingOwnerRef` 并入 U-07 |
| U-H7 | `controller_revision.go:264-283` | `unfinishedInPlaceRevision` 用一个 struct 兼容新旧 schema：`if Version != 0 \|\| Phase != "" { if Phase != "Done" {…} } else if !Done {…}`，`else` 归属靠缩进推断；`"Done"` 是魔法字符串；annotation 缺失靠 `json.Unmarshal("")` 报错返回 `""` 表示"没有" | 显式 `if !ok { return "" }`，复用 `inplace.StateOf`（见 U-11） |
| U-H8 | `controller_revision.go:192-211,243-249` | 注释写"(CurrentRevision and UpdateRevision)"，实际还保留 referenced、Pod 引用与 mixed 的 revision；`if err != nil && !NotFound {warn} else {deletedCount++}` 把 NotFound 也计成"已删除" | `add := func(rev string)` 统一入集合；删除分支显式 `continue` |
| U-H9 | `controller_revision.go:158-169` | `ModelServingAtRevision` 靠 annotation `== V1` 区分格式，缺失或 `cr==nil` 都当"旧格式"，nil 要到 `GetRolesFromControllerRevision(nil)` 才报错 | 入口显式校验 `cr`，提取 `revisionDataVersion(cr)` |
| U-H10 | `coordination_partition.go:32-91` | 一个函数里隐式约定很多：43 行 `len(selected) > 0 && !selected[role.Name]`（空列表=全选）；34/90 行 `return -1, nil`（-1=无冲突）；46-49 行 `n := int32(1)`（replicas nil 按 1）；64-76 行 `lo/hi/open` 区间；77-82 行 `if cmp := lo.Cmp(lower); cmp > 0 {…} else if cmp == 0 {…}`（条件带副作用）；86 行 `cmp > 0 \|\| (cmp == 0 && lowerOpen)` | 抽 `type interval{lo, hi *big.Rat; loOpen bool}` 与 `intersect`；具名常量；返回 `*Conflict` |
| U-H11 | `utils.go:93-94,112-125` | `GeneratePodName` 参数 `roleName` 实际接收 `roleID`，94 行注释位置不当；`GenerateWorkerPod` 在 `WorkerTemplate==nil` 时返回 nil 并只打日志，用 nil 表示"没有 worker" | 改参数名；返回 `(pod, ok)` 或 error |
| U-H12 | `revision_util.go:43-75` | 局部变量 `copy` 遮蔽内建；两个 `removeRoleReplicasFor*` 近乎同逻辑；通用名 `Revision(obj)` 实际是 spew+fnv32 专用实现 | 合并；`Revision` 不导出并改名 `hashObject` |
| U-H13 | `revision_data.go:35,169` | `headlessOwnsEntryAddress` 靠注释"Keep aligned with…"保证与插件一致；`"headless-service"`、`"volcano"` 硬编码 | 常量集中，或由插件注册元数据提供 |
| U-H14 | `controller_revision.go:52-108` | 旧路径写死 `Revision: 1`，v1 路径 `maxRevision+1`；`RecordModelServingRevision` 用 `for {}` + 输入输出式 `collisionCount` 指针参数 | 统一修订号策略，给重试加上限，用返回值代替指针参数 |

### 9.2 `datastore`

| 编号 | 级别 | 位置 | 问题 | 建议 |
|---|---|---|---|---|
| D-01 | P1 | `store.go:124-127,157-160` | 调用方依赖 ServingGroup/Role 列表按 ordinal 有序，实现用 `slices.SortFunc` + `aIndex - bIndex`（不稳定排序）；`GetParentNameAndOrdinal` 对不匹配的名字返回 `-1`，这些项的最终顺序取决于 map 遍历，不确定；`GetRoleList` 排序用的 `Role.Name` 实际存的是 roleID | `SortStableFunc` + `cmp.Compare(ordinal)`，名字作第二键，并在接口文档声明排序契约 |
| D-02 | P2 | `store.go:109-131` | `GetServingGroupByModelServing` 对 `*servingGroup` 浅拷贝，返回值与内部共享 `runningPods`/`roles` map（因未导出暂时安全，但脆弱）；注释写"This is a clone"；手动 `RUnlock` 而非 `defer` | 返回快照类型或克隆 map，统一 `defer` |
| D-03 | P3 | `store.go:166-192` | `GetRolesByGroup` 返回 `map[string]map[string]*Role`，元素实为拷贝（测试 `TestGetRolesByGroupReturnsRoleCopies` 明确断言），指针让人误以为可写回 | 改 `map[string]map[string]Role` |
| D-04 | P1 | `store.go:109-114,139-143,171-175,201-216,246-250,257-279` | "未找到"语义不一致：ModelServing 不存在时 `GetServingGroupByModelServing` 返回 `ErrServingGroupNotFound`，`GetRoleList` 返回未包装的 `fmt.Errorf`；group 缺失时返回哨兵；roleName 缺失返回空切片；`GetRunningPodNumByServingGroup` group 缺失返回 `0, nil`；`GetServingGroupStatus` 把 `ServingGroupNotFound` 混进状态枚举；错误信息多数不带 namespace；调用方有 `==`（`coordinated_role_observation.go:69`）也有 `errors.Is` | 定义 `ErrModelServingNotFound`/`ErrServingGroupNotFound`/`ErrRoleNotFound`，用 `%w` 带 `namespace/name`，调用方统一 `errors.Is` |
| D-05 | P2 | `store.go:322-463` | `AddServingGroup`、`AddRole`、`AddRunningPodToServingGroup`、`AddServingGroupAndRole` 有 4 份近似相同的"创建 group"块，后两者几乎一样；`AddRole` 在 revision 冲突时只 warn 并跳过（371），`AddServingGroup` 静默忽略 | 提取 `getOrCreateGroup`/`getOrCreateRole`，统一冲突策略 |
| D-06 | P2 | `store.go:290-320,322-463` | `DeleteServingGroup`/`DeleteRole` 在 ModelServing 条目下留下空 map，之后 `Get…` 返回空切片而不是 `ErrServingGroupNotFound`，与 D-04 不一致，轻微无界增长；`DeleteModelServing` 后迟到的 Pod 事件触发 `Add*` 会重建缓存条目（取决于 `initialSync` 门控，不确定） | 删最后一个子项时回收空 map；`Add*` 对已删除的 ModelServing 不隐式创建 |
| D-07 | P3 | `store.go:477-527` | `UpdateServingGroupStatus`/`UpdateServingGroupRevision` 里 `groups[groupName] = group` 是指针自赋值，无作用；错误格式不统一 | 删多余赋值，统一 `modelServingName.String()` |
| D-08 | P2 | `store.go:32-57,241-253,385-407` | `GetRunningPodNumByServingGroup` 生产 0 调用；`runningPods` 除 `DumpCache` 外无生产读者（可能是死状态）；`Store` 接口 22 个方法过大；`AddRunningPodToServingGroup(…)` 连续 5 个 string 参数；`UpdateServingGroupStatus` 参数名 `Status` 首字母大写 | 删除或限测试；拆接口；参数改结构体 |
| D-09 | P3 | `dump.go:25-82` | `RunningPods` 输出顺序不确定；DTO 注释声称"without exposing internal lock mechanics"与实际无关；`Exported*` 类型仅 debug endpoint 使用 | 排序，修正注释 |

**datastore 的"难以理解的写法"**

| 编号 | 位置 | 说明 | 建议 |
|---|---|---|---|
| D-H1 | `store.go:116-127,157-160` | 顺序契约未在接口声明（116 行一句 `// sort ServingGroups by index` 还在拷贝循环之上）；隐含三个约定：不匹配名字返回 `-1`、用减法比较、`Role.Name` 实际是 roleID | 接口文档写明；改 `cmp.Compare` + 稳定排序 |
| D-H2 | `store.go:113,119-122` | 注释"This is a clone"实为浅拷贝；同一函数 113 与 122 行手动 `RUnlock` | 注释改"shallow copy"或深拷贝，统一 `defer` |
| D-H3 | `store.go:138-148` | `GetRoleList` 有三种"找不到"：ModelServing 缺失→未包装 error；group 缺失→哨兵；role 缺失→空切片 + nil。调用方必须记住 | 统一语义（D-04）并写入接口注释 |
| D-H4 | `store.go:384,398-407,385-462` | 384 行注释提到的 `runningPodOfServingGroup` 不存在；407 行注释说 `runningPods` "has been initialized during AddServingGroup"，同函数 398 行就会创建；`AddRunningPodToServingGroup` 与 `AddServingGroupAndRole` 近乎复制粘贴并复用外层 `ok` | 改正注释，提取 `getOrCreate*` |
| D-H5 | `store.go:300-320,345-383` | `DeleteRole` 三层 `!ok → return` 早退；所有 `Add*`/`Delete*` 无返回值，失败静默；`AddRole` revision 冲突只 `klog.Warningf` 并 skip（371），调用方感知不到 | 返回 `changed bool` 或 error |

### 9.3 `podgroupmanager`

| 编号 | 级别 | 位置 | 问题 | 建议 |
|---|---|---|---|---|
| G-01 | **P0** | `manager.go:195-229,167-176,231-233` | `initPodGroupInformer` 在持有 `m.lock` 时调用 `cache.WaitForCacheSync`（225 行），可能永久阻塞；`GetPodGroupInformer`/`GetPodGroupLister`/`stopPodGroupInformer` 都要拿同一把锁，reconcile worker 和 CRD 删除事件都会被卡住（CRD delete 事件还排在被阻塞的 add 处理器之后），有类似死锁风险；初始化回调也在锁内执行（读代码推断，未复现） | 锁内只做状态切换，`WaitForCacheSync` 与回调放锁外；同步失败时清理并返回错误 |
| G-02 | P1 | `manager.go:78-142,119,221-224` | informer 的 ctx 来自 `context.Background()`（221），与 `Run` 的 `parentCtx` 无关，controller 停止时 informer 不停，goroutine 泄漏；`NewManager` 用 `context.TODO()`（89）；`AddFunc` 对 `obj` 未检查类型断言（119，会 panic），`UpdateFunc` 却检查；非 NotFound 的 CRD Get 错误只记日志（101）；`Store(false)` 对零值 atomic 冗余；`apiextClient` 为 nil 会 panic | `NewManager` 接受 ctx，informer ctx 派生自 `parentCtx`，用 tombstone 安全的类型检查 |
| G-03 | P2 | `manager.go:246-268` | `CreateOrUpdatePodGroup` 返回 `(error, time.Duration)` 违反"error 放最后"（staticcheck ST1008；接口签名在 `controller/model_serving_controller.go:76`，调用方在 `:3285`）；`retryAfter>0` 被当作"记日志并重新入队、吞掉错误"；"PodGroup 不归属该 ModelServing"（264）会永久以 `enqueueAfter`（1s）重试，无退避无 Event | 改 `(time.Duration, error)` 或类型化 `RequeueAfterError{After}`，对"不归属"使用限速队列并发 Event |
| G-04 | P3 | `manager.go:62-73,78-80` | `kubeClient` 字段与 `NewManager` 参数从未使用；导出的可变字段 `CrdInformer`/`PodGroupInformer`/`PodGroupLister` 绕过锁，仅测试赋值 | 删 `kubeClient`；字段改未导出并提供测试 option |
| G-05 | P3 | `manager.go:531-550` | `calculateRequiredRoleNames` 生产不可达，仅 `manager_test.go:881` 引用；注释笔误"This scenario is This scenario is" | 删除；确认 `datastore` 导入是否仅此处使用 |
| G-06 | P2 | `manager.go:293-337`（331-335） | `createPodGroup` 在 `Create` 返回 `AlreadyExists` 时被吞掉，随后仍打印"Created PodGroup ..."，且不走更新 | AlreadyExists 时走更新或区分日志 |
| G-07 | P2 | `manager.go:453-493` | `updatePodGroupIfNeeded` 用 `RetryOnConflict`，但每次重试都从 informer lister 重读，对象仍是旧的，重试基本无效；`hasSubGroupPolicy` 变 false 时 SubGroupPolicy 不会被清理；479 行注释"Apply network topology policy"实际在 SubGroupPolicy 代码之上；489 行日志写死"group-level gang scheduling" | 冲突时返回并由 controller 重新入队，或实时 GET；对称清理 |
| G-08 | P2 | `manager.go:352-385,637-675` | `calculateRequirements` 与 `appendSubGroupPolicy` 重复 `MinRoleReplicas` 计算，均解引用 `*role.Replicas` 无 nil 检查；`minRoleMember` 存的是"每个 replica 的 pod 数"，名字误导；`appendSubGroupPolicy` 名为 append 实为原地覆盖并返回同一指针 | 复用 `utils.RoleReplicas`，改名 `applySubGroupPolicy` |
| G-09 | P3 | `manager.go:389-~408` | `aggregateResources` 忽略 init 容器、`Overhead` 和仅有 limits 的请求（是否影响调度不确定）；`total == nil` 守卫（390）无作用 | 确认意图后补测试，删无用守卫 |
| G-10 | P2 | `manager.go:509-525` | `CleanupPodGroups` 没以 `hasPodGroupCRD` 为前置，且 `volcanoClient` 可能为 nil（潜在 nil 解引用，而 `DeletePodGroup` 有门控）；仅按 name label 选择并删除所有 PodGroup，无 owner/UID 校验；521 行日志"(gang scheduling disabled)"有误导（networkTopology 被移除时也调用，`:299` 传 `context.TODO()`）；509 行注释写 `cleanupPodGroups`；513、519 用 `%v` | 加 CRD 门控与 nil 检查，按 ownerRef 过滤，修正日志注释 |
| G-11 | P3 | `manager.go:212,258,496-507,513,519` | `%v`/`%w` 混用；`DeletePodGroup` 无文档注释；可用 `apierrors.IgnoreNotFound` | 统一 |
| G-12 | P3 | `manager.go:56,270-279,340-350,411-415,567` | `groupNameKey = "GroupName"`（56）与 `controller/model_serving_controller.go:69` 的 `GroupNameKey` 重复且必须相等；`shouldCreatePodGroup` 注释说"检查 gang scheduling 或 networkTopology 是否启用"，实际只检查 CRD 存在和 `SchedulerName == "volcano"`；`GenerateTaskName`/`buildOwnerReference`/`aggregateResources` 的 receiver 未使用；`buildOwnerReference` 与 `utils.newModelServingOwnerRef` 构造方式不同（无 `BlockOwnerDeletion`）；`handlePodGroupCRDChange(crd, isDeleted bool)` 布尔标志参数 | 共享常量，改普通函数，ownerRef 统一，拆 `onCRDUpsert`/`onCRDDelete` |
| G-13 | P3 | `manager.go:417-450` | `getExistingPodGroups` 在 lister 为 nil 时回退实时 List，而 `CreateOrUpdatePodGroup` 同样情况返回错误 | 统一策略 |
| G-14 | P3 | `manager.go`（约 680 行） | 单文件混合 CRD 监听、informer 生命周期、PodGroup 构建与 CRUD | 拆 `manager.go`/`crd.go`/`podgroup.go` |

**podgroupmanager 的"难以理解的写法"**

| 编号 | 位置 | 说明 | 建议 |
|---|---|---|---|
| G-H1 | `manager.go:326-327,373,480-481` | `hasSubGroupPolicy.Load()` 在 `calculateRequirements`（373，决定是否填充 `minRoleMember`）与调用方（`createPodGroup` 327、`updatePodGroupIfNeeded` 480-481，决定是否调 `appendSubGroupPolicy`）各读一次。若两次读取之间 CRD 事件翻转了该标志，`minRoleMember[role.Name]` 取到 0，`SubGroupSize` 会指向 0（读代码推断，未复现） | 入口读一次并向下传，或总是填充 `minRoleMember` |
| G-H2 | `manager.go:352-385` | 返回 `(int, map[string]int32, corev1.ResourceList)` 三元组，靠位置传语义；`minMember = minMember + (…)` 冗余；376-381 行对每个 worker 副本循环调用 `aggregateResources(..., minRoleReplicas)`，参数相同可一次乘出 | 返回 `requirements` 结构体，循环改乘法 |
| G-H3 | `manager.go:637-675` | `appendSubGroupPolicy` 名为 append 实为整体覆盖；`MinRoleReplicas` 的写法与 `calculateRequirements` 不同（先赋 `int(minReplicas)` 再 `min(...)` vs 直接 `min(...)`）；函数内新旧 `minReplicas` 同名异义；两层 if 可合并 | 改名 `applySubGroupPolicy`，复用 `minRoleReplicas()` |
| G-H4 | `manager.go:567-598` | `handlePodGroupCRDChange(crd, isDeleted bool)` 布尔标志；三处重复 `hasPodGroupCRD.Store(false); hasSubGroupPolicy.Store(false)`；"是否可用"有两个来源（`hasPodGroupCRD` 原子量与 `GetPodGroupLister()==nil`，`updatePodGroupIfNeeded` 458 行）可能不一致 | 拆 `onCRDDeleted`/`onCRDUpserted`，用 `setCapabilities(has, sub)` 统一写入 |
| G-H5 | `manager.go:453-493` | `existing` 参数只用于取 ns/name；重试闭包里用 informer lister 重读并整体重算；479 行注释与代码对不上；489 行日志写死 | 改实时 GET 或交给 controller 重新入队，修正注释日志 |
| G-H6 | `manager.go:633-635` | `hasPodGroupChanged` 名字泛指，实际只比较 `Spec`，忽略 labels 与 ownerRefs | 改名 `specChanged` |
| G-H7 | `manager.go:246-268` | `(error, time.Duration)` 中 `0` 表示"不重试"、`enqueueAfter` 表示"重试"；"not owned"这类终态错误也映射成重试，调用方遇到 `retryAfter>0` 就吞错 | 见 G-03 |
| G-H8 | `manager.go:270-279` | `shouldCreatePodGroup` 注释与行为不符 | 注释对齐，`"volcano"` 用常量 |

### 9.4 `inplace`

| 编号 | 级别 | 位置 | 问题 | 建议 |
|---|---|---|---|---|
| I-01 | P2 | `state.go:135,141,146` 及方法 ~165-250 | 包级函数 `InProgress`/`Completed`/`OnTarget` 生产不可达（`InProgress` 仅 controller 测试和 e2e，`Completed`/`OnTarget` 仅 `state_test.go`），生产使用同名方法；包级与方法同名成对出现，加上 `InProgress` 与 `Updating` 近义，令人困惑 | 保留一种形态（让方法对 nil 接收者安全），写清 `InProgress` 与 `Updating` 区别 |
| I-02 | P1 | `eligibility.go:59-66`（hash 比较在 65 行） | `OnlyRoleImagesChanged` 用 `CalRoleTemplateHash` 比较 role，而它是 32 位 FNV，碰撞时会把"非镜像变更"误判为"仅镜像变更"，走 in-place 而不是重建；`EqualRoleTemplateForRevision`（DeepEqual）已存在 | 改用 `EqualRoleTemplateForRevision`（先把镜像字段归零再深比较） |
| I-03 | P3 | `eligibility.go:27-68` | `OnlyImagesChanged` 返回 `(bool, error)`，`OnlyRoleImagesChanged` 只返回 `bool`；`findRole`（68）定义在 `eligibility.go` 却被 `state.go` 使用 | 统一返回形态，`findRole` 移到共享位置 |
| I-04 | P2 | `state.go:~373-450` | `Start` 约 45 行：调用 `StateOf(pod)` 两次、每个容器调用 `containerStatus` 两次；用 `!state.Done` 而不是 `InProgress()`；patch 失败时返回 `(true, err)` 契约含糊。`Complete` 在 state 为 nil 时用 `metav1.Now()`，而 `Start` 接受注入的 `now`；state 已 Done 时仍会重新 patch | 只解析一次 state，错误时返回 `(false, err)`，两者都接受注入的 `now` |
| I-05 | P3 | `state.go:~68-87` | `StateOf` 在 JSON 解析失败时静默返回 nil，上层可能把损坏的 annotation 当成"没有 in-place 状态" | 返回 `(*State, error)` 或至少记日志 |
| I-06 | P2 | `state.go:~90-133`、`utils/controller_revision.go:264-283` | 旧格式迁移（`legacyState`/`migrateLegacyState`）已核实包含在 v1.1.0-rc.0（该 tag 的 `state.go` 中就有这两个符号），不能随便删除；`inplace` ↔ `utils` 的依赖倒置（U-11）与 autoscaler 对 `inplace` 的耦合要通过叶子包解决 | 迁移代码隔离到 `legacy.go` 并写明移除版本；State schema 与解码移到叶子包 |

**inplace 的"难以理解的写法"**

| 编号 | 位置 | 说明 | 建议 |
|---|---|---|---|
| I-H1 | `state.go:~68-87,~107-133` | `StateOf` 靠 probe 结构 `{Version, Phase}` 判定"旧格式"（`Version != 0 \|\| Phase != ""`），是隐式 schema 探测；两处 JSON 错误都返回 `nil`，于是 `nil` 既表示"没有 state"也表示"state 损坏"；`migrateLegacyState` 里 `legacy.Phase == "Done"` 魔法字符串，`Baseline[name].RestartCount + 1` 的 `+1` 只靠一行注释 | schema 版本做成显式字段，返回 `(*State, error)`，常量化 `"Done"` |
| I-H2 | `state.go:373-420` | `Start` 返回的 bool 是"本次是否发起了 patch"，但 patch 失败时仍返回 `true`（419 行 `return true, applyPatch(...)`）；`StateOf(pod)` 在 374、378 行各调一次；374 行条件 `state != nil && !state.Done && state.Revision == revision` 一行三个条件；400-404 行 `Expected` 逻辑"先把基线上调到当前 `RestartCount` 再 `++`"，`Expected` 名字看不出是"期望重启次数" | 拆 `buildState(prior, pod, images)` 与 `renderPatch`，抽 `expectedRestart(status, prev)` |
| I-H3 | `state.go:~425-450` | `Complete(…, revision, roleHash string)` 用 `roleHash == ""` 表示"不更新 hash label"（空串哨兵）；`state == nil` 时凭空构造 Done 状态且用 `metav1.Now()` | 用 option 或指针参数，注入 `now` |
| I-H4 | `state.go:135-158` | 包级 `InProgress`/`Completed`/`OnTarget`/`Updating`/`UnexpectedRestart` 与同名方法成对出现；`OnTarget` 注释"is the package-level form of State.OnTarget"是同义反复；能工作靠方法对 nil 接收者安全，但"nil 即无状态"这一约定没有写在任何地方 | 见 I-01，并在 `State` 文档里写明 nil 语义 |
| I-H5 | `eligibility.go:36-66` | `OnlyRoleImagesChanged` 的 `false` 同时表示"role 不存在"与"不止镜像变了"；`OnlyImagesChanged` 靠"把新镜像拷进旧对象再比较 canonical 数据"这种间接手法，要看懂 `copyImages` 才理解 | 返回 `(bool, reason/error)`，role 缺失单独处理 |

### 9.5 `webhook`

| 编号 | 级别 | 位置 | 问题 | 建议 |
|---|---|---|---|---|
| W-01 | P1 | `eviction_handler.go:217-222` | `podLister.List` 失败时 `return true, ""`（**fail-open**，已核实）；同函数 tracker 更新失败返回 deny，`Handle` 在 Get pod/MS 失败时也是 deny，三处失败策略不一致 | 统一为 fail-closed，或在注释里写明与 `failurePolicy` 配合的理由 |
| W-02 | P2 | `eviction_handler.go:98-119`、`cmd/kthena-controller-manager/main.go:211-216` | `AllowEviction` 只在"开关关闭 + 外部手工装了 webhook 配置"时才有调用方，chart 部署下没有调用方（见 1.3 说明）；它重复了 `Handle` 的解码与响应逻辑，原因是 `allow`/`deny`/`sendResponse` 是不使用 `h` 的方法 | 二选一：删除 else 分支和 `AllowEviction`；保留则把三个方法改为包级函数并复用 |
| W-03 | P2 | `eviction_handler.go:121-207`、tracker 写入约 `:283` | `Handle` 不检查 `ar.DryRun`（已核实该文件无 `DryRun` 引用）。chart 声明 `sideEffects: None`（`validating-webhook.yaml:77`），但 handler 会更新 ConfigMap；`kubectl drain --dry-run=server` 会占用 disruption 预算 TTL 秒 | `DryRun` 为真时只做判定，不写 tracker |
| W-04 | P2 | `eviction_handler.go:70,211-214` | 全局 `evictionTrackerLocks sync.Map` 只增不减，每个出现过的 ModelServing 留一把锁（缓慢泄漏）；锁仅进程内有效，跨副本正确性靠 ConfigMap `Update` 的 resourceVersion 乐观锁 | 锁表放进 `EvictionHandler`，注释说明乐观锁才是并发保护，或去掉锁表 |
| W-05 | P2 | `eviction_handler.go:302-380` vs `382-477` | `checkServingGroupProtection` 与 `checkRoleProtection` 是同一状态机的复制（目标缺失→deny；目标已 not-ready→allow；已被 tracker 记录→allow；ready 数超过 minAvailable→allow 并记录；其余→deny），每个分支都带一次 13 个参数的日志调用（479-489） | 抽 `evaluateBudget(budgetInput) decision`，用 `logger.With(...)` 一次性输出 |
| W-06 | P2 | `eviction_handler.go:143-163` vs `168-189` | "先查 lister、失败再 live GET、NotFound→allow、其他错误→deny、lister miss 但 live 命中则 Warning" 对 Pod 和 ModelServing 各写了一遍 | 泛型 `getWithFallback[T]` 返回 `(obj, found, err)` |
| W-07 | P3 | `eviction_handler.go:150,175` | 对"对象已不存在所以 allow"的正常路径用 `klog.Errorf`，且重复打印 `listerErr` 与 `err` | 降为 `Infof` / `V(2)` |
| W-08 | P3 | `eviction_handler.go:114,1016` | `response, _ := json.Marshal(...)` 吞错 | marshal 失败返回 500 |
| W-09 | P3 | `eviction_handler.go:245-250,479-489` 等 | 每次 admission 在 Info 级别输出 15+ 个 key 的长日志，且先 `disruptionEntryKeys` 排序；`klog.Errorf("%s", reason)` 写法；全仓无 `klog.InfoS` | 决策日志降到 `V(2)`，统一结构化日志 |
| W-10 | P3 | `eviction_handler.go:65,271,846-888` | `disruptionEntry.triggerPodName` 只写入 ConfigMap、从不读取 | 保留则注明"仅供排查"，否则删除 |
| W-11 | P3 | `eviction_handler.go:846,878,884` | 三处重复声明同一个匿名结构体（`ExpiresAt`/`TriggerPodUID`/`TriggerPodName`） | 具名类型 `encodedEntry`，decode 单独兼容旧格式 |
| W-12 | P3 | `eviction_handler.go:589-598` | webhook 的 `isPodReady` 与 `utils` 里的同名函数语义不同（前者多判 `DeletionTimestamp`） | 统一到 `utils` 并写清差异 |
| W-13 | P3 | `eviction_handler.go:965-971` | `minAvailableOrDefault` 的"默认 1"对通过校验的对象不可达（`validator.go:620-624` 要求 `MinAvailable` 非 nil），只为历史对象保留 | 加注释说明保留对象 |
| W-14 | P3 | `eviction_handler.go:725,738,790~` | `ms.UID == ""` 时跳过 ownership 判断和 ownerRef 设置；真实 apiserver 对象必有 UID，疑似为测试夹具保留的生产分支（**不确定**，需核对测试是否依赖空 UID） | 测试补 UID 后删除分支，对应 C-T01 |
| W-15 | P2 | `validator.go:49`、`utils/utils.go:527` | `ParseModelServingFromRequest` 在 `Request == nil` 时空指针 panic，另见 U-17 | 判空返回 400，`%v`→`%w` |
| W-16 | P2 | `plugin_validation.go:40-47` | `BuildRevisionData` 返回的 `err` 被丢弃，用户只看到 "cannot compare existing plugin configuration"，没有原因 | 带上 `err` 或至少 klog |
| W-17 | P2 | `validator.go` 全文，对照 `plugins/manager.go:72-97` | webhook 非测试文件里没有任何创建期 plugin 校验：未知插件名、重复、`Type` 非 BuiltIn、`scope.target` 非法，都要等控制器运行时 `NewChain` 才报错 | `plugins` 提供 `ValidateSpecs(registry, specs) field.ErrorList`，`NewChain` 与 webhook 共用 |
| W-18 | P2 | `pod_template_validation.go:24-27` | 生产代码直接 import `k8s.io/kubernetes` 内部包（`pkg/api/pod`、`apis/core`、`apis/core/v1`、`apis/core/validation`），仓库里唯一的生产使用点，把控制器绑死在 `k8s.io/kubernetes v1.35.3` | 评估后保留，文件头注明原因与升级注意事项，或改 CEL/最小校验 |
| W-19 | P3 | `pod_template_validation.go:49`、`plugins/headless_service_plugin.go:228` | 局部变量 `copy` 遮蔽内建 | 改名 |
| W-20 | P1 | `validator.go:202-263` vs `265-336` | `validateRollingUpdateConfiguration` 与 `validateMaxUnavailableForRoles` 重复同一流程（`validateIntOrPercent`→`GetScaledValueFromIntOrPercent`→计算 partition 并忽略错误→判断 `maxUnavailable==0 && maxSurge==0`），且**行为不一致**：只有 role 版检查 `maxUnavailable > replicas`（约 311-318）；非法百分比会被报两次 | 抽 `validateRollingBudget(cfg, replicas, path)`，SG 与 Role 共用 |
| W-21 | P2 | `validator.go:265,338` | `validateMaxUnavailableForRoles` 实际校验 maxSurge、partition、maxUnavailable，名不副实；`validatorReplicas` 与其他 `validateXxx` 命名不一致 | 改名 `validateRoleRollingUpdateConfiguration` |
| W-22 | P2 | `validator.go:338-368` | `Spec.Replicas == nil` 被判"must be a non-negative integer"，而 CRD 默认值为 1（`model_serving_types.go:41`），同包其余代码用 `replicasOrDefault` 把 nil 当 1；函数同时校验 spec 副本数、roles 非空和 role 副本数 | 拆开，nil 处理与其他校验一致 |
| W-23 | P2 | `validator.go:381,402`、`role_coordination_validator.go:349`、`validator.go:294` | "role 查找表"出现 5 种形态（`roleMap` 按值拷贝整个 `Role`、`roleSpecsByName`、`roleReplicas`、`roleNames`、`selectedRoleNames`）；"副本默认 1"有三处手写，没用同文件的 `replicasOrDefault`（177） | 只留 `roleSpecsByName` 与 `replicasOrDefault`，放进 `utils` |
| W-24 | P3 | `validator.go:479-485` | `validateNonnegativeField` 是 `apivalidation.ValidateNonnegativeField` 的复制品，而该包已导入 | 直接使用 |
| W-25 | P2 | `validator.go:488-537` | `validateWorkerImages` 与 `validatePodTemplates` 大部分重叠（k8s core 校验已要求 image 非空且无首尾空白）；`validateImageField` 的 `image == ""` 分支不可达；注释写 "valid Docker reference" 实际只检查空白；只遍历 `Containers`，不含 `InitContainers`，函数名也不准（同时校验 entry 和 worker，两段复制粘贴） | 删除，只靠 `validatePodTemplates` |
| W-26 | P2 | `validator.go:452-477` vs `role_coordination_validator.go:126-136` | 百分比解析（`IsValidPercent`+`TrimSuffix`+`Atoi`+范围）两处各一份；`validateIntOrPercent` 的 `default:` 基本不可达；`>100` 一律拒绝，对 `maxSurge` 是否预期（**不确定**） | 抽 `parsePercent(s) (int, error)` |
| W-27 | P3 | `validator.go:543-548,576` | 错误信息 "valid pairs: (SGRecreate,SGRollingUpdate) or (RoleRecreate,RoleRollingUpdate)"，但实际只拒绝 `ServingGroupRecreate + RoleRollingUpdate`，默认组合 `(RoleRecreate, ServingGroupRollingUpdate)` 被接受却不在"valid pairs"里；注释语法含混，变量名 `unMatched` 不规范 | 改成 "ServingGroupRecreate cannot be combined with RoleRollingUpdate" |
| W-28 | P3 | `validator.go:642-683` | 副本数为 nil 或负的 role 被跳过，随后被误报成 "role does not exist in template.roles"；对 map 迭代，错误顺序不确定 | 区分"不存在"与"副本数无效"，按 key 排序 |
| W-29 | P3 | `validator.go:156-175` | 注释写 `validNameLength`，函数是 `validGeneratedNameLength`；拼名逻辑近似实际 pod 名格式且无文档；用户错误用 `klog.Errorf`（165）；每个无效 role 重复报同一个 `metadata.name` 错误，并与 `validateRoleNames` 重复 | 注释说明拼名规则，去掉 klog，合并到 `validateRoles` |
| W-30 | P2 | `validator.go:127-143` | `modelServingValidationErrors` 串联 12 个校验，其中 7 个各自遍历一次 roles | 合并成 `validateRole(i, role)` 单循环 |
| W-31 | P3 | `validator.go:39-44,110,116` | `ModelServingValidator` 是空 struct，`validateModelServing`/`validateModelServingUpdate` 是不用 receiver 的方法；`Handle` 里有大量复述代码的步骤注释 | 改包级函数，删冗余注释 |
| W-32 | P3 | `validator.go:121,123` vs `role_coordination_validator.go:48` | `validateCoordinatedPartitions` 内部又调一次 `validateRoleCoordinationImmutable`，更新路径算两次；`oldMS == nil` 在更新路径是死分支（66、219 行） | 去重，删死分支 |
| W-33 | P1 | `role_coordination_validator.go:104-212` | `validateRoleCoordination` 约 109 行，圈复杂度 28，做 4 件事 | 拆 `validateMaxSkew`、`resolveSelectedRoles`、`validateDependencies`，保留 `roleDependencyGraphHasCycle` |
| W-34 | P1 | `role_coordination_validator.go:137-155,357-376,449-454` | "被协调的 Role 集合"概念在 4 处各写一份：webhook 的 `validateRoleCoordination`、`selectedRoleNames`，`controller/role_rolling_update_coordinator.go:359-390`，`utils/coordination_partition.go:36-45`；"依赖图过滤到 selected"在 `validateInProgressDependencyCapacity`（295-306）与 `dependencyCapacityRequirements`（386-395）各写一遍。**一旦漂移会静默改变业务行为**，对应 C-X 系列 | `utils` 提供 `CoordinatedRoleSet(ms)` 与 `FilterDependencies(...)`，webhook/controller 共用 |
| W-35 | P2 | `role_coordination_validator.go:240-248,342-347` | `validateRoleCoordinationUpdate` 里内联解析 partition，出错 `continue` 静默跳过，而同文件已有 `resolvedRolePartition` | 复用，并注明为何可忽略错误 |
| W-36 | P3 | `role_coordination_validator.go:284`、`controller/role_rolling_update_coordinator.go:281` | 魔法字符串 `"RolloutComplete"` 两处各一份 | 放 API 包做常量 |
| W-37 | P3 | `role_coordination_validator.go:65-101,449-454` | `roleCoordination(ms)` 在 Type 非 `RoleRollingUpdate` 时返回 nil，名字没体现这个门控；`validateRoleCoordinationImmutable` 里 `names` 部分用了门控（73），`canonical` 部分（85-88）直接读 `RoleCoordination`，同一函数两种语义 | 改名 `activeRoleCoordination`，统一 |
| W-38 | P3 | `webhook/` 目录 | 文件名后缀混杂（`validator.go` / `role_coordination_validator.go` / `plugin_validation.go` / `pod_template_validation.go`）；`replicasOrDefault` 在 `validator.go` 却被 `eviction_handler.go` 等使用；1021 行的 `eviction_handler.go` 混了 HTTP、状态机、tracker 存储、key 编解码 | 见 9.7 |

**webhook 的"难以理解的写法"**

| 编号 | 位置 | 说明 | 建议 |
|---|---|---|---|
| W-H1 | `eviction_handler.go:142-143,170-171` | 复用 `err`：`listerErr := err; pod, err = ...Get(...)`，同一变量先后承载两个含义 | 单独函数返回 `(obj, listerErr, err)` |
| W-H2 | `eviction_handler.go:84` | 用变参 `disruptionTTL ...time.Duration` 模拟可选参数，`[0] > 0` 才生效，签名看不出语义 | functional option 或 `Config` 结构体 |
| W-H3 | `eviction_handler.go:339-345,435-441` | `minAvailable := -1` 作哨兵，靠 `if totalX == 0` 先后顺序隐式规避使用；`GetScaledValueFromIntOrPercent` 的 error 被 `_` 吞掉 | `total==0` 提前返回，出错按 deny |
| W-H4 | `eviction_handler.go:615-654,658-668` | 方法 `(h) cleanupRecoveredDisruptionEntries` 与同名包级函数仅靠 receiver 区分；方法返回"可能被替换的 pods 切片"同时原地修改 `entries`；`needsLiveRecoveryCheck` 与循环前半段重复 | 改名 `refreshAndCleanRecovered`，抽 `recoveredUnitPods` |
| W-H5 | `eviction_handler.go:843-860` | 条件里夹带副作用和吞错：`if err := json.Unmarshal(...); err == nil && encodedEntry.ExpiresAt != "" {…; continue}`，失败后落入"旧格式 string"分支，靠"解析失败即旧格式"的隐式约定 | 先看 `value[0] == '"'` 判格式，再分别解码 |
| W-H6 | `eviction_handler.go:790-795` | 把 `AlreadyExists` 手工转成 `apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, ...)`，只为复用外层 `RetryOnConflict`，没有注释 | 加注释，或外层显式判断 `IsAlreadyExists` |
| W-H7 | `eviction_handler.go:931-963` | `parseDisruptionUnitKey` 靠 `/` 拆分后的段数判断格式（6 段新 Role 格式、5 段兼容旧格式、4 段 ServingGroup），非 Role 的未知 level 也被当作 ServingGroup | key 加版本前缀或结构化编码 |
| W-H8 | `eviction_handler.go:983-993` | `expectedRoleInstancesInServingGroup(ms, roleName, fallback)` 找不到 role 时返回 `fallback`，`fallback<=0` 时返回魔法值 `1`；名字说"期望实例数"，实际是 role 副本数加兜底 | 拆"查副本数"与"兜底"，常量化 |
| W-H9 | `validator.go:627` | `if len(allErrs) > 0 \|\| ms.Spec.Replicas == nil \|\| *ms.Spec.Replicas < 0 { return allErrs }` 把"校验已失败"和"无需继续比较"混在一行 | 拆两个 if 并各加注释 |
| W-H10 | `validator.go:219-262,294-335` | 约 40 行里 `maxUnavailableValue := 1`、`maxSurgeValue := 0`、`budgetsValid := true` 三个预置哨兵，加上 `if err == nil { partition = … }` 的吞错写法，逻辑依赖"CRD 默认值 MaxUnavailable=1"这个隐式前提 | 封装 `resolveBudget(cfg, replicas) (budget, ok)`，写清默认值来源 |
| W-H11 | `role_coordination_validator.go:36-63` | `changed = changed \|\| !exists \|\| replicasOrDefault(...) != ... \|\| !DeepEqual(...) \|\| !utils.EqualRoleTemplateForRevision(...)` 一行 5 个条件在循环中累积；一个函数同时处理 create（`oldMS==nil`）与 update | 拆 `roleCapacityChanged(old, new)` |
| W-H12 | `role_coordination_validator.go:408-430` | `required map[string]bool` 一个 map 承载双重语义：key 存在表示"需要目标版本容量"，value=true 表示"还需保留旧版本"；闭包 `visit` 在 `for roleName := range selected` 每轮重新定义，同函数里还有第二个遍历 | 具名类型 `type requirement struct{ retainOld bool }`，`visit` 提成独立函数 |
| W-H13 | `role_coordination_validator.go:258-266` | `retentionFloor`/`stableBase`/`replacementCapacity`/`expansionCapacity` 的 `min`/`max` 嵌套公式没有推导注释；`int32` 与 `int` 反复转换，`1` 是魔法数 | 封装 `targetCapacity(old, new, partition, retainOld)` 并写公式说明 |
| W-H14 | `pod_template_validation.go:63-73` | 错误字段路径靠字符串前缀（`prefix`、`prefix+"["`、`prefix+"."`）改写，循环内每次重新计算 `fldPath.Child(name).String()`；一个函数同时做默认化、类型转换、校验、路径改写 | 拆 `toInternalTemplate` 与 `rewriteMetadataPaths` |

### 9.6 `plugins`

| 编号 | 级别 | 位置 | 问题 | 建议 |
|---|---|---|---|---|
| P-01 | P2 | `types.go:35,40`、`headless_service_plugin.go:80,95,182` | `HookRequest.RoleIndex` 只被转发给 `ensureService(ctx, req, _ int)`，该参数被 `_` 丢弃（已核实）；`HookRequest.ServiceLister` 被控制器 4 处赋值（`model_serving_controller.go:1392,1958,2990,3136`），plugins 包内无任何读取 | 删除第三个参数与两个字段；或让 headless 插件读 lister（P-09） |
| P-02 | P3 | `lws_labels_plugin.go:121-123` | `n < 0` 分支不可达（`podName[lastDash+1:]` 不含 `-`，`Atoi` 不会返回负数） | 删除 |
| P-03 | P1 | `legacy_child.go:128-208`（`normalizeChildPodSchemaDefaults`，圈复杂度 27） | 三份近似相同的 container 循环（176-186），硬编码 AzureDisk/ISCSI/RBD/ScaleIO 默认值（`ext4`、`rbd`、`admin`、`/etc/ceph/keyring`、`xfs`、`ThinProvisioned`），会随 k8s 版本漂移，覆盖也不完整；同仓库 `pod_template_validation.go:57` 用的是官方 `SetObjectDefaults_PodTemplate`，出现两套"补默认值"策略 | 拆 `normalizeContainers`/`normalizeVolumes`；评估改用上游默认化并说明不能的原因 |
| P-04 | P2 | `legacy_child.go:98-125` | `normalizeChildDefaults` 手抄 CRD kubebuilder 默认值（`"volcano"`、`RoleRecreate`、`10`、`0`、`MaxUnavailable=1`、`MaxSurge=0`、`Partition=0`），有漂移风险 | 集中为常量或复用 CRD 默认化函数 |
| P-05 | P2 | `legacy_child.go:36-96`（`PreserveChildPluginConfiguration`） | 约 60 行，同时判断旧对象是否有 headless 插件、比较插件声明、回写 `desired.Spec.Plugins`（61）与 `desired.Spec = spec`（93）、判断"只缩容"；名叫 Preserve 实际原地改 `desired`；第二个嵌套循环（85-93）按名字再配一次 role，前面已按下标配过（71-79） | 拆 `isCountsOnlyChange` 与 `applyCountsOnly` |
| P-06 | P3 | `legacy_child.go`、`manager.go` 文件名 | `legacy_child.go` 实际只服务于"父控制器（booster、LWS）同步子 ModelServing 时保留插件"；`manager.go` 里是 `Registry` 与 `Chain`，没有 manager | 重命名，见 9.7 |
| P-07 | P1 | `headless_service_plugin.go:182-241`（`ensureService`，圈复杂度 21） | 一个函数做 5 件事：检查遗留 worker（190-199）、构造 selector/labels、get-or-create（208-219）、校验归属与终止状态（222-227）、对账更新（228-239） | 拆 `checkNoLegacyWorkers`、`desiredService`、`getOrCreateService`、`reconcileService` |
| P-08 | P2 | `headless_service_plugin.go:256-276`（`isManagedHeadlessService`，圈复杂度 21） | owner 检查一行 4 个条件（261），返回值是一行 8 个条件的 `&&` 链（268-272） | 拆 `isControlledBy`、`roleIdentityMatches`、`nameMatches`、`labelsMatch` |
| P-09 | P2 | `headless_service_plugin.go:164,190-199,205` | 每次 `OnRoleSync` 都直连 API server：`Pods.List` + 2 次 `Services.Get`，而 `HookRequest` 里本有 `ServiceLister` | 读走 lister，写走 client |
| P-10 | P2 | `headless_service_plugin.go:261`、`utils/utils.go:282-292`、`webhook/eviction_handler.go:737-749` | 三处判断"ModelServing 是否拥有某对象"，语义不一致（后两者分别比 UID 和 Name+UID，且 `utils` 版每次 false 都打 Warning） | 统一到 `utils`（同 U-07） |
| P-11 | P3 | `headless_service_plugin.go:115,143,187` | 同一句 `"Kubernetes client is not initialized"` 重复三次，且大写（ST1005）、常量串用 `fmt.Errorf` | `var errNilKubeClient = errors.New(...)` 或 `requireKubeClient(req)` |
| P-12 | P3 | `headless_service_plugin.go:223,226` 等 | "Headless Service ..." 大写开头；有的错误被包装（127、175、220），有的裸返回（`OnRoleDelete` 的 Get 约 163，`ensureService` 的 List 约 188、Update 约 237） | 统一包装，小写开头 |
| P-13 | P2 | `headless_service_plugin.go:197`、`legacy_child.go:59,82` | `"MigrationRequired: ..."` 用字符串前缀充当机器可读标记，调用方无法 `errors.Is` | 定义 `ErrMigrationRequired`，`%w` 包装 |
| P-14 | P2 | `headless_service_plugin.go:104,111-134,139` | 导出方法 `Capture` 直接取 `req.ModelServing.Namespace`，无 nil 判断；`OnRoleDelete` 却有 nil 守卫，入口健壮性不一致 | 统一在 `capture` 内守卫 |
| P-15 | P2 | `headless_service_plugin.go:39-45,136-178`、`types.go:42` | `RoleCleanup` 是通用框架类型，却放在 headless 插件文件，又被 `types.go` 的 `HookRequest` 引用（抽象泄漏）；`OnRoleDelete` 直接操作 `cleanup.mu`，并在持锁下调用无锁的 `capture` | 移到 `types.go`，清理逻辑做成 `RoleCleanup` 自己的方法 |
| P-16 | P3 | `headless_service_plugin.go:78,119,191` | `utils.GeneratePodName(req.ServingGroup, req.RoleID, 0)` 手写 4 次 | 封装 `entryPodName(req)` |
| P-17 | P3 | `headless_service_plugin.go:69-70` | `63` 与 `"-discovery"` 是魔法值 | 使用 `validation.DNS1035LabelMaxLength` 与具名常量 |
| P-18 | P1 | `manager.go:102-109` | `NewRoleDeleteChain(nil, …)`：`NewChain(nil, …)` 返回空 chain 且无错误（73-75），随后 `registry.roleDeletePlugins`（109）对 nil 解引用会 panic；当前调用方传 `DefaultRegistry`，风险较低，但是潜在隐患；`NewChain(nil)` 还会静默忽略已配置的插件（fail-open） | `registry == nil` 返回 error，或统一用 `DefaultRegistry` |
| P-19 | P2 | `manager.go:146-219` | 四个 `Chain.OnXxx` 结构完全一致，只有过滤函数（`shouldRun`/`shouldRunRole`）和 hook 名不同 | 抽 `chain.each(ctx, req, hookName, filter, call)` |
| P-20 | P3 | `manager.go:221-256` | `containsRole` 只是 `slices.Contains` 的薄封装；`matchesTarget(target, needle)` 参数含糊；`shouldRun` 与 `shouldRunRole` 前半段重复 | 内联或合并为 `scopeAllows(spec, req, checkTarget)` |
| P-21 | P3 | `manager.go:61,150,165,178,194,210` | 类型名 `entry` 被循环变量 `entry` 遮蔽 | 类型改 `chainEntry` |
| P-22 | P3 | `types.go:45-57`、`demo_plugin.go:86-96`、`lws_labels_plugin.go:83-93`、`headless_service_plugin.go:93` | `Plugin` 接口强制四个 hook，三个插件各写一堆 no-op | 提供可嵌入的 `BasePlugin` |
| P-23 | P2 | `demo_plugin.go:43-45` | 演示插件 `demo-pod-tweaks` 在 `init()` 中注册进 `DefaultRegistry`，即生产路径（examples 与用户文档在引用，不能直接删） | 注释写明仅供示例，考虑迁到 `examples` 或用 build tag 隔离 |
| P-24 | P3 | `lws_labels_plugin.go:28`、`legacy_child.go:30`、`headless_service_plugin.go:36` | 同一个包在三处用了三个别名（`msutils`、`utils`、`workload`/`workloadv1alpha1`） | 统一 |
| P-25 | P3 | `lws_labels_plugin.go:102` | `ref.Kind == "LeaderWorkerSet"` 魔法字符串，未校验 APIVersion | 使用 `leaderworkerset` 包常量 |
| P-26 | P3 | `manager.go:49-58`、`headless_service_plugin.go:54-55` | `Register` 与 `RegisterRoleDelete` 两步注册，忘记第二步会静默丢失清理能力；`Register` 对同名注册静默覆盖（50） | `Register(name, factory, opts...)` 用 option 表达 role-delete，重复注册报错 |

**plugins 的"难以理解的写法"**

| 编号 | 位置 | 说明 | 建议 |
|---|---|---|---|
| P-H1 | `headless_service_plugin.go:121-132,146-149` | 用 `c.targets == nil` 隐式表示"还没 capture 过"，空的非 nil map 才表示"已 capture 且无目标"；`OnRoleDelete` 里 `cleanup == nil` 时创建临时 `&RoleCleanup{}`，调用方是否传入决定了有无重试保护，均无注释 | 显式 `captured bool` 字段，注释写明 nil `Cleanup` 的含义 |
| P-H2 | `headless_service_plugin.go:203-215` | 循环内每轮重新构造 `selector`、`managedLabels`；`service, err` 在 get→create→AlreadyExists→get 之间复用同一对变量并靠 fall-through；`if name == entry { selector[EntryLabelKey] = "true" }` 的含义（只有 entry service 只选 entry pod）未解释，`"true"` 是魔法串 | `desiredServices(req)` 返回两个 spec 的列表 |
| P-H3 | `headless_service_plugin.go:193-196` | 一行三个条件 `IsOwnedByModelServingWithUID(&pod, ms.UID) && pod.Spec.Subdomain == entry && pod.Labels[EntryLabelKey] != "true"`，"旧版 worker 仍在使用遗留 subdomain"的业务含义没写出来；`&pod` 取循环变量地址且每轮拷贝整个 Pod | 抽 `usesLegacySubdomain(pod, entry)`，下标遍历 |
| P-H4 | `headless_service_plugin.go:247-254,263-265` | 命名返回值 `(group, role, id string, ok bool)` 加重新赋值，难判断返回的是哪一组；`isManagedHeadlessService` 靠 `GetParentNameAndOrdinal`（正则 `(.*)-([0-9]+)$`）解析 role ID 与 group 名，依赖"名字即身份"的命名约定 | 注释说明约定，改用显式 label 或返回结构体 |
| P-H5 | `legacy_child.go:40-49,61` | `hasHeadless` 循环无 `break`；"预期插件 = 旧插件 + 隐式 headless"（47-49）与 `desired.Spec.Plugins = old.DeepCopy().Spec.Plugins`（61）的组合含义是：父控制器现在总是注入 headless，旧子对象没有，所以先补上再比较，通过后再还原为旧值，**没有任何注释** | `slices.ContainsFunc`，补迁移语义注释 |
| P-H6 | `legacy_child.go:66-79` | `countsOnly` 的计算混合"检查"和"原地归一化"（循环里既判断又执行 `b.Replicas = a.Replicas`）；依赖 `normalizeChildDefaults` 已给指针赋值（`*next.Spec.Replicas` 解引用）以及其末尾按名字排序后按下标一一对应，这些隐式前提未写明 | 分"先检查，再应用"，注明依赖排序 |
| P-H7 | `manager.go:102-143` | 第一个循环一边收集名字，一边把已配置同名 entry 的 `Scope` 置 nil（113-115），副作用藏在"收集名字"的循环里；第二个循环再扫描一遍 `chain.entries` 判断是否已配置 | 先建 `configured := map[name]int`，再分两步 |
| P-H8 | `manager.go:161-171` | `PluginWithoutInPlaceUpdate() (string, bool)` 名字难以顾名思义（`true` 意为"存在不支持原地更新的插件"）；不考虑 scope（一个只作用于别的 role 的插件也会阻止原地更新），是否有意**不确定** | 改名 `FirstPluginBlockingInPlaceUpdate`，注明 scope 语义 |
| P-H9 | `manager.go:239-245` | `spec.Scope.Target == ""` 隐式表示"所有目标"；根因是 `model_serving_types.go:104` 写成 `// kubebuilder:default=All`，**缺 `+` 前缀（已核实）**，默认值 marker 不生效（修复需 `make generate`，属 API 范围，不在本次整理范围内，但应单独跟踪） | 修正 marker，或在注释里写明 `""` 的含义 |

### 9.7 各子包建议的目标布局

| 包 | 建议布局（纯搬移，不改逻辑） |
|---|---|
| `utils` | `names.go`（命名/序号/正则）、`pod.go`（Pod 生成与 env 注入、Pod 谓词）、`owner.go`（统一的 ownership 判断，U-07）、`revision.go`（`ModelServingRevision`/`CalRoleTemplateHash`/`EqualRoleTemplateForRevision`）、`controller_revision.go`（ControllerRevision CRUD 与 GC）、`revision_data.go`（v1 数据，取决于 U-01 的决策）、`condition.go`（只保留一个 `SetCondition*` 入口）、`rollout.go`（`GetMax*`）、`index.go`（informer index）。admission HTTP 解析移到 webhook 包与 router 共用；`CoordinatedPartitionConflict` 移到 webhook 或 controller；`utils/test/helper.go` 移到 `testutil` |
| `datastore` | `types.go`（`ServingGroup`/`Role`/哨兵错误）、`store.go`（`Store` 接口与实现，接口建议拆成读/写两个子接口）、`dump.go` |
| `podgroupmanager` | `manager.go`（`Manager` 与生命周期）、`crd.go`（CRD 监听与能力标志）、`podgroup.go`（PodGroup 构建与 CRUD） |
| `inplace` | `state.go`（State schema 与解码）、`legacy.go`（旧格式迁移，注明可删除版本）、`patch.go`（`Start`/`Complete`）、`eligibility.go`；State schema 与解码若要被 `utils` 复用，应放叶子包 |
| `webhook` | `eviction_handler.go`（`EvictionHandler`/`Handle`/`allow`/`deny`）、`eviction_policy.go`（`evaluateBudget`）、`eviction_tracker.go`（ConfigMap tracker、key 编解码、清理）、`validator.go`（`Handle` 与编排）、`validate_spec.go`（replicas/roles/gang/eviction strategy/recovery）、`validate_rolling_update.go`、`validate_coordination.go`（合并 `role_coordination_validator.go`）、`validate_update.go`（不可变校验）、`validate_pod_template.go`、`webhook_util.go` |
| `plugins` | `types.go`（`HookRequest`/`Plugin`/`BasePlugin`/`RoleCleanup`）、`registry.go`、`chain.go`、`headless_service.go` + `headless_service_identity.go`、`lws_labels_plugin.go`、`demo_plugin.go`、`child_plugin_preserve.go`（原 `legacy_child.go`）、`child_defaults.go` |

---

## 10. 复杂度、规范与测试代码

### 10.1 复杂度热点（C-K）

`gocyclo -over 20`（仅非测试文件）共 21 个函数，其中 controller 包 14 个。下表按复杂度排序，"关联"列指向本文中已有的详细条目。

| 编号 | 复杂度 | 函数 | 位置 | 拆分建议 | 关联 |
|---|---|---|---|---|---|
| C-K01 | 82 | `updateModelServingStatusInRound` | `model_serving_controller.go:2526`（约 270 行） | 同时做"按 group 汇总状态 / 计算副本计数 / 写 store / 更新 API / revision GC / 设置 condition"。拆成纯函数 `summarizeGroups`、`computeStatusCounts`、`decideConditions`，以及唯一有副作用的 `persistStatus`；ctx 一路传入 | C-R07、C-H09 |
| C-K02 | 44 | `resolveRoleRolloutState` | `role_rolling_update_coordinator.go:560` | **10 个参数**，改为输入结构体 `rolloutInput`；它是纯计算，应能脱离 controller 做表驱动测试 | C-H01 |
| C-K03 | 40 | `rolesToDeleteForRoleRollingUpdateInRound` | `model_serving_controller.go:1759` | 拆"选候选 / 排序 / 套预算 / 套协同约束"四步 | C-R04、C-S04、C-H08 |
| C-K04 | 38 | `scaleUpRoles` | `model_serving_controller.go:1090` | 拆"恢复路径（revision 选择）/ 常规创建 / 协同扩容分组" | C-R05 |
| C-K05 | 36 | `manageRoleReplicasPerGroup` | `model_serving_controller.go:1275` | 先补 C-R01 的回归测试，再拆"补缺 / 缩容 / 删除孤儿" | C-R01 |
| C-K06 | 35 | `calculateRoleRolloutLimits` | `role_rolling_update_coordinator.go:854` | 纯函数，抽到 `coordination` 层，配表驱动测试 | C-H03 |
| C-K07 | 33 | `checkRoleReadyInRound` | `model_serving_controller.go:2193` | 拆"Pod 就绪 / in-place 状态 / 协同观测" | — |
| C-K08 | 32 | `stableCapacityFillsOrphanSlot` | `auxiliary_cleanup.go:143` | 名字与用途要靠读完整个函数才明白，需补注释并拆分 | C-H11 |
| C-K09 | 30 | `utils.RecordModelServingRevision` | `controller_revision_lifecycle.go:38` | 见 U-02（生产不可达，可随 U-01 一起处理） | U-01/U-02 |
| C-K10 | 29 | `utils.CoordinatedPartitionConflict` | `coordination_partition.go:32` | 见 U-22 / U-H10 | U-22 |
| C-K11 | 28 | `webhook.validateRoleCoordination` | `role_coordination_validator.go:104` | 见 W-33 | W-33 |
| C-K12 | 28 | `resolveRoleRolloutPolicy` | `role_rolling_update_coordinator.go:732` | 拆"默认值解析 / 合法性判定 / 百分比换算" | — |
| C-K13 | 27 | `plugins.normalizeChildPodSchemaDefaults` | `legacy_child.go:145` | 见 P-03 | P-03 |
| C-K14 | 26 | `manageRollingUpdateWithPolicy` | `model_serving_controller.go:1551` | 与 C-K03 一并处理 | C-S04 |
| C-K15 | 25 | `createPod` | `model_serving_controller.go:3114` | 拆"构造 Pod / 注入 plugin / 创建 / 写 store" | C-R03 |
| C-K16 | 24 | `webhook.validateMaxUnavailableForRoles` | `validator.go:265` | 见 W-20 | W-20 |
| C-K17 | 24 | `stageCoordinatedExpansionGang` | `role_coordinated_expansion.go:82` | 拆"分组 / 选取 / 创建" | — |
| C-K18 | 22 | `retireCoordinatedSurge` | `coordinated_role_observation.go:384` | — | — |
| C-K19 | 22 | `reconcileInPlaceUnit` | `inplace_update.go:445` | 拆"判定阶段 / 超时 / 回滚 / 完成" | C-H10 |
| C-K20 | 21 | `plugins.isManagedHeadlessService` | `headless_service_plugin.go:259` | 见 P-08 | P-08 |
| C-K21 | 21 | `(*HeadlessServicePlugin).ensureService` | `headless_service_plugin.go:182` | 见 P-07 | P-07 |

处理原则：**先写（或补全）特征测试，再拆函数**；纯计算类（C-K02、C-K06、C-K10、C-K12）优先抽成无 receiver 的纯函数，副作用类（C-K01、C-K04、C-K05、C-K15）拆成"决策 + 执行"两段，决策部分可独立单测。

### 10.2 风格与规范（C-N）

| 编号 | 级别 | 位置 | 问题 | 建议 |
|---|---|---|---|---|
| C-N01 | P3 | `coordinated_round.go:160,169,182`、`model_serving_controller.go:2250,3076`、`pod_deletion_targets.go:51,70`、`role_coordinated_expansion.go:66`；plugins 见 P-11/P-12 | 错误串首字母大写（staticcheck ST1005） | 统一小写，不以标点结尾 |
| C-N02 | P3 | `model_serving_controller.go:112,185` | 使用已弃用的 `workqueue.RateLimitingInterface`（112）与 `workqueue.DefaultControllerRateLimiter`（185），均为 SA1019；112 行用 `// nolint` 掩盖了告警 | 迁移到 `TypedRateLimitingInterface` / `DefaultTypedControllerRateLimiter`，去掉 `// nolint` |
| C-N03 | P3 | `model_serving_controller.go` | `fmt.Errorf` 中 `%v` 40 处、`%w` 19 处，错误链被截断，`errors.Is/As` 无法穿透 | 统一用 `%w`（不需要透传时才用 `%v`），同 C-H16 |
| C-N04 | P2 | `model_serving_controller.go` 等 | `context.TODO()` / `context.Background()` 共 18 处，在已有 ctx 的调用链内重新创建上下文，取消与超时无法传播（C-R07、U-09、G-10 都是实例） | 把 ctx 作为首参贯穿，`Xxx` 兼容包装删除后自然消除 |
| C-N05 | P3 | `binpack_scaledown.go` | 文件名叫 bin-pack，内容却是"缩容优先级打分"（状态优先级 > deletion cost > 序号）；导出了 `ServingGroupWithScore`、`Priority*` 常量，**包内无外部使用却导出**；常量 5 个取值 0、1 实际只表示布尔（"是否优先删除"），却展开成 10 个命名常量 | 重命名 `scaledown_priority.go`；类型与常量改为未导出；优先级改成 `bool` 或枚举 |
| C-N06 | P3 | 全目录 | `workload/v1alpha1` 的导入别名不统一：`workloadv1alpha1`（24 个文件）、`workload`（4）、`api`（4），同一包内混用 | 统一为 `workloadv1alpha1`，加 `importas` lint 规则，同 C-H17 |
| C-N07 | P3 | `lws_controller.go:120,130` | `UpdateFunc: func(old, new interface{})` 遮蔽内建 `new` | 改 `oldObj, newObj` |
| C-N08 | P2 | `lws_controller.go:294` | `object.GetSelfLink()` 在 Kubernetes 1.16 起弃用，1.20 起默认不再填充（1.24 起不可再开启），日志里的对象标识会是空串 | 改 `object.GetNamespace()+"/"+object.GetName()` |
| C-N09 | P3 | `lws_controller.go` constructModelServing | 局部变量 `roleReplicas := int32(1)` 遮蔽包级函数 `roleReplicas` | 改名，同 C-H13 |
| C-N10 | P3 | `pkg/model-serving-controller/README.md` | 空文件，目录没有任何模块说明 | 写一页模块地图（对应 11.3 的目标布局）和关键不变量（UID 围栏、ordinal 顺序契约、revision 机制） |
| C-N11 | P3 | `podgroupmanager/manager.go:246`、`model_serving_controller.go:76` | `error` 不在返回值末尾（ST1008） | 见 G-03 |

### 10.3 测试代码（C-Z）

controller 包共 20 个测试文件、16560 行、209 个顶层 `Test*` 函数（`^func Test` 统计，不含子测试）。

| 编号 | 级别 | 位置 | 问题 | 建议 |
|---|---|---|---|---|
| C-Z01 | P1 | `model_serving_controller_test.go`（**9731 行**，77 个 Test） | 所有主流程测试堆在一个文件，与"上帝文件"互为镜像；任何改动都难 review，冲突概率高 | 按 11.3 的源文件布局拆分：`scale_*_test.go`、`rollout_*_test.go`、`status_test.go`、`pod_lifecycle_test.go`、`delete_test.go`、`revision_test.go`，共用夹具放 `testutil_test.go` |
| C-Z02 | P2 | `merge_ready_regression_test.go`（423 行，7 个 Test）、`coordinated_inplace_regression_test.go`（357）、`coordinated_role_regression_test.go`（283，10 个 Test）、`coordinated_expansion_regression_test.go`（255） | 文件按"出现的原因"（merge ready / regression）命名而不是按被测特性命名；`merge_ready_regression_test.go` 里测的其实是协同滚动的读放大、scale-to-zero、探测失败等多类行为 | 按特性重新归并到 `coordinated_*_test.go` |
| C-Z03 | P2 | 11 个文件：`coordinated_creation`、`coordinated_partial_creation`、`coordinated_expansion_regression`、`coordinated_inplace_regression`、`coordinated_role_regression`、`coordinated_role_fixture`、`coordinated_round`、`role_coordinated_expansion`、`role_coordination_boundaries`、`role_rolling_update_coordinator`、`merge_ready_regression` | 协同滚动的测试分散在 11 个文件，命名前缀有 `coordinated_` / `role_coordinat*` / `role_rolling_update_` 三种，对应 C-S02 的源文件散乱 | 与源文件一起合并为 `coordination_*_test.go` |
| C-Z04 | P3 | `lws_controller_test.go`（509 行，2 个 Test）、`lws_controller_legacy_test.go`（113 行，1 个 Test） | 同一被测对象两个测试文件，后者以 "legacy" 命名但内容仍在测当前 controller | 合并；随 C-S05 一起搬到 `lws/` 包 |
| C-Z05 | P2 | `model_serving_controller_test.go:1878` | `time.Sleep(100 * time.Millisecond)` 等待"controller 处理删除"——但前面调用的 `controller.deletePod(pod)` 是同步的，这个 sleep 看起来没有等待对象；`AGENTS.md` 明确禁止用任意 sleep 掩盖时序问题 | 删除或改成 `wait.PollUntilContextTimeout` 等待可观察条件 |
| C-Z06 | P3 | `model_serving_controller_test.go:137`、`model_serving_controller_harness_test.go:119` | 两处 `time.Sleep(10ms)` 位于有 deadline 的轮询循环内，属于受限轮询，可接受，但自己手写了轮询 | 换成 `wait.PollUntilContextTimeout`，与其它测试一致 |
| C-Z07 | P1 | 见第 3、5 章 | 测试在维持死代码与测试专用分支存活：C-D01..D11 的 `XxxInRound` 兼容包装、`CreatePodsByRole`（7 处测试调用）、`RecordModelServingRevision`、`utils.SetCondition`；单测对象不带 `ResourceVersion`/UID 导致生产代码写了 C-T01、W-14 的分支 | 先让测试直接调用生产入口、补齐夹具字段，再删除这些分支和包装 |
| C-Z08 | P3 | `utils/test/helper.go`、`webhook/pod_template_validation_test.go:42` | 测试夹具放在非 `_test` 包（U-25）；测试使用已弃用的 `ModelBooster`（SA1019） | 迁 `testutil`；改用新类型 |

---

## 11. 整改路线

### 11.1 原则

1. **行为与结构分开改**：搬文件的 PR 不改逻辑，改逻辑的 PR 不搬文件，便于 review 与回滚。
2. **先有测试再重构**：有现成测试保护的区域（如 `validator.go` 有 1747 行测试）优先；没有覆盖的（eviction 失败路径，C-R01）先补特征测试。
3. **兼容性**：CRD 字段、HTTP 路由、CLI 参数不变；`ControllerRevision` 与 in-place annotation 的**线上格式**不变（I-06：旧格式迁移代码已发布在 v1.1.0-rc.0，不能直接删）。
4. **小步**：每个 PR 只处理一类问题，且不超过一个包。
5. 遵守 `AGENTS.md`：改 API 或 marker 后跑 `make generate`；不为通过 CI 而弱化断言、加 sleep 或跳过测试。

### 11.2 分阶段计划

| 阶段 | 内容 | 涉及编号 | 风险 | 预期收益 |
|---|---|---|---|---|
| **阶段 0 正确性** | 修复可能 panic / 阻塞 / 重试遗漏的问题，每个问题配回归测试 | C-R01、C-R02、C-R03、C-R04；G-01、G-02；W-15、P-18、C-N08 | 中（行为改变，需 e2e） | 消除 panic 与锁风险 |
| **阶段 1 清理死代码** | 删除零引用函数；把"仅测试"的包装下沉到 `_test.go` 或直接让测试调用生产入口；修正误导性注释 | C-D01..D11、U-12、U-13、G-05、I-01、G-04、W-10/W-13、U-01 的"误导注释"部分 | 低 | 删除量最直观，且为阶段 2、3 清除障碍 |
| **阶段 2 去测试专用分支** | 修测试夹具（补 `ResourceVersion`、UID、informer），删除 13 处 `ResourceVersion != ""` 判断及相关 nil 防御 | C-T01、C-T02、C-T03、W-14、C-Z07 | 中（测试需大量调整） | 生产代码不再知道自己是否在测试里 |
| **阶段 3 纯搬移的文件重组** | 在 `package controller` 内按 11.3 重组文件，不改任何逻辑；同步拆分测试 | C-S01..S06、C-N05、C-Z01..Z04 | 低（可用 `git diff --color-moved` 验证） | 3523 行 → 多个 300–600 行的文件 |
| **阶段 4 抽取协同滚动纯逻辑** | 把 `roleStablePopulation`、`calculateRoleRolloutLimits`、`resolveRoleRolloutState` 等纯计算抽成 `coordination_*.go`（或子包）；"被协调的 Role 集合"统一为一个实现，供 webhook/controller/utils 共用；LWS controller 搬到 `lws/` 包 | C-S02、C-S05、C-X*、W-34、U-21、U-22 | 中 | 消除 4 处重复实现，纯逻辑可表驱动测试 |
| **阶段 5 拆大函数** | 按 10.1 的顺序拆分，从纯计算到有副作用 | C-K01..C-K21、C-R07 | 中高 | 复杂度降到 <20 |
| **阶段 6 子包清理** | utils 拆文件与去重；datastore 统一错误语义与顺序契约；podgroupmanager 拆文件；inplace 收敛 API；webhook 合并校验循环；plugins 拆 `ensureService` | U-*、D-*、G-*、I-*、W-*、P-* | 低–中 | — |
| **阶段 7 规范收尾** | ST1005、`%w`、弃用 API 迁移、import 别名、`README.md`、`importas` lint 规则 | C-N*、U-08、P-11、P-12 | 低 | `make lint` 干净 |

阶段 0 与阶段 1 互不依赖，可并行；阶段 2 依赖阶段 1（先删包装，才能去掉对应的分支）；阶段 3 应在阶段 4、5 之前完成；阶段 6 与阶段 3 之后的各阶段互不阻塞。

### 11.3 `controller` 包目标布局

保持 `package controller` 不变，按职责拆分文件（行数为估计）：

| 目标文件 | 内容 | 来源 |
|---|---|---|
| `controller.go` | 结构体、构造函数、`Run`/`worker`/`processNextWorkItem`、常量 | `model_serving_controller.go:65-265,666-` |
| `handlers_modelserving.go` | ModelServing 的 add/update/delete 事件处理 | 同上 |
| `handlers_pod.go` | Pod 的 add/update/delete 事件、`handleObject` | 同上 |
| `sync.go` | `syncModelServing`、reconcile 编排 | 同上 |
| `scale_servinggroup.go` / `scale_role.go` | ServingGroup 与 Role 的扩缩容（`scaleUpRoles`、`manageRoleReplicasPerGroup` …） | 同上 |
| `rollout_servinggroup.go` / `rollout_role.go` | 滚动更新（`manageRollingUpdateWithPolicy`、`rolesToDeleteForRoleRollingUpdateInRound`、`outdatedRolesInRound`） | 同上 |
| `pod_create.go` / `pod_lifecycle.go` | Pod 创建（`createPod`、`createPodsByRole`）；grace 删除、readiness | 同上 |
| `delete.go` | `deleteServingGroup`、`deleteRole`；并入 `auxiliary_cleanup.go`、`pod_deletion_targets.go` | 同上 + 现有两个文件 |
| `status.go` | `updateModelServingStatusInRound` 及其拆分结果 | 同上 |
| `revision.go` | revision 解析、`historical_plugin_context.go` | 同上 + `historical_plugin_context.go` |
| `coordination_*.go` | `coordination_policy.go`、`coordination_state.go`（原 `resolveRoleRolloutState`）、`coordination_limits.go`、`coordination_observation.go`、`coordination_round.go`、`coordination_expansion.go` | 现有 4 个协同文件 |
| `inplace_update.go` | 保持；其中纯判定移到 `inplace/` 包 | 现有 |
| `scaledown_priority.go` | 缩容优先级打分（C-N05） | `binpack_scaledown.go` |
| `debug.go` | debug endpoint 与 dump | 同 `model_serving_controller.go` |
| `lws/`（子包） | `LWSController` 与其测试、常量 | `lws_controller.go` |

测试文件按同样的布局拆分；夹具集中到 `testutil_test.go`（含现有 `model_serving_controller_harness_test.go`、`coordinated_role_fixture_test.go`）。

**该布局之后的收益**：每个文件对应一个职责，新增特性时能定位到明确的落点；"XxxInRound / Xxx"双层包装在去掉兼容包装后自然消失，ctx 与 round 缓存只有一种传递方式。

### 11.4 需要维护者先做的决策

| 编号 | 决策 | 选项 |
|---|---|---|
| U-01 / U-03 | v1 ControllerRevision 机制 | A）删除整套 v1 写入与读取分支（连同测试）；B）真正接入写路径并统一两套 "ModelServing at revision" |
| W-01 | eviction 失败策略 | fail-closed 还是 fail-open；与 `failurePolicy` 的关系 |
| W-02 | `AllowEviction` | 删除，还是改为包级函数后保留 |
| W-03 | `DryRun` | 是否同意 dry-run 不占用 disruption 预算 |
| W-18 | `k8s.io/kubernetes` 内部包依赖 | 接受并文档化，还是改为 CEL/最小校验 |
| P-23 | demo 插件 | 是否仍需注册到 `DefaultRegistry` |
| P-H9 | `kubebuilder:default=All` 缺 `+` | 是否修正（会引入 CRD 变化，需 `make generate` 且需兼容性评估） |
| I-06 | 旧格式 in-place state 迁移代码的移除版本 | 需要约定下限版本 |
| C-N05 | `ServingGroupWithScore`、`Priority*` 是否被外部使用 | 不再导出前需全仓确认 |

### 11.5 每个 PR 的验证清单

```bash
make fmt
make lint
go test -race ./pkg/model-serving-controller/...
make test                      # 会先执行 generate
make gen-check                 # 仅当改了 API 类型或 kubebuilder marker
make test-e2e-controller-manager   # 涉及滚动更新/协同/in-place/删除路径的改动必须跑
```

- 纯搬移 PR：额外用 `git diff --color-moved=dimmed-zebra` 确认逻辑未变；`go test -count=1 ./pkg/model-serving-controller/...` 的 `Test*` 数量不应减少（controller 包目前 209 个顶层 Test 函数）。
- 删除死代码的 PR：在 PR 描述里附 `deadcode ./cmd/kthena-controller-manager` 的前后差异。
- 修复 C-R01/R02/R03/R04、G-01/G-02 的 PR：必须新增失败在修复前、通过在修复后的回归测试。

---

## 附录 A. 静态分析原始结果

复现命令（工具安装在仓库之外）：

```bash
go install golang.org/x/tools/cmd/deadcode@latest
go install honnef.co/go/tools/cmd/staticcheck@latest
go install github.com/fzipp/gocyclo/cmd/gocyclo@latest

go vet ./pkg/model-serving-controller/...
staticcheck -checks 'all,-ST1000,-ST1003,-ST1016,-ST1020,-ST1021,-ST1022' ./pkg/model-serving-controller/...
deadcode ./cmd/kthena-controller-manager | grep model-serving-controller
gocyclo -over 20 $(find pkg/model-serving-controller -name '*.go' ! -name '*_test.go')
```

### A.1 `deadcode`（共 26 项）

| 包 | 符号 |
|---|---|
| `controller`（10） | `refreshCoordinatedAvailability`、`isServingGroupOutdated`、`findOutdatedRolesInServingGroups`、`hasUpdateableOutdatedRole`、`outdatedRoles`、`rolesToDeleteForRoleRollingUpdate`、`manageRollingUpdate`、`coordinatedPodGroupLayout`、`stableRolePopulation`、`roleSpecsFromRevision` |
| `inplace`（3） | `InProgress`、`Completed`、`OnTarget` |
| `podgroupmanager`（1） | `calculateRequiredRoleNames` |
| `utils`（12） | `RecordModelServingRevision`、`controllerRevisionLess`、`updateControllerRevision`、`copyInt32`、`modelServingCollisionCount`、`RevisionDataHash`、`CreateHeadlessService`、`CheckPodRevision`、`ExpectedPodNum`、`ContainerRestarted`、`SetCondition`、`SetConditionWithRolloutState` |

`deadcode` 不报导出方法，因此 `controller.CreatePodsByRole`（C-D11）与 `datastore.GetRunningPodNumByServingGroup`（D-08）需要用 grep 另行确认，二者在生产代码中均无调用。

### A.2 `staticcheck`（非测试文件，共 16 项）

| 编码 | 位置 |
|---|---|
| ST1005 错误串大写（13） | `controller/coordinated_round.go:160,169,182`、`controller/model_serving_controller.go:2250,3076`、`controller/pod_deletion_targets.go:51,70`、`controller/role_coordinated_expansion.go:66`（controller 8 项）；`plugins/headless_service_plugin.go:115,143,187,223,226`（plugins 5 项） |
| SA1019 弃用 API（2） | `controller/model_serving_controller.go:112`（`workqueue.RateLimitingInterface`）、`:185`（`DefaultControllerRateLimiter`） |
| ST1008 error 非末位（1） | `podgroupmanager/manager.go:246` |

> 备注：13 + 2 + 1 = 16 项。此外 `webhook/pod_template_validation_test.go:42` 有一处测试文件内的 SA1019（`ModelBooster`）。无 U1000（未使用的非导出标识符）告警，`go vet` 无告警。

### A.3 `gocyclo -over 20`

见 10.1 的表（21 项）。



