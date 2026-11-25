# ServingGroup 作为对象：实现设计

## 0. 文档信息

| 项 | 内容 |
| --- | --- |
| 用途 | [proposal](modelserving-servinggroup-cr.md) 的实现设计。写给实现者和评审代码的人：API 逐字段定稿、ServingGroup `status` 的设计、两个 controller 的代码结构、版本升级的做法、测试和提交计划 |
| 两个重点 | 第 3 章（`status`）和第 6 章（版本升级） |
| 规则的来源 | 分支 `feat/008-modelserving-servinggroup-cr-design` @ `d5686ba8` 的 `docs/proposal/modelserving-servinggroup-cr/`（下面称「008」）：02 是规则，03 是状态机，06 是升级。这里不重抄规则，只写规则怎样落到代码上，和实现时定下来的地方。规则的编号原样引用：A1–A7（两个 controller 之间的约定）、L1–L8（账本）、S1′ 和 G0–G6（组一级的一轮）、E0–E9（Role 实例的转换）、S1–S9（组的转换）、M0–M10 和 R0–R11（两条 reconcile 线）、T1–T7（接管）、X1–X4（独占） |
| 基线 | `volcano-sh/kthena` `main` @ `bc26f686`。008 的走读基线是 `79c455ef`，两者之间的差别见 1.2 |
| 升级的源版本 | `v1.1.0`（`release-1.1` @ `24f0e136`）和变更前的 `main` @ `bc26f686` |
| 行为的依据 | `kthena-rollout-runner` 的契约 2.13，加上已经批准的五条（008 的 04 第 11 节的 C1、C2、C3、C4、C12）和三处声明过的字面出入 |
| 状态 | 随实现更新。哪些已经实现、哪些在 Kind 上验证过，记在工作区任务 010 的 `PROPOSAL_COMMIT.md` 里，不记在这里 |

## 1. 范围

### 1.1 做什么，不做什么

| 做 | 不做 |
| --- | --- |
| `ServingGroup` CRD 和它的 `status` | 把 Role 实例做成对象 |
| ServingGroup controller（一个组里面的事） | 改 ModelServing 的 `spec` 的任何字段 |
| 新的 ModelServing controller（组一级） | 在网分支独有的部分：带 `deletion-scope` 标记的 Pod（T2）、多出的三个 plugin 钩子、老的 headless Service 布局（P3） |
| 从 v1.1.0 和变更前的 `main` 就地接管存量负载 | 把重启故障的起点持久化（D9 不变） |
| 独占（X1、X2、X4），最后一个里程碑做 X3 和受控回退 | 新旧两条路径并存、功能开关 |
| 驱逐 webhook、校验 webhook、chart、RBAC 的配套改动 | `revisionHistoryLimit` 的清理（今天就没有实现） |

### 1.2 基线相对 008 的变化

`git diff 79c455ef bc26f686` 里与 ModelServing 有关的两处：

| 变化 | 处理 |
| --- | --- |
| plugin 框架多一个钩子 `OnModelServingSync`，返回 `HookResult{RequeueAfter, Err, Continue}`；`BasePlugin` 提供空实现。内置的 plugin 都没有实现它。今天阻塞性的错误（`Continue` 为假）让整轮 reconcile 在建任何 Pod 之前返回 | 两个 controller 都调。ModelServing controller：目标版本解析出来之后（M4）、计划之前（M6）；阻塞性的错误让这一轮不做 M6 到 M8，`status` 照写。ServingGroup controller：观察之后（R3）、Role 一级的计划之前（R6），用这个组的工作负载视图；阻塞性的错误让这一轮不建、不删、不打补丁，`status` 照写并带 `ReconcileFailed`。`RequeueAfter` 大于 0：延时入队。这样「钩子阻塞时不建 Pod」仍然成立 |
| partition 过滤和「已移除的 Role」抽成辅助函数 | 只是重构 |

### 1.3 与 008 不同的地方

实现时定下来、与 008 的文字不同的地方集中列在这里。理由和没有选的做法在任务 010 的 `decisions.md`。

| # | 008 | 这里 | 为什么 |
| --- | --- | --- | --- |
| V1 | `Failed`、`Succeeded` 的 Pod 在任何策略、任何 grace 下都算故障；`None` 下删这一个再补（005 的 D10、D13） | 与今天的代码和契约 2.13 相同：故障是「`Failed`，或者有计划外的容器重启并且不 Ready」；有限的 grace 对两种同样计时；`None`、`-1` 下不因故障删 Pod；`Succeeded` 不是触发条件 | D10、D13 与契约不同，不在已批准的条款里。照 008 做是一处没有批准的行为变化 |
| V2 | 「期望成员的名字」里的 role 按 Pod 的版本判断 | 只按标签和名字的格式判断 | 两个 controller 要对同一批 Pod 得出同一个成员集合；upstream 上 Role 的名字不可变，查版本没有必要 |
| V3 | `spec` 只列了版本、副本数和恢复策略 | `spec` 另带滚动方式、生效的更新方式、进度期限、协调配置、gang、拓扑、队列，和每个 Role 的滚动参数 | ControllerRevision 里只有调度器名、plugin 和 Role 的模板 |
| V4 | `status.deletionCost`；给人看的阶段 | 都不存。打印列从 condition 取 | 没有读者；API 约定不鼓励 `phase` |
| V5 | 配了原地更新而 plugin 不支持时，由 ServingGroup controller 写 `RecreateRequired`（DR21） | plugin 链支不支持原地更新、U 是不是 0，在 ModelServing 一级定成「生效的更新方式」；两个版本之间是不是只改了镜像，由 ModelServing controller 在改 `spec.revision` 之前判断。`RecreateRequired` 只留给要看 Pod 才知道的原因 | 今天这两样就是按 ModelServing 一次判定的；照 DR21 做，用 headless Service plugin 的负载会多走一步「改版本、报告、再删」 |
| V6 | 版本只有「等于 T」和「不等于 T」 | 三值：目标、其它、未知。未知的组不可替换，也不算目标 | 今天模板比较就是三值的；读不到历史不能当成「不同」去删 |
| V7 | Role 一级的事件记在 ServingGroup 上（DR23） | `RoleCreating`、`RoleRunning` 这类 Role 状态事件照旧记在 ModelServing 上 | upstream 的 e2e 断言事件的对象是 ModelServing；改断言是行为变化 |
| V8 | 整个组被删时尽力调一次 `OnRoleDelete` | 只在组的 `finalizers` 里有 `foregroundDeletion` 时调 | orphan 方式删组时 Pod 还要被收养回来，这时调钩子会删掉它们的 Service |
| V9 | 清空一个实例时，`OnRoleDelete` 在删 Pod 之前调（E2） | 在这个实例的 Pod 都消失之后、账本改写之前调（E3）。钩子不成功：账本停在 Draining，实例不重建，组报 `DrainBlocked` | 今天就是 Pod 删完之后才调。账本在钩子成功之前不动，所以「至少调一次」不受影响 |
| V10 | `Available`、`Progressing`、`UpdateInProgress` 同一时刻至多一个为真；message 写新的事实 | 原样沿用今天的 `utils.SetConditionWithRolloutAndProgressState`：`Progressing` 和 `UpdateInProgress` 可以同时为真，已经为真的 condition 不刷新 message | 008 对今天的描述不准确。照它做是一处行为变化，CLI 的显示次序也是按今天的语义写的 |
| V11 | `status.currentRevision` 为空而有组时，读不回来就等（`BaselineUnknown`） | 平时照 008。只有接管（T3）这一处，`status.currentRevision` 为空时照今天的做法取存量的组里出现最多的非目标版本，都是目标版本时取 T | 接管来的组上还没有基线的副本可读。旧版本在初次创建完成之前 `currentRevision` 是空的 |

### 1.4 对 upstream 用户的行为变化

三类。前两类已经有批准；第三类是今天的代码与契约 2.13 不同、这里照契约的地方，在任务记录里单独列给用户。

| 类 | 变化 |
| --- | --- |
| 已批准的条款 | C1 账本。C2 controller 不在时丢的成员按范围恢复。C3 实例建成之前丢的只补。C4 没有 Pod 上的删除标记；做了一半的删除由下一个进程接着做完。C12 Failed、Draining 写上之后不撤销 |
| 设计决定带来的 | 每次整组替换和整组恢复，组对象、PodGroup、Service 都是新的（名字不变）。选主默认打开，失主退出。新的 condition `RolloutBlocked`。`OnRoleDelete` 在整组被删时只尽力调。一个失败的组一级的写让这一轮后面的创建和删除不发。恢复策略和 grace 取动手那一刻的：等 grace 的期间改成 `None`、`-1` 或者调大 grace，恢复取消（今天睡眠的 goroutine 用的是故障那一刻抓下来的值）；恢复过再坏的 Pod 重新计 grace |
| 今天与契约不同、这里照契约 | 未就绪的受保护的组占可用性的额度（今天不占；`TestServingGroupRollingUpdateIgnoresUnavailableProtectedGroups` 断言的是今天的行为）。surge 的组总是建在不小于 N 的序号上并带标记（今天在稀疏布局下可以落在小于 N 的洞里、不带标记）。缩容不选 surge 的组。缩容删掉的组消失之前不开始替换。基线只在 P 是 0、没有空组时推进 |

没有变的：名字、标签、ModelServing 的 API、`status` 的计数和三个 condition、事件的对象和 reason、故障的定义和 grace、`None` 和 `-1` 的含义。

## 2. API

