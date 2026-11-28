# 014 — Proposal analyst 报告：ModelServing + Karmada 多集群方案

- 角色：Proposal analyst（只刻画、比较、评估；不改任何实现代码）
- 日期：2026-11-28
- 基线：Karmada `6d007132ec5112d2eed41b602d4aeeebaf96f4fc`（`<workspace>/karmada`）；
  kthena `1a5145e90e0e1d70c75def0ef9500055e2904c5f`（`<workspace>/kthena-014`）
- 路径写作 `karmada:pkg/...` 或 `kthena:pkg/...`；同一节里再次引用同一个文件时只写文件名和行号。

## 0. 证据分级和验证方法

每条结论带一个标签：

| 标签 | 含义 |
| --- | --- |
| 【源码】 | 在上述 commit 的源码里逐行读到，附 `路径:行号` |
| 【实测】 | 用 `go test -overlay` 在 Karmada 真实包里跑临时测试得到的数值。测试文件放在任务目录下的临时子目录，用 overlay 映射进 Karmada 的包；没有改动 `karmada/` 任何文件，跑完已删除；Go 1.27.3，`-mod=vendor`，`GOTMPDIR` 指到任务目录。这是包级别的实测，不是 Kind 上的端到端结果 |
| 【推断】 | 由源码事实推出，没有单独跑过 |
| 【未验证】 | 需要 Kind 或上游确认 |

没有创建 Kind 集群，没有跑 docker/helm，没有写 GitHub。`gh` 只做了只读查询（#8104、#8084 的描述和 diff、#7662、#7943）。
两个源码树 `git status` 都是干净的。

## 1. 结论（先看这一节）

草案的分层和分期总体成立，但有 **5 处要改**、**7 个草案没提到的问题**。

### 1.1 草案要改的地方

1. **`componentResource` 不是「可能有用」，而是现在不能和 `replicaRevision` 一起实现。**
   开了 `MultiplePodTemplatesScheduling` 之后，后果不是「Divided 不生效」，而是**每个成员集群的 `spec.replicas` 被改成 0**【实测 W2】。见 §4。
2. **`replicaResource` 的 requirement 选「整个 ServingGroup 的求和」，但要同时写明它只在「集群汇总估算」下可用。**
   有 scheduler-estimator 或带 GPU 维度的资源模型时，只要一个 group 放不进单个节点，结果就是 0【实测】。「最大 Pod」会按每组 Pod 数的倍数高估【实测】。见 §3。
3. **重调度先做 R2（kthena 侧控制器），R1（上游 descheduler 通用化）并行推进**，不是「R1 为主、R2 兜底」。
   上游 maintainer 已明确表示「只看 workload status 不够通用，还是要看 Pod」（#7662 的评论），R1 能否合入不确定；R2 不依赖上游，还能当 R1 的参考实现。见 §6。
4. **`statusReflection` 不要逐字段列举，改成「整份 `.status` 拷贝 + 追加合成字段」。**
   逐字段列举的 Lua 一旦落后于 kthena 的 CRD，新状态字段会被悄悄丢掉；内置到 Karmada 二进制后这个版本差更难管。见 §7。
5. **Layer 4 要给出明确的二选一**：成员侧 kthena autoscaler 只能配 `Duplicated` + `retention`；`Divided` 只能配控制面扩缩容。
   kthena autoscaler 直接改成员对象的 `spec.replicas` 和 role 的 `replicas`，Karmada 会把它改回去。见 §8.4。

### 1.2 草案没提到的问题

| # | 问题 | 级别 | 位置 |
| --- | --- | --- | --- |
| G1 | 默认配置的 Karmada 对**任何**请求 GPU 的副本，general estimator 都算出 0（资源模型里没有 GPU 维度时）。v1.18.0 起的行为，和 ModelServing 无关，但会让动态 Divided 直接调度失败 | 阻塞动态 Divided | §3.3 |
| G2 | 副本被拆分后，成员集群的 kthena validating webhook 会拒绝模板：整数 `maxUnavailable`/`partition`/`evictionStrategy.minAvailable` 大于该集群分到的 `replicas`，或百分比在小副本数下取整为 0 | 阻塞 Divided 下的一部分模板 | §8.2 |
| G3 | descheduler 降副本后，调度器会把副本原样还给同一个集群（Aggregated 下必现）【实测 PP1】；Karmada 里没有任何冷却或「避开刚降过的集群」的机制 | 重调度不收敛 | §5 |
| G4 | 用聚合状态做重调度有「状态过期导致重复降副本」的窗口，需要新鲜度校验 | 可能误删正在启动的 group | §6.4 |
| G5 | 别的集群容量不足以接收**全部**被降的副本时，调度器一个也不放【实测 PP4】，总副本数会一直低于期望 | 容量损失 | §5.3 |
| G6 | 滚动升级的 surge group 也计入 `unschedulableReplicas`；`unschedulableSince` 是最老 group 的时间，新 group 可能没等满阈值就被迁走 | 误迁移 | §8.3 |
| G7 | 控制面不跑 kthena webhook 时，非法模板在 Karmada 上被接受、在成员上才被拒；控制面和成员的 CRD 版本不一致会丢字段 | 运维 | §8.2、§8.5 |

### 1.3 每个问题的推荐项

