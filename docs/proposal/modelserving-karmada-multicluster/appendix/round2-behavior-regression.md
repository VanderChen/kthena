# 任务 014 补充：把 ServingGroup 做成 CR 由 Karmada 下发，是不是更好（客户行为与回归分析）

角色：Customer behavior and regression analyst（子代理）。日期：2026-11-28。
要回答的问题（用户原话）：「评估如果构造servinggroup cr是不是一个更好的选择」。

## 1. 结论

下文把现在的方案叫 **M**（控制面一个 ModelServing，Karmada 拆 `spec.replicas`，成员集群跑原版 kthena），把问题里的做法叫 **G**（每个 ServingGroup 是一个对象，由控制面上的一个上层 controller 创建，Karmada 把它整个下发到一个集群）。

从用户和运维能感知到的行为看，G **现在不是**更好的选择。M 在 Kind 上暴露了九个问题，G 消除其中四个，但它新增的回归更多、更重，而且它依赖的几样东西今天还不具备：010 的原型不能直接复用，正确放置要靠两个 alpha 特性门控，上游对 ServingGroup CR 还没有结论。

1. **G 不是「把任务 010 的原型搬到 Karmada 上」。** 010 的两个 controller 假定在同一个进程、同一个集群里：
   成员侧的 ServingGroup controller 要求组的 owner 是本集群里的 ModelServing，并从本集群的 ControllerRevision 读 Pod 模板；
   Karmada 下发时会去掉 ownerReference，成员集群里也没有这两样东西，所以 010 的 controller 对下发来的组什么都不做。
   上层的 ModelServing controller 读本集群的 Pod 缓存来判断「组的状态新不新鲜」和「缩容先删谁」，Karmada 控制面上没有 Pod。
   G 需要一份自带模板的新 ServingGroup API、一个重写过的上层 controller、一份新的解释器。（静态，第 3 节）
2. **G 对用户真正的改善有四项。**
   - `maxUnavailable`、`maxSurge`、`partition`、缩容删谁，重新按全局副本数算一次；模板里写整数 `maxUnavailable: 2` 不再被小份额的集群拒绝。
   - 每个组整个放到一个集群，可以用 Karmada 已有的「多组件、节点级」估算，不再把整组资源求和后去找一个放得下的节点；放不下时只是这一组不放，不是整个 ModelServing 一个都不放。
   - 换集群的单位就是一个组对象，同集群里正在运行的其他组不受影响。
   - 控制面上能按组看到状态（`kubectl get servinggroup`），组名全局唯一。
3. **G 对用户的回归有七项，前五项是高严重度。**
   - 整组重建、滚动升级、扩缩容都要控制面在场。控制面不可用时，M 的成员集群照常自愈和滚动；G 只剩组内的 Role 级恢复，进行到一半的滚动会停在「旧组已删、新组没建」的状态。
   - 上层 controller 用来决定「删哪个组」「预算还剩多少」的状态，要经过成员 → Work → ResourceBinding → 模板三次异步写。010 在同进程里只隔一跳，都专门做了复核来防误删；G 没有可用来复核的 Pod 缓存。
   - 现有的三种自动扩缩容都用不了：kthena autoscaler 需要本集群的 Pod 和 ModelServing，FederatedHPA 需要伸缩目标自己有 ResourceBinding，成员侧 HPA 没有对象可改。
   - 正确放置依赖两个默认关闭的 alpha 特性门控和一种特定的策略写法。配错了都不报错：所有组被放进同一个集群，或者每个组被下发到所有集群，或者一批同时创建的组挤进同一个「看起来最空」的集群。
   - 存量单集群用户接入要做两次在线迁移（Pod 的 owner 换成 ServingGroup，再把 ServingGroup 交给 Karmada）；变体 G-c 则只能把所有组重建一遍。
   - 每次滚动、每次整组重建都会重新调度，组在集群间的分布会漂移，新集群要冷启动；「每个地域一份完整副本」和「按固定比例拆分」这两种 M 已经支持的分发方式，G 表达不了。
   - 经过 Karmada 的写入量从「每个 ModelServing 每个集群一份状态」变成「每个 Pod 的每次状态变化」。
4. **M 的九个问题在 G 下：消除四个，形态改变三个，保留一个，变差一个。**（第 5 节和 6.3 的表）
5. **判断：** 以今天的代码和已知的用户场景，M 更适合作为多集群的第一步。G 值得做，需要下面四个条件同时成立，其中前两个目前不知道：
   - X1：上游是否采纳 ServingGroup CR（volcano-sh/kthena#2148 还在讨论，并要求暂缓实现 PR）。
   - X2：典型部署里每个集群有几个组。每个集群只有 1 到 2 个组时，M 的「各集群各滚各的」等于同时全停，全局预算是硬需求；每个集群有很多组时，M 的按份额预算够用。
   - X3：用户不依赖 kthena autoscaler，或者接受先没有自动扩缩容。
   - X4：可以接受整组恢复和滚动依赖 Karmada 控制面的可用性。

第 4 节按领域对比，第 5 节逐条回答 M 的九个问题，第 6 节是三张汇总表，第 7 节是 G 需要的回归覆盖，第 8 节是没法靠读代码确认的部分，第 9 节是判断的展开。

## 2. 范围、方法和证据标注

- 只读静态分析。没有起集群，没有运行任何项目测试，没有改任何源码树。执行过的只有查看类命令（`grep`、`sed`、`git log`）和一条只读的 `gh issue view 2148 --repo volcano-sh/kthena`。
- 源码和文档：
  - `karmada:` = `<workspace>/karmada`，`6d007132`。
  - `kthena:` = `<workspace>/kthena-014`，`1a5145e9`（上游 main）。
  - `kthena-010:` = `<workspace>/kthena-010`，`98fcbf01`（ServingGroup CR 原型，只读）。
  - `010-doc:` = `<workspace>/kthena/docs/proposal/modelserving-servinggroup-cr/`。
  - M 的设计和 Kind 证据：本任务目录的 `DESIGN.md`、`PROPOSAL_COMMIT.md`、`evidence/logs/kind-*.log`。
- 两个选项的定义按任务说明：
  - **M**：控制面一个 ModelServing，Karmada 拆 `spec.replicas`，成员跑原版 kthena。
  - **G**：控制面一个 ModelServing；一个对着 karmada-apiserver 运行的上层 controller（下文叫「上层 controller」）为每个副本建一个 ServingGroup 对象；Karmada 把每个 ServingGroup 整个下发到一个集群；成员只跑 ServingGroup 级的 controller。
  - **G-c**：不新增 CRD，用 `replicas: 1` 的 ModelServing 当下发单位。和 G 不同的地方集中写在 4.10。
- G 还没有设计稿，也没有实现。本文里关于 G 的行为，凡是没有标「静态」的都是推断。
- 证据标注：

| 标注 | 含义 |
| --- | --- |
| 静态 | 在上述 commit 的源码或文档里读到，附 `路径:行号` |
| Kind-M | 任务 014 在 Kind 上对选项 M 观察到的，附日志文件名。它不是 G 的证据 |
| 实测-2148 | volcano-sh/kthena#2148 正文里报告的测量值，不是本次测的 |
| 推断 | 由静态事实推出，没有执行过 |
| 需 Kind | 只有在集群上才能确认 |

- 分类：**预期变化**（包括改善）、**回归**、**中性**。
- 「覆盖」指向第 7 节的编号：`GU-` 单测或包级测试，`GL-` 解释器 testdata，`GK-` Kind 场景。

## 3. 决定 G 行为的静态事实

后面各节反复引用这些事实，用 F 编号。

**F1 010 的 ServingGroup controller 不处理 Karmada 下发来的组。**
- Karmada 生成 Work 时去掉 `metadata.ownerReferences`（`karmada:pkg/resourceinterpreter/default/native/prune/prune.go:77`）。
- 010 的 ServingGroup controller 对没有 ModelServing 作 controller owner 的组、或者本集群里找不到这个 ModelServing 的组，直接返回，不建 Pod（`kthena-010:pkg/model-serving-controller/sgcontroller/reconcile.go:107-113,292-308`）。
- Pod 模板、plugin、调度器名不在 ServingGroup 的 `spec` 里，而是从本集群的 ControllerRevision 和父 ModelServing 读（`kthena-010:pkg/apis/workload/v1alpha1/servinggroup_resource_types.go:25-28`；`kthena-010:pkg/model-serving-controller/sgcontroller/round.go:141-144`）。
- Karmada 的 Lua 解释器只拿到对象本身和脚本（`karmada:pkg/resourceinterpreter/customized/declarative/luavm/lua.go:158`），`spec` 里没有模板就算不出各组件的资源需求。
- 所以 G 的 ServingGroup 必须自带模板，或者改用解释器 webhook 去查 ControllerRevision。前者是新的 API；后者多一个要部署的组件，而且 pull 模式下解释器操作也在 karmada-agent 里执行（`DESIGN.md` 3.1），webhook 要对成员可达（推断）。

**F2 010 的 ModelServing controller 依赖本集群的 Pod 缓存。**
- 它用自己的 Pod 缓存算一份摘要，和组 `status.observedPods` 比，不一致就不信这份状态（`kthena-010:pkg/model-serving-controller/mscontroller/observe.go:149,165`；`010-doc:02-design.md` 3.1 的 A7）。
- 缩容和回收「未就绪的先删」、deletion cost 也从 Pod 缓存现算（`observe.go:170-192`；`010-doc:02-design.md` 4.4）。设计文档说明了原因：经组的 `status` 汇总会「晚一跳」，刚设完 cost 马上缩容可能不生效（3.1 的 A5）。
- 设计的部署前提是两个 controller 在同一个进程里，共用租约和 informer（`010-doc:02-design.md` 3.3）。
- Karmada 控制面没有 Pod。G 的上层 controller 只能读经 Karmada 回传的组状态。

