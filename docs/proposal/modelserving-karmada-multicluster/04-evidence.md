# 刻画和证据

- 作者：@VanderChen，2026-11-28
- 内容：[01-design.md](01-design.md) 和 [02-servinggroup-cr.md](02-servinggroup-cr.md) 的结论各自依据什么、怎么得到的、还缺什么。
- 基线：Karmada `6d007132`（master，`v1.20.0-beta.0-44`）；kthena `1a5145e9`（main，含 PR #2079）。
  路径记作 `karmada:pkg/...`、`kthena:pkg/...`。
- 没有改 Karmada 和 kthena 的任何实现代码。刻画用的脚本、YAML、探针和日志在 [`artifacts/`](artifacts/)，重跑方法见 [artifacts/README.md](artifacts/README.md)。
- 局限：Kind 的成员集群没有装 Volcano，ModelServing 用的是 `schedulerName: default-scheduler`；GPU 是打在节点 status 上的扩展资源，Pod 是 pause 镜像。
  Volcano gang 调度下的行为都还没有验证。

证据分四类，可信度从低到高：读源码；包级实测（把临时测试放进 Karmada 的真实包里运行）；解释器离线实测（`karmadactl interpret`）；Kind。

## 1. 读源码得到的现状

| 事实 | 位置（Karmada `6d007132` / kthena `1a5145e9`） |
| --- | --- |
| descheduler 的 GVK 白名单只有 `apps/v1 Deployment`，带 `TODO: make it as an option` | `karmada:pkg/descheduler/core/filter.go:30-33` |
| descheduler 从聚合状态按固定键 `readyReplicas` 读就绪数；ModelServing 的状态里没有这个键 | `karmada:pkg/descheduler/core/helper.go:126-148`；`kthena:pkg/apis/workload/v1alpha1/model_serving_types.go:439-486` |
| 不可调度数来自估算器；服务端只认 Deployment | `karmada:pkg/estimator/server/replica/replica.go:42-68` |
| descheduler 只降副本，不决定去向；默认阈值 5 分钟、间隔 2 分钟 | `karmada:pkg/descheduler/descheduler.go:211-250`；`karmada:cmd/descheduler/app/options/options.go:31-35` |
| Divided 下只有各集群副本之和不等于 `spec.replicas` 时才重新调度 | `karmada:pkg/util/binding.go:55-66`；`karmada:pkg/scheduler/scheduler.go:421-458` |
| 调度失败的绑定在不可调度队列里最多停 5 分钟，每 30 秒清一次 | `karmada:pkg/scheduler/internal/queue/scheduling_queue.go:40-45,188,280-290` |
| 集群汇总把没有绑定节点的 Pod 的请求计入 `allocating` | `karmada:pkg/controllers/status/cluster_status_controller.go:588-599` |
| 多模板估算只在 spread constraint 把集群数限定为 1 时使用 | `karmada:pkg/scheduler/core/estimation.go:43-65` |
| 特性门控开启且实现了 `InterpretComponent` 时不再调用 `GetReplicas` | `karmada:pkg/detector/detector.go:1503-1512` |
| 资源模型路径返回 0 时直接采用，不回退；回退分支在 `804b08c81`（v1.18.0）里被删 | `karmada:pkg/estimator/client/general.go:89-98`；`git show 804b08c81` |
| 集群创建时填默认资源模型（只有 cpu、memory），更新时不再填 | `karmada:pkg/registry/cluster/strategy.go:97-121` |
| ModelServing 只有校验 webhook；默认值来自 CRD schema | `kthena:pkg/model-serving-controller/webhook/validator.go`；`kthena:pkg/apis/workload/v1alpha1/model_serving_types.go:41-77` |
| 缩容顺序：调度不上 → 其他未就绪 → Running | `kthena:pkg/model-serving-controller/controller/binpack_scaledown.go:39-44,161-201` |
| 版本哈希不含 `spec.replicas` 和 role 的 `replicas` | `kthena:pkg/model-serving-controller/utils/revision_data.go:47-66` |
| 校验按成员对象上的 `spec.replicas` 判断整数 `maxUnavailable`、`partition`、`minAvailable` | `kthena:pkg/model-serving-controller/webhook/validator.go:228-259,565-600` |