类型在 `pkg/apis/workload/v1alpha1/servinggroup_resource_types.go`。模板类型原来叫 `ServingGroup`，改名 `ServingGroupTemplate`（JSON 不变）。

### 2.1 `spec`

`spec` 只有 ModelServing controller 写。它带两类东西：这个组的版本；ModelServing 上不属于版本、而在一个组里面要用到的设置。

| 字段 | 取自 | 谁用 |
| --- | --- | --- |
| `revision` | `ServingGroupRollingUpdate`：这个组的版本。`RoleRollingUpdate`：空 | 组里每个实例的模板 |
| `targetRevision` | `RoleRollingUpdate`：T | Role 一级滚动的目标 |
| `baselineRevision` | `ModelServing.status.currentRevision` | 受 Role partition 保护的实例；A4 |
| `sourceGeneration` | `ModelServing.metadata.generation` | A2 |
| `expectedPods` | 这个组的版本的模板：Σ `replicas` ×（1 + `workerReplicas`）。`RoleRollingUpdate` 下取 T 的布局 | `scale` 子资源；模板读不到时的 Ready（A1） |
| `rolloutStrategy.type` | `spec.rolloutStrategy.type` | 哪一层的滚动参数生效 |
| `rolloutStrategy.updateMethod` | **生效的**更新方式（1.3 的 V5） | 原地更新 |
| `rolloutStrategy.progressDeadlineSeconds` | 同名字段 | 原地更新的期限 |
| `rolloutStrategy.roleCoordination` | 同名字段 | 协调 |
| `recoveryPolicy`、`restartGracePeriodSeconds` | 同名字段 | 恢复 |
| `gangPolicy`、`networkTopology`、`queueName` | 模板的同名字段；队列取 ModelServing 的注解 | PodGroup |
| `roles[]`：`name`、`replicas`、`maxUnavailable`、`maxSurge`、`partition`、`baselineReplicas` | 模板里每个 Role；`baselineReplicas` 取基线版本记下的副本数 | Role 一级 |

三条约束：

1. **没有 Pod 模板。**模板、plugin、调度器名从 ControllerRevision 读。ControllerRevision 的数据就是这三样（`utils/revision_data.go` 的 `modelServingRevisionSpec`），别的设置它没有，所以要经 `spec` 下发。
2. **没有 schema 默认值。**apiserver 补上默认值之后，存下来的 `spec` 就和 ModelServing controller 算出来的不一样，「内容不同才下发」永远成立，组永远是未观测。ModelServing 上的字段已经带着它自己的默认值，照抄即可。Kind 上验证过：写进去的 `spec` 原样读回，`generation` 是 1。
3. **版本的值是 revision 的身份**：Pod 的 `modelserving.volcano.sh/revision` 标签、`status.updateRevision` 用的那个字符串。ControllerRevision 对象的名字是 `<ModelServing 名>-<这个字符串>`。

### 2.2 `status`

见第 3 章。

### 2.3 标签、注解、子资源

| 对象 | 内容 |
| --- | --- |
| ServingGroup 的名字 | `<ModelServing 名>-<序号>` |
| ServingGroup 的标签 | `modelserving.volcano.sh/name` |
| ServingGroup 的注解 | `modelserving.volcano.sh/surge: serving-group`：创建时写，转正时摘，不后加 |
| ServingGroup 的 owner | ModelServing，`controller`、`blockOwnerDeletion` |
| Pod、PodGroup、Service 的 owner | ServingGroup，`controller`、`blockOwnerDeletion`。PodGroup 今天没有 `blockOwnerDeletion`，补上 |
| Pod 的标签 | `modelserving.volcano.sh/instance-generation`：所在实例的代际号 |
| Pod 的注解 | `modelserving.volcano.sh/surge`：值 `role` 是 Role 一级的标记（来源就在 Pod 上）；值 `serving-group` 是组的标记的副本（输出） |
| ModelServing 的注解 | `modelserving.volcano.sh/servinggroup-adoption`：`InProgress`、`Done`、`RollingBack`（第 6 章） |
| ModelServing 的 condition | `RolloutBlocked`（3.11） |
| `scale` 子资源 | `spec.expectedPods`、`status.pods`、`status.labelSelector` |
| 短名、打印列 | `sg`；`REVISION`、`READY`、`PODS`、`EXPECTED`、`AGE`，宽输出另有 `FAILED` |

`scale` 子资源只给 PodDisruptionBudget 解析期望数用：PDB 对它选中的每个 Pod 找 controller owner，把这些 owner 的 `scale` 相加。选中一个 ModelServing 全部 Pod 的 PDB，分母是各组 `expectedPods` 之和，是对的。只选中某一种 Role 的 PDB，分母仍然是整组的 Pod 数，偏大；这是已知的限制，写进用户文档。今天 owner 是 ModelServing，分母是组的个数，比例型的 PDB 本来就不对。

## 3. ServingGroup `status` 的设计

### 3.1 原则

1. **`status` 里只有四样是事实，其余都是汇总。**事实是：账本每一项的阶段和代际号；Failed；RecreateRequired；`pods` 里「已经决定要建」的那一位（3.6）。汇总每一轮从对象重算，算出来的和存着的不同就写。所以 `status` 被清空之后，下一轮会把汇总补回来；事实丢了的后果在 3.8。
2. **事实的零值是不会引起删除的状态。**账本的零值是 Creating：缺的补，别的不动。Failed 的零值是没有失败。
3. **每一次写带 resourceVersion。**两个进程同时写，只有一个成功；被拒绝的那一个这一轮后面不发任何删除和创建。
4. **先写后做。**凡是要让别的成员跟着被删的决定（清空一个实例、整组失败），先把决定写进 `status`，再动 Pod。
5. **`status` 要说明它是按什么算的。**`observedGeneration` 说明按哪一份 `spec`，`observedPods` 说明按哪些 Pod。ModelServing controller 只在两样都对得上时才把 condition 当真。

### 3.2 字段

| 字段 | 性质 | 谁读 | 说明 |
| --- | --- | --- | --- |
| `observedGeneration` | 汇总 | ModelServing controller（A1） | 这一轮的决定是按这份 `spec` 做的。出错的轮次也推进 |
| `observedPods` | 汇总 | ModelServing controller（A7） | 成员 Pod 的摘要，3.3 |
| `conditions` | Failed、RecreateRequired 是事实；其余是汇总 | 3.4 | |
| `revision` | 汇总 | ModelServing controller：`RoleRollingUpdate` 下的 G6、切换滚动模式、`updatedReplicas` | 不在终止中的成员 Pod 运行的版本，从 Pod 的标签读，不抄 `spec.revision`。版本不一致或者没有 Pod 时为空。原地更新做完之前 Pod 的标签是旧的，这里也是旧的 |
| `referencedRevisions` | 汇总 | ModelServing controller：清理 ControllerRevision（A6） | 成员 Pod 引用到的版本，含终止中的 Pod，含没做完的原地更新的目标版本。排过序 |
| `pods` | 含一位事实 | ModelServing controller：「没有成员」；`scale` | 3.6 |
| `readyPods`、`faultyMembers`、`terminatingPods` | 汇总 | 人 | 判断不用它们 |
| `labelSelector` | 汇总 | `scale` | `modelserving.volcano.sh/group-name=<组名>` |
| `roleInstances[]` | 事实 | ServingGroup controller 自己 | 账本，3.5 |

### 3.3 `observedPods`：这份 `status` 是按哪些 Pod 算的

**问题。**组的就绪经过 `status` 才到 ModelServing controller，晚一跳。一个组刚丢了成员、`status` 还写着就绪的那一段里，它不能占健康的额度；一个组刚恢复、`status` 还写着未就绪的那一段里，它不能被当成未就绪的旧组不占额度地删掉。进程重启之后，停机期间变了而没有人写过 `status` 的组也是这种情形。

**做法。**ServingGroup controller 每次写 `status`，写上它这一轮用的那批成员 Pod 的摘要。ModelServing controller 用自己的 Pod 缓存、同一个函数算同一个摘要。相同：这份 `status` 是**新鲜的**。不同：这个组按**未观测**算，占位置，不进 `ready`，不是替换的候选。ModelServing controller 不另算一遍就绪。

代码在包 `pkg/model-serving-controller/servinggroup`，两个 controller 都从这里调：

| 函数 | 内容 |
| --- | --- |
| `MembershipOf`、`Members` | 一个 Pod 是不是这个组的成员。成员是：controller owner 的 UID 是这个组的 Pod（不看标签，终止中的也算）；加上待接管的，三种：没有 owner 而带代际号标签的（orphan 删组留下的）、owner 还是 ModelServing 的（旧版本建的）、没有 owner 也没有代际号标签而接管没做完的。待接管的 Pod 要不在终止中，标签和名字是这个组一个成员的：`<组名>-<role>-<实例序号>-<成员序号>` |
| `Project` | 把 Pod 缩成判定用到的字段：UID、名字、`deletionTimestamp`；八个标签（ModelServing 名、组名、role、role-id、entry、revision、role-template-hash、代际号）；原地更新的进度注解、值是 `role` 的 surge 注解；controller owner；容器的名字和镜像；`phase`；Ready 的值和 `lastTransitionTime`；init 容器的重启次数；容器的重启次数、`containerID`、`image`、`imageID` |
| `Digest` | 成员按名字排序，逐个把投影编码成 JSON，取 SHA-256 的前 128 位，写成 `v1.<32 个十六进制字符>` |
| `ClassifyMember` | 成员状态的判定。它第一步就调 `Project`，之后只读投影 |

三点说明。