**F3 多组件估算生效有四个条件，缺一个都不报错。**
- 特性门控 `MultiplePodTemplatesScheduling`，alpha，默认关（`karmada:pkg/features/features.go:118-126,192`）。
- 策略里有按集群的 spread constraint，`minGroups` 和 `maxGroups` 都是 1（`karmada:pkg/scheduler/core/estimation.go:43-65`）。
- `replicaSchedulingType` 是 `Divided`。不写或写 `Duplicated` 时，调度器忽略可用资源，直接取排在最前的集群（`karmada:pkg/scheduler/core/spreadconstraint/select_clusters.go:28-38,71-80`；`select_clusters_by_cluster.go:38-39`）。
- 拆分偏好不能是静态权重（`Weighted` 且没有 `dynamicWeight`）。这种写法下 spread constraint 被忽略，所有候选集群都被选中（`select_clusters.go:28-32,57-69`）；多组件绑定又是「下发到所有选中的集群」（`karmada:pkg/scheduler/core/common.go:72-81`），结果是每个组在每个集群各有一份。
- 集群的排序是：得分高的在前，得分相同时可用量大的在前，再相同按名字（`karmada:pkg/scheduler/core/spreadconstraint/util.go:43-61`）。

**F4 默认配置下，调度器不记「刚分出去、还没在成员上建出 Pod」的占用。**
`SchedulingOvercommitProtection` 是 alpha、默认关；门控的注释写明它要解决的就是「多个工作负载同时被调度进同一个资源空位」（`karmada:pkg/features/features.go:154-165,195`）。G 下每个组是一个独立的 ResourceBinding，各自估算。

**F5 多组件绑定重新调度时，不把这个组自己已经占用的资源加回去。**
- 触发条件：`spec.clusters` 为空（被驱逐之后），或者某个组件的副本数变了（`karmada:pkg/util/binding.go:46-52,77-98`）；另外还有策略的 placement 变化和显式触发（`karmada:pkg/scheduler/scheduler.go:421-441`）。
- 可用量 = 估算器结果 + 该集群已分配的副本数（`karmada:pkg/scheduler/core/spreadconstraint/group_clusters.go:365-369`）。多组件绑定的调度结果里只记集群名和组件，副本数是 0（`karmada:pkg/scheduler/core/common.go:72-81`），所以加回去的是 0。
- 估算器把这个组现有 Pod 的占用算在「已分配」里。当前集群放不下「再来一整组」时，可用量是 0，调度器换成别的集群（`select_clusters_by_cluster.go:65-94`）。
- 推断：一个正在运行的组，只要触发一次重新调度，而所在集群已经没有再放一整组的余量，就可能被整组搬走。需 Kind。

**F6 应用故障转移（`failover.application`）的实际规则。**
- 生效条件：策略里设了它、该 kind 有 `InterpretHealth`、绑定的 `propagateDeps` 为 true（`karmada:pkg/controllers/applicationfailover/rb_application_failover_controller.go:219-243`）。
- 计时：从 controller 第一次看到 `Unhealthy` 开始，记在进程内存里；状态一变回 `Healthy` 或 `Unknown` 就清掉；controller 重启后重新计（`karmada:pkg/controllers/applicationfailover/common.go:34-46,88-124`；`rb_application_failover_controller.go:89-119`）。
- 动作：超过 `tolerationSeconds` 就把这个集群从绑定里拿掉并建一个驱逐任务，不检查别的集群有没有容量（`rb_application_failover_controller.go:121-166`）。
- 驱逐任务存在期间，调度器不会再选这个集群（`karmada:pkg/scheduler/framework/plugins/clustereviction/cluster_eviction.go:50-57`）。
- 任务什么时候结束：新集群上的对象变成 `Healthy`，或者超时（`gracePeriodSeconds`，默认 600 秒）（`karmada:pkg/controllers/gracefuleviction/evictiontask.go:67-90`；`karmada:pkg/apis/policy/v1alpha1/propagation_types.go:341-348`）。
- 任务存在期间旧集群的 Work 保留，除非 `purgeMode: Directly`（`karmada:pkg/util/helper/binding.go:199-206`）。

**F7 状态回传的路径和限速。**
- 成员对象每次变化：反射状态并解释健康，写 Work 的 status，各发一条事件（`karmada:pkg/controllers/status/work_status_controller.go:239-290`）；再汇总到 ResourceBinding（`karmada:pkg/controllers/status/rb_status_controller.go:114-121`）；再经 `AggregateStatus` 写模板的 status（`karmada:pkg/controllers/status/common.go:148-204`）。没有 `AggregateStatus` 时模板的 status 不更新（`common.go:164-166`）。
- karmada-controller-manager 默认 `--kube-api-qps=40`、`--kube-api-burst=60`，ResourceBinding 和 Work 各 5 个并发（`karmada:cmd/controller-manager/app/options/options.go:201-210`）。

**F8 `ReviseReplica` 只在该 kind 实现了这个钩子时才调用**（`karmada:pkg/controllers/binding/common.go:85-96`）。

**F9 自动扩缩容各自依赖什么。**
- kthena autoscaler 用本集群的 Pod lister 取 Pod，直接访问 Pod IP 抓指标，然后 patch ModelServing 的 `spec.replicas` 或 role 的 `replicas`（`kthena:pkg/autoscaler/autoscaler/metric_collector.go:248,408`；`kthena:pkg/autoscaler/controller/autoscale_controller.go:198-201,482-494`）。
- FederatedHPA 先找伸缩目标自己的 ResourceBinding，从里面取目标集群；找不到就报 `FailedGetBinding`（`karmada:pkg/controllers/federatedhpa/federatedhpa_controller.go:241-249,408-449`）。

**F10 资源模型对多组件估算同样设上限。** 集群汇总估算器在多组件路径上，用资源模型的各档下界造虚拟节点再模拟放置；默认模型只有 cpu 和 memory 两个维度（`karmada:pkg/estimator/client/general.go:241-254,296-327`）。推断：请求 GPU 的组件放不进这些虚拟节点，结果是 0。

**F11 控制面上删除一个模板对象，不等成员上的对象和 Pod 消失。** ResourceBinding 被删时，binding controller 发出删 Work 的请求后立刻摘掉自己的 finalizer（`karmada:pkg/controllers/binding/binding_controller.go:83-90`）。

**F12 调度失败的表现。** 没有集群放得下时返回 `InsufficientCapacityError`（`karmada:pkg/scheduler/core/spreadconstraint/select_clusters_by_cluster.go:41-52`）；绑定在不可调度队列里最多停 5 分钟后重试（`karmada:pkg/scheduler/internal/queue/scheduling_queue.go:40-45`）。

**F13 010 的原型里没有「调度不上」的信号。** `kthena-010` 的 ModelServing 状态里没有 PR #2079 的 `unschedulableReplicas`、`unschedulableSince`；ServingGroup 的 status 里也没有「调度不上」的 condition 或时间（`kthena-010:pkg/apis/workload/v1alpha1/servinggroup_resource_types.go:174-204,245-306`）。

## 4. 分领域对比

每个表的三列依次是：今天的单集群、选项 M、选项 G。表后的说明只写表里放不下的理由。

### 4.1 API 和编写方式

| 项 | 单集群（今天） | M | G | 分类（G 相对 M） | 证据 |
| --- | --- | --- | --- | --- | --- |
| 用户写什么 | 一个 ModelServing | 控制面：ModelServing + PropagationPolicy；管理员装一份解释器 | 控制面：ModelServing + 一个按标签选中 ServingGroup 的 PropagationPolicy（必须 `Divided`、不能是静态权重、spread 为 1、`propagateDeps: true`）；管理员装 ServingGroup 的解释器、开两个 alpha 门控、部署上层 controller | 预期变化，但策略写错和门控没开都不报错（F3） | F3、F4、F6 |
| 策略选错对象 | 不适用 | 不适用 | 策略如果像 M 那样选中 ModelServing 本身，ModelServing 会被下发到成员；成员若装了完整的 kthena，就会再建一遍所有组 | 回归风险 | 推断 |
| 控制面 `kubectl` 能看到什么 | ModelServing | ModelServing（汇总计数）、ResourceBinding 里每个集群一项 | ModelServing，加每个组一个 ServingGroup 对象，列出版本、Ready、Pod 数 | 预期变化（改善） | `kthena-010:.../servinggroup_resource_types.go:314-319` |
| 组在哪个集群 | 不适用 | 看 ResourceBinding 的 `spec.clusters` | 要逐个看每个 ServingGroup 的 ResourceBinding；ServingGroup 对象上没有这个信息，除非上层 controller 抄一份 | 中性，需要补一个展示字段 | 推断 |
| 成员集群里能看到什么 | 同左 | ModelServing（份额）、Pod | 只有落在本集群的 ServingGroup 和 Pod；没有 ModelServing，成员侧运维熟悉的 `kubectl get modelserving` 是空的；kthena 记在 ModelServing 上的事件没有对象可挂 | 回归（排障入口变了） | 010 把 Role 实例的事件记在父 ModelServing 上（`kthena-010:.../sgcontroller/reconcile.go:212-224`）；其余是推断 |
| 组名和序号 | `<name>-<序号>` | 每个集群各自从 0 编号，组名、Pod 名跨集群重复 | 全局唯一的 `<name>-<序号>`，Pod 名跟着唯一 | 预期变化（改善）：集中采集的日志、指标不用再靠集群标签区分 | `010-doc:02-design.md` 2.2 |
| 换集群期间的重名 | 不适用 | 不适用 | 优雅驱逐期间，同一个组名同时存在于新旧两个集群 | 回归风险（低） | F6 |
| Pod 标签 | `modelserving.volcano.sh/name`、`group-name`、`role`、`role-id`、`entry`、`revision`、`role-template-hash` | 不变 | 成员 controller 照 010 的做法渲染时不变；ModelServer 的 `matchLabels` 和 PD 配对用的 `groupKey` 继续可用 | 中性，前提是自带模板的新 API 渲染出同样的标签 | `kthena:pkg/apis/workload/v1alpha1/labels.go:21-34`；`kthena:examples/kthena-router/ModelServer-prefill-decode-disaggregation.yaml:11,13`；`010-doc:02-design.md` 8.3 |
| 用户直接删一个组 | 没有这个对象 | 没有这个对象 | 在控制面删：上层 controller 重建，并重新调度。在成员上删：Karmada 把对象重新下发，UID 变了，账本丢了 | 预期变化，两处删除语义不同，要写进文档 | 成员上的对象被删后 Karmada 重新下发是静态（`karmada:pkg/controllers/execution/execution_controller.go:593-610`）；其余是推断 |
| 整个对象过 Lua | 不适用 | `ReviseReplica`、`Retain` 让完整对象往返一次 Lua，有 `{}` 和 `[]` 混淆的风险 | 不实现这两个钩子，对象不往返 | 预期变化（改善） | F8；第一轮报告 V7 |

