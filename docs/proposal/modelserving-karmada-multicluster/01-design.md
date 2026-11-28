# ModelServing + Karmada 多集群使用方案（设计稿）

- 作者：@VanderChen，2026-11-28
- 状态：提案，待评审。本文不含实现；实现在方案批准后进行。
- 基线：Karmada `6d007132`（master，`v1.20.0-beta.0-44`）；kthena `1a5145e9`（main，含 PR #2079）。
- 待决策点见 [03-decisions.md](03-decisions.md)；刻画证据、方案取舍和验证计划见 [04-evidence.md](04-evidence.md)；和 ServingGroup CR 的关系见 [02-servinggroup-cr.md](02-servinggroup-cr.md)。
- 记号：`karmada:` 指 karmada-io/karmada @ `6d007132`，`kthena:` 指 volcano-sh/kthena @ `1a5145e9`。

本文里每条技术结论带一个证据标签：

| 标签 | 含义 |
| --- | --- |
| 【Kind】 | 在 Kind 上用上述 commit 构建的 Karmada 和 kthena 观察到，日志在 [`artifacts/logs/`](artifacts/logs/) 的 `kind-*.log` |
| 【源码】 | 在上述 commit 的源码里读到 |
| 【包级实测】 | 用临时 Go 测试直接调用 Karmada 的真实包得到的数值；不是集群上的端到端结果 |
| 【离线实测】 | 用 `karmadactl interpret` 在本地执行解释器脚本得到的结果 |
| 【推断】 | 由源码事实推出，没有执行过 |
| 【待验证】 | 还没有在 Kind 上确认 |

Kind 环境的局限：成员集群没有装 Volcano，ModelServing 用的是 `schedulerName: default-scheduler`；GPU 是打到节点 status 上的扩展资源，Pod 是 pause 镜像。
所以 Volcano gang 调度下的行为都属于【待验证】。

## 1. 要解决的问题

一个 ModelServing 有 `spec.replicas` 个 ServingGroup。每个 ServingGroup 是一个独立的推理单元，
由若干 role 组成（例如 prefill、decode），每个 role 实例是 1 个 entry Pod 加若干 worker Pod，整组由 Volcano 做 gang 调度。

目标用法来自 karmada-io/karmada#8104 的讨论：

1. 用户在 Karmada 控制面上写一个 ModelServing 和一个 PropagationPolicy。
2. Karmada 把 `spec.replicas` 拆给多个成员集群，各集群的 ServingGroup 独立运行，只共享模型文件。
3. 某个集群里的 ServingGroup 因为 GPU 碎片、拓扑、队列配额等原因长时间调度不上，而别的集群有空闲 GPU 时，
   把这些 ServingGroup 换到别的集群。粒度是 ServingGroup，不是单个 Pod。

非目标：

- 跨集群的推理流量路由。本方案假设每个集群有自己的 kthena-router 和入口，全局流量分配在 Karmada 之外解决（9.4）。
- 把一个 ServingGroup 的不同 role 拆到不同集群。
- 迁移正在运行的 ServingGroup。本方案只换「还没调度上的」。

## 2. 结论

今天的 Karmada 加上一份解释器配置和两条 Karmada 配置，前两条用法就能工作。第三条需要改 karmada-descheduler，改动只在这一个可选组件里。

| 能力 | 现状 | 本方案的做法 | 阶段 |
| --- | --- | --- | --- |
| 每个集群一份完整副本（Duplicated） | 不装解释器也能下发，但控制面没有状态、健康永远是 Healthy【Kind】 | 装解释器 | 1 |
| 按固定比例拆副本（静态权重） | 不装解释器时，`Divided` 被静默当成 Duplicated【Kind】 | 装解释器【Kind：1:3 拆分正确】 | 1 |
| 按各集群剩余容量拆副本（Aggregated、动态权重） | 默认配置的 Karmada 对任何请求 GPU 的副本都估算为 0，调度失败【Kind】 | 装解释器，加两条 Karmada 配置【Kind：按 1 + 2 拆分，3 组全部 Running】 | 1 |
| 控制面看到汇总状态 | 没有 | 解释器的状态反射和聚合【Kind】 | 1 |
| 调度不上的 ServingGroup 换集群 | karmada-descheduler 不处理 ModelServing【Kind：超过阈值 3 分钟后绑定没有任何变化】 | 让 karmada-descheduler 从聚合状态读不可调度数，并且「有去处才降」 | 2 |
| 估算「这个集群能放几整组」 | 估算器已经能算对【包级实测】，但调度器只在「整份放一个集群」时才用它 | 向上游提「按组件集拆分副本」 | 3 |

刻画过程中确认了五件事，它们决定了方案的边界：

1. **不装解释器就写 `Divided`，每个集群都拿到完整的 `spec.replicas`。** 没有任何报错。【Kind：模板 3 个副本，两个成员各 3 个，共 6 组】
2. **已经有模板之后再装解释器，会中断服务。** 模板下一次更新时，所有集群正在运行的 ServingGroup 都被删除；
   调度能成功就重建，调度失败就一直停在 0。【Kind，两种情况都复现】详见 3.2。
3. **Karmada maintainer 在 #8104 里的反对意见成立。** 把整组的资源求和当成一个副本，节点级估算在找「单个节点放得下整组」的节点：
   一个真实能放 1 组的集群被估算成 0。【Kind + 包级实测】本方案接受这个限制并写明适用条件，长期解法是第 3 阶段。
4. **默认配置的 Karmada 不能对 GPU 工作负载做动态拆分。** 一个每 Pod 请求 1 个 GPU 的普通 Deployment 也调度失败。【Kind】
   这是 Karmada 的 bug，和 ModelServing 无关，有配置上的规避办法（5.3）。
5. **降副本之后调度器会把副本放到别的集群，前提是别的集群有容量。** 没有容量时，这个副本约 5 分钟不属于任何集群，然后回到原集群重新开始计时。【Kind】
   所以重调度要先确认有去处再降（8.3）。