- **按构造覆盖。**判定只读投影，摘要正好覆盖投影。所以「判定的结果变了而摘要没变」不会出现。投影比 008 的 4.1 列的多四样（容器的 `containerID`、`image`、`imageID` 和 `spec` 里的镜像）：原地更新是否完成靠比较它们。
- **不在投影里的。**`resourceVersion`、`pod-deletion-cost`、别的 condition、condition 的 message、节点名、IP、值是 `serving-group` 的 surge 注解（它是输出）。binpack 周期性地改 deletion cost、调度器改调度不上的 Pod 的 message，都不让摘要变，也就不引起 `status` 的写。
- **带版本。**以后投影或者编码变了，换成 `v2.`。新旧两个版本算出来的摘要不相等，所有的组在升级之后的第一轮里是不新鲜的，等 ServingGroup controller 各重写一次。这是安全的一侧，代价是升级之后的头一轮慢。

**摘要相等证明的是什么。**它证明两个 controller 是按同一批 Pod 的同一个样子说话，不证明这批 Pod 就是 apiserver 上现在的样子：两边的缓存可以一样旧。所以它不能代替写的前置条件，删除照样带 UID 和 resourceVersion。社区里没有找到给一组 Pod 算摘要的先例（调研见任务 010 的 `subagents/community-research.md`）；Deployment 对 ReplicaSet 的 `status` 不做新鲜度的检查，它多删的是一个 Pod，这里多删的是一个组。

**ServingGroup controller 用的是直读时。**有写要发的轮次，它按直读到的 Pod 算 `status`，摘要也按直读到的算，可以比 ModelServing controller 的缓存新。这时两边不相等，组按未观测算，直到缓存追上。缓存追上时 ModelServing controller 要有一轮：它也订阅成员 Pod 的事件（映射到所属的 ModelServing），另外在有组不新鲜的轮次之后带退避地重新入队。

### 3.4 condition

七个 condition。写的时候用 `meta.SetStatusCondition` 的语义：`lastTransitionTime` 只在 `status` 翻转时变（`RolloutBlocked` 的期限从它起算）；每个 condition 的 `observedGeneration` 是组的 `metadata.generation`。

| condition | 为真 | 假或者没有 | reason | ModelServing controller 怎样读 |
| --- | --- | --- | --- | --- |
| `Ready` | 每种 Role 的实例个数等于 `replicas`，每个实例已建成、成员齐全、全部就绪。各 Role 的副本数都是 0 的组为真。按每个实例自己应用的版本的布局算，不按 `spec.revision`。模板读不到时：现有的 Pod 不少于 `spec.expectedPods`、全部就绪、没有终止中的 | 其余 | `AllInstancesReady`；`InstancesNotReady`；`InstanceCountMismatch`；`TemplateUnresolved` | 额度里只在已观测并且新鲜时才算数；对外的 `availableReplicas` 取最后一次写上的值 |
| `Failed` | 这个组要整个删掉重建。**不可逆**：ServingGroup controller 读到的对象上是真，就不再写成假 | 没有失败 | `MemberLost`、`MemberFaulty`（`ServingGroupRecreate`）；`AllPodsLost`（`ServingGroupRollingUpdate` 下一个成员 Pod 都没有了） | 组的类别是「失败」。与 `Drained` 一起为真并且新鲜：G1 删除 |
| `Drained` | 只在 `Failed` 为真时写。没有故障的成员，没有终止中的 Pod | 还没有清空。`Failed` 不为真时没有意义，不读 | `NoFaultyMember`；`FaultyMembersRemain` | 见上 |
| `RecreateRequired` | 这个组只能删掉重建：配了原地更新而它的 Pod 打不了补丁；收养来的成员版本与组的不同。只加不撤 | 可以按配置的方式更新 | `InPlaceUpdateNotPossible`；`AdoptedRevisionMismatch` | 可替换，不看版本，不看 partition |
| `DrainBlocked` | 有实例停在清空中：`OnRoleDelete` 不成功 | 没有 | `RoleDeleteHookFailed` | 不读 |
| `ReconcileFailed` | 这一轮出错。`Ready` 仍然按看到的成员照实写 | 这一轮没有出错 | `TemplateUnresolved`；`NameOccupied`；`Error` | 不读；ModelServing 的 message 里引用 |
| `CoordinatedRoleRolloutBlocked` | 与今天 ModelServing 上的同名 condition 相同，按组报告 | reason 分 `ProgressAvailable` 和 `RolloutComplete` | 与今天相同 | 汇总：有组为真时取序号最小的那个组的 reason 和 message |

几条容易错的：

- `Failed` 为真时 `Ready` 照实写，ModelServing controller 不看它。
- `Failed` 和 `Drained` 可以在同一次写里都为真（S3′：一个 Pod 都没有了）。
- `Drained` 可以从真回到假（S4′：又有成员故障）。这时 Pod 变了，摘要对不上，G1 的条件本来就不成立。
- `RecreateRequired` 为真的组如果 Pod 是健康的，`Ready` 是真，照常计入可用数，按健康的额度替换。

给人看的打印列：`READY` 是 `Ready` 的值，`PODS` 是 `readyPods`，`EXPECTED` 是 `spec.expectedPods`。

### 3.5 账本 `roleInstances[]`

每个 Role 实例一项：`role`、`index`、`phase`、`generation`。它是 `status` 里唯一不能从观察重建的表。

| 阶段 | 含义 | 实例里缺一个成员时 |
| --- | --- | --- |
| 没有这一项，或 `Creating` | 创建中。零值 | 补这一个 |
| `Established` | 已建成：全部成员都在缓存里看到过 | 丢失。按 `recoveryPolicy` 的范围恢复 |
| `Draining` | 清空中：这个实例代际号不大于 `generation` 的 Pod 要全部删掉 | 接着删 |

**成员状态。**先对每个成员 Pod 判定状态（`ClassifyMember`），自上而下取第一个成立的：

| 状态 | 条件 |
| --- | --- |
| `Terminating` | `deletionTimestamp` 非空 |
| `Updating` | 原地更新没有做完，并且没有故障 |
| `Ready` | `Running` 并且 Ready |
| `NotReady` | 没有故障：既不是 `Failed`，也没有计划外的容器重启 |
| `Faulty` | 有故障，并且故障算数：所在的范围已经提交（组是 Failed，或者实例的账本是 Draining(g) 而这个 Pod 的代际号不大于 g）；或者恢复生效并且从本进程第一次看到起满了 grace |
| `Failing` | 有故障而不算数：恢复不生效（`None`、`-1`），或者没满 grace |

故障的定义与今天相同：`phase` 是 `Failed`，或者有计划外的容器重启；后者已经 Ready 的不算。原地更新期间预期之内的重启不算计划外。「恢复生效」和 grace 取组的 `spec` 里的副本和缓存里所属 ModelServing 两者中保守的一个（`Recovery.Cautious`）。故障的起点在 `FaultTracker` 里，以 Pod 的 UID、Ready 的 `lastTransitionTime`、原地更新的注解为键；它是唯一不在对象上的输入，新进程重新计满一个 grace（D9）。

**实例的转换。**每一轮对每个实例自上而下取第一个成立的。与 008 的 03 第 4.3 节相同，只有 E5 按 1.3 的 V1 改了：

| # | 触发 | 反应 | 写 |
| --- | --- | --- | --- |
| E0 | 组在终止中 | 不做事 | — |
| E1 | 组已失败 | 只删这个实例里 `Faulty` 的成员 | 删 Pod |
| — | 实例里有待接管的成员 | 换 owner；这一轮不为它创建，不删它的成员 | 改 Pod 的 owner |
| E2 | 账本是 Draining(g)，还有代际号不大于 g 的 Pod | 两步：先删 `Faulty` 的；没有 `Faulty` 的、也没有 `Terminating` 的之后，删其余的 | 删 Pod |
| E3 | 账本是 Draining(g)，没有代际号不大于 g 的 Pod | 先调 `OnRoleDelete`；不成功：不动账本，组报 `DrainBlocked`，下一轮重试。成功之后，还在期望之内：写 Creating(g′)，g′ = max(g, 现有 Pod 的最大代际号) + 1。不在：去掉这一项 | 钩子；`status` |
| E4 | 不在期望之内，并且有账本项或者有 Pod | 写 Draining(g) | `status` |
| E6′ | `ServingGroupRollingUpdate`；组期望有成员；有已建成的实例；一个成员 Pod 都没有了；直读确认过。对整个组判，先于 E5、E6 | 写 Failed 和 Drained | `status` |
| E5 | 有 `Faulty` 的成员；直读确认过 | `RoleRecreate`：写 Draining(g)。`ServingGroupRecreate`：写组的 Failed。（`None` 下没有 `Faulty` 的成员：恢复不生效） | `status` |
| E6 | 已建成，有成员缺失或者 `Terminating`；直读确认过 | `None`：缺的补，在终止的等。`RoleRecreate`：写 Draining(g)。`ServingGroupRecreate`：写组的 Failed | 建 Pod，或 `status` |
| E7 | 创建中，成员不齐 | 缺的建，带代际号 g；位置上有 `Terminating` 的等它消失；名字被不属于这个组的 Pod 占着：报告，等 | 建 Pod |
| E8 | 创建中，成员齐全，没有 `Terminating` 的 | 全部成员都在缓存里看到了：写 Established(g) | `status` |
| E9 | 已建成，成员齐全 | 不做事 | — |

**八条规则落在哪里：**