覆盖：GK-01、GK-02、GK-03、GU-11、GL-01。

### 4.2 `spec` 各字段的含义

| 字段 | 单集群（今天） | M | G | 分类（G 相对 M） | 证据 |
| --- | --- | --- | --- | --- | --- |
| `replicas` | 本集群的组数 | `Divided` 时是全局总数，成员对象被改写成份额 | 全局的 ServingGroup 对象个数，没有改写 | 预期变化（回到字面含义） | 推断 |
| `maxUnavailable`（组一级） | 按 `replicas` 算一次 | 每个成员按份额各算一次；全局同时不可用数是各集群之和；整数值大于份额时被成员拒绝（Kind-M：`kind-C6-replica-dependent-validation.log`） | 上层 controller 按全局 N 算一次 | 预期变化（改善）；执行依据是回传的状态，见 4.4 | `kthena:pkg/model-serving-controller/webhook/validator.go:239-256` |
| `maxSurge` | 本集群多建几组 | 每个集群各多建一份 | 全局多建 `maxSurge` 个组，Karmada 把 surge 组放到有容量的集群 | 预期变化：原集群满了滚动不再卡住；副作用是分布漂移，见 4.7 | `010-doc:02-design.md` 4.4；推断 |
| `partition` | 保护序号 `[0, P)` | 每个集群各自保护 `[0, P)`，全局金丝雀数量不再是 `N - P` | 按全局序号保护，金丝雀数量准确 | 预期变化（改善）；但金丝雀落在哪个集群不可控，「只在某个地域灰度」表达不了 | `010-doc:02-design.md` 4.3；推断 |
| 按集群改镜像做灰度（OverridePolicy） | 不适用 | 可用（`DESIGN.md` 9.1） | 成员上的 Pod 模板和上层 controller 记的版本对不上，组的 `status.revision` 失真 | 回归风险 | 推断 |
| `rolloutStrategy.type: RoleRollingUpdate`、Role 一级的 `maxUnavailable`、`maxSurge`、`partition`、`roleCoordination` | 组内 | 组内，不受拆分影响 | 组内，由成员 controller 执行 | 中性 | `kthena-010:.../servinggroup_resource_types.go:107-125,141-169` |
| `evictionStrategy` | 驱逐 webhook 按本集群的 ModelServing 和 `replicas` 算预算 | 每个集群按份额算一份预算 | **不会变回全局。** 驱逐是成员 apiserver 上的同步准入，成员里没有 ModelServing，也没有全局计数。要么按「本集群的组」重新定义，要么失效 | 回归风险（未定义） | `kthena:pkg/model-serving-controller/webhook/eviction_handler.go:338-341,557-558`；`010-doc:02-design.md` 8.1 |
| `recoveryPolicy: RoleRecreate` | 组内重建一个 Role 实例 | 不变 | 不变，成员 controller 本地执行 | 中性 | `010-doc:02-design.md` 5.3 |
| `recoveryPolicy: ServingGroupRecreate` | 本集群内整组重建 | 不变 | 成员写 `Failed`，上层 controller 删对象再重建，新对象重新调度，可能落到别的集群 | 回归（依赖控制面，见 4.7）；同时是新能力（坏掉的组可以换集群） | `010-doc:02-design.md` 5.4；F11 |
| `restartGracePeriodSeconds` | 组内 | 不变 | 不变 | 中性 | `kthena-010:.../servinggroup_resource_types.go:80-83` |
| `gangPolicy`、`networkTopology`、队列注解 | 每组一个 PodGroup | 不变；Queue、HyperNode 要另外分发 | 不变，前提是这些设置在 ServingGroup 的 `spec` 里（010 已经带了）；Queue、HyperNode 同样要另外分发 | 中性 | `kthena-010:.../servinggroup_resource_types.go:85-95` |
| `plugins` | 控制器在本集群调用钩子 | 不变 | 010 里两个 controller 都调 `OnModelServingSync`，并把本集群的 client 交给 plugin。上层 controller 交出去的会是 karmada-apiserver 的 client | 回归风险：plugin 在控制面上建的 Service 等对象只是模板，不是成员里的资源 | `kthena-010:pkg/model-serving-controller/mscontroller/reconcile.go:159-169`；`.../sgcontroller/reconcile.go:150-158` |
| `bootstrapAccelerateStrategy` | 这个 commit 只有 API 类型，控制器代码里没有引用 | 同左 | 同左。API 注释里的百分比按 `spec.replicas * role.replicas` 算；G 下可以全局算一次，但「有没有就绪的源」是集群内的概念 | 中性（未实现，不比较） | `kthena:pkg/apis/workload/v1alpha1/model_serving_types.go:55-58,114-127`；对 `pkg/` 的 `grep` 只命中类型文件 |

小结，G 下「重新变成全局」的是：`replicas`、组一级的 `maxUnavailable`、`maxSurge`、`partition`、缩容删谁、整组重建由谁决定。没有变回全局的是 `evictionStrategy`。新出现的跨集群行为是：surge 组和重建的组落在别的集群，以及由此带来的分布漂移。

覆盖：GU-04、GU-05、GU-10、GK-07 到 GK-10、GK-24。

### 4.3 校验和报错

| 项 | 单集群（今天） | M | G | 分类（G 相对 M） | 证据 |
| --- | --- | --- | --- | --- | --- |
| 非法 `spec` 在哪里被拦 | `kubectl apply` 当场 | 控制面接受，成员拒绝（异步） | 上层 controller 本来就在控制面，校验 webhook 注册到 karmada-apiserver 后是同步的 | 预期变化（改善），前提是 webhook 确实装在控制面 | `DESIGN.md` 第 6 节（校验器不依赖集群内 client） |
| 和副本数挂钩的规则 | 按 `replicas` | 按份额，原本合法的模板被小集群拒绝（Kind-M：C6） | 按全局 N | 预期变化（改善） | 同 4.2 |
| 成员侧还会拒绝什么 | 不适用 | webhook 的全部规则 | ServingGroup 的 CRD schema 和成员 webhook；控制面和成员的 CRD 版本不一致时字段被静默裁掉 | 中性（和 M 同类） | 推断 |
| 成员侧失败怎么显示 | 事件 | Work `Applied=False`；该集群从汇总里消失（Kind-M：C6） | 只有那一个 ServingGroup 没有状态，其他组照常显示 | 预期变化（改善） | F7 |
| 运行期错误（队列不存在、PodGroup 建不出来、模板读不到） | ModelServing 的事件和 condition | 只在成员上 | 组的 `ReconcileFailed` condition 回传到控制面 | 预期变化（改善） | `kthena-010:.../servinggroup_resource_types.go:197-199` |
| 调度不出去 | 不适用 | 整个绑定 `Scheduled=False` | 单个组的绑定 `Scheduled=False`；这个组没有任何集群、没有 Pod | 预期变化；多了一个要向用户解释的状态，见 4.4 | F12 |

覆盖：GU-10、GK-05、GK-10。

### 4.4 状态和可观测性

| 项 | 单集群（今天） | M | G | 分类（G 相对 M） | 证据 |
| --- | --- | --- | --- | --- | --- |
| 控制面 ModelServing 的 `status` 谁写 | 控制器 | Lua 汇总：计数器求和，`conditions`、revision 字段不汇总 | 上层 controller 自己算，`conditions`、`currentRevision`、`updateRevision`、`observedGeneration` 都有全局含义 | 预期变化（改善） | `DESIGN.md` 4.2 |
| 按组的可见性 | 没有对象，只能看 Pod | 控制面看不到组 | 每组一个对象 | 预期变化（改善） | 同 4.1 |
| 组状态里的 `observedGeneration` | 不适用 | 不适用 | 成员对象的 generation 和控制面模板的 generation 是两个计数空间。组换集群后成员的 generation 从 1 重新开始，原样回传会让上层 controller 一直认为「未观测」 | 回归风险，需要解释器按 Karmada 的惯例换算 | `kthena-010:.../mscontroller/observe.go:164`；第一轮报告 S4 |
| 换集群期间有两份状态 | 不适用 | 不适用 | 优雅驱逐期间新旧两个集群都在上报，`AggregateStatus` 拿到两项，必须有规则决定信哪个 | 回归风险 | F6、F7 |
| 「组对象存在但还没有 Pod」 | 没有这个状态 | 没有这个状态 | 010 在同进程里已经因为这个状态出过几个缺陷（#2148 的 Plan A 缺点 3）。G 下这个状态细分成：没调度出去、调度了没下发成功、下发了没上报、上报了；第一种可以无限期持续 | 回归风险 | volcano-sh/kthena#2148；F12 |
| `unschedulableReplicas`、`unschedulableSince` | 有 Pod 调度不上的组数和最早时间 | 各集群求和、取最早 | 要重新定义：没有任何集群的组没有 Pod，按 #2079 的口径它不算「调度不上」，按用户的理解它是 | 预期变化，语义要用户批准 | `kthena:pkg/apis/workload/v1alpha1/model_serving_types.go:460-465`；F13 |
| 成员不可达 | 不适用 | 状态停在旧值，汇总偏高；没有控制器据此做删除决定（重调度除外，有新鲜度规则） | 状态同样停在旧值，但上层 controller 会用它算滚动预算和缩容对象 | 回归 | 推断 |

**状态从「展示」变成「决策输入」是 G 最重要的一条回归机制。**
- M 下，决定删哪个组、预算够不够的是成员里的 kthena controller，它直接看本集群的 Pod。经 Karmada 回传的状态只给人看，以及给重调度用（`DESIGN.md` 8.2 的规则 1 专门防过期状态）。
- G 下，上层 controller 的每个删除决定都基于回传的状态。路径是三次异步写（F7），正常负载下是秒级，大规模滚动时受 40 QPS 限制会到分钟级（4.8）。
- 010 在同一个进程里只隔一跳，仍然加了摘要复核，理由写的是「组刚变坏或者刚恢复、status 还没写出来的那一跳不进额度，也不让它被当成未就绪的组删掉」（`010-doc:02-design.md` 3.1 的 A7）。G 没有 Pod 缓存可以做这个复核，只能用较弱的替代：回显 generation、集群 Ready 条件、状态的时间戳。

覆盖：GU-01、GU-02、GU-06、GU-07、GL-04、GL-05、GK-12、GK-18。

### 4.5 伸缩