## 3. 部署拓扑

### 3.1 组件放在哪里

```text
                     Karmada 控制面（karmada-apiserver）
   ┌──────────────────────────────────────────────────────────────────┐
   │ ModelServing CRD（只有 CRD，不跑 kthena 控制器）                    │
   │ ResourceInterpreterCustomization（ModelServing 的解释器，管理员装） │
   │ ModelServing 模板 + PropagationPolicy + 可选的 OverridePolicy       │
   │ karmada-scheduler / controller-manager / descheduler（第 2 阶段）   │
   └───────────────┬───────────────────────────────┬──────────────────┘
        Work（副本数已改写）                  Work（副本数已改写）
                   ▼                               ▼
   成员集群 region-a                    成员集群 region-b
   kthena（CRD、控制器、校验 webhook）     同左
   Volcano、Queue、GPU 设备插件           同左
   kthena-router、ModelServer、ModelRoute  同左
   ModelServing（spec.replicas = 分到的数）→ ServingGroup → Pod
                   │                               │
                   └── status ──► ResourceBinding.status.aggregatedStatus ──► 模板 status（汇总）
```

push 模式和 pull 模式的成员都可以：刻画里第三个成员是 pull 模式，下发、状态上报、解释器（在 karmada-agent 里执行）都正常。【Kind】

### 3.2 解释器必须先装

没装解释器时，Karmada 把 ModelServing 当成非工作负载：绑定上没有副本数，`spec.clusters` 里每个集群一项、不带副本数，每个集群拿到完整的模板。
之后装上解释器，绑定不会立刻变化；但模板只要更新一次（哪怕只是加一个注解），绑定就变成「有副本数的工作负载」，
而 `spec.clusters` 里还是那几项不带副本数的集群。Karmada 的 binding controller 这时按「分到 0 个副本」改写成员对象。【Kind + 源码】

| 情况 | 观察到的结果 |
| --- | --- |
| 调度随后成功 | 被留下的集群：成员对象的 `spec` 被改了两次（generation 1 → 3，按源码是副本数 2 → 0 → 2），全部 Pod 被删除重建（UID 全变）。没被选中的集群：ModelServing 被整个删除。等于所有集群的组全部重启 |
| 调度失败（例如 GPU 估算为 0） | 两个成员的 `spec.replicas` 都停在 0，Pod 全部删除；绑定显示 `Scheduled=False`，成员对象仍是 `Healthy` |

所以：

- 新环境：先装解释器，再创建第一个 ModelServing 模板。
- 已经有模板的环境：不能直接装。建议的步骤是先在 PropagationPolicy 上暂停下发（`spec.suspension.dispatching: true`），
  装解释器，更新模板让绑定重新计算，确认 `ResourceBinding.spec.clusters` 里每个集群的副本数正确，再恢复下发。【待验证】
  在这个步骤验证之前，把「已有模板后装解释器」视为不支持。

这个行为和 4.4 的问题是同一个根因：binding controller 在调度器还没分配副本时就按 0 改写成员对象。列入向上游报告的条目（第 10 节）。

### 3.3 其他安装约束

| 约束 | 原因 |
| --- | --- |
| 控制面和成员集群的 ModelServing CRD 版本一致；升级时先升控制面的 CRD，成员全部升完之前不用新字段 | CRD 不一致时字段会被 apiserver 静默裁掉【推断】 |
| 控制面的 CRD 用 `kubectl apply --server-side` 或 `kubectl create` 安装 | CRD 的 JSON 约 629 KB，客户端 apply 的注解会超长。【Kind：server-side apply 成功】 |
| 成员集群用 kthena 的 Helm chart 安装（自带 CRD），不通过 Karmada 分发 CRD | 避免每个集群的 Work 都带一份 629 KB 的 CRD |
| Volcano Queue、PriorityClass、HyperNode 等集群级对象单独分发 | 解释器的依赖解析只看 Pod 模板引用的 ConfigMap、Secret、ServiceAccount、PVC |
| 由 ModelBooster 或 LWS 兼容控制器生成的 ModelServing 不交给 Karmada | 成员集群里已有控制器在写这个对象，两边会互相覆盖【源码】 |

kthena 的 ModelServing 只有校验 webhook，没有 mutating webhook，默认值全部来自 CRD schema。【源码】
所以控制面存下来的对象已经带默认值，下发到成员后不会再变，Karmada 和 kthena 不会反复互相覆盖。

## 4. 解释器

交付物是一份 `ResourceInterpreterCustomization`（Lua）。草稿在 [`artifacts/interpreter/modelserving-customization.yaml`](artifacts/interpreter/modelserving-customization.yaml)。
7 个操作都用 `karmadactl interpret` 跑通【离线实测】；其中 6 个在 Kind 上经过真实的 Karmada 控制器执行【Kind】，
`dependencyInterpretation` 在 Kind 上没有用到（测试模板没有引用 ConfigMap 等对象）【待验证】。

### 4.1 副本单位

Karmada 的一个「副本」等于一个 ServingGroup，也就是 `ModelServing.spec.replicas` 的一个单位。
调度器按这个单位拆分，`ReviseReplica` 按这个单位改写成员对象，kthena 用同一个单位上报 `unschedulableReplicas`。

这回应了 #8104 里「descheduler 不认识 workload 自定义的重调度单位」的意见：不需要新概念，
单位就是 `InterpretReplica` 和 `ReviseReplica` 已经定义的那个副本。

### 4.2 各操作