更完整的逐段追踪在 [appendix/round1-proposal-analysis.md](appendix/round1-proposal-analysis.md) 和 [appendix/round1-behavior-regression.md](appendix/round1-behavior-regression.md)。

## 2. 包级实测

方法：把临时 `_test.go` 放进 Karmada 的真实包，`go test -run ... -count=1`，跑完删除。Go 1.27.3，Karmada `6d007132`。
这是包级别的调用，没有经过 gRPC、informer 和真实集群。

第一组（源文件在 [`artifacts/estimator-probe/`](artifacts/estimator-probe/)，输出在 [`artifacts/logs/estimator-probe.log`](artifacts/logs/estimator-probe.log)）：

| 测试 | 包 | 结论 |
| --- | --- | --- |
| `TestK014ProbeAccurateEstimator` | `pkg/estimator/server` | 节点级估算：整组求和在碎片集群上把真实容量 1 算成 0；最大 Pod 把 1 算成 4、把 2 算成 8；组件集 4 个场景全对 |
| `TestK014ProbeGeneralEstimator` | `pkg/estimator/client` | 集群汇总估算：整组求和只在「组内有放不进单节点的大 Pod」时高估；最大 Pod 同样成倍高估 |
| `TestK014ProbeG1DefaultResourceModels` | `pkg/controllers/status` | 用 Karmada 真实的默认 9 档资源模型：4 节点 × 4 GPU 全空闲，1 GPU 的请求可用副本数为 0，不带 GPU 的对照请求为 16，清空资源模型后为 16 |

第二组（`go test -overlay`，原始输出在 [appendix/round1-proposal-analysis.md](appendix/round1-proposal-analysis.md) 的附录）：调度器补副本的 5 个用例、`ensureWork` 的 3 个用例、
8 Pod × 1 GPU 场景下 7 种估算器配置的对照。其中「副本被原样还给原集群」这一条在 Kind 上没有出现，见第 4 节的 C5。

## 3. 解释器离线实测

- 草稿：[`artifacts/interpreter/modelserving-customization.yaml`](artifacts/interpreter/modelserving-customization.yaml)。
- 命令：`artifacts/interpreter/run-offline.sh`，用从 Karmada `6d007132` 构建的 `karmadactl interpret` 执行；输入在 `artifacts/interpreter/testdata/`。
- 输出：[`artifacts/logs/interpreter-offline.log`](artifacts/logs/interpreter-offline.log)。11 次调用全部 `exit 0`。

| 用例 | 结果 |
| --- | --- |
| `--check` | 7 个规则 PASS，`InterpretComponent` 为 UNSET（有意不实现） |
| `InterpretReplica`，2 个 role、每组 9 个 Pod | 副本数 5；需求 `cpu: 17`、`memory: 144Gi`、`nvidia.com/gpu: 29`，和手算一致；不带 `nodeClaim` |
| `InterpretReplica`，Pod 没写资源请求 | 副本数 2，需求为空 |
| `Retain`，带保留标签 | 保留成员的 `spec.replicas: 9` 和 decode role 的 `replicas: 4`；`securityContext: {}`、`emptyDir: {}` 保持原样 |
| `InterpretStatus` | 整份状态加 `readyReplicas`、`desiredReplicas`、`generation`、`resourceTemplateGeneration` |
| `InterpretStatus`，对象还没有状态 | `readyReplicas: 0`、`desiredReplicas: 2`、`generation: 1`，没有 Lua 错误 |
| `InterpretHealth` | 3/5 可用时为 false；没有状态时为 false |
| `InterpretDependency` | ConfigMap、PVC、Secret、ServiceAccount 各一条，重复引用已去重 |
| `AggregateStatus`，4 个集群，其中一个没有新字段（旧版 kthena）、一个被拒绝（没有状态） | 计数器求和；`unschedulableSince` 取最早；有集群没应用成功时不推进 `observedGeneration`；输出里没有合成键 |

## 4. Kind 刻画：按副本拆分（方案 M）

**环境**（`artifacts/logs/kind-00-environment.log`、`kind-01-*.log`、`kind-02-*.log`）：