| 项 | 单集群（今天） | M | G | 分类（G 相对 M） | 证据 |
| --- | --- | --- | --- | --- | --- |
| 缩容删哪些组 | 全局一个排序：调度不上 → 其他未就绪 → Running | 跨集群不看就绪情况；一次缩容清空了一个还有 Running 组的集群（Kind-M：`kind-C5-pingpong-and-directed-move.log:101-107`） | 上层 controller 选 ServingGroup 对象，可以恢复全局排序 | 预期变化（改善），有两个条件 | `kthena:pkg/model-serving-controller/controller/binpack_scaledown.go:39-44,159-185` |
| 上一行的条件 | 不适用 | 不适用 | 一是排序依据要进组的 status（010 从 Pod 读，并且没有「调度不上优先」这一档，F2、F13）；二是依据是回传的状态，刚变成 Ready 的组可能被当成未就绪的删掉 | 回归风险 | F2、F7、F13 |
| 扩容放哪里 | 本集群 | 一次决定把增量按估算分给各集群，`Aggregated` 优先已有副本的集群 | 每个新组独立调度，各自选「可用量最大」的集群 | 回归风险：一批同时创建的组会挤进同一个集群（F4） | F3、F4 |
| 没有集群放得下 | 组在本集群排队 | 动态拆分下，首次调度时整个 ModelServing 一个都不放（Kind-M：C2c）；扩容的增量同样是要么全放、要么全不放（推断） | 放得下的组照常放，放不下的组没有集群、没有 Pod | 预期变化（改善了「全有或全无」）；副作用：没有 Pending 的 Pod，成员集群的节点自动扩容不会被触发，组也不在任何调度队列里排队 | F12；推断 |
| 分发方式 | 不适用 | Duplicated、静态权重、动态拆分三种（`DESIGN.md` 5.1） | 只自然支持一种：按估算容量逐组放置。「每个地域一份完整副本」「按 1:3 拆」表达不了 | 回归（功能范围变小） | F3；推断 |
| 集群份额降到 0 | 不适用 | 成员上的 ModelServing 连同版本历史被删（Kind-M：C11） | 成员上本来就没有 ModelServing，只是最后一个组的 Work 被删；按依赖下发的 PVC 仍然跟着走 | 中性（略好） | 推断 |
| 改 role 的 `replicas` | 组内扩缩 | 组内扩缩 | 组件副本数变了会触发重新调度；所在集群放不下「再来一整组」时，正在运行的组可能被整组搬走（F5） | 回归风险（高） | F5；需 Kind |
| kthena autoscaler | 可用 | 只能在成员上跑，配 Duplicated 和 `retain-replicas`（`DESIGN.md` 第 7 节） | 成员上没有 ModelServing 可以 patch；控制面上没有 Pod 可以抓指标。两边都跑不了 | 回归（高） | F9 |
| `DisaggregatedTarget`（改 role 副本数） | 可用 | 成员上可用，`Retain` 保留 role 的 `replicas` | 同上跑不了；即使有别的办法改到 role 副本数，也会撞上 F5 | 回归（高） | F9、F5 |
| FederatedHPA | 不适用 | 可以指向模板的 scale 子资源（端到端待验证） | ModelServing 没有被下发，没有 ResourceBinding，FederatedHPA 报 `FailedGetBinding` | 回归 | F9 |
| 成员侧 HPA、KEDA | 可用 | 配 `retain-replicas` 可用 | 没有可伸缩的对象；ServingGroup 的 scale 子资源指向 `expectedPods`，只给 PDB 用，改了会被写回 | 回归 | `010-doc:02-design.md` 8.5 |

覆盖：GU-04、GU-09、GU-13、GK-04、GK-05、GK-06、GK-11、GK-12、GK-26。

### 4.6 重调度：调度不上的组换集群

| 项 | M | G | 分类（G 相对 M） | 证据 |
| --- | --- | --- | --- | --- |
| 被挪动的单位 | 降低问题集群的份额，由成员的 kthena 挑一个组删掉（优先调度不上的） | 一个 ServingGroup 的绑定整个换集群 | 预期变化（改善）：不存在「删错组」的竞态，同集群其他组不受影响 | Kind-M：`kind-C5-reduce-then-scheduler.log`；F6 |
| 现成的触发机制 | 没有，要改 karmada-descheduler（第 2 阶段） | 应用故障转移现成可用，但它看的是「健康」 | 预期变化；现成不等于够用，见下 | F6 |
| 区分「调度不上」和「没就绪」 | `unschedulableReplicas` 已经区分，加载模型的组不算 | 取决于 `InterpretHealth` 怎么定义。按 Ready 定义：加载模型、一个 Pod 崩溃、原地更新中都会被判不健康。按「没有调度不上的 Pod」定义：需要组的 status 新增这个信号（F13） | 回归风险（高），健康定义二选一都有代价 | F6、F13 |
| 计时 | 用成员上报的 `unschedulableSince` | 从 karmada-controller-manager 第一次看到不健康算起，记在内存里；状态抖一下或 controller 重启就重新计 | 回归风险（中） | F6 |
| 降之前确认有去处 | 设计了规则 6（未实现） | 没有。超时就驱逐 | 回归风险 | F6 |
| 没有集群有容量时 | Kind-M：副本 5 分 06 秒不属于任何集群，然后回到原集群重新计时 | 推断：旧集群的副本在优雅期内保留（默认最长 600 秒），然后被删；之后这个组没有集群；驱逐任务结束后原集群又可选，估算器看到卡住的 Pod 已经没了，把它放回去，重新开始计时。每一轮都删一遍、建一遍 Pod | 形态改变，没有消除 | F6、F12；需 Kind |
| 来回搬 | Kind-M：有容量时副本去了别的集群，没有还给原集群 | 新集群也调度不上时，再过一个容忍时间又被驱逐；这时原集群的驱逐任务如果已经超时，就搬回去 | 回归风险（中） | F6；需 Kind |
| 一个集群整体出问题 | descheduler 每轮按绑定处理，规则 6 限制「最多降到别的集群能接的数量」 | 这个集群上所有不健康的组差不多同时被驱逐，各自独立调度，挤进同一个集群（F4），在那里又调度不上 | 回归风险（中） | F4、F6；推断 |
| 被搬走的组损失什么 | 排队位置；没有 gang 调度时已经跑起来的部分 Pod | 相同。另外优雅驱逐期间新旧两份同时存在，没有 gang 调度时旧的那份继续占着 GPU | 中性（略差） | F6 |
| 误搬部分在服务的组 | 原本在服务的组只要有一个新 Pod 调度不上就整组计入（`DESIGN.md` 8.5） | 沿用同一个信号时相同 | 中性（没有改善） | 第一轮报告 R3 |
| 滚动期间的 surge 组 | surge 组计入不可调度，可能把这个集群的份额永久挪走 | surge 组是独立对象，只挪它自己 | 预期变化（改善） | `DESIGN.md` 8.5 |
| 人工挪一个组 | WorkloadRebalancer 会重排整个 ModelServing | WorkloadRebalancer 可以只点名一个 ServingGroup | 预期变化（改善）；注意 F5，点名之后它可能被搬到别处而不是原地不动 | 推断 |

`InterpretHealth` 还有一个用途冲突。Karmada 用同一个健康值做三件事：应用故障转移的触发；优雅驱逐任务的结束条件（「新集群健康了就删旧的」）；Work 和绑定上给人看的健康列。把健康定义得很窄（只看调度不上）能避免误搬，但新集群上的组刚调度上、还没就绪，旧集群那份就会被删；健康列对「崩溃循环的组」显示健康。把它定义成 Ready 则会误搬。（F6，推断）

覆盖：GL-06、GL-08、GK-13 到 GK-16。

### 4.7 滚动升级和恢复

| 项 | 单集群（今天） | M | G | 分类（G 相对 M） | 证据 |
| --- | --- | --- | --- | --- | --- |
| 全局预算 | 有 | 没有。各集群同时滚，同时不可用数是各集群预算之和（`DESIGN.md` 9.1，待验证） | 有，上层 controller 逐组推进 | 预期变化（改善）。每个集群只有 1 个组时，M 的默认配置等于所有集群同时停 | 推断 |
| 预算的可靠程度 | 一个进程、一份 Pod 缓存 | 每个集群各自可靠 | 基于回传的状态（4.4）；#2148 指出跨组预算在 010 里已经不是原子的（Plan A 缺点 4），G 的窗口更大 | 回归风险 | volcano-sh/kthena#2148 |
| 替换一个组之后它在哪 | 原地 | 原集群 | 010 的做法是删对象再同名重建。G 下新对象重新调度；旧组刚删、GPU 还没在集群汇总里释放时，新组更可能去别的集群 | 回归：每次滚动都可能改变组在集群间的分布，新集群要拉镜像、加载模型，按依赖下发的 PVC 跟着走 | F3（新绑定按可用量排序）、F11；集群状态默认每 10 秒上报一次（`karmada:cmd/controller-manager/app/options/options.go:165`）；`010-doc:02-design.md` 5.4；需 Kind |
| 控制面的组对象何时消失 | 010：Pod 都没了对象才消失，期间一直占序号、计入预算 | 不适用 | 控制面的对象在成员上的 Pod 消失之前就没了（F11）。010 的「同名的新对象只能在旧对象消失之后建」在控制面上不再等价于「旧 Pod 已经没了」 | 回归风险：新旧两份可能同时占 GPU，或者落在同一集群时新对象撞上还没删完的旧 Work | F11；`010-doc:02-design.md` 5.4 第 3 步 |
| 原地更新（只改镜像） | 可用 | 可用 | 上层 controller 改组的 `spec`，成员原地更新 Pod，不换集群 | 中性。这是 G 下唯一不引起重新调度的滚动方式 | `010-doc:02-design.md` 第 9 节；推断 |
| 回滚 | 版本历史在本集群 | 每个成员各有历史 | 版本历史只在控制面；回滚是再滚一遍，再漂移一次 | 中性（略差） | 推断 |
| 成员集群故障 | 不适用 | 默认不迁移；开启集群故障转移后重新拆分，剩余集群接不下全部被驱逐的份额时，这部分份额一个都不放 | 默认同样不迁移；开启后每个组独立重新调度，放得下几个算几个 | 预期变化（改善）；但 F4 的挤兑同样适用 | `karmada:pkg/features/features.go:174-178`；推断 |

**控制面不可用时，成员集群里还有什么能工作：**