| 操作 | 做什么 | 要点 |
| --- | --- | --- |
| `replicaResource`（`GetReplicas`） | 副本数取 `spec.replicas`（缺省 1）；资源需求是一个 ServingGroup 全部 Pod 的请求之和：Σ role.replicas ×（entry + workerReplicas × worker） | 不返回 `nodeClaim`：同一组里各 role 的 nodeSelector、容忍可能不同，合并不出一个正确的结果 |
| `replicaRevision`（`ReviseReplica`） | `spec.replicas = 分到的副本数` | 改副本数不触发滚动升级：kthena 的版本哈希不含 `spec.replicas`【源码】 |
| `statusReflection`（`ReflectStatus`） | 整份 `.status` 原样拷贝，再追加 4 个键（见下表） | 不逐字段列举：kthena 以后新增的状态字段不会被解释器悄悄丢掉 |
| `statusAggregation`（`AggregateStatus`） | 计数器求和；`unschedulableSince` 取最早；`labelSelector` 取任一集群的值；所有集群都观察到最新模板时才推进 `observedGeneration` | 只写 CRD 里有的字段。`conditions`、revision 字段不汇总，各集群可能不同 |
| `healthInterpretation`（`InterpretHealth`） | 成员已观察到最新 spec，且 `availableReplicas >= spec.replicas` | 不用 `updatedReplicas`：设了 `partition` 之后它永远达不到，会一直 Unhealthy |
| `retention`（`Retain`） | 模板带标签 `resourcetemplate.karmada.io/retain-replicas: "true"` 时，保留成员的 `spec.replicas` 和各 role 的 `replicas` | 只为成员侧自动扩缩容服务（第 7 节）。标签语义和 Karmada 内置的 Deployment 一致 |
| `dependencyInterpretation`（`GetDependencies`） | 各 role 的 entry、worker 模板引用的 ConfigMap、Secret、ServiceAccount、PVC，去重 | PVC 会跟着调度结果走，见 9.3 |
| `componentResource`（`GetComponents`） | **不实现** | 见 4.4 |

`ReflectStatus` 追加的键只存在于 `ResourceBinding.status.aggregatedStatus` 里，不写回模板：

| 键 | 值 | 谁用 |
| --- | --- | --- |
| `readyReplicas` | 等于 `availableReplicas`，缺失时写 0 | karmada-descheduler 按这个固定键读就绪数；ModelServing 的状态里没有它【源码】 |
| `desiredReplicas` | 成员对象当前的 `spec.replicas` | 重调度的新鲜度校验（8.2） |
| `generation`、`resourceTemplateGeneration` | 成员对象的代数、模板代数注解 | 聚合时判断是否推进 `observedGeneration`，Karmada 第三方定制的惯例 |

### 4.3 资源需求为什么用「整组求和」

候选有三种。下面是同一组场景下各估算器给出的可用副本数【包级实测】，记录在 [`artifacts/logs/estimator-probe.log`](artifacts/logs/estimator-probe.log)。

场景：碎片集群有 2 个节点、每节点 2 GPU；大节点集群有 1 个节点、8 GPU。

| 估算器 | 建模 | 碎片集群，组 = 4 个 Pod × 1 GPU | 碎片集群，组 = 3 GPU + 1 GPU | 大节点集群，两种组 |
| --- | --- | --- | --- | --- |
| 真实容量 | — | 1 | 0 | 2、2 |
| 节点级（scheduler-estimator） | 整组求和 | **0**（低估） | 0 | 2、2 |
| 节点级 | 最大 Pod | **4**（高估） | 0 | **8**（高估）、2 |
| 节点级 | 组件集 | 1 | 0 | 2、2 |
| 集群汇总（general，无资源模型） | 整组求和 | 1 | **1**（高估） | 2、2 |
| 集群汇总 | 最大 Pod | **4**（高估） | **1**（高估） | **8**（高估）、2 |
| 集群汇总 | 组件集 | 1 | **1**（高估） | 2、2 |

第一列里「整组求和」的两个数在 Kind 上得到了同样的结果：同一个碎片集群、同一种组，scheduler-estimator 给 0，general estimator 给 1。【Kind】

结论：

- **节点级的组件集估算在这些场景里都对。** 估算器这一侧已经具备正确答案，缺的是调度器在拆分副本时用它（第 3 阶段）。
  它是 First Fit：不会在资源维度上高估，但可能低估（真实能放 1 组而估算为 0），把单 Pod 请求大的组件排在前面可以减轻。【包级实测，第二轮】
- **整组求和**在集群汇总估算下基本准确，只看不到节点碎片和其他节点级约束；在节点级估算下，只要一组放不进单个节点就是 0，表现为「调度失败」。
- **最大 Pod** 按每组 Pod 数的倍数高估，表现为「放多了」，之后只能靠重调度往外挪，而估算器继续高估。
  Karmada 内置的 RayService、FlinkDeployment 用的是「Pod 数 + 取最大」，但它们的副本单位就是 Pod，和 ModelServing 不同。【源码】

选整组求和，因为它的失败方式是显式的（调度失败，有事件），而不是悄悄把副本压到一个放不下的集群。代价是适用条件要写清楚（5.3）。

### 4.4 为什么不实现 `componentResource`

打开 `MultiplePodTemplatesScheduling`（alpha，默认关）并且解释器实现了 `componentResource` 时，
Karmada 不再调用 `GetReplicas`，绑定上只有组件、没有副本数；而 `ReviseReplica` 仍然会被调用，每个成员集群的 `spec.replicas` 被写成 0。

【Kind】模板 2 个副本、两个成员各 1 个、都在运行；打开门控并加上 `componentResource` 后更新一次模板：
两个成员的 `spec.replicas` 都变成 0，Pod 全部删除，而绑定仍然显示 `Scheduled=True`、`FullyApplied=True`。

Karmada 内置的定制里没有一个同时实现 `componentResource` 和 `replicaRevision`，所以上游自己没有碰到。
解释器 YAML 里用注释写明原因，防止以后有人「顺手补上」。

### 4.5 交付位置

先放在 kthena 仓库（示例 YAML、按 Karmada 第三方定制目录格式写的 testdata、一个离线校验脚本）。
用户自己安装的定制优先于 Karmada 内置的【源码】，所以 kthena 可以随时发更新的版本。

