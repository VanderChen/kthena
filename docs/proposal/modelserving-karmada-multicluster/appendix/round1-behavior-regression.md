# 任务 014 — 客户行为与回归分析（ModelServing：单集群 vs 经 Karmada 管理）

角色：Customer behavior and regression analyst（子代理）。日期：2026-11-28。

## 1. 结论

1. **成员集群内部的行为不变，也不会和 Karmada 互相覆盖。** kthena 控制器对 ModelServing 只写 `status`，不加 finalizer、label、annotation；Karmada 注入的元数据不进 revision hash，也不会传到 Pod；Karmada 的 execution controller 比较成员对象前会去掉 `status`。所以对象被 Karmada 接管不会触发滚动，状态变化也不会触发重复下发。（静态）
2. **最重的回归来自「副本被拆开」。** `maxUnavailable`、`maxSurge`、`partition`、`evictionStrategy.minAvailable` 原来按全局副本数理解，现在每个成员按自己分到的副本数解析。后果有两层：可用性预算被放大（N 个集群同时滚动）；成员侧校验会直接拒绝原本合法的对象（例如模板 `replicas: 10`、`maxUnavailable: 3`，分到 2 个副本的集群拒绝），而且之后 Karmada 调整该集群副本数也会被同一条校验拦住。（kthena 侧静态，端到端需 Kind）
3. **校验从同步变成异步。** kthena 的校验 webhook 不在 Karmada API server 上跑，非法的 spec 和不可变字段的修改在控制面都能提交成功，只在成员上失败。错误出现在 `Work` 的 `Applied=False`、`ResourceBinding` 的 `FullyApplied=False` 和 `SyncFailed` 事件里，控制面 ModelServing 的 `status` 上没有任何痕迹；失败集群的状态还会从汇总里消失。
4. **方案草稿有五处没覆盖到的行为。**
   - `Retain` 只提了 `spec.replicas`。`DisaggregatedTarget` 自动伸缩改的是 `spec.template.roles[i].replicas`，会被 Karmada 反复改回去。
   - 成员侧自动伸缩加 `Retain` 以后，descheduler 调低的副本数在成员上不生效：调度不上的 group 留在原集群，同时别的集群又多建了一份。
   - `ReflectStatus` 的字段清单没有 `labelSelector`、`currentReplicas`、`currentRevision`、`updateRevision`；`observedGeneration` 和 `conditions` 怎么跨集群合并没有定义。
   - `ResourceInterpreterCustomization` 没装（或晚于模板创建）时，`Divided` 策略会退化成每个集群都拿到完整副本数。
   - 集群副本数变成 0 时，Karmada 默认删除该集群的整个 ModelServing，连同 ControllerRevision 和按依赖下发的 PVC。
5. **重调度本身有四类风险。** 误判（一个原本在服务的 group 只要有一个新 Pod 调度不上就整组被搬走）；观察和执行之间的竞态（可能删到 Running 的 group）；来回搬（`Aggregated` 优先把副本放回刚被调低的集群）；以及和 `failover.application` 叠加时整个集群的份额被一次性驱逐。

第 5 节是预期变化表，第 6 节是回归风险表，第 7 节是要补的测试，第 8 节是没法靠读代码确认的部分，第 9 节列了建议方案补上的内容。

## 2. 范围、方法和证据标注

- 只读静态分析。没有起集群，没有改源码，没有运行项目测试。除了只读的查看命令（`grep`、`sed`、`git log`、`gh` 查询），只执行过一条计算命令：用 `uv run --no-project --with pyyaml` 算 CRD 的 JSON 大小。两个源码目录的 `git status` 都是干净的。
- 源码：
  - `karmada:` = `<workspace>/karmada`，commit `6d007132ec5112d2eed41b602d4aeeebaf96f4fc`。
  - `kthena:` = `<workspace>/kthena-014`，commit `1a5145e90e0e1d70c75def0ef9500055e2904c5f`（含 PR #2079）。
- 上游只读查询：karmada-io/karmada#8104（讨论全文）、PR #8084、PR #7662（标题和状态，均 OPEN）；volcano-sh/kthena#2067（CLOSED）、PR #2079（MERGED，`4323a9bf`）。
- 「之前」指单集群 kthena；「之后」指任务说明里的方案草稿（6 条）。草稿里的通用 descheduler 和 kthena 侧 rescheduler 都还不存在，相关结论按现有 `karmada-descheduler` 的代码加草稿的描述推出。
- 证据标注：
  - **静态**：直接读到的代码行为，附 `路径:行号`。
  - **推断**：把几条静态事实连起来得到的结论，没有执行过。
  - **需 Kind**：必须在集群上跑才能确认。
- 分类：**预期变化** / **回归** / **中性**。
- 每条发现后面的「覆盖」指向第 7 节的测试编号（`U-` 单测，`L-` Lua 解释器 testdata，`K-` Kind 场景）。

## 3. 三个前提

前两点和任务说明里的背景描述不一致，第三点是后面反复用到的事实。分析都以代码为准。

- **P1 ModelServing 没有 mutating webhook。** 这个 commit 的 mutating webhook 只覆盖 LeaderWorkerSet、ModelBooster、AutoscalingPolicy；ModelServing 只有校验 webhook。默认值全部来自 CRD schema（`replicas: 1`、`schedulerName: volcano`、`recoveryPolicy: RoleRecreate` 等）。只要 Karmada API server 上装的是同一版 CRD，默认值在控制面就已经填好，成员上不会再变。
  证据（静态）：`kthena:charts/kthena/charts/workload/templates/kthena-controller-manager/component/mutating-webhook.yaml`；`kthena:cmd/kthena-controller-manager/main.go:210-227`；`kthena:pkg/apis/workload/v1alpha1/model_serving_types.go:41-42,47,68,77`。
- **P2 没有 `AutoscalingPolicyBinding`。** workload chart 的 CRD 只有 `autoscalingpolicies`、`modelboosters`、`modelservings`，伸缩目标写在 `AutoscalingPolicy` 的 `homogeneousTarget` / `heterogeneousTarget` / `disaggregatedTarget` 里。
- **P3 成员上会改 ModelServing `spec` 的只有四类。** kthena 的 ModelServing 控制器只调 `UpdateStatus`（`kthena:pkg/model-serving-controller/controller/model_serving_controller.go:2735,2945`）。改 `spec` 的是：
  - kthena autoscaler：merge patch `spec.replicas`（`kthena:pkg/autoscaler/controller/autoscale_controller.go:198-201`），JSON patch `spec.template.roles[i].replicas`（同文件 `:482-494`）；
  - 走 scale 子资源的 HPA、KEDA、`kubectl scale`；
  - ModelBooster 控制器，改它生成的同名子对象（`kthena:pkg/model-booster-controller/controller/model_serving_handler.go:70-77`）；
  - LWS 兼容控制器，只改它自己创建的对象（`kthena:pkg/model-serving-controller/controller/lws_controller.go:257-269`）。

## 4. 分领域对比

### 4.1 API 面和编写方式

**A1 用户要写的对象** — 预期变化
- 之前：一个 ModelServing。
- 之后：控制面上写 ModelServing 模板、`PropagationPolicy`，需要时加 `OverridePolicy`；管理员另外装 `ResourceInterpreterCustomization`（集群级对象）；CRD 要同时装在 Karmada API server 和成员集群。
- 覆盖：K-01。

**A2 `spec.replicas` 的含义** — 预期变化
- 之前：本集群的 ServingGroup 数。
- 之后：`Divided` 时是全局总数；`Duplicated` 时是每个集群的数。成员对象的 `spec.replicas` 由 `ReviseReplica` 按调度结果改写，两种模式都会改写。
- 证据（静态）：`karmada:pkg/controllers/binding/common.go:80-96`；`karmada:pkg/scheduler/core/assignment.go:181-187`。
- 覆盖：L-03，K-01。

**A3 `rollingUpdateConfiguration.maxUnavailable` / `maxSurge`** — 回归
- 之前：按全局 `replicas` 解析一次。
- 之后：每个成员按自己的副本数解析。整数值在每个集群各生效一次（5 个集群、`maxUnavailable: 1`，同时最多 5 个 group 不可用；`maxSurge: 1` 同时多占 5 组 GPU）。百分比在每个集群各自取整：`maxUnavailable` 向下取整，小集群可能得到 0；`maxSurge` 向上取整，合计会大于全局值。
- 证据（静态）：`kthena:pkg/model-serving-controller/utils/utils.go:628-653`；`kthena:pkg/model-serving-controller/controller/model_serving_controller.go:1846-1851`。
- 覆盖：U-K1，K-03，K-13。

**A4 `partition`** — 回归（语义变化）
- 之前：保护序号 `[0, partition)`，金丝雀数量是 `replicas - partition`。
- 之后：序号在每个集群各自从 0 开始，每个集群各自保护 `[0, partition)`。整数 `partition: 8` 在只分到 3 个副本的集群里等于全部保护；全局金丝雀数量不再是 `replicas - partition`。百分比按成员副本数向上取整，副本数为 1 的集群永远全保护。
- 证据（静态）：`kthena:pkg/model-serving-controller/controller/model_serving_controller.go:2986-3003,1021`。
- 覆盖：U-K4，K-14。

**A5 序号和名字** — 中性
- 之后：`<name>-<ordinal>` 的 group 名和 Pod 名在各集群重复。集中采集的日志和指标要带集群标签才能区分。
- 推断：如果以后有跨集群的路由用 `modelserving.volcano.sh/group-name` 配对 prefill/decode，同名 group 可能被错配；当前 kthena-router 只看本集群，不受影响。

**A6 `recoveryPolicy`、`restartGracePeriodSeconds`** — 中性
- 纯集群内行为，成员控制器照旧执行。

