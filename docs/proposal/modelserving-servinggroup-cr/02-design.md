# 完整设计

## 0. 文档信息

| 项 | 内容 |
| --- | --- |
| 用途 | ServingGroup 作为 CR、保留序号的完整设计：对象、两个 controller、组一级和 Role 一级的规则、并发、plugin 调用点、周边组件。状态和转换的逐条写法在 [03](03-state-machine.md)，这里写规则和理由 |
| 起点 | 对象模型取自 [007 的 v4](../modelserving-state-machine-v3-v4/04-v4-servinggroup-cr.md)：ServingGroup 是 CR，两个 controller，账本。组一级的规则取自契约 2.12 和 [v2](../modelserving-rollout-final/04-state-machine.md)：按序号。007 的 v4 和本目录的上一版（提交 `e35b2e37`）去掉了组的序号，这一版保留。改了的地方列在[第 11 节](#11-相对前面几版改了什么) |
| 沿用 | 两层的滚动、扩缩容和 Role 一级的协调，规则沿用 [v2](../modelserving-rollout-final/04-state-machine.md)；原地更新的接口点沿用 [007 的 05](../modelserving-state-machine-v3-v4/05-inplace-usage-isolation.md)；判据 J1–J8 沿用 [007 的 02](../modelserving-state-machine-v3-v4/02-dfa-criteria-and-tradeoffs.md) |
| 状态 | 设计提案。没有改代码，没有在集群上验证。组一级的规则和账本的两条防护规则用模型穷举过，范围见 [05](05-inference-verification.md) |
| 记号 | N、U、S、P 是最新 `spec` 解析出来的副本数、`maxUnavailable`、`maxSurge`、`partition`。T 是目标版本，B 是基线版本。S1′、G0–G6 是组一级一轮计划的各步。「走读」指读代码得出，行号对应 `volcano-sh/kthena` `main` @ `79c455ef`。D、R、O 开头的是 005 的编号，K、T 开头的是 007 的编号。别的文档里的编号：T1–T7 是 [06](06-live-upgrade.md) 的接管步骤；NT 开头的是 [04](04-behavior-expectations.md) 的 8.7 要新增的用例；DR、OP、UP 开头的是 [README](README.md) 里的决定 |

## 1. 概览

```text
ModelServing ──owns──► ServingGroup ──owns──► Pod、PodGroup、headless Service
     │                      │
     │ ModelServing         │ ServingGroup
     │ controller           │ controller
     ▼                      ▼
 组的个数、版本、次序      Role 实例、成员、恢复、原地更新
```

一句话：**ServingGroup 是一个对象，名字是 `<ModelServing 名>-<序号>`；范围一级的事实都写在这个对象上；决定的输入只来自对象；ModelServing controller 只管「有几个组、各在哪个序号、是什么版本」，ServingGroup controller 只管一个组里面的事。**

事实放在哪里：

| 事实 | 今天 | 本设计 | 写者 |
| --- | --- | --- | --- |
| 这个组存在、是哪个版本 | 一组 Pod 上的标签 | `ServingGroup` 对象和它的 `spec.revision` | ModelServing controller |
| 这个组正在被删除 | 内存里的单元状态；在网版本另有 Pod 上的 `deletion-scope` 注解 | `ServingGroup.metadata.deletionTimestamp` | ModelServing controller（发 DELETE） |
| 这个组丢了成员、要整组重建 | 内存 | `ServingGroup.status` 的 Failed | ServingGroup controller |
| 这个 Role 实例建成过 | 内存里的一个布尔值 | `ServingGroup.status.roleInstances[]` 的 Established | ServingGroup controller |
| 这个 Role 实例正在被清空 | 内存；在网版本另有 Pod 上的注解 | `roleInstances[]` 的 Draining | ServingGroup controller |
| 组是不是临时 surge | 组的每个 Pod 上的 surge 注解，逐个打 | 组对象上的 surge 注解：创建时写，转正时摘。Pod 上照旧带一份，是副本，不当输入 | ModelServing controller |
| 重启故障从什么时候起算 | 内存 | `ServingGroup.status.restartFaults[]` | ServingGroup controller |
| 基线版本 | `ModelServing.status.currentRevision` | 不变，另在每个组的 `spec.baselineRevision` 留一份 | ModelServing controller |

## 2. 对象

### 2.1 ServingGroup

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ServingGroup
metadata:
  name: llama-2                         # <ModelServing 名>-<序号>，见 2.2
  labels:
    modelserving.volcano.sh/name: llama
  annotations:
    modelserving.volcano.sh/surge: serving-group   # 只有临时的 surge 组有：创建时写，转正时摘掉，不会后加（4.2）
  ownerReferences:                      # ModelServing，controller=true，blockOwnerDeletion=true
spec:
  revision: llama-6b7f9                 # ServingGroupRollingUpdate：这个组的版本
  targetRevision: llama-6b7f9           # RoleRollingUpdate：组内滚动的目标
  baselineRevision: llama-5c4d8         # 两种模式都带：基线的副本；Role 模式下也是受 Role partition 保护的实例用的版本
  sourceGeneration: 12                  # 这份 spec 取自 ModelServing 的哪个 generation
  expectedPods: 6                       # 这个组期望的 Pod 数；给 scale 子资源和 PDB 用（8.5）
  recoveryPolicy: RoleRecreate
  restartGracePeriodSeconds: 300
  roles:                                # 每种 Role 的 replicas 和 Role 层的滚动参数、协调配置
  - name: prefill
    replicas: 2
status:
  observedGeneration: 3
  observedPods: 5f2a9c41d07be3a6        # 这份 status 是按哪些 Pod 算的：这个组的成员 Pod 的摘要（A7、4.1）
  conditions:
  - type: Ready                         # 口径与当前实现相同：每种 Role 的实例个数等于 replicas，每个实例运行并且就绪。空组（各 Role 的副本数都是 0）是真
  - type: Failed                        # 不可逆。真 = 请求删掉这个对象，在这个序号上重建
  - type: Drained                       # 只在 Failed 为真时有意义：没有故障成员，没有正在终止的 Pod
  - type: RecreateRequired              # 这个组只能删掉重建：配了原地更新而它不能原地更新（第 9 节）；收养来的成员版本与组的不同（5.5）
  - type: DrainBlocked                  # OnRoleDelete 一直不成功，有实例停在清空中（第 7 节）
  - type: ReconcileFailed               # 这一轮出错（例如引用的 ControllerRevision 不在）。Ready 仍然按看到的成员照实写
  - type: CoordinatedRoleRolloutBlocked # 含义不变，按组报告，ModelServing controller 汇总
  revision: llama-6b7f9                 # 全部实例已经应用的版本；不一致时为空
  referencedRevisions: [llama-6b7f9]    # 名下 Pod 引用到的全部版本，包括正在终止的
  deletionCost: 0                       # 名下 Pod 的 deletion cost 之和。给人看的；缩容的次序不读它（4.4）
  faultyMembers: 0                      # 给人看的；判断只用 condition
  terminatingPods: 0
  pods: 6                               # 名下现有的 Pod 数，含终止中的，加上已经决定要建而还没建出来的（5.2 的 L8）。scale 子资源的 status 和「没有成员」的判定用它
  roleInstances:                        # 账本，见 5.2
  - role: prefill
    index: 0
    phase: Established                  # Creating（零值）、Established、Draining
    generation: 4
  restartFaults:                        # 重启故障的起点，见 5.3。没有故障时为空
  - podUID: 0d1c...
    since: "2026-11-22T08:00:00Z"
    readyTransitionTime: "2026-11-22T07:59:58Z"
```

字段名是提案，不是 API 评审稿。`spec` 里**不内嵌 Pod 模板**：模板从 ControllerRevision 按名字读。这样对象小，`kubectl apply` 不会因为注解太大失败，版本的身份也只有一个来源。

Go 里已经有一个同名的模板类型 `ServingGroup`（走读，`pkg/apis/workload/v1alpha1/servinggroup_types.go:116`）。它要改名（例如 `ServingGroupTemplate`）。JSON 不变；仓库外引用这个 Go 类型的代码要跟着改。

### 2.2 名字

组名是 `<ModelServing 名>-<序号>`，与今天相同。Pod 名 `<组名>-<role>-<实例序号>-<worker 序号>`、headless Service 和 PodGroup 的名字都跟着组名，也与今天相同。标签 `modelserving.volcano.sh/group-name` 的值是组名。

- **序号是位置，UID 是这一代。**滚动替换和整组恢复都是「删掉这个对象，等它消失，再建一个新对象」；序号集合连续时新对象就在原来的序号上（稀疏时的例外见 4.2）。名字不变，UID 变了。用户按名字匹配的东西（脚本、告警、面板、DNS 名）不受影响；要分辨新旧两代的地方用 UID（[4.5](#45-名字确定创建幂等同名重建)）。
- **名字确定，创建是幂等的。**重复的创建撞名字。不用 `metadata.generateName`，也不用在内存里登记发出过哪些创建。撞名字之后要核对那个对象是不是自己的（4.5）。
- **新建的组用哪个序号**由规则决定（4.2）：正式的位置取小于 N 的最低空闲序号；临时的 surge 取不小于 N 的最低空闲序号。序号空闲不空闲只看有没有这个名字的 ServingGroup 对象（4.4）。
- **序号集合可以是稀疏的。**按 deletion cost 的缩容会留下洞（契约 0.6）。`group-index` 和 hostname 因此可以不连续，今天也是这样。
- owner 是 LeaderWorkerSet 的 ModelServing 不需要特殊处理：plugin 照旧从组名里取 `group-index`、hostname 和 DNS 名（走读，`plugins/lws_labels_plugin.go:61-95`）。滚动途中 `group-index` 临时有一个不小于 N 的值，与今天相同。

### 2.3 Pod 上新增的东西

| 项 | 内容 |
| --- | --- |
| controller owner | ServingGroup（`controller: true`、`blockOwnerDeletion: true`）。PodGroup、headless Service 相同；PodGroup 今天没有 `blockOwnerDeletion`（走读，`podgroupmanager/manager.go:340-350`），要补上，否则前台级联不等它 |
| 标签 `modelserving.volcano.sh/instance-generation` | 所在 Role 实例的代际号（5.2）。没有这个标签的 Pod 按 0。升级接管换 owner 时给存量的 Pod 补上（值是 0），所以 ServingGroup controller 管过的 Pod 都带着它 |
| 注解 `modelserving.volcano.sh/surge`，值 `serving-group` | 组一级 surge 标记的**副本**。来源在组对象上（2.1）。ServingGroup controller 只在 `ServingGroupRollingUpdate` 下维护：组带注解，名下每个 Pod 都带；组不带，把值是 `serving-group` 的摘掉。值是 `role` 的 Pod 它不动；`RoleRollingUpdate` 下不写副本。它是输出：今天的工具和 runner 从 Pod 上认临时的组，回退时旧版本也从 Pod 上读。只在两处被读，都是建组对象的那一刻：升级接管（[06](06-live-upgrade.md)）和找回（5.5） |
| 不变的 | Role 一级的 surge 注解（同一个 key，值 `role`，来源就在 Pod 上）、原地更新的进度注解、revision 和 role-template-hash 标签 |

没有 Pod finalizer，没有组的 finalizer。

### 2.4 谁写什么

| 对象上的东西 | 唯一的写者 | 方式 |
| --- | --- | --- |
| ServingGroup 的创建 | ModelServing controller | 名字由序号确定 |
| ServingGroup 的 DELETE | ModelServing controller | 前台级联，带 UID 和 resourceVersion |
| `ServingGroup.spec` | ModelServing controller | PATCH，带 UID 和 `sourceGeneration`，不下发更小的。改 `spec.revision` 的两种（4.2 的 G2′(b)、原地的 G3c）另带 resourceVersion |
| ServingGroup 的 labels、annotations | ModelServing controller，在创建时 | 创建之后只有一种改动：摘掉 surge 注解（转正），带 UID |
| `ServingGroup.status` | ServingGroup controller | UPDATE，带 resourceVersion |
| Pod、PodGroup、Service | ServingGroup controller | 创建按确定的名字；删除带 UID |
| `ModelServing.status` | ModelServing controller | 不变 |
| ModelServing 上的接管注解（进度） | ModelServing controller | PATCH；内容见 [06](06-live-upgrade.md) |
| ControllerRevision | ModelServing controller | 不变。清理时保留的集合见 8.6 |

ServingGroup controller 不创建、不删除 ServingGroup，只写「失败」。它只管 controller owner 是一个现存的 ModelServing（按 UID）的组；没有 owner、或者 owner 已经不在的组，它不创建、不删除、不恢复，发事件。这与今天用 orphan 方式删 ModelServing 之后 Pod 没有人管相同。

ModelServing controller 不写 Pod；读 Pod 有四处：

- 升级接管期间（接管注解不是 `Done`）：直读存量的 Pod、PodGroup、Service，并按 UID 删掉带着旧版本删除标记的 Pod 和没有活 Pod 的组留下的 PodGroup、Service（[06](06-live-upgrade.md) 的 T1、T2、T2′）。这是它唯一写 Pod 的地方。
- 找回：看哪些序号上有无主的成员 Pod，为它们重建同名的组对象（[5.5](#55-有人直接删了一个组)）。只读。
- 复核：组的 `status` 是不是按现在这些 Pod 算的（A7）。只读缓存，只比摘要，不另算一遍就绪。
- 次序：缩容和回收里「未就绪的先」和 deletion cost（4.4）。只读缓存；它们是偏好，不进额度。

ModelServing controller 按 controller ownerReference 的 UID 认自己的组，不按标签。没有 owner 的 ServingGroup（用 orphan 方式删 ModelServing 留下的）不会被同名重建的 ModelServing 收养：它占着那个序号，发事件报告，由人删除（4.5）。

用户直接改组的 `spec`，等于改 Deployment 名下的 ReplicaSet：ModelServing controller 下一轮写回去。用户直接删组：见 [5.5](#55-有人直接删了一个组)。

## 3. 两个 controller

| | ModelServing controller | ServingGroup controller |
| --- | --- | --- |
| 队列的 key | ModelServing | ServingGroup |
| 看什么 | ModelServing 的 `spec`；名下的 ServingGroup 对象 | 一个 ServingGroup 的 `spec` 和 `status`；名下的 Pod |
| 做什么 | 组一级的扩缩容和 `ServingGroupRollingUpdate`；删除失败的组并在原序号上重建；把 `spec` 下发到每个组；汇总 `status`；推进基线；解析和清理 ControllerRevision | Role 一级的扩缩容、`RoleRollingUpdate`、协调、恢复、原地更新；PodGroup 和 Service；plugin 钩子；写这个组的 `status` |

### 3.1 约定

两者之间只通过对象交换信息。约定有六条（A5 不再是约定，编号不动）：

| # | 约定 | 为什么 |
| --- | --- | --- |
| A1 | **`observedGeneration`。**`status.observedGeneration` 小于 `metadata.generation` 的组，ModelServing controller 按「未观测」计：占位置，不算确认的就绪（4.1）。ServingGroup controller 在「这一轮的决定是按这份 `spec` 做的」时推进它，不等动作做完。**出错的轮次也写**：只要读到了成员，`observedGeneration` 照样推进，Ready 按看到的成员照实算，另带 `ReconcileFailed`。模板读不到时（每个实例有几个成员在模板里），Ready 按「现有的 Pod 不少于 `spec.expectedPods`、全部就绪、没有终止中的」算 | 新 `spec` 刚写进去时 `status` 还是上一份的结论。出错时如果不写，这个组会一直是「未观测」（不能被替换），或者一直带着上一次的 Ready。出错时也不能把 Ready 一律写成假：成员其实在服务的组会被当成未就绪的组优先删掉 |
| A2 | **`sourceGeneration`。**下发的 `spec` 带着来源的 ModelServing `generation`。ModelServing controller 不下发比组上已有的更小的；看到更大的就重新入队 | 缓存旧的进程不会把旧的 `spec` 再写回去 |
| A3 | **失败和已清空是两个 condition。**Failed 为真是请求删除；Drained 为真是「故障成员和正在终止的 Pod 都没有了」。ModelServing controller 只在两个都为真时删 | 判断不依赖计数字段的零值。`status` 被旧客户端整体改写、计数丢失时，不会把「没有清空」读成「已清空」 |
| A4 | **基线的副本。**每个组的 `spec.baselineRevision` 由 ModelServing controller 写 | `ModelServing.status` 被清空时基线可以读回来（4.8） |
| A5 | （不再是约定。）deletion cost 由 ModelServing controller 从 Pod 缓存现算：成员 Pod 的注解之和，在决定的那一刻读，与今天相同（4.4）。`status.deletionCost` 只给人看 | 上一稿里它经组的 `status` 汇总，晚一跳：刚设完 cost 马上缩容可能不生效。runner 造稀疏夹具正是这样做的 |
| A6 | **引用到的版本。**`status.referencedRevisions` 报告名下 Pod 用到的全部版本 | ControllerRevision 的清理不再去 LIST Pod（8.6） |
| A7 | **复核。**ServingGroup controller 每次写 `status` 时一并写上 `status.observedPods`：这份 `status` 是按哪些 Pod 算的（这个组的成员 Pod 的摘要，取判定用到的字段，4.1）。ModelServing controller 用自己的 Pod 缓存、同一个函数算同一个摘要。相同，这份 `status` 是**新鲜的**；不同，这个组按未观测算，直到 ServingGroup controller 重写（4.1） | 组刚变坏或者刚恢复、`status` 还没写出来的那一跳不进额度，也不让它被当成未就绪的组删掉。停机期间变了而 `status` 没有人写的组，新进程不会按旧的 `status` 处理（[6.3](#63-直读和复核)）。ModelServing controller 不另算一遍就绪：就绪只有 ServingGroup controller 里的一个定义，这里只判断那个结论过期没有 |

### 3.2 决定的输入只来自对象

每一轮要发的写是对象快照的函数：`W = δ(spec, S)`（007 的 J1）。`S` 是：ModelServing；它名下的 ServingGroup，和命名空间里名字落在它的序号上而不属于它的 ServingGroup；带它的标签的 Pod（组名下的、无主的、升级接管期间还属于 ModelServing 的）、PodGroup、Service；ControllerRevision；升级期间另有回退开关（[06](06-live-upgrade.md)）。时间只和对象上的时间戳比较。这是本设计的第一条原则：换一个进程、清空全部内存，算出来的写相同。

| 决定用到的事实 | 在哪个对象上 |
| --- | --- |
| 有几个组，各在哪个序号 | ServingGroup 对象的名字 |
| 组的版本；基线 | `ServingGroup.spec.revision`；`ModelServing.status.currentRevision` 和它在每个组上的副本 |
| 组是不是临时 surge | ServingGroup 的 surge 注解 |
| 组就绪没有、要不要整组替换、清空没有 | `ServingGroup.status` 的 Ready、Failed、Drained |
| 这份 `status` 新不新鲜 | `status.observedGeneration` 对 `metadata.generation`；`status.observedPods` 对成员 Pod 的 UID 和判定用到的字段（A1、A7） |
| 缩容和回收先删谁 | Pod 的就绪、`deletionTimestamp` 和 deletion cost 注解（4.4） |
| 组在不在删除中 | `deletionTimestamp` |
| Role 实例建成过没有、在不在清空、是第几代 | `ServingGroup.status.roleInstances[]`；Pod 的代际号标签 |
| 成员的状态 | Pod 的 `phase`、conditions、容器状态、`deletionTimestamp` |
| 重启故障从什么时候起算 | `ServingGroup.status.restartFaults[]`（5.3） |
| 原地更新做到哪一步；Role 一级的 surge | Pod 上的注解 |
| 升级接管做到哪一步 | ModelServing 的接管注解（[06](06-live-upgrade.md)） |

**缓存是对象的旧副本。**按旧副本算出来的创建会撞名字（4.5），但按旧副本算出来的删除可以是另一个对象。所以有写要发的轮次不按缓存发，按直读重算（[6.3](#63-直读和复核)）。直读是读对象，进程里不用记自己发过什么。

进程里还有的东西都不改变任何创建和删除，只会让写晚发：

| 进程里的 | 作用 | 丢了之后 |
| --- | --- | --- |
| informer 缓存 | 对象的副本；触发 reconcile；决定这一轮要不要直读 | 重新 LIST |
| 工作队列和它的限速、退避 | 什么时候再看一个 key | 重试早一点 |
| 本进程是否为一个清空中的实例成功调过 `OnRoleDelete`，以组的 UID、实例和代际号为键（第 7 节） | 钩子至少调一次 | 再调一次 |
| ModelServing controller 从什么时候起看到一个组未观测、不新鲜或者待收养，以组的 UID 为键 | 只决定 `GroupNotObserved` 什么时候报（4.1）。不决定任何创建和删除 | 重新计，报告晚一个期限。两次重启的间隔一直短于期限时不报 |

最后一项影响一个 condition 什么时候写。condition 也是写，所以它是「决定的输入只来自对象」的一处豁免，后果只有报告的早晚（[01](01-design-rationale.md) 第 6 节的 J1、J2）。

当前实现、v2 和本目录的上一版里另有几样在内存里、会改变写或者扣留写的东西，本设计都放到了对象上或者不再需要：

| 以前在内存里 | 现在 |
| --- | --- |
| 「这个实例建成过」「这个删除是计划内的」 | 账本（5.2） |
| 组和实例的状态机（创建中、运行、删除中） | 每一轮从对象判定（[03](03-state-machine.md)） |
| 重启故障的起点（D9） | 账本里的一项（5.3）。**这改变已批准的 D9**，要批准（[04](04-behavior-expectations.md) 第 11 节） |
| 发出去还没看到的创建 | 不需要：名字确定，重复的创建撞名字（4.5） |
| 发出去还没看到的删除、转正、下发 | 不需要：有写的轮次按直读重算（6.3） |
| 新进程里「ServingGroup controller 先把组过一遍」的先后 | 不需要：复核（A7） |

Kubernetes 原生 controller 的做法（来源见 [01](01-design-rationale.md) 第 5 节）：

| controller | 子对象的名字 | 进程里有没有影响动作的内存 |
| --- | --- | --- |
| StatefulSet | 确定：`<名>-<序号>` | 没有 expectations。每一轮从 Pod 和自己的 `status` 重新算 |
| Deployment | ReplicaSet 的名字由模板的哈希确定 | 没有 expectations |
| ReplicaSet、Job、DaemonSet | Pod 的名字随机 | 有 expectations：记着发出了几个创建和删除还没看到，用来让下一轮晚做。它主要是推迟动作；等待期间 `spec`、Pod 的状态和删除的候选都可能变，所以不能说它只改时间、不改动作 |

所以「状态只来自对象」在原生 controller 里成立的范围是：持久的业务事实在对象上；名字确定的两个，进程里没有影响动作的记忆；名字随机的那几个另有一份。本设计的名字是确定的，属于前一类。

### 3.3 部署前提

1. 两个 controller 在同一个进程里，共用一把租约，共用 informer。
2. 选主打开；失主的进程退出。今天选主默认关（走读，`cmd/kthena-controller-manager/main.go:86`），失主的回调只打日志、进程不退出（走读，`pkg/controller/controller.go:187-189`）；两处都要改。

选主和失主退出是为了不让两个进程长时间同时在写（6.4），不是正确性的唯一依靠：名字确定、每个写带前置条件，两个进程并存时不会多建组。新进程里两个 controller 之间没有先后的要求。

## 4. 组一级

组一级用的是契约 2.12 的按序号规则，单位是 ServingGroup 对象：两个额度、未就绪的先再健康的、绝对序号的 partition、临时的 surge 单元、转正、回收、补最低的空洞。它和 Role 一级是同一套规则（4.9）。相对当前实现和契约，规则本身没有变，变的是事实记在哪里：组的存在、版本、surge 身份、失败，都在组对象上。

### 4.1 分类和计数

组的序号从名字解析：只认 `<ModelServing 名>-` 后面是没有前导零的十进制数的名字，别的写法按解析不出序号处理（4.5）。只数 controller owner 是这个 ModelServing（按 UID）的组。每个组恰好属于一类：

| 类 | 条件 |
| --- | --- |
| 终止中 | `deletionTimestamp` 非空 |
| 失败 | Failed 为真 |
| 运行 | 其余。下文记作 `live` |

每个组另有几个属性：

| 属性 | 条件 | 含义 |
| --- | --- | --- |
| surge | 带 surge 注解（2.1）。只在 `ServingGroupRollingUpdate` 下参与分类 | 临时容量：只为滚动期间保住可用数而存在，滚动做完要回收 |
| 正式 | 不带 surge 注解 | 计入 N 的组 |
| 没有成员 | 不在终止中，没有失败；`status.pods` 是 0（从来没有写过 `status` 的新对象也是 0）；Pod 缓存里没有它的成员 Pod（见表下） | 没有 Pod 可扰动，ServingGroup controller 也还没有决定建。不要求已观测。包括：没有 Pod 的空组；刚建出来的对象；PodGroup 建不出来而卡住的组 |
| 有成员 | 「没有成员」的否定 | — |
| 空组 | 各 Role 的副本数都是 0。按 `spec` 判断，与有没有 Pod 无关 | 用户有意把这个组缩成了零。它的 Ready 为真；有空组时基线不推进（4.2） |
| 可替换 | `live`；`ServingGroupRollingUpdate`；有成员；并且下面两条之一成立。(1) 版本不等于 T，序号不小于 P。(2) `RecreateRequired` 为真（第 9 节、5.5），不看版本，不看 P；B 读不到时序号小于 P 的不算 | 滚动要换掉的组。「有成员」管两条：`RecreateRequired` 为真而没有成员的组不算 |
| 受保护 | `live`；版本不等于 T 并且序号小于 P | 不因目标模板变化而被替换 |

**组的成员 Pod**：controller owner 的 UID 是这个组的 Pod，加上这个组待接管的 Pod（[03](03-state-machine.md) 第 3 节的三种：无主的成员 Pod、升级接管的存量 Pod、接管期间的无主存量）。五处用同一个集合、同一个函数：`status.observedPods` 的摘要；ServingGroup controller 为哪些 Pod 的事件入队；「没有成员」里 Pod 缓存的那一条；Pod 一级的未就绪；deletion cost 之和（4.4）。带着这个组名标签而不是成员的 Pod（抄了标签的副本，同名的上一代对象留下的 Pod）不进任何一处。

**组的版本**取 `spec.revision`。新建的目标组在 `status.revision` 还空着的时候就是目标。两个例外：`RoleRollingUpdate` 下不做版本分类（4.6）；从 Role 模式切到 ServingGroup 模式之后，`spec.revision` 为空的组取 `status.revision`，仍然为空（组内的版本不齐）时按「版本不等于 T」算：序号不小于 P 的可替换，序号小于 P 的受保护（4.7）。

「版本等于」指 ControllerRevision 的名字相同，或者经 ControllerRevision 解出的模板在语义上相同；目标版本的解析先沿用已有的身份。这是今天已有的机制，本设计不改算法。

| 计数 | 定义 |
| --- | --- |
| `active` | 全部组对象，包括终止中的和失败的 |
| `live` | 运行的组 |
| `regular` | 正式的组，包括终止中的和失败的 |
| `regularLive` | 正式的、运行的组。名下还有无主的成员 Pod 没收养完的不算（5.5） |
| `ready` | `live` 里**确认就绪**的 |
| `notReadyTarget` | `live` 里版本等于 T、没有确认就绪、不是可替换的 |
| `minAvailable` | max(0, N − U) |
| `maxScaleDown` | max(0, `live` − `minAvailable` − `notReadyTarget`) |
| `maxHealthyScaleDown` | max(0, `ready` − `minAvailable`) |
| 补得上（对一个正式的组） | 在工作副本上把它拿掉，把这一轮已经选中的删除、终止中的和失败的组也拿掉，再按 G0、G4 走到不动：正式的组的个数不比不删它时少。这里的 G4 不看 `active` < N + S：那是当下的容量上限，替换下来的组和别的在途的组消失之后会放开。只用在 G3c 和「有滚动要做」上 |
| 有滚动要做 | 存在可替换并且补得上的正式的组。按原地执行的替换不看补得上 |
| 缩容中 | `regular` > N |

**就绪的四种说法。**

| | 定义 | 用在 |
| --- | --- | --- |
| 确认就绪 | 已观测（A1）；`status` 新鲜（A7）；Ready 为真 | 两个额度、回收 surge 时的下限、G6；替换时「健康的」候选 |
| 确认未就绪 | 已观测；`status` 新鲜；Ready 不为真 | 替换时「未就绪的」候选；回收时可以直接回收的 surge 组 |
| Pod 一级的未就绪 | ModelServing controller 在 Pod 缓存里看这个组的成员 Pod：组期望有 Pod（`spec.expectedPods` 大于 0）而一个都没有；或者有不就绪的；或者有在终止的 | 只用在缩容和回收的次序上（4.4）。次序是偏好，用看得到的最新的事实，与今天单个 controller 直接读 Pod 时相同。它不与任何个数比，不进额度 |
| 报告的就绪 | 最后一次写上的 Ready，不问是否已观测，不问新不新鲜 | 只用在对外的 `status.availableReplicas` 和三个 condition 上（[03](03-state-machine.md) 第 6 节）。对外的可用数不应当因为 `spec` 下发而抖动 |

空组没有期望的实例，Ready 为真，与今天相同；它计入 `ready` 和对外的可用数。

**复核**（A7）。ServingGroup controller 每次写 `status`，一并写上 `status.observedPods`：这份 `status` 是按哪些成员 Pod 算的。它是一个摘要，不少于 64 位，对每个成员 Pod 取 UID 和**判定用到的字段**：`deletionTimestamp`、`phase`、Ready 的值和 `lastTransitionTime`、各容器（含 init 容器）的重启次数、controller owner 的 UID、判定用到的标签和注解（revision、代际号、原地更新的进度和重启基准、Role 一级的 surge 标记）。成员的判定只读这个投影：实现上先取投影、再判定，所以摘要按构造覆盖了算 `status` 时读的全部字段。它不取 resourceVersion，不含 `pod-deletion-cost`，也不含别的组件写的注解和 condition 的 message：这些变了不影响 `status` 的结论，不应当让组不新鲜。

ModelServing controller 对同一个集合、用自己的 Pod 缓存、同一个函数算同一个摘要。两个 controller 在同一个进程里，用同一个 Pod informer（3.3）。ServingGroup controller 的摘要按它这一轮算 `status` 用的那批 Pod 算：没有直读的轮次是缓存里的，直读的轮次（6.3）是直读到的。

摘要相同，这份 `status` 是**新鲜的**：它是按 ModelServing controller 现在看到的这些 Pod 算出来的，Ready 可以照着用。摘要不同，`status` 落后于 Pod（或者 ServingGroup controller 这一轮用的是直读，比缓存新）：这个组**按未观测算**，直到两边相同。占位置，不进 `ready`，不是替换的候选，回收时不当成未就绪的直接删。通常只差 ServingGroup controller 的一次 status 写。

ModelServing controller 不另算一遍「这个组就绪没有」。就绪只有一个定义，在 ServingGroup controller 里；这里只判断那个结论过期没有。模板改了每组的 Pod 数、Role 在缩容、有 Role 一级的 surge 实例、成员在原地更新，这些时候都没有「两边的定义对不上」的问题。

它的作用是两个方向的。一个组刚丢了成员、`status` 还没写出来时，Pod 变了，摘要对不上，健康额度里已经没有它。一个组刚恢复、`status` 还写着未就绪时，它不会被当成未就绪的旧组、不占健康额度地删掉。ModelServing controller 看到的滞后与今天单个 controller 直接读 Pod 时相同。空组没有 Pod，两边都是空集合的摘要。

代价：成员 Pod 的判定用到的字段每变一次，摘要变一次，ServingGroup controller 要写一次 `status`，这个组在这次写之前按未观测算。这些字段变的时候 `status` 的内容多数本来就要变（就绪变了、成员来了或者走了）；多出来的是只变了重启次数、`phase` 的那几次。Pod 启动的过程中它的 `status` 要写几次，这期间组本来就没有就绪；稳态下不写。别的组件改 Pod 上无关的字段（binpack 周期性地写 `pod-deletion-cost`；调度器改调度不上的 Pod 的 condition 的 message）不让摘要变。`status.deletionCost` 只给人看，在下一次本来就要发的 status 写里更新，可以落后。没有测量。

`status` 写不出来的组保持原样：未观测，或者一直不新鲜。ServingGroup controller 出错的轮次也照实写（A1），所以这只在 status 的写本身一直失败时出现。ModelServing controller 记着它从什么时候起看到一个组未观测或者不新鲜（在内存里，只用于报告，3.2）；超过期限时报 `RolloutBlocked`，reason 是 `GroupNotObserved`（[03](03-state-machine.md) 第 6 节）。

算式与契约附录 B 相同。契约里的「在途额度」不单独记：发出的删除立刻表现为组进入终止中，它已经不在 `live` 里；新建的目标组在 `notReadyTarget` 里。`notReadyTarget` 只数目标版本的组，不数未就绪的受保护的组（契约的 RU-B06）。有一种情形与契约的字面不同：替换的容量落在受保护的空洞上、按基线建出来时，它不是目标版本，不在 `notReadyTarget` 里，清理额度不等它就绪（契约 API 参考 2.3 要求等）。这是 v2 的算式本来就有的，只影响本来就未就绪的旧组，就绪数不因此再降。要声明（[04](04-behavior-expectations.md) 第 11 节）。

### 4.2 一轮的计划

计划是纯函数。它在一份工作副本上逐个动作迭代：每选中一个动作，就把效果记进副本（删除的组变成终止中，创建的组是一个未就绪的新组，转正的组去掉标记），重新分类和计数，再选下一个。

| 步 | 条件 | 动作 |
| --- | --- | --- |
| S1′ 找回 | 有无主的成员 Pod、而没有同名组对象的序号（[5.5](#55-有人直接删了一个组)） | 建对象。先于其余各步，不受「正式的组少于 N」限制 |
| G0 转正 | `ServingGroupRollingUpdate`：带 surge 注解的组，序号小于 N，并且 `regular` < N。`RoleRollingUpdate`：凡是带注解的组 | 摘掉注解。序号小的先。对象、UID、Pod 都不动。终止中的、失败的组同样适用 |
| G1 删失败的组 | Failed 和 Drained 都为真，并且 `observedPods` 与 Pod 缓存相同（Drained 说的是现在这些 Pod） | 前台级联删除。不重新判断，策略中途改了也删 |
| G2 缩容 | `regularLive` > N | 按 [4.4](#44-次序) 的缩容次序删一个，重复到 `regularLive` = N。不占滚动预算。可以留下稀疏的序号 |
| G2′ 没有成员的组 | (a) 没有成员，正式，序号不小于 N；没有别的删除在途，也没有还没补上的正式位置（没有终止中的、失败的组，这一轮前面没有选中删除，正式的组恰好 N 个）；把它拿掉之后 G4 现在就建得出一个正式的组（`active` 小于 N + S，小于 N 的最低空闲序号存在并且建得出来）。(b) 其余没有成员的组（范围外的正式的组除外），版本不等于「按它的序号现在新建会用的版本」 | (a) 删掉，一轮至多一个，不占预算；它腾出的容量由 G4 在最低的空洞上补。(b) 把 `spec.revision` 改成那个版本，PATCH 带 UID 和 resourceVersion；不删对象，不占预算 |
| G4 补正式的位置 | `regular` < N，并且 `active` < N + S | 在小于 N 的最低空闲序号上建一个正式的组，重复。版本：序号小于 P 并且 B ≠ T 时用 B，否则用 T |
| G3a 建 surge | 有按重建执行的滚动要做；不在缩容中；`active` < N + S | 在不小于 N 的最低空闲序号上建一个组，版本 T，带 surge 注解。重复到 `active` = N + S |
| G3b 回收 surge | 候选是 `live` 的 surge 组。没有滚动要做时全部回收；有滚动要做而 `live` > N + S 时，回收到 `live` 不超过 N + S 为止 | 按 4.4 的回收次序删。确认未就绪的直接删。其余的（确认就绪、不新鲜、未观测），只在删掉之后 `ready` 不低于 `minAvailable` 时删 |
| G3c 替换 | 有滚动要做；不在缩容中；只在 `ServingGroupRollingUpdate` 下 | 候选是可替换的、确认就绪或者确认未就绪的组，正式的和 surge 的都算。确认未就绪的：`maxScaleDown` > 0 时删一个。没有未就绪的了，再看确认就绪的：两个额度都大于 0 时删一个。每删一个重新算。**正式的组另要补得上**（4.1）；补不上的候选跳过，接着看下一个 |
| G5 下发 | 组的 `spec` 与期望不同 | PATCH（A2） |
| G6 推进基线 | P = 0，N > 0，`active` = N，没有带 surge 注解的组，没有空组，全部是目标、确认就绪 | `status.currentRevision` := T |

**替换是删除；补位由 G4 做。**G3c 删，组消失之后它的序号空出来，G4 在小于 N 的最低空闲序号上建。原序号小于 N 并且更低处没有空洞时，这就是在原序号上重建。名字相同的对象只能在旧对象消失之后建（4.5）。删之前先看补不补得上（4.1）：删了补不上，就少一个组。补不上有两种原因：替代的容量轮到一个得按 B 建的序号而 B 读不到；小于 N 的序号除了外来的对象占着的都满了。这个判断按整轮算，所以一个空闲序号只算给一处用：两个范围外的组指望同一个洞时，只删一个。序号小于 N 的带标记的组也是一种着落：删掉一个正式的组之后它会被转正。补不上的候选跳过，接着看下一个；候选都补不上时报 `BaselineUnknown` 或 `OrdinalOccupied`。可替换的正式的组都补不上时不算有滚动要做：不为它们建 surge，已有的 surge 照「没有滚动要做」回收。surge 是为换得动的旧组建的，不用来顶建不出来的正式位置；否则全保护的停点上基线读不到时，会一直留着一个目标版本的组。代价：正式的位置本来就缺着、剩下的旧组又都补不上时，没有多出来的组顶容量。

几点说明，每条后面是契约里对应的场景。

- **转正（D1）。**滚动途中 N 调大，已经在跑的 surge 组如果序号落进了新的正式范围，就留下来当正式的组，不删掉重建（SG-C03）。条件有两个：序号小于 N；正式的组还不够 N 个。只有总数等于 N 不够：序号不在正式范围里的 surge 组仍然是临时的，滚动做完照样回收（SG-S04）。转正是一次元数据的写，不改 `generation`，组不会因此变成未观测。转正之后它与本来就是正式的组没有任何区别。第二个条件在规则自己走得到的状态里总是成立（前提是一个写失败之后这一轮后面的写都不发，见下）；它只在接管来的、找回来的状态里起作用，对应契约 0.6 的「能承接正式容量」。
- **缩容中不替换，也不建 surge。**缩容删掉的组在消失之前还算在 `regular` 里，`regular` 大于 N，这期间不开始替换。对应契约 A.4「先执行缩容选择并等待本轮计划删除完成」。runner 的五个停点用例在这条规则下与期望相同，去掉之后都出不来（[05](05-inference-verification.md) 第 2 节）。代价：N 调小之后，一个卡在终止中的正式的组会一直挡住滚动；这时报 `RolloutBlocked`，reason 是 `WaitingForDeletion`。
- **补最低的空洞。**新增的正式容量总是落在小于 N 的最低空闲序号上。按 deletion cost 缩容留下的洞（`{0, 3}`）在之后的扩容里先被补上，一次补一个容量，不为排齐序号而删除健康的组（SG-S01、SG-S03）。序号不小于 N 的过期的正式组被替换时，它的替代容量也落在最低的空洞上，版本按新位置的序号选（SG-S02、SG-P11）。
- **稀疏布局下不保证原位。**范围内的组下方有空洞时，它被替换或整组恢复之后，替代的容量落在那个更低的洞上：`{1, 3}`、N = 2，替换 `sg-1` 之后是 `{0, 3}`。洞的个数不增加，名字变了。契约 A.3 第 3 条是「优先在原 ordinal 重建」。这是 v2 的规则本来就有的，契约的 35 个场景碰不到，要声明。原因是对象消失之后，单凭组的集合分不出「刚腾出来的序号」和「以前缩容留下的洞」。
- **surge 只在有滚动要做时存在。**被 partition 挡住、没有可替换的正式组时不建 surge；滚动做完、或者 P 调大到没有可替换的组时回收。回收一个就绪的 surge 组要看下限：替代的容量就绪之前不删它（SG-C16）。只有确认未就绪的 surge 组不看下限：`spec` 刚下发时一个在服务的 surge 组是未观测，它不算进 `ready`，但也不能当成未就绪的直接回收；这时要求 `ready` 本身不低于下限。失败的 surge 组不由 G3b 删，只由 G1 删：回收不能绕过「等故障成员消失」。
- **容量按 `active` 算。**终止中的和失败的组在消失之前占着位置和名字。规则自己的创建（G4、G3a）不会让 `active` 超过 N + S（4.5）。例外有两处：找回（S1′）建的对象不看这个上限（5.5）；两个进程看到的 N 不同时是各自的 N + S（6.4）。
- **未观测的组**（A1）和 `status` 不新鲜的组（A7）占位置、不算确认的就绪。它们不是替换的候选：`spec` 刚下发的那一刻全部组都是未观测，其中多数其实是健康的。缩容和回收不等观测，次序按 Pod 一级的就绪排。
- **没有成员的组。**判定看两边：组的 `status.pods` 是 0，Pod 缓存里没有它的成员 Pod。它没有 Pod 可扰动，它的版本只决定下一次建 Pod 时用什么，所以按「现在新建」的规则重新指派：序号小于 P 并且 B ≠ T 用 B，否则用 T；B 读不到并且序号小于 P 时不改。这对应契约的两句话：空组「后续非空扩容视为新建」（0.6）；「尚未创建的组直接使用最新适用版本」（A.4）。
  - **改版本和建 Pod 不会交错。**改版本的 PATCH 以组的 resourceVersion 为条件；ServingGroup controller 建第一个 Pod 之前的那次 status 写（5.2 的 L8）也是，它的每一次 status 写都是。PATCH 成功，说明从读到写之间这个对象没有被写过，也就没有 Pod 被建出来；status 写先到，PATCH 被拒绝，重算时这个组已经有成员。不会一边在按旧版本建 Pod、另一边在改它的版本。
  - **「尚未创建」和「已开始创建」的界**是 L8 的那次写：在 PodGroup 建成之后、第一个 Pod 之前。刚建出来的对象、PodGroup 建不出来而卡住的组是没有成员的组，目标变了直接改版本，放开之后按新版本起。Pod 的创建被拒绝的组已经写过 `pods`，是有成员的旧组，按预算替换（契约附录 B.2）。实例恢复清空了组里全部 Pod、正要重建的组也不算没有成员（L8），它按组自己的版本重建。
  - **范围外的没有成员的正式的组**（序号不小于 N）删掉，由 G4 补到低位。契约说空组「不保存旧稀疏序号」；刚建出来的、PodGroup 卡住的组还没有开始创建，同样按「新的正式容量落在最低的空洞上」处理。这是整理，不是容量上的需要，条件取最保守的：没有别的删除在途，没有还没补上的正式位置，并且把它拿掉之后 G4 现在就建得出来。低位的序号被外来的对象占着、要用的 B 读不到、或者 `active` 已经到了 N + S（带标记的组比 S 多而下限不许回收）时，删掉它会少一个组，所以留着。它没有 Pod，删掉立刻消失，补位等不到别的组消失，所以这里看当下的上限；替换（G3c）用的「补得上」不看。一轮至多做一个，前一个腾出来的位置补上之后才做下一个：两个范围外的组指望同一个洞时只删一个。等着的这段时间不改它的版本：它要去的是低位，那里的版本按低位的序号选。这样终局不取决于它当时有没有 Pod：有 Pod 的走替换，替代的容量同样落在低位。
  - **空组。**带 surge 注解的空组不算有滚动要做的理由，由 G3b 回收。范围内的空组对象留着。空组的 Ready 为真，但有空组时基线不推进（G6）：契约 0.6 的「全零计数不能使其提前推进」。
- **G6 是 v2 的 R17**，不变。灰度停点上基线不推进；还有 surge 的组没回收时也不推进。
- **有效的更新方式是原地时**（`ServingGroupRollingUpdate`、`updateMethod: InPlace`，并且 U 大于 0；U 为 0 时按重建执行、照常用 surge，与今天和 007 的 05 的规范化相同），G3c 的「删一个」换成「把这个组的 `spec.revision` 改成 T」，由 ServingGroup controller 在组内原地更新。这个 PATCH 带 UID 和 resourceVersion，前置条件与删除一样强。候选、次序和两个额度都不变。这时没有对象被删，不需要 surge：G3a 的理由只数按重建执行的可替换的组。不能原地更新的组，ServingGroup controller 把 `RecreateRequired` 写成真，它回到删除重建（第 9 节）；这样的组算不算建 surge 的理由，是 005 的待决项 O4，这里默认算（不算的话，U = 0 时收养来的这种组没有 surge 就没有健康额度，永远换不掉）。
- **一轮可以发多个写。**计划在工作副本上得出一批写，逐个发出。一个写失败（被拒绝，或者出错），这一轮后面的创建、删除、摘注解和改版本都不发：它们是在「前面的都成功了」的工作副本上算出来的。两种不算在内。创建撞名字不是失败（4.5）。G5 的下发照发：每个组期望的 `spec` 不取决于这一轮建了谁、删了谁，前面有一个写一直被拒绝时（配额、准入 webhook），Role 的副本数和 `expectedPods` 仍然要到得了已有的组。有写失败的轮次发事件，并把哪个写失败记在当时为真的那个 condition 的 message 里。任何一步停下，下一轮按当时的对象重新得出同一份或更保守的计划。
- **发之前按直读重算。**上表按缓存算出来有写要发时，直读这个 ModelServing 和它名下的组，按读到的重新走一遍上表，发重算出来的写（[6.3](#63-直读和复核)）。

### 4.3 partition：绝对序号

partition 保护序号 `[0, P)`，与今天、与契约、与 Role 一级相同。

| 状态或动作 | 规则 |
| --- | --- |
| 现存的组，序号小于 P | 不因目标模板变化而被替换。保护的是固定的序号 |
| 在序号小于 P 的位置上新建或重建 | 用基线 B。B 读不到时不建，等待（4.8）；也不跳到更高的序号上去建，免得留下新的洞 |
| 在序号不小于 P 的位置上新建或重建 | 用目标 T |
| 已经是目标版本的组因为 P 调大而落进保护区 | 保留，不为回到旧版本而重建。它之后被整组重建时按上面两行 |
| 上一轮没滚完又提交新版本 | 序号小于 P 的组不论是哪个版本都不动；基线不变，直到全部滚完 |
| 显式缩容 | 不受 partition 限制，可以删受保护的组 |
| 受保护的组未就绪 | 占可用性的额度（D2） |
| B 读不到 | 保护照常。序号小于 P 的组即使 `RecreateRequired` 为真也不替换：删掉之后建不出来。替代的容量得落在序号小于 P 的洞上的组同样不替换（4.2 的 G3c） |

组一级和 Role 一级的 `partition` 是同一种语义。API 注释和用户文档里「按序号保护」的说法不用改。

### 4.4 次序

| 选择 | 次序（先满足的先被选中） |
| --- | --- |
| 缩容删谁（G2） | 正式的、运行的组里：Pod 一级未就绪的（4.1）；deletion cost 低的；序号高的。deletion cost 是成员 Pod 的注解之和，从 Pod 缓存现算，在决定的那一刻读，与今天相同。今天按组名标签求和（走读，`binpack_scaledown.go:158-179`），这里按成员 Pod：带着标签而不是成员的 Pod（调试用的副本）的 cost 不计入，只影响缩容的次序。partition 不改变这个次序 |
| 替换删谁（G3c） | 确认未就绪的可替换组；确认就绪的可替换组。同类里序号高的先 |
| 回收哪个 surge（G3b） | Pod 一级未就绪的；序号高的 |
| 正式的位置建在哪（G4） | 小于 N 的最低空闲序号 |
| surge 建在哪（G3a） | 不小于 N 的最低空闲序号 |
| 先转正哪个（G0） | 序号低的 |

失败的组不在缩容和回收的候选里，只由 G1 删：不能绕过「等故障成员消失」。名下还有无主的成员 Pod 没收养完的组（5.5）也不是缩容、回收、替换的候选：它这时看起来未就绪，删掉之后又会被找回。它计入 `active` 和 `regular`，不计入 `regularLive`：缩容等它收养完再按上面的次序选。

**空闲序号**只按名字判断：命名空间里没有名字是 `<ModelServing 名>-<序号>` 的 ServingGroup 对象，不论 owner 是谁。残留的 Pod 不影响对象能不能建，它们由成员一级处理（4.5、5.5）。

### 4.5 名字确定：创建幂等，同名重建

**创建是幂等的。**组的名字由序号决定，序号由规则从对象的快照算出。重复的创建撞名字，apiserver 返回 AlreadyExists。

| 情形 | 发生什么 |
| --- | --- |
| 刚建的组还没出现在缓存里，又计划了一轮 | 直读里已经有它（6.3），不会再建。直读也旧的话，算出同一个序号，AlreadyExists |
| 两个进程同时在写 | 算出相同的名字，只有一边建成 |
| 原进程已经退出，它的创建请求才落地 | 落地的就是新进程要建的那个名字 |
| 缓存里的组其实已经被删掉 | 直读里没有它：这个序号空着，照规则建 |

规则自己的创建（G4、G3a）因此不会让 `active` 超过 N + S，换主之后也一样。两个进程看到的 N 不同时是各自的 N + S（6.4）；找回是另一个例外（5.5）。所以不需要在内存里登记发出过哪些创建。这是原生 controller 的做法：StatefulSet 的 Pod 名字确定，controller 里没有 expectations；ReplicaSet 的 Pod 名字随机，才要在内存里记「发出了几个创建，还没看到」。来源见 [01](01-design-rationale.md) 第 5 节。

**撞名字不等于「是我的」。**AlreadyExists 之后直读那个对象，核对 controller owner 的 UID：

| 那个对象的 owner | 处理 |
| --- | --- |
| 这个 ModelServing（UID 相同） | 已经有了。这一轮到此为止，下一轮按直读重新计划，不假定它的内容是这一轮想建的样子（标记、版本可以不同，之后由 G0、G2′、G3c 处理） |
| 别的 UID，或者没有 owner | **外来的同名对象。**这个序号被占着：不建，不删它，不收为己有，不计入任何计数。这一轮把这个序号记成被占着，在已经发出的写的基础上重算剩下的。发事件，`RolloutBlocked` 的 reason 是 `OrdinalOccupied`。它的 owner 是一个已经不存在的 UID 时（同名的 ModelServing 被后台删除又重建），垃圾回收器会删掉它，等待有尽头；没有 owner 的（用 orphan 方式删 ModelServing 留下的）要人删除 |

撞名字不算写失败，不触发「这一轮后面的写都不发」（4.2）。多数时候不会去撞：外来的对象占着哪些序号，除了直读到的，还用缓存里看到的（informer 里有命名空间里全部的 ServingGroup）。

属于这个 ModelServing、而名字解析不出序号的 ServingGroup 不参与计数，发事件报告。

**同名重建。**同一个名字先后是两个对象。名字是位置，UID 是这一代。

| 担心 | 为什么不会 |
| --- | --- |
| 新旧两代的 Pod 混在一起 | 组的删除是前台级联：对象在名下的 Pod、PodGroup、Service 都消失之前一直在，名字一直被占着。垃圾回收器漏掉的 Pod（它不知道的、并发建出来的）owner 是旧 UID，对新对象是「外来」：不数它，不删它，它占着的成员名字等它被回收（L7） |
| 缓存里是旧对象，对它发的写落在新对象上 | 每个写带 UID；删除另带 resourceVersion（6.2） |
| 汇总时把旧一代的 Pod 算进新对象 | 成员分类、`status` 的每个汇总字段、`scale` 子资源、驱逐 webhook 的计数，都只数 controller owner 的 UID 等于这个对象的 Pod |
| 进程里以名字为键的东西被带到下一代 | 进程里仅有的两项（3.2）以 UID 为键。队列的退避以名字为键：对象换了 UID 时 `Forget`。一次 Update 事件里新旧对象的 UID 不同，按「删除旧的，新增新的」处理 |
| 旧一代的账本被新一代用上 | 账本在对象的 `status` 里，新对象的账本是零值。代际号只在一个 UID 之内有意义；传给钩子的身份带组的 UID（第 7 节） |
| 用户用后台方式删组，对象先消失、Pod 还在 | 同名的新对象可以立刻建出来，占一个 `active`。它的成员位置被 owner 是旧 UID 的 Pod 占着，等它们被回收。不会有两批同名的 Pod，容量不超过上限 |
| PodGroup、Service 同名 | 它们随组级联删除，新一代重建。创建撞名字、或者「内容不同就更新」之前，核对 owner 的 UID。今天的实现已经是同名的 PodGroup 先删后建；前台级联比今天严。Volcano 对同名 PodGroup 先删后建的反应没有验证 |
| 组的删除事件丢了，缓存里一直是终止中 | 终止中的组超过等待期限时直读一次 |

先例里同名重建出过的问题和这里的对照，见 [01](01-design-rationale.md) 第 5 节。

**名字被占着是一种新的停法。**

| 情形 | 后果 | 报告 |
| --- | --- | --- |
| 一个组卡在终止中（有人加了 finalizer；垃圾回收器停了） | 它的序号不能重建。它是正式的组时，N 调小之后还会挡住滚动。带 S 的滚动照常做完，做完之后 surge 回收，U ≥ 1 时一直少一个，直到它消失 | `WaitingForDeletion`，message 里写明序号 |
| 外来的同名对象 | 这个序号建不出来。序号上有无主的成员 Pod 时也不找回（5.5） | `OrdinalOccupied` |
| 成员的名字被不属于这个组的 Pod 占着（后台删组之后上一代的 Pod 卡在终止中；外来的同名 Pod） | 这个成员建不出来，组一直未就绪（L7） | 组上 `ReconcileFailed`，message 写明哪个 Pod；ModelServing 上是 `TargetNotReady` |
| 同名的 PodGroup、Service 不属于这个组 | 不改它，不删它。组停在创建中 | 同上 |

第一种与今天一个 Pod 卡在终止中相同。本目录的上一版（随机名字）在带 S 的滚动途中可以换一个名字补建，这一版不能。另外两种不是名字的问题、但同样会一直停住的状态：组的 `status` 一直写不出来（4.1）；待收养一直收养不完（5.5）。两种都报 `GroupNotObserved`。

### 4.6 `RoleRollingUpdate` 下的组一级

顶层的 `maxUnavailable`、`maxSurge`、`partition` 在这个模式下不生效（契约 0.2）。组一级：

- 不做版本分类，没有可替换的组，不建 surge。只走 S1′、G0、G1、G2、G2′(a)、G4、G5、G6。
- **带 surge 注解的组**（从 ServingGroup 模式切过来时留下的）：G0 无条件摘掉注解，它成为正式的组；正式的组多于 N 时由 G2 缩掉。当前实现在这个模式下同样不看组一级的标记。
- 期望的 `spec` 里没有 `revision`：G5 把它清空。这样切回 ServingGroup 模式时不会拿着一个早就过时的 `spec.revision` 去分类（4.7）。
- 新组的 `spec` 带目标和基线两个版本。受 Role partition 保护的实例按基线建。G4 不用顶层的 P。
- 模板变化通过 G5 写进每个组的 `spec.targetRevision`，由 ServingGroup controller 在组内按 Role 一级的规则滚动。
- G6 的条件按各组报告的 `status.revision` 写：全部组的 `status.revision` 等于 T、确认就绪，没有空组，并且每个 Role 的 partition 都是 0。推进之后 G5 把新的基线写回每个组。

反过来，`ServingGroupRollingUpdate` 下 Role 一级不建、不回收 surge；Pod 上留着的 Role 一级的标记由 ServingGroup controller 摘掉，多出来的实例按 Role 的缩容处理。

### 4.7 运行中切换滚动模式

契约 2.12：合法的运行中切换只保证最终收敛，不保证过渡过程的预算。本设计里切换就是「分类规则换了一套」。

切到 Role 模式：可替换的组不再被整组替换，改由组内滚动；已有的 surge 组被摘掉注解，多出来的按缩容删掉（4.6）。

切到 ServingGroup 模式：Role 模式下的组 `spec.revision` 是空的（新建的本来就没有，原先在 ServingGroup 模式下建的被 G5 清空了），这样的组取 `status.revision` 作版本；`status.revision` 也为空（组内滚到一半，版本不齐）的，序号不小于 P 时算可替换，按预算替换；序号小于 P 时不动（4.3）。G5 把非空的 `status.revision` 填进 `spec.revision`，之后按一般情形处理。

没有需要迁移的中间状态。

### 4.8 基线

B 的来源是 `ModelServing.status.currentRevision`。它是下一轮的输入，这一点今天就是如此。

| 情形 | 取值 |
| --- | --- |
| `status.currentRevision` 有值，对应的 ControllerRevision 在 | 用它 |
| 为空，没有任何组 | 取 T |
| 为空，有组 | 从各组的 `spec.baselineRevision` 读回（A4） |
| 读不回来，或者对应的 ControllerRevision 不在 | B 未知。**保护照常**（保护看的是序号和「版本不是 T」，不看 B）；要用 B 建组、要把没有成员的组改成 B、要替换序号小于 P 的组、要替换一个替代的容量得按 B 建的组，这四种都不做，报告（`BaselineUnknown`），等待 |

B 未知时不猜成最新版本，这是契约 A.4 的「历史模板不可确认时安全等待」。B 被错误地写成 T（旧客户端整体改写 `status`）时，序号小于 P 的位置会按 T 重建，这一种挡不住；A4 的副本让它可以被发现和修回。

### 4.9 两层是同一套规则

组一级（ModelServing controller，单位是 ServingGroup）和 Role 一级（ServingGroup controller，单位是一个组里某种 Role 的实例）用同一套按序号的规则。计划可以是同一个函数，输入是「一组带序号的单元」。

| 规则 | 两层相同 |
| --- | --- |
| 额度 | `minAvailable`、`maxScaleDown`、`maxHealthyScaleDown`，算式相同 |
| partition | 绝对序号 |
| 候选次序 | 未就绪的先，再健康的；同类里序号高的先 |
| 缩容次序 | 未就绪、deletion cost 低、序号高 |
| surge | 建在不小于 N 的最低空闲序号上，带标记；没有滚动要做时回收 |
| 转正 | 序号小于 N，并且正式的单元少于 N |
| 补建 | 小于 N 的最低空闲序号；版本按序号和 partition |
| 先缩容再滚动 | 相同 |
| 替换 | 删掉，等它消失，由补建的规则补 |
| 非生效的那一层 | 不建、不回收 surge；留下的标记摘掉 |

不同的只有载体，因为组是对象，Role 实例不是：

| | 组一级 | Role 一级 |
| --- | --- | --- |
| 单元是什么 | ServingGroup 对象。没有 Pod 也存在 | 账本里的一项加一组 Pod |
| 就绪从哪里读 | 组的 `status`，要是按现在这些 Pod 算的才算数（A7）。有「未观测」 | Pod 的缓存 |
| surge 标记在哪 | 组对象的注解，一次写 | 实例的每个 Pod 上的注解，沿用 v2 |
| 「它消失了」指什么 | 对象没有了 | 没有代际号不大于 g 的 Pod |
| 补建发生在什么时候 | 对象消失之后的下一轮，版本按那时的 P、B、T | 清空之后同一轮写 Creating(g + 1) |
| 新旧两代靠什么分开 | 对象的 UID | 代际号（5.2） |
| 单元坏了怎样表示 | Failed condition，由 ModelServing controller 删对象 | 账本的 Draining，由 ServingGroup controller 删 Pod |
| 创建的幂等靠什么 | 组的名字 | Pod 的名字 |
| 「缩容中」数什么 | 正式的组，含终止中的和失败的 | 正式的实例，含清空中的 |
| 空的单元 | 空组是一个存在的对象，有版本 | 副本数是 0 时没有实例 |

Role 一级的 surge 标记仍然逐个打在 Pod 上，是「Role 实例不做成对象」（007 的 K5）的直接后果。把它也挪到账本里可以进一步统一，本设计没有做：它今天就在 Pod 上，不用迁移；挪进账本之后 `status` 被清空会让临时的实例变成正式的。

同一个注解 key 在三处出现，读的地方必须比对值：组对象上是组一级的标记；Pod 上值是 `role` 的是 Role 一级的标记；Pod 上值是 `serving-group` 的是组一级标记的副本（2.3）。

## 5. Role 一级

### 5.1 沿用的规则

ServingGroup controller 在一个组里面做的事，规则沿用 v2，没有变：

- Role 实例按序号。Role 一级的 N、U、S、P、surge 标记、转正、回收、候选次序（v2 的 04 第 5 节的 A2–A8；v2 指 `docs/proposal/modelserving-rollout-final/`）。
- roleCoordination（v2 的 04 第 8 节）。
- 原地更新和它的接口点（007 的 05 第 4 节）。
- 成员状态的判定（终态、重启故障、grace、`-1`）和 D10、D13、D14。

这套规则与组一级的是同一套（4.9）。变了的有五样：恢复的证据从内存和 Pod 上的标记换成账本（5.2）；重启故障的起点从内存挪到账本里，这改了 D9（5.3）；转正多一个在正常运行时总是成立的条件，与组一级相同（4.9）；两层之间的衔接规则变成对象的边界（组在终止中或已失败，它里面的一切就停）；钩子的调用者（第 7 节）。

Role 的 `replicas` 扩缩是独立的数量操作（契约 2.8）：G5 把新的数量写进所有不在终止中的组，各组自己扩缩，不按组一级的滚动预算分批。G5 只在内容不同时才写；内容没变的组不会因为 ModelServing 的 `generation` 变了而被改写、变成未观测。

### 5.2 账本

`status.roleInstances[]` 每个 Role 实例一项。每个实例都记，不论它有几个 Pod。

| 值 | 含义 |
| --- | --- |
| 没有这一项，或 Creating | 创建中。**零值** |
| Established | 已建成：全部成员都在缓存里看到过 |
| Draining | 清空中：这个实例代际号不大于 g 的 Pod 要全部删掉。清空之后建不建，由当时的 `spec` 决定 |

规则：

| # | 规则 | 挡住什么 |
| --- | --- | --- |
| L1 | **先写账本，再删 Pod。**ServingGroup controller 发起的每一种实例删除（Role 缩容、按重建执行的滚动替换、实例恢复、回收多出来的实例）前面都有一次写 Draining 的 status 写。这次写被拒绝，本轮后面的删除和创建都不发 | 删到一半停下，新进程分不清是丢失还是计划内删除 |
| L2 | **零值是不会引起删除的状态。**`status` 被清空、被旧版本改写，后果都落在「只补」的一侧 | `status` 当输入的风险 |
| L3 | **Established 只在缓存里看到全部成员之后写**，不是创建请求返回成功之后 | 创建成功、还没被看到就被人删了 |
| L4 | **证据要直读确认。**用来写 Draining 或 Failed 的证据，写之前对 apiserver 读一次：缺失要读到 NotFound；终止中、终态要读回同一个 UID 并且仍然如此 | 新进程的 Pod 缓存比账本旧 |
| L5 | **代际号。**账本每一项带代际号 g；Pod 创建时带当时的 g。Draining(g) 只删代际号不大于 g 的 Pod。「清空了」指没有代际号不大于 g 的 Pod。清空之后的新代际是 max(g, 现有 Pod 的最大代际号) + 1。账本的 g 不小于现有 Pod 的最大代际号：观察到更大的，先把账本抬上去 | 读到旧的 Draining 的进程碰不到新一批的 Pod；`status` 被清空之后 Draining 不会卡住 |
| L6 | **两步删除。**Draining 下先删故障的成员；没有故障的成员、也没有正在终止的 Pod 之后，再删其余的 | D15 |
| L7 | **槽位被别人占着。**直读读到同名而不属于自己的 Pod：不删它；自己的那个成员按丢失处理；创建撞名字时停在 Creating 并报告。判断来自直读或者创建返回的 AlreadyExists，不凭缓存：缓存里有而直读没有的，按缺失创建。占着名字的 Pod 不是这个组的成员，它消失时没有这个组的事件：等待靠带退避的重试 | 外来 Pod；删除通知丢了、缓存里还留着的上一代的同名 Pod |
| L8 | **先写后建。**`status.pods` 是 0 的组，建第一个 Pod 之前先发一次 status 写：`pods` 写成要建的个数，带 resourceVersion。写成功才建。这次写在这个组的 PodGroup 建成之后（不用 PodGroup 的组没有这个条件）：PodGroup 建不出来的组 `pods` 仍然是 0。清空之后重建时，写 Creating(g + 1) 的那一次同样把要建的个数记进 `pods` | ModelServing controller 给没有成员的组改版本（4.2 的 G2′(b)）和这里建 Pod，以同一个对象的 resourceVersion 分出先后。改版本先到：这次 status 写被拒绝，下一轮按新版本建。status 写先到：改版本的 PATCH 被拒绝，重算时这个组已经有成员。「Pod 按旧版本建出来了，组的版本却被不占预算地改掉」不会出现 |

L8 没有模型，是推演。L4 和 L5 是 007 里「没有模型证据」的两条。这次各有一个小模型：去掉 L4，没有丢失也会写 Draining；去掉 L5，新一批的 Pod 被旧的 Draining 删掉，并且恢复反复进行（[05](05-inference-verification.md) 第 4 节）。

每个实例都记账的代价：实例建成时一次 status 写（一轮里多个实例的转换合成一次）。L8 让每个组从没有 Pod 到有 Pod 多一次 status 写。单 Pod 的实例丢了成员，从「一次创建」变成「一次直读、至多三次 status 写、一次创建」。007 对单 Pod 实例免记账；这里不免，因为 `ServingGroupRecreate` 下单 Pod 实例丢了成员同样要让整组失败，计划内删除在任何策略下都要能跨重启续做，并且状态机只有一套。

### 5.3 恢复

「丢失」指已建成的实例里有成员缺失或在终止。「故障」指成员是终态（`Failed`、`Succeeded`），或者重启故障满了 grace。两者的反应相同：

| `recoveryPolicy` | 丢失（已建成的实例） | 故障 | 创建中的实例里成员缺失 |
| --- | --- | --- | --- |
| `None` | 补这一个 | 终态：删这一个再补。重启故障：只报告 | 补 |
| `RoleRecreate` | 写 Draining，清空，重建这个实例 | 同左 | 补 |
| `ServingGroupRecreate` | 写组的 Failed | 同左 | 补 |

- 范围取动手那一刻的策略。要写 Draining、写 Failed 的轮次直读所属的 ModelServing，`recoveryPolicy` 和 grace 取它上面的最新值（6.3）；组的 `spec` 里的那一份是副本，用来触发 reconcile 和平时判断成员是不是故障。直读是动手之前的最后一道：副本说该恢复而 ModelServing 上已经改成不该（`None`、`-1`、更长的 grace），不恢复；范围改了，按新的范围。这个方向不用等下发。反过来不是：副本还是 `None`、`-1` 或者更长的 grace 时，ServingGroup controller 不会走到要写的那一步，也就不读；打开策略、缩短 grace 要等 G5 下发到组才生效，升级接管没做完时（G5 不做）不生效。
- 终态不等 grace（D13）；重启故障按 grace，`-1` 不触发。
- 最后一列：在网版本连续运行时已经是这样，它只在「本进程见过这个实例建成」之后才把缺失扩大成范围恢复，这个「见过」只在内存里。`main` 和契约在连续运行时不看建没建成，删除事件到了就按范围恢复；对它们这是一处变化（007 的 T14）。
- 「建成过」现在写在对象上：**controller 不在的时候发生的丢失，回来之后照样按范围恢复。**今天两个版本在这种情形下都只补一个。按用户的口径，升级 controller、主备切换、多副本时主被删或所在节点故障算在正常运行的容灾范围里，这些窗口里必须按范围处理；只有 controller 完全故障和被删除之后的变化允许只补。这一版从对象上分不出后两种，一律按范围恢复（[04](04-behavior-expectations.md) 第 11 节的 C2）。

**整个组的 Pod 都没有了**（`ServingGroupRollingUpdate` 下）。组期望有成员，账本里有建成过的实例，而一个成员 Pod 都没有了：属于这个组的没有，待接管的（[03](03-state-machine.md) 第 3 节）没有，终止中的也没有。ServingGroup controller 写 Failed，Drained 同时为真，不再创建。任何恢复策略下都这样。ModelServing controller 删掉对象，在这个序号上按序号和当时的 partition 重建（5.4）。这是 D12：整个单元没有了，按当时的规则重新建，不沿用它原来的版本。ServingGroup controller 自己不能这样做，因为按序号选版本要用的 P 和 B 不在它手里。每组只有一个 Pod 时，这个 Pod 每丢一次，组对象换一个；Pod 直接按正确的版本起。组的 PodGroup、Service 随对象一起重建。`RoleRollingUpdate` 下不这样做：版本按 Role 的 partition 选，ServingGroup controller 自己有。

**重启故障的起点在账本里。**`status.restartFaults[]` 每个在重启故障里的成员一项：

| 项 | 规则 |
| --- | --- |
| 什么时候写 | ServingGroup controller 第一次看到一个成员不 Ready 并且有计划外的容器重启，并且恢复策略生效（不是 `None`，grace 不是 `-1`）。搭在这一轮本来就要发的 status 写里：成员变成重启故障时组的 Ready 本来就要变 |
| 内容 | 起点（当时的时间）；这一段故障的身份：Pod 的 UID，写起点时这个 Pod 的 Ready condition 的 `lastTransitionTime`，原地更新的重启基准 |
| 读的时候 | 身份对不上就作废，按「第一次看到」重新写。`lastTransitionTime` 比相等，不比大小：它是 kubelet 的时钟，起点是 controller 的时钟 |
| 到期 | 每轮现算：现在减起点不小于当时的 grace |
| `None`、`-1` | 不记，已有的去掉。改回有限的 grace 之后第一次看到时写新的。「去掉」要 ServingGroup controller 在 `None`、`-1` 期间处理过这个组：controller 停着的时候改过去又改回来的，旧的起点还在，身份对得上就接着用 |
| 清理 | 每次 status 写按当时的成员重算。不再故障的、已经不存在的 Pod 的项不写 |
| 写不上 | 这一轮后面的删除不发（L1 的规则）。起点没写上就没有开始计时，下一轮重试 |

身份是必需的。没有它：成员坏了（写上起点），controller 停下，成员自己恢复，之后又坏，controller 回来看到「在坏」加「旧的起点」，立刻按范围重建，这一段故障的 grace 一秒都没有给。Ready 变回过一次，`lastTransitionTime` 就变了，身份对不上，重新计。`lastTransitionTime` 精确到秒：恢复、再坏和最初变坏落在同一秒里时身份对得上，起点延续；这要三次状态变化在同一秒，忽略。

换主、重启之后 grace 接着算，不重新计。起点是 controller 第一次看到的时间，「容忍重启故障多久」的含义不变。controller 不在的时候才开始的故障，起点是它回来之后第一次看到的时刻。已批准的 D9 是起点在内存里、每个新 leader 重新计满一个 grace，**这里改了它**，要批准（[04](04-behavior-expectations.md) 第 11 节的 N1）。不批准时起点留在内存里，其余不受影响。

### 5.4 整组恢复

组的 Failed 有两个来源：`ServingGroupRecreate` 下的丢失或故障；`ServingGroupRollingUpdate` 下整个组的 Pod 都没有了（5.3）。写 Failed 的这一次写是提交点，写之前直读确认（6.3）。

1. ServingGroup controller 此后在这个组里不创建，只删除故障的成员，并在没有故障成员、没有正在终止的 Pod 时把 Drained 写成真。
2. ModelServing controller 看到 Failed 和 Drained 都为真，并且这份 `status` 是按现在这些 Pod 算的（A7），前台级联删除这个组（G1）。
3. 组对象在 Pod 都消失之前一直在，一直计入 `active`，一直占着它的序号。
4. 组消失之后，G4 在小于 N 的最低空闲序号上重建。连续的序号集合下就是原来的序号：名字不变，UID 是新的。版本按序号和当时的 partition：序号小于 P 用基线，否则用目标（D12）。失败的组带 surge 注解时，它消失之后那个序号空出来，要不要再建一个 surge 由 G3a 按当时有没有滚动要做决定。

第 1 步的等待保留 D15：故障的成员停下并且消失之前，其余成员继续运行。

删除的机制选前台级联（007 的 K1）：kthena 不在时组照样删得掉，没有新的卡住方式。同名的新对象只能在旧对象消失之后建。

整组恢复之后组名、Pod 名都不变，与今天相同（稀疏布局下的例外见 4.2）。

### 5.5 有人直接删了一个组

| 做法 | 结果 |
| --- | --- |
| `kubectl delete servinggroup`（后台，默认） | 组对象立刻消失，垃圾回收器随后删它的 Pod。这个序号空出来，规则在上面建同名的新对象；它的成员位置等旧 Pod 被回收（[4.5](#45-名字确定创建幂等同名重建)）。不会有两批同名的 Pod，容量不超过上限。用户看到的是：删掉的组自己回来，`AGE` 归零，与删 StatefulSet 的 Pod 相同 |
| 同上，`--cascade=foreground` | 与 G1、G2 的删除相同 |
| `--cascade=orphan` | Pod 留下，没有 controller owner，成为**无主的成员 Pod** |

**无主的成员 Pod**：带这个 ModelServing 的标签和代际号标签，没有 controller owner，不在终止中，并且**名字是它的组名标签指的那个组的一个期望成员的名字**。期望成员按 Pod 自己的 revision 标签指的版本里的 Role 判断，实例序号超出当前副本数的也算；那个版本读不到时按当前 `spec` 的 Role 判断。代际号标签是 ServingGroup controller 管过它的痕迹：随手建一个带标签的 Pod 不算。名字的条件挡住标签是抄来的 Pod：`kubectl debug --copy-to --keep-labels` 的副本带着原 Pod 的全部标签、没有 owner，名字是另一个。

| # | 规则 | 说明 |
| --- | --- | --- |
| 1 | **找回（S1′）。**ModelServing controller 通过 Pod 缓存上的一个索引知道哪些序号上有无主的成员 Pod 而没有同名的组对象。动手之前直读这些 Pod。给每个这样的序号建对象 | 先于 G0，不受「正式的组少于 N」限制：这些 Pod 已经在运行，对象建出来之后收养它们，容量不因此增加。多出来的再按缩容处理。序号被外来的同名对象占着时不建，报 `OrdinalOccupied`（4.5）；这些 Pod 留着，直到那个对象被处理。这个对象在名下的无主成员 Pod 收养完之前不是缩容、回收、替换的候选，只占位置；缩容数个数时也不数它，等收养完再选 |
| | 版本 | 这些 Pod 的 revision 标签一致、对应的 ControllerRevision 还在时用它；否则按序号和 partition（与 G4 相同） |
| | surge 注解 | `ServingGroupRollingUpdate` 下，序号不小于 N，并且至少一个这样的 Pod 带着值是 `serving-group` 的 surge 注解（2.3 的副本）：建出来的对象带注解。其余不带 |
| 2 | **收养。**ServingGroup controller 收养组名标签指着自己的无主的成员 Pod。换 owner 之前直读这个 Pod 和自己这个组（UID 没变，没有 `deletionTimestamp`） | 无主的同名 PodGroup、Service 同样收养。这就是成员状态「待接管」（[03](03-state-machine.md) 第 3 节） |
| 3 | **版本不同就回到重建。**`ServingGroupRollingUpdate` 下，组里没有未完成的原地更新，而收养来的成员的 revision 标签与组的 `spec.revision` 不同：写 `RecreateRequired`，在预算之内替换 | `RoleRollingUpdate` 下各实例按自己的标签接管，Role 一级的规则处理过期的实例。升级接管（Pod 的 owner 是 ModelServing）不走这一条（[06](06-live-upgrade.md)） |
| 4 | ControllerRevision 的清理把无主成员 Pod 的 revision 标签算进保留集合（8.6） | 否则对象没有了之后，这些 Pod 的版本不再被任何组引用 |
| 5 | **收养不完。**一个组超过期限还有没收养的无主成员 Pod（换 owner 一直被拒绝）：报 `GroupNotObserved`，message 写明是哪些 Pod | 这期间它不是缩容、回收、替换的候选（4.4）。期限的起点在内存里，只用于报告（3.2） |

名字对不上任何期望成员、或者不带代际号标签的带标签 Pod 不是无主的成员 Pod：不为它找回，不让它所在的组变成待收养，不收养，不删除，发事件报告。它占着的名字正好是某个成员的名字时（不带代际号标签的那一种），那个成员按 L7 等待。

分不清的情形（都要一次 orphan 删除，再加一次缓存的先后）：

| 情形 | 结果 |
| --- | --- |
| 稀疏的序号集合（`{1, 3}`、N = 2），orphan 删 `sg-3`；组的缓存先显示少了一个组，Pod 的缓存还没显示无主 | G4 先在序号 0 上建一个新组，随后找回 `sg-3`。正式的组 3 个，缩掉一个：未就绪的先。连续的集合下不会这样：G4 选中的正是被删的那个序号 |
| 受保护的序号上是第三个版本（不是 B 也不是 T）的组被 orphan 删掉，同样的先后 | 新对象按 B 建，收养来的 Pod 版本不同，`RecreateRequired`，在预算之内换成 B |

这是 ModelServing controller 读 Pod 的四处之一（2.4）。整条路是推演，没有验证。

用户文档里写明：不要用 orphan 方式删组。

### 5.6 PodGroup 和 Service

每个组一个 PodGroup、每个需要的 Role 实例一个 headless Service，owner 是 ServingGroup。`minMember` 等字段仍然按 ModelServing 的 gang 配置算，输入通过 G5 进到组的 `spec`。组被删除时它们随级联删除。

## 6. 并发

### 6.1 没有按资源的锁

两个队列，各自一个 key 同时只有一个 worker。回调只入队；回调做的映射（Pod 到它的组，组到它的 ModelServing）是事件对象上的纯函数。Pod 映射到的是把它当成员的那个组（4.1 的成员 Pod）：controller owner 是这个组的，比对 ownerReference 的 UID；待接管的，按组名标签找到组、再按 [03](03-state-machine.md) 第 3 节的条件判定。入队的范围和摘要的范围因此是同一个集合：让摘要变的 Pod 事件一定让这个组的 ServingGroup controller 跑一轮。Pod 的更新事件按新旧两个对象各映射一次：标签或者 owner 被改时，它离开一个组、进入另一个，两个组都入队。待接管的判定另读两样不在 Pod 上的东西：所属 ModelServing 的接管注解，和 Pod 自己的版本读不读得到。这两样变了时，把这个 ModelServing 名下的组都入队；对待接管的 Pod，映射因此不完全是事件对象上的纯函数。两个 controller 的内存按各自的 key 分开，互相不读对方的。

进程里不是对象副本的东西只有 [3.2](#32-决定的输入只来自对象) 列的几项，都只会让写晚发，不会改变写的内容。

### 6.2 每个写的前置条件

| 写 | 前置条件 | 被拒绝之后 |
| --- | --- | --- |
| 创建 ServingGroup | 名字确定 | AlreadyExists：直读，核对 controller owner 的 UID（4.5） |
| 摘掉组的 surge 注解（转正） | 只带 UID。这次写幂等、只减不加；带 resourceVersion 的话会被这个组的 status 写反复挤掉 | 重新计划 |
| DELETE ServingGroup | UID 和 resourceVersion，两个都必需；前台级联 | 重新计划。组在观察之后有任何变化（就绪变了、被转正了），这次删除都不成立 |
| PATCH `ServingGroup.spec` | body 里带 `metadata.uid`；`sourceGeneration` 不小于对象上的。改 `spec.revision` 的两种（G2′(b)、原地的 G3c）另带 resourceVersion：前一种要和「先写后建」分出先后，后一种占一个额度，前置条件与删除一样强 | 重新入队；带 resourceVersion 的重新计划 |
| UPDATE `ServingGroup.status` | 整个对象的 UPDATE，带 resourceVersion。同名的新对象 UID 不同，被拒绝 | 本轮后面的删除和创建不发；下一轮重新计划 |
| 创建 Pod、PodGroup、Service | 名字确定。AlreadyExists 时核对 controller owner 的 UID。「内容不同就更新」之前同样核对 | 不属于自己：L7 |
| DELETE Pod | UID；Draining 下另要代际号不大于 g | 重新计划 |
| PATCH Pod 的 owner、原地更新的补丁 | UID 和 resourceVersion | 重新计划。重复的原地补丁被拒绝，重启基准不会被第二次的时刻盖掉 |
| PATCH Pod 上的 surge 注解（Role 一级的标记；组一级标记的副本） | UID | 重新计划 |
| UPDATE `ModelServing.status` | resourceVersion | 重新计划 |

一个写失败（被拒绝，或者出错），这一轮后面的创建、删除、摘注解和改版本都不发；创建撞名字不算失败，G5 的下发照发（4.2）。

每个写只以被写的那个对象为条件。带 UID 是同名重建的要求：缓存里的旧对象和现在同名的新对象不是同一个（4.5）。预算是对多个组求和，写不成一个对象上的条件；这一点由下一节的直读收窄，没有完全解决（6.5）。

### 6.3 直读和复核

**有写要发的轮次按直读重算。**

| controller | 规则 |
| --- | --- |
| ModelServing controller | 一轮按缓存算出来有组一级的写要发（建组、删组、摘注解、下发 `spec`、推进基线）：直读这个 ModelServing 和命名空间里带它的名字标签的全部 ServingGroup（一次一致性读），按 controller owner 的 UID 分成自己的和外来的，按读到的重新计划，发重新计划出来的写。外来的对象占着哪些序号，另用缓存里看到的（没有标签的外来对象直读里没有）；缓存里也还没有的，在创建撞名字时发现（4.5），这一轮把那个序号当成被占着再算一次。没有写要发的轮次不读 |
| ServingGroup controller | 一轮按缓存算出来要写 Failed、写 Draining、删 Pod 或者打原地更新的补丁：直读这个组和它名下的 Pod，按读到的重新计划。要写 Failed、写 Draining 的轮次另直读所属的 ModelServing，取最新的恢复策略和 grace（5.3）。给一个没有 Pod 的组建第一个 Pod 之前先写 `status`（5.2 的 L8）。直读的轮次，`status` 和它的摘要都按直读到的 Pod 算 |

为什么要它。名字确定只保证重复的创建撞名字。删除不是这样：按旧的视图算出来的删除可以落在另一个对象上，而那个对象自己没有变过，UID 和 resourceVersion 都挡不住。三个例子，都是 N = 3、U = 1、S = 0，下限是 2：

| 情形 | 按缓存直接发 | 按直读重算 |
| --- | --- | --- |
| 新进程的第一次 LIST 是旧的。那之后 `sg-1` 变成未就绪，原进程已经删了它 | 新进程看到三个都就绪，删序号最高的 `sg-2`。就绪 1 个 | 直读里 `sg-1` 在终止中：健康额度是 0，不删 |
| 目标是 v1，刚删了旧版本的 `sg-2`，缓存还没看到；用户把目标改回 `sg-2` 的版本 | 缓存里 `sg-2` 还活着、还就绪，于是按新的目标删 `sg-1`。就绪 1 个 | 直读里 `sg-2` 在终止中：不删 |
| 刚转正了一个组，缓存还没看到；N 又调小 | 缓存里它还是 surge，正式的组不多于 N，于是先替换了别的组。应当先缩容 | 直读里它是正式的组：先缩容 |

上一版用进程里的在途登记和启动时的一致性 LIST 挡前两种。这里不记，只读：自己发过的写（建、删、摘注解、下发）在直读里都看得到，换一个进程也一样。

代价是每个有写的轮次一次 LIST。稳态下没有写要发，不读。没有测量。

直读挡不住的：读和写之间别的对象又变了（6.5）；`status` 的内容本身是旧的。后一种由复核处理。

**复核**（A7）。组的 `status` 要是按 ModelServing controller 现在在 Pod 缓存里看到的这些 Pod 算的（`observedPods` 相同）才算数；不新鲜的组按未观测算。它挡三种情形：

| 情形 | 没有复核 | 有复核 |
| --- | --- | --- |
| 一个组刚丢了成员，ServingGroup controller 还没把 `status` 写成未就绪；同时来了新模板 | 健康额度按三个就绪算，删一个健康的组。就绪低于下限 | 这个组的 Pod 变了，摘要对不上：不算确认就绪，额度是 0 |
| controller 停着的时候一个节点坏了，两个组的 Pod 不再就绪，`status` 没有人写。新进程起来，ModelServing 的 key 先被处理 | 按停机之前的 Ready 删健康的组 | Pod 缓存同步完才开始处理：这两个组的 Pod 变过，`status` 不新鲜 |
| 一个旧版本的组掉过一次就绪又恢复了，`status` 还写着未就绪 | 它是「未就绪的旧组」，不占健康额度就被删掉。实际就绪的组少了一个，可以低于下限 | `status` 不新鲜：这一轮不是候选。`status` 跟上之后按健康的组处理 |

第二种和第三种在停机之后都会出现，是上一版设「接管屏障」（新进程里 ServingGroup controller 先把每个组过一遍）的原因。有了复核，新进程里两个 controller 之间没有先后的要求，与 Deployment controller 相同：它也不等 ReplicaSet controller 先把 `status` 重算一遍。

复核只进额度和候选，不进次序；次序看 Pod（4.4）。它是 ModelServing controller 读 Pod 的一处，只读缓存，只比摘要。就绪的判定只有 ServingGroup controller 里的一份，没有两处要对齐的规则。Deployment controller 在 Recreate 策略下也直接看 Pod。摘要的范围和 ServingGroup controller 入队的范围是同一个集合（6.1），所以摘要一直对不上只有一种原因：这个组的 `status` 一直写不出来；它一直按未观测算，报 `GroupNotObserved`。两个 controller 共用一个 Pod informer 是前提（3.3）。有组不新鲜的轮次，ModelServing controller 隔一小段时间再看一次（ServingGroup controller 用了直读、Pod 缓存追上时没有别的事件带来下一轮）；这个重试带退避，上限取 `GroupNotObserved` 的期限的量级。摘要的开销、Pod 启动期间多出来的 status 写，留给真实验证。

### 6.4 两个进程同时在写

失主的进程在发现失主之前还可能发出写。选主不是 fencing。这段时间里：

| 写 | 结果 |
| --- | --- |
| 建 Pod | 名字确定，幂等 |
| 建组 | 名字确定，幂等：两边算出同一个序号，只有一边建成。两边看到的 `spec` 不同时（一边还是旧的 N），可能建出对方认为多余的组，按缩容或回收处理；`active` 不超过两边各自的 N + S |
| 转正 | 两边摘的是同一个注解。拿着旧 N 的一边可能转正一个对方认为不该转正的组：它成为范围外的正式的组，按缩容处理 |
| 账本的转换 | resourceVersion 分出先后，只有一边成功 |
| 删 Pod | 带 UID；读到旧的 Draining 的那一边受代际号限制，碰不到新一批 |
| 删组 | 带 UID 和 resourceVersion。一边刚转正的组，另一边按 surge 回收它的删除被拒绝 |
| 下发 `spec` | `sourceGeneration` 挡住旧的一边 |
| `ModelServing.status` | 两边交替覆盖，包括基线。旧的一边退出之后收敛 |
| plugin 钩子 | 重复调用 |
| 缩容、替换 | 规则是确定的：两边读到的对象相同时选中同一个。读到的不同时可能各选一个，一个额度删两个 |

**原进程已经退出，它发出的请求还在路上。**晚到的创建撞名字，或者成为那个序号上的组，内容与现任要建的相同或者由之后的规则处理。晚到的删除带 resourceVersion：对象变过就被拒绝，没变过就执行，它是当时成立的一个决定。按 Kubernetes API 的行为推出来的，没有在集群上验证。

模型里还有一种形态：旧的一边以为实例在创建中、把刚清空的 Pod 又建出来，新的一边再删掉；它持续到旧的一边退出为止，不产生错误的终局（[05](05-inference-verification.md) 第 4 节）。

### 6.5 没有解决的

- **预算是对多个对象求和。**删一个组可以带上它自己的 resourceVersion；直读之后、删除落地之前别的组变了，挡不住。单个进程里这个窗口是一次读到一次写之间；两个进程并存时是一个额度删两个。
- **Pod 缓存自己的滞后。**复核读的是 Pod 缓存。一个组刚变坏而 Pod 缓存还没更新时，健康额度仍然会算进它。今天单个 controller 直接读 Pod 的缓存，是同样的滞后。
- **Drained 晚一跳。**Drained 写成真之后又有成员出故障：Pod 变了，`status` 不新鲜，G1 不删，等 ServingGroup controller 把 Drained 写回假。窗口缩到 Pod 缓存的滞后：Pod 缓存还没看到那次故障时，组被前台删除，新出故障的成员和健康的成员一起删。结果仍然是「故障成员先停下、再删其余的」里的一种，只是新出故障的那个成员没有被单独等。另有一种故障不改 Pod：一个重启故障的成员不再重启、也不就绪，grace 到期的那一刻变成故障，摘要不变（原地更新的进度期限到期同样）；Drained 为真之后 G1 通常立刻就删，所以要 ModelServing controller 在这中间停过才碰得到。三个模型都没有这个窗口。
- **会一直停住、要人处理的几种。**卡在终止中的组、外来的同名对象占着序号；成员、PodGroup、Service 的名字被不属于这个组的对象占着（4.5）；待收养一直收养不完（5.5）；`status` 一直写不出来。每一种都有报告。
- **`status` 一直写不出来的组。**它按未观测算（未观测，或者不新鲜），不是替换的候选，报 `GroupNotObserved`。报告的就绪可能一直偏大，只影响对外的可用数；额度有复核，次序看 Pod。
- **换 owner 时目标对象刚换代。**收养（5.5）和升级接管换 owner 之前直读这个组，确认 UID、确认不在终止中。PATCH 的前置条件是 Pod 的 UID 和 resourceVersion，不是组的：直读之后、PATCH 之前组被删了重建，写进 Pod 的 owner 是一个不存在的 UID，垃圾回收器会删掉这个在运行的 Pod。同名的对象换 UID 在这一版里是常态；窗口是一次读到一次写之间。
- **后台删组期间的 PDB。**用户用后台方式删一个组之后，旧一代的 Pod 在被回收之前 owner 是一个不存在的 UID。用百分比的 PDB 要通过 owner 的 `scale` 子资源取期望数（8.5），这段时间取不到；选中这些 Pod 的 PDB 怎样表现没有验证。规则自己的删除是前台的，对象一直在，没有这个问题。
- **组一级的写一直被拒绝**（对象配额、准入 webhook）。计划不是空的，`RolloutBlocked` 的各个 reason 都不成立。有事件，当时为真的 condition 的 message 里写着哪个写失败，没有专门的 condition。G5 的下发不被它挡住（4.2）。
- **并行度的上限在限流上。**不同的组并行，但每个生命周期步骤的写也多了，直读也占限流。没有测量。

## 7. plugin 调用点

框架的四个钩子和原地更新的 plugin 接口，调用者都从 ModelServing controller 换成 ServingGroup controller。特定实现的 plugin 不在这里分析。

| 钩子 | 今天（走读） | 本设计 | plugin 作者要核对的 |
| --- | --- | --- | --- |
| `OnPodCreate` | 创建 Pod 之前，`model_serving_controller.go:3242` | ServingGroup controller，创建 Pod 之前。位置不变 | `HookRequest.ServingGroup` 是组名，格式不变，仍然可以从里面取序号 |
| `OnPodReady` | Pod 就绪时，`:2156` | ServingGroup controller，同一个时机 | 接管之后可能对已经就绪的 Pod 再调一次 |
| `OnRoleSync` | 每轮同步一个 Role 实例时，`:1547` | ServingGroup controller，每轮 | 传入的 owner：plugin 创建的对象挂在 ServingGroup 名下 |
| `OnRoleDelete` | 删除一个 Role 实例时，`:3118`；删除整个组时对每个实例调（`:3369-3375`） | **实例被清空**：写 Draining 之后、删 Pod 之前调；调之前直读这个组的 `status`，账本仍是 Draining(g) 才调；离开 Draining 的那次 status 写之前，本进程必须成功调过一次（接管到一个已经清空的 Draining 时先补调）。**整个组被删**：ServingGroup controller 看到 `deletionTimestamp` 时尽力调一次，不保证 | 幂等。**组被删时不保证调到**：plugin 的集群内资源要挂在 ServingGroup 名下，靠垃圾回收清理。在钩子里清理集群外资源的 plugin 要另想办法 |
| 原地更新的 plugin 接口 | 打补丁时 | ServingGroup controller | 无变化 |

`HookRequest` 另带上组的 UID 和实例的代际号 g，plugin 可以用它们分辨同一个名字上的新旧两批。代际号只在一个组的 UID 之内有意义：同名重建之后新对象的账本从零值开始，g 又从头数。调用之前的直读挡住「拿着过期的 Draining 的进程对新一批实例调删除钩子」的大部分情形，挡不住直读之后才发生的变化，所以 plugin 自己也要按 g 核对。内置的 headless Service plugin 今天靠进程内的一份记忆防止把旧的删除决定用到新对象上（走读，`plugins/headless_service_plugin.go:109-181`）；接管之后这份记忆没有了，要改成按对象上的事实核对：Service 创建时写上组的 UID 和实例的代际号，删除之前比对。

**钩子一直失败的后果。**今天钩子在 Pod 的 DELETE 之后调，失败不挡删除。本设计里 `OnRoleDelete` 不成功，实例就停在清空中：不删 Pod，不重建。一个出故障的 plugin 因此会挡住按范围的恢复、Role 的缩容和按重建的 Role 滚动。组上用 `DrainBlocked` condition 报告，并发事件。

**`ServingGroupRollingUpdate` 下不要依赖 `OnRoleDelete`。**这个模式下滚动、缩容、整组恢复全是整组删除，钩子只是尽力调一次；它不是偶尔调不到，是从来不保证。

`OnRoleDelete` 在组删除时不再有保证，是本设计对 plugin 作者唯一收紧的地方。它是「组没有 finalizer」的直接后果。两个没选的做法：给组加 finalizer（kthena 不在时组、namespace、CRD 删不掉）；删组之前加一段由 `spec` 触发的清空协议（有保证，多一跳，controller 不在时组删不掉）。

内置的 headless Service plugin 创建的 Service 挂在 ServingGroup 名下，随组级联删除，不依赖钩子。

在网版本的 plugin 框架比上游多三个钩子：Pod 运行、Pod 删除、整组删除。目标版本要带上它们（[06](06-live-upgrade.md) 的 P1），调用者同样是 ServingGroup controller：前两个在对应的 Pod 事件上调；整组删除的钩子与组被删时的 `OnRoleDelete` 是同一个时机，同样只尽力调一次。

## 8. 周边组件

### 8.1 驱逐 webhook

今天按「owner 是 ModelServing」认 Pod。改成认两种归属：owner 是 ServingGroup、它的 owner 是这个 ModelServing；或者 owner 直接是这个 ModelServing（接管到一半）。预算计数两种都算。只改一处会让保护无声失效。

### 8.2 autoscaler 和 HPA

autoscaler 仍然改 ModelServing 的副本数和 Role 的 replicas；到 Pod 多经过一个 controller。Role 实例的序号保留，HPA 的 selector 靠「0 号实例」保证每组一个 Pod 的做法不变（走读，`model_serving_controller.go:2896-2912`）。滚动途中的 surge 组也会被选中，与今天相同。

### 8.3 router 和别的读标签的组件

`group-name` 标签只被当作不透明的标识（007 的走读）。组名的格式和今天相同，解析它、或者依赖 `<名>-0-...` 这种固定名字的用户脚本、告警、固定 DNS 名都不受影响。

### 8.4 校验 webhook

名字长度今天按副本数的位数估计（走读，`webhook/validator.go:162-179`），并且每次 UPDATE 都跑。组名的格式没有变，这条校验的写法不变；它今天按 N 估计位数，有 surge 时序号可以到 N + S − 1，要把它算进去。一般的规则仍然成立：新的或收紧的校验只拦新引入的，对象上没被这次更新改动的值放行（007 的 05 第 2.1 节），否则存量对象的任何更新都被拒，包括 autoscaler 的 PATCH。

### 8.5 PodDisruptionBudget

用 `maxUnavailable` 或百分比的 PDB，要通过 Pod 的 controller owner 的 `scale` 子资源取期望数（按 Kubernetes 的 disruption controller 的源码，没有在集群上验证）。今天 owner 是 ModelServing，它有 `scale` 子资源，`specReplicasPath` 是 `.spec.replicas`，也就是组的个数，不是 Pod 的个数。owner 换成 ServingGroup 之后：

- ServingGroup 没有 `scale` 子资源时，这类 PDB 解不出期望数，驱逐全部被拒。
- 所以 ServingGroup 要有 `scale` 子资源。`spec` 里加一个由 ModelServing controller 写的 `expectedPods`（这个组期望的 Pod 数），`specReplicasPath` 指向它；`status` 里对应的是 `status.pods`：现有的 Pod 数，加上已经决定要建而还没建出来的（L8）。它按这个组该用的版本的模板算：各 Role 的 `replicas` 乘以每个实例的 Pod 数，求和；不含 Role 一级的 surge 实例。它只给 PDB 读（另外只在模板读不到时给 ServingGroup controller 算 Ready 用，A1）；有人通过 `scale` 改它，下一轮被写回。它随 Role 的副本数变，和 Role 副本数的下发是同一次 `spec` 写。

这样 PDB 的百分比第一次是按 Pod 数算的。用整数 `minAvailable` 的 PDB 不受影响。

### 8.6 ControllerRevision 的清理

今天清理历史版本时保留「controller owner 是这个 ModelServing 的 Pod 引用到的版本」，每次直接 LIST Pod（走读，`utils/controller_revision.go:228-262`）。owner 换成 ServingGroup 之后这个判断对所有 Pod 都不成立，会删掉还在用的历史版本。改成保留：B、T、每个现存的组的 `spec` 里出现的版本、每个组 `status.referencedRevisions` 里的版本（A6）、无主的成员 Pod 的 revision 标签指向的版本（5.5）。名下有未观测的组时这一轮不清理：它的 `referencedRevisions` 还不可信。

### 8.7 协调模式的基线

roleCoordination 今天把滚动的基线写在 ControllerRevision 的注解上。ServingGroup controller 不写 ControllerRevision；这份基线由 ModelServing controller 通过 G5 下发到组的 `spec`。ControllerRevision 上的那个注解仍然由 ModelServing controller 写，回退之后旧版本读得到；新版本自己不读它。

### 8.8 事件

今天全部事件都记在 ModelServing 上。两个 controller 之后：

| 事件 | 记在哪个对象上 | 谁发 |
| --- | --- | --- |
| Role 实例和 Pod 一级的（实例在创建、在删除、已运行；plugin 报错；原地更新；删除时身份变了） | ServingGroup | ServingGroup controller |
| 组一级的（建了哪个组、删了哪个组、原因是缩容、替换还是失败；版本解析不出来；不接管的无主 Pod） | ModelServing，message 带组名 | ModelServing controller |

`kubectl describe modelserving` 里看得到每个组的来去，看不到组里面的细节；细节在 `kubectl describe servinggroup`。组被删掉之后，它的事件在保留期之内仍然可以按 `involvedObject.name` 查。同一个序号上先后的两个对象名字相同：`kubectl describe servinggroup` 只显示当前这个 UID 的事件，上一代的用 `kubectl get events --field-selector involvedObject.name=<组名>` 看。记在 ModelServing 上的「删了某个组」「建了某个组」名字相同，message 里带 UID。

`ModelServing` 的 condition 的 message 里报告范围内的空洞和范围外的正式的组（契约 A.5 的「身份债」），与今天相同。

### 8.9 拓扑和亲和

`groupPolicy` 和按 Role 的网络拓扑今天写进每个组的 PodGroup。本设计不变：值经 G5 进到组的 `spec`，由 ServingGroup controller 写进 PodGroup。PodGroup 的 owner 是 ServingGroup，名字跟着组名。Volcano 对 PodGroup 的 owner 变化、对同名 PodGroup 先删后建的反应没有验证；后一种今天的实现已经如此。

### 8.10 RBAC

chart 里：

| 权限 | 给谁 |
| --- | --- |
| `servinggroups` 的 get、list、watch | 聚合进内置的 view、edit、admin |
| `servinggroups` 的 delete | 聚合进 edit、admin。删一个组等于重建一个副本；能删 Pod 的人本来就能触发恢复 |
| `servinggroups` 的 create、update、patch；`servinggroups/status` 的任何写 | 只给 controller。手工建一个带正确 owner 的组会被当成多出来的组参加缩容，老组里有 deletion cost 更低的时候被删的是老组；`status` 是恢复的输入；surge 注解「只会被摘掉、不会后加」不能由 API 强制，靠没有人有 patch 权限 |

## 9. 原地更新

原地更新整个在 ServingGroup controller 里面，接口点沿用 007 的 05 第 4 节。相对 007 多出一处：

- `ServingGroupRollingUpdate` 配原地更新并且 U 大于 0 时（U 为 0 按重建执行，4.2），组不被替换，组的版本原地变。所以 `spec.revision` 在这种配置下可以被 G3c 改写（4.2）；更新途中的组 `spec.revision` 已经是 T，`status.revision` 还不是，按「目标、未就绪」计。成员分类先看原地更新的进度注解，再看重启故障：正在按预期重启的容器不是故障。
- **回到重建。**007 的 05 的接口点里，「有终态成员按重建」「超期」「被作废」「plugin 不支持」这几个结果都要求放弃原地更新。ServingGroup controller 遇到其中任何一个，就把组的 `RecreateRequired` condition 写成真，并停下组内的原地更新。ModelServing controller 把这样的组按可替换的、不可原地更新的处理：在预算之内删除它，在原序号上重建。收养来的成员版本与组的不同时也用它（5.5）。`RecreateRequired` 只有这两个来源，只增不减，随组一起消失。今天「有一个 plugin 不支持就整体回退成重建」的行为由此保留，只是判断从 ModelServing controller 挪到了 ServingGroup controller，并且按组进行。

原地更新没有模型，上面两条是推演。

007 的 05 里要裁定的限制（T17–T21、T26）不因本设计改变，仍然等裁定。

## 10. 没有了的、留下的和新增的

相对当前实现和在网版本：

| 没有了的 | 范围 |
| --- | --- |
| 跨多个 Pod 的恢复承诺（v2 的标记和 M1–M7；在网版本的 `deletion-scope` 注解和内存里的删除计划） | 全部 |
| 「建成过」只在内存里 | 全部 |
| 内存里的组和实例状态机；进程启动时把每个 Pod 当成新增事件重放来重建它 | 全部 |
| 组一级的 surge 身份由逐个打在 Pod 上的标记决定，会只打了一半 | 组一级：来源是一个对象上的一个注解，Pod 上的是副本。Role 一级还在 Pod 上 |
| 发出去还没看到的写要在内存里记着（v2 的在途叠加；上一版的在途登记） | 全部：有写的轮次按直读重算 |
| 重启故障的起点在内存里 | 全部 |
| 同序号上新旧两批 Pod 的隔离（在网版本的实例身份注解、创建前的 LIST） | 组一级靠对象的 UID；Role 实例一级换成代际号 |
| 两层共用一份工作副本的衔接规则 | 变成对象的边界 |
| 按 ModelServing 的互斥锁（在网版本） | 全部 |
| 整个 ModelServing 串在一个 key 上 | 不同的组并行 |

| 留下的 | 说明 |
| --- | --- |
| 组的序号和名字 | 与今天相同 |
| 组一级的规则：绝对序号的 partition、surge 的组、转正、回收、稀疏序号和补最低空洞 | 与契约 2.12 相同，与 Role 一级同一套（4.9） |
| 每次带 S 的滚动多建 S 个组，做完再回收 | 契约的行为。上一版去掉过它，代价是序号集合会漂移、名字要换 |

| 新增的 | 代价 |
| --- | --- |
| 一个 CRD | API 面、RBAC、生成的 client、文档、chart、模板类型改名 |
| 第二个 controller 和 A1–A7 | 状态汇总晚一跳；额度另由复核把关；组的 `status` 多一个摘要字段 |
| 有写的轮次直读 | 每个有写的轮次一次 LIST |
| ModelServing controller 读 Pod 缓存 | 复核、找回、缩容和回收的次序，只读 |
| `status` 当输入 | L2、L4、L5；`status` 端点要用 RBAC 保护 |
| 同名重建要靠 UID 分辨两代 | 每个写带 UID（4.5、6.2） |
| 故障起点写在账本里 | 搭在本来就有的 status 写里；改变 D9 |
| 部署前提 | 选主、失主退出 |
| 稳态下的写 | 成员 Pod 的判定用到的字段每变一次，它的组一次 status 写（摘要变了）；就绪变了，ModelServing 再写一次。每个组从没有 Pod 到有 Pod 多一次。Pod 不变时不写。对象多 N 个 |
| 一次升级 | [06](06-live-upgrade.md) |

## 11. 相对前面几版改了什么

**相对上一版（随机名字、组一级按数量，提交 `e35b2e37`）。**

| 上一版 | 这里 | 原因 |
| --- | --- | --- |
| 组名是随机后缀，每次替换和恢复都换 | `<ModelServing 名>-<序号>`，不变 | 用户按名字匹配的东西不受影响 |
| 组一级按数量：Deployment 的算式加按数量的 partition | 按序号，契约 2.12 的规则 | 与 Role 一级统一；与契约的差别只剩账本一类（[04](04-behavior-expectations.md)） |
| 没有 surge 身份；最后一个旧组删掉时数量正好是 N | surge 的组带注解，转正，回收 | 按序号时序号集合要回到正式范围 |
| 创建不幂等：在途登记、在途屏障、启动时的一致性 LIST | 没有这三样。有写的轮次按直读重算 | 名字确定挡住重复的创建（4.5）；直读挡住按旧视图算出来的删除（6.3） |
| 接管屏障：新进程里 ServingGroup controller 先把每个组过一遍 | 没有。组的 `status` 要是按现在这些 Pod 算的才算数（A7） | 停机期间变了而 `status` 没跟上的组通不过复核（6.3） |
| 健康额度按晚一跳的 `status` 算，列为已知的局限 | 额度另加复核 | 契约的用例里有 37 例的夹具正好对准这一跳（04 第 8 节） |
| deletion cost 经组的 `status` 汇总（A5），晚一跳 | 从 Pod 缓存现算 | 刚设完 cost 马上缩容要生效，与今天相同 |
| 空组的 Ready 是假 | 是真，与今天相同；有空组时基线不推进 | 对外的可用数不变 |
| 受保护的集合按「基线版本的在前」；缩容「过期的先」；空组「按新建的规则重新指派」的一套推论 | 都按序号 | 同上 |
| 无主成员 Pod 的收养：只停 G4、三种出口、同名而版本不同时删掉重建 | 找回、收养、版本不同回到重建（5.5） | 名字确定之后，连续的序号集合下没有「多建一个再缩掉」 |
| 组里的 Pod 全没了而对象还在：按组自己的版本补 | 写 Failed，按序号和 partition 重建（5.3） | D12 |
| 空组的版本按数量重新指派 | 没有成员的组按序号重新指派；范围外的空组删掉（4.2） | 契约 0.6、A.4 |
| 命名看「owner 是 LWS 或者回退窗口开着」；回退窗口 | 没有 | 名字前后相同 |
| 组的「接管中」状态、Adopted condition、`adopt-members` 注解 | 没有 | 「待接管」是成员一级的状态就够了 |
| 故障起点在内存里（D9），列为豁免 | 账本里的一项，带这一段故障的身份 | 决定的输入只来自对象（3.2）。要批准 |

**相对 007 的 v4。**

| 007 的 04 | 这里 | 原因 |
| --- | --- | --- |
| 去掉组的序号，名字是随机后缀 | 保留序号 | 见上 |
| 第 6 节：组一级按数量的规则轮廓 | 4.1–4.9，按序号 | 见上 |
| 6.4：失败的组在缩容次序里排第一 | 失败的组只由 G1 删 | 否则缩容绕过 D15 |
| 7.1：等两个计数为 0 | Drained condition | 计数的零值正好是「可以删」 |
| 第 5 节：单 Pod 的实例不记账 | 每个实例都记 | 5.2 |
| 第 5 节：缺失之前直读一次 | 任何用来写 Draining、Failed 的证据都直读 | 缓存里上一代的终止中 Pod 会被当成证据 |
| 第 5 节：代际号一句话 | L5 的四句 | `status` 被清空之后 Draining 会卡住 |
| 6.2：多建是常态 | 不会多建 | 名字确定 |
| 组的 DELETE 带 UID | 带 UID 和 resourceVersion | 缓存旧时会删掉一个已经恢复的组 |
| K3 LWS、K7 deletion cost 待定 | 2.2（不需要特殊处理）；4.4（从 Pod 缓存现算） | — |
| 组的 `spec` 的 PATCH 不带条件 | `sourceGeneration` | 缓存旧的进程会把旧 `spec` 写回去 |
| `OnRoleDelete` 没有提 | 第 7 节 | 组被删时今天会调 |
| ControllerRevision 的清理没有提 | 8.6 | 按 owner 的判断失效，会删掉在用的历史版本 |
| PDB 没有提 | 8.5 | 用百分比的 PDB 要读 owner 的 `scale` 子资源 |
| 第 12 节迁移要点 | [06](06-live-upgrade.md) | — |