是否提交给 Karmada 作为内置第三方定制
（`pkg/resourceinterpreter/default/thirdparty/resourcecustomizations/workload.serving.volcano.sh/v1alpha1/ModelServing/`），**暂缓决定**。
内置的好处是 3.2 的「先装解释器」约束自动满足。坏处是同一个 kind 只能有一种解释方式：内置之后「ModelServing 按副本拆分」就成了 Karmada 发行版的默认行为，
以后如果改成按组分发（第 13 节），就是破坏性变更。等第 13 节的方向定了再提交。

## 5. 三种分发模式

### 5.1 支持矩阵

| 模式 | 是否依赖容量估算 | 第 1 阶段是否承诺 | 适用 |
| --- | --- | --- | --- |
| Duplicated | 否 | 是 | 在线服务多活：每个地域一份完整容量 |
| Divided + 静态权重 | 否 | 是【Kind】 | 各集群容量已知且稳定 |
| Divided + Aggregated 或动态权重 | 是 | 有条件，见 5.3【Kind：Aggregated】 | 离线推理：哪里有空闲 GPU 就用哪里 |

### 5.2 示例

```yaml
apiVersion: policy.karmada.io/v1alpha1
kind: PropagationPolicy
metadata:
  name: deepseek
  namespace: llm
spec:
  resourceSelectors:
    - apiVersion: workload.serving.volcano.sh/v1alpha1
      kind: ModelServing
      name: deepseek
  placement:
    clusterAffinity:
      clusterNames: [region-a, region-b, region-c]
    replicaScheduling:
      replicaSchedulingType: Divided
      replicaDivisionPreference: Aggregated   # 或 Weighted + dynamicWeight / staticWeightList
```

### 5.3 动态拆分的适用条件

动态拆分依赖估算器，有两个前提，都是 Karmada 侧的配置。

**前提 1：绕开「GPU 请求估算为 0」。** `CustomizedClusterResourceModeling` 默认开启，集群加入时会被填入只有 cpu、memory 两个维度的默认资源模型；
请求里只要有模型没覆盖的资源（例如 `nvidia.com/gpu`），general estimator 就返回 0，并且不回退到汇总估算。
这是 v1.18.0 起的行为（commit `804b08c81` 删掉了原来的回退分支）。【源码】

【Kind】默认配置下，一个 3 副本、每 Pod 1 GPU 的 Deployment，目标集群分别有 4 个和 8 个空闲 GPU：
general estimator 给 `[0, 0]`，scheduler-estimator 给 `[4, 8]`，调度器取最小值，报 `Clusters available replicas 0 are not enough to schedule`。

规避办法二选一：

- 清空 GPU 集群的 `Cluster.spec.resourceModels`：
  `kubectl patch cluster <name> --type=json -p '[{"op":"remove","path":"/spec/resourceModels"}]'`。
  更新时不会被重新填默认值。【Kind：清空后同一个 Deployment 调度成功】
- 在 karmada-scheduler 上关闭 `CustomizedClusterResourceModeling`。【包级实测】

**前提 2：一组放不进单个节点的 ModelServing，不能用 scheduler-estimator 的结果。** 否则估算为 0（4.3）。
注意它不是按集群生效的：候选集群里只要有一个连不上 scheduler-estimator，调度器就丢掉这次调度里所有集群的节点级结果，全部退回汇总估算。【源码 + Kind】
所以实际的选择是，这类 ModelServing 的候选集群要么都不部署 scheduler-estimator，要么接受「有一个没部署，其余的也用不上」。

【Kind】清空资源模型之后，3 个副本、每组 4 个 Pod × 1 GPU：general estimator 给 `[1, 2]`，scheduler-estimator 给 `[0, 2]`，
合计 2 小于 3，调度失败，一个也不放。去掉碎片集群的 scheduler-estimator 之后（调度器记一条连接错误，这次调度只剩 general estimator 的结果），
按 1 + 2 拆分，3 组全部 Running，控制面状态 `replicas: 3`、`availableReplicas: 3`。

整组能放进单个节点的 ModelServing 不受前提 2 影响。

满足这两条时，估算值是「（可分配 − 已分配 − 等待调度的 Pod 的请求）÷ 整组需求」，看不到 gang、碎片、拓扑、队列配额、nodeSelector，所以会高估。
高估的后果由第 8 节的重调度兜底。

### 5.4 模板写法约束

成员集群按**自己分到的副本数**校验和执行 `rolloutStrategy`，不是按模板里的全局副本数。【源码】
下面这些写法在单集群合法，拆分后会被成员集群的 kthena webhook 拒绝：

| 写法 | 被拒的情形 | 推荐写法 |
| --- | --- | --- |
| 整数 `maxUnavailable` 大于该集群分到的副本数 | 模板 `replicas: 4`、`maxUnavailable: 2`，某集群分到 1【Kind】 | 不写（默认 1），或用 1【Kind：去掉后通过】 |
| 整数 `partition` 大于该集群分到的副本数 | 模板 `partition: 8`，某集群分到 3 | 用百分比，或用 OverridePolicy 按集群做灰度 |
| 百分比 `maxUnavailable` 取整为 0 且没设 `maxSurge` | `25%`，某集群分到 3 | 同时设 `maxSurge: 1`（GPU 紧张的集群见 8.5） |
| 整数 `evictionStrategy.minAvailable` 大于该集群分到的副本数 | `minAvailable: 3`，某集群分到 2 | 改用 `evictionStrategy.maxUnavailable` |

【Kind】被拒时的表现：该集群的 Work 是 `Applied=False`，message 带 webhook 的原文
（`maxUnavailable cannot be greater than replicas`）；绑定是 `FullyApplied=False`；模板上有 `SyncFailed` 事件；
该集群上没有 ModelServing；控制面的汇总状态只统计了另一个集群（4 个副本里只显示 3 个），`observedGeneration` 不推进。

之后调度器或 descheduler 调整这个集群的副本数，如果调整后的值违反同一条规则，也会被拦住。【推断】