**A7 `rolloutStrategy.evictionStrategy`** — 回归（语义变化）
- 之前：`minAvailable` / `maxUnavailable` 按全局副本数算驱逐预算。
- 之后：驱逐 webhook 在成员上按成员副本数算，每个集群一份预算。
- 证据（静态）：`kthena:pkg/model-serving-controller/webhook/eviction_handler.go:338,557`。
- 覆盖：U-K1。

**A8 `status.labelSelector` 和 scale 子资源** — 中性（草稿有缺口）
- 之前：`labelSelector` 选中每个 group 的一个 entry Pod，HPA/KEDA 通过 scale 子资源使用。
- 之后：成员上不变。控制面的 scale 子资源：`spec.replicas` 可读可写；`status.replicas` 是汇总值；`status.selector` 取自 `.status.labelSelector`，草稿的 `ReflectStatus` / `AggregateStatus` 没列这个字段，所以是空的。FederatedHPA 不受影响，它从每个成员的 scale 子资源取 selector。
- 证据（静态）：`kthena:pkg/apis/workload/v1alpha1/model_serving_types.go:485,490`；`kthena:pkg/model-serving-controller/controller/model_serving_controller.go:2923-2941`；`karmada:pkg/controllers/federatedhpa/federatedhpa_controller.go:498-514`。
- 覆盖：L-04，L-05，K-24。

**A9 `bootstrapAccelerateStrategy.initialSeedReplicas`** — 预期变化
- 之前：百分比按 `spec.replicas * role.replicas` 算，种子副本在一个集群里。
- 之后：按成员副本数算，每个集群各自起种子；新进入的集群没有可用的 P2P 源，等于冷启动。
- 证据（静态）：`kthena:pkg/apis/workload/v1alpha1/model_serving_types.go:119-127`。

**A10 Volcano 队列注解** — 回归风险（依赖没被带过去）
- ModelServing 上的 `scheduling.volcano.sh/queue-name` 注解会随模板下发，成员控制器用它填 PodGroup 的队列。`Queue` 是集群级对象，`InterpretDependency` 只看 Pod 模板，不会把它带到成员集群。
- 证据（静态）：`kthena:pkg/model-serving-controller/podgroupmanager/manager.go:284-290`。
- 覆盖：K-23。

### 4.2 默认值和校验

**V1 默认值** — 中性
- 之前和之后一样由 CRD schema 填。控制面存下来的对象已经带默认值，下发到成员后 API server 不会再改，Karmada 的「内容没变就跳过更新」能命中。嵌入的 PodSpec 只有 CRD schema 里带的默认值（例如端口的 `protocol: TCP`），两边一样；原生 Pod 的其他默认值不会填进对象（webhook 只在私有副本上补默认值再校验，不改提交的对象）。
- 证据（静态）：`kthena:pkg/model-serving-controller/webhook/pod_template_validation.go:45-57`；`karmada:pkg/util/objectwatcher/objectwatcher.go:178-186`。
- 覆盖：K-05。

**V2 只在 webhook 里做的校验，控制面全部放行** — 回归（报错方式）
- 之前：`kubectl apply` 当场被拒。
- 之后：控制面接受，成员拒绝。控制面只保留 CRD 自带的校验（enum、`roles` 1–4 个且不重名、`restartGracePeriodSeconds >= -1`、CEL 的 `gangPolicy is required once set` 和 `minRoleReplicas is immutable`）。下面这些会漏到成员才报错：
  - role 名必须是小写 DNS-1035（CRD 的 pattern 允许大写和数字开头）；生成名长度；
  - 镜像名、Pod 模板的原生校验；
  - `replicas` 和 role `replicas` 非负（CRD 没有 `minimum`）；
  - 滚动预算、`gangPolicy.minRoleReplicas`、`workerReplicas > 0` 时必须有 `workerTemplate`；
  - `recoveryPolicy` 和 `rolloutStrategy.type` 的组合、`evictionStrategy`、role coordination。
- 错误出现的位置：
  - `Work` 条件 `Applied=False`，reason `AppliedFailed`，message 带成员 API server 返回的 webhook 拒绝信息；
  - `ResourceBinding.status.aggregatedStatus[i].applied=false` 和 `appliedMessage`，条件 `FullyApplied=False`；
  - `SyncFailed` 事件，同时记在 `Work` 和控制面的模板对象上；
  - 控制面 ModelServing 的 `status` 没有任何条件反映这次失败。execution controller 会带退避一直重试。
- 证据（静态）：`kthena:pkg/model-serving-controller/webhook/validator.go:132-148,190-204,273-303`；`kthena:pkg/apis/workload/v1alpha1/servinggroup_types.go:57-58,136-138`；`karmada:pkg/controllers/execution/execution_controller.go:316-337`；`karmada:pkg/util/work.go:38-65`；`karmada:pkg/util/helper/workstatus.go:137-143,181-190`。
- 覆盖：K-02。

**V3 和副本数挂钩的校验，遇上被拆开的副本** — 回归
- 之前：`maxUnavailable`、`partition` 是整数时不能大于 `replicas`；`maxUnavailable` 和 `maxSurge` 不能同时解析成 0；`evictionStrategy.minAvailable` 不能大于 `replicas`。按全局副本数判断。
- 之后：成员按自己分到的副本数判断，原本合法的模板在小集群上被拒：
  - `replicas: 10`、`maxUnavailable: 3`，分到 2 个副本的集群：`maxUnavailable cannot be greater than replicas (2)`；
  - `replicas: 10`、`partition: 8`，分到 3 个副本的集群：同样被拒；
  - `replicas: 10`、`maxUnavailable: 25%`、没设 `maxSurge`，分到 3 个副本的集群：`floor(0.75) = 0`，报 `maxUnavailable and maxSurge cannot both resolve to 0`。
- 校验在每次更新时都跑。descheduler 或 scheduler 之后把某个集群的副本数调小，如果调小后的值违反同一条规则，这次更新也会被拒：这个集群停在旧副本数，别的集群照样多建，总量超出期望。
- 成员关掉 webhook 时不报错，但运行时同样按成员副本数算（推断）：整数 `maxUnavailable` 大于副本数会让 `minAvailable` 变负，一轮就能删掉所有旧 group；百分比解析成 0 且没有 surge 时可删数量是 0，滚动卡住。
- 默认配置（不写 `rolloutStrategy`，`maxUnavailable` 默认 1）不受影响。
- 证据（静态）：`kthena:pkg/model-serving-controller/webhook/validator.go:228-259,565-600`；`kthena:pkg/model-serving-controller/controller/model_serving_controller.go:1846-1851`。端到端的拒绝路径是推断。
- 覆盖：U-K1，K-03。

**V4 不可变字段的修改** — 回归（报错方式）
- 之前：改 `plugins`、`schedulerName`、role 名集合（增删改名）、`roleCoordination` 当场被拒。
- 之后：控制面接受，所有成员拒绝，所有 `Work` 一直是 `Applied=False`，直到用户把模板改回去。按单集群的习惯「删了重建」会删掉所有成员上的实例。
- 证据（静态）：`kthena:pkg/model-serving-controller/webhook/validator.go:119-130,669-674`。
- 覆盖：K-04。

**V5 `Applied=False` 的集群从状态汇总里消失** — 回归
- `Work` 没应用成功时，汇总项只带 `applied=false` 和消息，不带上一次的 `status`。这个集群上还在服务的 group 不再计入控制面的 `replicas` / `availableReplicas`，看起来像容量掉了。
- 证据（静态）：`karmada:pkg/util/helper/workstatus.go:181-190`。
- 覆盖：L-05，K-04。

**V6 会不会反复互相覆盖** — ModelServing 本身中性；两种自动伸缩组合是回归
- ModelServing 本身不会（静态）：
  - kthena 控制器不改 `spec` 和元数据（P3）；
  - Karmada 更新时保留成员上多出来的 finalizer、ownerReference、label、annotation（`karmada:pkg/util/objectwatcher/objectwatcher.go:126-147`）；
  - 成员对象的 status 变化不会触发 execution controller（`karmada:pkg/controllers/execution/execution_controller.go:559-588`，`karmada:pkg/resourceinterpreter/default/native/prune/prune.go:81`）；
  - Karmada 写到成员对象上的 label/annotation（含每次模板 generation 变化都会更新的 `resourcetemplate.karmada.io/generation`）不进 revision 数据，也不会让 kthena 重新 reconcile（`kthena:pkg/model-serving-controller/utils/revision_data.go:47-66,185-222`；`kthena:pkg/model-serving-controller/controller/model_serving_controller.go:289`）。
- 会反复覆盖的两种情况：
  - 成员侧 kthena autoscaler 改 `spec.replicas`，没有 `Retain` 时每次都被 Karmada 改回模板值。草稿第 5 条已经要求 `Retain`。
  - `DisaggregatedTarget` 改 `spec.template.roles[i].replicas`，草稿的 `Retain` 没覆盖。execution controller 看到非 status 的差异就重新下发，autoscaler 下一轮再改回来，每一轮都是 role 的 Pod 被建了又删。
- 证据（静态）：`kthena:pkg/autoscaler/controller/autoscale_controller.go:460-496`；`karmada:pkg/util/objectwatcher/objectwatcher.go:149-207`。循环本身是推断，需 Kind。
- 覆盖：L-07，K-06。