| 能力 | M | G |
| --- | --- | --- |
| 现有的组继续服务 | 能 | 能 |
| Role 级恢复（`RoleRecreate`） | 能 | 能，前提是组的 `spec` 自带模板（F1） |
| 整组恢复（`ServingGroupRecreate`、整组 Pod 丢失） | 能，成员的 kthena 本地重建 | 不能。成员只会把组标成 `Failed`，没人删、没人建 |
| 进行中的滚动 | 各成员按已经收到的模板继续滚完 | 停在当前步。如果停在「旧组已删、替代组还没建」，容量缺口一直持续到控制面恢复 |
| 成员侧扩缩容 | 模式 A 下能 | 没有 |
| 新的模板下发、状态汇总、换集群 | 不能 | 不能 |

M 这一列是 `DESIGN.md` 9.7 的推断；G 这一列是由 010 的分工（`010-doc:02-design.md` 第 3 节的表）推出的。两列都没有在 Kind 上验证。

覆盖：GU-03、GU-08、GK-07、GK-08、GK-17、GK-18、GK-19。

### 4.8 可用性和性能

设 N 是组数，C 是集群数。

| 项 | M | G | 证据 |
| --- | --- | --- | --- |
| 控制面对象数 | 1 个模板 + 1 个 ResourceBinding + C 个 Work | 1 个 ModelServing + N 个 ServingGroup + N 个 ResourceBinding + N 个 Work，滚动时再加 surge | 推断 |
| 每个 Work 的大小 | 一份 ModelServing | 一份自带模板的 ServingGroup，量级相同，但有 N 份 | F1 |
| 调度次数 | 每次副本数变化调度 1 次 | 每个组各 1 次；每次重建、每次驱逐再各 1 次 | 推断 |
| 成员上的状态写 | ModelServing 的 status | 每个 Pod 一生约 4 次组状态写；一次 e2e 里 164 个组被更新 1,573 次，是 51 个 ModelServing 被更新 668 次的 2.35 倍 | 实测-2148 |
| 经过 Karmada 的写 | 成员 ModelServing 的每次状态写，在控制面最多引出 3 次写 | 成员上每次组状态写，在控制面最多引出 3 次写（Work、ResourceBinding、模板），另有 2 条事件 | F7 |
| 依赖对象 | 每个 ConfigMap、Secret、PVC 一个附属绑定 | 同样一个附属绑定，但它要跟随 N 个组的调度结果，每次有组换集群都要更新 | 第一轮报告 SC4；推断 |

一个量级估算（推断，不是测量）：100 个组、每组 8 个 Pod 的一次全量滚动，按 #2148 的每 Pod 约 4 次状态写，是约 3,200 次成员侧写、最多约 9,600 次控制面写。karmada-controller-manager 默认 40 QPS，并且是所有 controller 共用的；只算这一项也要占满 4 分钟。push 模式下这些写都由 controller-manager 发出，pull 模式下 Work 的那一次由各成员的 agent 发出。写入如果内容没变会被跳过，实际值会低于上限，要在 Kind 上数。

这个延迟不只是「控制面显示得慢」。4.4 已经说明，G 的上层 controller 用这份状态算预算，所以状态慢多少，滚动就慢多少，误判的窗口就有多大。

其他可用性变化：

- 多一个要运维的组件（上层 controller，对着 karmada-apiserver 运行，需要自己的选主、升级、监控）。
- 版本组合从「控制面 CRD、各成员 kthena」变成「上层 controller、控制面两个 CRD、各成员的 ServingGroup controller 和 CRD」。
- 按集群的自治变弱（4.7 的表）。

覆盖：GK-20。

### 4.9 升级和迁移

| 项 | M | G | 分类（G 相对 M） | 证据 |
| --- | --- | --- | --- | --- |
| 成员集群需要的 kthena | 上游 main 原版 | 带 ServingGroup CRD 和 controller 的版本，上游没有；#2148 明确要求暂缓实现 PR | 回归（依赖一个没做出的上游决定） | volcano-sh/kthena#2148 |
| Karmada 需要什么 | 默认门控，加两条配置（清资源模型、部分集群不部署 scheduler-estimator） | `MultiplePodTemplatesScheduling`（alpha，v1.15 起）和 `SchedulingOvercommitProtection`（alpha，v1.18 起）；节点级准确还要每个成员有可达的 scheduler-estimator | 回归风险：alpha 的行为和字段可能变 | F3、F4 |
| 接管已有的单集群 ModelServing | 用 `Overwrite` 接管，UID 不变、Pod 不重启（`DESIGN.md` 9.6，源码加推断） | 两步：先在成员上把 Pod 的 owner 从 ModelServing 换成 ServingGroup（010 的在线迁移；#2148 指出它和 PR #2126 的校验 webhook 冲突）；再让控制面上层 controller 建出同名的组模板去接管成员上的组，并把成员上的 ModelServing 以不级联的方式拿掉 | 回归（高）：两次在线迁移，第二次没有现成的设计 | volcano-sh/kthena#2148 的 Plan A 缺点 2；`010-doc:06-live-upgrade.md`；推断 |
| 退出多集群 | 模板带 `preserveResourcesOnDeletion` 时保留成员对象 | 反向做上面两步 | 回归 | 推断 |
| 两种方式并存 | 不适用 | G（新 CRD）和 M 可以在同一个控制面并存：两个 kind 各有解释器。但同一个 ModelServing 只能选一种；选错策略的后果见 4.1 | 中性 | F8 |
| 已有模板后再改解释器或开门控 | Kind-M：C10、C10b，组被清零或重建 | 见第 5 节 P1 | 形态改变 | 见第 5 节 |

覆盖：GK-21、GK-25。

### 4.10 变体 G-c：用 `replicas: 1` 的 ModelServing 当单位

G-c 的好处是成员集群跑上游原版 kthena：不需要新 CRD，Pod 的 owner 不变，对象自带模板，`unschedulableReplicas` 已经有。它因此避开了 F1、F13 和对 #2148 结论的依赖，整组恢复也回到成员本地（成员的 ModelServing controller 就在那里）。

它和 G 不同的回归：

| 项 | G-c 的行为 | 分类 | 证据 |
| --- | --- | --- | --- |
| 名字和标签 | 子对象叫 `<name>-<k>`，组名变成 `<name>-<k>-0`，Pod 的 `modelserving.volcano.sh/name` 变成 `<name>-<k>`。按 `modelserving.volcano.sh/name: <name>` 选 Pod 的 ModelServer、Service、亲和规则全部选不中 | 回归（高） | `kthena:examples/kthena-router/ModelServer-prefill-decode-disaggregation.yaml:11`；`kthena:examples/models/deepseek-v4-flash/router.yaml:26`；`kthena:examples/kube-scheduler-pod-affinity/modelserving-host-affinity.yaml:38` |
| 用户写什么 | 需要一个新的顶层 kind，或者父子都用 ModelServing、靠标签区分。后一种写错策略时父对象也被下发 | 回归风险 | 推断 |
| 和副本数挂钩的校验 | 子对象 `replicas: 1`，模板里的整数 `maxUnavailable: 2`、`partition: 3`、不带 `maxSurge` 的百分比都会被成员 webhook 拒绝，上层 controller 必须改写子对象的 `rolloutStrategy` | M 的问题保留 | `kthena:pkg/model-serving-controller/webhook/validator.go:239-256` |
| 解释器 | 要多组件估算就必须给 ModelServing 实现 `componentResource`。这时如果同一份解释器还带着 M 用的 `replicaRevision`，每个子对象的 `spec.replicas` 都会被写成 0（Kind-M 的 C3）。M 和 G-c 不能共用一个控制面 | 回归风险（高） | F8；`kind-C3-component-resource-trap.log` |
| 接管存量 | 组名变了，现有 Pod 不能被新的子对象收养，只能逐组重建 | 回归（高） | 推断 |
| 版本历史 | 每个子对象各有一套 ControllerRevision | 中性（略差） | 推断 |
| 自动扩缩容 | 成员上的 kthena autoscaler 只能按子对象的名字一个个配，并且它改 `spec.replicas` 会破坏「一个子对象一个组」 | 回归 | F9 |

G-c 和 G 共有的：F3、F4、F5、F6 的全部风险，状态作为决策输入的问题（只是整组恢复不再受影响），分发方式变少，对象数和写入量。

## 5. 选项 M 在 Kind 上暴露的问题，在 G 下各自怎样

任务说明写的是八个，列出来是九条，这里九条都回答。「G」指新 CRD 的做法，G-c 不同时单独说明。

**P1 已有模板之后再装解释器，组被清零**（Kind-M：C10、C10b）：形态改变。
- 根因是绑定从「非工作负载」变成「工作负载」的瞬间，binding controller 按 0 调 `ReviseReplica`。ServingGroup 的解释器不实现 `ReviseReplica`，这条路径不走（F8，静态）。
- 新的形态（推断，需 Kind）：后装解释器或后开门控，会让已有的绑定第一次带上组件，从而触发重新调度；所在集群已满的组可能被搬走（F5）。
- G-c：如果解释器带 `replicaRevision`，C10 和 C3 原样重现。

**P2 不装解释器时，`Divided` 变成每个集群完整副本**（Kind-M：C0）：形态改变。
- G 下不装解释器，或者门控没开而解释器只实现了 `componentResource` 时，绑定是「非工作负载」。每个组仍然只去一个集群（spread constraint 仍然生效），但可用量对所有集群都是「无穷大」，排序退化成按名字，所有组进同一个集群（F3，推断）。
- 策略没写 `Divided` 时，即使装了解释器也忽略容量（F3，静态）。
- 策略写成静态权重时，每个组被下发到所有候选集群（F3，静态加推断）。
- 三种情况都没有报错。

**P3 默认资源模型对 GPU 估算为 0**（Kind-M：C2a）：保留。
- 多组件路径同样受资源模型约束（F10）。结论是推断，任务 014 的包级实测是在「无资源模型」下做的，没有覆盖这个组合。
- 规避办法相同（清空 `Cluster.spec.resourceModels` 或关 `CustomizedClusterResourceModeling`）。