| 规则 | 落在 |
| --- | --- |
| L1 先写账本，再删 Pod | R8 的 `status` 写在 R9 的删除之前；R8 被拒绝，这一轮到此为止 |
| L2 零值不引起删除 | 没有账本项按 Creating 处理（`ledger.go` 里只有一处取阶段的函数） |
| L3 Established 只在缓存里看到全部成员之后写 | E8 看的是 informer 里的 Pod，不是创建请求的返回 |
| L4 证据要直读确认 | R7：这一轮要写 Failed、写 Draining、删 Pod、打原地更新的补丁时，直读这个组和带它的组名标签的 Pod，用读到的重做 R4 到 R7 |
| L5 代际号 | Pod 创建时带 `instance-generation` 标签；Draining(g) 只删代际号不大于 g 的；看到比账本大的代际号，先把账本抬上去 |
| L6 两步删除 | E1、E2 |
| L7 名字被别人占着 | 创建返回 `AlreadyExists` 之后直读那个 Pod，核对 controller owner 的 UID |
| L8 先写后建 | 3.6 |

### 3.6 `pods` 和先写后建

`pods` 同时是三样东西：`scale` 子资源的 `status`；「没有成员」的一半（另一半是 Pod 缓存里没有成员 Pod）；先写后建的记录。

```text
existing = 成员 Pod 的个数，含终止中的
started  = existing > 0，或者读到的 status.pods > 0，或者这一轮过了先写后建的闸
pods     = existing + (started ? 这一轮还要建的成员个数 : 0)
```

**先写后建的闸。**一个组读到的 `status.pods` 是 0、一个成员 Pod 都没有、这一轮有成员要建：PodGroup 建成之后（不用 PodGroup 的组没有这个条件），先发一次 `status` 写，`pods` 是要建的个数，带 resourceVersion。写成功才建第一个 Pod。

它要挡的是这件事：ModelServing controller 看到一个没有成员的组的版本不对，会不占预算地把 `spec.revision` 直接改掉（G2′(b)），因为它没有 Pod 可扰动。改版本的 PATCH 和这次 `status` 写都以组的 resourceVersion 为条件，只有一个成功。改版本先到：`status` 写被拒绝，下一轮按新版本建。`status` 写先到：PATCH 被拒绝，重算时这个组已经有成员，按预算替换。「Pod 按旧版本建出来了，组的版本却被不占预算地改掉」不会出现。

几种情形：

| 情形 | `pods` | 是不是没有成员 |
| --- | --- | --- |
| 刚建出来的对象，还没有轮到它 | 0 | 是 |
| PodGroup 建不出来 | 0（`started` 为假） | 是。目标变了直接改版本 |
| 过了闸，第一个 Pod 还没建出来就换了进程 | 要建的个数（读到的 `pods` 大于 0，`started` 为真） | 不是。新进程接着按原来的版本建 |
| 实例恢复清空了组里全部 Pod，正要重建 | 要建的个数（E3 写 Creating(g′) 的那一次一并记上） | 不是。按组自己的版本重建 |
| 各 Role 的副本数都是 0 | 0 | 是 |
| `status` 被清空，而 Pod 都在 | Pod 的个数 | 不是 |
| `status` 被清空，而一个 Pod 都没有 | 0 | 是。后果只是版本可以在创建之前被改掉 |

ModelServing controller 的 PATCH 第一次常常输给新对象的第一次 `status` 写。这是一次重试，不算「一个写失败」，不触发「这一轮后面的写都不发」。

### 3.7 写协议

一轮里 `status` 至多写两次，多数轮次一次或者零次：

| 写 | 什么时候 | 内容 | 被拒绝时 |
| --- | --- | --- | --- |
| 动手之前 | 每一轮都算；算出来的和读到的不同才写。它同时是 R8（账本的转换、Failed）、L8（先写后建的那一位）和 R11（汇总） | 账本、Failed、Drained、`pods`，和这一轮算出来的全部汇总 | 返回。这一轮后面不发任何删除、创建、补丁 |
| 出错之后 | 这一轮的删除、创建、钩子里有一个出错 | 同上，加 `ReconcileFailed` | 重新入队 |

- 008 把 R8、L8、R11 分成三次写。三次的内容都是从同一份观察算出来的，合成一次之后「账本、Failed、先写后建的那一位都在任何删除和创建之前落盘」不变，少一到两次写。
- 同一个错误再出现时，算出来的 `status` 与已经写上的相同，不再写：出错不会变成一圈一圈的 `status` 写。
- **写用 `UpdateStatus`，带读到的 resourceVersion。**不用 PATCH：要的就是「读之后没有人写过」这个前置条件。
- **写回校验。**`UpdateStatus` 返回的对象里，账本、`observedPods`、`pods` 要与发出去的相同。不同，说明集群里的 CRD 比 controller 旧，字段被裁掉了：这一轮按出错处理，不发后面的删除和创建，发事件 `StatusNotPersisted`。这样「先写后做」不会因为写被悄悄裁掉而变成「没写就做」。做法取自 RoleBasedGroup。
- **出错的轮次也写**（A1）：只要读到了成员，`observedGeneration` 照样推进，`Ready` 照实算，带 `ReconcileFailed`。不写的话这个组会一直是未观测，或者一直带着上一次的 `Ready`。
- **`observedGeneration` 的推进**：这一轮的决定是按这份 `spec` 做的，不等动作做完。

一轮里的次序（R0–R11 的实现）：

```text
R1  取组；owner 不是现存的 ModelServing：发事件，返回。组在终止中：返回
R2  有待接管的成员：直读这个组，换 owner
R3  观察：成员 Pod（缓存）；spec 引用的 ControllerRevision；OnModelServingSync
R4  成员分类、实例状态
R5  组已失败：只删 Faulty 的成员；算 Drained；到 R11
R6  Role 一级的计划：哪些实例在期望之内、建在哪个序号、用哪个版本
R7  账本的转换。要写 Failed、写 Draining、删 Pod、打补丁：直读，重做 R4–R7
R8  提交（status）
R9  钩子和删除
R10 PodGroup、Service、OnRoleSync；先写后建；创建；原地更新的补丁；标记
R11 汇总（status）
```

### 3.8 `status` 被清空或者被改写

| 丢了什么 | 下一轮 | 后果 |
| --- | --- | --- |
| 汇总（`observedGeneration`、`observedPods`、`Ready`、`Drained`、`revision`、计数） | 组自己的更新事件带来一轮，重算，写回 | 这期间组是未观测 |
| 账本的 Established | 实例回到 Creating。成员齐全：E8 重新写上 Established | 在重新写上之前丢了成员：只补，不按范围恢复 |
| 账本的 Draining | 实例回到 Creating。还在期望之内：缺的补上，已经删了一半的实例剩下的 Pod 留着。不在期望之内：E4 重新写 Draining | 一次恢复做了一半、停下、补回原样。没有多删 |
| 账本的代际号 | 取现有 Pod 的最大代际号 | 没有 |
| Failed | 证据还在（成员缺失或者故障）：重新写上。证据没了（故障的成员已经删了，别的都在）：组留下，缺的补 | 一次整组恢复做了一半、停下 |
| RecreateRequired | 从 Pod 和版本重新得出 | 没有 |
| `pods` | 3.6 的表 | 至多是版本在创建之前被改掉 |

**旧客户端的整体改写。**类型化的客户端做 `UpdateStatus` 时会丢掉它不认识的字段。演进的规则因此是：以后新加的 `status` 字段要当决定的输入，它的零值必须是保守的一侧。

**权限。**`servinggroups/status` 的写只给 controller。聚合到 view、edit、admin 的角色只有读，edit 和 admin 另有 `delete`。

### 3.9 大小和写量

| 项 | 估计 |
| --- | --- |
| `spec` | Role 至多 4 个，名字至多 12 个字符：2 KB 以内 |
| 账本 | 每个实例一项，JSON 里 60 到 70 字节。16 个实例约 1 KB；1000 个实例约 70 KB。Role 的副本数没有上限，所以账本没有硬上限，也不截断：它是决定的输入。没有做超过某个项数时的告警；`status` 的大小可以从对象本身看到 |
| 稳态下的写 | 没有。Pod 不变，摘要不变 |
| 一个 Pod 启动 | 它的投影变三到五次（`phase`、`containerID`、Ready），组的 `status` 跟着写这么多次；就绪翻转时 ModelServing 的 `status` 写一次 |
| 一个组从无到有 | 另多一次先写后建，每个实例建成时一次（一轮里多个实例的转换合成一次） |
| 直读 | 只在有写要发的轮次：ServingGroup controller 读这个组和它的 Pod；ModelServing controller 读它自己和名下的组 |

客户端的 QPS 和 burst 今天默认是 client-go 的 5 和 10。多出来的 `status` 写和直读会被它限住，接线的那个提交把默认值提高到 50 和 100；`--kube-api-qps`、`--kube-api-burst` 照旧可以改。

### 3.10 `status` 在版本之间怎样演进

这一节说的是这个版本之后、新版本升到更新的版本。

| 变化 | 规则 |
| --- | --- |
| CRD 和 controller 的先后 | 先装 CRD，再升 controller。次序反了时写回校验（3.7）会让新 controller 停下来报告，不会带着被裁掉的账本继续 |
| 加一个汇总字段 | 随便加。旧 controller 的 `UpdateStatus` 会把它丢掉，新 controller 下一轮补回来 |
| 加一个当输入的字段 | 零值必须是保守的一侧（3.8） |
| 加一个 condition | 旧 controller 不认识的 condition 会被它保留：它按类型更新自己写的那几个 |
| 改投影或者摘要的编码 | 换版本前缀。升级之后头一轮所有的组不新鲜 |
| 改账本的阶段集合 | 不能让旧 controller 读到它不认识的阶段。要加阶段的话分两个版本：先让 controller 认识，再开始写 |
| 滚动升级 controller（多副本，开着选主） | 新旧两个副本先后当主，读写同一份 `status`。上面几条保证后来的主读到的事实是它认识的 |