**V7 整个对象过一遍 Lua** — 回归风险
- `ReviseReplica` 和 `Retain` 把完整对象转成 Lua table 再转回来。Lua 分不清空对象 `{}` 和空数组 `[]`，Karmada 用输入对象做参照来还原，路径匹配时忽略数组下标。Pod 模板里常见的 `emptyDir: {}`、`securityContext: {}`、`resources: {}` 都走这条路。
- 如果还原错了，成员的 Pod 模板校验会拒绝，表现是 `Applied=False`，不会悄悄改掉模板。
- 证据（静态）：`karmada:pkg/resourceinterpreter/customized/declarative/luavm/lua.go:185-203,258-275`；`karmada:pkg/resourceinterpreter/customized/declarative/luavm/lua_convert.go:38-80,99-180`。
- 覆盖：L-03，L-07。

**V8 负数 `spec.replicas`** — 未验证
- 控制面接受（CRD 没有 `minimum`），成员 webhook 才拒绝。`GetReplicas` 返回负数后 Karmada scheduler 的行为没有读到明确处理。
- 覆盖：L-02，K-02。

### 4.3 状态和错误

**S1 控制面 ModelServing 的 `status`** — 预期变化（草稿有缺口）
- 之前：控制器写全量状态。
- 之后：没有 `AggregateStatus` 时控制面 `status` 是空的；有了以后是各集群的汇总。草稿只列了 `replicas`、`updatedReplicas`、`availableReplicas`、`unschedulableReplicas`、`unschedulableSince`、`observedGeneration`、`conditions`，所以 `currentReplicas`、`currentRevision`、`updateRevision`、`collisionCount`、`labelSelector` 在控制面都是空的。
- 另外，自定义 `ReflectStatus` 一旦存在就取代默认的「整个 `.status` 原样上报」。kthena 设计文档里「字段原样到达控制面」这句话在装了解释器以后不再成立，`ResourceBinding.status.aggregatedStatus` 里也只剩 Lua 返回的字段。
- 证据（静态）：`karmada:pkg/controllers/status/common.go:164-166`；`karmada:pkg/resourceinterpreter/interpreter.go:339-366`；`karmada:pkg/resourceinterpreter/default/native/default.go:155-164`；`kthena:docs/proposal/modelserving-unschedulable-replicas-status.md:89`。
- 覆盖：L-04，L-05，K-01。

**S2 `kubectl get` / `karmadactl get`** — 预期变化
- 控制面 `kubectl get modelserving` 的 Replicas / Available / Updated 三列取汇总后的 `status`。`Duplicated` 时汇总值是「集群数 × `spec.replicas`」，会大于 `spec.replicas`。每个集群的明细要用 `karmadactl get` 或看 `ResourceBinding`。
- 覆盖：K-01。

**S3 `conditions` 的跨集群合并** — 回归风险（草稿没定义）
- 之前：`Available`、`Progressing`、`UpdateInProgress`、`CoordinatedRoleRolloutBlocked` 四种条件，其中 `Available` 和另外两个进度条件不会同时为 True；message 里带 group 序号。
- 之后：每个集群一套条件，序号在各集群重复。草稿只说「汇总计数器」，没说条件怎么合。照搬某一个集群的条件会误导；每次汇总都重新生成 `lastTransitionTime` 会让控制面 status 每次都变。
- 证据（静态）：`kthena:pkg/model-serving-controller/utils/utils.go:465-513`；`karmada:pkg/controllers/status/common.go:192-204`。
- 覆盖：L-05。

**S4 `observedGeneration`** — 回归风险
- 之前：等于本对象的 `metadata.generation`。
- 之后：成员的 generation 和模板的 generation 是两个计数空间（scheduler 或 descheduler 调副本数只涨成员的 generation）。原样上报再原样写到模板会得到错误的值，依赖它判断「已收敛」的工具（Argo CD 等）会误判。Karmada 的做法是：`ReflectStatus` 额外上报成员 `generation` 和注解 `resourcetemplate.karmada.io/generation`，`AggregateStatus` 在所有成员都观察到最新模板后才把模板的 `observedGeneration` 设成模板的 generation。
- 证据（静态）：`karmada:pkg/controllers/binding/common.go:270-274`；`karmada:pkg/resourceinterpreter/default/native/aggregatestatus.go:83-103`；`karmada:pkg/resourceinterpreter/default/thirdparty/resourcecustomizations/apps.kruise.io/v1alpha1/CloneSet/customizations.yaml`（`AggregateStatus` / `ReflectStatus` 两段）。
- 覆盖：L-04，L-05，K-01。

**S5 `unschedulableSince` 的汇总** — 中性（有两个实现陷阱）
- 取各集群最小值，和单集群「最老的那个」语义一致。
- 没有集群有调度不上的 group 时，字段必须不存在。CRD 里它是 `format: date-time`，写成空字符串会让整次 status 更新失败。
- 时间来自成员集群的时钟，descheduler 用控制面的时钟比较，集群间时钟偏差会让阈值提前或推后（未验证）。
- 证据（静态）：`kthena:charts/kthena/charts/workload/crds/workload.serving.volcano.sh_modelservings.yaml:18070-18075`。
- 覆盖：L-05。

**S6 合成的 `readyReplicas`** — 预期变化（有三个实现陷阱）
- descheduler 从 `aggregatedStatus[i].status.readyReplicas` 读就绪数，读不到就当成未知（-1）。
- `availableReplicas` 是 `omitempty`，值为 0 时成员 status 里没有这个键，Lua 里是 `nil`，必须显式写成 `readyReplicas = 0`。
- descheduler 对这个值做 `r.(float64)` 断言，不是数字会 panic。
- `readyReplicas` 不在 ModelServing 的 CRD schema 里，只能留在 `ResourceBinding` 里，不能写回模板的 `status`（会被裁掉，见 UP2 的同类问题）。
- 证据（静态）：`karmada:pkg/descheduler/core/helper.go:41-58,126-148`；`kthena:pkg/apis/workload/v1alpha1/model_serving_types.go:455`。
- 覆盖：L-04，U-M1。

**S7 `InterpretHealth`** — 预期变化（定义决定故障转移行为）
- 没有这个 hook 时 Karmada 一律当作 Healthy；Lua 里访问了不存在的字段会报错，结果是 Unknown。它的定义直接决定 RO5（应用故障转移）和集群驱逐时旧副本保留多久。
- 证据（静态）：`karmada:pkg/controllers/status/work_status_controller.go:270-290`。
- 覆盖：L-06，K-16。

**S8 kthena 的事件** — 回归（排障路径变长）
- `RoleCreating`、`InPlaceUpdateFallback`、`CoordinatedRoleRolloutBlocked` 等事件只在成员集群里；控制面对象上只有 Karmada 的事件。

**S9 成员不可达时的状态** — 回归风险
- `Work` 状态停在最后一次上报的值，控制面汇总继续把它算进去，`availableReplicas` 偏高。见 R9。

### 4.4 伸缩

**SC1 从控制面扩容** — 预期变化
- 动态拆分只分配增量，按估算的可用副本数分给各集群，`Aggregated` 优先已有副本的集群；静态权重按权重重新算。成员控制器在缺失的序号上建 group。
- 证据（静态）：`karmada:pkg/scheduler/core/division_algorithm.go:121-137`；`karmada:pkg/scheduler/core/assignment.go:151-178,199-211`；`kthena:pkg/model-serving-controller/controller/model_serving_controller.go:1006-1050`。
- 覆盖：K-01。

**SC2 从控制面缩容：哪个集群减副本** — 回归
- 之前：全局只有一个排序，先删调度不上的，再删其他没就绪的，最后才删 Running 的。
- 之后：这个顺序只在每个集群内部成立。跨集群时，动态拆分按上一次的分配比例减，静态权重按权重重新分，都不看就绪情况：全部 Running 的集群和全部调度不上的集群一起减。`Aggregated` 还会把副本最少的集群整个减到 0。
- 证据（静态）：`karmada:pkg/scheduler/core/division_algorithm.go:75-119`；`karmada:pkg/scheduler/core/assignment.go:199-211`。PR #7662 讨论的是同一类问题。
- 覆盖：K-07。

**SC3 成员内部删哪个 group** — 中性
- 优先级：调度不上（0）→ 其他未就绪（1）→ Running（2）；同级比 Pod 删除代价；再同级删序号大的。和单集群一致。
- 证据（静态）：`kthena:pkg/model-serving-controller/controller/binpack_scaledown.go:39-44,161-201`；`kthena:pkg/model-serving-controller/controller/model_serving_controller.go:3065-3111`。
- 覆盖：U-K2。

**SC4 某个集群的副本数变成 0** — 回归
- 之前：`replicas: 0` 只删 Pod，ModelServing 对象、ControllerRevision 历史、PVC 都在。
- 之后：scheduler 把 0 副本的集群从结果里去掉，对应的 `Work` 被删，成员上的 ModelServing 被后台级联删除，Pod、PodGroup、headless Service、ControllerRevision 一起消失。按依赖下发的 PVC、ConfigMap、Secret、ServiceAccount 跟着调度结果走，也会从该集群移除；PVC 如果是 `Delete` 回收策略，模型缓存就没了。之后副本再回到这个集群是一个全新的对象（新 UID、没有历史）。
- 缓解：`karmada-scheduler --enable-empty-workload-propagation=true` 会保留 0 副本的集群；PVC 用独立的策略下发，不走依赖。
- 证据（静态）：`karmada:pkg/scheduler/core/util.go:189-197`；`karmada:pkg/scheduler/core/generic_scheduler.go:110-112`；`karmada:cmd/scheduler/app/options/options.go:162`；`karmada:pkg/util/objectwatcher/objectwatcher.go:209-264`；`karmada:pkg/dependenciesdistributor/dependencies_distributor.go:391-427,753-786`；`kthena:pkg/model-serving-controller/controller/model_serving_controller.go:340`。
- 覆盖：K-08。