**P4 整组求和遇上节点级估算**（Kind-M：C2c）：消除，有条件。
- 门控打开、成员有 scheduler-estimator 时走组件集估算。任务 014 的包级实测里，节点级组件集估算在四个场景下全对（`evidence/logs/estimator-probe.log`）。这是 G 最实在的技术收益，而且不需要改 Karmada 调度器。
- 门控没开时，如果解释器另外实现了 `replicaResource`，退回「副本数 1 + 整组求和」，和 M 相同；没有实现就是 P2 的情况。
- 估算器仍然看不到 gang、队列配额、网络拓扑，高估仍会发生，换集群的机制仍然需要。

**P5 `componentResource` 加 `replicaRevision` 把副本数写成 0**（Kind-M：C3）：G 消除，G-c 保留。
- G：ServingGroup 的解释器只要不实现 `replicaRevision` 就不会触发（F8）。这一点要用测试钉住（GL-01）。
- G-c：见 4.10。

**P6 和副本数挂钩的校验；各集群同时滚动**（Kind-M：C6；`DESIGN.md` 9.1）：G 消除，G-c 保留。
- G：校验按全局 N，预算全局算一次。代价是预算的执行依赖回传的状态（4.4、4.7）。
- G-c：子对象 `replicas: 1`，校验问题更容易踩到。

**P7 跨集群缩容不看就绪情况**（Kind-M：`kind-C5-pingpong-and-directed-move.log:101-107`）：设计上消除。
- 由上层 controller 选对象。需要把「调度不上优先」这一档和排序依据放进组的 status（F2、F13），并且接受依据是回传状态（刚就绪的组可能被删）。

**P8 没有去处时的循环**（Kind-M：C5 的结果 3）：形态改变。
- 用应用故障转移时没有「有去处才动」的预检（F6）。推断的循环见 4.6 的表。和 M 的区别是每一轮都删建一遍 Pod。
- 要真正消除，G 同样需要一个自己写的驱逐组件，工作量和 M 的第 2 阶段是同一类。

**P9 成员侧自动扩缩容和 `Divided` 不能共存**（Kind-M：C7）：变差。
- M 至少有两种明确可用的组合（成员侧扩缩容配 Duplicated；控制面改副本数配 Divided）。G 下三种现有的自动扩缩容都不可用（4.5、F9），改 role 副本数还会撞上 F5。

**G 新增、M 没有的失败方式**（编号供 6.2 引用）：

| # | 失败方式 | 证据 |
| --- | --- | --- |
| N1 | 一批同时创建的组挤进同一个集群，超出它的真实容量 | F4 |
| N2 | 正在运行的组因为一次重新调度被整组搬走（改 role 副本数、改策略、点名重平衡、后装解释器） | F5 |
| N3 | 每次滚动和整组重建都重新调度，分布漂移、冷启动、PVC 跟着走 | 4.7 |
| N4 | 控制面不可用时整组恢复停摆，滚动停在容量缺口上 | 4.7 |
| N5 | 用过期或冻结的状态做删除决定；不可达集群上的组一直被算作 Ready | 4.4 |
| N6 | 换集群期间两个集群同时上报，状态取哪一份没有定义 | 4.4 |
| N7 | 组换集群后 `observedGeneration` 对不上，一直被当作未观测 | 4.4 |
| N8 | 组对象长期没有集群、没有 Pod，不排队，也不触发节点扩容 | 4.5 |
| N9 | 健康定义的用途冲突：误搬，或者旧副本删得太早 | 4.6 |
| N10 | 成员上的驱逐保护和 PDB 没有 ModelServing 可依据 | 4.2 |
| N11 | plugin 的 `OnModelServingSync` 在控制面上拿到错误集群的 client | 4.2 |
| N12 | 控制面的组对象先于成员上的 Pod 消失，同名重建可能和旧 Pod 并存 | F11 |
| N13 | 经过 Karmada 的写入量随 Pod 数增长，状态延迟直接拖慢滚动 | 4.8 |
| N14 | 策略误选 ModelServing 本身，成员重复建组 | 4.1 |

## 6. 汇总表

### 6.1 用户可见的差异：M 和 G

| 方面 | M | G |
| --- | --- | --- |
| 用户写的对象 | ModelServing + 选中它的策略 | ModelServing + 选中 ServingGroup 的策略（`Divided`、不能是静态权重、spread 为 1、`propagateDeps`） |
| 管理员要装的 | 一份解释器，两条 Karmada 配置 | 上层 controller、ServingGroup 的 CRD 和解释器、两个 alpha 门控、每个成员的 scheduler-estimator、成员侧的 ServingGroup controller |
| 成员集群跑什么 | 上游原版 kthena | 新的 ServingGroup controller（上游没有） |
| 分发方式 | Duplicated、静态权重、动态拆分 | 只有按容量逐组放置 |
| `replicas` | 全局总数，成员对象被改写 | 全局的组对象数 |
| 组一级滚动预算、`partition` | 每个集群按份额各算一份 | 全局一份 |
| 模板里的整数 `maxUnavailable` 等 | 可能被小份额集群拒绝 | 按全局 N 校验 |
| 校验报错 | 异步，在成员上 | 同步，在控制面（webhook 装在控制面时） |
| 控制面上按组查看 | 没有 | 有 |
| 组名 | 各集群重复 | 全局唯一 |
| 成员上的 `kubectl get modelserving` | 有（份额） | 没有 |
| 缩容删谁 | 跨集群不看就绪 | 全局排序（依据是回传的状态） |
| 扩容 | 一次决定，增量按估算分 | 每组各自调度 |
| 容量不够时 | 动态拆分下，这一次要放的副本全部不放 | 放不下的那几个组不放 |
| 改 role 副本数 | 组内扩缩 | 可能触发整组换集群 |
| kthena autoscaler | 成员上可用（配 Duplicated） | 不可用 |
| FederatedHPA | 可以指向模板（待验证） | 不可用 |
| 调度不上的组换集群 | 要改 karmada-descheduler；按组、有预检的设计 | 现成的应用故障转移按健康触发，没有预检；要精确同样得写新组件 |
| 滚动之后组在哪个集群 | 原集群 | 可能换集群 |
| 整组恢复 | 成员本地 | 需要控制面，可能换集群 |
| 控制面不可用 | 成员照常自愈、滚动 | 只有 Role 级恢复 |
| 驱逐保护（`evictionStrategy`） | 每个集群按份额 | 未定义 |
| 接管存量单集群对象 | `Overwrite`，Pod 不重启 | 两次在线迁移 |
| 控制面写入量 | 随 ModelServing 数和集群数增长 | 随 Pod 数增长 |

### 6.2 G 的回归风险

严重度看用户受到的影响，可能性看在默认配置和常见用法下会不会遇到。

| # | 风险 | 严重度 | 可能性 | 缓解 | 证据类型 |
| --- | --- | --- | --- | --- | --- |
| RG-01 | 010 的两个 controller 不能直接用：成员侧不建 Pod，上层没有 Pod 缓存 | 高 | 必然 | 自带模板的新 ServingGroup API；重写上层 controller 的输入 | 静态（F1、F2） |
| RG-02 | 用回传的状态做删除决定：滚动预算被突破，缩容删到刚就绪的组（N5） | 高 | 中 | 回显 generation、集群 Ready 条件、状态时间戳三重新鲜度检查；不新鲜的组既不算就绪也不作为删除候选 | 推断，需 Kind |
| RG-03 | 控制面不可用时整组恢复停摆，滚动停在容量缺口上（N4） | 高 | 低到中（取决于控制面的可用性） | 成员侧在同一个对象里原地整组重建；滚动改成「先建后删」 | 推断，需 Kind |
| RG-04 | kthena autoscaler、FederatedHPA、成员侧 HPA 都不可用 | 高 | 必然（对用自动扩缩容的用户） | 新写一个控制面的指标聚合和伸缩组件 | 静态（F9） |
| RG-05 | 正在运行的组因重新调度被整组搬走（N2） | 高 | 中（集群接近满载时改 role 副本数就会遇到） | 向上游提修复（把本组占用加回去），或上层 controller 给每个已放置的组固定集群 | 静态加推断（F5），需 Kind |
| RG-06 | 同时创建的组挤进同一个集群（N1） | 中到高 | 高（默认门控下，每次扩容、滚动、故障转移） | 打开 `SchedulingOvercommitProtection`；或上层 controller 逐个创建并等放置完成 | 静态（F4），需 Kind |
| RG-07 | 门控没开、解释器没装、策略没写 `Divided` 或写成静态权重：所有组进同一个集群，或忽略容量，或每个组下发到所有集群，都不报错 | 高 | 中（配置错误） | 上层 controller 启动时检查并拒绝工作；文档 | 静态加推断（F3），需 Kind |
| RG-08 | 每次滚动和整组重建都改变分布，冷启动、PVC 跟着走（N3） | 中 | 高 | 能原地更新的走原地更新；重建时固定原集群，只有放不下才放开 | 推断，需 Kind |
| RG-09 | 按 Ready 定义健康：加载模型或单个 Pod 故障的组被整组搬走，容忍时间短于冷启动时永远起不来 | 高 | 高（如果这样定义） | 窄定义，只看调度不上；需要组的 status 新增这个信号 | 静态（F6、F13）加推断 |
| RG-10 | 窄定义的健康：新集群刚调度上就删旧副本；健康列失真（N9） | 中 | 中 | 旧副本本来就没有运行的 Pod 时可接受；文档说明健康列的含义 | 推断 |
| RG-11 | 故障转移的计时在内存里，抖动或重启清零，且不从 `unschedulableSince` 算起 | 中 | 中 | 自己写驱逐组件，用成员上报的时间 | 静态（F6） |
| RG-12 | 没有去处时的循环，每轮删建 Pod | 中 | 中 | 驱逐前预检；同上需要自己的组件 | 推断，需 Kind |
| RG-13 | 一个集群整体出问题时，上面的组同时被驱逐并挤进另一个集群 | 中 | 低到中 | 驱逐限速；RG-06 的缓解 | 推断 |
| RG-14 | 组长期没有集群：不排队，不触发节点扩容，`unschedulableReplicas` 的口径对不上（N8） | 中 | 中 | 定义并上报「未放置」状态；允许按策略放到一个指定集群去排队 | 静态（F12）加推断 |
| RG-15 | 换集群后 `observedGeneration` 对不上，组一直未观测（N7） | 中 | 高（不处理就必现） | 解释器按 `resourcetemplate.karmada.io/generation` 换算 | 推断，离线可验证 |
| RG-16 | 换集群期间两份状态，取哪份没有定义（N6） | 中 | 中 | `AggregateStatus` 里写明规则并测试 | 推断，离线可验证 |
| RG-17 | 控制面对象先于成员 Pod 消失，同名重建和旧 Pod 并存（N12） | 中 | 中 | 上层 controller 等 Work 消失后再建；或加自己的 finalizer | 静态（F11）加推断 |
| RG-18 | `evictionStrategy` 和 PDB 在成员上失去依据（N10） | 中 | 中（节点维护时） | 重新定义为按本集群的组计数，并写明不是全局 | 静态加推断 |
| RG-19 | plugin 钩子在控制面上拿到错误集群的 client（N11） | 中 | 取决于是否用 plugin | 上层 controller 不调 `OnModelServingSync`，或给钩子加作用域 | 静态 |
| RG-20 | Duplicated、静态权重、按集群灰度表达不了 | 中 | 取决于用法 | 这些用法继续用 M | 推断 |
| RG-21 | 存量接入需要两次在线迁移；和 PR #2126 的 webhook 冲突 | 高 | 必然（对存量用户） | 只支持新建的 ModelServing 走 G；存量留在 M | 实测-2148 加推断 |
| RG-22 | 状态写入量随 Pod 数增长，拖慢滚动（N13） | 中 | 规模大时高 | 成员侧合并状态写；调高 QPS；账本不回传到控制面 | 实测-2148 加推断，需 Kind |
| RG-23 | 默认资源模型下 GPU 估算为 0（和 M 相同） | 高 | 高（默认配置） | 同 M | 推断，需包级实测 |
| RG-24 | 依赖两个 alpha 门控和一个未定的上游决定 | 中 | 不适用 | 跟踪上游；G-c 可以避开后者 | 静态 |
| RG-25 | G-c：标签值变化让现有 selector 失效；接管只能重建；和 M 的解释器互斥 | 高 | 必然（选 G-c 时） | 模板加公共标签并改 ModelServer；一个控制面只用一种方式 | 静态加推断 |
| RG-26 | 策略误选 ModelServing，成员重复建组（N14） | 高 | 低 | 上层 controller 检测到自己管理的 ModelServing 带有 Karmada 的策略标签时报错并停止 | 推断 |