| 项 | 值 |
| --- | --- |
| 集群 | `k014-host`（Karmada 控制面）、`k014-m1`（push，1 控制面 + 2 worker）、`k014-m2`（push，单节点）、`k014-m3`（pull，单节点）。Kubernetes v1.37.0，amd64 |
| Karmada | `hack/local-up-karmada.sh` 从源码构建，版本串 `v1.20.0-beta.0-44-g6d007132e-dirty`，commit `6d007132`。`-dirty` 是因为构建期间临时给 `artifacts/kindClusterConfig/member1.yaml` 加了两个 worker 节点，脚本退出时已用 `git checkout` 还原；Go 代码没有改动。各组件的镜像 digest 在环境日志里 |
| 特性门控 | `local-up` 默认给各组件加 `--feature-gates=AllAlpha=true,AllBeta=true`。刻画的对象是默认配置，所以用 `kind/03-reset-feature-gates.sh` 去掉了这个参数。C3 单独打开 `MultiplePodTemplatesScheduling` |
| kthena | 从 upstream main（`1a5145e9`）构建 `ghcr.io/volcano-sh/kthena-controller-manager:dev-014`，`kind load` 到三个成员，Helm 安装 workload chart；Pod 的 imageID 为 `sha256:0b3913b5…`，三个成员一致 |
| GPU | 给节点 status 打补丁的扩展资源 `nvidia.com/gpu`：m1 两个 worker 各 2 个，m2、m3 各 8 个。Pod 用 `registry.k8s.io/pause:3.10` |
| 调度器 | 成员集群没有装 Volcano，ModelServing 用 `schedulerName: default-scheduler` |
| 脚本和 YAML | [`artifacts/kind/`](artifacts/kind/)（`00-env.sh` 到 `04-set-feature-gate.sh`、`lib.sh`、`manifests/`、`99-cleanup.sh`） |

**结果**：

| 编号 | 场景 | 观察到的 | 日志 |
| --- | --- | --- | --- |
| C0 | 不装解释器；模板 3 个副本；Divided/Aggregated，m1、m2 | 绑定没有副本数；两个成员都是 3（共 6 组）；控制面状态为空；m1 有 2 组、m2 有 1 组调度不上，Work 仍是 `Healthy`；聚合项里有整份状态，含 `unschedulableReplicas` | `kind-C0-no-interpreter.log` |
| C10 | 在 C0 的状态上装解释器 | 2 分钟内没有变化。给模板加一个注解后：绑定变成 `replicas: 3`，调度失败（GPU 估算为 0），两个成员的 `spec.replicas` 变成 0，Pod 全部删除 | `kind-C10-interpreter-installed-later.log` |
| C10b | 同 C10，但用能调度成功的配置（不请求 GPU） | 调度结果 `{k014-m1:2}`。m1：generation 1 → 3，4 个 Pod 全部删除重建（UID 全变）。m2：ModelServing 和 4 个 Running 的 Pod 被删除 | `kind-C10b-*.log` |
| C2a | 默认配置；3 副本 × 1 GPU 的 Deployment；m1 空闲 4 GPU，m2 空闲 8 GPU | general estimator `[0, 0]`，scheduler-estimator `[4, 8]`，调度失败：`Clusters available replicas 0 are not enough to schedule` | `kind-C2a-gpu-estimate-zero.log` |
| C2b | 清空三个成员的 `spec.resourceModels` | `allocatableModelings` 变空；同一个 Deployment 调度成功 | `kind-C2b-C2c-*.log` |
| C2c | ModelServing 3 副本，每组 4 Pod × 1 GPU；m1 真实能放 1 组 | general `[1, 2]`，scheduler-estimator `[0, 2]`；合计 2 < 3，调度失败，一个也不放。把 m1 的 scheduler-estimator 缩到 0 后：调度器记连接错误并忽略，按 m1:1、m2:2 拆分，3 组全部 Running，控制面 `replicas: 3`、`availableReplicas: 3`、`observedGeneration` 等于模板代数 | 同上 |
| C4 | ModelServing 3 副本，每组 3 GPU + 1 GPU；m1 估算 1、真实 0；等待 8 分 28 秒 | m1 的组调度不上：worker Pod Running，entry Pod Unschedulable。`unschedulableReplicas: 1` 和 `unschedulableSince` 出现在聚合项和模板状态里，m1 的 health 是 `Unhealthy`。descheduler（阈值 5 分钟、间隔 2 分钟）日志里没有这个绑定，绑定没有变化，没有相关事件 | `kind-C4-unschedulable-and-descheduler.log` |
| C5 | 手工降副本，模拟 descheduler | 四种结果，见 [01-design.md](01-design.md) 8.3 的表。Running 的组 Pod UID 不变，kthena 删的是调度不上的组 | `kind-C5-reduce-then-scheduler.log`、`kind-C5-pingpong-and-directed-move.log` |
| C5b | 定向迁移：同一次更新里 m2 4 → 3、新增 m1:1 | 调度器日志 `Don't need to schedule`；m1 上建出 Work 和组；8 个副本全部可用 | `kind-C5-pingpong-and-directed-move.log` |
| C6 | 模板 4 副本、整数 `maxUnavailable: 2`；静态权重 1:3 | m1 分到 1，被 kthena webhook 拒绝；Work `Applied=False`，绑定 `FullyApplied=False`，模板有 `SyncFailed` 事件；控制面只统计到 m2 的 3 个。去掉 `rolloutStrategy` 后两个成员都应用成功 | `kind-C6-replica-dependent-validation.log` |
| C7 | 在成员上把 `spec.replicas` 3 改成 5 | 不带保留标签：3 秒内被改回。带标签：保持 5；绑定仍是 3，控制面 `spec.replicas: 4`、`status.replicas: 6` | `kind-C7-retain.log` |
| C3 | 打开 `MultiplePodTemplatesScheduling`，解释器加 `componentResource`，更新一次模板 | 绑定只有 2 个组件、没有副本数；两个成员 `spec.replicas` 从 1 变成 0，Pod 全部删除；绑定仍是 `Scheduled=True`、`FullyApplied=True` | `kind-C3-component-resource-trap.log` |
| C9 | pull 模式成员（m3） | 在 C5 里作为目标集群：下发、状态上报、健康解释都正常 | `kind-C5-*.log` |
| C11 | 集群副本数降到 0 | 该集群被移出绑定，成员上的 ModelServing 被删除 | `kind-C5-reduce-then-scheduler.log` |