**SC5 金丝雀期间副本进入新集群** — 回归
- 新建的成员对象 `status.currentRevision` 是空的，所有序号都按新版本创建，`partition` 起不到保护作用。全局金丝雀比例被放大。
- 证据（静态）：`kthena:pkg/model-serving-controller/controller/model_serving_controller.go:1019-1023`。
- 覆盖：U-K4，K-14。

**SC6 成员侧自动伸缩（`HomogeneousTarget` / `HeterogeneousTarget` / HPA）** — 预期变化，和重调度叠加是回归
- 需要 `Retain` 保留成员的 `spec.replicas`。Karmada 只给 Deployment 做「把成员副本数同步回模板」，ModelServing 没有，所以控制面的 `spec.replicas` 和 `ResourceBinding` 的分配是过期的，汇总的 `status.replicas` 和 `spec.replicas` 对不上。
- 和 descheduler 冲突：descheduler 调低 `ResourceBinding` 里该集群的副本数，`Retain` 又把成员的值留住，调度不上的 group 不会被删；scheduler 却已经把这些副本分给别的集群。结果是总量超出期望，并且原集群的问题没解决。
- 在控制面创建并已下发的 `autoscaling/v2` HPA 会让 Karmada 自动给它指向的模板打上 `resourcetemplate.karmada.io/retain-replicas=true`；kthena 的 `AutoscalingPolicy` 不会触发这个标记。原生 Deployment 的保留逻辑是看这个 label 的。
- 证据（静态）：`karmada:pkg/controllers/deploymentreplicassyncer/deployment_replicas_syncer_controller.go:97`；`karmada:pkg/controllers/hpascaletargetmarker/hpa_scale_target_marker_worker.go:75-112`；`karmada:pkg/resourceinterpreter/default/native/retain.go:145-163`。和 descheduler 的冲突是推断。
- 覆盖：L-07，K-06。

**SC7 `DisaggregatedTarget`** — 回归
- 见 V6：改的是 role 的 `replicas`，草稿的 `Retain` 不覆盖。

**SC8 控制面自动伸缩** — 预期变化
- FederatedHPA 可以用：读模板的 scale 子资源，再到每个成员读 scale 和 Pod。选中的是每个 group 的一个 entry Pod，和单集群 HPA 一样。
- kthena 的 `AutoscalingPolicy` 不能放在控制面：它用本集群的 Pod lister，并直接访问 Pod IP 抓指标。它的 panic / stable 策略、异构优化、P/D 比例伸缩在「控制面伸缩」模式下都用不上。
- 证据（静态）：`karmada:pkg/controllers/federatedhpa/federatedhpa_controller.go:471-552`；`kthena:pkg/autoscaler/autoscaler/metric_collector.go:248,408`。
- 覆盖：K-24。

### 4.5 重调度

**R1 被搬走的 group 损失什么** — 预期变化
- 之前：调度不上的 group 一直排队，资源空出来就地启动。
- 之后：超过阈值后整组 Pod 被删，在另一个集群重新建，新集群要完整冷启动（拉镜像、下载或加载模型）。
- 一个从未就绪的 group 损失的只是排队位置；用 Volcano gang 调度时它的 Pod 都还没绑定节点，没有已加载的模型，也没有在途请求。会丢掉已加载的模型和在途请求的是三种情况：没有 gang 调度时已经跑起来的那部分 Pod、R2 的竞态、R3(a) 的部分在服务的 group。
- 覆盖：K-09。

**R2 观察和执行之间的竞态** — 回归
- descheduler 决定「减 N 个」时依据的是上一次上报的状态；成员控制器执行时按当时的优先级挑 N 个。中间如果调度不上的 group 已经调度上了，被删的就是正在加载模型的 group；如果它们已经 Running，被删的就是 Running 的 group，正在处理的请求被中断。「不低于就绪数」的保护只在决策那一刻成立。
- 证据（静态）：`karmada:pkg/descheduler/descheduler.go:216-231`；`kthena:pkg/model-serving-controller/controller/model_serving_controller.go:3065-3111`。竞态本身是推断。
- 覆盖：U-M1，K-09。

**R3 误判** — 回归
- (a) **部分在服务的 group。** 一个原本 Running 的 group，只要进入 `Scaling`（role 扩缩容）或 `Creating`（Pod 失败或被重建），又有一个新 Pod 调度不上，就整组计入 `unschedulableReplicas`，缩容时排在最前。它其余就绪的 Pod 可能还在接流量。单集群下它原地等着恢复。
  证据（静态）：`kthena:pkg/model-serving-controller/controller/model_serving_controller.go:1200,1274,2302-2306,2370-2376,2796-2823`；`kthena:pkg/model-serving-controller/controller/binpack_scaledown.go:181-185`。
- (b) **抢占进行中。** 调度器已经选好节点、在等被抢占的 Pod 退出时，Pod 仍是 `Unschedulable`，判断里不看 `nominatedNodeName`。
  证据（静态）：`kthena:pkg/model-serving-controller/utils/utils.go:365-388`。
- (c) **scheduling gate。** 被 gate 住时不计数，但 gate 去掉后如果调度不上，时间从 gate 开始算，可能一出现就超过阈值。设计文档已说明。
  证据（静态）：`kthena:docs/proposal/modelserving-unschedulable-replicas-status.md:54`。
- (d) **集群自动扩容中。** Cluster Autoscaler 靠 `Unschedulable` 的 Pod 触发扩节点，节点到位前 Pod 可能已被搬走（推断）。
- (e) **滚动时的 surge group。** 见 R6。
- 覆盖：U-K3，U-K5，K-11。

**R4 阈值按最老的 group 算** — 预期变化（文档已说明，和 Deployment 的行为不同）
- 最老的 group 超过阈值后，这个集群所有调度不上的 group 一起被搬，包括刚出现几秒的。Karmada 对 Deployment 是按每个 Pod 各自算。一个长期卡住的 group 加一批刚扩出来的 group，后者会在下一轮立刻被搬走。
- 证据（静态）：`kthena:docs/proposal/modelserving-unschedulable-replicas-status.md:53`。
- 覆盖：U-M1。

**R5 来回搬** — 回归
- `Aggregated`：补副本时把上次有副本的集群排在最前，一直取到累计可用数够用为止。刚被调低的集群只要还有副本，并且估算器认为它有空位，这些副本就原样分回去。通用估算器看不到 gang、GPU 碎片、队列配额、网络拓扑，这种情况很容易出现。
- 静态权重和 `Duplicated`：scheduler 每次按权重或原值重算，调低的副本数马上被恢复。现有 descheduler 用策略过滤掉了这两种；kthena 侧的 rescheduler 如果不做同样的过滤，就会周期性地删了建、建了删。
- 证据（静态）：`karmada:pkg/scheduler/core/assignment.go:151-178,199-211`；`karmada:pkg/scheduler/core/division_algorithm.go:75-101`；`karmada:pkg/util/binding.go:55-66`；`karmada:pkg/descheduler/core/filter.go:52-62`。
- 覆盖：U-M2，U-M4，K-10。

**R6 滚动更新期间** — 回归风险
- `maxSurge > 0` 时 surge group 也是 group。集群满了它调度不上，会计入 `unschedulableReplicas`，于是一次滚动更新就可能触发跨集群搬迁：这个集群的份额被永久调低，别的集群按新版本多建。单集群下只是滚动卡住。调低以后原集群的滚动怎么继续，要在集群上看。
- 证据（静态）：`kthena:pkg/model-serving-controller/controller/model_serving_controller.go:819-865`；`kthena:pkg/model-serving-controller/controller/surge_rollout.go:91-112,193-234`。结果是推断。
- 覆盖：U-K5，K-12。

**R7 集中搬迁** — 回归风险
- descheduler 每 2 分钟扫一遍所有 binding。一个集群 GPU 用尽时，上面所有 ModelServing 在同一轮被调低，scheduler 逐个 binding 独立估算，都选中同一个「看起来最空」的集群，把它打满，5 分钟后再搬一轮。`SchedulingOvercommitProtection`（alpha，默认关）能减轻。
- 证据（静态）：`karmada:cmd/descheduler/app/options/options.go:33-34`；`karmada:pkg/descheduler/descheduler.go:162-171`；`karmada:pkg/features/features.go:195`。
- 覆盖：K-10。

**R8 估算偏低导致没地方放** — 回归风险
- `GetReplicas` 把一个 ServingGroup 所有 Pod 的请求加起来当成「一个副本」。按节点规格估算的路径（资源模型、scheduler-estimator）会去找「单个节点放得下整组」的节点，多节点的 group 得到 0，scheduler 报 `Clusters available replicas ... are not enough to schedule`。首次调度时什么都不下发；重调度时被调低的副本哪儿也放不了，连原集群的排队位置都没了。Karmada 维护者在 #8104 里提的就是这一点。
- 证据（静态）：`karmada:pkg/estimator/client/general.go:66-110`；`karmada:pkg/scheduler/core/division_algorithm.go:75-78`。scheduler-estimator 服务端的代码没有读，这部分依据 #8104 的讨论。
- 覆盖：L-01，K-09。

**R9 用过期状态做决定** — 回归风险
- 现有 descheduler 每次都通过估算器实时问成员集群，集群不可达时拿不到数就跳过。改成读 `ResourceBinding` 里的状态以后没有这层检查：成员不可达时 `unschedulableSince` 停在旧值，会一直「超过阈值」。副本被分到别的集群，等原集群恢复，调低的副本数下发下去，删的可能是已经 Running 的 group。
- 证据：推断，依据 `karmada:pkg/descheduler/core/helper.go:62-96` 和草稿第 4 条。
- 覆盖：U-M1，K-15。

### 4.6 滚动更新和恢复