### 3.11 `ModelServing.status` 怎样从组汇总

计数的口径与今天相同（008 的 03 第 6 节）：

| 字段 | 定义 |
| --- | --- |
| `replicas` | 名下全部组对象，含终止中的和失败的 |
| `updatedReplicas` | 版本等于 T、不在终止中的；`RecreateRequired` 为真的不算 |
| `currentReplicas` | 版本等于 B 的 |
| `availableReplicas` | 运行的组里最后一次写上的 `Ready` 为真的。不问是否已观测、是否新鲜：`spec` 刚下发的那一拍里对外的可用数不应当抖 |
| `currentRevision`、`updateRevision`、`collisionCount`、`labelSelector`、`observedGeneration` | 不变 |

三个已有的 condition（`Available`、`Progressing`、`UpdateInProgress`）原样沿用今天的函数 `utils.SetConditionWithRolloutAndProgressState`（1.3 的 V10），只是两个输入从组对象算：

- `rolloutActive`：有没回收完的 surge；或者有不受保护而版本不是 T 的组。`RoleRollingUpdate` 下是「有运行的组的 `status.revision` 不等于 T」，Role 的 partition 停点上也是真，与今天相同。
- `progressActive`：有没回收完的 surge；或者有未就绪的组；或者组的个数不等于 N；或者可用数不等于 N。

`currentReplicas` 照今天的表达式逐字搬（没有配 partition 并且全部更新、全部可用、个数等于 N 时等于 `updatedReplicas`），今天的四个 `status` 单测的期望值原样保留。基线的推进按 G6（1.4 的第三类）。

新增的 `RolloutBlocked`：有事要做，这一轮组一级的计划却是空的，并且同一个原因已经超过期限（5 分钟；`BaselineUnknown` 和 `OrdinalOccupied` 不设期限）。「有事要做」实现成「`Available` 不为真」：008 逐条列的那些情形（有可替换的组、组数不等于 N、有失败的组、有没回收的 surge 组……）每一条都让 `Available` 为假，反过来 `Available` 为真时 `RolloutBlocked` 一定是假。等待还没满期限时，这一轮带着剩下的时间重新入队，到点再判。从来没有阻塞过的 ModelServing 上没有这个 condition。reason 自上而下取第一个成立的：`BaselineUnknown`、`OrdinalOccupied`、`WaitingForDeletion`、`GroupNotObserved`、`TargetNotReady`、`BudgetExhausted`。期限的起点取对象上的时间（`deletionTimestamp`、condition 的 `lastTransitionTime`、`creationTimestamp`）；只有「不新鲜」和「待收养」两种的起点在内存里，丢了只让报告晚一个期限。

`CoordinatedRoleRolloutBlocked` 从各组汇总（3.4）。

接管注解是 `InProgress` 时计数不重算，保留旧版本写的值，只更新 condition、`observedGeneration` 和 `labelSelector`（协调的校验 webhook、LWS controller、e2e 的等待函数都在等 `observedGeneration`）。回退的开关在的时候 `status` 整个不更新，`observedGeneration` 也不推进：这期间不要改 ModelServing。

## 4. 两个 controller 的结构

### 4.1 包

| 包（`pkg/model-serving-controller/` 下） | 内容 | 状态 |
| --- | --- | --- |
| `servinggroup` | 组里面的规则，纯函数：成员、投影、摘要、成员分类、账本的转换、`status` 的计算、Role 一级的计划 | 新 |
| `grouplevel` | 组一级的规则，纯函数：分类、计数、一轮的计划 | 新。从 008 的参考模型移植 |
| `workload` | 版本的解析（基于 ControllerRevision 的 lister）；「工作负载视图」 | 新。从 `controller/revision_history.go` 搬 |
| `objcache` | 两个 controller 共用的 informer 存储和索引 | 新 |
| `sgcontroller` | ServingGroup controller | 新 |
| `mscontroller` | ModelServing controller | 新 |
| `manager` | 建 informer、起两个 controller；X2 的闸；调试端点 | 新 |
| `apitest` | 测试替身（4.5），只在测试里用 | 新 |
| `lws` | LWS controller | 从 `controller/` 原样搬 |
| `utils`、`plugins`、`podgroupmanager`、`inplace`、`webhook` | 留用。改动见 4.4 和第 7 章 | 改 |
| `controller`、`datastore` | 删除 | — |

新的两个 controller 先在新包里写完，旧的 `controller` 包一直是接线的那一个，它的测试一直是绿的。一个接线提交把 `pkg/controller/controller.go` 换到新的两个；随后的提交删掉旧包。没有开关，没有两条路径并存的版本。

### 4.2 工作负载视图

留用的辅助函数几乎都收 `*ModelServing`：渲染 Pod（`utils.GenerateEntryPod`）、建 PodGroup、plugin 的 `HookRequest`、`inplace.TargetImages`、`utils.RoleRevisionHash`。把它们改成收新的类型要动几千行。

ServingGroup controller 因此为一个组、一个版本造一个 `*ModelServing`：

```text
metadata（名字、命名空间、UID、ownerReferences、注解）  ← 缓存里所属的 ModelServing
spec 里不属于版本的设置                                  ← 组的 spec
schedulerName、plugins、Role 的模板和 workerReplicas     ← 这个版本的 ControllerRevision（utils.ApplyRevision）
```

决定只取决于组的 `spec` 和 ControllerRevision。所属的 ModelServing 只在三处被读：身份（R1 本来就要读）；LWS plugin 要它的 ownerReferences；恢复策略和 grace 的保守取值（3.5）和动手之前的直读。

### 4.3 informer、索引、入队

两个 controller 在同一个进程里，共用 informer：ModelServing、ServingGroup、Pod（照旧只看带组名标签的）、Service、PodGroup、ControllerRevision（新；今天每一轮 LIST）。

| 索引 | 用途 |
| --- | --- |
| Pod 按 controller owner 的 UID | 一个组的成员 |
| Pod 按组名标签（只收没有 controller owner 的，和 owner 是 ModelServing 的） | 待接管的成员；找回（S1′） |
| ServingGroup 按 controller owner 的 UID | 一个 ModelServing 名下的组 |
| ServingGroup 按 `modelserving.volcano.sh/name` 标签 | ControllerRevision 新增时找到引用它的组 |
| ServingGroup 按名字的前缀（`<前缀>-<序号>` 的前缀） | 占着某个序号的名字而不是自己的对象，不管它带什么标签 |
| Pod 按 ModelServing 名字标签（只收 controller 不是 ServingGroup 的） | 存量的 Pod；无主的成员 Pod（S1′） |

| 事件 | ServingGroup controller 入队 | ModelServing controller 入队 |
| --- | --- | --- |
| ServingGroup 增、改、删 | 这个组 | 它的 owner |
| 成员 Pod 增、改、删（含 owner 被改：新旧两个对象各映射一次） | 所在的组 | 所在组的 owner（3.3） |
| PodGroup、Service 改、删 | owner 的那个组 | — |
| ControllerRevision 增 | 引用它的组 | 它的 owner |
| ModelServing 增、`generation` 变、接管注解变、队列注解变、删 | `recoveryPolicy` 或 grace 变了：名下的组 | 它自己 |
| 定时 | grace 到期；原地更新期限到期；被占着的名字的重试 | 阻塞原因的期限；终止中的组的等待期限；有组不新鲜时的重试 |

回调只入队，不读写状态，不调 API。队列的 key 以名字为键；一次更新事件里新旧对象的 UID 不同，按删除加新增处理，并对这个 key 调 `Forget`。

### 4.4 每个写的前置条件

| 写 | 前置条件 |
| --- | --- |
| 创建 ServingGroup、Pod、PodGroup、Service | 名字确定。`AlreadyExists` 之后直读，核对 controller owner 的 UID |
| 删 ServingGroup | UID 和 resourceVersion，前台级联 |
| PATCH ServingGroup 的 `spec` | UID；改 `spec.revision` 的另带 resourceVersion；不下发 `sourceGeneration` 更小的 |
| 摘 ServingGroup 的 surge 注解 | UID |
| 写 ServingGroup 的 `status` | resourceVersion |
| 删 Pod | UID |
| 改 Pod 的 owner、标签、注解 | UID 和 resourceVersion |
| 原地更新的补丁 | UID 和 resourceVersion（`inplace` 包已经这样做） |
| 写 `ModelServing.status` | resourceVersion |

留用的包里跟着改的：

| 包 | 改动 |
| --- | --- |
| `utils` | 渲染 Pod 的函数加 owner 和代际号（新函数，旧的留到删除旧包的那个提交） |
| `podgroupmanager` | 去掉对 `datastore` 的依赖；owner 由调用者给，补 `blockOwnerDeletion`；队列名从参数来；按名字删 PodGroup 的入口去掉（靠级联） |
| `plugins` | `HookRequest` 加组对象和实例的代际号。headless Service plugin：Service 的 owner 是组；删除钩子里按 UID 删这个实例的 Service；进程里的 `RoleCleanup` 记忆去掉，改成比较 Service 上记的组 UID 和代际号 |

### 4.5 测试替身

client-go 的 fake clientset 不检查 resourceVersion 和 UID 的前置条件，不模拟前台级联和垃圾回收。只用它的话，把 4.4 的前置条件全部去掉，测试照样通过。