**过程说明**：

- 两次环境上的弯路，都已修正并反映在脚本里：kthena 的 controller-manager 会 watch networking 的 CRD，
  只装 workload chart 时它不停报错，改成单独 `kubectl apply` 这些 CRD；Helm 的 `networking.kthenaRouter.enabled=false` 没有阻止 router 部署，改回 `networking.enabled=false`。
- C2b 的对照 Deployment 调度成功后占了 m1 的 3 个 GPU，随即删除，后续场景开始前确认过 GPU 已释放。

**没有覆盖的**：

- Volcano gang 调度下的全部行为。
- `dependencyInterpretation` 在真实集群里的执行。
- 滚动升级和 surge 组（原计划的 C8）。
- 重调度的规则 1（新鲜度）、规则 6（有去处才降）：这两条是要实现的东西，现在只验证了它们要解决的现象。
- [01-design.md](01-design.md) 3.2 节建议的「暂停下发后再装解释器」的步骤。

## 5. Kind 刻画：按组分发（方案 G）

用 `replicas: 1` 的 ModelServing 当单元，看「每个 ServingGroup 一个独立对象、整体放进一个集群」在 Karmada 里的行为。结论的讨论在 [02-servinggroup-cr.md](02-servinggroup-cr.md)。

**环境**

和第 4 节相同（同一组脚本，Karmada `6d007132` 重新构建，kthena `1a5145e9`），差别：

| 项 | 值 |
| --- | --- |
| 特性门控 | scheduler、controller-manager、webhook 开 `MultiplePodTemplatesScheduling`；G-4b 之后在这三个组件和三个 scheduler-estimator 上再加 `SchedulingOvercommitProtection`（`artifacts/logs/kind-G-01-feature-gates.log`） |
| scheduler-estimator | 三个成员都保留（G-7 临时把 m1 的缩到 0） |
| 资源模型 | G-0 用默认值，之后清空 |
| 解释器 | [`artifacts/unit/unit-customization.yaml`](artifacts/unit/unit-customization.yaml)：只有 `componentResource`、状态反射和聚合、健康、依赖；没有 `replicaResource`、`replicaRevision`。健康定义成「没有调度不上的 Pod」。离线检查在 `artifacts/logs/unit-interpreter-offline.log` |
| 单元 | `artifacts/unit/gen-unit.sh`：5 种形状的 1 副本 ModelServing |
| 策略 | `artifacts/unit/manifests/pp-units.yaml`（spread 约束：集群 min=max=1；`Divided/Aggregated`），`pp-units-failover.yaml`（再加应用故障转移：容忍 30 秒，`Gracefully`，宽限 60 秒，`propagateDeps: true`） |