**RO1 模板更新同时到达所有集群** — 回归
- 之前：一个可用性预算。
- 之后：binding controller 在一次循环里更新所有集群的 `Work`，各成员按自己的预算同时滚动。全局同时不可用的数量是各集群预算之和（A3）；`RoleRollingUpdate` 本来就是所有 group 同时更新，现在是所有集群的所有 group。
- 证据（静态）：`karmada:pkg/controllers/binding/common.go:74-153`；`kthena:pkg/apis/workload/v1alpha1/model_serving_types.go:223-224`。
- 覆盖：K-13。

**RO2 跨集群灰度** — 预期变化（新能力）
- `OverridePolicy` 在 `ReviseReplica` 之后应用，可以按集群改镜像或 `partition`；`PropagationPolicy.spec.suspension.dispatchingOnClusters` 可以先暂停向部分集群下发。两者都要用户自己编排，没有全局的「一次只动一个集群」的控制器。`OverridePolicy` 也能覆盖 `spec.replicas`，会破坏各集群之和等于模板值。
- 证据（静态）：`karmada:pkg/controllers/binding/common.go:108-116,147`。
- 覆盖：K-13，K-14。

**RO3 回滚** — 中性
- 把模板改回去，所有集群同时回滚。ControllerRevision 历史在每个集群各一份；被删过又重建的成员没有历史（SC4）。

**RO4 成员集群故障** — 预期变化（新能力，默认不动）
- 默认：约 30 秒后集群标记为 NotReady，execution controller 停止向它同步，状态停在旧值，副本不会迁移。集群故障转移要同时打开 `Failover` 特性门控、`--enable-no-execute-taint-eviction` 并且有 `NoExecute` 污点。
- 打开以后：该集群的副本分给其他集群；优雅驱逐默认最多保留旧 `Work` 10 分钟，或者到新集群上的对象被判为 Healthy 为止。模型冷启动超过 10 分钟时，旧副本（如果还活着）会先被删。
- 证据（静态）：`karmada:pkg/features/features.go:178-179`；`karmada:cmd/controller-manager/app/options/cluster_failover.go:50-51`；`karmada:cmd/controller-manager/app/options/options.go:183,217`；`karmada:pkg/controllers/execution/execution_controller.go:140-144`；`karmada:pkg/controllers/gracefuleviction/evictiontask.go:67-116`。
- 覆盖：K-15。

**RO5 应用故障转移（`failover.application`）** — 回归风险
- 某个集群上的对象被判为 Unhealthy 超过 `tolerationSeconds`，Karmada 把这个集群的全部份额驱逐，包括 Running 的 group。如果 `InterpretHealth` 把「有 group 没就绪」当成不健康，那么滚动更新、模型加载慢、有调度不上的 group 都会触发。它和按 group 粒度工作的 descheduler 是两套机制，谁先到阈值谁先动手。
- 用户要在 `PropagationPolicy` 里显式配置才会生效。
- 证据（静态）：`karmada:pkg/controllers/applicationfailover/rb_application_failover_controller.go:121-162`；`karmada:pkg/controllers/applicationfailover/common.go:109-118`。
- 覆盖：L-06，K-16。

**RO6 `recoveryPolicy`** — 中性
- Pod 失败后的重建完全在成员内完成，和单集群一样。

### 4.7 可用性和性能

**AV1 控制面不可用** — 中性
- 成员集群继续运行：kthena 和 Volcano 都在成员本地，成员上的对象不依赖 Karmada。停掉的是模板下发、状态汇总、重调度、FederatedHPA。成员侧 autoscaler 继续工作。
- 证据：推断（成员侧代码没有任何对 Karmada 的调用）。
- 覆盖：K-17。

**AV2 成员 API server 不可达** — 见 RO4 和 R9。

**AV3 状态更新带来的开销** — 中性
- kthena 只在计数器、条件状态、`unschedulableSince` 变化时写 status；条件的 message 只在状态翻转时改写。
- 成员每写一次 status，Karmada 侧发生：`ReflectStatus` 和 `InterpretHealth` 各跑一次 Lua，往 `Work` 上记 2 条事件；`Work` 状态有变化才更新；`ResourceBinding` 汇总；`AggregateStatus` 跑一次 Lua；模板 status 有变化才更新并记 1 条事件。模板 status 的更新不会触发重新调度。
- 证据（静态）：`kthena:pkg/model-serving-controller/controller/model_serving_controller.go:2886-2945`；`kthena:pkg/model-serving-controller/utils/utils.go:488-494`；`karmada:pkg/controllers/status/work_status_controller.go:239-290,346-351`；`karmada:pkg/util/helper/status.go:52-71`；`karmada:pkg/controllers/status/common.go:186-205`；`karmada:pkg/detector/detector.go:340-343`。
- 覆盖：K-25。

**AV4 Lua 解释器的成本** — 回归风险（未验证）
- 每次调用有 1 秒上限，包含编译脚本和把整个对象转成 Lua table；VM 池大小 10，所有资源类型共用。ModelServing 最多 4 个 role、8 个 Pod 模板，对象可以很大。超时的后果是状态不更新或下发失败。
- 证据（静态）：`karmada:pkg/resourceinterpreter/customized/declarative/luavm/lua.go:74-127`；`karmada:pkg/resourceinterpreter/customized/declarative/configurable.go:49`。
- 覆盖：L-09，K-25。

**AV5 CRD 体积** — 中性（安装注意事项）
- ModelServing CRD 的 YAML 是 1.42 MB，压成 JSON 是 629 KB。客户端 `kubectl apply` 会因为注解超长失败，要用服务端 apply 或 `kubectl create`。用 Karmada 下发 CRD 时每个集群的 `Work` 都带这 629 KB。
- 证据：文件大小是实测值；后果是推断。

### 4.8 升级兼容性

**UP1 成员的 kthena 没有 PR #2079** — 中性（安全降级）
- 成员 status 里没有这两个字段，Lua 里按 0 和空处理，这个集群永远不会被调低。它上面调度不上的 group 不会被搬走，控制面的 `unschedulableReplicas` 只统计了新版本的成员。
- 覆盖：L-04，K-21。

**UP2 控制面的 CRD 比成员旧** — 回归风险
- `AggregateStatus` 写进模板的新字段被 Karmada API server 裁掉。Karmada 比较的是「裁剪后的旧 status」和「Lua 生成的新 status」，永远不相等，于是每次 binding 状态变化都调一次 `UpdateStatus` 并记一条事件。
- 证据（静态）：`karmada:pkg/controllers/status/common.go:192-204`。结论是推断。
- 覆盖：K-21。

**UP3 成员之间 kthena / CRD 版本不一致** — 回归风险
- 旧成员的 API server 会悄悄裁掉不认识的 `spec` 字段，`Work` 仍然是 `Applied=True`，这个集群上新特性不生效但没有任何报错。各版本 webhook 的校验规则不同，同一个模板可能只在部分集群应用成功。
- 证据：推断。
- 覆盖：K-21。

**UP4 Karmada 没有 descheduler 的改动** — 中性
- 现有 descheduler 只处理 Deployment，ModelServing 的 binding 被忽略，不发生重调度，其余功能不受影响。如果先上 kthena 侧 rescheduler、以后又启用通用 descheduler，会有两个组件同时改同一个 `ResourceBinding`。
- 证据（静态）：`karmada:pkg/descheduler/core/filter.go:31-50`。

**UP5 没装 `ResourceInterpreterCustomization`** — 回归
- 没有 `GetReplicas` 时 Karmada 把 ModelServing 当成非工作负载：下发到所有候选集群，不调 `ReviseReplica`，每个集群都是模板里的完整 `spec.replicas`，哪怕策略写的是 `Divided`。先建模板和策略、后装解释器的顺序会多占 N 倍资源。装上以后已有 binding 会不会自动重算，没有确认。
- 另外，用户自己的 `ResourceInterpreterCustomization` 优先于 Karmada 内置的同类定制。如果 Karmada 以后内置了 ModelServing 的解释器，而 kthena 文档里又给了一份，两份会各自演进。
- 证据（静态）：`karmada:pkg/detector/detector.go:1515-1536`；`karmada:pkg/scheduler/core/common.go:68-82`；`karmada:pkg/apis/work/v1alpha2/binding_types_helper.go:219-224`；`karmada:pkg/resourceinterpreter/interpreter.go:339-366`。
- 覆盖：K-20。

**UP6 打开 `MultiplePodTemplatesScheduling`** — 中性（但有一个陷阱）
- 按草稿（不实现 `componentResource`）打开这个门控没有影响，仍然走 `GetReplicas`。
- 陷阱：如果以后给 ModelServing 加了 `componentResource`，门控打开时 Karmada 改走 `GetComponents`，不再调 `GetReplicas`。多组件的工作负载不拆副本，下发到所有候选集群，调度结果里副本数是 0；而 `ReviseReplica` 仍然会被调用，成员的 `spec.replicas` 被写成 0。Karmada 内置的定制里没有一个同时实现这两个 hook。
- 证据（静态）：`karmada:pkg/detector/detector.go:1502-1513`；`karmada:pkg/scheduler/core/common.go:59-82`；`karmada:pkg/controllers/binding/common.go:86-96`；`karmada:pkg/features/features.go:192`。「副本被写成 0」是推断，需 Kind。
- 覆盖：K-22，U-M5。

