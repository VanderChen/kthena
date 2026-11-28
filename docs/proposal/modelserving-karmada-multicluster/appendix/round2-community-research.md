# ServingGroup CR 与多集群调研

**截至 2026-11-28。**源码基线：Karmada [`6d007132`](https://github.com/karmada-io/karmada/commit/6d007132ec5112d2eed41b602d4aeeebaf96f4fc)、kthena [`1a5145e9`](https://github.com/volcano-sh/kthena/commit/1a5145e90e0e1d70c75def0ef9500055e2904c5f)。

## 摘要

- **G 更贴近 Karmada 当前的多组件调度模型**：一个传播对象作为一个完整组件集，放入一个集群；但这不等于维护者已推荐 kthena 新增 ServingGroup CR。
- **M 更贴近当前 kthena API 和迁移路径**，但把多 Pod 的整组资源求和当成单 Pod 副本，会造成节点级估算失真。
- G 的优势有明确前提：正确配置单集群 spread 和容量感知策略、实现组件及健康解释器，并解决控制面父子控制器、升级、状态和故障语义。
- 两种方案都没有现成保证：**只有明确不可调度的组才跨集群迁移；模型加载中的组不迁移；无目标容量时不丢失已有服务**。
- 最重要的跨项目先例是 RBG 的 `RoleBasedGroupSet → RoleBasedGroup` CR 层级，但找到的是**集群内生命周期设计**，不是控制面逐组跨集群放置的先例。

## A. Karmada 社区与产品证据

### 1. 多组件调度的方向与成熟度

**设计目标。**多 Pod 模板提案明确将“一个 CR 的所有组件放在同一集群”作为约束；跨集群拆分组件是非目标，需要在调度前拆分。估算器按组件需求尝试完整集合，不保证找到最优装箱。该设计支持 G 的*放置形状*，但没有要求必须新建 kthena CR。[`karmada-io/karmada:docs/proposals/scheduling/multi-podtemplate-support/multiple-pod-template-support.md:27-40,54-58`](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/docs/proposals/scheduling/multi-podtemplate-support/multiple-pod-template-support.md#L27-L58)

**版本差异。**feature gate 源码标注 Alpha 自 v1.15；v1.15 首阶段主要完成组件解析和 quota 统计，**完整资源感知调度 MVP 从 v1.16 起**。当前基线仍为 Alpha、默认关闭；v1.19 文档要求在 controller-manager、scheduler、webhook 均启用。[`features.go:118-126,186-195`](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/features/features.go#L118-L126)；[阶段 I #6641](https://github.com/karmada-io/karmada/issues/6641)，[阶段 II #6734](https://github.com/karmada-io/karmada/issues/6734)，[v1.19 用户文档](https://karmada.io/docs/userguide/scheduling/multi-component-scheduling/，2026-09-04)。

**实际策略限制。**仅启用 `GetComponents` 不足以保证“一组、一个有容量的集群”：

- 当前源码要求 `spreadByField: cluster` 且 `minGroups=maxGroups=1` 才走组件集合估算；仅凭 cluster affinity 即使恰好匹配一个集群，也不会满足该判断。[`estimation.go:35-65`](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/scheduler/core/estimation.go#L35-L65)
- 无 spread 时选择所有候选集群；默认或 `Duplicated` 策略忽略容量阈值。上游 Flink E2E 使用单集群 spread 加 `Divided/Aggregated`，但多组件分支仍将对象作为整体，不做组件副本拆分。[`select_clusters.go:27-35,71-77`](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/scheduler/core/spreadconstraint/select_clusters.go#L27-L35)；[`common.go:42-47,59-82`](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/scheduler/core/common.go#L42-L82)；[`schedule_multi_template_test.go:156-177,212-225`](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/test/e2e/suites/base/schedule_multi_template_test.go#L156-L177)。

**Beta 尚无已确认日期或完整毕业清单。**[阶段 V #7895](https://github.com/karmada-io/karmada/issues/7895)，2026-11-26：多组件扩缩容进入重调度已完成；增量估算、纯缩容跳过估算、失败时阻止新配置下发仍未完成。zhzhuang-zju 提议组件级 revision hook；RainbowMango 表示倾向同意，但“可能不在本 release”。这不是已承诺的 Beta 条件或时间表。[评论](https://github.com/karmada-io/karmada/issues/7895#issuecomment-7094291631)、[回复](https://github.com/karmada-io/karmada/issues/7895#issuecomment-7187058571)。

**组件集合副本跨集群拆分。**[总跟踪 #5115](https://github.com/karmada-io/karmada/issues/5115) 的 2024-07-18 [评论](https://github.com/karmada-io/karmada/issues/5115#issuecomment-2235044073)提出 replica division 是关键问题；所读全部评论未给出针对“多个完整组件集分别分配到不同集群”的明确方案或维护者承诺。[#7895](https://github.com/karmada-io/karmada/issues/7895) 的“replica set estimation”讨论的是**单个多组件对象内部扩缩容**，不能当作集合跨集群拆分承诺。现有 [`common.go:42-47`](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/scheduler/core/common.go#L42-L47) 明确不支持 replica division。

### 2. 单集群多角色推理与 best effort

[#8030](https://github.com/karmada-io/karmada/issues/8030)，2026-11-06，提出多角色解耦推理必须留在一个成员集群；当前估算不足时，联邦层不传播，成员侧碎片整理器因此看不到 pending 需求。作者建议可选地优先选择完全可容纳的集群，否则选择较合适的集群，并保留**完整期望数量**交给成员侧整理。它明确将跨集群 replica division 排除为另一问题。[#8030 全文及唯一评论](https://github.com/karmada-io/karmada/issues/8030#issuecomment-6726915132)。

该评论仅表示愿意参与，**没有维护者接受或拒绝该设计的意见**。它是一个完整多组件对象对应一个 binding 的相关用例，但并非 ServingGroup CR 产品先例。它也不同于 [#6333](https://github.com/karmada-io/karmada/issues/6333)、[#7116](https://github.com/karmada-io/karmada/issues/7116)：后两者讨论先分配可容纳的**部分副本**，而 #8030 要求把完整需求送至一个成员集群。

### 3. 故障转移、慢启动与过量分配

- **Application failover** 依赖 `InterpretHealth`、依赖传播和持续 unhealthy 的 `decisionConditions.tolerationSeconds`，默认 300 秒；并非内建的 `PodScheduled=False/Unschedulable` 专用触发器。自定义健康解释器可以选择如何表达健康，但须防止把加载模型误判为容量故障。[v1.19 文档](https://karmada.io/docs/userguide/failover/application-failover/，2026-09-04)；[`propagation_types.go:303-311,323-348,397-406`](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/apis/policy/v1alpha1/propagation_types.go#L303-L348)。
- **版本陷阱：**上述 v1.19 网页仍写 `Immediately/Graciously`；当前源码只接受 `Directly/Gracefully/Never`。旧值由 [#7792](https://github.com/karmada-io/karmada/pull/7792) 于 2026-08-04、commit `a503a61d` 移除。`Gracefully` 等新端健康**或超时**后删除旧端；`Never` 要人工清理，不是无条件的零中断保证。[`propagation_types.go:279-300,332-345`](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/apis/policy/v1alpha1/propagation_types.go#L279-L300)。
- 有状态保持自 v1.12 支持从旧对象状态抽取字段并注入标签；不自动搬运 checkpoint，文档要求单集群迁移、`StatefulFailoverInjection` Alpha gate，并仅在立即删除模式注入。[文档](https://karmada.io/docs/userguide/failover/application-failover/#application-state-preservation，v1.19)。
- **慢加载：**[#6233](https://github.com/karmada-io/karmada/issues/6233)，2025-03-26，要求独立的初始化容忍时间，避免只能用一个大 `tolerationSeconds` 兼顾初始化与故障。它仍开放。[#7621 评论](https://github.com/karmada-io/karmada/issues/7621#issuecomment-6307463706)，2026-10-21，另一 AI 推理用户要求 GPU 成本驱动的重平衡同时等待新端 ready；维护者指向仍在评审的 [#7662](https://github.com/karmada-io/karmada/pull/7662)。
- **状态抖动：**[#7431](https://github.com/karmada-io/karmada/issues/7431) 报告 `Unhealthy → Unknown` 会重置计时，可能长期延迟 failover。维护者解释这是出于迁移谨慎的**预期行为**；issue 已关闭，改变行为的 [#7432](https://github.com/karmada-io/karmada/pull/7432) 仍未合并。基线源码确实将 Unknown 放入清计时集合。[维护者回复](https://github.com/karmada-io/karmada/issues/7431#issuecomment-4326682763)；[`common.go:88-103,108-123`](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/controllers/applicationfailover/common.go#L88-L123)。
- **无目标容量：**[#7455](https://github.com/karmada-io/karmada/issues/7455) 报告 graceful eviction 在无替代目标时可能删除原端；维护者认为超时删除是既定 fallback，需要更多用户反馈；修复 PR [#7472](https://github.com/karmada-io/karmada/pull/7472) 已关闭未合并。源码明确超时可结束任务，空目标健康判断也需结合实际 `hasScheduled` 状态验证，不能直接断言每次都会提前删除。[评论](https://github.com/karmada-io/karmada/issues/7455#issuecomment-4427094831)；[`evictiontask.go:67-86,92-115`](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/controllers/gracefuleviction/evictiontask.go#L67-L115)。
- **Unschedulable 请求：**[#5987](https://github.com/karmada-io/karmada/issues/5987)，2024-12-27，要求 FlinkDeployment 在 Pod 不可调度时由 descheduler 重调度；维护者建议考虑 application failover，作者区分了二者的 unhealthy 与 unschedulable 语义。[讨论](https://github.com/karmada-io/karmada/issues/5987#issuecomment-2572037845)。[#8104](https://github.com/karmada-io/karmada/issues/8104) 对 kthena 提出同类组级需求。检索 Karmada issue 的 `failover`、`unschedulable`、`PodScheduled`、`decisionConditions`，**未找到已接受的独立 unschedulable application-failover 触发器提案**。
- **SchedulingOvercommitProtection** 通过 scheduler 的内存 assumption 和 estimator 扣减，缓解多个连续 binding 在 Pod 绑定节点之前重复占用同一快照容量；Alpha、默认关，scheduler 和 estimator 均需启用。重启丢失 assumption；慢健康可能双重扣减，五分钟 TTL 是后备而非可配置保证。[提案 `README.md:17-27,58-76`](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/docs/proposals/scheduling/estimator-reservation/README.md#L17-L76)；[v1.19 文档](https://karmada.io/docs/userguide/scheduling/scheduling-overcommit-protection/)。
- 文档将其适用范围限定为容量感知的 `Divided`；当前源码也能记录多组件 assumption，并传入组件估算请求，但**不能据此宣称 G 的任意策略均受保护**，应按单集群 `Divided/Aggregated` 路径实测。[`scheduler.go:712-765`](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/scheduler/scheduler.go#L712-L765)；[`util.go:75-97,112-117`](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/scheduler/core/util.go#L75-L117)。

### 4. 用户案例、LWS 与规模

- Karmada [adopters](https://karmada.io/adopters/) 页面称阿里 Qwen App 使用它跨区域调度和安全重平衡弹性 AI 工作负载；Bloomberg 用于训练集群及 GPU/CPU 工作负载。页面**未说明逐 ServingGroup binding**。[`ADOPTERS.md:1-7`](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/ADOPTERS.md#L1-L7)。
- 2026-03-26 KubeCon EU 官方议程中，Bloomberg 与 Anyscale 分享 Karmada + KubeRay `RayService` 的多集群推理蓝图，涉及 placement、spreading 和 failover；议程本身不证明 G 的对象层级或某个生产配置。[官方议程](https://kccnceu2026.sched.com/event/2CW5v/achieving-resilient-multi-cluster-ai-inference-on-kubernetes-with-karmada-and-kuberay-wei-cheng-lai-bloomberg-han-ju-chen-anyscale)。
- 在 `karmada-io/karmada` issue/PR 中搜索 `LeaderWorkerSet` 和 `"leader worker set"` 均无结果；基线内置第三方解释器目录及提案搜索也未找到 LWS。**这是检索范围内的缺口，不是维护者反对支持的证据。**[#5115 评论](https://github.com/karmada-io/karmada/issues/5115) 中 RainbowMango 表示欢迎用户贡献更多内置解释器。
- [2022 CNCF 性能报告](https://www.cncf.io/blog/2022/11/29/support-for-100-large-scale-clusters/) 使用 Karmada `v1.3.0-4-g1f13ad97`、模拟节点，报告 100 集群、50 万节点、200 万 Pod；它不是 ServingGroup/RB 上限，更不是当前版本生产保证。
- [#7062](https://github.com/karmada-io/karmada/issues/7062)，2025-12-30，报告压力场景 12,645 RB、6,646 Work，RB→Work 落后约 6,000；约 200 Work/s 是报告中的估计，1,000+/s 是**期望**，相关 [#7063](https://github.com/karmada-io/karmada/pull/7063) 未合并，不能当成已实现吞吐。基线性能脚本默认 10 集群、每集群 10 模拟节点，用于测 P50/P90/P99 和吞吐，未提供 G 的容量上限。[`hack/performance/README.md:104-109,184-192`](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/hack/performance/README.md#L104-L109)。
- **推断：**M 通常一个 ModelServing 对应一个 RB、每目标集群一个 Work；G 通常每组一个 RB 和至少一个 Work，迁移/优雅驱逐还可能有临时额外对象。实际数量须计入依赖、策略与 rollout。

## B. 对比系统的放置单位

| 系统 | 单位、父子关系和容量 | 无法运行时 |
|---|---|---|
| **Kueue 单集群 LWS** | 每 LWS replica/group 一个 `Workload`，按整组准入；扩容组先被 scheduling gate 阻塞，quota 准入后释放；可选 TAS 约束每组拓扑。LWS 是父对象，但这不是多集群逐组放置。[文档，2026-05-21](https://kueue.sigs.k8s.io/docs/tasks/run/leaderworkerset/) | quota 或指定拓扑不足时等待准入；不能据此推断自动跨集群迁移。 |
| **MultiKueue LWS** | manager 上每 replica 一个 Workload，**但全部 Workload 原子地跟随 primary 到同一个 worker cluster**；同步后在该 worker 创建 LWS。它不是 G 的“各组独立选集群”先例。LWS 无 `managedBy`，manager 的 LWS 状态不可靠。[`kubernetes-sigs/kueue:site/content/en/docs/tasks/run/multikueue/leaderworkerset.md:14-32`](https://github.com/kubernetes-sigs/kueue/blob/fe078e797c828dcbb87943da05b7f9cf727ceedc/site/content/en/docs/tasks/run/multikueue/leaderworkerset.md#L14-L32)，文档 2026-02-02。 | manager quota reservation 后向 worker 投递；worker 准入胜出，其他候选副本删除。未准入前等待；文档没有承诺已放置后因 Pod unschedulable 逐组跨集群重放。[MultiKueue，Beta 自 v0.9](https://kueue.sigs.k8s.io/docs/concepts/multikueue/)，2026-08-19。 |
| **MultiKueue JobSet** | JobSet 作为整体 job/Workload 投递一个 worker；manager 的 `managedBy` 默认交给 MultiKueue。资源计算包含 replicatedJobs、parallelism 和 replicas，不是父对象拆成跨集群子组。[JobSet 文档](https://kueue.sigs.k8s.io/docs/tasks/run/multikueue/jobsets/)，2024-11-07；[资源计算](https://kueue.sigs.k8s.io/docs/tasks/run/jobsets/)。 | worker quota 不准入则等待候选准入；不能推断任意失败都自动换集群。 |
| **Volcano Global** | 用户提交 Volcano Job；hub 的 webhook 暂停其 RB，controller 按队列/Job 优先级、公平性和资源 admission 排队，恢复 RB 后交给 Karmada。成员仍运行 Volcano。项目自称 Alpha，**不是 ModelServing 分组实现**。[架构文档](https://volcano.sh/docs/keyfeatures/multiclusterscheduling/)，2026-05-26；[`volcano-sh/volcano-global:README.md:3-13`](https://github.com/volcano-sh/volcano-global/blob/bc14015ed014e5109af993da712ffe703df72ac3/README.md#L3-L13)。 | 未通过 admission 的 RB 保持暂停等待；该文档没有证明已运行 Job 的自动跨集群迁移。 |
| **OCM** | `Placement` 选择集群并产生 `PlacementDecision`；`ManifestWorkReplicaSet` 基于该决定为选中集群生成 ManifestWork，主要是模板复制，不是原生每推理组拆分。v0.16 文档将后者标为 Alpha。Placement 可按 CPU/内存等集群分数排序，但未证明逐组 Pod 拓扑装箱。[Placement](https://v0-16.open-cluster-management.io/docs/concepts/content-placement/placement/)；[ManifestWorkReplicaSet](https://v0-16.open-cluster-management.io/docs/concepts/work-distribution/manifestworkreplicaset/)，2025-01-02。 | 集群 unreachable 可结合 taint 和 toleration 延迟改变决策；文档未承诺应用级 unschedulable 迁移。 |
| **KubeAdmiral** | 支持对象复制或按静态/动态权重拆 replicas；动态权重使用集群剩余资源。这是 M 类控制面模型，并非已验证多模板组级估算。[策略文档](https://kubeadmiral.io/docs/userguide/propagation-policy/)，2024-08-21。 | 文档支持失败后 replica 重分配及避免扰动，但未建立 ServingGroup 级故障语义。 |
| **Armada** | 独立控制面排队；一个 Job 对应一个 Pod，多个 Job 可按 gang 原子准入。gang **默认可以跨 worker 集群**；需集群唯一的 node uniformity label 才限制同集群。不是父 CR 分发子 CR。[Gang 文档](https://armadaproject.io/docs/gang-jobs)，基线 [`13390eaf`](https://github.com/armadaproject/armada/commit/13390eaff708466af2e3d773565171295334005d)，2026-11-26。 | 整 gang 放不下则等待；gang fail-fast、不可自动重试；一成员被抢占则整 gang 停止，但程序崩溃不自动停止其他成员。 |
| **RBG** | `RoleBasedGroupSet.spec.replicas` 创建多个 `RoleBasedGroup` 子 CR；每个 RBG 含多个角色，可配置 group gang 和生命周期。这是 G 的**对象层级**最直接先例，但源码/文档描述集群内控制器，不证明控制面逐组跨集群放置。[`sgl-project/rbg:api/workloads/v1alpha2/rolebasedgroupset_types.go:39-53,60-69`](https://github.com/sgl-project/rbg/blob/6f8dcd25a962a06b0cbd7ee7a3d436e6218adf69/api/workloads/v1alpha2/rolebasedgroupset_types.go#L39-L69)；[`doc/features/gang-scheduling.md:1-12,48-53`](https://github.com/sgl-project/rbg/blob/6f8dcd25a962a06b0cbd7ee7a3d436e6218adf69/doc/features/gang-scheduling.md#L1-L53)，2026-11-27 基线。 | gang 放不下时等待；角色实例恢复策略覆盖指定失败，但 Pending/NotReady 本身不是重建触发器。[`failure-handling.md:3-13,27-39`](https://github.com/sgl-project/rbg/blob/6f8dcd25a962a06b0cbd7ee7a3d436e6218adf69/doc/features/failure-handling.md#L3-L39)。 |
| **NVIDIA Grove** | `PodCliqueSet` 描述整套多组件，`PodCliqueScalingGroup` 和 `PodGang` 表示内部共同扩缩与 gang；目标是组级生命周期、拓扑和启动顺序，而非已证明多集群 federation。[`ai-dynamo/grove:README.md:46-64`](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/README.md#L46-L64)，2026-11-26；[NVIDIA，2025-11-10](https://developer.nvidia.com/blog/streamline-complex-ai-inference-on-kubernetes-with-nvidia-grove/)。 | 依成员调度器的 gang/topology 和 Grove 恢复策略；未找到逐组跨集群失败转移承诺。 |
| **LWS / DisaggregatedSet** | LWS 将多 Pod group 作为 replica；DisaggregatedSet 管理多个角色 LWS。DS KEP **明确将多集群 federation 列为非目标**；其 Kueue 示例仅准入底层 LWS pod groups。[`kubernetes-sigs/lws:README.md:9-11,19-29,35-40`](https://github.com/kubernetes-sigs/lws/blob/6ec2d989beecd5825d9c0c85ae23c59c940adadf/README.md#L9-L40)；[`keps/766-DisaggregatedSet/README.md:41-75`](https://github.com/kubernetes-sigs/lws/blob/6ec2d989beecd5825d9c0c85ae23c59c940adadf/keps/766-DisaggregatedSet/README.md#L41-L75)，2026-11-27 基线。 | Kueue TAS 下角色不满足拓扑时等待；没有 DS 直接跨集群重调度保证。[开发文档，`1cb7505e`](https://lws.sigs.k8s.io/main/docs/examples/disaggregatedset/topology-aware-scheduling/)；该文档可能包含未发布功能。 |
| **KServe / llm-d** | KServe v0.21 的 `LLMInferenceService` 组合 P/D LWS 和请求级 EPP；EPP 是请求路由，不是跨集群 group placement。[KServe 架构](https://kserve.github.io/website/docs/concepts/architecture/control-plane-llmisvc)。llm-d 的开放 [fleet RFC #2380](https://github.com/llm-d/llm-d/issues/2380)，2026-08-26，讨论集群能力、健康和路由；[PR #2600](https://github.com/llm-d/llm-d/pull/2600) 为未合并实验性多集群路由指南。 | RFC 讨论无兼容容量时显式拒绝及健康 failover；没有证明逐 ServingGroup CR 的部署或迁移。 |

RBG、Grove、LWS 的公开 issue 中检索了 `multi-cluster`；未找到它们的组 CR **为多集群放置而引入或已用于每组一个跨集群 binding** 的明确声明。DS 的非目标则是正面证据，而非仅未搜索到。

## C. kthena 上游现状

- [#2148](https://github.com/volcano-sh/kthena/issues/2148)，2026-11-27，仍开放。作者比较 ServingGroup CR、父 status ledger 和未知的 Plan C：CR 提供组 UID、独立 status 和 GC，但需要 Pod owner 迁移、两控制器协调，且 Role 实例层仍需持久状态；ledger 有共享 `resourceVersion` 和写放大风险。文中原型测得 164 个组 1,573 次更新；ledger 的二次写放大是**估算，不是实测**。
- 已读 #2148 全部三条评论：作者邀请讨论；维护者 hzxuzhonghu 仅说 [#2126 修复部分场景](https://github.com/volcano-sh/kthena/issues/2148#issuecomment-7220729247)；作者确认已注意到。**没有维护者选择 CR 或 ledger，也没有将 CR 与跨集群 placement 联系起来。**
- [#2112](https://github.com/volcano-sh/kthena/issues/2112) 和 [PR #2126](https://github.com/volcano-sh/kthena/pull/2126) 针对实例身份、删除 fencing 和重启恢复；PR 截至本报告仍开放、未合并，head `e64091a2`。#2148 作者认为 established 与 committed deletion 仍有缺口；这是讨论者的分析，不是已确认的上游结论。
- [#2067](https://github.com/volcano-sh/kthena/issues/2067) 明确提出 M 的多集群重调度动机，并区分模型加载与 Pod unschedulable。[PR #2079](https://github.com/volcano-sh/kthena/pull/2079) 于 2026-11-26 合并，commit `4323a9bf`；当前 status 有 `unschedulableReplicas` 和最早 `unschedulableSince`，但这不等于 Karmada 已消费这些字段。[`volcano-sh/kthena:pkg/apis/workload/v1alpha1/model_serving_types.go:454-465`](https://github.com/volcano-sh/kthena/blob/1a5145e90e0e1d70c75def0ef9500055e2904c5f/pkg/apis/workload/v1alpha1/model_serving_types.go#L454-L465)。
- [v1.2.0 跟踪 #2031](https://github.com/volcano-sh/kthena/issues/2031) 将 ModelServing 多集群重调度列为计划，引用 #2079；这不是对完整端到端 Karmada 重调度的发布保证。
- 搜索 kthena issue/PR 的 `ServingGroup CRD`、`ServingGroup multi-cluster`，并检查 #2148、#2067、#2079：**未发现上游维护者明确把 ServingGroup CR 方案与跨集群放置联系起来**。Karmada [#8104](https://github.com/karmada-io/karmada/issues/8104) 讨论的是逻辑组重调度，也未提出必须建 CR。

## 对照：aspect → M 的证据 → G 的证据

| Aspect | M | G |
|---|---|---|
| Karmada 模型 | 已有 replica division，但整组求和会令节点估算失真；[#8104 维护者质疑](https://github.com/karmada-io/karmada/issues/8104#issuecomment-7130549641)。 | 与“一个完整多组件 CR 放一个集群”的[提案](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/docs/proposals/scheduling/multi-podtemplate-support/multiple-pod-template-support.md#L27-L58)一致，但 Alpha。 |
| 容量与碎片 | 总和估算在节点级可能过于保守；集群总量又看不到节点碎片。[#8104](https://github.com/karmada-io/karmada/issues/8104)。 | 可调用组件集合估算，但需要正确策略、节点估算器及实测；不是 Volcano queue、拓扑和实际 gang 成功的证明。[源码](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/scheduler/core/estimation.go#L43-L86)。 |
| kthena 兼容与实现成本 | 保持当前父对象和成员控制器；已有 unschedulable status。[#2079](https://github.com/volcano-sh/kthena/pull/2079)。 | 有独立 UID/status/可见性，但 [#2148](https://github.com/volcano-sh/kthena/issues/2148) 记录升级、owner、两层状态和 rollout 复杂性。 |
| 故障粒度 | 需从成员 status 安全地选择并减少不可调度组；现有 descheduler 不认识该单位。[#8104](https://github.com/karmada-io/karmada/issues/8104)。 | 对象级 failover 粒度自然匹配组，但仍需专门健康语义、超时、防抖及无目标保护。[failover 文档](https://karmada.io/docs/userguide/failover/application-failover/)。 |
| 部分容量 | 一个 binding 的全部副本可能整体调度失败；渐进式分配仍是开放需求。[#7116](https://github.com/karmada-io/karmada/issues/7116)。 | **推断：**独立 binding 可让能放下的组先放置、其他组等待；并发容量假设与重试延迟仍需验证。[#7344](https://github.com/karmada-io/karmada/issues/7344)。 |
| 控制面规模 | 较少 RB/Work，但单对象状态可能集中。[#2148](https://github.com/volcano-sh/kthena/issues/2148)。 | 每组产生额外对象和调度/状态写入；已有大规模 RB→Work 压力报告，但没有 G 专项上限。[#7062](https://github.com/karmada-io/karmada/issues/7062)。 |

## 维护者最可能偏好的方向——推断

**推断，不是社区决定：**若只问“如何准确估算并将一个耦合多 Pod 单元放在一个集群”，Karmada 维护者更可能认可 **G 的单对象、单组件集形状**，而不是 M 的整组求和伪装成单 Pod 副本。依据是提案明确要求组件同集群、必要时“ahead of scheduling”拆分，以及 zhzhuang-zju 在 [#8104](https://github.com/karmada-io/karmada/issues/8104#issuecomment-7130549641) 说：

> “I’m not sure treating an entire `ServingGroup` as a single `ModelServing` replica is the right approach”

其具体理由是单节点估算和 workload-specific 重调度单位。

但这**不能推断他们会要求 kthena 上游引入 ServingGroup CR**：同一讨论中维护者先询问应用是否跨集群拆分、组是否独立；组件集副本跨集群拆分尚无明确结论。kthena 维护者对 CR 也尚未表态。综合生命周期迁移成本，当前证据不足以宣布 G 在产品整体上优于 M。

## 各方案需要向上游提出什么

**M：**

1. Karmada：在 #8104 讨论组级 unschedulable 信号及 descheduler 如何安全消费，不仅移除 GVK 白名单；明确目标容量、过期状态、冷却、无目标时行为。[#8104](https://github.com/karmada-io/karmada/issues/8104)
2. Karmada：提出“多个完整组件集作为可拆分 replicas”的 API、估算及分配语义，避免把它与拆分同一组内部组件混淆；处理 `GetComponents` 与 `ReviseReplica` 交互和升级兼容。[#5115](https://github.com/karmada-io/karmada/issues/5115)、[#7895](https://github.com/karmada-io/karmada/issues/7895)
3. kthena：确认 #2079 status 的新鲜度、多个组不同起始时间、scale-down 优先级和 rollout 语义；将跨集群消费协议明确化。[#2067](https://github.com/volcano-sh/kthena/issues/2067)、[#2079](https://github.com/volcano-sh/kthena/pull/2079)

**G：**

1. kthena：在 #2148 单独提出**多集群动机**，讨论 hub ModelServing controller 与成员 ServingGroup controller 的职责、子对象 API、Role 层状态、UID fencing、GC、升级/回滚和全局 rollout budget；不要假设单集群原型已经解决跨集群问题。
2. Karmada：确认单组 CR 的 `GetComponents`、单集群 `Divided/Aggregated` 策略、GPU/节点/queue/topology 估算、组件扩缩容失败下发及 assumption 保护；补充大数量 RB/Work 基准。[#7895](https://github.com/karmada-io/karmada/issues/7895)、[#7062](https://github.com/karmada-io/karmada/issues/7062)
3. 两社区：定义“明确不可调度”与“正常加载”的不同状态、持续时间、目标准入、源端删除、重试及冷却；现有 `Gracefully` 超时不是严格 readiness barrier。[#6233](https://github.com/karmada-io/karmada/issues/6233)、[#7455](https://github.com/karmada-io/karmada/issues/7455)
4. 明确 hub 父子 GC 与成员对象生命周期不能依赖跨集群 owner reference；Kubernetes owner reference 由所在 API 集群解析。[Kubernetes owners 文档](https://kubernetes.io/docs/concepts/overview/working-with-objects/owners-dependents/)，2022-01-08。

## 开放问题

1. 一组的 gang、角色拓扑、Volcano queue quota 和 GPU 碎片，哪些能够由 Karmada estimator 可靠表达，哪些只能成员侧最终判定？
2. G 在资源竞争、controller 重启和 assumption TTL 到期时，能否避免连续组过量放置？无容量时是否先等待、允许成员碎片整理，还是换集群？
3. 组处于模型下载、加载、ready、明确 unschedulable 和 Unknown 时，健康解释器及计时器应如何区分？
4. 无替代集群时，是否允许超时删除源组；如何防止反复迁移、重复执行和跨集群状态丢失？
5. G 的全局 rollout、surge、`maxUnavailable`、状态聚合、owner 迁移和旧版本回滚如何保持原有语义？
6. 典型及峰值组数下，G 的 RB/Work、状态写入、scheduler 吞吐与恢复延迟是否可接受？
7. 社区是否愿意支持 M 的完整组件集 replica division，或更倾向由上层控制器先拆分？目前没有明确答复。