**结果**

| 编号 | 场景 | 观察到的 | 日志 |
| --- | --- | --- | --- |
| G-0 | 默认资源模型；策略没写 `replicaScheduling` | 调度器算出的估算值是三个集群全 0，然后忽略了它，把单元放到 m1（恰好放得下） | `kind-G0-default-resource-models.log` |
| G-0b | 同上，策略写 `Divided/Aggregated` | 调度失败：`no enough resource when selecting 1 clusters` | 同上 |
| G-1 | 清空资源模型；逐个创建 6 个单元（每组 4 Pod × 1 GPU） | 每一步的估算值都等于真实剩余容量；5 个单元按 m1:1、m2:2、m3:2 放满并 Ready；第 6 个不调度，成员里没有它的 Pod | `kind-G1-unit-estimation-accuracy.log` |
| G-2 | m1 空着；单元是 3 GPU + 1 GPU | m1 估算 0，单元不调度 | `kind-G2-G3-shapes-and-queueing.log` |
| G-3 | m1 空着；单元的 Pod 带 m1 不满足的 nodeSelector | m1 估算 0，单元不调度 | 同上 |
| G-2/3 对照 | m1 空着；单元是 4 Pod × 1 GPU | m1 估算 1，放进 m1 并 Ready | 同上 |
| 排队 | 在 m2 腾出 4 个 GPU | 排队的单元在 4 分 09 秒后被调度（距它上一次失败 5 分 17 秒） | 同上 |
| G-4a | 一次创建 5 个单元，不开超卖保护 | 65 毫秒内完成 5 次估算；3 个进了 m2（容量 2），其中 2 个各有 2 个 Pod Ready、2 个调度不上；m1 空着 | `kind-G4-burst-overcommit.log` |
| G-4b | 同上，开超卖保护 | 估算值逐次递减，5 个按 1 + 2 + 2 放满并 Ready | 同上 |
| G-5a | 单元带 Pod 反亲和（需要 2 个节点），先只允许 m2（单节点），再放开 m1 | 单元在 m2 上 1 个 Pod Ready、1 个调度不上，健康为 `Unhealthy`；约 30 秒后被驱逐并调度到 m1，2 个 Pod Ready | `kind-G5-stuck-unit-failover.log` |
| G-5b | 同样的单元，只允许 m2、m3（都放不下） | 4 分 34 秒里 6 次驱逐，在两个集群之间轮换，中间有两段时间两边都不在；每次 Pod 的 UID 都变 | 同上 |
| G-5c | 单元已调度、Pod 永远不 Ready | 健康为 `Healthy`，2 分钟内 0 次驱逐，Pod UID 不变 | 同上 |
| G-6 | 运行中的单元缩小 role（worker 3 → 2 → 1） | 所在集群还放得下一整组时留在原地；放不下时（m2 只剩 1 个 GPU）被搬到 m3 | `kind-G6-running-unit-not-sticky.log` |
| G-7 | 4 个单元放满 m2、m3 并 Ready，然后把 m1 加进策略 | 1 个 Running 的单元被从 m2 搬到 m1；另外 3 个的绑定变成 `Scheduled=False`，Pod 没动 | `kind-G7-missing-scheduler-estimator.log` |

过程说明：

- G-5 第一次运行被 Karmada 的 webhook 拒绝（应用故障转移要求 `propagateDeps: true`），改策略后重跑。
- G-6 第 1 步放宽策略之前没有单独打印单元所在的集群；同样的操作在 G-7 里有前后记录。
- G-7 原本想看「一个成员没有 scheduler-estimator 时估算退化成什么」，但放宽策略本身先把一个单元搬进了 m1，这一点没有测出来。
- G-6 第 3 步有两个单元没被调度，而当时 m3 还放得下一个，原因没有查。