| 问题 | 推荐 |
| --- | --- |
| Q1 无解释器 | 明确写成「不支持」：`Divided` 会退化成每个集群都拿到完整 `replicas` |
| Q2 requirement 建模 | 求和（整组）；Phase 1 只承诺 Duplicated、静态权重、以及「汇总估算」下的动态 Divided |
| Q3 `componentResource` | 现在不实现；向上游报告 `ReviseReplica(0)` 的问题 |
| Q4 来回迁移 | R2 用「定向迁移」（同一次更新里减 A 加 B，总数不变）；上游方案是调度器扩容时把仍报告不可调度副本的集群可用量置 0 |
| Q5 R1 细节 | 方案 (a')：反射状态里的约定字段 + descheduler 的状态源 + GVK 白名单参数；不新增解释器操作 |
| Q6 Lua 可行性 | 7 个操作全部能用 Lua 写，已在真实 Lua VM 里跑通；不需要 webhook 解释器 |
| Q7 kthena 侧缺口 | 缩容顺序正确；要处理的是 webhook 校验（G2）、surge 计数（G6）、autoscaler 冲突 |
| Q8 分期 | Phase 1 加约束清单；Phase 2 拆成 2a（R2）和 2b（上游）；Phase 3 依赖上游多模板路线，不设时间承诺 |

## 2. Q1：没有任何解释器时 Karmada 怎么处理 ModelServing

结论：**`Divided` 策略被静默当成 `Duplicated`，每个被选中的集群都拿到完整的 `spec.replicas`；状态能到 ResourceBinding，到不了模板；健康状态永远是 Healthy。**

逐段：

1. **detector**。`applyReplicaInterpretation` 只在 `HookEnabled(InterpretReplica)` 时调用 `GetReplicas`
   （`karmada:pkg/detector/detector.go:1516-1536`）。默认解释器只认内置 kind（`karmada:pkg/resourceinterpreter/default/native/default.go:59-64`），
   第三方内置里没有 ModelServing。所以 `ResourceBinding.spec.replicas = 0`、`replicaRequirements = nil`、`components` 为空。【源码】
2. **scheduler**。
   - `calAvailableReplicas` 对 `replicas == 0 且无 components` 的绑定直接跳过估算（`karmada:pkg/scheduler/core/util.go:69-73`）。【源码】
   - 没有 spread constraint 时选中全部可行集群（`karmada:pkg/scheduler/core/spreadconstraint/select_clusters.go:29-32`）。【源码】
   - `AssignReplicas` 的条件 `(spec.Replicas > 0 || spec.ReplicaRequirements != nil) && len(spec.Components) <= 1` 不成立，
     走「非 workload」分支：每个候选集群一条 `TargetCluster{Name}`，`Replicas` 为 0（`karmada:pkg/scheduler/core/common.go:68-82`）。
     【实测 NI1】结果 `[{A 0} {B 0}]`，`IsWorkload()=false`。
   - 调度器注释写明：non-workload 即使策略是 divided 也按 duplicated 处理，并且每次事件都重新调度
     （`karmada:pkg/scheduler/scheduler.go:442-450`）。【源码】
3. **binding controller**。`ensureWork` 只在 `bindingSpec.IsWorkload()` 且有 `ReviseReplica` 钩子时改副本
   （`karmada:pkg/controllers/binding/common.go:86-96`）。这里两个条件都不成立，Work 里是原样模板。
   【实测 W3】模板 `replicas: 4`、两个集群 → 两个 Work 里都是 `replicas: 4`。
4. **状态**。
   - `ReflectStatus` 没有专门处理时回退到拷贝整份 `.status`（`karmada:pkg/resourceinterpreter/default/native/default.go:155-164`、
     `reflectstatus.go:378-389`），所以 `ResourceBinding.status.aggregatedStatus[*].status` 里**已经有**
     `availableReplicas`、`unschedulableReplicas`、`unschedulableSince`。【源码】
   - 没有 `InterpretHealth` 钩子时直接记为 Healthy（`karmada:pkg/controllers/status/work_status_controller.go:270-275`）。【源码】
   - 没有 `AggregateStatus` 钩子时不回写模板状态（`karmada:pkg/controllers/status/common.go:164-166`）。
     控制面上 `kubectl get modelserving` 的状态是空的，scale 子资源的 `status.replicas` 也是 0。【源码】
5. **descheduler**。GVK 白名单只有 `apps/v1 Deployment`（`karmada:pkg/descheduler/core/filter.go:31-33,47-50`），ModelServing 的绑定被过滤掉。【源码】

对草案的影响：Layer 2 的「Duplicated」在没有解释器时也能跑，但用户写了 `Divided` 会得到 N 倍的 GPU 占用且没有任何报错。
指南里要把它列为第一条反例，Kind 上也要先刻画它（§10 的 C0）。

## 3. Q2：副本 requirement 怎么建模（核心争议）

### 3.1 三种建模在各估算器下的实测值

场景：一个 ServingGroup = 8 个 Pod，每个 4 CPU / 16Gi / 1 GPU。集群 4 个节点，每个 32 CPU / 128Gi / 4 GPU。
真实容量（Volcano gang，Pod 可跨节点）：16 GPU 全空闲时 2 组；每节点只剩 3 GPU（共 12）时 1 组。

| 估算器及配置 | (i) 整组求和（8 GPU） | (ii) 最大 Pod（1 GPU） | (iii) components（1+7 个 Pod） | 真实 |
| --- | --- | --- | --- | --- |
| general，只有汇总（无 `AllocatableModelings`），16 GPU 空闲 | **2** | 16 | 2 | 2 |
| general，只有汇总，12 GPU 空闲（每节点 3） | **1** | 12 | 1 | 1 |
| general，资源模型只有 cpu/memory（默认配置），gate 开 | **0** | **0** | **0** | 2 |
| general，同上但 `CustomizedClusterResourceModeling=false` | 2 | 16 | 2 | 2 |
| general，资源模型带 GPU 维度（4 个节点落在 gpu≥4 档） | **0** | 16 | 2 | 2 |
| scheduler-estimator 节点级，16 GPU 空闲 | **0** | 16 | 2 | 2 |
| scheduler-estimator 节点级，每节点 3 GPU 空闲 | **0** | 12 | 1 | 1 |

异构组（1 个 entry：8 CPU 无 GPU；4 个 worker：每个 2 GPU），节点级估算：

| 节点状态 | (i) 求和 | (ii) 逐资源取最大（8 CPU、2 GPU） | (iii) components | 真实 |
| --- | --- | --- | --- | --- |
| 每节点 3 GPU 空闲 | 0 | 4 | 1 | 1 |
| 每节点 4 GPU 空闲 | 0 | 8 | 2 | 2 |

以上全部【实测】（`GeneralEstimator.maxAvailableReplicas` / `MaxAvailableComponentSets`，以及
`nodeResourceEstimator.Estimate` / `EstimateComponents`）。

### 3.2 为什么是这些数

- **节点级估算把一个副本当成一个 Pod**：对每个节点算 `node.Allocatable.MaxDivided(request)` 再求和
  （`karmada:pkg/estimator/server/framework/plugins/noderesource/noderesource.go:122-129`）。
  求和后的请求放不进任何单节点，所以是 0。这正是 zhzhuang-zju 在 #8104 里的反对意见 (a)，**成立**。【源码】
- **资源模型路径和节点级是同一个模拟器**：`getMaximumReplicasBasedOnResourceModels` 把副本包成一个单 Pod 的 component，
  对「模型节点」做 first-fit（`karmada:pkg/estimator/client/general.go:507-553`，模拟器在
  `karmada:pkg/estimator/scheduling_simulator_components.go:83-130`）。模型节点的容量是每档的 `Min`
  （`general.go:296-361`）。【源码】
- **汇总路径只做除法**：`(allocatable - allocated - allocating) / request`，逐资源取最小
  （`general.go:465-505`）。不看节点、不看 `nodeClaim`。【源码】
- **components 路径模拟「一整套能放几套」**，Pod 可以落在不同节点（`noderesource.go:150-195`、`general.go:163-203,262-292`）。
  估算器这一侧已经能算出正确答案。【源码】

### 3.3 G1：默认配置下 GPU 请求估算为 0

- `CustomizedClusterResourceModeling` 是 Beta、默认开启（`karmada:pkg/features/features.go:181`）。【源码】
- 集群创建时如果没写 `spec.resourceModels`，aggregated-apiserver 会填入默认模型，只有 cpu 和 memory 两个维度
  （`karmada:pkg/registry/cluster/strategy.go:97-103`、`karmada:pkg/apis/cluster/mutation/mutation.go:81-`）。【源码】
- 模拟器里 `MaxDivided` 对模型节点上不存在的扩展资源算出 `0 / n = 0`
  （`karmada:pkg/util/resource.go:237-243`）。【源码】
- 模型路径返回 `(0, nil)` 后直接采用，不再回退到汇总路径（`general.go:89-98`）。【源码】
- 旧实现在请求里有模型未覆盖的资源时会返回错误 `resource model is inapplicable as missing resource` 并回退到汇总路径；
  这个分支在 commit `804b08c81`（2026-05-27，包含在 v1.18.0 及之后）里被删掉了。【源码，`git show 804b08c81`】
- 【实测】上表第三行：三种建模全是 0。
- 调度器对各估算器取最小值（`karmada:pkg/scheduler/core/util.go:86-98,157-170`），general 给 0 则最终是 0，
  动态 Divided 报 `Clusters available replicas 0 are not enough to schedule`
  （`karmada:pkg/scheduler/core/division_algorithm.go:76-78`）。【源码】

这个问题和 ModelServing 的建模方式无关，请求 `nvidia.com/gpu` 的普通 Deployment 也一样。
【未验证】需要在 Kind 上用默认安装的 Karmada 确认（§10 的 C2a，带一个 Deployment 对照组）。确认后值得单独向上游报 bug。

规避办法（任选其一，都是配置，不改代码）：

1. 把 GPU 集群的 `Cluster.spec.resourceModels` 清空。更新时不会被重新填默认值（`strategy.go:117-121`），
   清空后 `getAllocatableModelings` 返回空（`karmada:pkg/controllers/status/cluster_status_controller.go:642-645`），general 走汇总路径。【源码】
2. 在 karmada-scheduler 上关掉 `CustomizedClusterResourceModeling`。【实测】关掉后求和得到 2。
3. 给资源模型加上 GPU 维度。这只对「最大 Pod」和 components 有用；求和仍是 0。

### 3.4 各建模的失败方式

| 建模 | 失败方式 | 触发条件 |
| --- | --- | --- |
| (i) 整组求和 | **放不出去**：估算为 0，绑定 `Scheduled=False` | 有 scheduler-estimator，或资源模型带 GPU 维度，且一组放不进单节点。多节点推理的 group 必然触发 |
| (i) 整组求和 | 轻度高估 | 只有汇总估算时看不到节点碎片、拓扑、队列配额 |
| (ii) 最大 Pod | **放多了**：高估倍数约等于每组 Pod 数（上例 8 倍） | 任何估算器 |
| (iii) components | 见 §4，现在不能用于 Divided | — |

为什么不选 (ii)：高估是乘法级的，Aggregated 会把全部副本压到第一个「看起来够」的集群，
之后靠 descheduler 往外挪，而估算器继续高估，副本又被还回来（§5）。上游的 FlinkDeployment 和 RayService 用了「Pod 数 + 逐资源取最大」
（`karmada:pkg/resourceinterpreter/default/thirdparty/resourcecustomizations/flink.apache.org/v1beta1/FlinkDeployment/customizations.yaml`
的 `replicaResource`，注释写着 `Until multiple podTemplates are supported`），但它们的副本单位就是 Pod，和 ModelServing 不同。

还考虑过并否决的一种：副本数报「总 Pod 数」、requirement 报最大 Pod，`ReviseReplica` 再除回去。
Karmada 按整数把 Pod 数分给各集群，不会按组对齐，例如 16 个 Pod 被分成 12 + 4，除以 8 向下取整后是 1 + 0，丢一组。【推断】

### 3.5 今天的 Karmada 能不能对 Divided 算出「能放几整组」

**不能。**

- 调度器只在 gate 开启且 `isMultiTemplateSchedulingApplicable` 为真时走 `MaxAvailableComponentSets`
  （`karmada:pkg/scheduler/core/util.go:113-118`）；后者要求有 components 且 spread constraint 把集群数限定为 1
  （`karmada:pkg/scheduler/core/estimation.go:43-65`）。【源码】
- detector 一旦走了 components 分支就不再调用 `GetReplicas`（`detector.go:1503-1512`），绑定上没有副本数可分。【源码】

### 3.6 让它可行的最小 Karmada 改动，以及和 #8084 的关系

估算器不用改，改动集中在 detector 和 scheduler 的判定上：

1. detector：两个钩子都实现时都调用，绑定同时带 `replicas`（组数）和 `components`（一组的构成）。
2. 调度器估算：`components` 多于 1 个且 `replicas > 0` 时，不管 spread constraint，用
   `MaxAvailableComponentSets` 的结果作为该集群的可用副本数。
3. `AssignReplicas`：这类绑定允许按副本拆分，`ensureWork` 照常调用 `ReviseReplica`。
4. assumption cache：按「每集群副本数 × 一组的 components」记账（现在多模板是整份记，`scheduler.go:759-766`）。

代码量不大，但语义上是新增一类 workload：「可按副本拆分的多组件 workload」。
#8084 当前的 diff 方向正好相反：走 components 分支时显式清空 `Replicas` 和 `ReplicaRequirements`，
新增的 `IsMultiTemplateWorkload()` 注释写明多模板 workload 作为整体调度、每个目标集群持有全部副本；
单模板 workload 则把 `InterpretReplica` 的结果镜像成一个名为 `default` 的 component（双写）并带上 `PodSelector`。【源码，#8084 diff】

所以 Phase 3 不是在 #8084 上加几行，而是要先和上游就「副本数 × 组件集」这个第三类语义达成一致。
在 #8084 的方向下，ModelServing 属于「单模板 + `default` component」这一类，`PodSelector` 选中它的全部 Pod。

### 3.7 推荐

- `GetReplicas` 返回 `spec.replicas` 和整组求和的 `resourceRequest`，不带 `nodeClaim`
  （各 role 的 nodeSelector/toleration 可能不同，合并不出一个正确的；汇总路径也不用它）。
- 指南里写清楚三种放置方式的支持情况：

  | 放置方式 | 是否依赖估算 | Phase 1 是否承诺 |
  | --- | --- | --- |
  | Duplicated | 否 | 是 |
  | Divided + 静态权重 | 否（`assignByStaticWeightStrategy` 不看可用量，`karmada:pkg/scheduler/core/assignment.go:199-211`） | 是 |
  | Divided + Aggregated / 动态权重 | 是 | 只在「汇总估算」下承诺：清空 GPU 集群的 `resourceModels`，且这些集群不部署 scheduler-estimator |

- 不提供「最大 Pod」的可切换选项，避免用户选到会来回迁移的配置。

## 4. Q3：`componentResource` 陷阱

**确认，而且比草案描述的严重。**

链路：

1. gate 开且实现了 `InterpretComponent`：绑定只有 `components`，`replicas = 0`（`detector.go:1503-1512`）。【源码】
2. 调度器：【实测 CT1、CT2】无论 components 是 2 个还是 1 个，`AssignReplicas` 的结果都是每个候选集群 `Replicas: 0`，
   而 `IsWorkload()` 为 true（`karmada:pkg/apis/work/v1alpha2/binding_types_helper.go:219-224`）。
3. `ensureWork`：`IsWorkload()` 为真且有 `ReviseReplica` 钩子，就用 `targetCluster.Replicas`（也就是 0）改写模板
   （`binding/common.go:86-96`）。【实测 W2】两个集群的 Work 里 `spec.replicas` 都是 **0**。

| 解释器实现了什么 | gate 关 | gate 开 |
| --- | --- | --- |
| `replicaResource` + `replicaRevision` | 正常拆分【实测 W1：3 和 1】 | 正常拆分 |
| 再加 `componentResource` | 正常拆分（components 分支不走） | **所有集群副本数变 0** |
| 只有 `componentResource` | 等同无解释器 | 每个集群拿到完整副本数，Divided 失效 |

上游内置的定制里没有任何一个同时实现 `componentResource` 和 `replicaRevision`
（FlinkDeployment 有 `replicaResource` + `componentResource` 但没有 `replicaRevision`；RayService 有前后两个但没有 `componentResource`），
所以上游自己没踩到。【源码，`grep` 全部 thirdparty 定制】

#8084 合入后这个行为不变：components 分支仍然清空副本数。【源码，#8084 diff】

推荐：

1. ModelServing 的定制**不实现** `componentResource`，在 YAML 里加注释说明原因。
2. 向上游报告：`ensureWork` 对「只有 components 的绑定」不应该调用 `ReviseReplica(0)`。
   修复点可以是把条件从 `IsWorkload()` 收紧为「调度器确实做过副本分配」。这是独立的小 bug，容易被接受。
3. 「整份 ModelServing 只放一个集群、用 components 精确估算」这种用法，在第 2 条修掉之前不要提供。

## 5. Q4：descheduler 降副本之后会发生什么

### 5.1 链路

1. descheduler 把 `binding.spec.clusters[A].replicas` 减去不可调度数，下限是 ready 数
   （`karmada:pkg/descheduler/descheduler.go:211-250`）。【源码】
2. 调度器发现各集群副本之和不等于 `spec.replicas`，触发重调度
   （`karmada:pkg/util/binding.go:63-66`、`scheduler.go:428-434`）。【源码】
3. 走 `assignByDynamicStrategy` → `dynamicScaleUp`：目标是缺的那部分，各集群的权重是估算器给出的可用副本数
   （`assignment.go:213-244`、`division_algorithm.go:121-136`）。【源码】
4. Aggregated 下 `resortAvailableClusters` 把**之前已有副本的集群排在最前**，然后取前缀直到够数
   （`assignment.go:151-178`、`division_algorithm.go:81-90`）。【源码】

### 5.2 来回迁移【实测】

A 原有 5 个副本，descheduler 降到 3，缺 2：

| 用例 | 策略 | 估算器给的可用量 | 结果 |
| --- | --- | --- | --- |
| PP1 | Aggregated | A=4，B=10 | `A:5` —— 两个副本全部还给 A |
| PP2 | 动态权重 | A=4，B=10 | `A:4, B:1` —— 还回去一个 |
| PP3 | Aggregated | A=0，B=10 | `A:3, B:2` —— 正确 |
| PP5 | Aggregated，B 已有 2 个副本 | A=4，B=10 | `B:4, A:3` —— B 也算「之前已有」，按可用量排在前面 |

PP1 的后果：成员集群重新创建 2 个 group，再次不可调度，等满阈值（默认 5 分钟，轮询间隔 2 分钟，
`karmada:cmd/descheduler/app/options/options.go:33-34`）后再被降，周而复始，B 一直空着。

### 5.3 G5：放不全就一个不放【实测 PP4】

缺 2 个、A=0、B=1 时调度器报 `Clusters available replicas 1 are not enough to schedule`，不做部分放置
（`division_algorithm.go:76-78`）。绑定停在 `A:3`，总数 3 < 5。调度失败不会回滚 descheduler 的修改
（`scheduler.go:600-607`），靠重试等容量出现。【源码 + 实测】

### 5.4 Deployment 是怎么避免来回迁移的

**没有专门机制。** descheduler 没有状态，调度器里唯一「避开某集群」的过滤器是 `ClusterEviction`，
它只看 `GracefulEvictionTasks`，那是故障转移用的（`karmada:pkg/scheduler/framework/plugins/clustereviction/cluster_eviction.go:49-57`）。【源码】

Deployment 不来回迁移，靠的是估算准：scheduler-estimator 按单 Pod 请求逐节点算，放不下 Pod 的集群会报接近 0。
Pod 因为估算器看不到的原因（PVC、拓扑分布、反亲和）不可调度时，Deployment 同样会来回迁移。【推断】

相关但不能依赖的机制：`SchedulingOvercommitProtection`（alpha，默认关）会在工作负载变 Healthy 之前，
把它已分配的资源从该集群的可用量里扣掉，TTL 5 分钟（`scheduler.go:728-779`、
`karmada:pkg/scheduler/event_handler.go:496-538`、`karmada:pkg/scheduler/cache/cache.go:37-41`）。
它能减轻来回迁移，但这是副作用，而且让 `healthInterpretation` 的定义影响调度。【源码】

### 5.5 ModelServing 需要什么

估算器对 ModelServing 一定会高估（gang、碎片、拓扑、队列配额它都看不到），所以必须有显式反馈。按侵入程度从小到大：

| 方案 | 做法 | 需要改 Karmada | 评价 |
| --- | --- | --- | --- |
| P1 | 用动态权重代替 Aggregated | 否 | 每轮只还回去一部分，几轮后收敛；初始放置不再聚合 |
| P2 | R2 做「定向迁移」：同一次更新里 A 减 n、B 加 n，总数不变 | 否 | 总数没变时调度器不会重调度（`binding.go:63-66`、`scheduler.go:421-458`），完全绕开问题；代价是 R2 要自己选目标集群 |
| P3 | 调度器扩容时，把该绑定在聚合状态里仍报告 `unschedulableReplicas > 0` 的集群可用量置 0 | 是，调度器核心 | 利用状态滞后：descheduler 刚降完时聚合状态还显示不可调度，正好排除 A。改动小，但要上游接受调度器读 workload 状态 |
| P4 | descheduler 在绑定上记录「某集群在某时间前避开」，调度器加过滤插件 | 是，API + 插件 | 最正规，也最重 |

推荐：R2 先做 P2；向上游提 P3，P4 作为 maintainer 更愿意接受时的备选。

## 6. Q5：让 descheduler 支持任意 workload（R1）的做法

### 6.1 现状里要动的点

| 环节 | 现状 | 位置 |
| --- | --- | --- |
| GVK 过滤 | 硬编码 Deployment，带 `TODO: make it as an option` | `descheduler/core/filter.go:30-33` |
| ready 数 | 读聚合状态里的 `readyReplicas`，带 `TODO: cooperate with custom resource interpreter`；读不到时记为 -1 | `descheduler/core/helper.go:126-148,50-54` |
| 是否需要处理 | `Ready < Spec` 的集群才处理 | `helper.go:99-109` |
| 不可调度数 | 对所有注册的估算器取最小值；只有 scheduler-estimator 注册 | `helper.go:62-96`、`estimator/client/accurate.go:34-37` |
| 估算器服务端 | 只认 Deployment，其他 kind 返回错误 | `estimator/server/replica/replica.go:50-68`、`server.go:66-71` |
| 任一集群出错 | 整次调用报错，这个绑定本轮不处理 | `accurate.go:254-277`、`helper.go:75-78` |

两个由此推出的事实：

- ModelServing 的状态里没有 `readyReplicas`，ready 会是 -1：所有集群都被当成「待处理」，
  而且 `updateScheduleResult` 里「不低于 ready」的保护失效（`descheduler.go:219-226`）。【源码】
- pull 模式的集群通常没有可达的 scheduler-estimator，只要它在待处理列表里，整个绑定都不会被 descheduler 处理。【推断】

### 6.2 三个方案

| | (a') 反射状态里的约定字段 | (b) 新增解释器操作 | (c) Pod selector（#8084）+ 按组计数 |
| --- | --- | --- | --- |
| 数据来源 | 聚合状态里的 `readyReplicas`、`unschedulableReplicas`、`unschedulableSince` | 新操作返回不可调度副本数 | 估算器在成员集群列 Pod |
| Karmada 改动 | descheduler 两个文件 + 一个启动参数 | `config.karmada.io` API、三种解释器实现、`karmadactl interpret`；descheduler 要么内嵌解释器框架，要么在 `AggregatedStatusItem` 上加类型化字段 | #8084 合入 + estimator proto + 服务端按标签分组 + descheduler 接线 |
| pull 模式 | 可用（状态走 Work） | 可用 | 需要每个成员有可达的 estimator |
| 没有 estimator | 可用 | 可用 | 不可用 |
| 副本单位 ≠ Pod | 天然支持，单位由 workload 控制器定义 | 支持 | 需要新增「分组标签键」概念；没有它只能保守地少算 |
| 阈值精度 | 只有最老一组的时间 | 取决于实现 | 每个 Pod 各自的时间，最准 |
| 状态新鲜度 | 需要校验（§6.4） | 同左 | 实时 |
| 和上游方向 | maintainer 说过「只看 status 不够通用」；但 descheduler 已经在按约定字段读 `readyReplicas` | 新 API，评审成本高 | 是 maintainer 指的方向，但他们同时指出没有「workload 自定义重调度单位」的概念 |

关于 (b)：解释器操作在 controller-manager 和 agent 里对成员对象执行，descheduler 是独立进程，没有解释器框架。
要让 descheduler 用到结果，最终还是得把数值放进聚合状态。所以 (b) 实际上等于「(a') + 一份正式契约」，
不需要真的新增操作：现有的 `statusReflection` 就能产出这些字段。【推断】

关于 (c) 不带分组键的退化形式：selector 选中 ModelServing 的全部 Pod，估算器返回不可调度的 **Pod** 数，
再除以每组 Pod 数向上取整。Volcano 在 gang 不满足时只把一部分 Pod 标成 Unschedulable（kthena 设计文档
`kthena:docs/proposal/modelserving-unschedulable-replicas-status.md` 的 Design Details），所以这样算会少算，
但不会多降，多轮后收敛。maintainer 坚持看 Pod 时可以用它。【推断】

### 6.3 推荐：(a')

具体内容：

1. `filter.go`：GVK 列表改成启动参数，默认值仍是 Deployment（行为不变）。
2. `helper.go`：
   - ready 继续读 `readyReplicas`（由 `statusReflection` 合成，§7）。
   - 聚合状态项里有 `unschedulableReplicas` 键的绑定，用状态源；否则走原来的 estimator。
     两种来源不取最小值，按绑定二选一，避免对不支持的 kind 发 gRPC。
   - 状态源的取值规则见 §6.4。
3. 不改 scheduler-estimator、proto、解释器 API。

回应 #8104 里的三条反对：

- (a) 求和后的请求按单节点估算：成立，属于估算问题，和「不可调度副本从哪来」无关；短期靠配置（§3.7），长期靠 §3.6。
- (b) descheduler 不认识 workload 自定义的重调度单位：单位就是 `InterpretReplica`/`ReviseReplica` 定义的那个「副本」，
  descheduler 本来就按它增减；workload 控制器用同一个单位报不可调度数。不需要新概念。
- (c) estimator 只认识 Deployment 的 Pod：对「副本 = 一组 Pod」的 workload，数 Pod 需要分组语义
  （忽略正在删除的组、被 scheduling gate 挡住的 Pod 等），这些语义已经在 kthena 控制器里
  （`kthena:pkg/model-serving-controller/controller/unschedulable_replicas.go:28-60`），搬进 Karmada 是重复实现。
  状态源是可选项，不影响 #8084 的 Pod 路径服务单模板 workload 和抢占。

### 6.4 G4：状态源的取值规则

设某集群的反射状态为 `s`，绑定里该集群的副本数为 `spec`。

1. **新鲜度**：`s.generation == s.observedGeneration`，且 `s.desiredReplicas == spec`。
   `desiredReplicas` 是 `statusReflection` 回显的成员对象 `spec.replicas`（§7）。任一不满足，本轮跳过该集群。
2. **阈值**：`now - s.unschedulableSince >= threshold`。
3. **数量**：`min(s.unschedulableReplicas, spec - s.readyReplicas)`，且结果不小于 0。
4. **集群可达**：集群的 Ready 条件为 True，否则状态可能是旧的。

为什么需要第 1 条。例：A 的 `spec=5`，2 个 Running、1 个启动中、2 个不可调度。

- 第一轮：降到 3，成员删掉 2 个不可调度的组。
- 成员状态还没同步回来时第二轮开始：聚合状态仍是「ready 2、不可调度 2」。
  `Ready(2) < Spec(3)`，按旧数据再降 2，被 ready 下限拦在 2，于是 A 变成 2。
- 成员此时只有 2 个 Running 和 1 个启动中的组，缩到 2 会**删掉那个已经调度成功、正在加载模型的组**。

模板代数的注解 `resourcetemplate.karmada.io/generation` 帮不上忙：descheduler 改的是绑定，模板代数没变。
所以要回显成员的 `spec.replicas`。走 estimator 实时列 Pod 的 Deployment 没有这个问题。【推断】

可选的第 5 条（防止过早迁移）：descheduler 在内存里记录每个「绑定 × 集群」在最近一个阈值窗口内观察到的
不可调度数的最小值，只迁移这个最小值。它解决「一个老的组等了很久，新扩出来的 10 个组立刻跟着被迁走」的问题（§8.3）。
进程重启后记录清零，只会更保守。

### 6.5 R1、R2、R3 及故障转移的定位

| 方案 | 结论 |
| --- | --- |
| R2：独立控制器，对 Karmada API 工作 | **先做**。算法就是 §6.3 + §6.4，再加 §5.5 的 P2。改 `ResourceBinding.spec.clusters` 的方式和 descheduler 一样（`descheduler.go:244`）。可以放在 kthena 仓库，也可以独立仓库 |
| R1：上游 descheduler 通用化 | 并行推进，用 R2 的 Kind 结果作为论据 |
| R3：WorkloadRebalancer | 不适合自动重调度。它触发 Fresh 模式，全部副本重新分配（`division_algorithm.go:139-166`），正在运行的 group 也可能被挪走、重新加载模型。#7662 的「保留可用副本」还只是提案。可以作为人工「立即重平衡」工具写进指南，注明会中断服务 |
| 应用故障转移（`failover.application`） | 草案没列。它按 Health 为 Unhealthy 持续多久来驱逐**整个集群上的份额**（`karmada:pkg/controllers/applicationfailover/common.go:114-118`），健康的 group 也一起走。只适合「整份放一个集群」的用法，并且容忍时间必须大于最坏的模型加载时间，否则冷启动期间会被反复驱逐 |

R2 的两个注意点：

- 不要和 karmada-descheduler 同时处理同一种 kind。现在 descheduler 只处理 Deployment，没有冲突；R1 合入后二选一。
- P2 让 R2 自己选目标集群，等于一个小调度器。第一版只在「绑定里已有副本的集群」和「策略 `clusterNames` 里的集群」中选，
  并且只选当前 `unschedulableReplicas == 0` 的；不实现污点、API 启用检查等过滤，这些由首次调度保证。

## 7. Q6：Lua 解释器可行性

**7 个操作全部能用 Lua 写。** 下面的脚本在 Karmada 的真实 Lua VM（关闭 open libs 的沙箱）里对一个 3 个 role 的样例跑通【实测】。

### 7.1 沙箱限制

| 限制 | 位置 | 影响 |
| --- | --- | --- |
| 每次调用 1 秒超时 | `karmada:pkg/resourceinterpreter/customized/declarative/luavm/lua.go:84-86` | role 最多 4 个（`kthena:pkg/apis/workload/v1alpha1/servinggroup_types.go:136-139`），远够 |
| 可用库：base、table、string（安全版）、math、os 只有 `time` 和 `date` | `lua.go:204-224`、`karmada:pkg/util/lifted/lua/oslib_safe.go:49-52` | 够用 |
| 禁用 `load`、`dofile`、`getmetatable`、`setmetatable`、`rawget`、`rawset` 等 | `lua.go:231-252` | 不能用元表技巧 |
| `kube` 库只有 `resourceAdd`、`accuratePodRequirements`、`getPodDependencies`、`getResourceQuantity` | `luavm/kube.go:58-63` | **没有乘法**，见下 |
| 空表在编码时分不清 `{}` 和 `[]` | `luavm/lua_convert.go:38-140` | `ReviseReplica` 和 `Retain` 有参照对象可以还原（【实测】`securityContext: {}`、`emptyDir: {}` 保持不变）；`AggregateStatus` 没有参照对象 |

### 7.2 各操作

**`replicaResource`（`GetReplicas`）** —— 整组求和。

- `spec.replicas` 缺省为 1（`kthena:pkg/apis/workload/v1alpha1/model_serving_types.go:37-42`）；role 的 `replicas` 缺省为 1；
  `workerReplicas` 必填，可以是 0；`workerReplicas > 0` 时 `workerTemplate` 必须存在
  （`servinggroup_types.go:61-79`、`kthena:pkg/model-serving-controller/webhook/validator.go:377-382`）。
- 每组 Pod 数 = Σ role.replicas × (1 + workerReplicas)。
- kthena 的 `PodTemplateSpec` 是 `{metadata: {labels, annotations}, spec: PodSpec}`（`servinggroup_types.go:88-112`），
  形状和 `corev1.PodTemplateSpec` 兼容，可以直接传给 `kube.accuratePodRequirements`。只写了 limits 的容器会按 limits 计入请求。【实测】
- 乘法用「反复加倍」的 `kube.resourceAdd` 实现。

```lua
local kube = require("kube")
local function mul(q, n)
  local result, base = nil, q
  while n > 0 do
    if n % 2 == 1 then result = kube.resourceAdd(result, base) end
    n = math.floor(n / 2)
    if n > 0 then base = kube.resourceAdd(base, base) end
  end
  return result
end
function GetReplicas(obj)
  local replica = 1
  if obj.spec ~= nil and obj.spec.replicas ~= nil then replica = obj.spec.replicas end
  local total = {}
  local function addReq(tpl, count)
    if tpl == nil or count <= 0 then return end
    local req = kube.accuratePodRequirements(tpl)
    if req == nil or req.resourceRequest == nil then return end
    for name, q in pairs(req.resourceRequest) do
      total[name] = kube.resourceAdd(total[name], mul(q, count))
    end
  end
  local roles = {}
  if obj.spec ~= nil and obj.spec.template ~= nil and obj.spec.template.roles ~= nil then
    roles = obj.spec.template.roles
  end
  for _, role in ipairs(roles) do
    local rr = role.replicas
    if rr == nil then rr = 1 end
    addReq(role.entryTemplate, rr)
    addReq(role.workerTemplate, rr * (role.workerReplicas or 0))
  end
  if next(total) == nil then return replica, nil end
  return replica, { resourceRequest = total }
end
```

【实测】样例（prefill：1×(entry 1 GPU + 2 worker×1 GPU)；decode：2×(entry 2 GPU + 1 worker×2 GPU)；router：1 个 500m 的 entry）
得到 `replicas=4`，`cpu=43500m, memory=176Gi, nvidia.com/gpu=11`，和手算一致。

**`replicaRevision`（`ReviseReplica`）** —— `obj.spec.replicas = desiredReplica`。【实测】结果是 int64，空对象字段保持原样。

**`statusReflection`（`ReflectStatus`）** —— 整份拷贝，再追加 4 个键。

```lua
function ReflectStatus(observedObj)
  local status = {}
  if observedObj.status ~= nil then
    for k, v in pairs(observedObj.status) do status[k] = v end
  end
  status.readyReplicas = status.availableReplicas or 0
  if observedObj.spec ~= nil and observedObj.spec.replicas ~= nil then
    status.desiredReplicas = observedObj.spec.replicas
  end
  if observedObj.metadata ~= nil then
    status.generation = observedObj.metadata.generation
    if observedObj.metadata.annotations ~= nil then
      local g = tonumber(observedObj.metadata.annotations["resourcetemplate.karmada.io/generation"])
      if g ~= nil then status.resourceTemplateGeneration = g end
    end
  end
  return status
end
```

| 追加的键 | 用途 |
| --- | --- |
| `readyReplicas` | descheduler 读它（`karmada:pkg/util/constants.go:222-223`，全仓库只有 descheduler 用）。等于 `availableReplicas` |
| `desiredReplicas` | §6.4 的新鲜度校验 |
| `generation`、`resourceTemplateGeneration` | Karmada 第三方定制的惯例，`statusAggregation` 用它们决定是否推进 `observedGeneration`（参照 `apps.kruise.io/v1alpha1/CloneSet/customizations.yaml:94-117,149-161`） |

整份拷贝的理由见 §1.1 第 4 条。`readyReplicas = availableReplicas` 在 surge 期间可能大于该集群的 `spec`，
descheduler 的下限判断带了 `Ready <= Spec` 的条件（`descheduler.go:223`），不会因此把副本数调高。

**`statusAggregation`（`AggregateStatus`）**

- 求和：`replicas`、`currentReplicas`、`updatedReplicas`、`availableReplicas`、`unschedulableReplicas`。
- `unschedulableSince` 取最小。`metav1.Time` 序列化成固定格式的 UTC RFC3339 字符串，直接按字符串比较。【实测】
- `labelSelector` 各集群相同，取任意一个。FederatedHPA 走 scale 子资源需要它
  （`model_serving_types.go:490`）。
- `observedGeneration`：所有集群都满足「`resourceTemplateGeneration` 等于模板代数且成员 `generation == observedGeneration`」时才推进。
- **只写 CRD 里有的字段。** `readyReplicas`、`desiredReplicas` 这类键写进模板状态会被 apiserver 裁掉，
  之后每次比较新旧状态都不相等（`karmada:pkg/controllers/status/common.go:192-197`），导致反复调用 `UpdateStatus`。【推断】
- `conditions`、`currentRevision`、`updateRevision`、`collisionCount` 不聚合。各集群可能因为 OverridePolicy 而不同。

**`healthInterpretation`（`InterpretHealth`）**

```lua
function InterpretHealth(observedObj)
  if observedObj.status == nil then return false end
  if observedObj.status.observedGeneration ~= observedObj.metadata.generation then return false end
  local replicas = 1
  if observedObj.spec.replicas ~= nil then replicas = observedObj.spec.replicas end
  return (observedObj.status.availableReplicas or 0) >= replicas
end
```

不要用 `updatedReplicas >= replicas`：设置了 `partition` 之后它永远达不到，会一直 Unhealthy。
健康状态有两个下游：应用故障转移（§6.5）和 `SchedulingOvercommitProtection` 的记账释放（§5.4）。

**`dependencyInterpretation`（`GetDependencies`）** —— 遍历每个 role 的 `entryTemplate`，以及 `workerReplicas > 0` 时的
`workerTemplate`，调用 `kube.getPodDependencies(tpl, namespace)`，按 `kind/namespace/name` 去重。
【实测】得到 ConfigMap、Secret、ServiceAccount、PVC 各一条，重复引用的 ConfigMap 只出现一次。

**`retention`（`Retain`）** —— 只为成员侧自动扩缩容服务（§8.4）。模板带标签
`resourcetemplate.karmada.io/retain-replicas: "true"` 时保留成员的 `spec.replicas`，和 Deployment 的内置行为一致
（`karmada:pkg/resourceinterpreter/default/native/retain.go:145-163`）。【实测】
如果用了 kthena 的分 role 扩缩容，还要按 role 名（不是下标）保留 `spec.template.roles[*].replicas`。

### 7.3 Lua 表达不了的

| 项 | 原因 | 处理 |
| --- | --- | --- |
| 合并多个模板的 `nodeClaim` | 语义问题：多个 nodeSelector/亲和/容忍没有一个正确的合并结果 | 不返回 `nodeClaim` |
| `plugins[].config` 里引用的对象 | 不透明 JSON（`model_serving_types.go:186-188`） | 文档说明需要单独分发 |
| Volcano Queue | 集群级对象，队列名来自 ModelServing 自身的注解 `scheduling.volcano.sh/queue-name`（`kthena:pkg/model-serving-controller/podgroupmanager/manager.go:282-289`） | 用 ClusterPropagationPolicy 单独分发；各集群队列不同时用 OverridePolicy 改注解 |
| 「一组的 gang 最小集」和「整组」的区别 | `gangPolicy.minRoleReplicas` 允许部分 role 先启动（`servinggroup_types.go:25-43`） | requirement 按整组算，这是期望占用 |

以上都不需要 webhook 解释器。

### 7.4 交付形式

- 先放在 kthena 仓库：一份 `ResourceInterpreterCustomization` YAML，加上按 Karmada 第三方定制目录格式写的 `testdata/*-test.yaml`
  （参照 `karmada:pkg/resourceinterpreter/default/thirdparty/resourcecustomizations/apps.kruise.io/v1alpha1/CloneSet/testdata/`），
  用 `karmadactl interpret` 做离线验证（`karmada:pkg/karmadactl/interpret/interpret.go:61-85`）。之后上游化基本是复制文件。
- 用户自己安装的定制优先级高于 Karmada 内置的（`karmada:pkg/resourceinterpreter/interpreter.go:137-158,339-366`），
  所以 kthena 可以随时发更新的版本覆盖内置。pull 模式下解释器跑在 karmada-agent 里，要注意 agent 的版本。

## 8. Q7：kthena 侧的缺口

### 8.1 没有问题的部分

| 检查项 | 结论 | 位置 |
| --- | --- | --- |
| 缩容是否先删不可调度的组 | **是。** 排序键依次为：状态优先级（不可调度 0，其他未就绪 1，Running 2）、deletion cost 升序、序号降序。正在删除的组不参与排序也不计数 | `kthena:pkg/model-serving-controller/controller/binpack_scaledown.go:39-44,161-201`、`model_serving_controller.go:3065-3114` |
| 「不可调度」的判定 | 组不是 Running，且至少一个未在终止的自有 Pod 的 `PodScheduled=False/Unschedulable` | `binpack_scaledown.go:179-185`、`unschedulable_replicas.go:28-60` |
| 序号变稀疏后扩容 | 补 `[0, 期望数)` 里缺失的序号；`partition` 以下的用旧版本 | `model_serving_controller.go:1004-1048,3033-3059` |
| Karmada 频繁改 `spec.replicas` 是否触发滚动升级 | **否。** 版本哈希的输入是 `schedulerName`、`plugins`、各 role 的名字、模板和 `workerReplicas`，不含 `spec.replicas` 和 role 的 `replicas` | `kthena:pkg/model-serving-controller/utils/revision_data.go:47-66` |
| mutating webhook 造成的持续 diff | **不存在。** ModelServing 只有 validating webhook，默认值全部来自 CRD schema；webhook 目录里的 mutator 是给 LeaderWorkerSet 的 | `kthena:pkg/model-serving-controller/webhook/validator.go`、`lws_mutator.go` |
| 字段归属冲突 | Karmada 用带 `resourceVersion` 的整对象 `Update`，不是 server-side apply，没有 field manager 冲突；状态子资源不受影响 | `karmada:pkg/util/objectwatcher/objectwatcher.go:126-207` |
| ModelServing 的标签注解是否传到 Pod | 否。控制器只读队列注解 | `podgroupmanager/manager.go:282-289` |

缩容顺序的一个边界：成员按**当前**状态选。如果降副本的决定基于过期状态，不可调度的组不够删时，
接下来删的是启动中的组，最后才是 Running。§6.4 的新鲜度校验就是防这个。

### 8.2 G2、G7：成员侧 validating webhook 在拆分后拒绝模板

校验用的是成员对象上的 `spec.replicas`，也就是拆分后的值：

| 规则 | 位置 | 拆分后怎么失败 |
| --- | --- | --- |
| 整数 `maxUnavailable` 不能大于 `replicas` | `validator.go:239,254-259` | 模板 `replicas: 10, maxUnavailable: 2`，某集群分到 1 |
| 整数 `partition` 不能大于 `replicas` | `validator.go:240,254-259` | 模板 `partition: 4`，某集群分到 3 |
| `maxUnavailable` 和 `maxSurge` 不能同时解析为 0 | `validator.go:245-250` | `maxUnavailable: "10%"` 向下取整，集群分到 3 时是 0；没设 `maxSurge` 就被拒 |
| `evictionStrategy.minAvailable` 解析后不能大于 `replicas` | `validator.go:589-599` | 整数 `minAvailable: 3`，某集群分到 2 |
| role 级别的同类规则 | `validator.go:215-223` | 不受影响，Karmada 不改 role 的 `replicas` |

`replicas <= 0` 时这些检查跳过（`validator.go:234`），所以分到 0 的集群不会被拒。

后果：

- 该集群的 Work 一直 `Applied=False`，对象停在旧的 spec。
- **descheduler 或 R2 把副本降下去的那次更新同样会被拒**，不可调度的组删不掉，重调度卡住。

处理办法，按优先级：

1. 指南里给出在任何拆分下都合法的写法：`maxUnavailable` 用整数 1 或不写，`partition` 用百分比或 0，
   `evictionStrategy` 用 `maxUnavailable` 而不是 `minAvailable`。
2. 需要按集群调整时用 OverridePolicy。
3. kthena 侧改动（需要单独批准）：`replicas` 小于这些整数值时改为钳制而不是拒绝。这改变现有校验语义，属于受保护的行为变更。

G7：控制面上如果不注册 kthena 的 validating webhook，非法模板会被 karmada-apiserver 接受，到成员才被拒，用户看到的是
`FullyApplied=False`。`ModelServingValidator` 不依赖集群内的 client（`validator.go:45`），可以对 karmada-apiserver 部署一份来提前失败；
但上表这些依赖拆分结果的规则，控制面校验不出来。

### 8.3 G6：`unschedulableReplicas` 的两个语义边界

- **surge group 也被计入。** 状态更新遍历所有组，包括滚动升级多创建出来的
  （`model_serving_controller.go:2764-2823`；`status.replicas` 是 `len(groups)`，`2888`）。
  GPU 已满的集群里 surge group 必然不可调度。只有 surge、旧组都在时 `Ready == Spec`，descheduler 不处理；
  但 `maxUnavailable >= 1` 且 `maxSurge >= 1` 时，被替换的组使 `Ready < Spec`，surge group 就会被算成该迁走的副本，
  于是**滚动升级过程中副本被迁到别的集群**。【推断】
  建议：指南里对 GPU 紧张的集群推荐 `maxSurge: 0`；§6.4 的第 3 条（上限 `spec - ready`）能限制数量但不能消除。
- **`unschedulableSince` 是最老那一组的时间**（`model_serving_types.go:462-465`）。
  一个组等了很久之后又扩出来 10 个组，这 10 个会在下一轮立刻被视为「已超过阈值」。
  kthena 的设计文档接受了这一点（同一 ModelServing 的组是同构的）。但如果成员集群有节点自动扩容，
  新组本来几分钟后就能调度。§6.4 的可选第 5 条针对这种情况。

### 8.4 成员侧 autoscaler 和 Karmada 的冲突

- kthena autoscaler 直接 patch 成员对象的 `spec.replicas`
  （`kthena:pkg/autoscaler/controller/autoscale_controller.go:199-201`），分 role 扩缩容时用 JSON patch 改 role 的 `replicas`（`489-494`）。
  它用 Pod lister 抓指标（`autoscale_controller.go:93,271,310,358`），所以只能跑在成员集群。【源码】
- Karmada 的 execution controller 发现成员对象内容变了就重新同步，用模板覆盖
  （`karmada:pkg/controllers/execution/execution_controller.go:520-591`、`objectwatcher.go:169-188`）。
  没有 `Retain` 时 autoscaler 的修改会被改回去，两边反复。【源码】
- Deployment 有 `deploymentReplicasSyncer` 把成员副本数同步回模板，它只处理 Deployment
  （`karmada:pkg/controllers/deploymentreplicassyncer/`）。ModelServing 没有对应机制，
  所以「成员侧扩缩容 + Divided」时绑定里的数字是过期的，拆分和重调度都基于错误数据。【源码 + 推断】

明确的二选一：

| 模式 | 放置 | 谁改 `replicas` | 需要 | 重调度 |
| --- | --- | --- | --- | --- |
| A | Duplicated | 成员侧 kthena autoscaler | 模板加 `retain-replicas` 标签 + `retention` 脚本 | 不适用 |
| B | Divided | 控制面：人工、FederatedHPA 或 CronFederatedHPA | `statusAggregation` 聚合 `replicas` 和 `labelSelector` | R2 / R1 |

FederatedHPA 通过各成员集群的 scale 子资源工作（`karmada:pkg/controllers/federatedhpa/federatedhpa_controller.go:497`），
不限 kind。ModelServing 有 scale 子资源，`status.labelSelector` 每组选中一个 entry Pod
（`model_serving_controller.go:2924-2942`）。能否用自定义指标端到端跑通【未验证】。

### 8.5 其他

- **CRD 版本**：控制面的 CRD 比成员旧，新字段在创建模板时就被 karmada-apiserver 裁掉；比成员新，字段在成员被裁掉且没有报错。
  要求两边一致，升级时先升成员的 CRD。【推断】
- **命名**：组名是 `<ms>-<序号>`，Pod 名是 `<组名>-<role>-<role序号>-<pod序号>`
  （`kthena:pkg/model-serving-controller/utils/utils.go:77-79,93-97`）。各集群里完全相同。
  集群内没有问题；跨集群的监控、路由、日志要带集群标签。
- **ModelBooster 和 LWS**：model-booster 控制器和 LWS 控制器会在成员集群里创建并更新 ModelServing
  （`kthena:pkg/model-booster-controller/controller/model_serving_handler.go:77`、
  `kthena:pkg/model-serving-controller/controller/lws_controller.go:269`）。
  这类 ModelServing 不能再由 Karmada 分发，否则两边互相覆盖。本方案的对象是用户直接创建的 ModelServing。
- **部署位置**：karmada-apiserver 上只装 ModelServing 的 CRD（可选加 validating webhook），不跑 kthena 控制器。
  成员集群装完整的 kthena、Volcano、队列和 GPU 设备插件。首次调度时没装 CRD 的集群会被调度器的 `APIEnablement` 插件排除；
  已经在调度结果里的集群不受这个检查影响（`karmada:pkg/scheduler/framework/plugins/apienablement/api_enablement.go:50-78`）。
- **PVC 依赖**：`dependencyInterpretation` 会把 PVC 一起分发，各集群要有同名的 StorageClass。

## 9. 对草案各层和分期的评估

### 9.1 各层

| 层 | 评估 | 修改 |
| --- | --- | --- |
| Layer 1 解释器 | 方向对。7 个操作可行 | requirement 用整组求和且不带 `nodeClaim`；去掉 `componentResource`；`statusReflection` 整份拷贝并追加 4 个键；`statusAggregation` 只写 CRD 里的字段；健康判定不用 `updatedReplicas` |
| Layer 2 分发模式 | 方向对 | 加上 §3.7 的支持矩阵、G1 的规避配置、§8.2 的模板写法约束、CRD 版本要求、不支持 ModelBooster/LWS 派生的 ModelServing |
| Layer 3 重调度 | R1/R2/R3 的定位要调整 | 见 §6.5；补上来回迁移（§5.5）、新鲜度（§6.4）、放不全就不放（§5.3）、应用故障转移 |
| Layer 4 自动扩缩容 | 结论对，缺机制说明 | 写成 §8.4 的二选一表 |
| Layer 5 滚动升级 | 结论对 | 补充：整数的 `maxUnavailable`/`partition` 是每个集群各自生效，总量是各集群之和；surge 和重调度的相互作用（§8.3）；想分集群灰度要用 OverridePolicy 或分批改策略 |

### 9.2 分期

| 阶段 | 草案 | 建议 |
| --- | --- | --- |
| Phase 1 | 不改 Karmada：解释器 + 指南 | 保留。交付物加上：`karmadactl interpret` 用的测试数据；支持矩阵和反例清单（C0、C3、C6）；G1 的规避配置 |
| Phase 2 | Karmada 上游：内置解释器、descheduler 通用化 | 拆成两条线。**2a（自己可控）**：R2 控制器，含新鲜度校验和定向迁移。**2b（上游）**：按被接受的把握从高到低——① G1 的 bug 报告；② `ensureWork` 对只有 components 的绑定调用 `ReviseReplica(0)`；③ 内置 ModelServing 定制；④ descheduler 的 (a')；⑤ 调度器防来回迁移（P3） |
| Phase 3 | 按 component set 做 Divided | 保留为长期目标，不设时间。它和 #8084 当前「多模板整体调度」的语义相反，需要先在上游立项讨论 |

2a 是否需要 kthena 侧代码改动：R2 本身是新组件；现有控制器只在采纳 §8.2 第 3 条或想把 surge group 从计数里排除时才要改，
两者都改变现有行为，需要单独批准。

## 10. Kind 刻画和验证场景

### 10.1 环境

- 1 个宿主集群装 Karmada（默认配置，记录版本和各组件的 feature gate），2 个 push 模式成员（m1、m2），C9 另加 1 个 pull 模式成员（m3）。
- 成员集群：Volcano、kthena（CRD、控制器、webhook）。karmada-apiserver 只装 ModelServing CRD。
- **假 GPU**：给节点的 status 打补丁增加扩展资源（如每个 worker 节点 `nvidia.com/gpu: 4`），Pod 用 pause 镜像请求该资源。不需要真实 GPU。
- 每个场景记录：`ResourceBinding` 的 `spec`（`replicas`、`replicaRequirements`、`clusters`）和 `status`（`aggregatedStatus`、条件）、
  各成员的 ModelServing `spec.replicas` 和 `status`、Work 的条件、相关组件日志。

### 10.2 Phase 1 之前要做的刻画（不改任何代码）

| 编号 | 步骤 | 预期 | 类型 |
| --- | --- | --- | --- |
| C0 | 不装解释器；模板 `replicas: 4`；策略 Divided/Aggregated，两个集群 | 绑定无 `replicas`；`clusters` 两项都没有副本数；两个成员都是 4（共 8 组）；`aggregatedStatus` 有整份状态；控制面模板状态为空；组 Pending 时 Work 仍 Healthy | 反例 |
| C1 | 装解释器；依次试 Duplicated、静态权重 3:1 | 成员副本数符合策略；控制面模板状态是求和结果；有组 Pending 时 Health 为 Unhealthy；`aggregatedStatus` 里有 `readyReplicas`、`desiredReplicas` | 正例 |
| C2a | 默认 Karmada；Divided/Aggregated；一组 8 Pod × 1 GPU。同时放一个请求 1 GPU 的 Deployment 做对照 | 两者都 `Scheduled=False`，消息含 `available replicas 0`；`Cluster.status.resourceSummary.allocatableModelings` 非空 | 反例，确认 G1 |
| C2b | 清空两个成员的 `spec.resourceModels` | 可用量 = 空闲 GPU / 8 向下取整；副本按它聚合到一个集群 | 正例 |
| C2c | 给 m1 部署 scheduler-estimator | 整组求和：m1 可用量 0。换成「最大 Pod」的定制：m1 可用量是真实值的 8 倍，副本放多了 | 反例 |
| C3 | controller-manager 和 scheduler 开 `MultiplePodTemplatesScheduling`；定制里加 `componentResource` | 有 `replicaRevision`：两个成员 `spec.replicas` 变 0。去掉 `replicaRevision`：两个成员都是完整副本数 | 反例 |
| C4 | m1 只够 2 组，静态权重让它拿到 4 | m1：2 个 Running、2 个不可调度；`status.unschedulableReplicas: 2` 且有 `unschedulableSince`；PodGroup 处于 Pending/Inqueue；这些值出现在 `aggregatedStatus` | 正例 |
| C5 | 在 C4 基础上换成动态策略，手工把绑定里 m1 的副本数 4→2（模拟 descheduler） | kthena 删掉的是那 2 个不可调度的组，Running 组的 Pod UID 不变。然后观察调度器：Aggregated 且估算器认为 m1 还有余量 → 副本还给 m1（来回迁移）；动态权重 → 部分到 m2；m2 余量不足 2 → 调度失败，总数停在 2 | 正反例都有，确认 G3、G5 |
| C6 | 模板 `replicas: 4`，分别带整数 `maxUnavailable: 2`、`partition: 3`、`maxUnavailable: "10%"`、`minAvailable: 3`；静态权重 3:1 | 分到 1 的集群：Work `Applied=False`，消息来自 kthena webhook；绑定 `FullyApplied=False`。改成 §8.2 推荐写法后通过 | 反例，确认 G2 |
| C7 | 在成员上直接 patch `spec.replicas`（模拟 autoscaler）。先不带 retain 标签，再带上 | 不带：几秒内被改回。带：保留 | 正反例 |
| C8 | 改模板里的镜像 | 两个成员同时开始滚动；各自按自己的 `maxUnavailable`；m1 已满且 `maxSurge: 1` 时 surge 组不可调度并计入 `unschedulableReplicas` | 刻画 G6 |
| C9 | m3 以 pull 模式加入，重复 C1、C4 | 状态正常上报；解释器在 agent 里生效 | 正例 |
| C10 | 控制面 CRD 去掉一个较新的可选字段后创建带该字段的模板 | 字段在控制面被裁掉，成员上没有 | 反例 |
| C12 | 把 m2 从策略里移除；把模板副本数改成 0 | m2 上的 ModelServing 和它的组被删除；m1 缩到 0 且 webhook 不拒绝 | 回归 |

### 10.3 Phase 2a（R2）的验证

| 编号 | 步骤 | 预期 |
| --- | --- | --- |
| R-1 | C4 的状态下运行 R2，阈值设 1 分钟 | 超过阈值后 m1 减 2、m2 加 2；m1 的 Running 组不受影响；绑定各集群之和不变，调度器没有重调度的日志 |
| R-2 | 新鲜度：让 m1 的状态上报延迟（停掉 controller-manager 对 m1 的同步或暂停 kthena 控制器），R2 连续跑两轮 | 第二轮因 `desiredReplicas` 和绑定不一致而跳过，没有再次降副本 |
| R-3 | m2 也满 | R2 不动，记录事件说明没有可用目标 |
| R-4 | 未超过阈值 | 不动 |
| R-5 | 组是因为镜像拉取失败而未就绪，不是不可调度 | `unschedulableReplicas` 为 0，R2 不动 |
| R-6 | 滚动升级期间（C8 的状态） | 按 §6.4 第 3 条，迁移数不超过 `spec - ready` |
| R-7 | pull 模式成员 | 行为和 push 一致 |
| R-8 | 同时运行 karmada-descheduler | 它不处理 ModelServing 的绑定；Deployment 的行为不变 |

### 10.4 可选

- C13：FederatedHPA 指向控制面的 ModelServing，验证 scale 子资源和 `labelSelector` 聚合后能否工作。
- Phase 2b 的每个上游改动各自需要用改过的 Karmada 镜像在同一套场景上重跑（C2a、C3、C5 是对应的前后对比）。

## 11. 未验证的风险

| # | 风险 | 怎么确认 |
| --- | --- | --- |
| U1 | G1 在真实部署里是否如实测那样出现（默认资源模型确实被写入、`allocatableModelings` 确实非空） | C2a |
| U2 | 节点 status 补丁出来的扩展资源，Volcano 和 Karmada 的集群状态采集是否都按预期计入 | C2b、C4 |
| U3 | `AggregateStatus` 没有参照对象，返回的整个对象里空字段可能被编码成 `[]`；`UpdateStatus` 是否因此被 apiserver 拒绝。其他内置定制同样如此且在用，推测没问题 | C1，模板里带 `emptyDir: {}`、`securityContext: {}` |
| U4 | Volcano 在 gang 不满足时给 Pod 写的 `PodScheduled` 条件和 kthena 的判定是否在所有 Volcano 版本下一致 | C4，记录 Volcano 版本 |
| U5 | 上游是否接受 (a') 和 P3。maintainer 已有保留意见 | 先有 R2 的结果再提 |
| U6 | #8084 仍在评审，范围可能收窄；本报告引用的是 2026-11-28 的 diff | 合入前复查 |
| U7 | FederatedHPA 对 ModelServing 的端到端可用性 | C13 |
| U8 | R2 的定向迁移绕开了调度器的过滤插件，目标集群的合法性只靠「在策略的集群列表里」保证；使用标签选择器或多组亲和的策略时不够 | R2 设计评审时限定支持的策略形式 |
| U9 | kthena 控制器对绑定副本数高频变化（来回迁移时每个周期一次）的开销：每次是若干 Pod 和 PodGroup 的创建删除，没有容器真正启动 | C5 观察 |

## 附录：实测原始输出

Karmada `6d007132`，Go 1.27.3，`go test -overlay ... -mod=vendor`。

```text
general | S1 summary only (no modelings), 16 free GPU          | (i) sum-of-group=2 | (ii) largest-pod=16 | (iii) component-sets=2
general | S2 cpu/mem-only models (default-like), 16 free GPU   | (i) sum-of-group=0 | (ii) largest-pod=0  | (iii) component-sets=0
general | S2b same models, CustomizedClusterResourceModeling=false | (i) sum-of-group=2 | (ii) largest-pod=16 | (iii) component-sets=2
general | S3 models with GPU dimension (4 nodes at gpu-min=4)  | (i) sum-of-group=0 | (ii) largest-pod=16 | (iii) component-sets=2
general | S4 summary only, fragmented: 12 free GPU (3 per node) | (i) sum-of-group=1 | (ii) largest-pod=12 | (iii) component-sets=1

node-level | N1 4 nodes x 4 free GPU; group=8 pods x 1 GPU                  | (i)=0 | (ii)=16 | (iii)=2
node-level | N2 4 nodes x 3 free GPU; group=8 pods x 1 GPU                  | (i)=0 | (ii)=12 | (iii)=1
node-level | N3 4 nodes x 3 free GPU; group=1 entry(0gpu)+4 workers x 2 GPU | (i)=0 | (ii)=4  | (iii)=1
node-level | N4 4 nodes x 4 free GPU; group=1 entry(0gpu)+4 workers x 2 GPU | (i)=0 | (ii)=8  | (iii)=2

assign | PP1 Aggregated: A 5->3, estimator A=4,B=10        | result=[{A 5}]
assign | PP2 DynamicWeight: A 5->3, estimator A=4,B=10     | result=[{A 4} {B 1}]
assign | PP3 Aggregated: A 5->3, estimator A=0,B=10        | result=[{A 3} {B 2}]
assign | PP4 Aggregated: A 5->3, estimator A=0,B=1         | err=failed to scale up: Clusters available replicas 1 are not enough to schedule.
assign | PP5 Aggregated: A 5->3, B holds 2, estimator A=4,B=10 | result=[{B 4} {A 3}]
assign | CT1 components only (2 comps), replicas=0         | isWorkload=true  | result=[{A 0} {B 0}]
assign | CT2 components only (1 comp), replicas=0          | isWorkload=true  | result=[{A 0} {B 0}]
assign | NI1 no interpreter                                | isWorkload=false | result=[{A 0} {B 0}]

ensureWork | W1 replicaResource+replicaRevision, divided A=3,B=1          | member spec.replicas = map[karmada-es-a:3 karmada-es-b:1]
ensureWork | W2 gate on + componentResource + replicaRevision             | member spec.replicas = map[karmada-es-a:0 karmada-es-b:0]
ensureWork | W3 no interpreter                                            | member spec.replicas = map[karmada-es-a:4 karmada-es-b:4]

lua | GetReplicas -> replicas=4 requirements={"resourceRequest":{"cpu":"43500m","memory":"176Gi","nvidia.com/gpu":"11"}}
lua | ReviseReplica(3) -> spec.replicas=3(int64) securityContext={} volumes[0]={"emptyDir":{},"name":"cache"}
lua | ReflectStatus -> 整份 status + desiredReplicas=4, generation=7, readyReplicas=2, resourceTemplateGeneration=3
lua | AggregateStatus -> {"availableReplicas":2,"currentReplicas":4,"labelSelector":"x=y","observedGeneration":7,"replicas":4,"unschedulableReplicas":2,"unschedulableSince":"2026-11-20T06:40:00Z","updatedReplicas":4}
lua | InterpretHealth (available 2 < replicas 4) -> false
lua | GetDependencies -> ConfigMap cm1, Secret sec1, ServiceAccount sa1, PersistentVolumeClaim pvc1
lua | Retain (retain-replicas=true, observed 9, desired 4) -> spec.replicas=9
```

实测的局限：

- W2 的绑定是按 detector 源码和 CT1 的结果手工构造的，解释器用的是只实现 `ReviseReplica` 的桩；没有跑真实的 detector → scheduler → binding controller 全链路。
- S2 用的是手写的只含 cpu/memory 的两档模型，不是 Karmada 的默认九档模型；逻辑相同（模型里没有 GPU 维度）。
- 全部是包级别调用，没有经过 gRPC、informer 和真实集群。对应的端到端确认在 §10。