**UP7 把已有的单集群 ModelServing 纳入 Karmada** — 回归风险
- 默认 `conflictResolution: Abort`：成员上已有同名对象且不是 Karmada 管理的，`Work` 报 `already exists ... Karmada will not manage this resource`，不动现有对象。
- `Overwrite`：对现有对象做更新，UID 不变，Pod 的 ownerReference 仍然有效。模板内容一致时 revision 不变，Pod 不重启。但 `spec.replicas` 会被改成分给这个集群的数，比原来少就会删 Running 的 group（同级里序号大的先删）。
- `Aggregated` 首次调度不知道哪个集群已经有这个工作负载，可能把所有副本放到别的集群；原集群不在结果里，上面的对象没人管，继续运行，结果是双份。
- 证据（静态）：`karmada:pkg/util/objectwatcher/objectwatcher.go:149-155,188,307-324`；`karmada:pkg/apis/policy/v1alpha1/propagation_types.go:146-157`；`kthena:pkg/model-serving-controller/utils/revision_data.go:185-222`。最后一条是推断。
- 覆盖：K-18。

**UP8 退出 Karmada 管理** — 预期变化（有一个陷阱）
- 删模板：默认连成员上的对象一起删。`preserveResourcesOnDeletion: true` 时成员对象保留，Karmada 的 label 和 annotation 被清掉，Pod 不受影响。
- 只删 `PropagationPolicy`：`ResourceBinding` 和成员对象都保留，直到模板被删。
- 陷阱：`preserveResourcesOnDeletion` 对「集群从调度结果里被移除」同样生效。`Divided` 加重调度时，某个集群降到 0 以后，上面的 ModelServing 带着最后一次的副本数继续运行，没人管，而这些副本已经分给了别的集群。以后副本再分回这个集群，又会因为默认的 `Abort` 报「已存在」。
- 证据（静态）：`karmada:pkg/controllers/execution/execution_controller.go:205-266`；`karmada:pkg/detector/detector.go:1164-1168`；`karmada:pkg/apis/policy/v1alpha1/propagation_types.go:185-202`。
- 覆盖：K-19。

**UP9 成员没装 Volcano 或 PodGroup CRD** — 回归风险
- `schedulerName` 默认是 `volcano`。成员没有 Volcano 时没人给 Pod 写 `PodScheduled` 条件，Pod 一直 Pending 但不是 `Unschedulable`，`unschedulableReplicas` 是 0，永远不会被搬走。没有 PodGroup CRD 时不做 gang 调度，group 的一部分 Pod 会先占住 GPU。
- 证据（静态）：`kthena:pkg/model-serving-controller/podgroupmanager/manager.go:272-280`；`kthena:pkg/model-serving-controller/utils/utils.go:365-388`。
- 覆盖：K-23。

### 4.9 相关的 kthena 资源

**REL1 ModelServer / ModelRoute / kthena-router** — 回归风险
- `ModelServer.workloadSelector` 只选 router 所在集群、同一 namespace 的 Pod。副本分散以后，每个有副本的集群都要有 kthena-router、ModelServer、ModelRoute（按 `Duplicated` 下发到同一组集群），跨集群的流量入口要在 Karmada 之外解决。
- `InterpretDependency` 只看 Pod 模板引用的 ConfigMap、Secret、ServiceAccount、PVC，不会带上这些路由资源。
- 某个集群副本数为 0 时路由对象还在，router 没有后端。前面的全局负载均衡如果不做健康检查，这部分流量会失败。
- 证据（静态）：`kthena:pkg/apis/networking/v1alpha1/modelserver_types.go:34-43`；`karmada:pkg/util/helper/pod.go:85-120`。
- 覆盖：L-08，K-23。

**REL2 AutoscalingPolicy** — 预期变化
- 只能在成员里跑（SC8）。选了控制面伸缩就不能再把它下发到成员。它在成员上有 mutating webhook 补 `spec.behavior` 默认值；如果 CRD schema 没有填同样的默认值，Karmada 每次同步都会发一次不产生变化的更新（不是死循环，未验证）。
- 证据（静态）：`kthena:pkg/autoscaler/webhook/autoscalingpolicy_mutator.go:81-161`；`karmada:pkg/util/objectwatcher/objectwatcher.go:188-203`。
- 覆盖：K-05，K-06。

**REL3 ModelBooster** — 回归风险
- ModelBooster 控制器在本集群创建并更新子 ModelServing。下发 ModelBooster 时每个集群各自生成完整副本数的 ModelServing，不拆副本，也不归 ModelServing 的解释器和重调度管。同时下发 ModelBooster 和它生成的同名 ModelServing 会冲突：默认 `Abort` 报已存在；`Overwrite` 则是两个控制器写同一个对象。
- 证据（静态）：`kthena:pkg/model-booster-controller/controller/model_serving_handler.go:33-97`。

**REL4 Volcano Queue、PriorityClass、网络拓扑（HyperNode）** — 回归风险
- 都是集群级、每个集群各不相同的对象，不会自动跟过去。`networkTopology` 的层级在不同集群含义不同，在没有对应拓扑的集群里更容易调度不上，进而触发更多重调度。
- 证据：推断（A10 的静态事实加集群差异）。

**REL5 PVC、Secret、ConfigMap、ServiceAccount** — 预期变化
- 通过 `InterpretDependency` 和默认开启的 `PropagateDeps` 跟着 ModelServing 走，生命周期也跟着调度结果走（SC4）。

## 5. 预期行为变化

| 编号 | 变化 | 用户看到的 | 来源 |
| --- | --- | --- | --- |
| IC-01 | 编写对象变多 | 控制面写模板、`PropagationPolicy`、可选的 `OverridePolicy`；管理员装解释器；CRD 装两处 | A1 |
| IC-02 | `spec.replicas` 的含义 | `Divided` 是全局总数，`Duplicated` 是每个集群的数 | A2 |
| IC-03 | 成员对象的 `spec.replicas` 归 Karmada 写 | 在成员上手改副本数会被改回去，除非配置了保留 | A2，SC6 |
| IC-04 | 控制面 `status` 是汇总值 | 计数器求和；`Duplicated` 时大于 `spec.replicas`；明细看 `ResourceBinding` 或 `karmadactl get` | S1，S2 |
| IC-05 | 调度不上的 ServingGroup 会换集群 | 超过阈值后被删，在别的集群重建，不再原地排队 | R1，R4 |
| IC-06 | 阈值按最老的 group 算 | 同一集群里刚出现的调度不上的 group 会跟着一起搬 | R4 |
| IC-07 | 模板更新同时到所有集群 | 各集群按自己的预算同时滚动；灰度靠 `OverridePolicy` 或暂停下发 | RO1，RO2 |
| IC-08 | 自动伸缩二选一 | 成员侧（kthena autoscaler / HPA，加保留）或控制面（FederatedHPA）；kthena `AutoscalingPolicy` 不能做全局伸缩 | SC6，SC8 |
| IC-09 | 集群故障可以迁移副本 | 默认不迁移，按 Karmada 的开关启用 | RO4 |
| IC-10 | 依赖对象跟着走 | Pod 模板引用的 ConfigMap、Secret、ServiceAccount、PVC 自动下发，也自动移除 | REL5，SC4 |

## 6. 回归风险

严重度：高 = 可能中断服务、成倍占用资源或让对象卡住不动；中 = 行为和单集群不同，需要人工介入或明确写进文档；低 = 可观测性或体验。可能性一栏写的是触发条件。