所以先写一个测试替身（`apitest`，只在测试里用）：在 fake clientset 上加 reactor，检查 `Update`、`UpdateStatus`、`Patch`、`Delete` 的 resourceVersion 和 UID，创建时分配 UID 和递增的 resourceVersion，前台删除时先给对象打 `deletionTimestamp` 和 finalizer、等名下的对象都删掉再删它。两个 controller 的测试都跑在它上面。

在它上面再做一个停点测试的框架：按脚本跑一个场景，在第 k 次写之后停下，换一个没有任何内存的 controller 实例接着跑（可以配一份旧的缓存），终局要和不停相同；对每个 k 都做一遍。Kind 上的杀进程只作为确认。

## 5. Role 一级怎样搬

008 的 4.9 说组一级和 Role 一级是同一套按序号的规则，计划可以是同一个函数。实现照这句话做：

| 部分 | 做法 |
| --- | --- |
| Role 的扩缩容；独立的 `RoleRollingUpdate`（额度、partition、surge、转正、回收、次序） | **调用同一个计划函数** `grouplevel.Plan`，单元是一个组里某种 Role 的实例。实例到单元的对应：清空中 = 终止中；已建成并且成员全部就绪 = 就绪；Role 一级的 surge 标记 = surge；版本按这个 Role 的模板比较，语义相同的算同一个版本。`ServingGroupRollingUpdate` 下用它「不分版本」的那一支：只扩缩、补洞、摘掉留下的标记 |
| roleCoordination | 纯函数（`calculateRoleRolloutLimits`、`slowestReadyProgress`、`allowedStartedReplicas` 等）连同它们的测试原样搬进 `servinggroup`；读 `datastore` 的部分改成读账本和成员 |
| 原地更新 | `inplace` 包不动。`controller/inplace_update.go` 里按单元推进的部分搬进 `sgcontroller`；「能不能原地更新」里只看模板的部分（`OnlyImagesChanged`）移到 ModelServing controller |
| 建 Pod、PodGroup、plugin 钩子 | 搬，owner 换成组 |
| 缩容的次序（未就绪的先、deletion cost 低的先、序号高的先） | 比较函数原样搬；优先级从账本和成员得出 |

旧函数的去向逐个列在任务 010 的 `subagents/proposal-analysis.md` 第 1 节。旧的测试随旧包删除；每个 `*_regression_test.go` 的去向（新的单测、e2e、runner 的用例，或者不再适用的理由）在删除的那个提交之前补进任务记录。

**没有了的东西**，它们的工作由对象上的事实完成：

| 旧代码 | 现在 |
| --- | --- |
| `datastore`；`syncAll`；`addServingGroupAndRoleFromPod` | 每一轮从对象判定 |
| `podDeletions`（按 UID 记着一次删除选中了谁） | 代际号加 UID 的前置条件 |
| `roleCleanups`；`finishPendingDeletions` | Service 的 owner 是组；清空一个实例时在钩子里按 UID 删 |
| `recreatedSinceDeletion`（补偿事件的滞后） | 不靠事件 |
| `graceMap` 和每个故障 Pod 一个睡眠的 goroutine | `FaultTracker` 加延时入队 |
| `markPodUnavailable` | 就绪每一轮现算 |
| `updateModelServing` 里「去掉拓扑时删掉全部 PodGroup」那一个边沿 | 组的那一轮就地更新 PodGroup |

## 6. 版本升级

### 6.1 源版本

两个源版本对 ModelServing 来说是一条代码线：`release-1.1` 和变更前的 `main` 之间，`utils`、`webhook`、`inplace`、`podgroupmanager`、`datastore` 逐字节相同，差别只有后加的东西（bootstrap 的 API 字段、`OnModelServingSync`、Role 状态事件、partition 过滤的重构）。对象上的形态相同：

| 对象 | 形态 |
| --- | --- |
| Pod、Service | owner 是 ModelServing，`controller`、`blockOwnerDeletion` |
| PodGroup | owner 是 ModelServing，`controller`，没有 `blockOwnerDeletion` |
| 组一级的 surge 标记 | Pod 注解 `modelserving.volcano.sh/surge: serving-group` |
| 原地更新的进度 | Pod 注解，格式与新版本相同 |
| 协调的基线 | ControllerRevision 上的注解 |

008 的 06 的五条前提在这两个源版本上：

| 前提 | 结论 |
| --- | --- |
| P1 目标版本是源版本的超集 | 成立：这次不删任何 API 字段、plugin、钩子 |
| P2 版本的身份连续 | 成立，条件是三样不动：`BuildRevisionData` 和 `RecordModelServingRevision` 不改；类型改名不改 JSON；CRD 里 Role 的模板、`plugins`、`schedulerName` 之下不加带默认值的字段。加一个单测，把 `release-1.1` 上构造的样例的哈希钉住 |
| P3 老的 headless Service 布局 | 不适用（只属于在网版本） |
| P4 校验只拦新的 | 成立：不加规则。校验 webhook 另要放宽一处（6.4 的 T1） |
| P5 驱逐 webhook 认两种归属 | 要改，并且要在接线之前：否则 owner 一换，驱逐保护对每个 Pod 无声地失效（第 7 章） |

### 6.2 旧进程看到 owner 被换掉会怎样

读 `release-1.1` 的 `model_serving_controller.go`：

1. Pod 的 informer 包在 `FilteringResourceEventHandler` 里，过滤条件是「有 kind 是 ModelServing 的 ownerReference」。一次更新让过滤的结果从真变假，client-go 对旧对象调 `OnDelete`。
2. `deletePod` 拿到的旧对象 owner 还是 ModelServing、没有 `deletionTimestamp`，于是走到 `handleDeletedPod`。
3. `ServingGroupRecreate`：按名字删掉这个组的 PodGroup，并删掉组里所有还归 ModelServing 管的 Pod。`RoleRecreate`（默认）：删掉这个实例里还归它管的 Pod。`None`：下一轮去建「缺失」的 Pod，撞名字，重试。

所以默认配置下，旧进程每看到一次接管的 PATCH，就删掉同一个实例里还没换 owner 的 Pod。**新旧两个进程不能有任何重叠，而且这一点要由新版本自己保证。**

### 6.3 独占

| # | 做法 | 实现 |
| --- | --- | --- |
| X1 | 自查 | chart 给 controller-manager 的 Pod 模板加标签 `modelserving.volcano.sh/controller-protocol: servinggroup`。标的是协议的代，不是镜像版本：以后新版本之间的升级不应当被它挡住。进程启动时（参选之前）另外给自己的 Pod 补上这个标签，补不上就每 5 秒重试：不是用这个 chart 部署的 Deployment 可能没有模板上的标签，那样同一个版本的几个副本会互相当成旧版本、谁也不启动。自己的 Pod 名取环境变量 `POD_NAME`（chart 经 downward API 注入），没有时取主机名。进程拿到租约之后、启动任何 controller 之前，在自己的命名空间里列出带 `app.kubernetes.io/component: kthena-controller-manager`、不带这个标签、还没有结束（`Succeeded`、`Failed` 之外，终止中的也算在跑）的 Pod；有，就不启动任何 controller，只应答 webhook，每 5 秒查一次，在自己的 Pod 上发事件 `WaitingForPredecessor` 说明在等谁。运行中每 5 秒查一次；看到这样的 Pod，进程退出（退出码非零），容器重启之后回到启动自查。不在集群里跑（本地调试）时不查 |
| X2 | CRD 和权限的闸 | 进程一启动就试着 LIST 一次 ServingGroup，每 5 秒重试，不论有没有拿到租约。成功之前 `/readyz` 返回 503（Deployment 的就绪探针指向它），`/healthz`（存活）照常，两个 controller 不启动。不退回旧的工作方式。新 Pod 不就绪时滚动更新不会停掉旧 Pod，旧版本接着工作：漏装 CRD 只是让升级停在那里。chart 里另用 `lookup` 在升级开始之前失败；`lookup` 在 `helm template` 和多数 GitOps 的渲染里返回空，所以只在「release 的命名空间查得到而 CRD 查不到」时才失败 |
| X3 | 换凭据 | chart 里 controller-manager 的 ServiceAccount 改名为 `kthena-controller-manager-servinggroup`，两个绑定跟着改。`helm upgrade` 删掉旧的 ServiceAccount，旧版本的进程（包括失联节点上还活着的）随即失去凭据：Kind 上旧 Pod 在被 Deployment 删除之前就因为认证失败退出了一次，`kubectl get pod` 里短暂显示 `Error`，这是预期的。装回旧 chart 时反方向同样生效，回退靠它保证新版本的进程不再写。X2 和 X3 有一处相互作用：CRD 没装而 chart 的检查又没有触发（Helm 不能 `lookup`）时，X2 让新 Pod 不就绪、旧 Pod 留着，X3 却已经让旧 Pod 的凭据失效，这段时间没有 controller 在工作。不另加机制，写进升级步骤当作漏装 CRD 的代价：工作负载不受影响，只是不恢复、不滚动；装上 CRD 之后新进程自己变成就绪 |
| X4 | 选主 | `--leader-elect` 默认改成真；失主的回调改成退出进程（今天只打一行日志，进程留着，已经在睡眠的 grace goroutine 还会去删 Pod）。`ReleaseOnCancel` 仍然是假 |

运行中的自查选「退出」而不是 008 写的「停掉两个 controller、继续应答 webhook」：informer 和队列停下之后不能原地重启，退出重来是同一个效果，少一条要测试的路径。代价是这个 Pod 重启的几秒里它不应答 webhook；别的副本和那个旧 Pod 照样应答。