### 6.3 M 的问题在 G 下：消除、保留还是改变

| # | M 的问题（Kind 证据） | G（新 CRD） | G-c | 备注 |
| --- | --- | --- | --- | --- |
| P1 | 后装解释器清零组（C10、C10b） | 形态改变：不写 0，但已满集群里的组可能被搬走 | 解释器带 `replicaRevision` 时保留 | 推断，需 Kind |
| P2 | 不装解释器等于每个集群完整副本（C0） | 形态改变：所有组进同一个集群，或忽略容量，或每个组下发到所有集群 | 同 G | 推断 |
| P3 | 默认资源模型下 GPU 估算为 0（C2a） | 保留 | 保留 | 推断 |
| P4 | 整组求和遇上节点级估算（C2c） | 消除（门控打开时） | 消除（门控打开时） | 包级实测支持 |
| P5 | `componentResource` 陷阱（C3） | 消除（不实现 `replicaRevision`） | 保留，并且和 M 互斥 | 静态 |
| P6 | 按份额校验、各集群同时滚动（C6） | 消除（预算依赖回传状态） | 校验保留，预算由上层 controller 编排 | 静态加推断 |
| P7 | 跨集群缩容不看就绪（C5b） | 设计上消除（依据是回传状态） | 同 G | 推断 |
| P8 | 没有去处时的循环（C5） | 形态改变 | 形态改变 | 推断 |
| P9 | 成员侧扩缩容和 `Divided` 冲突（C7） | 变差：三种自动扩缩容都不可用 | 变差 | 静态 |

## 7. G 需要的针对性回归覆盖

如果决定做 G 的原型，下面是最小的覆盖。期望值按「用户不应该受到的影响」写；标了「记录实际值」的是目前只有推断、要靠这次测试定结论的。

### 7.1 单元测试和包级测试

| 编号 | 被测 | 用例 | 期望 |
| --- | --- | --- | --- |
| GU-01 | 上层 controller 的计划 | 组的回传状态不新鲜（回显的 generation 和模板不一致） | 这个组占位置，不算就绪，也不是缩容或替换的候选 |
| GU-02 | 同上 | 组没有被调度到任何集群 | 算不可用，占序号；不被反复删建；ModelServing 的状态里有明确的原因 |
| GU-03 | 同上 | surge 组还没调度出去或还没就绪 | 不删任何旧组 |
| GU-04 | 同上 | 缩容，候选里有调度不上、未就绪、Running 三类，分布在不同集群 | 顺序是调度不上、未就绪、Running；同类比 deletion cost，再比序号 |
| GU-05 | 同上 | `partition`，序号有洞 | 保护的是全局序号 `[0, P)` |
| GU-06 | 同上 | 某个集群 NotReady | 该集群上的组按「未知」算，不计入可用，也不删 |
| GU-07 | 同上 | 优雅驱逐期间收到两个集群的状态 | 只采信当前调度结果里的那个集群 |
| GU-08 | 同上 | 同名重建，旧的 Work 还在 | 等旧的消失后再建；任何时刻控制面只有一个同名对象 |
| GU-09 | 同上 | 只改 role 的 `replicas` | 只改组的 `spec`，不删建组对象 |
| GU-10 | ModelServing 校验（控制面） | `replicas: 4`、整数 `maxUnavailable: 2`；`partition: 8` 大于 N | 前者通过，后者被拒，都是同步的 |
| GU-11 | 成员侧渲染 | 用自带模板的 ServingGroup 渲染 Pod，对比今天的 ModelServing 渲染结果 | 名字、七个 `modelserving.volcano.sh/*` 标签（`kthena:pkg/apis/workload/v1alpha1/labels.go:21-34`）、PodGroup 和 headless Service 的名字逐一相同 |
| GU-12 | 成员侧 ServingGroup controller | 本集群没有 ModelServing、没有 ControllerRevision，组没有 ownerReference | 照常建 Pod、做 Role 级恢复（对照 `kthena-010:.../sgcontroller/reconcile.go:107-113` 的现状是不动） |
| GU-13 | Karmada 调度器（包级探针，方法同任务 014 的 estimator-probe） | 多组件绑定已在集群 A，A 没有再放一整组的余量，触发一次重新调度 | 记录实际值。不应该换集群；如果换了，RG-05 成立 |
| GU-14 | Karmada 集群汇总估算器（包级探针） | 默认资源模型、组件请求 GPU，调 `MaxAvailableComponentSets` | 记录实际值。推断是 0 |
| GU-15 | 上层 controller 的启动检查 | 门控没开；策略不是 `Divided`，或是静态权重；ModelServing 自己带着 Karmada 的策略标签 | 每一种都报错并停止创建组 |

### 7.2 解释器 testdata（ServingGroup，`karmadactl interpret`）

操作一列用 `DESIGN.md` 4.2 的写法：定制 YAML 里的字段名，括号里是对应的解释器操作。

| 编号 | 操作 | 输入 | 期望 |
| --- | --- | --- | --- |
| GL-01 | `--check` | 整份定制 | `componentResource`、`statusReflection`、`statusAggregation`、`healthInterpretation`、`dependencyInterpretation` 通过；`replicaRevision` 和 `retention` 是 UNSET |
| GL-02 | `componentResource`（`GetComponents`） | 2 个 role，`replicas: 2`，`workerReplicas: 3` | 每个 role 两个组件（entry、worker），副本数 2 和 6，各带自己的请求和 nodeClaim |
| GL-03 | `componentResource` | role 的 `replicas: 0`；Pod 没写请求 | 不报错；组件副本数 0 或省略，请求为空 |
| GL-04 | `statusReflection`（`ReflectStatus`） | 成员 generation 是 1，模板 generation 注解是 7 | 整份 status，加 `generation` 和 `resourceTemplateGeneration` |
| GL-05 | `statusAggregation`（`AggregateStatus`） | 一项；两项（驱逐中）；一项 `applied=false`；还没有 status | 一项时照抄并换算 `observedGeneration`；两项时按写明的规则取；后两种不保留上一次的 Ready |
| GL-06 | `healthInterpretation`（`InterpretHealth`） | 有 Pod 调度不上；都调度上但没就绪；一个 Pod 崩溃循环；没有 status | 第一种不健康，其余按选定的定义；没有 status 的结果要写明（它决定刚创建的组是否开始计时） |
| GL-07 | `dependencyInterpretation`（`GetDependencies`） | 两个 role 引用同一个 ConfigMap，另有 Secret、PVC、ServiceAccount | 各一条，去重 |
| GL-08 | `healthInterpretation`、`statusAggregation` | 成员跑的是没有「调度不上」信号的旧版本 | 按健康处理，不触发驱逐 |

### 7.3 Kind 场景

环境沿用任务 014 的（一个控制面，m1 两个 2-GPU 的 worker，m2、m3 各一个 8-GPU 节点，m3 是 pull 模式），另外每个成员部署 scheduler-estimator，清空资源模型。除了 GK-02（关 `MultiplePodTemplatesScheduling`）和 GK-04 的第一遍（关 `SchedulingOvercommitProtection`），都打开两个 alpha 门控。每个场景同时在成员上直接数 Pod，不只看控制面的状态。