默认配置（不写 `rolloutStrategy`）不受影响。是否放宽 kthena 的这些校验会改变现有行为，本方案不做，列为待定事项。

## 6. 状态和排障

| 想知道什么 | 看哪里 |
| --- | --- |
| 总体容量 | 控制面 `kubectl get modelserving`：Replicas、Available、Updated 是各集群之和【Kind】。Duplicated 时会大于 `spec.replicas` |
| 每个集群的明细 | `ResourceBinding.status.aggregatedStatus[*]`，或 `karmadactl get` |
| 有多少组调度不上、从什么时候开始 | 模板 `status.unschedulableReplicas`、`status.unschedulableSince`（各集群里最早的那个）【Kind】 |
| 哪个集群有问题 | 聚合项的 `health`：有组没就绪时是 `Unhealthy`【Kind】 |
| 模板为什么没生效 | Work 的 `Applied=False` 和 message；绑定的 `FullyApplied=False`；模板对象上的 `SyncFailed` 事件【Kind】 |
| Pod 级的事件 | 只在成员集群 |

和单集群相比有两处变差，需要写进用户文档：

- **校验从同步变成异步。** kthena 的校验 webhook 不在 karmada-apiserver 上，非法的 spec、对不可变字段的修改在控制面都能提交成功，
  到成员才失败。可选的改善是把 kthena 的校验 webhook 也注册到 karmada-apiserver（校验器不依赖集群内的 client【源码】），
  但 5.4 那些依赖拆分结果的规则在控制面校验不出来。
- **被拒绝的集群会从汇总里消失。** Work 没应用成功时聚合项不带状态，这个集群上还在服务的 ServingGroup 不计入控制面的计数，看起来像容量掉了。【Kind】

## 7. 自动扩缩容：二选一

kthena autoscaler 直接改成员对象的 `spec.replicas` 和 role 的 `replicas`，并且只能跑在成员集群（它用本集群的 Pod 抓指标）。【源码】

【Kind】在成员上把 `spec.replicas` 从 3 改成 5：模板不带保留标签时，3 秒内被 Karmada 改回 3；模板带标签时保持 5。
带标签时绑定里仍然是 3，控制面 `spec.replicas: 4` 而 `status.replicas: 6`，绑定里的数字已经过期。

所以两种模式不能混用：

| 模式 | 分发 | 谁改副本数 | 需要 | 重调度 |
| --- | --- | --- | --- | --- |
| A：成员侧扩缩容 | Duplicated | 成员集群的 kthena autoscaler、HPA、KEDA | 模板加 `resourcetemplate.karmada.io/retain-replicas: "true"` | 不适用，重调度跳过带这个标签的对象 |
| B：控制面扩缩容 | Divided | 人工、FederatedHPA、CronFederatedHPA 改模板的 `spec.replicas` | 聚合状态里的 `replicas` 和 `labelSelector` | 适用 |

模式 A 加 Divided 是不支持的组合：Karmada 只给 Deployment 做「把成员副本数同步回模板」，ModelServing 没有，拆分和重调度都会基于过期的数字。

FederatedHPA 通过各成员的 scale 子资源工作，不限 kind；ModelServing 有 scale 子资源。能否用自定义指标端到端跑通【待验证】。

## 8. 重调度：调度不上的 ServingGroup 换集群

### 8.1 输入

kthena 已经提供了所需的全部信息（PR #2079）：

- `status.unschedulableReplicas`：至少有一个 Pod 是 `PodScheduled=False/Unschedulable` 的 ServingGroup 数。
- `status.unschedulableSince`：其中最老的一组从什么时候开始调度不上。
- 缩容时先删调度不上的组，再删其他未就绪的，最后才是 Running 的。

【Kind】这两个字段经过解释器到达 `ResourceBinding.status.aggregatedStatus` 和模板状态。
把某个集群的副本数从 2 降到 1 时，kthena 删掉的是调度不上的那一组，Running 的那一组 Pod UID 不变。

它区分了「调度不上」和「还在启动」。正在拉镜像、加载模型的组不算，不会被换走。

### 8.2 判定规则

对绑定里的每个集群，设反射状态为 `s`，绑定里该集群的副本数为 `spec`：

| # | 规则 | 防什么 |
| --- | --- | --- |
| 1 | 新鲜度：`s.generation == s.observedGeneration`，且 `s.desiredReplicas == spec`；否则本轮跳过这个集群 | 用过期状态重复降副本，删到正在启动的组（见下） |
| 2 | 集群 Ready，且该集群的聚合项 `applied` 为 true | 成员不可达时状态停在旧值 |
| 3 | 阈值：`now - s.unschedulableSince >= threshold`（默认 5 分钟，和 karmada-descheduler 现有参数一致） | 短暂排队 |
| 4 | 数量：`min(s.unschedulableReplicas, spec - s.readyReplicas)`，不小于 0 | 滚动升级的 surge 组被多算 |
| 5 | 只处理动态拆分（Aggregated、动态权重）的绑定；跳过带 `retain-replicas` 标签的模板 | 静态权重和 Duplicated 下调度器会立刻把副本数改回去 |
| 6 | 有去处：见 8.3 | 降完没有集群能接，副本在一段时间内不属于任何集群 |
| 7 | 可选：只迁移最近一个阈值窗口内观察到的不可调度数的最小值 | 一个老的组等了很久，刚扩出来的 10 个组跟着立刻被迁走 |

规则 1 的例子：集群 A 的 `spec=5`，2 组 Running、1 组启动中、2 组调度不上。第一轮把 A 降到 3，成员删掉 2 个调度不上的组。
成员状态还没同步回来时第二轮开始，看到的仍是「就绪 2、调度不上 2」，会再降到 2；成员此时只剩 2 个 Running 和 1 个启动中的组，
被删的就是那个已经调度成功、正在加载模型的组。回显成员的 `spec.replicas` 就是为了识别这种过期状态。【推断，待验证】