| 编号 | 风险 | 严重度 | 可能性 | 缓解 | 证据 | 覆盖 |
| --- | --- | --- | --- | --- | --- | --- |
| RR-01 | 和副本数挂钩的校验拒绝被拆开的副本，也拦住之后的副本调整（V3） | 高 | 高：用了整数 `maxUnavailable` / `partition` / `minAvailable`，或百分比加小集群；默认配置不触发 | 文档限定可用的写法；或由用户决定是否放宽 kthena 的校验（改既有行为，需单独批准）；或用 `OverridePolicy` 按集群覆盖 | kthena 侧静态；端到端需 Kind | U-K1，K-03 |
| RR-02 | 各集群同时滚动，全局不可用数是各集群预算之和（A3，RO1） | 高 | 高：任何多集群滚动 | 文档；用暂停下发或 `OverridePolicy` 分批；推荐百分比加 `maxSurge` | 静态 | K-13 |
| RR-03 | 没装解释器时 `Divided` 变成每个集群完整副本（UP5） | 高 | 中：安装顺序错或漏装 | 安装文档强调顺序；Kind 确认装上后是否自动收敛 | 静态；收敛行为需 Kind | K-20 |
| RR-04 | `DisaggregatedTarget` 改的 role 副本数被 Karmada 反复改回（V6，SC7） | 高 | 中：成员侧用了 `DisaggregatedTarget` | `Retain` 同时保留 role 的 `replicas`，或文档禁止这种组合 | 静态加推断；需 Kind | L-07，K-06 |
| RR-05 | `failover.application` 把整个集群的份额驱逐（RO5） | 高 | 低到中：用户显式开启 | `InterpretHealth` 的定义要容忍滚动和加载；文档说明 `tolerationSeconds` 必须远大于冷启动时间加重调度阈值，或不建议启用 | 静态；`InterpretHealth` 定义未定 | L-06，K-16 |
| RR-06 | 观察和执行之间的竞态删到 Running 的 group（R2） | 高 | 低：窗口是巡检周期加下发延迟 | 写 binding 前重新确认状态；文档说明这是尽力而为 | 推断 | U-M1，K-09 |
| RR-07 | `componentResource` 加门控把成员副本数写成 0（UP6） | 高 | 低：需要以后有人加 `componentResource` 并打开 alpha 门控 | 设计里写明不实现 `componentResource`；加一条回归用例 | 静态加推断；需 Kind | U-M5，K-22 |
| RR-08 | 部分在服务的 group 被当成调度不上整组搬走（R3a） | 中 | 中：role 扩容、Pod 重建时集群正好没资源 | 由用户决定是否只统计从未就绪过的 group（改 #2079 的行为，需单独批准）；或文档说明 | 静态 | U-K3，K-11 |
| RR-09 | 来回搬（R5） | 中 | 高：`Aggregated` 加通用估算器；或对静态权重、`Duplicated` 也做重调度 | 只对动态拆分启用；给刚被调低的集群加冷却或排除 | 静态 | U-M2，U-M4，K-10 |
| RR-10 | 集中搬迁把下一个集群打满（R7） | 中 | 中：一个集群资源耗尽时 | 限速；评估 `SchedulingOvercommitProtection` | 静态加推断 | K-10 |
| RR-11 | 成员侧伸缩加保留，使重调度不生效并多建副本（SC6） | 中 | 中：两者同时启用 | 文档写明两者互斥；或 rescheduler 跳过带保留 label 的对象 | 推断 | L-07，K-06 |
| RR-12 | `partition` 按集群生效；副本进入新集群时全部是新版本（A4，SC5） | 中 | 中：用 `partition` 做金丝雀 | 文档；金丝雀改用 `OverridePolicy` 按集群做 | 静态 | U-K4，K-14 |
| RR-13 | 集群降到 0 时删除整个 ModelServing、历史和依赖的 PVC（SC4） | 中 | 中：`Aggregated` 缩容或重调度 | `--enable-empty-workload-propagation`；PVC 独立下发 | 静态 | K-08 |
| RR-14 | 跨集群缩容不看就绪情况（SC2） | 中 | 中：有集群部分未就绪时缩容 | 文档；跟进 PR #7662 | 静态 | K-07 |
| RR-15 | 校验和不可变字段的错误变成异步，失败集群的状态从汇总里消失；kthena 的事件只在成员集群（V2，V4，V5，S8） | 中 | 高：任何非法 spec 或不可变字段修改 | 文档给出排查路径；可选：把 kthena 校验 webhook 也注册到 Karmada API server | 静态 | K-02，K-04 |
| RR-16 | 控制面状态缺字段、`observedGeneration` 和 `conditions` 合并未定义（S1，S3，S4） | 中 | 高：按草稿实现就会出现 | 见第 9 节 | 静态 | L-04，L-05 |
| RR-17 | Lua 里的空值和缺失字段：`unschedulableSince` 空字符串、`availableReplicas` 缺失、`status` 为空（S5，S6，S7） | 中 | 中：边界状态（0 个可用、刚创建） | Lua testdata 覆盖 | 静态 | L-04，L-05，L-06 |
| RR-18 | 估算偏低导致不下发或被调低的副本没处放（R8） | 中 | 中：group 跨多个节点且启用了按节点规格的估算 | 设计里写明估算器配置；需要时由用户决定是否用其他估算方式 | 静态；scheduler-estimator 部分未读 | L-01，K-09 |
| RR-19 | 滚动时 surge group 触发跨集群搬迁（R6） | 中 | 中：`maxSurge > 0` 且集群接近满 | 由用户决定 surge group 是否计入；或文档说明 | 静态加推断；需 Kind | U-K5，K-12 |
| RR-20 | 接管已有对象时删 Running 的 group 或出现双份（UP7） | 中 | 中：迁移场景 | 迁移文档：先用 `Duplicated` 或固定集群接管，再改策略 | 静态加推断 | K-18 |
| RR-21 | 用过期状态调低不可达集群的副本（R9） | 中 | 低：成员不可达超过阈值 | rescheduler 跳过 NotReady 的集群和过期状态 | 推断 | U-M1，K-15 |
| RR-22 | `preserveResourcesOnDeletion` 留下没人管的 ModelServing（UP8） | 中 | 低：该选项和 `Divided` 重调度一起用 | 文档 | 静态 | K-19 |
| RR-23 | 路由资源、Queue 等没跟过去；0 副本集群的 router 没有后端（REL1，REL4，A10） | 中 | 中 | 文档给出要一起下发的对象清单 | 静态加推断 | K-23 |
| RR-24 | 成员没装 Volcano 时调度不上的 group 不被发现（UP9） | 中 | 低：集群配置不一致 | 用集群亲和性排除；文档 | 静态 | K-23 |
| RR-25 | 版本不一致时字段被悄悄裁掉、控制面重复更新状态（UP2，UP3） | 低到中 | 中：滚动升级期间 | 升级顺序：先控制面 CRD，再成员；成员全部升完之前不用新字段 | 推断 | K-21 |
| RR-26 | Lua 超时或往返改坏 Pod 模板（V7，AV4） | 中 | 低 | testdata 加基准；改坏时成员会拒绝 | 静态；耗时未测 | L-03，L-09 |
| RR-27 | `evictionStrategy` 的预算按集群计算（A7） | 低到中 | 低：用了这个字段 | 文档 | 静态 | U-K1 |

## 7. 针对性回归覆盖

### 7.1 单元测试

kthena（放进现有测试文件，表驱动）：

- **U-K1** `pkg/model-serving-controller/webhook/validator_test.go`：用「成员分到的副本数」做输入，记录现有校验的结果——整数 `maxUnavailable` / `partition` 大于副本数、`25%` 配 3 个副本且无 `maxSurge`、`evictionStrategy.minAvailable` 大于副本数、默认配置配 1 个副本。这是对现状的刻画；如果用户批准放宽校验，再改期望值。
- **U-K2** `binpack_scaledown` 相关测试：要删的数量大于调度不上的 group 数时，顺序是调度不上 → 其他未就绪 → Running；Running 的 group 不会因为有 Pod 调度不上而提前。
- **U-K3** status 计算：原本 Running 的 group 进入 `Scaling` 或 `Creating` 后有一个 Pod 调度不上，`unschedulableReplicas` 和缩容优先级各是多少（先刻画现状，R3a 的取舍由用户定）。
- **U-K4** `scaleUpServingGroups`：`partition > 0` 且 `status.currentRevision` 为空的新对象，各序号用哪个 revision（刻画 SC5）。
- **U-K5** status 计算：滚动期间调度不上的 surge group 是否计入 `unschedulableReplicas`（刻画 R6）。

Karmada（descheduler 改动落地时）：

- **U-M1** `pkg/descheduler/core/helper_test.go`：从状态取数——阈值未到和已到；两个字段缺失（旧成员）；`readyReplicas` 缺失；`unschedulableReplicas` 大于分配数；目标不低于就绪数；`applied=false`（没有 `status`）的集群；集群 NotReady 时跳过。
- **U-M2** `pkg/descheduler/core/filter_test.go`：ModelServing 被纳入；静态权重和 `Duplicated` 仍被排除。
- **U-M3** `pkg/descheduler/descheduler_test.go`：`updateScheduleResult` 的目标副本数表（含降到 0）。
- **U-M4** `pkg/scheduler/core` 的分配测试：一个集群被调低后，`Aggregated` 和动态权重各把增量分给谁（估算器报告该集群仍有空位时）。刻画 R5。
- **U-M5** `pkg/controllers/binding` 的 `ensureWork` 测试：同时有 `componentResource` 和 `ReviseReplica`、门控打开时，成员对象的副本数是多少。刻画 UP6。

### 7.2 Lua 解释器 testdata

目录按 Karmada 的约定：`pkg/resourceinterpreter/default/thirdparty/resourcecustomizations/workload.serving.volcano.sh/v1alpha1/ModelServing/testdata/`。同一批输入也可以用 `karmadactl interpret -f <customization> --operation ...` 在 kthena 仓库里离线跑。

- **L-01** `interpretreplica-test.yaml`：两个 role（`replicas` 不同、带 `workerReplicas`、GPU 资源）；没有 `workerTemplate` 的 role；`workerReplicas: 0`；有 initContainer。期望 `replica = spec.replicas`，资源是一个 ServingGroup 的总和；各 role 的 `nodeSelector` / 容忍不同时取哪个要有明确期望。
- **L-02** `interpretreplica-test.yaml`：`spec.replicas: 0`；负数（期望行为要先定）。
- **L-03** `revisereplica-test.yaml`：只有 `spec.replicas` 变化；输入里带 `emptyDir: {}`、`securityContext: {}`、`resources: {}`、空的 `args: []`、百分比形式的 `maxUnavailable`、Karmada 注入的注解；目标副本数为 0。输出除 `spec.replicas` 外逐字段相等。
- **L-04** `interpretstatus-test.yaml`：完整 status；`availableReplicas` 缺失时输出 `readyReplicas: 0`；没有 `status` 时输出空对象且不报错；输出里有成员 `generation` 和 `resourceTemplateGeneration`；两个新字段缺失（旧成员）；`labelSelector`、`currentReplicas`、revision 字段照传。
- **L-05** `aggregatestatus-test.yaml`：两个集群求和；`unschedulableSince` 取最小；都没有时字段不存在（不是空字符串）；某一项没有 `status`（`applied=false`）；`statusItems` 为空时计数器清零；只有全部成员都观察到最新模板时才推进 `observedGeneration`；条件合并的期望；`Duplicated` 的求和；输出里没有 `readyReplicas`。
- **L-06** `interprethealth-test.yaml`：全部就绪；有调度不上的 group；滚动中；`status` 为空或字段缺失（不能报 Lua 错误）；`spec.replicas: 0`。每一条的期望值要和 RO5 的结论一起定。
- **L-07** `retention-test.yaml`：带 `resourcetemplate.karmada.io/retain-replicas=true` 时保留成员的 `spec.replicas`，不带时用期望值；role 的 `replicas` 是否保留（按 RR-04 的决定）；其他字段一律用期望值；往返保真同 L-03。
- **L-08** `interpretdependency-test.yaml`：多个 role 的 entry 和 worker 模板引用的 ConfigMap、Secret、ServiceAccount、PVC 取并集并去重；没有依赖时返回空。
- **L-09** 4 个 role、每个模板几十个环境变量的大对象：每个操作的耗时远小于 1 秒（用 Go benchmark 包住 `luavm`）。