选主不是隔离栅：失主的进程在发现失主之前发出的请求照样落地。所以每个写另带前置条件（4.4），两个进程并存时不会多建组，可能发生的是一个额度被用两次。

Deployment 的更新策略是默认的 RollingUpdate（多一个、不少一个）：新 Pod 先就绪（webhook 起来），旧 Pod 再被停掉，webhook 没有空窗。X1 的等待不能影响就绪，否则旧 Pod 永远不会被停掉。

### 6.4 接管

按 ModelServing 进行，每一步幂等。进度记在 ModelServing 的注解 `modelserving.volcano.sh/servinggroup-adoption` 上：没有（还没查过）、`InProgress`、`Done`、`RollingBack`。

**存量对象**：带这个 ModelServing 的名字标签、controller owner 是它（按 UID）的 Pod、PodGroup、Service；接管没做完时另包括带标签、没有 controller owner、名字对得上的 Pod。终止中的存量 Pod 不算。

| 步 | 谁 | 做什么 | 实现上的点 |
| --- | --- | --- | --- |
| T1 | ModelServing controller | 注解不是 `Done`：直读存量的 Pod。一个都没有，并且名下的组都已观测、新鲜、没有待接管的成员：写 `Done`。否则写 `InProgress` | 一次 LIST（Pod），按名字标签，按 owner 的 UID 过滤。PodGroup 和 Service 不查：它们由各组的同步入口顺手接过去（T4），不拦 `Done`。**新建的 ModelServing 也走这一步**，结果是 `Done`。注解写在主资源上，要过校验 webhook，而它对每次 UPDATE 都重新校验整个对象：按现在的规则不合法、今天却在运行的对象会卡在接管上。所以校验 webhook 放行 `spec` 在语义上没有变的 UPDATE（只放宽，不违反 P4） |
| T2′ | — | **没有实现。**没有活的存量 Pod 的组名下如果留着存量的 PodGroup、Service：序号小于 N 的，G4 建出组对象之后由 T4 接过去；序号不小于 N 的留着，owner 还是 ModelServing，随它删除时被回收 | T7 不查 PodGroup 和 Service，所以它们不拦接管。代价是可能留下几个没有 Pod 的旧对象 |
| T3 | ModelServing controller | 每个还有活的存量 Pod 的组名：建同名的 ServingGroup | 就是 S1′：存量的 Pod 和无主的成员 Pod 走同一段代码。`spec.revision` 取这个组的 Pod 的 revision 标签；标签不一致的取这个序号现在该有的版本。surge 注解判定一次：`ServingGroupRollingUpdate`，序号不小于直读到的 N，名下至少一个 Pod（含终止中的）带值是 `serving-group` 的注解。撞名字时核对 owner 的 UID，对象上有没有注解以第一次创建的为准 |
| T4 | ServingGroup controller | 每个待接管的成员：先直读这个 ServingGroup（UID 没变，没有 `deletionTimestamp`）和所属的 ModelServing（注解不是 `RollingBack`），再把 owner 换成它 | 一次 JSON merge patch：整个 `ownerReferences` 列表（保留不是 controller 的 owner）、代际号标签（没有的补 0），body 里带 Pod 的 `uid` 和 `resourceVersion`。PodGroup 在 `podgroupmanager.SyncPodGroup` 里、headless Service 在 plugin 的 `OnRoleSync` 里原地换 owner（UID 不变；Service 另带上实例的代际号，删除钩子只删代际号不大于请求的）。ClusterRole 要给 PodGroup 和 Service 加 `patch`，给 `servinggroups/finalizers` 加 `update`（开着 `OwnerReferencesPermissionEnforcement` 的集群里，设置 `blockOwnerDeletion` 要它） |
| T5 | ServingGroup controller | 账本从零值开始。一个实例的成员按它应用的版本的布局齐全：写 Established | 就是 E8 |
| T7 | ModelServing controller | 直读：没有存量的 Pod 了，并且名下的组都已观测、新鲜。写 `Done` | 等组报告过再写 `Done`：刚建出来的组还没有写出 Ready，这时重算计数，可用数会掉到 0 再回来（Kind 和单测里都看到过） |

`InProgress` 期间，ModelServing controller 对这个 ModelServing 只做 T3 的创建，G0 到 G6 的写都不发（M3 的闸），`status` 的计数不重算。ServingGroup controller 对已经换完 owner 的组照常工作。

T4 与 orphan 删组之后的收养是同一段代码（R2）。差别只有一处：收养来的无主成员版本与组的不同要报 `RecreateRequired`；owner 是 ModelServing 的存量成员不比较版本（原地更新到一半的组里 Pod 的标签是旧的）。

**账本从零值开始带来的缺口。**旧版本没有在任何对象上记「建成过」。升级的空窗里丢的 Pod，接管之后只补，不按范围恢复。这与旧版本自己升级时相同；之后每一次升级都按范围恢复。

### 6.5 升级那一刻的各种状态

| 升级时 | 接过来之后 |
| --- | --- |
| 稳态 | N 个同名的 ServingGroup，实例都是 Established。没有动作 |
| 带 surge 的滚动途中 | T3 建的 surge 组带注解。`Done` 之后照规则做完：替换旧组、在腾出来的序号上重建、回收 surge 组 |
| N 刚调大，surge 组的序号已经小于 N | 对象不带注解，是正式的组；Pod 上的副本随后被摘掉 |
| partition 灰度停点 | 序号小于 P 的组受保护，就是原来那几个 |
| 稀疏的序号集合 | 序号从名字解析，照常接管 |
| 稀疏的序号上滚动途中，多出来的容量建在小于 N 的空洞里 | 不带标记的组多于 N：按缩容处理，删一个。未就绪的先：可能删的是刚建的那个组。升级前检查报出这样的 ModelServing，建议等滚动做完再升级 |
| `RoleRollingUpdate` 滚动途中 | 各实例按自己的标签接管，Role 一级的规则接着做；值是 `role` 的标记不动 |
| 原地更新途中 | 组的 `spec.revision` 取进度注解里的目标；成员接着完成 |
| 整组恢复或者实例恢复做了一半 | 没有标记可续：缺的补（与旧版本自己重启相同） |
| 有 `Failed` 的 Pod，或者在重启故障里的 Pod | grace 从新进程第一次看到起重新计。`None`、`-1` 下一直容忍，与今天相同 |
| 各 Role 都是 0 的组 | 没有 Pod，接管时不建对象；`Done` 之后 G4 按规则建出空组，旧版本留下的 PodGroup 由同步入口原地接过去（T2′ 没有实现，见 6.4） |
| 整个组的 Pod 都在终止 | 不接管，不等。它们消失之后 G4 在那个序号上建 |
| 带着标签、名字对不上的 Pod | 不接管，不删除，发事件 |

### 6.6 升级前检查

一个只读的子命令（`kthena` CLI 的 `modelserving preflight`），对集群和每个 ModelServing 给出结论。输出的项照 008 的 06 第 7.1 节，去掉只属于在网版本的几项。必须有的三项：

1. 按目标版本的算法算出当前 `spec` 的版本，与存量 Pod 的 revision 标签比：「升级后不滚动」或者「升级后会滚动 k 个组」。
2. 不带 surge 标记的组多于 N 的 ModelServing。
3. 对每个 ModelServing 做一次 server-side dry-run 的注解写入，确认它过得了校验 webhook。

另外存下验收用的基线：每个 Pod 的 UID、各容器的重启次数、Ready 的时间；PodGroup 的 UID；`status.availableReplicas`。

### 6.7 操作步骤和验收

| 步 | 做什么 |
| --- | --- |
| 1 | 升级前检查 |
| 2 | `kubectl apply --server-side` 装 ServingGroup 的 CRD。Helm 升级不处理 `crds/` |
| 3 | `helm upgrade`。从这一步到验收完，不排空节点 |
| 4 | 等：新 Pod 起来只应答 webhook，等旧 Pod 消失，取得租约，各 ModelServing 接管 |
| 5 | 验收：每个 ModelServing 的注解是 `Done`；每个还有活 Pod 的组有同名的 ServingGroup；Pod 的 UID 和重启次数不变；没有人改过 `spec` 的 ModelServing 没有组在终止；可用数全程不低于升级前；PodGroup 的 UID 不变 |

### 6.8 回退

回退是新版本的一个模式，由一个开关进入：controller 所在命名空间里名叫 `kthena-servinggroup-rollback` 的 ConfigMap。它在的时候，两个 controller 不做正常的 reconcile，也不做正向接管。

| 步 | 做什么 |
| --- | --- |
| 1 | 人创建开关 |
| 2 | 对每个 ModelServing：等终止中的组消失；把注解改成 `RollingBack` |
| 3 | 对每个 ServingGroup：直读，把名下的 Pod、PodGroup、Service 的 owner 换回 ModelServing；同一次 PATCH 里把 Pod 上组一级 surge 注解的副本写成与组对象一致 |
| 4 | 直读核对：这个 ServingGroup 名下没有对象了；带注解的组，换回去的每个 Pod 都带着副本 |
| 5 | 用 orphan 方式删掉 ServingGroup |
| 6 | 摘掉 ModelServing 上的接管注解和 `RolloutBlocked` condition（旧版本只管它自己的三个 condition，不摘的话它会永远留着） |
| 7 | 全部做完之后装回旧版本 |

三点：