karmada-descheduler 处理 Deployment 时没有这个问题，因为它每轮都通过估算器到成员集群实时数 Pod。

### 8.3 降副本之后发生什么，以及为什么要「有去处才降」

karmada-descheduler 的做法是只把问题集群的副本数降下去，去向交给调度器。在 Kind 上手工模拟了这个动作，观察到四种结果【Kind】：

| # | 降之前 | 别的集群 | 结果 |
| --- | --- | --- | --- |
| 1 | A 有 1 个副本，调度不上；降到 0 | C 有容量 | 副本到了 C。A 被移出绑定，A 上的 ModelServing 被删除 |
| 2 | A 有 2 个副本，1 个 Running、1 个调度不上；降到 1 | C 有容量 | 副本到了 C。A 上 Running 的组不受影响 |
| 3 | 同 2 | 都满了 | 调度失败。这个副本 5 分 06 秒不属于任何集群，然后调度器重试，把它放回 A，`unschedulableSince` 重新开始计时 |
| 4 | 模板扩容 1 个副本（没有降副本） | C 的估算可用量比 A 大 | 新副本到了 C |

结果 1、2 里副本没有被还给 A，原因是 Karmada 的集群汇总把「还没绑定节点的 Pod」的请求计入 `allocating` 并从可用量里扣掉【源码】。
调度器在绑定被修改后不到一秒就重新调度，这时卡住的 Pod 还在，A 看起来是满的（估算值 0）。
100 秒后再看，这些 Pod 已经被删掉、集群状态已刷新，A 的估算值回到了高估的 1。

包级实测里出现过「副本被原样还给 A」的结果，那是在假定估算器已经给出 A 有余量的前提下测的；在真实的时序里这个前提不成立。
它仍然可能发生在调度器积压、或者调度先失败后重试的情况下，结果 3 就是后者。

结果 3 说明了为什么需要规则 6。降之前这一组至少在 A 里排队，A 一有资源就能启动；降之后它在 5 分钟内不在任何集群排队，
回来之后又要重新等满阈值，然后再被降一次，循环下去。调度器的 5 分钟来自它的不可调度队列上限【源码】。

规则 6 的做法：降之前用和 general estimator 相同的公式，检查绑定的候选集群里是否有别的集群能放下至少一个副本
（`(allocatable − allocated − allocating) ÷ 整组需求 >= 1`，集群 Ready）。有，才降，并且最多降「别的集群估算能接收的数量」；
没有，就不动，记一条事件。去向仍然由调度器决定，调度器的亲和、污点、API 启用等过滤全部照常生效。

规则 6 是估算，不是保证：预检通过但调度仍然失败时，退化成结果 3，和今天 karmada-descheduler 对 Deployment 的行为一样。

**备选：定向迁移。** 在同一次更新里把 A 减 n、把 C 加 n，各集群副本之和不变，调度器不会重新调度。
【Kind：调度器日志是 `Don't need to schedule`，新加入绑定的集群正常建出 Work 和 ServingGroup】
它完全避开结果 3 和「还回 A」的可能，代价是重调度组件要自己选目标集群，等于绕过调度器的过滤插件。
第一版不采用；如果实际运行中「预检通过但调度失败」出现得多，再启用。

### 8.4 放在哪里

Kind 的结果说明重调度只需要改 karmada-descheduler，不需要改调度器。所以建议只做一份实现，放在 `karmada/pkg/descheduler`：

| 改动 | 说明 |
| --- | --- |
| GVK 可配置 | 现在硬编码 Deployment，源码里有 `TODO: make it as an option`。上游 PR #8109 已经提出同样的参数，和它协调 |
| 不可调度数的状态来源 | 聚合状态里有 `unschedulableReplicas` 键的绑定，按 8.2 的规则从状态取数；否则走原来的 estimator。两种来源按绑定二选一 |
| 规则 6 的预检 | 只对状态来源生效；Deployment 的行为不变 |

交付方式有两种，可以并行：

- 向 Karmada 上游提交。上游 issue #4046（2023 年，open）是同一个请求，#8104 是当前的讨论。
- 上游合入之前，从我们的 karmada 分支构建 karmada-descheduler 镜像给早期用户。karmada-descheduler 是可选组件，
  调度器和 controller-manager 保持原版，替换范围只有这一个 Deployment。

如果上游不接受状态来源（maintainer 在 #7662 的讨论里表示过「只看 workload 状态不够通用」），同一份算法可以打包成独立的控制器，
对 karmada-apiserver 工作，写 `ResourceBinding.spec.clusters` 的方式和 descheduler 相同。这是后备，不是首选。

先走哪条路是待决策点，见 [03-decisions.md](03-decisions.md)。

### 8.5 已知的边界

| 边界 | 后果 | 处理 |
| --- | --- | --- |
| 估算器持续高估问题集群（8.3 的结果 1、2 之后，A 的估算值回到 1） | 以后的扩容可能再次把副本放到 A，再等一个阈值后被换走 | 第 3 阶段解决估算；在那之前靠重调度反复纠正 |
| 滚动升级的 surge 组也计入 `unschedulableReplicas`【源码】 | GPU 已满的集群里，一次滚动升级可能把这个集群的份额换到别的集群 | 规则 4 限制数量；指南里对 GPU 紧张的集群推荐 `maxSurge: 0`。是否让 kthena 不统计 surge 组需要单独批准 |
| 原本在服务的组，role 扩容或 Pod 重建时新 Pod 调度不上，也整组计入【源码】 | 缩容时它排在最前，其余还在接流量的 Pod 一起被删 | 规则 4 的 `spec - ready` 能挡住一部分。是否只统计从未就绪过的组需要单独批准 |
| `unschedulableSince` 只有最老一组的时间 | 新出现的组可能没等满阈值就被迁走 | 规则 7 |
| 成员集群有节点自动扩容 | 节点到位前组可能已被迁走 | 阈值要大于节点扩容时间；文档说明 |
| 没有 gang 调度时，卡住的组里已经调度上的 Pod 会占着 GPU【Kind：1 个 worker Pod Running，entry Pod 调度不上】 | 浪费资源，直到这一组被换走 | 用 Volcano；这也是 kthena 默认 `schedulerName: volcano` 的原因 |
| 成员没装 Volcano 而 `schedulerName` 是 `volcano` | 没人给 Pod 写调度条件，`unschedulableReplicas` 一直是 0，永远不会被换走【推断】 | 用集群亲和排除这类集群；文档说明 |
| 成员运行的 kthena 没有 PR #2079 | 两个字段不存在，按 0 处理，这个集群不参与重调度【离线实测】 | 安全降级，不需要处理 |