### 7.3 Kind 场景

环境：一个 Karmada 控制面加两到三个成员集群，成员里装 kthena 和 Volcano；用扩展资源或节点污点模拟 GPU 不足。每个场景都要记录镜像 tag、命令、YAML 和观察到的状态。

- **K-01 基线**：`Divided` 加动态拆分，`replicas: 6`。期望：成员 `spec.replicas` 之和为 6；Pod 只在成员里；控制面 `kubectl get modelserving` 三列是汇总值；`observedGeneration` 等于模板 generation；控制面 scale 子资源可读。
- **K-02 校验错误的位置**：role 名用大写，或 Pod 模板缺容器名。期望：控制面 apply 成功；`Work` 是 `Applied=False` 且 message 带 webhook 拒绝信息；`ResourceBinding` 是 `FullyApplied=False`；模板上有 `SyncFailed` 事件；改正后恢复。另测 `spec.replicas: -1`。
- **K-03 和副本数挂钩的校验**：`replicas: 10`、`maxUnavailable: 3`、静态权重 8:2；再测 `25%`、不设 `maxSurge`、某集群 3 个副本。期望记录哪个集群被拒、其余集群是否正常、之后调整副本数是否也被拒。
- **K-04 不可变字段**：在控制面加一个 role。期望：所有 `Work` 失败；成员上的 Pod 不变；控制面汇总的计数器怎么变（V5）。
- **K-05 没有重复下发**：稳定运行 10 分钟并让 Pod 就绪状态翻转几次。期望：成员 ModelServing 的 `metadata.generation` 不变；execution controller 没有因状态变化而更新对象。另外下发一个 `AutoscalingPolicy`，数它被更新的次数。
- **K-06 成员侧自动伸缩**：`HomogeneousTarget` 带和不带保留 label；`DisaggregatedTarget`。期望记录副本数是否来回跳、Pod 是否反复创建删除；再叠加重调度看 SC6 的冲突。
- **K-07 跨集群缩容**：集群 A 全部 Running，集群 B 全部调度不上，从控制面把副本数减半。记录每个集群减了多少、成员内删的是哪些 group。
- **K-08 集群降到 0**：记录成员 ModelServing、ControllerRevision、依赖下发的 PVC 是否被删；打开 `--enable-empty-workload-propagation` 后重做。
- **K-09 重调度主路径**：集群 A 资源不足，阈值设 1 分钟。期望：A 被调低的数量等于调度不上的 group 数，不低于可用数；被删的是调度不上的 group，Running 的 Pod UID 不变；B 上建出新 group。再测估算器认为没有集群放得下时 `ResourceBinding` 的状态。
- **K-10 来回搬和集中搬迁**：`Aggregated`、只用通用估算器；多个 ModelServing 同时在 A 上调度不上。记录 `ResourceBinding` 事件里搬迁的轮数和落点。
- **K-11 部分在服务的 group**：一个 Running 的 group，增加某个 role 的副本数，或在节点被封锁后删一个 Pod，让新 Pod 调度不上。记录 `unschedulableReplicas`，以及超过阈值后整组是否被删。
- **K-12 滚动加 surge**：`maxSurge: 1`、`maxUnavailable: 0`，集群已满，descheduler 开着。记录 surge group 是否被计入、份额是否被调低、原集群的滚动最后停在什么状态。
- **K-13 同时滚动**：改镜像，全程记录各集群 `availableReplicas` 之和的最小值；再用暂停下发分批重做。
- **K-14 `partition` 金丝雀**：整数和百分比各一次，记录每个集群新旧版本的 group 数；金丝雀期间让副本进入一个新集群，看它们是哪个版本。
- **K-15 成员不可达**：暂停一个成员的控制面节点。默认配置下记录状态是否过期、副本是否迁移、rescheduler 是否对它做决定；打开集群故障转移后记录迁移时间和旧副本保留多久。
- **K-16 应用故障转移**：配置 `failover.application`，做一次慢滚动（就绪探针延迟）。记录是否触发整集群驱逐。
- **K-17 控制面不可用**：停掉 karmada-apiserver，在成员里删一个 Pod。期望 kthena 照常重建；成员侧 autoscaler 照常工作；恢复后状态重新汇总。
- **K-18 接管已有对象**：成员里先有 4 个 Running group。分别用 `Abort`、`Overwrite` 加 `Duplicated`、`Overwrite` 加 `Divided`。记录报错信息、Pod UID 是否变化、哪些 group 被删。
- **K-19 退出管理**：`preserveResourcesOnDeletion: true` 后删模板，期望成员对象和 Pod 不变、Karmada 的 label 被清掉；再测 `Divided` 下某集群降到 0 时是否留下没人管的对象。
- **K-20 解释器安装顺序**：先建模板和 `Divided` 策略，再装解释器。记录装之前每个集群的副本数，以及装之后是否自动收敛。
- **K-21 版本不一致**：一个成员用 #2079 之前的 kthena 和 CRD；另测控制面 CRD 比成员旧。记录汇总值、rescheduler 是否忽略旧成员、控制面是否反复更新 status。
- **K-22 特性门控**：打开 `MultiplePodTemplatesScheduling`，解释器按草稿实现。期望行为和 K-01 一致。
- **K-23 相关资源**：ModelServer、ModelRoute 按 `Duplicated` 下发；从每个集群的 router 发请求；副本数为 0 的集群返回什么。某个成员缺少 Volcano Queue，或没装 Volcano，记录 PodGroup 状态和 `unschedulableReplicas`。
- **K-24 FederatedHPA**：以模板为目标，按 entry Pod 的指标伸缩 `spec.replicas`。
- **K-25 状态开销**：从 0 扩到 20 个 group。统计 `Work` 状态更新次数、`ResourceBinding` 更新次数、事件数，以及 Lua 调用的耗时。

## 8. 未验证风险

下面这些读代码确认不了，在跑过对应的 Kind 场景之前不能当作结论。

1. **通用 descheduler 和 kthena 侧 rescheduler 都不存在。** 第 4.5 节按现有 descheduler 的代码加草稿描述推出，实际实现可能不同。
2. **scheduler-estimator 服务端没有读。** R8 中「按节点判断整组能不能放下」依据的是通用估算器的代码和 #8104 里维护者的说法。
3. **装上解释器之后已有 binding 是否自动重算**（UP5）。
4. **负数 `spec.replicas` 在 Karmada scheduler 里的行为**（V8）。
5. **`componentResource` 加门控把副本写成 0**（UP6）：调用链是读出来的，没有执行。
6. **surge group 被调低以后原集群滚动的最终状态**（R6）。
7. **`Aggregated` 接管已有对象时会不会选别的集群**（UP7）。
8. **Lua 往返对真实 Pod 模板是否完全保真，以及大对象的耗时**（V7，AV4）。
9. **`AutoscalingPolicy` 和 ModelBooster 的 mutating webhook 是否导致每次同步都多一次更新**（REL2）。
10. **集群间时钟偏差对阈值的影响**（S5）。
11. **PVC 作为依赖被移除的时机**：有 `pvc-protection` finalizer，要等 Pod 退出；回收策略不同，后果不同（SC4）。
12. **Pull 模式（`karmada-agent`）**：分析走的是控制面里的控制器代码路径，agent 里的同名控制器没有单独核对。
13. **FederatedHPA 对 ModelServing 的实际效果**：只读了取 scale 和 Pod 的部分，指标计算没有核对（SC8）。
14. **通过 Karmada 下发 629 KB 的 CRD 是否有问题**（AV5）。
15. **跨集群同名 group 对 P/D 配对的影响**：当前 router 只看本集群，这是对以后跨集群路由的提醒（A5）。

## 9. 建议方案补上的内容

这些是分析得出的建议，是否采纳由方案和用户决定；涉及改 kthena 既有行为的（标了「需批准」）不在本任务当前阶段的范围内。

1. `ReflectStatus` 加上 `labelSelector`、`currentReplicas`、`currentRevision`、`updateRevision`、成员 `generation` 和 `resourceTemplateGeneration`；`AggregateStatus` 按 Karmada 对 Deployment / CloneSet 的做法处理 `observedGeneration`，并写明 `conditions` 的合并规则。`readyReplicas` 只留在 `ResourceBinding` 里。
2. `Retain` 按 `resourcetemplate.karmada.io/retain-replicas` label 决定是否保留 `spec.replicas`，并明确 role 的 `replicas` 怎么办（保留，或禁止 `DisaggregatedTarget` 和 Karmada 一起用）。
3. 写明「成员侧自动伸缩」和「重调度」互斥，rescheduler 跳过带保留 label 的对象。
4. 写明只对动态拆分（`Aggregated`、动态权重）做重调度；跳过 NotReady 的集群和 `applied=false` 的项；给刚被调低的集群设冷却。
5. 写明不实现 `componentResource`，并说明原因。
6. 给出 `rolloutStrategy` 在多集群下的推荐写法（百分比、`maxSurge >= 1`），列出会被成员拒绝的写法；`partition` 金丝雀改用 `OverridePolicy` 按集群做。是否放宽成员侧校验需批准。
7. 写明 `InterpretHealth` 的定义，以及不建议和 `failover.application` 一起用（或给出 `tolerationSeconds` 的下限）。
8. 安装和运维文档：解释器先于模板安装；CRD 先升级控制面再升级成员，成员全部升完之前不用新字段；`--enable-empty-workload-propagation` 和 PVC 独立下发；要一起下发的对象清单（ModelServer、ModelRoute、Queue、PriorityClass）；校验失败的排查路径；接管和退出的步骤。
9. R3a（部分在服务的 group）和 R6（surge group）是否计入 `unschedulableReplicas`，由用户决定。改动属于 #2079 的行为变化，需批准。
