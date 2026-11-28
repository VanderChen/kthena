# ModelServing 与 Karmada 多集群社区调研

**截至 2026-11-28。**源码基线：Karmada [`6d007132`](https://github.com/karmada-io/karmada/commit/6d007132ec5112d2eed41b602d4aeeebaf96f4fc)，kthena [`1a5145e9`](https://github.com/volcano-sh/kthena/commit/1a5145e90e0e1d70c75def0ef9500055e2904c5f)。Karmada 文档为 v1.19；未合并提案不代表现有能力。

## 结论

1. Karmada 目前可以借助自定义 `InterpretReplica` 和 `ReviseReplica` 将整数副本分配到多个集群；但把整个 ServingGroup 的资源求和当成一个 Pod 的需求，会使节点级估算出现误判。现有多组件调度则把整个 CR 当作一个集合放入单集群，**不能同时表达“每组多 Pod”与“多组跨集群划分”**。([Karmada v1.19 多组件文档](https://karmada.io/docs/userguide/scheduling/multi-component-scheduling/)、[karmada-io/karmada:pkg/scheduler/core/common.go:38-69](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/scheduler/core/common.go#L38-L69))
2. 现有 descheduler 只支持 Deployment，按超过阈值的不可调度 **Pod** 数减副本；并非任意 Pending Pod。([Karmada v1.19 descheduler](https://karmada.io/docs/userguide/scheduling/descheduler/)、[karmada-io/karmada:pkg/estimator/server/replica/replica.go:42-75](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/estimator/server/replica/replica.go#L42-L75))
3. 上游更愿意先讨论**通用的不可调度单元发现、准确估算及保留已有副本的重调度**，而不是把 WorkloadRebalancer 扩成复杂迁移框架。ModelServing 专属生命周期、缩容和业务就绪语义宜留在 kthena。([#8104 维护者回复，2026-11-24](https://github.com/karmada-io/karmada/issues/8104#issuecomment-7130549641)、[#7662 评审，2026-07-21](https://github.com/karmada-io/karmada/pull/7662#pullrequestreview-6008556267)、[具体行内意见](https://github.com/karmada-io/karmada/pull/7662#discussion_r3619073694))
4. kthena 已有组级不可调度计数和优先删除不可调度组，但尚无 Karmada 端组级重调度；其时间戳是**最老组**的时间，不能证明每个被计数的组都超过阈值。([kthena PR #2079，2026-11-26 合并](https://github.com/volcano-sh/kthena/pull/2079)、[volcano-sh/kthena:docs/proposal/modelserving-unschedulable-replicas-status.md:48-54](https://github.com/volcano-sh/kthena/blob/1a5145e90e0e1d70c75def0ef9500055e2904c5f/docs/proposal/modelserving-unschedulable-replicas-status.md#L48-L54))

## A. Karmada 社区与当前实现

### 1. 上游讨论方向

- **[#8104](https://github.com/karmada-io/karmada/issues/8104)**：开放，2026-11-20 创建。维护者指出，仅放开 GVK 不够，还需要找到工作负载所属 Pod；并明确询问为何已有估算仍发生 GPU 碎片化，以及迁移单位究竟是 Pod、整个 workload 还是 ServingGroup。随后指出总资源作为一个 replica 会被单节点检查，以及目前没有 workload-specific 单元。不同集群的组独立运行、GPU 碎片和 ServingGroup 粒度由讨论参与者明确说明。([2026-11-23 评论](https://github.com/karmada-io/karmada/issues/8104#issuecomment-7089472685)、[2026-11-24 使用场景](https://github.com/karmada-io/karmada/issues/8104#issuecomment-7121124864)、[维护者后续意见](https://github.com/karmada-io/karmada/issues/8104#issuecomment-7130549641))
- **[#8084](https://github.com/karmada-io/karmada/pull/8084)**：开放、未合并，2026-11-17 创建，head `2f02f324`。提议 `InterpretPodSelector`、`Component.PodSelector`，并在特性门开启时把单模板结果双写到 `default` component；它属于抢占项目，不等于已经实现组级 descheduling。([PR 描述](https://github.com/karmada-io/karmada/pull/8084))
- **[#7943](https://github.com/karmada-io/karmada/issues/7943)**：开放，2026-10-14 创建。优先级抢占路线图将 selector、binding 与 estimator 联动列为工作项；多组件 victim/preemptor 位于后续阶段。讨论明确倾向 selector，而不是修改工作负载以追踪 Pod。([任务列表](https://github.com/karmada-io/karmada/issues/7943)、[2026-10-23 维护者回复](https://github.com/karmada-io/karmada/issues/7943#issuecomment-6356028133))
- **[#7662](https://github.com/karmada-io/karmada/pull/7662)**：开放、proposal-only，2026-06-23 创建，head `ca6c9179`。初稿含 preserve-ready 和 source-preserving migration；RainbowMango 建议聚焦“只重调度不能运行的部分，不动已就绪部分”，不把 WorkloadRebalancer 做成复杂策略框架。作者随后赞同缩小范围，并提出 `PreserveScheduled`，区别于把所有 unavailable 都视为应迁移。另一维护者建议通过 component selector 找 Pod，再调用 estimator。**这些是尚未达成最终 API 的讨论。**([行内评审](https://github.com/karmada-io/karmada/pull/7662#discussion_r3619073694)、[作者回复](https://github.com/karmada-io/karmada/pull/7662#issuecomment-5092574880)、[selector 设计讨论](https://github.com/karmada-io/karmada/pull/7662#issuecomment-5126046690))
- **[#7621](https://github.com/karmada-io/karmada/issues/7621)**：开放，2026-06-12 创建；讨论复杂 workload 的连续服务、预热与防振荡。独立用户提出多集群 AI inference 和 GPU 成本驱动迁移，维护者指向 #7662 并邀请评审。([2026-10-21 场景](https://github.com/karmada-io/karmada/issues/7621#issuecomment-6307463706))
- **新增发现 [#8109](https://github.com/karmada-io/karmada/pull/8109)**：开放、未合并，2026-11-21 创建，head `44ce5ffb`。它提出配置支持的 workload 并按 owner UID 查 Pod，但仍按 Pod 计数；作者承认 gang 需要不同单位，并暂停等待 #8104 设计讨论。它不能直接解决 ModelServing。([PR 描述](https://github.com/karmada-io/karmada/pull/8109)、[组粒度评审与回复](https://github.com/karmada-io/karmada/pull/8109#discussion_r4661311826))
- 相关开放议题：[#5115](https://github.com/karmada-io/karmada/issues/5115) 跟踪多组件 workload；[#7895](https://github.com/karmada-io/karmada/issues/7895) 跟踪 v1.20 增量扩缩容估算；[#8030](https://github.com/karmada-io/karmada/issues/8030) 讨论单集群多角色推理的 best-effort 下发与本地碎片整理；[#6333](https://github.com/karmada-io/karmada/issues/6333)、[#7116](https://github.com/karmada-io/karmada/issues/7116) 讨论部分/渐进副本分配；[#4046](https://github.com/karmada-io/karmada/issues/4046) 是较早的 status-based deschedule 请求。#8030 明确说跨集群副本分配是另一问题。
- [#8100](https://github.com/karmada-io/karmada/issues/8100)，开放、2026-11-20，报告特定 DecimalSI 内存单位可能使通用组件集合估算错误；这是议题报告而非本次独立复现。

**判断而非承诺：**上游讨论支持提出跨工作负载的“副本单元”和 Pod 归属接口，但不能推断维护者已经接受读取任意 CRD status，或接受将完整迁移编排放进 WorkloadRebalancer。

### 2. 多组件估算与划分

`MultiplePodTemplatesScheduling` 自 v1.16 起为 Alpha、默认关闭；需在 controller-manager、scheduler、webhook 启用。`InterpretComponent` 优先于 `InterpretReplica`，前者成功时后者不执行。([Karmada v1.19 多组件文档](https://karmada.io/docs/userguide/scheduling/multi-component-scheduling/)、[解释器文档](https://karmada.io/docs/userguide/globalview/customizing-resource-interpreter/))

`MaxAvailableComponentSets` 已存在，但当前多模板调度要求显式 `spreadConstraints` 指定恰好一个集群；scheduler 对整个多组件模板要求一个完整集合，并跳过副本划分。提案讨论过完整集合估算，并把组件跨集群拆分列为非目标；这**不是**已经支持把多个完整 ServingGroup 集合分到不同集群。([karmada-io/karmada:pkg/scheduler/core/estimation.go:35-64](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/scheduler/core/estimation.go#L35-L64)、[karmada-io/karmada:pkg/scheduler/core/common.go:38-82](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/scheduler/core/common.go#L38-L82)、[提案:35-58](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/docs/proposals/scheduling/multi-podtemplate-support/multiple-pod-template-support.md#L35-L58))

**设计含义：**需要提出“一个可复制单元包含固定比例的多组件；每个单元内组件同集群，多个单元可分到不同集群”，而不是要求把一个 ServingGroup 的角色拆到不同集群。完整集合容量估算是可复用起点，但调度、revision、配额和状态模型仍需扩展。

### 3. 第三方 CRD 先例

以下均为 Karmada `6d007132` 的内置 third-party Lua，不应概括为都支持跨集群组级重调度：

| 类型 | 已验证的建模及操作 |
|---|---|
| Volcano Job | 每个 task 为 component，副本取 `minAvailable`，并有 health、status reflection/aggregation。([Job:10-73](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/resourceinterpreter/default/thirdparty/resourcecustomizations/batch.volcano.sh/v1alpha1/Job/customizations.yaml#L10-L73)) |
| RayCluster | head 为 1，worker group 各自是 component；有 health、status aggregation。([RayCluster:10-82](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/resourceinterpreter/default/thirdparty/resourcecustomizations/ray.io/v1/RayCluster/customizations.yaml#L10-L82)) |
| RayService | 使用单模板式近似：总数为 head 加 workers，CPU/内存取各组最大值；`ReviseReplica` 将剩余量分给 worker groups。([RayService:10-104](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/resourceinterpreter/default/thirdparty/resourcecustomizations/ray.io/v1/RayService/customizations.yaml#L10-L104)) |
| FlinkDeployment | JobManager、TaskManager 分 component；另有旧式 `GetReplicas` 近似，以及 health/status 操作。([FlinkDeployment:154-185](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/resourceinterpreter/default/thirdparty/resourcecustomizations/flink.apache.org/v1beta1/FlinkDeployment/customizations.yaml#L154-L185)、[:251-274](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/resourceinterpreter/default/thirdparty/resourcecustomizations/flink.apache.org/v1beta1/FlinkDeployment/customizations.yaml#L251-L274)) |
| Kruise CloneSet/StatefulSet | 单模板 `GetReplicas`/`ReviseReplica`，并反射、聚合状态及解释健康。([CloneSet:10-26](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/resourceinterpreter/default/thirdparty/resourcecustomizations/apps.kruise.io/v1alpha1/CloneSet/customizations.yaml#L10-L26)、[StatefulSet:10-26](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/resourceinterpreter/default/thirdparty/resourcecustomizations/apps.kruise.io/v1beta1/StatefulSet/customizations.yaml#L10-L26)) |
| Kubeflow TFJob/PyTorchJob/MPIJob | 按角色模板解析 component；TFJob 的 PS、Worker、Chief 等各有副本和需求。([TFJob:38-71](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/resourceinterpreter/default/thirdparty/resourcecustomizations/kubeflow.org/v1/TFJob/customizations.yaml#L38-L71)、[MPIJob:27-94](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/resourceinterpreter/default/thirdparty/resourcecustomizations/kubeflow.org/v2beta1/MPIJob/customizations.yaml#L27-L94)) |

### 4. 重调度、故障转移与自动伸缩

- descheduler 的 GVK 白名单仅 Deployment；必须是动态 `Divided`：`Aggregated` 或带动态权重的 `Weighted`，静态权重不属于该路径。它从成员状态读取固定字段 `readyReplicas`；ModelServing 使用 `availableReplicas`，因此即使放开 GVK 也需要解决字段语义。([filter.go:30-61](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/descheduler/core/filter.go#L30-L61)、[policy.go:91-105](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/util/helper/policy.go#L91-L105)、[helper.go:126-147](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/descheduler/core/helper.go#L126-L147))
- 当前 WorkloadRebalancer 触发**全新计算**，不保证只移动卡住的组；#7662 的保留行为尚未合并。([Karmada v1.19 WorkloadRebalancer](https://karmada.io/docs/userguide/scheduling/workload-rebalancer/)、[#7662](https://github.com/karmada-io/karmada/pull/7662))
- Cluster failover 面向集群故障/`NoExecute`；Application failover 面向资源整体健康，依赖健康解释和依赖传播。`Graciously` 可等目标健康，但超时后仍可删除源；`Never` 要求人工清理。二者不是“健康集群中仅移动一个不可调度组”。([Cluster failover v1.19](https://karmada.io/docs/userguide/failover/cluster-failover/)、[Application failover v1.19](https://karmada.io/docs/userguide/failover/application-failover/)、[故障转移分析](https://karmada.io/docs/userguide/failover/failover-analysis/))
- FederatedHPA 查询成员指标并修改可伸缩目标；CronFederatedHPA 支持 scale subresource，但文档提醒避免多个伸缩器同时直接写同一 workload。ModelServing 已定义 scale 子资源，selector 有意选每组首角色首实例的一个 entry Pod；但控制面仍需正确聚合 status/selector，且该 Pod 的指标未必代表所有角色的负载。最后一句是设计推论，需实际验证。([FederatedHPA v1.19](https://karmada.io/docs/userguide/autoscaling/federatedhpa/)、[CronFederatedHPA v1.19](https://karmada.io/docs/userguide/autoscaling/cronfederatedhpa/)、[kthena:model_serving_controller.go:2923-2941](https://github.com/volcano-sh/kthena/blob/1a5145e90e0e1d70c75def0ef9500055e2904c5f/pkg/model-serving-controller/controller/model_serving_controller.go#L2923-L2941))

### 5. 状态和 webhook 部署

- 无专属 `InterpretStatus` 时，Karmada 默认反射完整 `.status`，可进入 binding 的各集群状态；但无 `AggregateStatus` handler 时，不能据此假定控制面 ModelServing 自动获得聚合状态。([default.go:129-136、154-163](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/resourceinterpreter/default/native/default.go#L129-L163)、[common.go:186-200](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/pkg/controllers/status/common.go#L186-L200))
- CRD 应存在于 Karmada API server 和成员集群；官方示例使用 `ClusterPropagationPolicy` 分发 CRD。`Retain` 用于保留成员端确需自治的字段，不能无条件保留 `spec.replicas`，否则可能与 Karmada 的副本分配冲突。([CRD 教程 v1.19](https://karmada.io/docs/tutorials/crd-application/)、[解释器 Retain 指南](https://karmada.io/docs/userguide/globalview/customizing-resource-interpreter/))
- **区分两种 webhook**：Karmada resource-interpreter webhook 是解释接口；示例说明 Pull 模式下，成员侧使用 `Retain`/`InterpretStatus`/`InterpretHealth` 需要部署对应 webhook，并考虑网络可达性。Kubernetes admission webhook 则由各 API server 调用。kthena 当前 Helm 模板对 ModelServing 配置的是 `failurePolicy: Fail` 的 validating webhook；该模板的 mutating webhook 目标是 LWS、ModelBooster 和 AutoscalingPolicy，并非 ModelServing。若要求控制面也执行同等验证，应为控制面配置可达的验证服务；这是部署建议，非官方强制要求。([Karmada 示例:38-40、368](https://github.com/karmada-io/karmada/blob/6d007132ec5112d2eed41b602d4aeeebaf96f4fc/examples/customresourceinterpreter/README.md#L38-L40)、[kthena validating:54-73](https://github.com/volcano-sh/kthena/blob/1a5145e90e0e1d70c75def0ef9500055e2904c5f/charts/kthena/charts/workload/templates/kthena-controller-manager/component/validating-webhook.yaml#L54-L73)、[mutating:13-78](https://github.com/volcano-sh/kthena/blob/1a5145e90e0e1d70c75def0ef9500055e2904c5f/charts/kthena/charts/workload/templates/kthena-controller-manager/component/mutating-webhook.yaml#L13-L78))

## B. Volcano 与 kthena

**Volcano Global** 基线 [`bc14015e`](https://github.com/volcano-sh/volcano-global/commit/bc14015ed014e5109af993da712ffe703df72ac3)，2026-03-06：创建 ResourceBinding 时 webhook 对已注册 workload 设置 `spec.suspension.scheduling=true`；dispatcher 对挂起 binding 按队列/作业优先级和容量做准入，再解除挂起交由 Karmada 调度。([mutating.go:61-99](https://github.com/volcano-sh/volcano-global/blob/bc14015ed014e5109af993da712ffe703df72ac3/pkg/webhooks/resourcebinding/mutating/mutating.go#L61-L99)、[dispatcher.go:103-197](https://github.com/volcano-sh/volcano-global/blob/bc14015ed014e5109af993da712ffe703df72ac3/pkg/dispatcher/dispatcher.go#L103-L197)、[capacity.go:95-135](https://github.com/volcano-sh/volcano-global/blob/bc14015ed014e5109af993da712ffe703df72ac3/pkg/dispatcher/plugins/capacity/capacity.go#L95-L135))

这是**Karmada 之上扩展调度/准入的架构先例**，不是已验证的 ServingGroup 卡住后迁移实现。其示例 Volcano Job 解释器只读取并修改 `tasks[1]`，不能作为异构组精确估算模板。([示例:10-23](https://github.com/volcano-sh/volcano-global/blob/bc14015ed014e5109af993da712ffe703df72ac3/docs/deploy/vcjob-resource-interpreter-customization.yaml#L10-L23))

kthena [#2067](https://github.com/volcano-sh/kthena/issues/2067) 已关闭；[#2079](https://github.com/volcano-sh/kthena/pull/2079) 于 2026-11-26 合并为 `4323a9bf`。非终止 Pod 中只要一个满足 `PodScheduled=False/Unschedulable`，对应非 available 组可计数；缩容优先选择此类组。`unschedulableSince` 是所有组的最早时间，零时间可回退创建时间；不能用它严格计算每组各自超过阈值的数量。([实现:28-59](https://github.com/volcano-sh/kthena/blob/1a5145e90e0e1d70c75def0ef9500055e2904c5f/pkg/model-serving-controller/controller/unschedulable_replicas.go#L28-L59)、[缩容:158-184](https://github.com/volcano-sh/kthena/blob/1a5145e90e0e1d70c75def0ef9500055e2904c5f/pkg/model-serving-controller/controller/binpack_scaledown.go#L158-L184)、[设计限制:52-54](https://github.com/volcano-sh/kthena/blob/1a5145e90e0e1d70c75def0ef9500055e2904c5f/docs/proposal/modelserving-unschedulable-replicas-status.md#L52-L54))

[#2031](https://github.com/volcano-sh/kthena/issues/2031) 的 v1.2.0 计划将 #2079 列为多集群重调度相关工作；这不意味着 Karmada 端已经完成。[#2148](https://github.com/volcano-sh/kthena/issues/2148)，开放、2026-11-27，正在讨论组和角色实例的持久身份、已建立状态及已提交删除；明确要求暂缓实现 PR。它对跨集群操作的重启恢复和身份 fencing 有直接关联。

## C. 可比项目

| 项目及版本 | 放置与容量 | 未获容量时及状态 |
|---|---|---|
| **MultiKueue v0.20** | 整个 Job/Workload 通过 `AdmissionCheck` 到候选 worker；manager quota 控制派发，worker 本地准入决定获胜者。AllAtOnce 可并行竞争；Incremental 无准入时每 5 分钟增加候选。([文档](https://kueue.sigs.k8s.io/v0.20/docs/concepts/multikueue/)) | 首个 worker 准入后删除其他候选并同步远端状态；`clusterName` 确定后不可变，不能据此宣称已准入后自动跨集群迁移。LWS 每副本一个 Workload，但所有副本原子跟随同一 worker；因无 `managedBy`，管理端 LWS 状态可能不准确。([LWS 文档，提交 f6d23ad](https://kueue.sigs.k8s.io/docs/tasks/run/multikueue/leaderworkerset/)) |
| **Kueue 本地 waitForPodsReady** | 可基于准入后的 readiness 或 v0.20 Alpha `unscheduledTimeout` 取消准入、挂起并重排队，带退避。([文档](https://kueue.sigs.k8s.io/docs/tasks/manage/setup_wait_for_pods_ready/)) | 这是本地重排队机制；本次未验证其与 MultiKueue 已选 cluster 的重新选择组合行为，不能视为已证实跨集群再派发。 |
| **OCM v1.0 文档** | Placement 根据 predicates/prioritizers 选集群；ManifestWorkReplicaSet 向选中集群复制整个 manifest，非自动划分 CR 的整数副本。([Placement](https://v1-0.open-cluster-management.io/docs/concepts/content-placement/placement/)、[ManifestWorkReplicaSet](https://v1-0.open-cluster-management.io/docs/concepts/work-distribution/manifestworkreplicaset/)) | 可按集群不可达 taint 改选；ManifestWork 支持 JSONPath 状态反馈和 rollout 状态。未发现其按组内 Pod 不可调度自动搬迁 ServingGroup 的能力。([ManifestWork v0.16](https://v0-16.open-cluster-management.io/docs/concepts/work-distribution/manifestwork/)) |
| **KubeAdmiral `77bd7238`** | 支持动态/静态权重划分、副本状态收集及 avoid-disruption。([策略文档，2024-08-21](https://kubeadmiral.io/docs/userguide/propagation-policy/)) | 自动迁移框架按超过阈值的不可调度 Pod 计数；该基线的原生插件注册表只列 Deployment，因此不能推断支持多 Pod 组。([plugins.go:38-61](https://github.com/kubewharf/kubeadmiral/blob/77bd7238f3ffaabbf9d42cf8818387d55cabbd95/pkg/controllers/automigration/plugins/plugins.go#L38-L61)、[util.go:25-73](https://github.com/kubewharf/kubeadmiral/blob/77bd7238f3ffaabbf9d42cf8818387d55cabbd95/pkg/controllers/automigration/util.go#L25-L73)) |
| **Armada `13390eaf`** | 按 gang 的全部 job/Pod 原子调度，可用统一节点标签约束拓扑；默认甚至可跨集群。([gang 文档](https://armadaproject.io/docs/gang-jobs)) | 放不下则整个 gang 排队；启动或集群失败时 gang fail-fast，不自动重试，需以新 gang ID 重提交。executor 报告状态。([gang 文档](https://armadaproject.io/docs/gang-jobs)、[核心概念](https://armadaproject.io/docs/core-concepts)) |
| **Liqo 1.x** | 单位是 Pod；虚拟节点暴露协商得到的远端配额，virtual kubelet 创建远端 twin Pod 并同步状态。([官方文档](https://docs.liqo.io/en/stable/features/offloading.html)) | 连接失败使虚拟节点 NotReady，触发 Kubernetes 常规驱逐；未发现自动按 ServingGroup 整体重派的保证。 |
| **KServe/llm-d** | KServe Next 文档用 LWS、InferencePool 和 endpoint picker 提供多节点及推理请求路由；示例是单集群。([KServe Next](https://kserve.github.io/website/docs/next/model-serving/generative-inference/llmisvc/llmisvc-envoy-ai-gateway)) | 这是请求路由而非已证实的跨集群组级资源重调度；本次未发现该组合解决 ModelServing 所需故障转移的直接先例。 |

## D. 流量范围

Karmada MCS 的 `ServiceExport`/`ServiceImport` 可实现跨集群服务发现，但要求成员网络互通及不重叠 CIDR；它不自动定义 ServingGroup 的推理负载或 KV-cache 感知路由。([Karmada v1.19 MCS](https://karmada.io/docs/userguide/service/multi-cluster-service/))

**建议架构推论：**初期可将全局流量引到各集群 gateway，再在集群内选择可用 ServingGroup；与副本迁移解耦。切流需考虑目标组实际就绪、流量排空及长请求；这些保证需由具体 gateway/业务实现验证。([KServe Next 路由示例](https://kserve.github.io/website/docs/next/model-serving/generative-inference/llmisvc/llmisvc-envoy-ai-gateway))

## 问题对照

| 问题 | Karmada 当前处理 | ModelServing 缺口 | 上游议题/PR |
|---|---|---|---|
| 异构多 Pod 组容量 | 单集群 component-set 估算 | 多个完整组跨集群划分 | [#5115](https://github.com/karmada-io/karmada/issues/5115)、[#7895](https://github.com/karmada-io/karmada/issues/7895) |
| 不可调度检测 | Deployment Pod 超时计数 | 组去重、组级计时和健康字段 | [#8104](https://github.com/karmada-io/karmada/issues/8104)、[#8109](https://github.com/karmada-io/karmada/pull/8109) |
| Pod 归属 | Deployment owner 链 | selector 只能找 Pod，不能单独定义组 | [#8084](https://github.com/karmada-io/karmada/pull/8084)、[#7943](https://github.com/karmada-io/karmada/issues/7943) |
| 保留可用副本 | 普通 Rebalancer 全新调度 | 只移动不可调度组、避免干扰预热 | [#7662](https://github.com/karmada-io/karmada/pull/7662)、[#7621](https://github.com/karmada-io/karmada/issues/7621) |
| 容量不足 | 完整调度可能失败 | 部分组先运行、剩余组渐进分配 | [#6333](https://github.com/karmada-io/karmada/issues/6333)、[#7116](https://github.com/karmada-io/karmada/issues/7116) |
| 成员状态 | 可反射原始 status | 控制面聚合、`availableReplicas` 与组时间语义 | [kthena #2067](https://github.com/volcano-sh/kthena/issues/2067)、[#2079](https://github.com/volcano-sh/kthena/pull/2079) |
| 连续服务 | 整体故障转移与 graceful eviction | 组级 readiness、排空及可靠迁移事务 | [#7621](https://github.com/karmada-io/karmada/issues/7621)、[kthena #2148](https://github.com/volcano-sh/kthena/issues/2148) |

## 建议及社区开放问题

**建议向 Karmada 提出：**

1. 以“**可分配的完整 component set**”描述 ServingGroup：集合内 Pod 同集群，多个集合可独立分布；提供 GPU 碎片、节点/拓扑和队列限制的具体反例，避免只提供资源总和。关联 #5115、#7895、#8104。
2. 在 #8084/#8104 中讨论通用的 Pod→单元归属、去重及超时语义；说明 selector 是发现 Pod 的桥梁，不自动提供组身份或每组计时。
3. 在 #7662 中支持**保留已有可用/已调度副本、只重新安排明确的不可调度缺口**；明确区分 startup、unavailable、unschedulable，不将完整业务迁移框架塞入 Rebalancer。
4. 单独讨论部分分配和重试，与 #6333/#7116 对齐；避免将 #8030 的单集群碎片整理与跨集群分组划分混为一谈。

**建议留在 kthena：**ServingGroup/Role 身份、Pod→组映射、组级状态、缩容选择、模型加载与流量就绪，以及重启恢复和删除 fencing；先参与 #2148 的持久状态讨论。若短期采用外置控制器，必须先验证其与 Karmada scheduler、HPA、故障转移和 rollout 的字段所有权及并发行为；Volcano Global 只是扩展架构先例，不证明直接修改 binding 安全。

**向社区确认：**

- Karmada 是否愿意抽象“多模板组成一个可分配 replica unit”，并让完整集合数参与 `Divided`？
- selector 加组键、通用解释器返回单元，还是可信的成员 workload status，哪种最适合提供组级不可调度数量与时间？
- 如何区分已分配、已调度、已就绪和不可调度，并避免重调度预热中的组？
- 是否需要每组超时信息，而不是仅最老时间戳？如何处理成员状态陈旧、对象重建和代际变化？
- 可否定义安全的扩展点，避免外置控制器与 scheduler 同时改写 binding？

**搜索缺口：**检索了 Karmada、kthena 的 `LeaderWorkerSet`、component sets、multi-cluster、inference 和 unschedulable CRD 相关议题；未发现 Karmada 已实现 LWS/ModelServing 组级跨集群迁移的直接先例。未创建集群、运行容器或修改 GitHub。