### 8.6 不用的机制

| 机制 | 为什么不用 |
| --- | --- |
| WorkloadRebalancer | 触发全量重新分配，正在运行的组也可能被挪走、重新加载模型。#7662 的「保留可用副本」还是提案。可以作为人工「立即重平衡」工具写进指南，注明会中断服务 |
| 应用故障转移（`failover.application`） | 按「不健康持续多久」驱逐整个集群的份额，Running 的组一起走。只适合「整份放一个集群」的用法，并且容忍时间要远大于最坏的模型加载时间 |
| 让 Karmada 的估算器去数 ModelServing 的 Pod（#8084、#8109 的方向） | 对「副本 = 一组 Pod」的 workload，数 Pod 需要分组语义（忽略正在删除的组、被 scheduling gate 挡住的 Pod、gang 不满足时只有部分 Pod 被标记），这些已经在 kthena 控制器里实现，搬进 Karmada 是重复实现。它仍然是单模板 workload 和抢占的正确方向，两者不冲突 |

## 9. 其他行为

### 9.1 滚动升级

模板更新同时到达所有集群，各集群按自己的预算同时滚动。全局同时不可用的组数是各集群预算之和：
5 个集群、`maxUnavailable: 1`，最多同时有 5 组不可用。需要一次只动一个集群时，用 OverridePolicy 按集群改镜像或 `partition`，
或用 `PropagationPolicy.spec.suspension.dispatchingOnClusters` 暂停向部分集群下发。没有全局的灰度控制器，要用户自己编排。【源码，待验证】

`partition` 按集群生效：序号在每个集群各自从 0 开始。金丝雀期间副本进入一个新集群时，新建的组全部是新版本。【源码】

### 9.2 从控制面缩容

跨集群缩容不看就绪情况。【Kind】模板从 10 缩到 8，三个集群分别有 2、4、4 个副本，其中 2 个副本的集群里 1 个 Running、1 个调度不上：
Aggregated 把这个集群整个清空，Running 的那一组也被删了。集群内部「先删调度不上的」的顺序只在集群内成立。

### 9.3 集群副本数降到 0

Karmada 把 0 副本的集群从调度结果里去掉，对应的 Work 被删，成员上的 ModelServing 被删除【Kind】，
ControllerRevision 历史和按依赖下发的 PVC 也跟着移除，回收策略是 `Delete` 时模型缓存就没了【源码】。

建议：模型缓存的 PVC 用独立的 PropagationPolicy 下发，不走依赖；需要保留对象时在 karmada-scheduler 上开 `--enable-empty-workload-propagation`。【待验证】

### 9.4 流量和相关资源

`ModelServer.workloadSelector` 只选 router 所在集群的 Pod。副本分散以后，每个可能有副本的集群都要有 kthena-router、ModelServer、ModelRoute
（按 Duplicated 下发到同一组集群）。某个集群副本数为 0 时路由对象还在但没有后端，前面的全局负载均衡必须做健康检查。【源码】

### 9.5 成员集群故障

默认不迁移：集群 NotReady 后 Karmada 停止向它同步，状态停在旧值。集群故障转移要显式开启。
开启后优雅驱逐默认最多保留旧 Work 10 分钟；模型冷启动超过 10 分钟时旧副本会先被删。【源码】

### 9.6 接管和退出

- 接管已有的单集群 ModelServing：默认 `conflictResolution: Abort` 会报「已存在」并且不动现有对象。
  建议先用 Duplicated 或固定单集群的策略加 `Overwrite` 接管（UID 不变、Pod 不重启），确认后再改成 Divided。
  直接用 Divided 接管会把副本数改成分到的数，多出来的 Running 组被删。【源码 + 推断】
- 退出：模板带 `preserveResourcesOnDeletion: true` 时删模板会保留成员对象。注意这个选项对「集群从调度结果里被移除」同样生效，
  和重调度一起用会留下没人管的 ModelServing。【源码】

### 9.7 控制面不可用

成员集群继续运行，kthena 和 Volcano 都在本地。停掉的是模板下发、状态汇总、重调度和 FederatedHPA。【推断】

## 10. 分期和交付物

| 阶段 | 内容 | 落在哪里 | 改实现代码 |
| --- | --- | --- | --- |
| 1 | 解释器 YAML 和 testdata；使用指南（拓扑、安装顺序、三种模式、5.3 的配置、5.4 的写法约束、排障、接管和退出、不支持的组合）；示例策略 | kthena：`examples/`、`docs/`。是否提交 Karmada 内置定制暂缓决定（4.5） | 只有文档和 YAML |
| 2 | karmada-descheduler：GVK 可配置、状态来源、8.2 的规则、有去处才降 | karmada：`pkg/descheduler`、`cmd/descheduler` | 是 |
| 3 | 调度器按「副本数 × 组件集」拆分：两个钩子都实现时绑定同时带副本数和组件，用 `MaxAvailableComponentSets` 作为各集群的可用副本数 | karmada，需要先在上游立项（关联 #5115、#7895） | 是 |

另外有三个刻画中发现的 Karmada 问题，独立于上面的阶段，建议向上游报告（只起草，发布需要授权）：