## 6. 方案取舍

方案正文在 [01-design.md](01-design.md)。这里只记录比较过的选项和否决理由。

### 6.1 资源需求建模

| 选项 | 失败方式 | 结论 |
| --- | --- | --- |
| 整组求和 | 节点级估算下一组放不进单节点就是 0，调度失败（显式） | **采用**，并写明适用条件 |
| 最大 Pod | 按每组 Pod 数的倍数高估，副本被压到放不下的集群（隐式） | 否决 |
| 副本数报总 Pod 数、需求报最大 Pod、`ReviseReplica` 再除回去 | Karmada 按整数拆 Pod 数，不按组对齐，会丢组 | 否决 |
| 组件集（`componentResource`） | 现在不能用于 Divided，还会触发副本数变 0 | 第 3 阶段，需要上游改调度器 |

### 6.2 重调度的不可调度数从哪里来

| 选项 | 评价 | 结论 |
| --- | --- | --- |
| 聚合状态里的约定键（`readyReplicas`、`unschedulableReplicas`、`unschedulableSince`） | 不需要 estimator，pull 模式可用，副本单位天然正确；需要新鲜度校验；只有最老一组的时间 | **采用** |
| 新增解释器操作 | descheduler 是独立进程，没有解释器框架，最终还是要把数值放进聚合状态；等于上一项加一份新 API | 否决 |
| Pod selector（#8084）加按组计数 | 需要每个成员有可达的 estimator，需要新增「分组键」概念，分组语义要在 Karmada 里重复实现 | 不作为主路径；maintainer 坚持时可用「不可调度 Pod 数 ÷ 每组 Pod 数」的保守退化形式 |

### 6.3 降副本之后的去向

| 选项 | 评价 | 结论 |
| --- | --- | --- |
| 只降副本，去向交给调度器，降之前预检「有去处」 | Kind 上有容量时调度器把副本放到了别的集群；调度器的过滤照常生效；预检是估算，失败时退化成今天 Deployment 的行为 | **采用** |
| 定向迁移（同一次更新里 A 减、C 加） | Kind 上机制可行；完全避开「无处可去」和「还回原集群」；但要自己选目标集群，绕过调度器的过滤插件 | 备选，第一版不做 |
| 只降副本，不预检 | Kind 上无处可去时副本 5 分钟不属于任何集群，然后回到原集群重新计时，循环 | 否决 |

方案的初稿和第一轮方案分析（附录）都认为「降完之后副本会被还给原集群」，并据此选了定向迁移。Kind 的结果不支持这个前提，所以改成上表。

### 6.4 重调度放在哪里

| 选项 | 评价 | 结论 |
| --- | --- | --- |
| 改 karmada-descheduler（我们的 karmada 分支），同时提交上游 | 改动集中在一个可选组件；上游合入前可以只替换这一个镜像；和 #8104、#4046、#8109 的方向一致 | **建议** |
| 独立的控制器，算法相同 | 不依赖上游；多维护一个组件 | 上游不接受状态来源时的后备 |
| WorkloadRebalancer | 全量重新分配，会动正在运行的组 | 只作为人工工具写进指南 |
| 应用故障转移 | 驱逐整个集群的份额 | 不用于 Divided |

第一轮方案分析建议先做独立控制器，理由是上游方案还需要改调度器核心才能防止副本被还回原集群。Kind 上不需要改调度器，这个理由不再成立，
所以建议改成先做 descheduler。先走哪条路是待决策点（[03-decisions.md](03-decisions.md)）。

## 7. 验证计划

环境同第 4 节，实现阶段另外在成员集群装 Volcano，并用改动后的分支构建镜像。

### 7.1 第 1 阶段（解释器和指南）

| 内容 | 通过标准 |
| --- | --- |
| 解释器 testdata（按 Karmada 第三方定制目录格式） | 覆盖 [appendix/round1-behavior-regression.md](appendix/round1-behavior-regression.md) 7.2 节的 L-01 到 L-09；Karmada 的 `thirdparty_test.go` 通过 |
| Kind：Duplicated、静态权重、Aggregated | 重跑 C1/C2c 的正例，这次用 Volcano 和带 ConfigMap、Secret、PVC 的模板 |
| Kind：反例 | 重跑 C0、C3、C6、C10，确认指南里写的后果和实际一致 |
| Kind：「暂停下发后再装解释器」 | 正在运行的组 Pod UID 不变 |
| Kind：C8 | 两个成员同时滚动；记录 surge 组是否计入 `unschedulableReplicas` |