| 编号 | 场景 | 期望观察 |
| --- | --- | --- |
| GK-01 | 3 个组，每组 4 个 Pod × 1 GPU；m1、m2 | 控制面 3 个 ServingGroup、3 个绑定，每个绑定恰好一个集群；m1 最多 1 组，其余在 m2；全部 Running；成员上没有 ModelServing；Pod 标签和单集群一致 |
| GK-02 | 同 GK-01，但不开 `MultiplePodTemplatesScheduling`，或不装解释器 | 记录实际值。推断：所有组进同一个集群，没有报错 |
| GK-03 | 同 GK-01，策略不写 `replicaScheduling`；再换成静态权重 | 记录实际值。推断：前者忽略容量，后者每个组出现在所有集群 |
| GK-04 | 一次从 1 扩到 8，不开 `SchedulingOvercommitProtection`；再开着重跑 | 关：记录有几个组被放进放不下的集群。开：每个集群的组数不超过真实容量 |
| GK-05 | 所有集群都满，再扩 1 个组 | 这个组的绑定是 `Scheduled=False`，没有 Work，任何集群都没有它的 Pod；ModelServing 的状态里看得出原因；腾出容量后记录多久被放置 |
| GK-06 | 一个 Running 的组所在集群没有再放一整组的余量，把它的一个 role 的 `replicas` 加 1 | 这个组的 Pod UID 不变，只多出新的 Pod。如果组被搬走，RG-05 成立 |
| GK-07 | 4 个组分在两个集群，`maxUnavailable: 1`、`maxSurge: 0`，改模板 | 在成员上采样：任何时刻最多 1 个组未就绪；记录每个被替换的组新旧所在集群和每步耗时 |
| GK-08 | 同 GK-07，`maxSurge: 1`，原集群已满 | surge 组出现在另一个集群；滚动能完成；记录滚动前后各集群的组数 |
| GK-09 | `partition: 2`，N 为 4 | 序号 0、1 不论在哪个集群都保持旧版本；2、3 是新版本 |
| GK-10 | `replicas: 4`、整数 `maxUnavailable: 2`，两个集群各分到 1 和 3 | 控制面接受，滚动正常（对照 Kind-M 的 C6） |
| GK-11 | 重放 Kind-M 的缩容：10 缩到 8，其中一个集群有 1 个 Running、1 个调度不上的组 | 被删的是调度不上的组和排序靠后的组；Running 组的 Pod UID 不变；没有集群被清空 |
| GK-12 | 停住状态回传（停 karmada-controller-manager 或 agent）；让一个原本未就绪的组在成员上变成 Ready；然后缩容 1 个 | 不删这个组；上层 controller 报告状态不新鲜并等待 |
| GK-13 | 用估算器看不到的 nodeSelector 让 m1 上的一个组调度不上，m3 有容量 | 超过阈值后这个组的绑定换到 m3；m1 上其他 Running 组的 Pod UID 不变；记录旧副本保留了多久 |
| GK-14 | 一个组已调度但就绪探针 15 分钟不过；另一个组有一个 Pod 崩溃循环 | 两个组都不被换集群 |
| GK-15 | 同 GK-13，但所有集群都满 | 有预检时：不驱逐，有事件。没有预检时：记录循环周期和 Pod 被删建的次数 |
| GK-16 | 同 GK-13，但 m3 上也调度不上 | 记录是否搬回 m1、间隔多久；不应该无限来回 |
| GK-17 | 停掉上层 controller 或 karmada-apiserver。在 `RoleRecreate` 的组里删一个 Pod；在 `ServingGroupRecreate` 的组里删 entry Pod；在滚动进行到一半时停 | 第一种：Role 在成员本地重建。第二种：记录组是否停在 `Failed`、恢复后多久重建。第三种：记录容量缺口持续多久 |
| GK-18 | 让 m1 不可达（断开 apiserver），然后改模板开始滚动 | m1 上的组不被算作可用；滚动不会因为它们「冻结的 Ready」而多删别的组 |
| GK-19 | `ServingGroupRecreate`，杀掉一个组的 entry Pod | 组被重建；记录它是否留在原集群、组名是否不变、从故障到新 Pod 就绪的时间，并和单集群的时间比 |
| GK-20 | 4 组 × 4 Pod 的一次滚动，G 和 M 各跑一遍 | 从 karmada-apiserver 的审计日志或请求计数里数 Work、ResourceBinding、模板的写次数；记录 G 和 M 的比值，以及成员状态变化到控制面可见的延迟分布 |
| GK-21 | 单集群上已有 3 个 Running 组的 ModelServing，接入 G；再退出 | Pod UID 和重启次数全程不变；记录每一步的人工操作 |
| GK-22 | pull 模式成员重跑 GK-01、GK-13 | 行为和 push 一致 |
| GK-23 | 成员装 Volcano，`schedulerName: volcano`，重跑 GK-01、GK-13 | gang 不满足时整组没有 Pod 运行；调度不上的信号仍然出现 |
| GK-24 | 带 `evictionStrategy` 的 ModelServing，对成员节点 `kubectl drain` | 记录实际值：驱逐是被预算拦住、全部放行，还是全部拒绝 |
| GK-25 | 已有运行中的组，再装解释器或再开门控，然后更新一次模板 | Pod UID 不变。如果组被搬走，P1 的新形态成立 |
| GK-26 | 控制面建 `AutoscalingPolicy` 和 FederatedHPA 指向 ModelServing | 记录实际值。推断：前者没有 Pod 可抓，后者 `AbleToScale=False`、原因 `FailedGetBinding` |

## 8. 未验证的风险

下面这些靠读代码定不了结论。

1. **F5 是否真的会搬走运行中的组。** 结论由四段代码连起来推出，没有执行过。它决定了 role 级扩缩容在 G 下能不能用，优先级最高（GU-13、GK-06）。
2. **F10：默认资源模型在多组件路径上是不是把 GPU 估算成 0。**（GU-14）
3. **回传状态的实际延迟。** 路径和限速是静态的，延迟分布要测（GK-20）。它决定全局滚动预算的可信程度，也决定大规模滚动要多久。
4. **写入量的实际比值。** #2148 的 2.35 倍是 010 在单集群 e2e 里的数；经过 Karmada 之后的放大是上限估算，内容相同的写会被跳过。
5. **应用故障转移在「每个绑定一个集群」下的完整时序**：旧副本保留多久、没有去处时是否如推断的那样循环、驱逐任务结束后是否搬回原集群（GK-13、GK-15、GK-16）。
6. **同名重建的时序。** 控制面的新对象和成员上还没删完的旧对象、旧 Work 相遇时，Karmada 和成员 controller 各自怎么处理（GK-07、GK-19）。
7. **Volcano gang 调度下的全部行为。** 和 M 一样没有覆盖。
8. **成员上的驱逐 webhook 和 PDB 在没有 ModelServing 时的行为**（GK-24）。
9. **存量接入的第二步**（把成员上已有的 ServingGroup 交给控制面，并拿掉成员上的 ModelServing）没有任何现成设计，可行性未知。
10. **上游的方向。** #2148 没有结论；`MultiplePodTemplatesScheduling` 还是 alpha，`DESIGN.md` 第 10 节引用的上游工作（PR #8084、issue #5115、#7895）本次没有重新核对。任何一个有变化，本文的静态事实都要重新核对。
11. **用户的实际部署形态**：每个集群几个组、是否用 kthena autoscaler、是否需要「每个地域一份完整副本」。这三项决定第 9 节的判断往哪边倒，本次分析拿不到。
12. **G 的静态事实只核对了 Karmada master `6d007132`。** 已发布版本里多组件调度、组件副本变化触发重调度的代码可能不同。

## 9. 判断

**哪个选项对用户更好，取决于两件现在不知道的事：上游会不会采纳 ServingGroup CR（X1），以及典型部署里每个集群有几个组（X2）。在这两件事明确之前，M 更适合作为第一步。**

理由分三层。

第一层，G 的收益是真实的，但其中最大的两项不是只有 G 能给。
- 节点级的组件集估算，是因为「每个单位整个放一个集群」才用得上。G-c 不用新 CRD 也能得到它；M 的第 3 阶段是让调度器在拆分副本时用同一个估算器，也能得到它，只是要等上游。
- 全局滚动预算，本质是「有一个能看到所有集群的编排者」。M 下有可能由一个只编排下发节奏的小组件得到（用策略的暂停下发或按集群的 `partition` 逐个集群放行，`DESIGN.md` 9.1 提到了这两个手段，都还没有验证），成员集群的自治不受影响。它做不到「跨集群的单组粒度」，但对每个集群有多个组的部署可能已经够用。
- 只有 G 能给的是：按组的对象身份、全局唯一的组名、按组粒度的缩容选择、跨集群的 surge。

第二层，G 的代价里有三项是结构性的，不是多写点代码能消掉的。
- 组一级的生命周期依赖控制面在场（RG-03）。这改变了「控制面挂了，业务照常」这个多集群用户通常默认的性质。
- 决策基于异步回传的状态（RG-02）。010 的经验是同进程一跳都要复核；跨集群三跳只能做弱复核。
- 自动扩缩容要从头设计（RG-04），而且 role 级伸缩和 Karmada 的重调度触发条件直接冲突（RG-05）。

第三层，G 的前提今天不具备：010 的 controller 不能直接复用（RG-01），存量迁移没有路径（RG-21），依赖两个 alpha 门控（RG-24），上游对 ServingGroup CR 没有结论。kthena#2148 把 CR 路线列为未决的讨论，并要求暂缓实现 PR。在单集群里还没有定论的对象模型，不适合先成为多集群方案的地基。

什么情况下结论会反过来：

| 条件 | 说明 |
| --- | --- |
| X1 成立 | 上游决定采纳 ServingGroup CR，并且 `spec` 自带模板或有等价的办法。这时 RG-01、RG-21 的第一步由上游承担 |
| X2 是「每个集群 1 到 2 个大组」 | M 的按份额预算没有意义，`partition` 也没有意义；全局编排是硬需求，按组粒度才够 |
| X3 成立 | 用户不用 kthena autoscaler，或者接受晚一个阶段再有 |
| X4 成立 | 控制面按生产级高可用部署，用户接受整组恢复依赖它 |
| GU-13、GK-06 的结果是「不会搬走」，或上游修掉 | 否则 role 级扩缩容在 G 下不可用 |

建议的下一步（供协调者取舍，不是实现批准）：

1. 不改变 M 的第 1 阶段。它不依赖上面任何一个未知数。
2. 向用户确认 X2 和 X3。这两个答案比任何进一步的静态分析都更能决定方向。
3. 如果要给 G 留一个低成本的验证入口，先做两个包级探针（GU-13、GU-14）和一个 G-c 的 Kind 场景（GK-01 加 GK-06，用上游原版 kthena 和 `replicas: 1` 的 ModelServing，解释器单独写一份：实现 `componentResource`，不实现 `replicaRevision`，不能和 M 的那份装在同一个控制面）。它们不需要 ServingGroup CRD，就能确认 F5 和 F10 这两条推断，以及多组件估算在真实集群上的表现。
4. M 的文档里补一条：每个集群只有 1 个组时，默认的滚动配置会让所有集群同时停，要用 `maxSurge` 或按集群暂停下发。`DESIGN.md` 9.1 给了「5 个集群最多 5 组同时不可用」的算式，没有点明这个极端情况；它本身还是推断，要在 Kind 的 C8 里一并确认。