- **与收养的竞争。**正常的一轮里 owner 是 ModelServing 的 Pod 是待接管的成员。开关出现时正在跑的一轮可能把刚换回去的 Pod 又接回来。所以 T4 换 owner 之前的那次直读里另查所属 ModelServing 的注解：是 `RollingBack` 就不换；回退的第 3 步在第 2 步的写成功之后才开始。
- **放弃回退**：删掉开关。注解是 `RollingBack` 的和已经没有注解的 ModelServing 落回 T1，重新接管。
- **带着开关启动**：新版本什么都不接管，工作负载留在 ModelServing 名下。为了不让它看起来像故障，进入和离开回退模式各记一行日志，并在开关这个 ConfigMap 上发 `RollbackMode` 事件。再次升级之前要先删掉开关。
- **旧版本要对象回到什么样子**：三类对象的 controller owner 是 ModelServing；revision 和 role-template-hash 标签在（新版本一直在写）；原地更新的注解格式没变；ControllerRevision 上协调的基线注解在（新版本照旧写它）。代际号标签旧版本不认识，留着无妨。

### 6.9 chart 的改动

| 改动 | 原因 |
| --- | --- |
| `crds/` 下多一个 CRD | — |
| ClusterRole：`servinggroups`（全部动词）、`servinggroups/status`、`servinggroups/finalizers`（`update`）；`controllerrevisions` 的 `watch`；`podgroups`、`services` 的 `patch` | 4.4、6.4 |
| 聚合到 view、edit、admin 的 ClusterRole | 3.8 |
| Pod 模板的协议标签 | X1 |
| 存活和就绪两个探针分开 | X2 |
| `--leader-elect` 默认真 | X4 |
| `lookup` 的检查 | X2 |
| ServiceAccount 换名字（`kthena-controller-manager-servinggroup`） | X3 |

### 6.10 做不到的和没有覆盖的

- 升级的空窗里（旧 Pod 开始终止，到这个 ModelServing 接管完成）控制面不工作：扩缩容、滚动、恢复都不发生。数值在 Kind 上量。
- 旧的 controller Pod 停不下来（节点失联）时，新进程一直等。有事件和日志。
- 不在这个命名空间里的旧进程、用别的凭据在本地跑的旧版本，新版本自己发现不了。
- 在网分支的升级不在范围里。它另需要：T2、三个钩子、P3，以及先把两条代码线收敛。

## 7. 周边组件

| 组件 | 改动 | 什么时候 |
| --- | --- | --- |
| 驱逐 webhook | `isOwnedByCurrentModelServing` 认两种归属：owner 是这个 ModelServing；owner 是一个 ServingGroup，而那个 ServingGroup 的 controller owner 是这个 ModelServing。webhook 的进程里加 ServingGroup 的 lister；lister 里没有、或者 UID 对不上时直读一次，每个请求每个组至多一次。那个 ServingGroup 读到 `NotFound` 才算「不是我的」；别的错误（apiserver 抖动、权限没跟上）下 Pod 照样受保护：读失败不能变成绕过预算的驱逐 | 接线之前 |
| 校验 webhook | `spec` 在语义上没有变的 UPDATE 放行 | 接管之前 |
| autoscaler、ModelBooster | 按标签找 Pod，不看 owner。只受类型改名影响 | — |
| LWS controller | 搬到自己的包。它等 `ModelServing.status.observedGeneration`，接管期间这个字段照常推进 | 接线之前 |
| ControllerRevision 的清理 | 保留的集合换成：T、B、各组的 `spec` 引用的、各组的 `status.referencedRevisions`。只在静止时清：`Available` 为真、没有 partition（组一级和 Role 一级）、没有存量或者无主的成员 Pod、每个组都已观测并且新鲜（`referencedRevisions` 才可信）。静止的每一轮都从缓存比一遍，有要删的才发请求，所以被打断的清理下一轮接着做（今天只在 `status` 的版本变化的那一轮试一次）。不再 LIST 命名空间里全部的 Pod。`spec.revisionHistoryLimit` 今天没有代码读它，这一次也不实现 | ModelServing controller |
| 协调的基线 | 照旧写 ControllerRevision 上的注解（回退要它），另经 `spec.roles[].baselineReplicas` 下发 | ModelServing controller |
| 事件 | Role 状态事件记在 ModelServing 上（1.3 的 V7）；成员和 Pod 一级的记在 ServingGroup 上；组一级的记在 ModelServing 上 | — |
| 调试端口 | `--debug-port` 和路径 `/debug/modelserving/cache` 留着，内容换成从 ServingGroup 的 lister 生成的 JSON | 接线 |
| `--workers` | 两个 controller 各起这么多个 worker | 接线 |
| `--leader-elect=false` | 仍然可以关（本地调试）。关了之后独占只剩 X1 | 接线 |
| chart 的 `replicas` | 默认仍然是 1。只有一个副本时失主退出意味着这个 Pod 重启的几秒里没有进程应答 webhook；文档里建议两个副本 | 文档 |
| `spec.bootstrapAccelerateStrategy` | 今天只有 API，没有代码读它。不动。以后实现时它管的是一个组里实例创建的次序，属于 ServingGroup controller，要经 `spec` 下发 | — |
| 名字长度的校验 | 不收紧（校验 webhook 对每次更新重新校验整个对象，收紧会拦住存量） | — |
| 用户文档、CRD 参考、examples | 新增 ServingGroup 一节；recovery 的行为变化；升级和回退的步骤 | 周边里程碑 |

## 8. 测试和验收

| 层 | 内容 | 验收 |
| --- | --- | --- |
| 纯函数的单测 | 成员、摘要、成员分类；账本的转换（E0–E9 每一格一行）；`status` 的计算；组一级的计划的每一步 | 表驱动 |
| 对照测试 | 组一级的计划对 008 的参考模型导出的向量：契约的 43 段脚本、runner 组合用例的停点、`normal` 套件、穷举模型的抽样 | 每个向量的动作序列逐项相同 |
| controller 的单测 | 跑在 4.5 的替身上。每一条 E、S 的边；每个写的停点；两个 controller 实例交错 | 每个停点续做之后终局相同 |
| P2 的回归 | `release-1.1` 上的样例的哈希 | 钉住 |
| e2e（Kind） | `test/e2e/controller-manager` 现有的全部；新增：ServingGroup 的生命周期和 `status`、整组恢复、三种方式删组、换主之后续做 | 现有的断言不放宽。要改的是按 owner 认对象的四处谓词（`IsControlledBy(pod, ms)`、Service 的 owner UID 等于 ModelServing 的 UID）：改成沿 ServingGroup 找到 ModelServing，断言的内容不变。另一处用 `observedGeneration` 推进当作「就绪的变化已经处理」的同步手法，换成等组的 `status` |
| runner 的行为规范测试 | 变更前后同一个集合，逐例对比 | PASS 变 FAIL 的要么对应已批准的条款，要么是要修的回归 |
| 容灾（Kind） | 两个副本，在每一类写之后杀主；controller 不在时删 Pod | 续做；按范围恢复 |
| 升级演练（Kind） | 两个源版本；6.5 的各行；接管每一步之后杀进程；不装 CRD 直接升；装了 CRD 直接升 | 6.7 的第 5 步 |
| 回退演练（Kind） | 升级、回退、旧版本接着跑、再升级 | Pod 的 UID 和重启次数全程不变 |
| 量一次 | 20 个组、每组 4 个 Pod：创建、一次滚动、一次排空节点各有多少次 API 调用和 `status` 写 | 记下来，不设门槛 |

## 9. 提交计划

每个提交能编译，所动的包的单测通过。手写的代码每个提交尽量不超过 600 行；生成的代码、纯删除各自成提交。

| 段 | 提交 |
| --- | --- |
| A 铺垫 | 模板类型改名；ServingGroup 的类型；生成的代码；chart 的 CRD 和 RBAC；`utils` 渲染 Pod 带 owner 和代际号；`podgroupmanager` 去掉 `datastore`；plugin 的 `HookRequest`；驱逐 webhook 认两种归属；校验 webhook 放行 `spec` 没变的更新；测试替身 |
| B 纯函数 | 成员和摘要；成员分类；账本；`status`；组一级的分类、计划、向量、对照测试；版本的三值；`ModelServing.status`；协调的纯函数（搬）；Role 一级的计划；版本的解析和工作负载视图 |
| C ServingGroup controller | 骨架；PodGroup 和 Service；创建和先写后建；恢复；Role 的扩缩和滚动；协调；原地更新；收养 |
| D ModelServing controller 和切换 | 骨架和版本；计划、直读重算、执行；`status` 和清理；接管（T1、T2′、T3、T7）；搬 LWS controller；**接线**（两个 controller、选主默认开、失主退出、QPS、X1、X2）；删旧包（源码、测试分几个提交、`datastore`）；e2e |
| E 其余 | 升级前检查；用户文档；runner（契约 2.14、适配、新用例）；回退；X3 |

## 10. 没有解决的

| 项 | 内容 |
| --- | --- |
| 预算是对多个对象求和 | 两个进程并存时一个额度可能被用两次。靠独占把窗口压到失主的那几秒 |
| 同名重建依赖垃圾回收器 | 垃圾回收器停了，组停在终止中，那个序号建不出新的。有 reason，没有缓解 |
| 同名的 PodGroup 先删后建、PodGroup 的 owner 变化 | Volcano 的调度缓存按命名空间和名字认 PodGroup。读源码没有发现问题，在 Kind 上确认 |
| 只选中一种 Role 的比例型 PDB | 分母偏大（2.3） |
| 大的账本 | 没有上限，整个重写。1000 个实例约 70 KB |
| 重启故障的 grace 在新进程里重新计 | D9。重启的间隔一直短于 grace 时一直不恢复 |