| # | 问题 | 证据 | 修复方向 |
| --- | --- | --- | --- |
| 1 | 请求里有资源模型未覆盖的资源时 general estimator 返回 0（v1.18.0 起），默认配置下 GPU 工作负载不能动态拆分 | Kind：`kind-C2a-gpu-estimate-zero.log` | 恢复回退到汇总估算 |
| 2 | 绑定从非工作负载变成工作负载时，binding controller 在调度器分配副本之前按 0 改写成员对象 | Kind：`kind-C10*.log` | `ensureWork` 只在调度器确实做过副本分配时才调用 `ReviseReplica` |
| 3 | 只有 components 的绑定调用了 `ReviseReplica(0)` | Kind：`kind-C3-component-resource-trap.log` | 同上，是同一个根因 |

第 3 阶段不设时间。PR #8084 当前的方向是「多模板 workload 整体调度」，和「按组件集拆分副本」是两种语义，需要先和上游对齐。

第 2、3 阶段有一个备选方向：按组分发（第 13 节）。它不需要第 3 阶段的调度器改动，但依赖另外两个 alpha 特性，并且有一个必须先解决的问题。

## 11. 不支持的用法

| 用法 | 后果 | 证据 |
| --- | --- | --- |
| 不装解释器，策略写 `Divided` | 每个集群都是完整副本数，没有报错 | Kind |
| 已经有模板之后再装解释器（没有暂停下发） | 下一次模板更新时所有集群的组被删除；调度失败时停在 0 | Kind |
| 解释器里加 `componentResource` 并打开 `MultiplePodTemplatesScheduling` | 所有成员的副本数变成 0 | Kind |
| 默认配置的 Karmada + 动态拆分 + GPU 请求 | 调度失败，`available replicas 0` | Kind |
| 多节点的 ServingGroup + scheduler-estimator + 动态拆分 | 该集群估算为 0；合计不够时一个也不放 | Kind |
| 成员侧自动扩缩容 + `Divided` | 绑定里的副本数过期 | Kind |
| 把 ModelBooster 生成的 ModelServing 交给 Karmada | 两个控制器写同一个对象 | 源码 |
| `failover.application` + Divided | 一个组没就绪超过容忍时间，整个集群的份额被驱逐 | 源码 |
| 负数 `spec.replicas` | 控制面接受，成员拒绝；调度器的行为没有确认 | 源码 |

## 12. 对 #8104 讨论的回应要点

供准备社区回复时使用，不代表已经发出。

1. **「求和后的请求会被按单节点估算」**：成立，Kind 上复现了：真实能放 1 组的集群被 scheduler-estimator 估算成 0。
   这是估算问题，和「不可调度副本数从哪里来」是两件事。短期靠配置限定适用范围，长期请上游考虑「按完整组件集拆分副本」，
   节点级的组件集估算在我们测的全部场景里都对。
2. **「descheduler 没有 workload 自定义的重调度单位」**：单位就是解释器已经定义的副本。descheduler 本来就按它增减，
   workload 控制器用同一个单位报不可调度数即可。
3. **「不可调度数应该从 Pod 来」**：对单模板 workload 成立。对「副本 = 一组 Pod」的 workload，分组语义在 workload 控制器里，
   聚合状态是它对外的契约；descheduler 已经在按约定键读 `readyReplicas`。状态来源可以作为可选项，不影响 #8084 的 Pod 路径。
4. **「调度器会考虑可用资源，为什么还会不可调度」**（maintainer 的提问）：汇总估算看不到节点碎片、nodeSelector、拓扑和队列配额。
   Kind 上用一个估算器看不到的 nodeSelector 复现了：估算 1，实际 0。
5. **改动范围比预想的小**：Kind 上手工模拟降副本之后，原版调度器把副本放到了别的集群，没有还给原集群。
   所以只需要改 descheduler，不需要改调度器。需要补的是「没有去处时不要降」。
6. **另外两个和 ModelServing 无关的问题**：默认配置下 GPU 请求估算为 0；绑定变成工作负载的瞬间成员副本数被写成 0。值得单独报告。

## 13. 备选方向：按组分发

方案初稿之后追加的问题：如果把 ServingGroup 做成 CR，是不是更好的选择。完整评估在 [02-servinggroup-cr.md](02-servinggroup-cr.md)，这里只记结论和它对本方案的影响。

按组分发的意思是：每个 ServingGroup 在控制面上是一个独立对象，Karmada 把它整体放进恰好一个集群，用多组件估算。

| 问题 | 结论 | 证据 |
| --- | --- | --- |
| 这个形状是不是更好 | 估算上是：本方案估错的三种情况（多节点的组、组内有大 Pod、带 nodeSelector）都估对了；放不下的组在控制面排队，不进成员集群 | Kind |
| 需不需要新的 ServingGroup CR | 不需要。`replicas: 1` 的 ModelServing 就能当单元，kthena 不用改 | Kind |
| 已有的 ServingGroup CR 原型能不能直接用 | 不能。它的 spec 不带 Pod 模板，下发后没有父对象，组内控制器不处理它 | 源码 |
| 现在能不能用于生产 | 不能。正在运行的单元会被搬到别的集群：往策略里加一个集群，或者调一下 role 的副本数，都会触发。上游 #7895 把它列为未完成项 | Kind |
| 卡住的组换集群是不是变简单了 | 起步更简单（现有的应用故障转移，不改代码），但没有去处时单元会在集群之间来回搬，比本方案的循环更频繁 | Kind |

对本方案的影响：

- 第 1 阶段不变。
- 4.5：解释器是否提交为 Karmada 内置定制，暂缓决定。
- 第 2、3 阶段：如果主要用法是按容量动态放置，并且「运行中的单元会被搬走」有了解法，按组分发可以替代改 descheduler 和第 3 阶段。
  在同一个 Karmada 控制面上，ModelServing 不能同时按本方案和按组件解释，所以到时候是二选一，不是叠加。