### 7.2 第 2 阶段（karmada-descheduler）

| 编号 | 步骤 | 预期 |
| --- | --- | --- |
| R-1 | C4 的状态，别的集群有容量，阈值设 1 分钟 | 超过阈值后问题集群的副本数降到就绪数；副本出现在别的集群；Running 的组 Pod UID 不变 |
| R-2 | 新鲜度：暂停成员的 kthena 控制器让状态停住，descheduler 连续跑两轮 | 第二轮因 `desiredReplicas` 和绑定不一致而跳过 |
| R-3 | 别的集群都满 | descheduler 不降，记一条事件；组留在原集群排队 |
| R-4 | 未超过阈值 | 不动 |
| R-5 | 组因为镜像拉取失败而未就绪 | `unschedulableReplicas` 为 0，不动 |
| R-6 | 滚动升级期间 | 降的数量不超过 `spec - ready` |
| R-7 | pull 模式成员 | 行为和 push 一致 |
| R-8 | Deployment 回归 | 带和不带新参数时，Deployment 的 deschedule 行为和基线一致 |
| R-9 | 静态权重、Duplicated、带保留标签的模板 | 不处理 |

单元测试：`pkg/descheduler/core` 的取数规则表（阈值、字段缺失、`applied=false`、集群 NotReady、不低于就绪数、新鲜度、预检）。

### 7.3 行为回归

[appendix/round1-behavior-regression.md](appendix/round1-behavior-regression.md) 7.3 节的 K-01 到 K-25 里和所做改动相关的场景；其余作为指南内容的依据，不逐条执行。

## 8. 还没有验证的

| # | 内容 | 怎么确认 |
| --- | --- | --- |
| 1 | Volcano gang 调度下的全部行为：`unschedulableReplicas` 的判定、卡住的组不占 GPU、估算值 | 7.1 用 Volcano 重跑 |
| 2 | 「暂停下发后再装解释器」能否避免清零 | 7.1 |
| 3 | 重调度的新鲜度问题（规则 1 要防的现象）能否在真实时序里复现 | R-2 |
| 4 | 预检通过但调度失败的频率；是否需要启用定向迁移 | 第 2 阶段运行观察 |
| 5 | 滚动升级、surge 组和重调度的相互作用 | C8、R-6 |
| 6 | `dependencyInterpretation` 在真实集群里的执行；PVC 随调度结果移除的后果 | 7.1 |
| 7 | FederatedHPA 对 ModelServing 的端到端可用性 | 可选场景 |
| 8 | 调度器或 descheduler 调整副本数时，被 [01-design.md](01-design.md) 5.4 的校验拦住的后果 | C6 的延伸 |
| 9 | 上游是否接受状态来源 | 在 #8104 讨论 |
| 10 | PR #8084 仍在评审，本文引用的是 2026-11-28 的 diff | 合入前复查 |
| 11 | 按组分发：Volcano gang 下的全部行为；控制面的对象数量、写入量和调度延迟；G-c0（1 副本加方案 M 的解释器）下应用故障转移的行为；不装解释器时放置的单元在装上之后是否保持不动 | [appendix/round2-proposal-analysis.md](appendix/round2-proposal-analysis.md) 9.4 的 K3、K10–K17 |
| 12 | 按组分发：一个成员没有 scheduler-estimator 时估算退化成什么。G-7 这次没有隔离出来 | 重跑附录 round2-proposal-analysis.md 9.4 的 K4 后半部分 |
| 13 | 按组分发：批量创建时有单元在集群有余量的情况下没有被调度（G-6 第 3 步的 h4、h5），原因没有查 | 重现并读调度器的记账 |
| 14 | 自包含 ServingGroup CR 加控制面父控制器（G-a）的全部结论：没有原型，都是读 ServingGroup CR 原型的源码推出来的 | 只有 #2148 选了 CR 路线才需要 |
