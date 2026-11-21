# 完整设计

## 0. 文档信息

| 项 | 内容 |
| --- | --- |
| 用途 | ServingGroup 作为 CR 的完整设计：对象、两个 controller、组一级和 Role 一级的规则、并发、plugin 调用点、周边组件。状态和转换的逐条写法在 [03](03-state-machine.md)，这里写规则和理由 |
| 起点 | [007 的 v4](../modelserving-state-machine-v3-v4/04-v4-servinggroup-cr.md)。它是评估稿；这里把它定为终稿，把其中标成「示意」「轮廓」「待定」的地方写成规则。改了的地方列在[第 11 节](#11-相对-007-的-v4-改了什么) |
| 沿用 | Role 一级的滚动、扩缩容、协调的规则沿用 [v2](../modelserving-rollout-final/04-state-machine.md)；原地更新的接口点沿用 [007 的 05](../modelserving-state-machine-v3-v4/05-inplace-usage-isolation.md)；判据 J1–J8 沿用 [007 的 02](../modelserving-state-machine-v3-v4/02-dfa-criteria-and-tradeoffs.md) |
| 状态 | 设计提案。没有改代码，没有在集群上验证。组一级的规则和账本的两条防护规则用模型穷举过，范围见 [05](05-inference-verification.md) |
| 记号 | N、U、S、P 是最新 `spec` 解析出来的副本数、`maxUnavailable`、`maxSurge`、`partition`。T 是目标版本，B 是基线版本。G1–G6 是组一级一轮计划的六步。「走读」指读代码得出，行号对应 `volcano-sh/kthena` `main` @ `79c455ef`。D、R、O 开头的是 005 的编号，K、T 开头的是 007 的编号 |

## 1. 概览

```text
ModelServing ──owns──► ServingGroup ──owns──► Pod、PodGroup、headless Service
     │                      │
     │ ModelServing         │ ServingGroup
     │ controller           │ controller
     ▼                      ▼
 组的个数、版本、次序      Role 实例、成员、恢复、原地更新
```

一句话：**ServingGroup 是一个对象，名字不带序号；范围一级的事实都写在这个对象上；ModelServing controller 只管「有几个组、各是什么版本」，ServingGroup controller 只管一个组里面的事。**

事实放在哪里：

| 事实 | 今天 | 本设计 | 写者 |
| --- | --- | --- | --- |
| 这个组存在、是哪个版本 | 一组 Pod 上的标签 | `ServingGroup` 对象和它的 `spec.revision` | ModelServing controller |
| 这个组正在被删除 | 内存里的单元状态；在网版本另有 Pod 上的 `deletion-scope` 注解 | `ServingGroup.metadata.deletionTimestamp` | ModelServing controller（发 DELETE） |
| 这个组丢了成员、要整组重建 | 内存 | `ServingGroup.status` 的 Failed | ServingGroup controller |
| 这个 Role 实例建成过 | 内存里的一个布尔值 | `ServingGroup.status.roleInstances[]` 的 Established | ServingGroup controller |
| 这个 Role 实例正在被清空 | 内存；在网版本另有 Pod 上的注解 | `roleInstances[]` 的 Draining | ServingGroup controller |
| 组是不是临时 surge | Pod 上的 surge 注解 | 没有这个事实 | — |
| 基线版本 | `ModelServing.status.currentRevision` | 不变，另在每个组的 `spec.baselineRevision` 留一份 | ModelServing controller |

## 2. 对象

### 2.1 ServingGroup

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ServingGroup
metadata:
  name: llama-7k2qd                     # 见 2.2
  labels:
    modelserving.volcano.sh/name: llama
  annotations:
    modelserving.volcano.sh/legacy-ordinal: "2"   # 只有从旧版本接管来的组有，见 06
  ownerReferences:                      # ModelServing，controller=true，blockOwnerDeletion=true
spec:
  revision: llama-6b7f9                 # ServingGroupRollingUpdate：这个组的版本
  targetRevision: llama-6b7f9           # RoleRollingUpdate：组内滚动的目标
  baselineRevision: llama-5c4d8         # 两种模式都带：基线的副本；Role 模式下也是受 Role partition 保护的实例用的版本
  sourceGeneration: 12                  # 这份 spec 取自 ModelServing 的哪个 generation
  expectedPods: 6                       # 这个组期望的 Pod 数；只给 scale 子资源和 PDB 用（8.5）
  recoveryPolicy: RoleRecreate
  restartGracePeriodSeconds: 0
  roles:                                # 每种 Role 的 replicas 和 Role 层的滚动参数、协调配置
  - name: prefill
    replicas: 2
status:
  observedGeneration: 3
  conditions:
  - type: Ready                         # 全部期望的实例已建成并且就绪
  - type: Failed                        # 不可逆。真 = 请求整组替换
  - type: Drained                       # 只在 Failed 为真时有意义：没有故障成员，没有正在终止的 Pod
  - type: Adopted                       # 只有接管来的组有：成员全部换了 owner
  revision: llama-6b7f9                 # 全部实例已经应用的版本；不一致时为空
  referencedRevisions: [llama-6b7f9]    # 名下 Pod 引用到的全部版本，包括正在终止的
  deletionCost: 0                       # 名下 Pod 的 deletion cost 之和
  faultyMembers: 0                      # 给人看的；判断只用 condition
  terminatingPods: 0
  roleInstances:                        # 账本，见 5.2
  - role: prefill
    index: 0
    phase: Established                  # Creating（零值）、Established、Draining
    generation: 4
```

字段名是提案，不是 API 评审稿。`spec` 里**不内嵌 Pod 模板**：模板从 ControllerRevision 按名字读。这样对象小，`kubectl apply` 不会因为注解太大失败，版本的身份也只有一个来源。

Go 里已经有一个同名的模板类型 `ServingGroup`（走读，`pkg/apis/workload/v1alpha1/servinggroup_types.go:116`）。它要改名（例如 `ServingGroupTemplate`）。JSON 不变；仓库外引用这个 Go 类型的代码要跟着改。

### 2.2 名字

名字由 ModelServing controller 在客户端生成，不用 `metadata.generateName`：请求超时之后要能按名字查。分配方式是 controller 内部的策略，不是用户字段。

| 策略 | 名字 | 用在 | 次序键 |
| --- | --- | --- | --- |
| 随机 | `<ModelServing 名>-<5 位后缀>`。后缀取自不含元音和易混字符的字母数字表，并且至少含一个字母 | 新建的 ModelServing。默认 | 创建时间，再按名字 |
| 序号 | `<ModelServing 名>-<最小的空闲序号>` | owner 是 LeaderWorkerSet 的 ModelServing（一直用）；从旧版本接管来的 ModelServing 在回退窗口之内（[06](06-live-upgrade.md)） | 序号 |

- 后缀至少含一个字母，是为了和序号名字分得开：`<名>-<纯数字>` 一定是序号名字。
- 序号策略下名字的唯一性就是序号的唯一性。LWS 的 plugin 照旧从组名里取 `group-index`、hostname 和 DNS 名（走读，`plugins/lws_labels_plugin.go:61-95`）；两个活着的组不可能同序号，仍然是名字在保证。007 的待决项 K3 这样定。
- **序号只是名字。**组一级的算法在两种策略下相同，都按数量。序号策略下的两处限制：组一级的 S 取 0（LWS 的转换本来就不映射滚动配置，走读，`controller/lws_controller.go` 里没有 `maxSurge`、`partition`）；这样替换出来的组拿回原来的序号，缩容删最大的序号，集合保持 `[0, N)`。接管来的 ModelServing 在回退窗口内不受 S 的限制，序号集合可以有洞。
- Pod 名的前缀跟着组名：`<组名>-<role>-<实例序号>-<worker 序号>`。Role 实例的序号和 `WORKER_INDEX` 保留。
- 标签 `modelserving.volcano.sh/group-name` 的值是组名。

### 2.3 Pod 上新增的东西

| 项 | 内容 |
| --- | --- |
| controller owner | ServingGroup（`controller: true`、`blockOwnerDeletion: true`）。PodGroup、headless Service 相同；PodGroup 今天没有 `blockOwnerDeletion`（走读，`podgroupmanager/manager.go:340-350`），要补上，否则前台级联不等它 |
| 标签 `modelserving.volcano.sh/instance-generation` | 所在 Role 实例的代际号（5.2）。没有这个标签的 Pod 按 0 |
| 不再使用的 | 组一级的 surge 注解。Role 一级的 surge 注解、原地更新的进度注解、revision 和 role-template-hash 标签原样使用 |

没有 Pod finalizer，没有组的 finalizer。

### 2.4 谁写什么

| 对象上的东西 | 唯一的写者 | 方式 |
| --- | --- | --- |
| ServingGroup 的创建 | ModelServing controller | 名字先登记再发请求 |
| ServingGroup 的 DELETE | ModelServing controller | 前台级联，带 UID 和 resourceVersion |
| `ServingGroup.spec` | ModelServing controller | PATCH，带 `sourceGeneration`，不下发更小的 |
| ServingGroup 的 labels、annotations | ModelServing controller，只在创建时 | 创建之后不变 |
| `ServingGroup.status` | ServingGroup controller | UPDATE，带 resourceVersion |
| Pod、PodGroup、Service | ServingGroup controller | 创建按确定的名字；删除带 UID |
| `ModelServing.status` | ModelServing controller | 不变 |
| ControllerRevision | ModelServing controller | 不变。清理时保留的集合见 8.6 |

ServingGroup controller 不创建、不删除 ServingGroup，只写「失败」。ModelServing controller 不读写 Pod；唯一的例外是升级接管期间读存量的 Pod（[06](06-live-upgrade.md)）。

用户直接改组的 `spec`，等于改 Deployment 名下的 ReplicaSet：ModelServing controller 下一轮写回去。用户直接删组：见 [5.5](#55-有人直接删了一个组)。

## 3. 两个 controller

| | ModelServing controller | ServingGroup controller |
| --- | --- | --- |
| 队列的 key | ModelServing | ServingGroup |
| 看什么 | ModelServing 的 `spec`；名下的 ServingGroup 对象 | 一个 ServingGroup 的 `spec` 和 `status`；名下的 Pod |
| 做什么 | 组一级的扩缩容和 `ServingGroupRollingUpdate`；删除失败的组并按数量补；把 `spec` 下发到每个组；汇总 `status`；推进基线；解析和清理 ControllerRevision | Role 一级的扩缩容、`RoleRollingUpdate`、协调、恢复、原地更新；PodGroup 和 Service；plugin 钩子；写这个组的 `status` |

两者之间只通过对象交换信息。约定有六条：

| # | 约定 | 为什么 |
| --- | --- | --- |
| A1 | **`observedGeneration`。**`status.observedGeneration` 小于 `metadata.generation` 的组，ModelServing controller 按「未观测」计：占位置，不算就绪。ServingGroup controller 在「这一轮的决定是按这份 `spec` 做的」时推进它，不等动作做完 | 新 `spec` 刚写进去时 `status` 还是上一份的结论 |
| A2 | **`sourceGeneration`。**下发的 `spec` 带着来源的 ModelServing `generation`。ModelServing controller 不下发比组上已有的更小的；看到更大的就重新入队 | 缓存旧的进程不会把旧的 `spec` 再写回去 |
| A3 | **失败和已清空是两个 condition。**Failed 为真是请求删除；Drained 为真是「故障成员和正在终止的 Pod 都没有了」。ModelServing controller 只在两个都为真时删 | 判断不依赖计数字段的零值。`status` 被旧客户端整体改写、计数丢失时，不会把「没有清空」读成「已清空」 |
| A4 | **基线的副本。**每个组的 `spec.baselineRevision` 由 ModelServing controller 写 | `ModelServing.status` 被清空时基线可以读回来（4.8） |
| A5 | **deletion cost 汇总。**`status.deletionCost` 是名下 Pod 注解之和。还没有报告时按 0 | ModelServing controller 不读 Pod（007 的 K7）。它是偏好，晚一跳可以接受：刚设完 cost 马上缩容，可能还没生效 |
| A6 | **引用到的版本。**`status.referencedRevisions` 报告名下 Pod 用到的全部版本 | ControllerRevision 的清理不再去 LIST Pod（8.6） |

**部署前提。**

1. 两个 controller 在同一个进程里，共用一把租约。
2. 选主打开；失主的进程退出。今天选主默认关（走读，`cmd/kthena-controller-manager/main.go:86`），失主的回调只打日志、进程不退出（走读，`pkg/controller/controller.go:187-189`）；两处都要改。
3. 新进程过了接管屏障才开始写（[6.3](#63-接管屏障)）。

前提 1 不成立时（两个 controller 拆成两个 Deployment），接管屏障里「ServingGroup controller 先把每个组过一遍」要换成写在组 `status` 上的任期号。本设计不展开。

## 4. 组一级

组一级用的是 Deployment 的滚动算式（两个额度、未就绪的先、再健康的），单位从 Pod 换成组，再加一个按数量的 partition。有一处不照 Deployment：滚动途中改副本数时不按比例在新旧两边分摊，而是先把规模做完再继续滚动（契约 A.4）；CloneSet 和 Kruise Rollout 也是这样做的。

### 4.1 分类和计数

每个组恰好属于一类，自上而下判定：

| 类 | 条件 |
| --- | --- |
| 终止中 | `deletionTimestamp` 非空 |
| 失败 | Failed 为真 |
| 目标 | 组的版本等于 T |
| 受保护 | B ≠ T，并且它在「按次序最靠前的 min(P, 非目标组的个数) 个非目标组」之内。**不问它是不是 B 那个版本** |
| 过期 | 其余 |

「版本等于」指 ControllerRevision 的名字相同，或者经 ControllerRevision 解出的模板在语义上相同；目标版本的解析先沿用已有的身份。这是今天已有的机制，本设计不改算法。

| 计数 | 定义 |
| --- | --- |
| `active` | 全部组对象，包括终止中的和失败的 |
| `live` | 目标、受保护、过期三类 |
| `ready` | `live` 里 Ready 为真并且已观测的 |
| `good` | 目标 + 受保护 |
| `notReadyTarget` | 目标里没有就绪的 |
| `minAvailable` | max(0, N − U) |
| `maxScaleDown` | max(0, `live` − `minAvailable` − `notReadyTarget`) |
| `maxHealthyScaleDown` | max(0, `ready` − `minAvailable`) |
| `cap` | 还有过期的组时是 N + S，否则是 N |

算式与契约附录 B 相同。契约里的「在途额度」在这里不需要单独记：发出的删除立刻表现为组进入终止中，它已经不在 `live` 里。

`notReadyTarget` 只数目标版本的组，不数未就绪的受保护的组（契约的 RU-B06 也是这样）。受保护的组未就绪时 `ready` 少一个，健康的组能不能删由 `maxHealthyScaleDown` 挡住；`maxScaleDown` 里多出来的那一个只能用在未就绪的过期组上，删它不降低 Ready。

### 4.2 一轮的计划

计划是纯函数。它在一份工作副本上逐个动作迭代：每选中一个动作，就把效果记进副本（删除的组变成终止中，创建的组是一个未就绪的新组），重新分类和计数，再选下一个。

| 步 | 条件 | 动作 |
| --- | --- | --- |
| G1 删失败的组 | Failed 和 Drained 都为真 | 前台级联删除。不重新判断，策略中途改了也删 |
| G2 缩容 | `live` > `cap` | 按 [4.4](#44-次序) 的缩容次序删一个，重复到 `live` = `cap`。不占滚动预算 |
| G3 替换 | 有过期的组；`active` ≤ `cap`；只在 `ServingGroupRollingUpdate` 下 | 未就绪的过期组：`maxScaleDown` > 0 时删一个。没有未就绪的了，再看健康的：`maxScaleDown` 和 `maxHealthyScaleDown` 都大于 0 时删一个。每删一个重新算。未观测的组不是候选 |
| G4 创建 | min(N − `good`, `cap` − `active`) > 0 | 建一个，重复。版本：B ≠ T 并且非目标的组少于 P 时用 B，否则用 T |
| G5 下发 | 组的 `spec` 与期望不同 | PATCH（A2） |
| G6 推进基线 | P = 0，N > 0，`active` = N，全部组是目标、已观测、就绪；全部 Role 的 replicas 都是 0 的组不算就绪 | `status.currentRevision` := T |

几点说明。

- **G3 要求 `active` ≤ `cap`。**缩容删掉的组在消失之前还占着位置，`active` 大于 `cap`，这期间不开始替换。对应契约 A.4 第 2 条「先执行缩容选择并等待本轮计划删除完成」。它是对象上的事实，不需要记住哪些终止中的组是缩容删的。正常滚动时 `active` 不超过 N + S，不受影响。
- **未观测的组**（A1）占位置、不算就绪，这是保守的一侧。但它也不能被当成「未就绪的过期组」优先替换，缩容时也不排在「未就绪的先」里：`spec` 刚下发的那一刻全部组都是未观测，其中多数其实是健康的（EX-3）。
- **G4 的容量按 `active` 算**：终止中的和失败的组在消失之前占着位置。所以没有滚动在进行时（`cap` = N），失败的组消失之前不会建替换的（007 的 K6，默认不做）。滚动在进行时 surge 的名额照常使用，不专门留给恢复，也不专门躲开恢复。
- **G6 是 v2 的 R17**，不变。灰度停点上基线不推进。
- **配了原地更新时**（`ServingGroupRollingUpdate` 加 `updateMethod: InPlace`），G3 的「删一个」换成「把这个组的 `spec.revision` 改成 T」，由 ServingGroup controller 在组内原地更新。候选、次序和两个额度都不变：被改的组成为未就绪的目标组，占着 `notReadyTarget`，直到它就绪。能不能原地更新是配置上的事实（007 的 05），ModelServing controller 不去问 plugin。
- 没有稳定的中间态要记：任何一步停下，下一轮按当时的对象重新得出同一份或更保守的计划。

### 4.3 按数量的 partition

今天 partition 按绝对序号保护 `[0, P)`。没有序号之后：

| 定义 | 内容 |
| --- | --- |
| 保护什么 | 至多 P 个**非目标**的组，取次序最靠前的。它们不因目标模板变化而被替换 |
| 新建的组用哪个版本 | 非目标的组少于 P（并且 B ≠ T）时用 B，否则用 T |
| 目标回到基线（B = T） | 没有受保护的组。全部非目标的组都要替换。回滚因此生效 |
| 缩容 | 受保护的排在后面（4.4） |
| 不回退 | 已经是目标版本的组不会为了凑够 P 个而被换回 B |

与按序号的对照：

| 场景 | 按序号（契约 2.12） | 按数量 |
| --- | --- | --- |
| N=3、P=1，三个 v1，提交 v2 | sg-2、sg-1 换成 v2 | 次序靠后的两个换成 v2。相同 |
| 灰度停点上扩容，P 是整数 | 新序号都不小于 P，用 v2 | 非目标的组已经有 P 个，用 v2。相同 |
| 同上，P 是百分比、随 N 变大 | 新序号都不小于新的 P，用 v2 | 非目标的组少于新的 P：不足的部分用 B。**不同**（SG-P04） |
| P 调小 | 解除保护的序号被替换 | 次序最靠后的非目标组被替换。相同 |
| P 调大 | 现存的 v2 不回退；之后落在保护序号上的重建用 B | 现存的 v2 不回退；之后非目标的组少于 P 时，不论哪个组重建、或者扩容，都用 B |
| 受保护的是第三个版本（上一轮没滚完又提交新版本） | 序号小于 P 的不动 | 次序最靠前的 P 个非目标组不动。相同（EX-1） |
| 想让指定的某几个组留在旧版本 | 可以，按序号 | 不可以。只能指定数量，留下的是次序最靠前的 |

Role 一级的 partition 仍然按序号，同一个 API 里有两种语义。API 注释、用户文档里「按序号保护」的说法，对组一级要改。`kubectl get servinggroup` 要有一列显示版本和是否受保护。

先例：OpenKruise CloneSet 的 partition 就是「保留在旧版本的 Pod 的数量」，扩容时旧版本不足 partition 的部分按旧版本创建。LeaderWorkerSet 的 `groupIdentity: Hash` 去掉组序号之后直接拒绝 `partition`。来源见 [01](01-design-rationale.md) 第 6 节。

### 4.4 次序

**次序键**是全序：存量接管来的组在前，彼此按存量序号；其余按创建时间，再按名字。序号命名策略下按序号。「最靠前」对应今天的低序号，「最靠后」对应高序号。同一秒建出来的组靠名字分先后。

| 选择 | 次序（先满足的先被选中） |
| --- | --- |
| 缩容删谁（G2） | 已观测并且未就绪的；过期的；不受保护的；deletion cost 低的；次序最靠后的 |
| 替换删谁（G3） | 已观测并且未就绪的过期组；健康的过期组。同类里次序最靠后的 |
| 受保护的是谁 | 非目标的组里次序最靠前的 |

与今天的差别：

- 缩容时**过期的先**。过期的组反正要换，缩容顺手删掉它，少一次重建。今天的次序里没有这一项，靠的是「临时 surge 不参加缩容」；没有 surge 身份之后要靠它（SG-C12）。
- 缩容时**受保护的排在 deletion cost 前面**。按数量的 partition 的含义是「留 P 个」，缩容不应当因为 deletion cost 低就先删受保护的。今天 partition 不改变缩容的次序。
- 失败的组不在缩容的候选里，只由 G1 删。缩容不能绕过「等故障成员消失」。

### 4.5 创建不幂等

Pod 的名字是确定的，重复创建会撞名字。随机名字的组没有这道挡板。三条规则把多建挡在正常运行之外：

| 规则 | 内容 |
| --- | --- |
| 在途登记 | 发创建请求之前把名字记在内存里。计划时，登记了而缓存里还没有的组算存在、未就绪。`spec` 的下发同样登记：刚 PATCH 过的组，在缓存显示出新的 `generation` 之前按未观测计。否则缓存里那个旧对象的 `observedGeneration` 等于它自己的 `generation`，看起来是「已观测、就绪」 |
| 在途屏障 | 有在途的创建时，这个 ModelServing 的组一级不发别的写。登记带期限（沿用 v2 的 30 秒）；到期按名字直读，不存在就去掉 |
| 接管屏障 | 新进程从一次一致性的 LIST 开始（6.3） |

三条都在时，单个进程不会多建。会多建的只剩一种情形：两个进程同时在写（失主的进程还没退出）。这时多出来的组是最新的、多半未就绪，下一轮按缩容的次序删掉，健康的老组不动；它带着自己的 PodGroup 进过调度队列，资源紧张时可能挤掉别的作业（推断）。

所以「活动的组不超过 N + S」在单进程下每一步成立，在两个进程并存时最终成立。007 的 K4。

### 4.6 `RoleRollingUpdate` 下的组一级

顶层的 `maxUnavailable`、`maxSurge`、`partition` 在这个模式下不生效（契约 0.2）。组一级：

- 不做版本分类，每个 `live` 的组都是目标，`cap` = N。只走 G1、G2、G4、G5、G6。
- 新组的 `spec` 带目标和基线两个版本。受 Role partition 保护的实例按基线建。
- 模板变化通过 G5 写进每个组的 `spec.targetRevision`，由 ServingGroup controller 在组内按 Role 一级的规则滚动。
- G6 的条件按各组报告的 `status.revision` 写：全部组的 `status.revision` 等于 T、已观测、就绪，并且每个 Role 的 partition 都是 0。推进之后 G5 把新的基线写回每个组。

### 4.7 运行中切换滚动模式

契约 2.12：合法的运行中切换只保证最终收敛，不保证过渡过程的预算。本设计里切换就是「分类规则换了一套」：切到 Role 模式，过期的组不再被整组替换，改由组内滚动；切到 ServingGroup 模式，组的版本取 `status.revision`，不一致的组（组内滚到一半）是过期的，按预算替换。没有需要迁移的中间状态。

### 4.8 基线

B 的来源是 `ModelServing.status.currentRevision`。它是下一轮的输入，这一点今天就是如此。

| 情形 | 取值 |
| --- | --- |
| `status.currentRevision` 有值，对应的 ControllerRevision 在 | 用它 |
| 为空，没有任何组 | 取 T |
| 为空，有组 | 从各组的 `spec.baselineRevision` 读回（A4） |
| 读不回来，或者对应的 ControllerRevision 不在 | B 未知。**保护照常**（保护看的是「非目标」，不看 B）；需要用 B 建组的那一步不建，报告，等待 |

B 未知时不猜成最新版本，这是契约 A.4 的「历史模板不可确认时安全等待」。B 被错误地写成 T（旧客户端整体改写 `status`）时保护会失效，这一种挡不住；A4 的副本让它可以被发现和修回。

## 5. Role 一级

### 5.1 沿用的规则

ServingGroup controller 在一个组里面做的事，规则沿用 v2，没有变：

- Role 实例按序号。Role 一级的 N、U、S、P、surge 标记、转正、回收、候选次序（04 第 5 节的 A2–A8）。
- roleCoordination（04 第 8 节）。
- 原地更新和它的接口点（007 的 05 第 4 节）。
- 成员状态的判定（终态、重启故障、grace、`-1`）和 D9、D10、D13、D14。

变了的只有三样：恢复的证据从内存和 Pod 上的标记换成账本（5.2）；两层之间的衔接规则变成对象的边界（组在终止中或已失败，它里面的一切就停）；钩子的调用者（第 7 节）。

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
| L7 | **槽位被别人占着。**直读读到同名而不属于自己的 Pod：不删它；自己的那个成员按丢失处理；创建撞名字时停在 Creating 并报告 | 外来 Pod |

L4 和 L5 是 007 里「没有模型证据」的两条。这次各有一个小模型：去掉 L4，没有丢失也会写 Draining；去掉 L5，新一批的 Pod 被旧的 Draining 删掉，并且恢复反复进行（[05](05-inference-verification.md) 第 4 节）。

每个实例都记账的代价：实例建成时一次 status 写（一轮里多个实例的转换合成一次）。单 Pod 的实例丢了成员，从「一次创建」变成「一次直读、至多三次 status 写、一次创建」。007 对单 Pod 实例免记账；这里不免，因为 `ServingGroupRecreate` 下单 Pod 实例丢了成员同样要让整组失败，计划内删除在任何策略下都要能跨重启续做，并且状态机只有一套。

### 5.3 恢复

「丢失」指已建成的实例里有成员缺失或在终止。「故障」指成员是终态（`Failed`、`Succeeded`），或者重启故障满了 grace。两者的反应相同：

| `recoveryPolicy` | 丢失（已建成的实例） | 故障 | 创建中的实例里成员缺失 |
| --- | --- | --- | --- |
| `None` | 补这一个 | 终态：删这一个再补。重启故障：只报告 | 补 |
| `RoleRecreate` | 写 Draining，清空，重建这个实例 | 同左 | 补 |
| `ServingGroupRecreate` | 写组的 Failed | 同左 | 补 |

- 范围取动手那一刻的 `spec`。
- 终态不等 grace（D13）；重启故障按 grace，`-1` 不触发。
- 最后一列：在网版本连续运行时已经是这样，它只在「本进程见过这个实例建成」之后才把缺失扩大成范围恢复，这个「见过」只在内存里。`main` 和契约在连续运行时不看建没建成，删除事件到了就按范围恢复；对它们这是一处变化（007 的 T14）。
- 「建成过」现在写在对象上：**controller 不在的时候发生的丢失，回来之后照样按范围恢复。**今天两个版本在这种情形下都只补一个。

### 5.4 整组恢复

`ServingGroupRecreate` 下的丢失或故障：ServingGroup controller 把 Failed 写成真。这一次写是提交点。

1. ServingGroup controller 此后在这个组里不创建，只删除故障的成员，并在没有故障成员、没有正在终止的 Pod 时把 Drained 写成真。
2. ModelServing controller 看到 Failed 和 Drained 都为真，前台级联删除这个组（G1）。
3. 组对象在 Pod 都消失之前一直在，一直计入 `active`。
4. 组消失之后，G4 按数量补一个。名字是新的；版本按 4.3 选。

第 1 步的等待保留 D15：故障的成员停下并且消失之前，其余成员继续运行。

删除的机制选前台级联（007 的 K1）：kthena 不在时组照样删得掉，没有新的卡住方式。代价是有人手工用后台方式删组时，组对象立刻消失而 Pod 还在终止，新旧两组的 Pod 短暂并存。

整组恢复之后组名变了，Pod 名字跟着变。这是对外的变化（[07](07-user-and-ops-guide.md)）。

### 5.5 有人直接删了一个组

| 做法 | 结果 |
| --- | --- |
| `kubectl delete servinggroup`（后台，默认） | 组对象立刻消失，垃圾回收器随后删它的 Pod。ModelServing controller 看到少了一个组，按数量补一个新的。旧组的 Pod 还在终止时新组可能已经在创建：容量短暂超过 N + S |
| 同上，`--cascade=foreground` | 与 G1、G2 的删除相同 |
| `--cascade=orphan` | Pod 留下，没有 controller owner。它们带着 ModelServing 和组名的标签，按 [06](06-live-upgrade.md) 的规则被收养：ModelServing controller 重建同名的组，ServingGroup controller 换回 owner |

### 5.6 PodGroup 和 Service

每个组一个 PodGroup、每个需要的 Role 实例一个 headless Service，owner 是 ServingGroup。`minMember` 等字段仍然按 ModelServing 的 gang 配置算，输入通过 G5 进到组的 `spec`。组被删除时它们随级联删除。

## 6. 并发

### 6.1 没有按资源的锁

两个队列，各自一个 key 同时只有一个 worker。回调只入队；回调做的映射（Pod 到它的组，组到它的 ModelServing）是事件对象上的纯函数。两个 controller 的内存按各自的 key 分开，互相不读对方的。

内存里只有三样东西，都只会扣留动作，不会改变动作：

| 内存 | 丢了的后果 |
| --- | --- |
| 在途的写（组的创建；Pod 的创建和删除），带期限 | 有接管屏障时没有后果。没有时：多建组，事后缩回 |
| 故障起点（D9） | 重启故障的恢复晚一个 grace |
| 工作队列的退避 | 重试早一点 |

### 6.2 每个写的前置条件

| 写 | 前置条件 | 被拒绝之后 |
| --- | --- | --- |
| 创建 ServingGroup | 名字先登记；没有在途的创建 | 结果不明：按名字直读 |
| DELETE ServingGroup | UID 和 resourceVersion；前台级联 | 重新计划。组在观察之后有任何变化（包括 `status`），这次删除都不成立 |
| PATCH `ServingGroup.spec` | `sourceGeneration` 不小于对象上的 | 重新入队 |
| UPDATE `ServingGroup.status` | resourceVersion | 本轮后面的删除和创建不发；下一轮重新计划 |
| 创建 Pod | 名字确定。`AlreadyExists` 时核对 owner 和身份 | 不属于自己：L7 |
| DELETE Pod | UID；Draining 下另要代际号不大于 g | 重新计划 |
| UPDATE `ModelServing.status` | resourceVersion | 重新计划 |

每个写只以被写的那个对象为条件。预算是对多个组求和，写不成一个对象上的条件；这一点没有解决（6.5）。

### 6.3 接管屏障

新进程（重启、换主、升级）开始写之前：

1. 对 ModelServing、ServingGroup、Pod 各做一次一致性的 LIST，等缓存追上这次 LIST 的版本。
2. ServingGroup controller 先把每个组处理一遍，写出当前的 `status`。
3. 之后 ModelServing controller 才发组的删除和创建。

第 1 步挡住「缓存里缺最新的组，于是再建一批」和「缓存里的组是旧的，于是删错一个」。第 2 步挡住「停机期间 Pod 坏了，`status` 里 Ready 还是真」。屏障是进程内的一个信号，只扣留动作。

### 6.4 两个进程同时在写

失主的进程在发现失主之前还可能发出写。选主不是 fencing。这段时间里：

| 写 | 结果 |
| --- | --- |
| 建 Pod | 名字确定，幂等 |
| 建组 | 不幂等，会多建，事后缩回 |
| 账本的转换 | resourceVersion 分出先后，只有一边成功 |
| 删 Pod | 带 UID；读到旧的 Draining 的那一边受代际号限制，碰不到新一批 |
| 删组 | 带 UID 和 resourceVersion |
| 下发 `spec` | `sourceGeneration` 挡住旧的一边 |
| `ModelServing.status` | 两边交替覆盖，包括基线。旧的一边退出之后收敛 |
| plugin 钩子 | 重复调用 |
| 缩容、替换 | 两边可能各选一个，一个额度删两个 |

模型里还有一种形态：旧的一边以为实例在创建中、把刚清空的 Pod 又建出来，新的一边再删掉；它持续到旧的一边退出为止，不产生错误的终局（[05](05-inference-verification.md) 第 4 节）。

### 6.5 没有解决的

- **预算是对多个对象求和。**删一个组可以带上它自己的 resourceVersion；别的组在观察之后变了，挡不住。
- **健康额度按晚一跳的 `status` 算。**Pod 变化，组的 `status` 才变，ModelServing controller 才看到。一个组刚丢了就绪、`status` 还没更新的那一小段时间里，ModelServing controller 可能多删一个健康的组。今天单个 controller 直接读 Pod 的缓存，也有这种滞后，只是少一跳。模型里这一条可达（[05](05-inference-verification.md) 的 L1）。
- **并行度的上限在限流上。**不同的组并行，但每个生命周期步骤的写也多了。没有测量。

## 7. plugin 调用点

框架的四个钩子和原地更新的 plugin 接口，调用者都从 ModelServing controller 换成 ServingGroup controller。特定实现的 plugin 不在这里分析。

| 钩子 | 今天（走读） | 本设计 | plugin 作者要核对的 |
| --- | --- | --- | --- |
| `OnPodCreate` | 创建 Pod 之前，`model_serving_controller.go:3242` | ServingGroup controller，创建 Pod 之前。位置不变 | `HookRequest.ServingGroup` 是组名，随机后缀时不能再解析出序号；需要序号的 plugin 只能用在序号命名的 ModelServing 上 |
| `OnPodReady` | Pod 就绪时，`:2156` | ServingGroup controller，同一个时机 | 接管之后可能对已经就绪的 Pod 再调一次 |
| `OnRoleSync` | 每轮同步一个 Role 实例时，`:1547` | ServingGroup controller，每轮 | 传入的 owner：plugin 创建的对象挂在 ServingGroup 名下 |
| `OnRoleDelete` | 删除一个 Role 实例时，`:3118`；删除整个组时对每个实例调（`:3369-3375`） | **实例被清空**：写 Draining 之后、删 Pod 之前调；离开 Draining 的那次 status 写之前，本进程必须成功调过一次。**整个组被删**：ServingGroup controller 看到 `deletionTimestamp` 时尽力调一次，不保证 | 幂等。**组被删时不保证调到**：plugin 的集群内资源要挂在 ServingGroup 名下，靠垃圾回收清理。在钩子里清理集群外资源的 plugin 要另想办法 |
| 原地更新的 plugin 接口 | 打补丁时 | ServingGroup controller | 无变化 |

`HookRequest` 另带上实例的代际号，plugin 可以用它分辨同一个序号上的新旧两批。

`OnRoleDelete` 在组删除时不再有保证，是本设计对 plugin 作者唯一收紧的地方。它是「组没有 finalizer」的直接后果。两个没选的做法：给组加 finalizer（kthena 不在时组、namespace、CRD 删不掉）；删组之前加一段由 `spec` 触发的清空协议（有保证，多一跳，controller 不在时组删不掉）。

内置的 headless Service plugin 创建的 Service 挂在 ServingGroup 名下，随组级联删除，不依赖钩子。

## 8. 周边组件

### 8.1 驱逐 webhook

今天按「owner 是 ModelServing」认 Pod。改成认两种归属：owner 是 ServingGroup、它的 owner 是这个 ModelServing；或者 owner 直接是这个 ModelServing（接管到一半）。预算计数两种都算。只改一处会让保护无声失效。

### 8.2 autoscaler 和 HPA

autoscaler 仍然改 ModelServing 的副本数和 Role 的 replicas；到 Pod 多经过一个 controller。Role 实例的序号保留，HPA 的 selector 靠「0 号实例」保证每组一个 Pod 的做法不变（走读，`model_serving_controller.go:2896-2912`）。两个进程并存时多建出来还没缩回的组也会被选中。

### 8.3 router 和别的读标签的组件

`group-name` 标签只被当作不透明的标识（007 的走读）。不受影响。解析它、或者依赖 `<名>-0-...` 这种固定名字的用户脚本、告警、固定 DNS 名受影响（[07](07-user-and-ops-guide.md)）。

### 8.4 校验 webhook

名字长度今天按副本数的位数估计（走读，`webhook/validator.go:162-179`），并且每次 UPDATE 都跑。随机后缀更长。新的或收紧的校验只拦新引入的：对象上没被这次更新改动的值放行（007 的 05 第 2.1 节的规则）。否则存量的长名字对象的任何更新都被拒，包括 autoscaler 的 PATCH。

### 8.5 PodDisruptionBudget

用 `maxUnavailable` 或百分比的 PDB，要通过 Pod 的 controller owner 的 `scale` 子资源取期望数（按 Kubernetes 的 disruption controller 的源码，没有在集群上验证）。今天 owner 是 ModelServing，它有 `scale` 子资源，`specReplicasPath` 是 `.spec.replicas`，也就是组的个数，不是 Pod 的个数。owner 换成 ServingGroup 之后：

- ServingGroup 没有 `scale` 子资源时，这类 PDB 解不出期望数，驱逐全部被拒。
- 所以 ServingGroup 要有 `scale` 子资源。`spec` 里加一个由 ModelServing controller 写的 `expectedPods`（这个组期望的 Pod 数），`specReplicasPath` 指向它；`status` 里对应的是现有的 Pod 数。它只给 PDB 读；有人通过 `scale` 改它，下一轮被写回。

这样 PDB 的百分比第一次是按 Pod 数算的。用整数 `minAvailable` 的 PDB 不受影响。

### 8.6 ControllerRevision 的清理

今天清理历史版本时保留「controller owner 是这个 ModelServing 的 Pod 引用到的版本」，每次直接 LIST Pod（走读，`utils/controller_revision.go:228-262`）。owner 换成 ServingGroup 之后这个判断对所有 Pod 都不成立，会删掉还在用的历史版本。改成保留：B、T、每个现存的组的 `spec` 里出现的版本、每个组 `status.referencedRevisions` 里的版本（A6）。

### 8.7 协调模式的基线

roleCoordination 今天把滚动的基线写在 ControllerRevision 的注解上。ServingGroup controller 不写 ControllerRevision；这份基线由 ModelServing controller 通过 G5 下发到组的 `spec`。

## 9. 原地更新

原地更新整个在 ServingGroup controller 里面，接口点沿用 007 的 05 第 4 节。相对 007 多出一处：

- `ServingGroupRollingUpdate` 配原地更新时，组不被替换，组的版本原地变。所以 `spec.revision` 在这种配置下可以被 G5 改写；组的版本以 `status.revision` 为准，更新途中的组按「目标、未就绪」计。成员分类先看原地更新的进度注解，再看重启故障：正在按预期重启的容器不是故障。

007 的 05 里要裁定的限制（T17–T21、T26）不因本设计改变，仍然等裁定。

## 10. 没有了的和新增的

| 没有了的 | 范围 |
| --- | --- |
| 跨多个 Pod 的恢复承诺（v2 的标记和 M1–M7；在网版本的 `deletion-scope` 注解和内存里的删除计划） | 全部 |
| 「建成过」只在内存里 | 全部 |
| 组一级的 surge 标记、转正、回收，待决项 O11 | 组一级。Role 一级还在 |
| 序号空洞、补洞、身份债 | 组一级 |
| 每次滚动多建又多删的 S 个组 | 组一级 |
| 同序号上新旧两批 Pod 的隔离（在网版本的实例身份注解、创建前的 LIST） | 组一级没有这个问题；Role 实例一级换成代际号 |
| 两层共用一份工作副本的衔接规则 | 变成对象的边界 |
| 按 ModelServing 的互斥锁（在网版本） | 全部 |
| 整个 ModelServing 串在一个 key 上 | 不同的组并行 |

| 新增的 | 代价 |
| --- | --- |
| 一个 CRD | API 面、RBAC、生成的 client、文档、chart、模板类型改名 |
| 第二个 controller 和 A1–A6 | 状态汇总晚一跳 |
| `status` 当输入 | L2、L4、L5；`status` 端点要用 RBAC 保护 |
| 创建不幂等 | 4.5 的三条规则 |
| 按数量的 partition | 一处对外的语义变化；和 Role 一级的按序号并存 |
| 接管屏障和部署前提 | 选主、失主退出 |
| 稳态下的写 | 每次就绪变化从 1 次 status 写变成 2 次；对象多 N 个 |
| 一次升级 | [06](06-live-upgrade.md) |

## 11. 相对 007 的 v4 改了什么

| 007 的 04 | 这里 | 原因 |
| --- | --- | --- |
| 第 6 节是「规则的轮廓」 | 4.1–4.8，一套完整的算式和次序 | 终稿 |
| 6.3：保护版本等于基线的组 | 保护非目标的组，不问版本 | 基线没有推进时，现存的第三个版本的组会被换回基线（EX-1） |
| 6.4：缩容时「既不是基线也不是目标的先」，deletion cost 在「基线后」前面 | 「过期的先」；「受保护的后」在 deletion cost 前面 | SG-C12 会删掉最新的目标组；多出一个组时调大 P 会删掉受保护的组（EX-2） |
| 6.4：失败的组在缩容次序里排第一 | 失败的组只由 G1 删 | 否则缩容绕过 D15 |
| 7.1：等两个计数为 0 | Drained condition | 计数的零值正好是「可以删」 |
| 第 5 节：单 Pod 的实例不记账 | 每个实例都记 | 5.2 |
| 第 5 节：缺失之前直读一次 | 任何用来写 Draining、Failed 的证据都直读 | 缓存里上一代的终止中 Pod 会被当成证据 |
| 第 5 节：代际号一句话 | L5 的四句 | `status` 被清空之后 Draining 会卡住 |
| 6.2：多建是常态（缓存滞后、重启、超时、两个进程） | 只在两个进程并存时 | 在途屏障和接管屏障 |
| 组的 DELETE 带 UID | 带 UID 和 resourceVersion | 缓存旧时会删掉一个已经恢复的组 |
| K3 LWS、K7 deletion cost 待定 | 2.2、A5 | 本任务要求给出推荐 |
| 组的 `spec` 的 PATCH 不带条件 | `sourceGeneration` | 缓存旧的进程会把旧 `spec` 写回去 |
| `OnRoleDelete` 没有提 | 第 7 节 | 组被删时今天会调 |
| ControllerRevision 的清理没有提 | 8.6 | 按 owner 的判断失效，会删掉在用的历史版本 |
| PDB 没有提 | 8.5 | 用百分比的 PDB 要读 owner 的 `scale` 子资源 |
| 第 12 节迁移要点 | [06](06-live-upgrade.md) | 本任务的要求 |
