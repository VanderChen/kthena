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
| plugin 框架多一个钩子 `OnModelServingSync`，返回 `HookResult{RequeueAfter, Err, Continue}`；`BasePlugin` 提供空实现。内置的 plugin 都没有实现它。今天阻塞性的错误（`Continue` 为假）让整轮 reconcile 在建任何 Pod 之前返回 | 两个 controller 都调，请求里带的都是缓存里的 ModelServing。ModelServing controller：每一轮一开始就调，在解析目标版本和做计划之前；阻塞性的错误让这一轮到此为止并返回错误：不做计划，不发任何写，`status` 也不写，由队列按出错的退避重试。这是上游的做法，没有改。ServingGroup controller：每个组的每一轮也调一次，在这一轮的任何写之前；阻塞性的错误让这一轮不建、不删、不打补丁，只写 `status`，带 `ReconcileFailed`，已经承诺的成员数留着（3.7）。`RequeueAfter` 大于 0 时：不阻塞的结果，ModelServing 这一侧到点再跑一轮；阻塞的结果，ServingGroup 这一侧到点再问，不走队列的退避，ModelServing 这一侧不看它，照旧退避。这样「钩子阻塞时不建 Pod」仍然成立 |
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
| V7 | Role 一级的事件记在 ServingGroup 上（DR23） | `RoleCreating`、`RoleRunning` 这类 Role 状态事件照旧记在 ModelServing 上，含义也照旧：按实例的就绪报（7.1） | upstream 的 e2e 断言事件的对象是 ModelServing；改断言是行为变化 |
| V8 | 整个组被删时尽力调一次 `OnRoleDelete` | 只在组的 `finalizers` 里有 `foregroundDeletion` 时调。回退的开关在、所属的 ModelServing 是 `RollingBack`、所属的 ModelServing 已经不在或者正在删除，这三种情形下不调（7.2） | orphan 方式删组时 Pod 还要被收养回来，这时调钩子会删掉它们的 Service。交还期间同理：钩子会删掉刚交还的 Service（第二轮评审的发现 R2-S3）。plugin 链要从所属的 ModelServing 读，它不在了就读不到 |
| V9 | 清空一个实例时，`OnRoleDelete` 在删 Pod 之前调（E2） | 在这个实例的 Pod 都消失之后、账本改写之前调（E3）。钩子不成功：账本停在 Draining，实例不重建，组报 `DrainBlocked` | 今天就是 Pod 删完之后才调。账本在钩子成功之前不动，所以「至少调一次」不受影响 |
| V10 | `Available`、`Progressing`、`UpdateInProgress` 同一时刻至多一个为真；message 写新的事实 | 原样沿用今天的 `utils.SetConditionWithRolloutAndProgressState`：`Progressing` 和 `UpdateInProgress` 可以同时为真，已经为真的 condition 不刷新 message | 008 对今天的描述不准确。照它做是一处行为变化，CLI 的显示次序也是按今天的语义写的 |
| V11 | `status.currentRevision` 为空而有组时，读不回来就等（`BaselineUnknown`） | 平时照 008。只有接管（T3）这一处，`status.currentRevision` 为空时照今天的做法取存量的组里出现最多的非目标版本，都是目标版本时取 T。只数会被接管的 Pod（不在终止中，有成员身份，owner 是这个 ModelServing 或者是待接管的无主成员），只认这个 ModelServing 读得出来的版本 | 接管来的组上还没有基线的副本可读。旧版本在初次创建完成之前 `currentRevision` 是空的。同名的前一个 ModelServing 留下的 Pod、正在删除的 Pod 的版本不是这个 ModelServing 的：按它建出来的组渲染不出来（R2-M4） |
| V12 | G3b：没有要滚的了就回收 surge 组。确认不就绪的立刻回收；其余的在 `ready` 不低于下限时回收。不看正式的位置补齐没有 | 多一个前提。**surge 组顶替着一个正式的位置**：另外有 N 个活着的组已经有成员时它才是多余的，才回收。「有成员」看 Pod 缓存：有不在终止中的成员 Pod，或者本来就不要 Pod。`status.pods` 不算数：它在第一个 Pod 建出来之前就写上了（3.6 的先写后建），终止中的 Pod 也算在里面。这一轮刚建的组、还在等收养的组不算。surge 组自己有没有成员不问：刚建出来的 surge 组还没有 Pod，`maxUnavailable` 不小于 `replicas` 时（`replicas: 1` 加默认的 `maxUnavailable` 就是）旧组在建 surge 组的那一轮里已经全部删掉，下一轮它还没有 Pod，这时回收它等于没有 surge。一个例外是「没有余地」：缺一个正式的组（终止中的、失败的正式组还占着位置，不算缺），而活着的组已经占满 N + S（`maxSurge` 调小之后 surge 组比 S 多），空着的位置要等一个 surge 组走了才建得出来，这时照 008 回收。过了前提之后，「确认不就绪的立刻回收、其余的看下限」不变。Role 的临时实例用同一个函数，规则相同 | 行为规范测试查出来的（runner F9，`grace-restart` RUN-431，9A.3）。滚动的最后一步，最后一个旧组还在终止、或者它的替代刚建出对象还没有 Pod，这时删掉 surge 组，有 Pod 的组就不到 N 个。上游（`surge_rollout.go` 的 `retireSurgeServingGroups`、`expectedWithSurge`）要等终止中的旧组消失、空出来的序号重建之后才回收，就绪的和不就绪的一样；上游建组时 Pod 是同步建出来的，建不出来这一轮就在回收之前结束。这里组对象和 Pod 分两步，所以前提写成「有 Pod」。runner 的账本在有不在终止中的 Pod 的单元不多于 N 时删目标版本的单元记 `UNEXPECTED_TARGET_REPLACED`，不分就绪与否。用户选定照上游（决策 I-67、I-71、I-74）。代价也与上游相同：替代的组的 Pod 建不出来的时候 surge 组一直留着（第 10 章）。008 的参考模型不动；改了这一条规则的副本在任务 010 的 `evidence/model-vectors/model-010/`，对照向量从它重新导出，每个驱动的结果与 008 的逐项比较过（`results-vs-008.md`）|
| V13 | Role 一级与组一级是同一套按序号的规则（4.9）：Role 的 partition 之下缺的实例按基线建，不分这个组是什么时候建的 | 在眼下这次滚动里新建的组，每个实例都按目标建，partition 之下的也是。组靠注解 `created-revision` 知道自己是新建的（2.3）。这样的组在同一次滚动里丢了 partition 之下的实例，也按目标建回来；上游这时按 `status.currentRevision` 建。恢复策略是 `None` 时缺的 Pod 当场补上；默认的 `RoleRecreate` 先清空再建，建的是一个目标的实例，一般下一轮就建。协调滚动下有一种情况要等：这个 Role 的实例一个都不剩，它的 partition 又等于副本数，这时它是重新开始目标，要等所依赖的 Role 就绪（没有注解的组里这些位置是基线的，不等）。给它下发别的目标时注解摘掉，从此它和别的组一样；这时什么都还没有建出来的组（账本里没有建成过的实例，也没有 Pod）不摘，注解改成新的目标，整个按新的目标建。注解只在 `RoleRollingUpdate` 下有，策略换成 `ServingGroupRollingUpdate` 时摘掉 | 行为规范测试查出来的（runner F13，`normal` RUN-231、233、273，9A.3）。上游新建一个组时整个组按当前模板建（`scaleUpServingGroups` 调 `CreatePodsForServingGroup`），契约的 P13、P14 也是这样：partition 保护的是已有的组里的旧版本，不是要新的组造出一个旧版本的实例。丢了之后也按目标建，是这条规则带来的：实例的版本每一轮从对象上现定，组不记得哪个实例是后来丢的；一个从来没有跑过旧版本的组里冒出一个旧版本的实例，也说不通（决策 I-77）。什么都还没有建出来的组跟着新的目标走，原先只算到组承诺 Pod 为止，是代码复审查出来的：组承诺了之后 Pod 可以一直建不出来（被配额拒绝），这时模板再变，注解摘了，它 partition 之下的实例就按基线建；上游这时整个组按当前模板建。建出了一部分的组这时仍然摘，缺的实例在 partition 之下按基线建，是留下的已知限制（第 10 章）|

### 1.4 对 upstream 用户的行为变化

四类。前两类已经有批准；第三类是今天的代码与契约 2.13 不同、这里照契约的地方，在任务记录里单独列给用户；第四类是实现之后由第二轮评审和回归复核（任务 010 的 `subagents/behavior-regression-recheck.md`）确认的差别。

| 类 | 变化 |
| --- | --- |
| 已批准的条款 | C1 账本。C2 controller 不在时丢的成员按范围恢复。C3 实例建成之前丢的只补。C4 没有 Pod 上的删除标记；做了一半的删除由下一个进程接着做完。C12 Failed、Draining 写上之后不撤销 |
| 设计决定带来的 | 每次整组替换和整组恢复，组对象是新的（名字不变）；PodGroup 和 Service 跟着它重建，这一点没有变：上游也是先删再建（`deleteServingGroup` 删 PodGroup，`OnRoleDelete` 删 Service）。选主默认打开，失主退出。新的 condition `RolloutBlocked`。`OnRoleDelete` 在整组被删时只尽力调。历史版本（ControllerRevision）经 informer 的缓存读，不再每一轮向 API server 发 LIST、也不按名字 GET；读不到某个版本时，受影响的是用到它的那个组（那个实例不建，或者组的这一轮失败并报 `ReconcileFailed`，每次重试一行错误日志），不是整个 ModelServing 的 reconcile，日志的措辞也与上游不同。一个失败的组一级的写让这一轮后面的创建和删除不发。恢复策略和 grace 取动手那一刻的：等 grace 的期间改成 `None`、`-1` 或者调大 grace，恢复取消（今天睡眠的 goroutine 用的是故障那一刻抓下来的值）；恢复过再坏的 Pod 重新计 grace。`RoleRollingUpdate` 下，眼下这次滚动里新建的组丢了 Role partition 之下的实例，按目标建回来，和这个组其余的实例一样；上游在每个组里都按 `status.currentRevision` 建。由此还有一处：金丝雀被放弃（模板改回去）而 Role 的 partition 留着时，金丝雀期间新建的组在 partition 之下的实例停在被放弃的版本上（partition 之下的实例从不被替换），ModelServing 一直是 `UpdateInProgress`；把 partition 调成 0 或者删掉那几个 Pod 就结束。读上游的代码结局相同（没有运行上游确认）。`RoleRecreate` 下建回来的是一个目标的实例；协调滚动时，Role 的实例一个都不剩、partition 又等于副本数的话，要等它所依赖的 Role 就绪（1.3 的 V13） |
| 今天与契约不同、这里照契约 | 未就绪的受保护的组占可用性的额度（今天不占；旧包里的 `TestServingGroupRollingUpdateIgnoresUnavailableProtectedGroups` 断言的是今天的行为，它随旧包删除）。surge 的组总是建在不小于 N 的序号上并带标记（今天在稀疏布局下可以落在小于 N 的洞里、不带标记）。缩容不选 surge 的组。缩容删掉的组消失之前不开始替换。基线只在 P 是 0、没有空组时推进。Role 一级调的是同一个计划函数（第 5 章），前四条对一个组里某种 Role 的实例同样成立；上游的 Role 逻辑在这四种情形下是不是不同，没有逐条比过 |
| 实现之后确认的，不改代码，写进升级指南（决策 I-59） | `status.observedGeneration == metadata.generation` 只表示 ModelServing controller 已经把这一代的设置下发给各组，不表示各组已经动手；要等 `Available`，或者看 ServingGroup。事件的变化（7.1）。plugin 钩子的四点（7.2）。`RoleRollingUpdate` 下某个实例的版本读不出来时，这一轮不建、不打补丁（上游只是不更新 PodGroup，7.3）。接管期间 ModelServing 的 `status` 不动（3.11）。九个小项：`spec` 没有变的 UPDATE 不再校验；队列注解改了立刻生效，ModelServing 的每次更新（含只改元数据的）都带来一轮；去掉 `networkTopology` 时 PodGroup 就地更新，不再全部删掉重建；`/debug/modelserving/cache` 的 JSON 换了样子；`kubectl scale sg` 写得进去，随后被下发覆盖；回退之后 Pod 和 Service 留着 `instance-generation` 标签；命名空间的 edit、admin 可以删 ServingGroup；Go 类型 `ServingGroup` 改名 `ServingGroupTemplate`；`RolloutBlocked` 真过一次之后以 `False/NotBlocked` 留在对象上 |

没有变的：名字；已有的标签（Pod 和 headless Service 多一个 `instance-generation`）；ModelServing 的 API；`status` 的计数和三个 condition 的定义；已有事件的对象和 reason（少一个 `DeletionIdentityChanged`，见 7.1）；故障的定义和 grace；`None` 和 `-1` 的含义。

## 2. API

类型在 `pkg/apis/workload/v1alpha1/servinggroup_resource_types.go`。模板类型原来叫 `ServingGroup`，改名 `ServingGroupTemplate`（JSON 不变）。

### 2.1 `spec`

`spec` 只有 ModelServing controller 写。它带两类东西：这个组的版本；ModelServing 上不属于版本、而在一个组里面要用到的设置。

| 字段 | 取自 | 谁用 |
| --- | --- | --- |
| `revision` | `ServingGroupRollingUpdate`：这个组的版本。`RoleRollingUpdate`：空 | 组里每个实例的模板 |
| `targetRevision` | T，两种滚动方式下都下发 | `RoleRollingUpdate`：Role 一级滚动的目标。`ServingGroupRollingUpdate`：组用它判断自己是不是旧版本的组（表后的说明） |
| `baselineRevision` | `ModelServing.status.currentRevision` | 受 Role partition 保护的实例（带 `created-revision` 注解的组不用它，2.3）；A4 |
| `sourceGeneration` | `ModelServing.metadata.generation` | A2 |
| `expectedPods` | 这个组的版本的模板：Σ `replicas` ×（1 + `workerReplicas`）。`RoleRollingUpdate` 下取 T 的布局 | `scale` 子资源；模板读不到时的 Ready（A1） |
| `rolloutStrategy.type` | `spec.rolloutStrategy.type` | 哪一层的滚动参数生效 |
| `rolloutStrategy.updateMethod` | **生效的**更新方式（1.3 的 V5） | 原地更新 |
| `rolloutStrategy.progressDeadlineSeconds` | 同名字段 | 原地更新的期限 |
| `rolloutStrategy.roleCoordination` | 同名字段 | 协调 |
| `recoveryPolicy`、`restartGracePeriodSeconds` | 同名字段 | 恢复 |
| `gangPolicy`、`networkTopology`、`queueName` | 模板的同名字段；队列取 ModelServing 的注解 | PodGroup |
| `roles[]`：`name`、`replicas`、`maxUnavailable`、`maxSurge`、`partition`、`baselineReplicas` | 模板里每个 Role；`baselineReplicas` 取基线版本记下的副本数 | Role 一级 |

四条约束：

1. **没有 Pod 模板。**模板、plugin、调度器名从 ControllerRevision 读。ControllerRevision 的数据就是这三样（`utils/revision_data.go` 的 `modelServingRevisionSpec`），别的设置它没有，所以要经 `spec` 下发。
2. **没有 schema 默认值。**apiserver 补上默认值之后，存下来的 `spec` 就和 ModelServing controller 算出来的不一样，「内容不同才下发」永远成立，组永远是未观测。ModelServing 上的字段已经带着它自己的默认值，照抄即可。Kind 上验证过：写进去的 `spec` 原样读回，`generation` 是 1。有一处例外：`networkTopology.{groupPolicy,rolePolicy}.mode` 的默认值 `hard` 来自复用的 Volcano 类型，去不掉；ModelServing 的 schema 有同一个默认值，下发的值总是已经带着它，不影响「内容不同才下发」。
3. **没有关于更新的规则。**`gangPolicy` 用自己的类型 `ServingGroupGangPolicy`，不复用模板的 `GangPolicy`：后者带着「`minRoleReplicas` 不可变」的转换规则，而这条规则在字段一侧缺失时不检查，ModelServing 上可以分两步改掉它；两步落在同一次下发之间时，组的 `Update` 每一轮都被拒绝，这个组永远拿不到新的 `spec`（独立评审的发现 A2）。`spec` 只由 controller 写，ModelServing 上允许的变化在这里都要写得进去。
4. **版本的值是 revision 的身份**：Pod 的 `modelserving.volcano.sh/revision` 标签、`status.updateRevision` 用的那个字符串。ControllerRevision 对象的名字是 `<ModelServing 名>-<这个字符串>`。

**`ServingGroupRollingUpdate` 下的 `targetRevision`：旧版本的组。**组的 `spec.revision` 与 `spec.targetRevision` 不同（两个版本都读得出来，渲染出的 Pod 不同），这个组就是旧版本的组。它不把自己的 Pod 带到任何版本：不发起原地更新，不置 `RecreateRequired`，不管它的 Pod 跑着什么版本。已经打过补丁的 Pod 各自做完，做完的那个 Pod 的标签改成它被打补丁时的那个版本。扩缩、补缺、恢复照常，按 `spec.revision` 做。把这个组带到目标是 ModelServing controller 的事：在预算之内改它的 `spec.revision`（原地更新）或者替换它；序号在 partition 之下的留着。两个版本相同，或者有一个读不出来（包括 `targetRevision` 为空）时，组照 `spec.revision` 做。

没有这个字段时，组只知道自己的版本，Pod 的版本与它不同就一律当成「要带到自己的版本」。滚动方式从 `RoleRollingUpdate` 改成 `ServingGroupRollingUpdate` 时，一个组的 Pod 可以跑着两个版本，Role 的 partition 会让它一直如此。这样的组按基线下发之后，把新版本的 Pod 当成要改回来的：生效的更新方式是 InPlace 时，所有的组同时打补丁，不看预算，partition 之下的也打；是 Recreate 时置 `RecreateRequired`，而它不受 partition 约束（第二轮评审的发现 R2-M1）。

代价：模板每变一次，每个组的 `spec` 写一次。`RoleRollingUpdate` 下本来就是这样。

**滚动方式从 `RoleRollingUpdate` 改成 `ServingGroupRollingUpdate` 时**，组的 `spec.revision` 是空的，由 ModelServing controller 在下发时定，自上而下取第一个成立的：

| 组的情形 | `spec.revision` |
| --- | --- |
| `status` 说的不是现在这批 Pod（未观测或者不新鲜） | 这一轮不下发 |
| 没有成员 | 这个序号现在该有的版本：partition 之下取基线，其余取目标 |
| Pod 都跑着同一个版本（`status.revision` 不为空） | `status.revision` |
| Pod 跑着两个版本，基线与目标不同 | 基线。它随即是旧版本的组：Pod 留在各自的版本上，在预算之内被改版本或者被替换 |
| Pod 跑着两个版本，基线与目标相同或者比不出来 | 不下发，`spec` 保持原样，等它报出单一的版本 |

最后一行不能取目标：当成目标版本的组会把所有的 Pod 一起带到目标。空的 `spec.revision` 也不能写下去：那样的组永远不可替换。

### 2.2 `status`

见第 3 章。

### 2.3 标签、注解、子资源

| 对象 | 内容 |
| --- | --- |
| ServingGroup 的名字 | `<ModelServing 名>-<序号>` |
| ServingGroup 的标签 | `modelserving.volcano.sh/name` |
| ServingGroup 的注解 | `modelserving.volcano.sh/surge: serving-group`：创建时写，转正时摘，不后加。`modelserving.volcano.sh/created-revision: <版本>`：`RoleRollingUpdate` 下新建一个组时写，值是建它用的版本；给这个组下发别的目标的那一次写里摘掉，什么都还没有建出来的组则改成新的目标；策略换成 `ServingGroupRollingUpdate` 的那一次写里也摘掉；没有它的组不后加（表后的说明） |
| ServingGroup 的 owner | ModelServing，`controller`、`blockOwnerDeletion` |
| Pod、PodGroup、Service 的 owner | ServingGroup，`controller`、`blockOwnerDeletion`。PodGroup 今天没有 `blockOwnerDeletion`，补上 |
| Pod 的标签 | `modelserving.volcano.sh/instance-generation`：所在实例的代际号 |
| Pod 的注解 | `modelserving.volcano.sh/surge`：值 `role` 是 Role 一级的标记（来源就在 Pod 上）；值 `serving-group` 是组的标记的副本（输出），创建 Pod 时照组对象抄上，组失去标记之后由组的下一轮从 Pod 上摘掉 |
| ModelServing 的注解 | `modelserving.volcano.sh/servinggroup-adoption`：`InProgress`、`Done`、`RollingBack`（第 6 章） |
| ModelServing 的 condition | `RolloutBlocked`（3.11） |
| `scale` 子资源 | `spec.expectedPods`、`status.pods`、`status.labelSelector` |
| 短名、打印列 | `sg`；`REVISION`、`READY`、`PODS`、`EXPECTED`、`AGE`，宽输出另有 `FAILED` |

`scale` 子资源只给 PodDisruptionBudget 解析期望数用：PDB 对它选中的每个 Pod 找 controller owner，把这些 owner 的 `scale` 相加。选中一个 ModelServing 全部 Pod 的 PDB，分母是各组 `expectedPods` 之和，是对的。只选中某一种 Role 的 PDB，分母仍然是整组的 Pod 数，偏大；这是已知的限制，写进用户文档。今天 owner 是 ModelServing，分母是组的个数，比例型的 PDB 本来就不对。

**Pod 上组一级 surge 标记的副本。**标记本身在组对象上。两个 controller 只在一处读 Pod 上的副本：组对象丢了而 Pod 还在时（orphan 方式删组，或者旧版本留下的存量），找回这个组的那一步（S1′、T3）靠它知道这是不是 surge 组。所以副本要跟着组走：ModelServing controller 把组转正（摘掉组上的注解）之后，ServingGroup controller 在这个组的下一轮里把自己名下 Pod 上值是 `serving-group` 的注解摘掉，每个 Pod 一次带 UID 和 resourceVersion 的 PATCH。这个注解不在摘要里（3.3），所以不引起 `status` 的写。没有 PodGroup 的轮次只做删除（3.7），这一步也不做，留到下一轮。上游在转正时就把 Pod 上的注解摘掉；第一版只摘了组上的，Pod 上的副本留到 Pod 消失：日后丢了对象的组会被当成 surge 组找回，读这个注解的工具会把在服务的 Pod 当成临时的（runner 的发现 F2，回归复核的 R-2）。

**组上的 `created-revision` 注解。**`RoleRollingUpdate` 下组的 `spec.revision` 是空的，组自己看不出它是滚动之前就有的，还是滚动途中新建的。这两种组要分开：Role 的 partition 让已有的组里它之下的实例留在基线，缺了也按基线建；滚动途中新建的组没有跑过基线，整个组按目标建，partition 之下的实例也是（1.3 的 V13）。ModelServing controller 新建一个组（G4）时在创建请求里写上这个注解，值是建这个组用的版本，也就是这时的目标。为已有的 Pod 找回对象（S1′）和接管（T3）建出来的对象不写：它们代表的是已有的组。失败的组被替换、组被删掉之后重建，建出来的是新对象，也是新的组，照样写。ServingGroup controller 只在一处读它：注解的值等于 `spec.targetRevision` 时，这个组的 Role 计划把目标当作基线（第 5 章）。每一轮都从对象上读，账本里不记，所以先写后建之间换了进程、或者这样的组在同一次滚动里丢了 partition 之下的实例，建出来的仍然是目标。

注解说的是「这个组是在眼下这次滚动里出生的」，所以目标一变就摘掉：ModelServing controller 给一个组下发的目标与注解的值不同时，在改 `spec.targetRevision` 的同一次 `Update` 里把它摘掉，此后不再加。会改到目标的写有两处：下发设置；改一个组的版本（发起整组原地更新，或者把没有成员的组改成它的序号现在该有的版本）。两处都这样做。不摘的话，模板回到早先的样子时它又会生效：版本的身份是从模板算出来的，模板改回去，身份就是原来的那个。比如每个组都是 v1 时建的，滚到 v2 做完之后带着 Role 的 partition 改回 v1；或者灰度途中加了一个组，灰度放弃了又重做。这时注解的值又等于目标，而这些组已经跑过基线，它们 partition 之下的实例丢了要按基线建。两样在同一次写里改，所以谁也读不到新的目标配着旧的注解；这次写失败或者冲突时两样都没有变，下一轮照样再发。

什么都还没有建出来的组是例外。它还在出生，而新的组是整个按当前模板建的，所以给它下发别的目标时注解不摘，改成新的目标：这个组整个按新的模板建。「什么都没有建出来」是两条：账本里没有建成过的实例（没有条目，或者只有代次为 0 的 `Creating`），并且 controller 看不到它的任何 Pod。对象刚建出来还没有轮过的组是这样；承诺了 Pod 而一个都建不出来的组也是这样，比如 Pod 被配额拒绝，或者它的 controller 在建第一个 Pod 之前就停了。只看组承诺了没有（`status.pods`）不够：承诺之后 Pod 可以一直建不出来，这时模板一变，注解要是摘了，等 Pod 建得出来，partition 之下的实例就按基线建，而这个组从来没有跑过基线。

建出了东西的组跑过出生时的那个版本，下发别的目标时注解照常摘掉，还没有建全的也一样。它缺的实例从此是一个普通的组的位置：partition 之上按新的目标建，partition 之下按基线建。后一种正是这个注解要消除的现象，留在这一个窗口里，是已知的限制（第 10 章）。协调滚动下新的组本来就分两步建（依赖别的 Role 的那个 Role 等对方就绪），这个窗口不需要任何故障。建了一半的实例按它开始时的版本补全。

账本那一条读的是这一次写所基于的对象，写带着它的 resourceVersion。实例建成是组的一次 `status` 写（3.6），所以「有实例建成了」和「把注解改成新的目标」互斥：后到的一个冲突，重新判定。Pod 那一条读的是缓存，只靠账本不行：实例是在它最后一个 Pod 建出来之后的那一轮才记成建成的，有一个 Pod 被拒绝的实例根本不记，所以组可以已经有按上一个目标建的 Pod 而账本里什么都没有。

Pod 不在互斥之内：建 Pod 不是对组的写。组承诺 Pod 的那一轮接着就按它算好的计划建 Pod，期间对组写了什么它都不管。所以从组承诺起，到 ModelServing controller 看到它的一个 Pod、或者组的又一次 `status` 写落地为止，下发可以把组改成「按新的目标建的」，而组接着建出来的是上一个目标的 Pod。这个窗口不需要缓存落后：下发的那一刻这些 Pod 确实还不存在。下发撞上承诺而冲突时，重新判定的那一轮正是这次 `status` 写带来的，恰好落在建 Pod 的当口。窗口仍然窄：模板要在新的组承诺前后的那一点时间里再变一次。

ServingGroup controller 看得见自己的 Pod，所以这样的组它不照注解办。组里只要有一个不属于目标的 Pod，Role 计划的基线就取 `spec.baselineRevision`，和普通的组一样（第 5 章）。属于目标的 Pod，是带着目标版本的，或者带着别的版本而它的 Role 在两个版本里渲染出来一样的。版本读不出来、比不了的 Pod 不算属于目标：不然等那个版本从历史里清掉，组又会照注解办。正在终止的 Pod 不算，组并不留着它。这样，窗口里的组和建出了一部分的组结果一样：它还没有的实例在 partition 之下按基线建，组是全的（第 10 章）。没有这一条的话，它 partition 之下已有的 Pod 留在上一个目标，其余的按新的目标建；协调滚动、各个 Role 的 partition 都等于副本数时，依赖别的 Role 的那个 Role 等不到目标版本的容量，一直不建，组停在不全的状态。按目标建出来的组，Pod 带的就是目标版本，不用比，所以平常这一条不起作用，这样的组丢了 partition 之下的实例也照旧按目标建回来。最后一个别的版本的 Pod 没有了之后，组里只有目标的 Pod，又照注解办。

注解只在 `RoleRollingUpdate` 下有。`ServingGroupRollingUpdate` 下组按 `spec.revision` 整个建，组的 partition 之下的组用的是基线；注解说的「按目标建」在那里不成立，留着的话，策略换回来那天又会被当真。所以给组下发 `ServingGroupRollingUpdate` 的那一次写里把它摘掉，那里不写也不改；换回 `RoleRollingUpdate` 之后每个组都是已有的组。

没有注解的组，建没建出东西都不加。不改目标的写不动它：目标版本记不下来时的受限轮次（7.4）只减副本数，转正只摘 surge 标记。只有 ModelServing controller 写这个注解。与组的 `spec` 对不上的注解，下发时照上面的规则处理一次（摘掉，什么都没有建出来的组改成目标），之后不再为它写；这种状态是别人加了注解，或者不认识这个注解的构建改了目标留下的。

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
| `revision` | 汇总 | ModelServing controller：`RoleRollingUpdate` 下的 G6、切换滚动模式、`updatedReplicas` | 不在终止中的成员 Pod 运行的版本，从 Pod 读，不抄 `spec.revision`。一个 Pod 运行着它标签上的版本，也运行着它的 Role 渲染出同样 Pod 的每个版本：逐 Role 滚动不动没有变化的 Role，连标签也不改。标签都相同时就是那个版本；不同时依次试 `spec.targetRevision`、`spec.revision`、`spec.baselineRevision`、各个标签，取第一个全体成员都运行着的。没有这样的版本，或者没有 Pod，为空。原地更新做完之前 Pod 的标签是旧的，这里也是旧的 |
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
| `RecreateRequired` | 这个组只能删掉重建：配了原地更新而它的 Pod 打不了补丁；或者按重建更新而组里有 Pod 的版本与组的不同（收养来的成员、切换更新方式之前没有打完补丁的成员）。「版本不同」按 Role 比较，两种更新方式都这样比：Pod 所属的 Role 在两个版本里渲染出同样的 Pod 就不算不同（逐 Role 滚动之后没有变化的 Role 保留着旧标签）；版本读不出来时等，不重建。旧版本的组（2.1）不置它。`ServingGroupRollingUpdate` 下只加不撤；`RoleRollingUpdate` 下的组不带它，带着的在下一次算 `status` 时去掉 | 可以按配置的方式更新 | `InPlaceUpdateNotPossible`（两种情形共用，message 写明是哪一种） | 可替换，不看版本，不看 partition |
| `DrainBlocked` | 有实例停在清空中：`OnRoleDelete` 不成功 | 没有 | `RoleDeleteHookFailed` | 不读 |
| `ReconcileFailed` | 这一轮出错。`Ready` 仍然按看到的成员照实写 | 这一轮没有出错 | `NameOccupied`：成员 Pod、PodGroup、Service 的名字被不属于这个组的对象占着；组名下的 Pod 丢了成员标签；同名的上一个组对象的 Pod 还在（3.6）。`Error`：其余的错 | 不读；ModelServing 的 message 里引用 |
| `CoordinatedRoleRolloutBlocked` | 与今天 ModelServing 上的同名 condition 相同，按组报告 | reason 分 `ProgressAvailable` 和 `RolloutComplete` | 与今天相同 | 汇总：有组为真时取序号最小的那个组的 reason 和 message |

几条容易错的：

- `Failed` 为真时 `Ready` 照实写，ModelServing controller 不看它。
- `Failed` 和 `Drained` 可以在同一次写里都为真（S3′：一个 Pod 都没有了）。
- `Drained` 可以从真回到假（S4′：又有成员故障）。这时 Pod 变了，摘要对不上，G1 的条件本来就不成立。
- `RecreateRequired` 为真的组如果 Pod 是健康的，`Ready` 是真，照常计入可用数，按健康的额度替换。
- `RecreateRequired` 只对整组替换有意义。`RoleRollingUpdate` 下没有人替换整个组：留着它，这个组永远不算已更新，ModelServing 永远是 `UpdateInProgress`；滚动方式改回去的那一天，这个组不管 Pod 跑着什么都会被替换（第二轮评审的发现 R2-M6）。
- 在原地更新的实例不是 `Ready`（reason `InstancesNotReady`），从决定打补丁的那一轮到 Pod 的标签改完为止。「在原地更新」要有事实：有 Pod 已经打过补丁而标签还没有改，或者这一轮要打补丁。只是因为某个 Pod 的版本读不出来而在等的单元，一个补丁也没打，不算；它的 `Ready` 照 Pod 算（第 5 章）。

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
| E7 | 创建中，成员不齐 | 缺的建，带代际号 g；位置上有 `Terminating` 的等它消失；名字被不属于这个组的 Pod 占着：报告，等；同名的上一个组对象的 Pod 还在：这个组这一轮一个成员也不建（3.6） | 建 Pod |
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
| L7 名字被别人占着 | 创建返回 `AlreadyExists` 之后直读那个 Pod，核对 controller owner 的 UID。同名的上一个组对象留下的 Pod 没有消失或者被释放之前，新对象不建任何成员（3.6） |
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
| 同名的上一个组对象的 Pod 还在 | 0（这一轮什么都不建，`started` 为假） | 是 |
| 做不出计划的一轮（`OnModelServingSync` 阻塞、Role 的计划出错） | 读到的 `pods` 和现有成员数里大的那个（3.7） | 与上一轮相同 |

ModelServing controller 的 PATCH 第一次常常输给新对象的第一次 `status` 写。这是一次重试，不算「一个写失败」，不触发「这一轮后面的写都不发」。

**不与上一个对象的 Pod 并存。**用户用 Background 方式删组时，对象立刻消失，ModelServing controller 随即建出同名的新对象，垃圾回收器再一个一个地删上一个对象的 Pod。上一个对象的 Pod 是：controller owner 是同名而 UID 不同的 ServingGroup 的 Pod。它们不是成员，不进摘要，但是进这一轮的观察：只要还有一个，这一轮就一个成员也不建，报 `ReconcileFailed`（`NameOccupied`，message 写还剩几个和其中一个的名字），`pods` 记 0，组照旧算没有成员。最后一个消失之后再建；被释放成无主的 Pod（orphan 方式删组留下的）不算上一个对象的，走收养（R2）。为了在缓存里找到它们，Pod 多一个索引（4.3）；它们消失的事件按 owner 的名字映射到这个组，带来下一轮。

第一版是哪个成员的名字空出来就建哪个，新旧两个对象的 Pod 会并存一小段时间（runner 的发现 F5，008 的 NT4）。

### 3.7 写协议

一轮里 `status` 至多写两次，多数轮次一次或者零次：

| 写 | 什么时候 | 内容 | 被拒绝时 |
| --- | --- | --- | --- |
| 动手之前 | 每一轮都算；算出来的和读到的不同才写。它同时是 R8（账本的转换、Failed）、L8（先写后建的那一位）和 R11（汇总） | 账本、Failed、Drained、`pods`，和这一轮算出来的全部汇总。**`ReconcileFailed` 照读到的原样留着**：这一轮会不会出错，动手之后才知道 | 返回。这一轮后面不发任何删除、创建、补丁 |
| 动手之后 | `ReconcileFailed` 有变化：这一轮出了错而之前没有（或者错得不一样），或者之前有而这一轮没有出错 | 同上，`ReconcileFailed` 按这一轮的结果 | 重新入队 |

- 008 把 R8、L8、R11 分成三次写。三次的内容都是从同一份观察算出来的，合成一次之后「账本、Failed、先写后建的那一位都在任何删除和创建之前落盘」不变，少一到两次写。
- 同一个错误再出现时，两次算出来的 `status` 都与已经写上的相同，不再写：出错不会变成一圈一圈的 `status` 写，队列的退避才起得了作用。第一版在动手之前的那次写里把 `ReconcileFailed` 去掉、出错之后再加回来，于是每个出错的轮次写两次，每次写又触发下一轮（独立评审的发现 S1）。
- **清空钩子（`OnRoleDelete`）失败的轮次返回错误**：实例停在 Draining、没有 Pod，除了重试没有别的事件会带来下一轮（S2）。
- **没有 PodGroup 的轮次照样删，只是不建、不打补丁**：已经失败的组要先清空才能被替换（S4）。这样的轮次里「还要建几个」照旧计入 `pods`：缓存还没显示出来的成员可能就在其中，写成 0 等于告诉 ModelServing controller 这个组没有成员（S6）。PodGroup 的布局读不出来（7.3）也是这一种。这一轮自己写下的 Draining 也照样删 Pod，见第 10 章。
- **做不出计划的轮次保留已经承诺的成员数。**`OnModelServingSync` 阻塞，或者 Role 的计划出错（比如协调滚动没有拿到基线的副本数），这一轮不知道还要建几个，只写 `status`：`pods` 取读到的值和现有成员数里大的那个，不按缓存里的成员数重算。刚建完第一批 Pod、缓存还没有显示出来的那一拍，按缓存算是 0，等于告诉 ModelServing controller 这个组没有成员（第二轮评审的发现 R2-S4）。阻塞的钩子带 `RequeueAfter` 时这一轮不返回错误，到点重新入队；不带时返回错误，走队列的退避（R2-S6）。
- **写用 `UpdateStatus`，带读到的 resourceVersion。**不用 PATCH：要的就是「读之后没有人写过」这个前置条件。
- **写回校验。**`UpdateStatus` 返回的对象里，账本、`observedPods`、`pods` 要与发出去的相同。不同，说明集群里的 CRD 比 controller 旧，字段被裁掉了：这一轮按出错处理，不发后面的删除和创建，发事件 `StatusNotPersisted`。这样「先写后做」不会因为写被悄悄裁掉而变成「没写就做」。做法取自 RoleBasedGroup。
- **出错的轮次也写**（A1）：只要读到了成员，`observedGeneration` 照样推进，`Ready` 照实算，带 `ReconcileFailed`。不写的话这个组会一直是未观测，或者一直带着上一次的 `Ready`。
- **`observedGeneration` 的推进**：这一轮的决定是按这份 `spec` 做的，不等动作做完。

一轮里的次序（R0–R11 的实现）：

```text
R0  取读锁（回退的交还要等它，6.8）
R1  取组。组在终止中：前台删除、成员都没了、不在回退中，就尽力调 OnRoleDelete（7.2），返回。
    没有 owner，或者名字不是 <ModelServing>-<序号>：在组上发事件 OwnerNotFound，返回。
    所属的 ModelServing 不在了或者正在删除：不发事件，返回（垃圾回收器会删掉这个组）。
    回退的开关在，或者所属的 ModelServing 是 RollingBack：返回
R2  有待接管的成员：直读这个组，换 owner
R3  观察：成员 Pod 和同名的上一个对象留下的 Pod（缓存）；spec 引用的 ControllerRevision；
    OnModelServingSync（阻塞性的错误：只写 status，不建、不删、不打补丁；带 RequeueAfter 时到点再问）
R4  成员分类、实例状态
R5  组已失败：只删 Faulty 的成员；算 Drained；到 R11
R6  Role 一级的计划：哪些实例在期望之内、建在哪个序号、用哪个版本（出错：只写 status）
R7  账本的转换。要写 Failed、写 Draining、删 Pod、打补丁、结束清空（E3）、协调滚动途中的创建：直读，重做 R4–R7
R8  清空完了的实例调 OnRoleDelete；同步 PodGroup；提交（status：账本、Failed、先写后建的那一位、汇总）
R9  删除。没有 PodGroup 的轮次到此为止
R10 摘 surge 标记；OnRoleSync；创建；原地更新的补丁；OnPodReady
R11 这一轮的出错有变化时再写一次（status）
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

**接管期间 `status` 不动。**接管没有做完的轮次（6.4 的 M3 的闸）里，ModelServing 的 `status` 保持旧版本写的样子：四个计数、`Available`、`Progressing`、`UpdateInProgress`、协调的 condition、`observedGeneration` 都不重算，`RolloutBlocked` 也不报。`currentRevision`、`updateRevision`、`labelSelector` 照常算；稳态下它们与旧版本写的相同，这样的 ModelServing 在接管期间不写 `status`。各组都报告过、接管注解写成 `Done` 的那一轮起照常重算。

两个原因：

- 刚建出来的组还没有写 `Ready`。按它们算，`Available` 在每个 ModelServing 上翻成假、`Progressing` 翻成真，接管完再翻回来，而 Pod 一个也没有动。看着这个 condition 的告警、`kubectl wait --for=condition=Available` 的门禁，在升级时对每个 ModelServing 触发一次。第一版只冻住了计数，Kind 的升级演练里看到了 condition 的翻转（回归复核的 U-1）。
- `observedGeneration` 推进而协调的 condition 还是上一代写的，这两样合起来说的是假话。校验 webhook 把「`observedGeneration` 不小于 `generation`，并且协调的 condition 的 reason 是 `RolloutComplete`」当成没有协调滚动在进行；它会把一次将要开始的滚动当成已经完成，放过不该放的修改（第二轮评审的发现 R2-M5）。

代价：接管期间对 ModelServing 的修改不被处理，`observedGeneration` 也不动；LWS controller 把 `observedGeneration` 落后的 ModelServing 当成 `status` 陈旧。升级步骤要求升级前各 ModelServing 已经对账完、验收之前不改它们（6.7）。

回退的开关在的时候 `status` 整个不更新，`observedGeneration` 也不推进：这期间不要改 ModelServing。

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
| Pod 按 controller owner 的名字（只收 controller 是 ServingGroup 的） | 同名的上一个组对象留下的 Pod：owner 的名字是这个组、UID 不是（3.6） |

| 事件 | ServingGroup controller 入队 | ModelServing controller 入队 |
| --- | --- | --- |
| ServingGroup 增、改、删 | 这个组 | 它的 owner |
| 成员 Pod 增、改、删（含 owner 被改：新旧两个对象各映射一次） | 所在的组 | 所在组的 owner（3.3） |
| PodGroup、Service 改、删 | owner 的那个组 | — |
| ControllerRevision 增 | 引用它的组 | 它的 owner |
| ModelServing 增、改、删。改是每一次更新，只改元数据或者只写了 `status` 的也算（今天 `spec` 没有变的更新不入队） | `recoveryPolicy`、grace 或者接管注解变了：名下的组 | 它自己 |
| 定时 | grace 到期；原地更新期限到期；被占着的名字的重试；阻塞的 `OnModelServingSync` 给的 `RequeueAfter` | 阻塞原因的期限；终止中的组的等待期限；有组不新鲜时的重试；不阻塞的 `OnModelServingSync` 给的 `RequeueAfter` |

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
| `utils` | 渲染 Pod 的函数（`GenerateEntryPod`、`GenerateWorkerPod`）没有改：ServingGroup controller 渲染之后把 owner 换成组、补上代际号标签。只有旧 controller 调用的辅助函数随旧包删掉 |
| `podgroupmanager` | 去掉对 `datastore` 的依赖；owner 由调用者给，补 `blockOwnerDeletion`；队列名从参数来；按名字删 PodGroup 的入口去掉（靠级联）。旧 controller 的入口随旧包删掉，ServingGroup controller 只走 `SyncPodGroup` |
| `plugins` | `HookRequest` 加组对象（`Owner`）和实例的代际号（`Generation`）。headless Service plugin：请求带着 `Owner` 时，Service 的 owner 是组；删除钩子里按代际号删这个实例的 Service，比较的是 Service 上记的组 UID 和代际号，不用进程里的 `RoleCleanup` 记忆。`RoleCleanup` 这个类型还在，只有不带 `Owner` 的请求会走到它 |

### 4.5 测试替身

client-go 的 fake clientset 不检查 resourceVersion 和 UID 的前置条件，不模拟前台级联和垃圾回收。只用它的话，把 4.4 的前置条件全部去掉，测试照样通过。

所以先写一个测试替身（`apitest`，只在测试里用）：在 fake clientset 上加 reactor，检查 `Update`、`UpdateStatus`、`Patch`、`Delete` 的 resourceVersion 和 UID，创建时分配 UID 和递增的 resourceVersion，前台删除时先给对象打 `deletionTimestamp` 和 finalizer、等名下的对象都删掉再删它。两个 controller 的测试都跑在它上面。

在它上面再做一个停点测试的框架：按脚本跑一个场景，在第 k 次写之后停下，换一个没有任何内存的 controller 实例接着跑（可以配一份旧的缓存），终局要和不停相同；对每个 k 都做一遍。Kind 上的杀进程只作为确认。

## 5. Role 一级怎样搬

008 的 4.9 说组一级和 Role 一级是同一套按序号的规则，计划可以是同一个函数。实现照这句话做：

| 部分 | 做法 |
| --- | --- |
| Role 的扩缩容；独立的 `RoleRollingUpdate`（额度、partition、surge、转正、回收、次序） | **调用同一个计划函数** `grouplevel.Plan`，单元是一个组里某种 Role 的实例。实例到单元的对应：清空中 = 终止中；已建成并且成员全部就绪 = 就绪；Role 一级的 surge 标记 = surge；版本按这个 Role 的模板比较，语义相同的算同一个版本。基线取 `spec.baselineRevision`；带 `created-revision` 注解、它等于目标、并且组里没有别的版本的 Pod 的组，基线取目标（2.3）。`ServingGroupRollingUpdate` 下用它「不分版本」的那一支：只扩缩、补洞、摘掉留下的标记 |
| roleCoordination | 纯函数（`calculateRoleRolloutLimits`、`slowestReadyProgress`、`allowedStartedReplicas`、依赖闭包、稳定集合等）连同它们的测试原样搬进 `servinggroup/coordination.go`。读 `datastore` 的部分改成读账本和成员（`coordination_state.go`）。它不是另一套计划，而是 Role 计划上的一层过滤：`grouplevel.Plan` 给出候选，协调的上界决定哪些实例这一轮可以开始、哪些删除要压住。基线的副本数照旧记在 ControllerRevision 的注解上，另经 `spec.roles[].baselineReplicas` 下发；没拿到它的组这一轮报 `ReconcileFailed`、什么都不滚。组在 `status` 里报 `CoordinatedRoleRolloutBlocked`，ModelServing 取序号最小的那个组的；有组还没有按下发的 `spec` 报告时不说 `RolloutComplete`（保守的一侧：校验 webhook 读的是「不是 `RolloutComplete`」）。协调滚动途中的创建与删除一样要直读。账本里有而还没有 Pod 的实例，协调按它将要用的版本算：partition 之下是基线，其余是目标；基线取目标的组里都是目标，与 Role 计划一致。从上游逐字搬来的规则里有两种停住而 condition 不说的状态，原样保留（第 10 章） |
| PodGroup 的布局（`workloadForPodGroup`） | 搬成 `sgcontroller.podGroupView`。`RoleRollingUpdate` 下的组，不论有没有 roleCoordination，每个 Role 取各实例还在跑的最小布局；roleCoordination 另有一条，等依赖的 Role 的 gang 下限先不抬（7.3） |
| 目标版本记不下来时的缩容（`scaleDownOnRevisionError`） | 搬成 `mscontroller` 里一个受限的轮次：只做副本数的减少，不建、不换（7.4） |
| 原地更新 | `inplace` 包不动。`controller/inplace_update.go` 里按单元推进的部分搬进 `sgcontroller`，判定是纯函数 `servinggroup.PlanInPlace`（表后的说明）；「能不能原地更新」里只看模板的部分（`OnlyImagesChanged`）移到 ModelServing controller |
| 建 Pod、PodGroup、plugin 钩子 | 搬，owner 换成组 |
| 缩容的次序（未就绪的先、deletion cost 低的先、序号高的先） | 比较函数原样搬；优先级从账本和成员得出 |

旧函数的去向逐个列在任务 010 的 `subagents/proposal-analysis.md` 第 1 节。旧的 `controller` 包和 `datastore` 包已经删除，分 11 个提交，每个都能编译：先是旧 controller 的测试，按它们共用辅助函数的次序分六个提交；再是它的源码、`podgroupmanager` 里只有它调用的入口、`datastore`、`plugins` 的 `RoleCleanup.Capture`、`utils` 里只有它调用的辅助函数。上游就已经没有人引用的四个 `utils` 函数这次不删，只剩测试在用的几个导出函数也留着：与本任务无关的清理不带进来。被删的每个测试函数（313 个）的去向——新包里的哪个单测、哪个 e2e，或者不再适用的理由——在任务 010 的 `evidence/old-code-removal/REPORT.md`。

**原地更新的判定（`servinggroup.PlanInPlace`）。**一起换版本的 Pod 是一个单元：整组替换时是整个组，目标是 `spec.revision`；逐 Role 滚动时是一个 Role 实例，目标是 Role 计划给它的版本。判定只读 Pod、Pod 上的进度注解和版本，所以换了进程接着做。

自上而下取第一个成立的：

| 情形 | 做什么 |
| --- | --- |
| 整组替换的组是旧版本的组（2.1） | 不发起，不要求重建。打过补丁的 Pod 各自做完，标签改成它被打补丁时的版本 |
| 没有在途的更新（没有打过补丁而标签还没改的 Pod），也没有一个 Pod 的 Role 与目标渲染不同 | 什么都不做：不打补丁，不改标签，不要求重建。不分更新方式。有 Pod 的版本读不出来时也一样，只记一行日志 |
| 有待接管的成员，或者有终止中的成员 | 等 |
| 单元回到了 Pod 标签上的那个版本，而 Pod 的镜像已经为另一个版本改过 | 要求重建：按标签看它们已经是目标，没有哪一次滚动会把镜像改回来 |
| 还有 Pod 的标签不是目标、又没有在打目标的补丁；生效的更新方式是 InPlace | 检查能不能只改镜像：重启策略、容器对不对得上、Pod 的版本与目标之间是不是只差镜像（整组替换比整份模板，逐 Role 滚动比这个 Role）。与目标渲染相同而标签是旧版本的 Pod，最后一项直接算通过，不问别的 Role 中间变过什么；它跟着别的 Pod 走一个不带镜像的补丁，标签随单元一起改。能：打补丁。不能：要求重建（整个组置 `RecreateRequired`，Role 实例由 Role 计划清空重建），报 `InPlaceUpdateFallback`。版本读不出来：等；一个补丁都还没打的话，这个单元不算在更新 |
| 同上；生效的更新方式不是 InPlace | 其中有 Pod 的 Role 与目标渲染不同，或者正在更新到别的版本：要求重建，不报 fallback。有版本读不出来的：等。都与目标渲染相同：不动它们，把别的 Pod 已经开始的更新做完 |
| 该打的补丁都打了 | 等打过补丁的 Pod 都跑着目标的镜像并且就绪、单元的成员齐全，然后改标签，报 `InPlaceUpdateCompleted` |

「与目标渲染相同的 Pod」这几处是第二轮评审之后定下来的（R2-M2、R2-S1、R2-S2）。逐 Role 滚动不动没有变化的 Role，这些 Pod 带着旧版本的标签。原先 InPlace 下拿两个版本的整份模板比：别的 Role 中间改过镜像以外的字段，整组就被要求重建，而每个 Pod 都已经是目标渲染出来的样子；只改过镜像，所有的组同时开始一次没有镜像可改的更新，标签改完之前都不是 `Ready`。原先一个 Pod 的版本读不出来，单元就算在更新，组一直不是 `Ready`，而没有任何补丁打过、也不会打。

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
| `DeletionIdentityChanged` 事件，和它背后「删除途中同名的 Pod 换了一个」的分支 | 每个删除带 UID 的前置条件，换过的 Pod 删不到，没有这个分支 |

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
| X1 | 自查 | chart 给 controller-manager 的 Pod 模板加标签 `modelserving.volcano.sh/controller-protocol: servinggroup`。标的是协议的代，不是镜像版本：以后新版本之间的升级不应当被它挡住。进程启动时（参选之前）另外给自己的 Pod 补上这个标签，补不上就每 5 秒重试：不是用这个 chart 部署的 Deployment 可能没有模板上的标签，那样同一个版本的几个副本会互相当成旧版本、谁也不启动。进程自己补标签只是兜底，模板上的标签仍然是必需的：新副本从被建出来到它的进程补上标签为止没有标签，正在工作的主这时会把它当成旧版本而退出（第 10 章）。自己的 Pod 名取环境变量 `POD_NAME`（chart 经 downward API 注入），没有时取主机名。进程拿到租约之后、启动任何 controller 之前，在自己的命名空间里列出带 `app.kubernetes.io/component: kthena-controller-manager`、不带这个标签、还没有结束（`Succeeded`、`Failed` 之外，终止中的也算在跑）的 Pod；有，就不启动任何 controller，只应答 webhook，每 5 秒查一次，在自己的 Pod 上发事件 `WaitingForPredecessor` 说明在等谁。运行中每 5 秒查一次；看到这样的 Pod，进程退出（退出码非零），容器重启之后回到启动自查。不在集群里跑（本地调试）时不查 |
| X2 | CRD 和权限的闸 | 进程一启动就试着 LIST 一次 ServingGroup，每 5 秒重试，不论有没有拿到租约。成功之前 `/readyz` 返回 503（Deployment 的就绪探针指向它），`/healthz`（存活）照常，两个 controller 不启动。不退回旧的工作方式。新 Pod 不就绪时滚动更新不会停掉旧 Pod，旧版本接着工作：漏装 CRD 只是让升级停在那里。chart 里另用 `lookup` 在升级开始之前失败；`lookup` 在 `helm template` 和多数 GitOps 的渲染里返回空，所以只在「release 的命名空间查得到而 CRD 查不到」时才失败 |
| X3 | 换凭据 | chart 里 controller-manager 的 ServiceAccount 改名为 `kthena-controller-manager-servinggroup`，两个绑定跟着改。`helm upgrade` 删掉旧的 ServiceAccount，旧版本的进程（包括失联节点上还活着的）随即失去凭据，写不了任何东西。开着选主的进程续不上租约，自己退出：Kind 上看到被替换的 Pod 在被 Deployment 删除之前短暂显示 `Error`、重启一次，这是预期的。没开选主的进程（旧版本的默认值）不退出，失去凭据之后留到 Deployment 的滚动把它删掉。装回旧 chart 时反方向同样生效，回退靠它保证新版本的进程不再写。X2 和 X3 有一处相互作用：CRD 没装而 chart 的检查又没有触发（Helm 不能 `lookup`）时，X2 让新 Pod 不就绪、旧 Pod 留着，X3 却已经让旧 Pod 的凭据失效，这段时间没有 controller 在工作。不另加机制，写进升级步骤当作漏装 CRD 的代价：工作负载不受影响，只是不恢复、不滚动；装上 CRD 之后新进程自己变成就绪 |
| X4 | 选主 | `--leader-elect` 默认改成真；失主的回调改成退出进程（今天只打一行日志，进程留着，已经在睡眠的 grace goroutine 还会去删 Pod）。`ReleaseOnCancel` 仍然是假 |

运行中的自查选「退出」而不是 008 写的「停掉两个 controller、继续应答 webhook」：informer 和队列停下之后不能原地重启，退出重来是同一个效果，少一条要测试的路径。代价是这个 Pod 重启的几秒里它不应答 webhook；别的副本和那个旧 Pod 照样应答。

选主不是隔离栅：失主的进程在发现失主之前发出的请求照样落地。所以每个写另带前置条件（4.4），两个进程并存时不会多建组，可能发生的是一个额度被用两次。

Deployment 的更新策略是默认的 RollingUpdate（多一个、不少一个）：新 Pod 先就绪（webhook 起来），旧 Pod 再被停掉，webhook 没有空窗。X1 的等待不能影响就绪，否则旧 Pod 永远不会被停掉。

### 6.4 接管

按 ModelServing 进行，每一步幂等。进度记在 ModelServing 的注解 `modelserving.volcano.sh/servinggroup-adoption` 上：没有（还没查过）、`InProgress`、`Done`、`RollingBack`。

**存量对象**：带这个 ModelServing 的名字标签、controller owner 是它（按 UID）的 Pod、PodGroup、Service；接管没做完时另包括带标签、没有 controller owner、名字对得上的 Pod。终止中的存量 Pod 不算。

| 步 | 谁 | 做什么 | 实现上的点 |
| --- | --- | --- | --- |
| T1 | ModelServing controller | 注解不是 `Done`：直读存量的 Pod。一个都没有，并且名下的组都已观测、新鲜、没有待接管的成员：写 `Done`。否则写 `InProgress` | 一次 LIST（Pod），按名字标签，按 owner 的 UID 过滤，只数有成员身份的（标签和名字都是 `<组>-<role>-<实例>-<成员>`）。owner 是 ModelServing 而没有成员身份的 Pod（比如照着成员抄出来、改了名字的副本）没有组会接管它：不等它，不动它，在 ModelServing 上发事件 `PodNotTakenOver`；等它的话接管永远做不完，而且没有任何提示（独立评审的发现 M6）。PodGroup 和 Service 不查：它们由各组的同步入口顺手接过去（T4），不拦 `Done`。**新建的 ModelServing 也走这一步**，结果是 `Done`。注解写在主资源上，要过校验 webhook，而它对每次 UPDATE 都重新校验整个对象：按现在的规则不合法、今天却在运行的对象会卡在接管上。所以校验 webhook 放行 `spec` 在语义上没有变的 UPDATE（只放宽，不违反 P4） |
| T2′ | — | **没有实现。**没有活的存量 Pod 的组名下如果留着存量的 PodGroup、Service：序号小于 N 的，G4 建出组对象之后由 T4 接过去；序号不小于 N 的留着，owner 还是 ModelServing，随它删除时被回收 | T7 不查 PodGroup 和 Service，所以它们不拦接管。代价是可能留下几个没有 Pod 的旧对象 |
| T3 | ModelServing controller | 每个还有活的存量 Pod 的组名：建同名的 ServingGroup | 就是 S1′：存量的 Pod 和无主的成员 Pod 走同一段代码。`spec.revision` 取这个组的 Pod 的 revision 标签；标签不一致的取这个序号现在该有的版本。`spec.targetRevision` 一并写上 T：存量里版本不是 T 的组，接过来就是旧版本的组（2.1），不把自己的 Pod 带到别的版本，`Done` 之后按预算被替换，或者留在 partition 之下。surge 注解判定一次：`ServingGroupRollingUpdate`，序号不小于直读到的 N，名下至少一个 Pod（含终止中的）带值是 `serving-group` 的注解。撞名字时核对 owner 的 UID，对象上有没有注解以第一次创建的为准 |
| T4 | ServingGroup controller | 每个待接管的成员：先直读这个 ServingGroup（UID 没变，没有 `deletionTimestamp`）和所属的 ModelServing（注解不是 `RollingBack`），再把 owner 换成它 | 一次 JSON merge patch：整个 `ownerReferences` 列表（保留不是 controller 的 owner）、代际号标签（没有的补 0），body 里带 Pod 的 `uid` 和 `resourceVersion`。PodGroup 在 `podgroupmanager.SyncPodGroup` 里、headless Service 在 plugin 的 `OnRoleSync` 里原地换 owner（UID 不变；Service 另带上实例的代际号，删除钩子只删代际号不大于请求的）。ClusterRole 要给 PodGroup 和 Service 加 `patch`，给 `servinggroups/finalizers` 加 `update`（开着 `OwnerReferencesPermissionEnforcement` 的集群里，设置 `blockOwnerDeletion` 要它） |
| T5 | ServingGroup controller | 账本从零值开始。一个实例的成员按它应用的版本的布局齐全：写 Established | 就是 E8 |
| T7 | ModelServing controller | 直读：没有存量的 Pod 了，并且名下的组都已观测、新鲜。写 `Done` | 等组报告过再写 `Done`：刚建出来的组还没有写出 Ready，这时重算，可用数会掉到 0 再回来（Kind 和单测里都看到过），`Available` 会翻成假再翻回来（3.11） |

`InProgress` 期间，ModelServing controller 对这个 ModelServing 只做 T3 的创建，G0 到 G6 的写都不发（M3 的闸），`spec` 也不下发，`status` 保持旧版本写的样子（3.11）。ServingGroup controller 对已经换完 owner 的组照常工作。

T4 与 orphan 删组之后的收养是同一段代码（R2）。差别只有一处：收养来的无主成员版本与组的不同要报 `RecreateRequired`；owner 是 ModelServing 的存量成员不比较版本（原地更新到一半的组里 Pod 的标签是旧的）。

**账本从零值开始带来的缺口。**旧版本没有在任何对象上记「建成过」。升级的空窗里丢的 Pod，接管之后只补，不按范围恢复。这与旧版本自己升级时相同；之后每一次升级都按范围恢复。

### 6.5 升级那一刻的各种状态

| 升级时 | 接过来之后 |
| --- | --- |
| 稳态 | N 个同名的 ServingGroup，实例都是 Established。没有动作 |
| 带 surge 的滚动途中 | T3 建的 surge 组带注解。`Done` 之后照规则做完：替换旧组、在腾出来的序号上重建、回收 surge 组 |
| N 刚调大，surge 组的序号已经小于 N | 对象不带注解，是正式的组；Pod 上的副本由这个组的下一轮摘掉（2.3） |
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

一个只读的子命令：`kthena preflight model-servings [NAME] [-n NS | -A] [-o table|json] [--baseline-file F | --compare-baseline F] [--skip-dry-run]`（动词在前，与 `kthena get`、`kthena describe` 一致），对集群和每个 ModelServing 给出结论（`ready`、`attention`、`wait`、`blocked`；退出码 0、2、2、3，没能跑是 1）。`--compare-baseline` 在升级之后拿升级前的记录核对。输出的项照 008 的 06 第 7.1 节，去掉只属于在网版本的几项。必须有的三项：

1. 按目标版本的算法算出当前 `spec` 的版本，与存量 Pod 的 revision 标签比：「升级后不滚动」或者「升级后会滚动 k 个组」。
2. 不带 surge 标记的组多于 N 的 ModelServing。
3. 对每个 ModelServing 做一次 server-side dry-run 的注解写入，确认它过得了校验 webhook。

另外存下验收用的基线：每个 Pod 的 UID、各容器的重启次数、Ready 的时间；PodGroup 的 UID；`status.availableReplicas`。

### 6.7 操作步骤和验收

| 步 | 做什么 |
| --- | --- |
| 1 | 升级前检查。另外确认每个 ModelServing 的 `status.observedGeneration` 等于 `metadata.generation`：接管期间这个字段不动（3.11）。升级前检查的子命令不查这一项，用户文档给了命令 |
| 2 | `kubectl apply --server-side` 装 ServingGroup 的 CRD。Helm 升级不处理 `crds/` |
| 3 | `helm upgrade`。从这一步到验收完，不排空节点，不改 ModelServing |
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

几点：

- **与收养的竞争。**正常的一轮里 owner 是 ModelServing 的 Pod 是待接管的成员。开关出现时正在跑的一轮可能把刚换回去的 Pod 又接回来。所以 T4 换 owner 之前的那次直读里另查所属 ModelServing 的注解：是 `RollingBack` 就不换；回退的第 3 步在第 2 步的写成功之后才开始。
- **与正在跑的一轮的竞争。**上一条只管得住「读注解之后才换 owner」的那一轮。开关出现之前已经读过注解的一轮，可能在交还读完这个组、确认它名下没有东西之后，才收养一个 Pod 或者建出一个 Pod；组随后被 orphan 删除，这个 Pod 要么跟着丢了 owner，要么（建在组消失之后）被垃圾回收。所以 ServingGroup controller 的每一轮从头到尾持有一把读锁，开关是在锁里读的；一个 ModelServing 的交还（`rollBack`）先拿写锁：到那时开关出现之前开始的轮次都已经结束（独立评审的发现 M2）。两个 controller 在同一个进程里、同一个租约下，进程内的锁就够了；换主重叠的那一个请求不在其中（第 10 章）。
- **拿到锁之后再看一次开关，并且持锁到这个 ModelServing 交还完。**`rollBack` 是在锁外面看到开关才进来的。它等锁的时候开关可能被摘掉：摘开关放行了各组的轮次，它们照常收养、创建，而只在等锁之前看过开关的交还接着换 owner、删组。所以拿到写锁之后再读一次开关，不在了就放弃这一次交还（摘开关的事件会带来正常的一轮）；在，就一直持锁到这一次 `rollBack` 返回：这个 ModelServing 交还完，或者因为它有组还在终止中而稍后重试。持锁期间没有哪个组的轮次能开始（第二轮评审的发现 R2-M3）。代价有两处：各 ModelServing 的交还是串行的，一次一个；交还进行的时候所有组的轮次都在等锁。回退模式下各组的轮次本来就什么都不做，等的只是锁。一个 ModelServing 的交还要直读它的组和 Pod，对每个 Pod、Service、PodGroup 各发一次 PATCH，每个组一次删除，持锁的时间随它的大小走。
- **放弃回退**：删掉开关。注解是 `RollingBack` 的和已经没有注解的 ModelServing 落回 T1，重新接管。开关在的时候各组的轮次什么都不做就结束了，带来那些轮次的事件不会再来一遍，所以开关被删掉时除了每个 ModelServing，每个 ServingGroup 也各入队一次（M4）。
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
| LWS controller | 搬到自己的包。它把 `status.observedGeneration` 不等于 `generation` 的 ModelServing 当成 `status` 陈旧。接管期间这个字段保持旧版本写的值（3.11）：升级前已经对账完的 ModelServing 不受影响，升级前就落后的要到接管做完才追上 | 接线之前 |
| ControllerRevision 的清理 | 保留的集合换成：T、B、各组的 `spec` 引用的、各组的 `status.referencedRevisions`。只在静止时清：`Available` 为真、没有 partition（组一级和 Role 一级）、没有存量或者无主的成员 Pod、每个组都已观测并且新鲜（`referencedRevisions` 才可信）。静止的每一轮都从缓存比一遍，有要删的才发请求，所以被打断的清理下一轮接着做（今天只在 `status` 的版本变化的那一轮试一次）。不再 LIST 命名空间里全部的 Pod。`spec.revisionHistoryLimit` 今天没有代码读它，这一次也不实现 | ModelServing controller |
| 协调的基线 | 照旧写 ControllerRevision 上的注解（回退要它），另经 `spec.roles[].baselineReplicas` 下发 | ModelServing controller |
| 事件 | 除了两个只在组上的，都记在 ModelServing 上。Role 状态事件按实例的就绪报，进入一个状态时报一次。全表见 7.1 | — |
| plugin 钩子 | 五个钩子各由谁调、调几次、失败时怎样，见 7.2 | — |
| PodGroup | 逐 Role 滚动的组按各实例还在跑的最小布局生成，见 7.3 | ServingGroup controller |
| 目标版本记不下来时 | 只做副本数的减少，见 7.4 | ModelServing controller |
| 调试端口 | `--debug-port` 和路径 `/debug/modelserving/cache` 留着，内容换成从 ServingGroup 的 lister 生成的 JSON | 接线 |
| `--workers` | 两个 controller 各起这么多个 worker | 接线 |
| `--leader-elect=false` | 仍然可以关（本地调试）。关了之后独占只剩 X1 | 接线 |
| chart 的 `replicas` | 默认仍然是 1。只有一个副本时失主退出意味着这个 Pod 重启的几秒里没有进程应答 webhook；文档里建议两个副本 | 文档 |
| `spec.bootstrapAccelerateStrategy` | 今天只有 API，没有代码读它。不动。以后实现时它管的是一个组里实例创建的次序，属于 ServingGroup controller，要经 `spec` 下发 | — |
| 名字长度的校验 | 不收紧（校验 webhook 对每次更新重新校验整个对象，收紧会拦住存量） | — |
| 用户文档、CRD 参考、examples | 新增 ServingGroup 一节；recovery 的行为变化；升级和回退的步骤 | 周边里程碑 |

### 7.1 事件

recorder 的 component 仍然是 `modelserving-controller`。

| reason | 类型 | 记在 | 谁发 | 什么时候 |
| --- | --- | --- | --- | --- |
| `ServingGroupCreated` | Normal | ModelServing | ModelServing controller | 建出一个组对象。message 写组名、版本和原因：`regular`、`surge`、`recover` |
| `ServingGroupDeleted` | Normal | ModelServing | ModelServing controller | 删掉一个组对象。原因：`scale-down`、`replace`、`replace-notready`、`failed`、`reclaim-surge`、`memberless-out-of-range`、`empty-out-of-range` |
| `OrdinalOccupied` | Warning | ModelServing | ModelServing controller | 要建的组的名字被不归这个 ModelServing 管的对象占着 |
| `PodNotTakenOver` | Warning | ModelServing | ModelServing controller | 接管没做完的每一轮，对每个 owner 是这个 ModelServing 而没有成员身份的 Pod（6.4） |
| `RolledBack` | Normal | ModelServing | ModelServing controller | 这个 ModelServing 交还完（6.8） |
| 协调的 condition 的 reason：`DependencyNotReady`、`MaxSkewLimitReached`、`OldVersionDependencyPresent`、`IncompatiblePartitions`（Warning）；`ProgressAvailable`、`RolloutComplete`（Normal） | 见左 | ModelServing | ModelServing controller | `CoordinatedRoleRolloutBlocked` 随一次 `status` 写变了。与上游相同 |
| `RevisionUnresolved` | Warning | ModelServing | 两个 controller | ServingGroup controller：组的版本读不出来（整组替换是 `spec.revision`，逐 Role 滚动是 `spec.targetRevision`），message 写组名；同一个组、同一个版本在一个进程里只报一次，读得出来之后重新计。ModelServing controller：当前模板的版本记不下来（7.4） |
| `RoleCreating`、`RoleRunning`、`RoleDeleting` | Normal | ModelServing | ServingGroup controller | 一个 Role 实例进入这个状态（表后的说明） |
| `InPlaceUpdateStarted` | Normal | ModelServing | ServingGroup controller | 这一轮给一个单元的 Pod 打了补丁 |
| `InPlaceUpdateCompleted` | Normal | ModelServing | ServingGroup controller | 一个单元的标签改完。旧版本的组里各自做完的 Pod 不报 |
| `InPlaceUpdateFallback` | Normal | ModelServing | 两个 controller | 一个组或者一个 Role 实例本该原地更新，改成重建。ServingGroup controller 在写下这个决定的那一轮报；ModelServing controller 在计划里为此删组时报 |
| `InPlaceUpdateTimeout` | Warning | ModelServing | ServingGroup controller | 一次原地更新过了期限。同一次更新在一个进程里只报一次 |
| `PluginError` | Warning | ModelServing | ServingGroup controller | `OnPodCreate` 拒绝了一个要建的 Pod。message 是钩子的错误 |
| `OwnerNotFound` | Warning | ServingGroup | ServingGroup controller | 组没有 ModelServing 作 controller，或者名字不是 `<ModelServing>-<序号>`。这样的组的每一轮都报 |
| `StatusNotPersisted` | Warning | ServingGroup | ServingGroup controller | 写回校验不过（3.7） |
| `RollbackMode` | Warning | 开关这个 ConfigMap | manager | 进程看到开关（6.8） |
| `WaitingForPredecessor` | Normal | controller-manager 自己的 Pod | 启动自查 | 在等旧版本的 Pod（6.3 的 X1） |

**Role 状态事件按就绪报。**message 与上游相同：`Role <role>/<role-id> in ServingGroup <组> is now Creating`（或者 `Running`、`Deleting`）。ServingGroup controller 每一轮写完 `status` 之后给每个实例定一个状态：

| 状态 | 条件 |
| --- | --- |
| Deleting | 账本是 Draining |
| Running | 账本有这一项，布局读得出来，实例已建成，每个成员都就绪 |
| Creating | 其余：账本有这一项，或者有成员，或者这一轮要为它建成员 |

状态与上一次报的不同，就报一次。几点：

- 实例有 Pod 不再就绪时回到 Creating，都就绪时回到 Running。原地更新途中的 Pod 不算就绪，所以实例先报 `RoleCreating`、更新完再报 `RoleRunning`；上游在 Pod 的就绪翻转时也是这样。
- 上一次报的是什么只记在进程里，不在对象上。新进程没有记录：它看到的已经在跑的实例不报 `RoleRunning`，没就绪的实例报一次 `RoleCreating`。上游重启之后把每个实例重报一遍。
- 什么都不存在、这一轮也什么都不建的实例不报，比如 PodGroup 同步不上的那几轮。
- 第一版在账本的转换上报：实例的 Pod 都存在（Established）就报 `RoleRunning`，比就绪早，message 却写着「is now Running」；缓存显示出 Pod 之前的每一轮都重报 `RoleCreating`（第二轮评审的发现 R2-S5，回归复核的 U-2）。

**两个 Warning 恢复，一个没有了。**`PluginError` 和 `RevisionUnresolved` 上游记在 ModelServing 上。第一版没有发：失败只在组的 condition 上（`ReconcileFailed`，`Ready` 的 `TemplateUnresolved`），只看 ModelServing 事件的用户看不到（U-3）。现在照旧记在 ModelServing 上，组的 condition 也留着。`DeletionIdentityChanged` 不再有：上游在删除途中发现同名的 Pod 换了一个时报它；现在每个删除带 UID 的前置条件，换过的 Pod 删不到，没有「身份变了」这个分支。

**`OwnerNotFound` 只报给会一直留着的组。**没有 owner 的组、名字不是 `<ModelServing>-<序号>` 的组没有人管，要人来处理。所属的 ModelServing 正在删除或者已经不在的组不报：垃圾回收器会删掉它。第一版对后一种也报，删一个 ModelServing 时一部分组上出现 Warning（U-6，e2e 的清理阶段看到过）。

### 7.2 plugin 钩子

| 钩子 | 谁调，什么时候 | 调几次 | 失败时 |
| --- | --- | --- | --- |
| `OnModelServingSync` | 两个 controller，各在一轮的任何写之前（1.2） | ModelServing 的每一轮一次，每个组的每一轮一次：次数随组数走 | 1.2 |
| `OnPodCreate` | ServingGroup controller，建每个 Pod 之前，用这个 Pod 的版本的 plugin 链和模板 | 每次创建一次，重试时再调 | Pod 不建，这一轮出错（`ReconcileFailed`），在 ModelServing 上发 `PluginError` |
| `OnPodReady` | ServingGroup controller，成员 Pod 就绪之后，用这个 Pod 的版本的 plugin 链 | 每个 Pod（按 UID）每个进程一次。调过没有只记在进程里：换了进程再调一次，钩子要经得起 | 这一轮出错，下一轮再调 |
| `OnRoleSync` | ServingGroup controller，每一轮创建之前，对期望之内、布局读得出来、不在清空中的每个实例 | 每一轮，要幂等。只做删除的轮次不调 | 这一轮出错，后面的创建和补丁不发 |
| `OnRoleDelete`，清空一个实例 | ServingGroup controller，这个实例的 Pod 都消失之后、账本改写之前（E3） | 至少一次，到成功为止 | 账本停在 Draining，组报 `DrainBlocked`，这一轮返回错误。挡住的是这一个实例：它不重建。别的实例、别的组、ModelServing 的那一轮不受影响。上游是整个 ModelServing 的 reconcile 失败重试 |
| `OnRoleDelete`，整个组被删 | ServingGroup controller，组在终止中、缓存里已经没有它的 Pod 时，对账本里的每个实例 | 尽力：对象可能先没了，一次也调不到；组还在的每一轮都调，也可能不止一次 | 只记日志 |

走这条路的是每一次整组删除：`ServingGroupRollingUpdate` 的替换、缩容、`ServingGroupRecreate` 的恢复、surge 组的回收，以及用户自己删掉一个 ServingGroup。这条路上钩子失败不重试（runner 的 `plugin-retry` RUN-536 在上游上要求重试，这里是声明过的出入），失败不在任何对象上留下 condition 或事件，只有 controller 的一行日志（`--v=2`，chart 的默认值）。所以 plugin 的 `OnRoleDelete` 要释放的东西里，凡是不能以组为 owner 的（集群之外的登记、别的命名空间里的对象、集群级的对象、以 ModelServing 为 owner 的对象），在这条路上没有清理的保证。内置的 plugin 没有这样的东西：只有 `headless-service` 实现了 `OnRoleDelete`，它的 Service 以组为 owner。要「至少一次」就得让对象活到钩子成功，也就是给每个 ServingGroup 加 controller 的 finalizer；回退的交还要先摘它，controller 不在时 ModelServing 和命名空间删不掉，名字一直被占着，所以没有这样做。

整个组被删时调 `OnRoleDelete` 有四个条件，缺一个就不调：组的 `finalizers` 里有 `foregroundDeletion`（成员随它一起删）；回退的开关不在；所属的 ModelServing 还在、不在删除中、注解不是 `RollingBack`；账本不是空的。为了交出成员而删的组（orphan 方式，回退的交还就是这样）不调：headless Service 的钩子会把刚交还的 Service 删掉（R2-S3）。

`HookRequest` 带两个字段：`Owner` 是实例所在的组，`Generation` 是实例的代际号。plugin 为一个实例建的资源要以 `Owner` 为 controller，记下 `Generation`；`OnRoleDelete` 只删代际号不大于请求里那个的。这样整个组被删时资源随垃圾回收消失，不依赖钩子；一次删除的决定被重放时，不会删到中间重建出来的实例的资源。

### 7.3 PodGroup

一个 PodGroup 的一个子组，对一种 Role 的所有实例只有一个大小。`RoleRollingUpdate` 下，同一种 Role 的实例各按自己被建出来时的版本的布局跑；滚动改了 `workerReplicas`，新旧实例的大小就不同。

`sgcontroller.podGroupView`（上游的 `workloadForPodGroup`）：逐 Role 滚动的组，不论有没有 roleCoordination，PodGroup 里每种 Role 取各实例还在跑的最小布局，就是 `workerReplicas` 最小的那个模板。已有的、不在清空中的实例按它的 Pod 标签上的版本算，还没有建出来的实例按 Role 计划给它的版本算。受 Role partition 保护的实例也在内，所以 partition 停点上 PodGroup 一直是小的那个布局。副本数和 `gangPolicy` 里显式写的下限取当前的。所有实例都到了目标的布局，PodGroup 也就是目标的布局。实例是否就绪照旧按它自己版本的完整布局判。整组替换的组不走这一段，按 `spec.revision` 的布局生成。

某个实例的版本读不出来时布局定不下来：这一轮按 PodGroup 同步失败处理，PodGroup 不动，这一轮只做删除，不建、不打补丁，报 `ReconcileFailed`，重试。上游在这种情形下只跳过这个组的 PodGroup 更新，别的照做。这里更严：PodGroup 不可信时不往里建 Pod，并且看得见。

roleCoordination 另有一条，与变更前相同：配了依赖、调度器是 volcano 时，等依赖的 Role 这一轮不建目标版本的实例，PodGroup 对这个 Role 保持它现在公布的下限，不让 gang 去等不会被建出来的实例。`gangPolicy` 里显式写的下限不压。

第一版只给 roleCoordination 搬了最小布局这一条，独立滚动的组一上来就按目标生成 PodGroup。目标给 Role 加 worker 时，gang 向每个实例要两个 Pod，而没有换的实例只有一个：gang 永远凑不齐，Volcano 不绑定替换出来的 Pod，滚动停在少一个实例的地方（runner 的发现 F4，回归复核的 R-1）。

### 7.4 目标版本记不下来时

ModelServing controller 每一轮先把当前模板记成 ControllerRevision，由此得到目标版本。这次写被拒绝时没有目标版本，不能建、也不能换任何东西。副本数的减少不需要模板，照做。这是上游的 `scaleDownOnRevisionError`，搬成 `mscontroller` 里一个受限的轮次（决策 I-56）：

受限的一轮（`mscontroller/scalein.go`）用上一次记下来的目标（`status.updateRevision`）顶替缺的那个来做计划，计划里只留「把组数减下去」的删除，别的动作都丢掉：

- 组：多于 `replicas` 的删到 `replicas`。`ServingGroupRollingUpdate`、不是原地更新、`replicas` 大于 0 时另留出配置的 `maxSurge`：目标读不出来，不能断定 surge 的组已经没有用。先删正式的组，带 surge 标记的排在后面；`replicas` 为 0 时全部删掉，带标记的也删。删除之前照常直读、在读到的东西上重新计划。
- Role 实例：没有配 `roleCoordination` 的 ModelServing，留下的每个组里 Role 的副本数调小的部分下发（只改这个数、与之对应的 `expectedPods` 和 `sourceGeneration`），partition 之下的组也一样：副本数不属于版本，正常的一轮也把当前的数下发给每个组。调大的、新增或去掉的 Role 不下发。组自己的版本读不出来时不动它，在错误里点名。配了 `roleCoordination` 的不动：多出来的实例可能是协调滚动的 surge，不能绕过协调的检查。
- 不建任何东西，不替换任何东西，不改任何组的版本，不写 ModelServing 的 `status`（`observedGeneration` 留在后面）。
- 在 ModelServing 上发 Warning 事件 `RevisionUnresolved`，每个 generation 每个进程一次。这一轮总是按出错返回（版本的错误连同缩容中出的错），到版本记得下来为止固定每 15 秒重试一次，不用队列那个越来越长的退避：版本记不下来的那几秒里，ModelServing 和各组的每个事件都会触发一轮、每一轮都失败，退避的延迟十几次之后就是十几分钟，而「现在可以写了」没有任何事件来告诉 controller。Kind 上观察到过：准入策略撤掉 5 分钟之后滚动还没有开始。
- 不做的情形：从来没有记下过目标（`status.updateRevision` 为空或读不出来）；存量还没有接管完（接管注解不是 `Done`）；`maxSurge` 或 partition 解析不出来。

与上游的差别：上游在 Role 逐个滚动时给 Role 留 `replicas + maxSurge` 个实例，这里下发确切的数，组在自己的滚动还需要时留着带 surge 标记的实例；上游在 `maxSurge` 写错时仍然下调 Role，这里整轮不做；「算出来的名字上已有一个内容不同的 ControllerRevision」在上游是错误，这里按碰撞换名记下来，是正常的一轮。

第一版在版本记不下来时整轮直接返回，缩容也不做。上游有专门的测试断言缩容照做，提案阶段的分析已经指出设计里缺这一条（旧代码覆盖表的 MB-1、GAP-9）。测试在 `mscontroller/scalein_test.go`，18 个，逐条对应上面的规则，也断言这期间没有 `create servinggroups`、没有 `create pods`、没有改版本的写。

## 8. 测试和验收

| 层 | 内容 | 验收 |
| --- | --- | --- |
| 纯函数的单测 | 成员、摘要、成员分类；账本的转换（E0–E9 每一格一行）；`status` 的计算；组一级的计划的每一步。第二轮评审之后加的：`TestPlanInPlace` 里旧版本的组的四个用例，以及「与目标渲染相同的 Pod」和「版本读不出来的 Pod」的五个新用例；`TestComputeStatusConditionLifecycle` 里 `RoleRollingUpdate` 下去掉 `RecreateRequired` 的一段 | 表驱动 |
| 对照测试 | 组一级的计划对 008 的参考模型导出的向量：契约的 43 段脚本、runner 组合用例的停点、`normal` 套件、穷举模型的抽样 | 每个向量的动作序列逐项相同 |
| controller 的单测 | 跑在 4.5 的替身上。每一条 E、S 的边；每个写的停点；两个 controller 实例交错。第二轮评审之后加的，ServingGroup controller：`TestAGroupThatIsNotAtTheTargetLeavesItsPodsAlone`、`TestPodGroupOfRolledRolesUsesTheSmallestLayout`、`TestTheCopyOfTheSurgeMarkFollowsTheGroup`、`TestNoMemberIsCreatedBesidePodsOfAnEarlierObject`、`TestRoleStatesFollowTheReadinessOfThePods`、`TestNoRoleIsReportedAsCreatedWhileNothingIsCreated`、`TestOnlyAGroupThatStaysWithoutAnOwnerIsReported`、`TestTheDeleteHooksAreForAGroupThatIsDeletedWithItsMembers`、`TestTheCommitmentSurvivesARoundThatCannotBePlanned`、`TestABlockingHookSaysWhenToAskAgain`、`TestWarningsAreRecordedOnTheModelServing`；ModelServing controller：`TestTheRolloutStrategyIsChanged` 的两个新子用例、`TestRollbackIsAbandonedWhenTheSwitchIsRemovedWhileItWaits`（带 `-race`）、`TestTheBaselineIsNotReadFromPodsOfAnotherModelServing`、`TestWorkloadsOfAnEarlierVersionAreTakenOver` 里接管期间 condition 和 `observedGeneration` 不动的断言。哪个测试钉哪条规则见 9A | 每个停点续做之后终局相同。接管的各个写之后的停点没有单测，只有 Kind 上的杀进程（第 10 章） |
| P2 的回归 | `release-1.1` 上的样例的哈希 | 钉住 |
| e2e（Kind） | `test/e2e/controller-manager` 现有的全部；新增：ServingGroup 的生命周期和 `status`（`TestServingGroupLifecycle`）、三种方式删组（`TestServingGroupDeletion`）、整组恢复（`TestServingGroupRecreateReplacesGroupObject`）、换主之后续做（`TestServingGroupControllerFailoverDuringRollingUpdate`）、`RoleRunning` 跟着就绪走（`TestModelServingRoleRunningEventFollowsReadiness`） | 现有的断言不放宽。要改的是按 owner 认对象的四处谓词（`IsControlledBy(pod, ms)`、Service 的 owner UID 等于 ModelServing 的 UID）：改成沿 ServingGroup 找到 ModelServing，断言的内容不变。另外，用 `observedGeneration` 推进当作「就绪的变化已经处理」的同步手法（Role 缩容的次序、协调滚动的停点）换成等组的 `status` |
| runner 的行为规范测试 | 变更前后同一个集合，逐例对比 | PASS 变 FAIL 的要么对应已批准的条款，要么是要修的回归 |
| 容灾（Kind） | 两个副本，在每一类写之后杀主；controller 不在时删 Pod | 续做；按范围恢复 |
| 升级演练（Kind） | 两个源版本；6.5 的各行；接管每一步之后杀进程；不装 CRD 直接升；装了 CRD 直接升 | 6.7 的第 5 步。另外，接管期间每个 ModelServing 的 `status` 不动（3.11）：演练脚本为此加了一项检查 |
| 回退演练（Kind） | 升级、回退、旧版本接着跑、再升级 | Pod 的 UID 和重启次数全程不变 |
| 量一次 | 20 个组、每组 4 个 Pod：创建、一次滚动、一次排空节点各有多少次 API 调用和 `status` 写 | 记下来，不设门槛 |

## 9. 提交计划

每个提交能编译，所动的包的单测通过。手写的代码每个提交尽量不超过 600 行；生成的代码、纯删除各自成提交。功能分支只由协调者提交；e2e、补测试、7.4 的缩容轮次、文档这几块由子代理在各自的 detached worktree 里提交，协调者核对之后 cherry-pick（决策 I-58）。

| 段 | 提交 |
| --- | --- |
| A 铺垫 | 模板类型改名；ServingGroup 的类型；生成的代码；chart 的 CRD 和 RBAC；`utils` 渲染 Pod 带 owner 和代际号；`podgroupmanager` 去掉 `datastore`；plugin 的 `HookRequest`；驱逐 webhook 认两种归属；校验 webhook 放行 `spec` 没变的更新；测试替身 |
| B 纯函数 | 成员和摘要；成员分类；账本；`status`；组一级的分类、计划、向量、对照测试；版本的三值；`ModelServing.status`；协调的纯函数（搬）；Role 一级的计划；版本的解析和工作负载视图 |
| C ServingGroup controller | 骨架；PodGroup 和 Service；创建和先写后建；恢复；Role 的扩缩和滚动；协调；原地更新；收养 |
| D ModelServing controller 和切换 | 骨架和版本；计划、直读重算、执行；`status` 和清理；接管（T1、T2′、T3、T7）；搬 LWS controller；**接线**（两个 controller、选主默认开、失主退出、QPS、X1、X2）；删旧包（11 个提交：测试六个，源码，`podgroupmanager`、`datastore`、`plugins`、`utils` 里只有旧 controller 用的部分各一个，见第 5 章）；e2e |
| E 其余 | 升级前检查；用户文档；runner（契约 2.14、适配、新用例）；回退；X3 |

## 9A. 独立评审之后补上的规则

### 9A.1 第一轮

三位没有参与编码的评审者（ServingGroup controller；ModelServing controller 和接线；API、chart、webhook）读的是工作分支 `62367667`。下面是由评审带来、前面各章没有写或者写得不对的规则；每一条都有一个去掉修正就失败的测试。发现的全文和处置在任务 010 的 `subagents/code-review-*.md`。

| 规则 | 没有它会怎样 | 测试 |
| --- | --- | --- |
| 一轮的出错在动手之后定一次；重复的出错不写 `status`（3.7） | 建不出 Pod 的组以 apiserver 应答的速度空转，每轮两次 `status` 写，占掉两个 controller 共用的限流额度 | `TestARepeatedFailureWritesNothing` |
| `OnRoleDelete` 失败的轮次返回错误 | 实例停在 Draining、没有 Pod，没有事件带来下一轮 | `TestAFailingDeleteHookIsRetried` |
| 没有 PodGroup 的轮次照样删；已承诺的成员数留在 `pods` 里 | 失败的组清不空、换不掉；缓存落后时组报成没有成员，被换了版本 | `TestAFailedGroupIsDrainedWithoutItsPodGroup`、`TestTheCommitmentSurvivesARoundWithoutThePodGroup` |
| 「清空完了」（E3）要直读；已存在的同名 Pod 只有带着成员标签、并且是这一代的才算数 | 新主的缓存还没有 Pod 时，下一代被提交而上一代的 Pod 还在，被当成新一代的成员；丢了标签的成员让实例永远停在 Creating，而且没有任何报告 | `TestADrainDoesNotEndOnAStaleCache`、`TestAMemberWithoutItsLabelsIsReported` |
| 组的名字必须是 `<ModelServing>-<序号>` | 手工建的、owner 指向某个 ModelServing 的组会得到 Pod，而 ModelServing 看不见它 | `TestAGroupUnderAnotherNameIsLeftAlone` |
| `OnModelServingSync` 两个 controller 都调；ServingGroup controller 里阻塞性的错误让这一轮只写 `status` | 钩子说「现在不行」时已有的组照样建、删 Pod | `TestABlockingModelServingHookStopsTheGroup` |
| `RoleCreating`、`RoleRunning`、`RoleDeleting` 事件照旧发在 ModelServing 上。什么时候发，第二轮改过（9A.2） | 依赖这些事件的用户和 e2e 看不到 Role 的状态变化 | `TestRoleInstancePhasesAreReported` |
| 组不在了，进程里记着的东西随 informer 的删除事件丢掉；ModelServing 同理。删除事件来的时候正在跑的那一轮，第二轮补上（9A.2） | 后台删除的对象没有哪一轮看得到它在终止，记着的东西留到进程结束 | `TestAGroupThatIsGoneIsForgotten` |
| PodGroup 的 informer 和 ServingGroup controller 不论谁先出现都接上，只接一次 | CRD 启动时就在的集群里 PodGroup 的事件没有人听，被删掉的 PodGroup 不会被补回来（对基线是回归） | `TestPodGroupWatch` |
| 直读里按名字补读「缓存里有、标签列表里没有」的自己的组；组的事件按名字也能找到 ModelServing | 组丢了名字标签之后，每一轮都去建它、得到 AlreadyExists、这一轮别的写全部丢掉 | `TestAGroupWithoutItsLabelIsStillCounted`、`TestServingGroupEventsReachTheModelServing` |
| 「这个组多久没报告了」每一轮都维护 | 报告过的组留着很早以前的时刻，下一次落后时立刻被报成 `GroupNotObserved` | `TestRolloutBlocked` 的最后一个子测试 |
| 接管原地更新到一半的组：组的版本取 Pod 正在更新到的那个，不取标签 | 组按标签上的旧版本建出来，ServingGroup controller 以为更新被撤回，要求重建，升级把 Pod 换掉 | `TestAnInPlaceUpdateUnderWayIsTakenOver` |
| `RoleRollingUpdate` 改成 `ServingGroupRollingUpdate`：组的 `spec.revision` 取它报告的 `status.revision`；还没有报告的组先不下发。Pod 跑着两个版本的组，这一轮定的是「算作基线版本，按额度替换」，第二轮改成基线与目标不同才这样做（9A.2，2.1） | 组被下发成空版本：永远不可替换，`updatedReplicas` 永远是 0，丢了的 Role 实例建不回来 | `TestTheRolloutStrategyIsChanged` |
| 接管不等没有成员身份的存量 Pod（6.4） | 接管永远做不完，没有提示 | `TestAPodThatIsNoMemberDoesNotHoldUpTheTakeOver` |
| `status.currentRevision` 为空又没有组对象时，基线取存量各组里出现最多的非目标版本。哪些 Pod 算数，第二轮收紧了（9A.2） | 旧版本在滚动途中、写 `status` 之前被停掉的话，partition 之下新建的组用了新版本 | `TestTheBaselineOfAnEarlierVersionIsReadFromItsPods` |
| 组上的 `sourceGeneration` 比 apiserver 上的 ModelServing 还大时照样下发（先确认缓存里的 ModelServing 是最新的） | 这个数被别人改大之后，组永远不再被下发 | `TestASourceGenerationFromNowhereDoesNotStopTheHandDown` |
| 回退的交还等各组正在跑的轮次结束；离开回退模式时每个组入队一次（6.8）。等完之后还要再看一次开关，第二轮补上（9A.2） | Pod 在组被确认「名下没有东西」之后才被收养或者建出来，随组丢掉 owner 或者被回收；回退期间丢的 Pod 没有人补 | `TestRollbackWaitsForARoundInFlight`、`TestEnqueueAll` |
| 进程给自己的 Pod 补协议标签（6.3） | 不用 chart 部署的多副本互相当成旧版本，谁也不启动 | `TestLabelPod` |
| `ServingGroupSpec.gangPolicy` 用自己的类型（2.1） | ModelServing 上分两步改 `minRoleReplicas` 之后，组的下发永远被拒绝 | CRD 里没有那条规则；`TestTheGangPolicyKeepsWhetherItIsSet` |

评审确认没有问题、因此没有改的部分，和评审没能确定的部分，也记在同一组文件里。

### 9A.2 第二轮

第一轮的修正和 roleCoordination 的搬迁做完之后，两位没有写过被评审代码的评审者（ModelServing controller、manager、接线；ServingGroup controller、纯函数）读的是分支头 `236c60e9`。同一个快照另有三处来源：行为和回归的复核（`subagents/behavior-regression-recheck.md`，编号 R-n、U-n）、runner 的行为规范测试在 Kind 上的发现（F2、F4、F5）、删旧代码时逐个测试做的覆盖表（MB-n）。下面是它们带来的规则，「来源」一栏写发现的编号和决策的编号。发现的全文和处置在 `subagents/code-review-round2.md`，决策在 `decisions.md` 的 I-43 到 I-59。

| 规则 | 没有它会怎样 | 来源 | 测试 |
| --- | --- | --- | --- |
| `spec.targetRevision` 在两种滚动方式下都下发。整组替换的组，`spec.revision` 与它不同就是旧版本的组：不发起原地更新，不置 `RecreateRequired`，只把打过补丁的 Pod 各自做完（2.1） | 滚动方式从 `RoleRollingUpdate` 改成 `ServingGroupRollingUpdate` 之后，跑着两个版本的组把新版本的 Pod 改回去：InPlace 下所有的组同时打补丁，不看预算和 partition；Recreate 下置 `RecreateRequired`，不受 partition 约束 | R2-M1；I-43 | `sgcontroller.TestAGroupThatIsNotAtTheTargetLeavesItsPodsAlone`；`servinggroup.TestPlanInPlace` 的四个 held 用例；`mscontroller.TestTheRolloutStrategyIsChanged` 的两个新子用例（Recreate 和 InPlace 各一个） |
| 切换滚动方式时，Pod 跑着两个版本的组：基线与目标不同才按基线下发，否则不下发（2.1） | 基线就是目标时，这个组被当成目标版本，把所有的 Pod 一起带到目标 | R2-M1；I-43 | 上一行的两个子用例钉的是基线与目标不同的一半：组按基线下发，不要求重建，在预算之内被替换，partition 之下的留着。基线与目标相同时不下发，没有单独的用例 |
| 原地更新：没有在途的更新、也没有一个 Pod 的 Role 与目标渲染不同，就什么都不做，不分更新方式。与目标渲染相同而标签是旧版本的 Pod，在别的 Pod 更新时跟着走一个不带镜像的补丁，不按整份模板比（第 5 章） | InPlace 生效时，逐 Role 滚动留下的旧标签让整组被重建（别的 Role 改过镜像以外的字段），或者让所有的组同时做一次空更新、短暂不 `Ready`（只改过镜像） | R2-M2、R2-S1；I-44 | `TestPlanInPlace` 的五个新用例 |
| 第一个补丁之前因为版本读不出来而等待的单元不算在更新（3.4） | Recreate 下有 Pod 的版本读不出来，组被标成在更新，永远不 `Ready`，而没有补丁打过、也不会打 | R2-S2；I-44 | 同上 |
| 逐 Role 滚动的组，不论有没有 roleCoordination，PodGroup 按各实例还在跑的最小布局生成；有实例的版本读不出来，这一轮按 PodGroup 同步失败处理（7.3） | 目标给 Role 加 worker 时 gang 永远凑不齐，替换出来的 Pod 不被绑定，滚动停住 | runner F4、R-1、MB-2；I-45 | `TestPodGroupOfRolledRolesUsesTheSmallestLayout` |
| 组失去 surge 标记之后的下一轮，摘掉 Pod 上的副本（2.3） | 留着的副本让日后丢了对象的组被当成 surge 组找回；读这个注解的工具把在服务的 Pod 当成临时的 | runner F2、R-2、MB-4；I-46 | `TestTheCopyOfTheSurgeMarkFollowsTheGroup` |
| 同名的上一个组对象的 Pod 还在时，新对象一个成员也不建，报 `NameOccupied`，`pods` 记 0（3.6） | Background 方式删组之后，哪个名字空出来就建哪个，新旧两个对象的 Pod 并存 | runner F5，008 的 NT4；I-47 | `TestNoMemberIsCreatedBesidePodsOfAnEarlierObject` |
| Role 状态事件按实例的就绪报，进入一个状态时报一次；上一次报的是什么记在进程里，新进程不重报已经在跑的实例（7.1） | Pod 一存在就报 `RoleRunning`，比就绪早；缓存显示出 Pod 之前每一轮重报 `RoleCreating` | R2-S5、U-2；I-48 | `TestRoleStatesFollowTheReadinessOfThePods`、`TestNoRoleIsReportedAsCreatedWhileNothingIsCreated`；e2e `TestModelServingRoleRunningEventFollowsReadiness` |
| `PluginError`、`RevisionUnresolved` 记在 ModelServing 上（7.1） | 失败只在组的 condition 上，只看 ModelServing 事件的用户看不到 | U-3、MB-3；I-48 | `TestWarningsAreRecordedOnTheModelServing` |
| `OwnerNotFound` 只报给会一直留着的组（7.1） | 删一个 ModelServing 时，一部分组上出现 Warning | U-6；I-48 | `TestOnlyAGroupThatStaysWithoutAnOwnerIsReported` |
| 整个组被删时的 `OnRoleDelete` 只对前台删除的组调，回退中不调（7.2） | 交还时 headless Service 的钩子把刚交还的 Service 删掉 | R2-S3；I-49 | `TestTheDeleteHooksAreForAGroupThatIsDeletedWithItsMembers` |
| 做不出计划的轮次保留已经承诺的成员数（3.7） | `status.pods` 写成 0，ModelServing controller 把这个组当成没有成员，不占预算地改它的版本 | R2-S4；I-50 | `TestTheCommitmentSurvivesARoundThatCannotBePlanned` |
| 阻塞的 `OnModelServingSync` 带 `RequeueAfter` 时，组这一侧到点再问。ModelServing 这一侧不变（1.2） | 组走队列的退避，钩子放行之后很久才动 | R2-S6；I-50 | `TestABlockingHookSaysWhenToAskAgain` |
| `rollBack` 拿到写锁之后再看一次开关，并且持锁到这一次交还返回（6.8） | 开关在它等锁的时候被摘掉，交还和放行的组内轮次同时在跑 | R2-M3；I-51 | `TestRollbackIsAbandonedWhenTheSwitchIsRemovedWhileItWaits` |
| 存量的基线只数会被接管的 Pod，只认这个 ModelServing 读得出来的版本（1.3 的 V11） | 同名的前一个 ModelServing 留下的 Pod、正在删除的 Pod 决定了基线，partition 之下按它建出来的组渲染不出来，永远不 `Ready` | R2-M4；I-52 | `TestTheBaselineIsNotReadFromPodsOfAnotherModelServing` |
| 接管期间 ModelServing 的 `status` 不动：计数、三个 condition、协调的 condition、`observedGeneration`（3.11） | `Available` 在每个 ModelServing 上翻成假再翻回来；`observedGeneration` 推进而协调的 condition 是旧的，校验 webhook 放过不该放的修改 | U-1、R2-M5；I-53 | `TestWorkloadsOfAnEarlierVersionAreTakenOver` |
| `RoleRollingUpdate` 下的组不带 `RecreateRequired`，带着的去掉（3.4） | 这个组永远不算已更新，ModelServing 永远是 `UpdateInProgress`；滚动方式改回去时它不管 Pod 跑着什么都被替换 | R2-M6；I-54 | `TestComputeStatusConditionLifecycle` |
| 一轮结束时它的组已经不在（或者换了 UID），就丢掉进程里记着的东西 | 删除事件来的时候正在跑的一轮把记忆加回去，留到进程结束 | R2-S8 | 没有单独的测试 |
| 目标版本记不下来时仍然执行副本数的减少，不建、不换（7.4） | 用户调小 `replicas` 而 ControllerRevision 写不进去时，缩容不发生；上游发生 | MB-1；I-56 | 随实现的提交补上 |

没有改代码的：

| 项 | 处置 |
| --- | --- |
| roleCoordination 的两种停住而 condition 不说的状态（R2-S7） | 规则是从上游逐字搬来的，上游也是这样。原样保留，列进第 10 章（I-55） |
| 只做删除的轮次里，这一轮自己发起的排空照做（评审对 S4 的备注） | 接受，列进第 10 章（I-59） |
| Deployment 的 Pod 模板没有协议标签时，新副本出现会让主退出等待（评审对接线的备注） | X1 的直接后果。标签是必需的，写进升级指南和第 10 章（I-59） |
| `observedGeneration` 的含义、plugin 钩子的四点、Role 一级沿用组一级的规则、九个小项（U-4、U-5、U-7、U-8） | 设计的直接后果，或者上游就有而没有写明。写进升级指南和 1.4（I-59） |
| 上游就已经没有人引用的四个 `utils` 函数 | 不删（I-57） |

### 9A.3 行为规范测试之后

runner 在 `258927f7` 的构建上跑了 220 例，与变更前的基线逐例对比，查出下面几处。前三处是回归，都修了；第四处是加固；第五处是组一级的规则 G3b 与契约不同，当时没有改，列给用户之后按用户的选择改了（第五行）。最后一行是 `normal` 套件查出来的另一处回归，也修了。

| 规则 | 没有它会怎样 | 来源 | 测试 |
| --- | --- | --- | --- |
| 「同名的上一个对象还留着 Pod 就不建成员」这个判断要在直读上做（3.5） | Pod 的删除事件没有到达缓存时（watch 丢事件），新的组永远停在 `NameOccupied` | runner F6（`lost-deletion` RUN-443…445）；I-63 | `sgcontroller.TestAPodOfAnEarlierObjectThatIsGoneDoesNotHoldTheGroup` |
| ModelServing（generation 比组的 `spec.sourceGeneration` 新）声明的某个 Role 的实例数比组被告知的少时，组不建这个 Role 的实例；取两个数里保守的一个，与恢复策略的取法相同（4.2） | 组的 `spec` 比 ModelServing 晚一步到。controller 停着的时候用户调小了副本数，新进程起来时组里还是旧的数，丢掉的实例被照旧的数建回来，随后又被删掉：要 3 个而一度有 5 个 | runner F10（`boundary-sparse` RUN-584、589，`history-sparse` RUN-498，基线上通过）；I-64 | `sgcontroller.TestNoInstanceIsCreatedFromACountTheModelServingHasLowered` |
| 同一轮里离开的同一个 Role 的实例，按序号从大到小删 | 上游和契约 2.2 都是降序；原先按升序发 DELETE | runner F7（`boundary-numeric` RUN-560）；I-65 | `sgcontroller.TestInstancesOfABatchLeaveInDescendingOrder` |
| ModelServing controller 的一轮在每个写之前看一眼缓存：ModelServing 的 generation 已经变了就结束这一轮，不写 `status`，下一轮在新的 `spec` 上计划 | 一轮是在读到的 ModelServing 上计划的，写是一个一个发的。中间用户改了模板，剩下的写照旧发出去：新的要求被接受几十毫秒之后，还按旧版本建出一个组，随后要再换掉。窗口缩到 informer 送达变更的那几毫秒，没有消除 | runner F8（`servinggroup-compound-v2` RUN-637，三次里失败两次；基线上也失败）；I-66 | `mscontroller.TestARoundStopsWritingWhenTheModelServingHasChanged` |
| surge 组只在多余的时候回收：另外有 N 个活着的组有不在终止中的 Pod（或者不要 Pod）；`status.pods`、这一轮刚建的组、等收养的组都不算；surge 组自己有没有成员不问。例外是没有余地（缺一个正式的组，而活着的组占满 N + S）。G3b，1.3 的 V12 | `N=3`、`maxSurge=1`、`maxUnavailable=1`，surge 组一直不就绪。最后一个旧组开始终止、它的替代还没建出来的时候，surge 组被删掉，新的 0 号组建出来之前只有两个组。可用的组数没有低于下限，但是 runner 的账本把它记成 `UNEXPECTED_TARGET_REPLACED`；上游在空出来的序号重建之前留着它。三种更窄的写法都不够，评审逐一指出：只对不就绪的 surge 组加「组数多于 N」，补位（G4）和回收在同一轮里，新的组对象建出来的那一轮 Pod 还没有，就绪的 surge 组在同一个位置上也一样被记违反；用「不是没有成员」（`status.pods` 不为 0）当「有成员」，替代的组刚写下要建 Pod、第一个 Pod 还没建出来时 surge 组就会走，Kind 上只差 5 毫秒，Pod 建不出来时必然走；让「自己没有成员的 surge 组」照旧回收（它不顶替任何位置），刚建出来的 surge 组也在其中：`replicas: 1`、`maxSurge: 1`、默认的 `maxUnavailable` 下，旧组和 surge 组在同一轮里一删一建，下一轮 surge 组还没有 Pod 就被删掉，Kind 上它的 Pod 晚到（配额、准入慢）时必然如此，旧组终止的那十秒里没有一个组有 Pod | runner F9（`grace-restart` RUN-431，基线上通过）；I-67、I-71、I-74、I-82；第三轮独立评审、它的复查和提案分析（任务 010 的 `subagents/i71-*.md`）| `grouplevel.TestPlanReclaimSurge`（从「is kept while the last old group is terminating」到「neither is a failed one that is not drained」的 22 例）；`servinggroup.TestPlanRolesOfAGroupThatIsRolledRoleByRole` 的 6 行；`sgcontroller.TestARoleSurgeInstanceStaysUntilTheLastPlaceHasAPod`；`mscontroller` 的三个 `TestTheSurgeGroupStays…`（两个 controller 一起跑：最后一个旧组慢慢终止、替代的 Pod 被拒绝创建、另一个组的 Pod 全在终止，各有就绪和不就绪的 surge 组两种）和 `TestTheSurgeGroupIsNotReclaimedBeforeItHasAPod`（`replicas: 1`，ModelServing controller 连跑两轮）。规则和两处取数的 20 处单点改动都被判错。`TestPlanAgainstModel` 的向量从改过的参考模型重新导出 |
| `RoleRollingUpdate` 下新建的组整个按目标建，Role 的 partition 之下的实例也是。ModelServing controller 新建组时写注解 `created-revision`，值是建它用的版本；给这个组下发别的目标时在同一次写里摘掉，什么都还没有建出来的组改成新的目标，不后加；注解只在 `RoleRollingUpdate` 下有。注解的值等于 `spec.targetRevision` 的组，Role 计划的基线取目标；协调滚动读的是同一个计划。组里有别的版本的 Pod 时不照注解办，基线照旧。2.3，1.3 的 V13 | 两个组，Role `f` 三个实例、`maxUnavailable` 是 1、partition 是 1，模板从 A 改成 B，各组停在 `f0` 是 A、`f1` 和 `f2` 是 B。这时把组数调成 3，或者组数和模板在同一次请求里改：新的组的 `f0` 按基线建成 A。上游整个新组都是 B。注解要是不摘：版本的身份是从模板算出来的，模板改回这个组出生时的样子，身份就是原来的那个，注解的值又等于目标，这个早已跑过基线的组又被当成新建的，它 partition 之下的实例丢了按目标建回来。每个组都带着出生时的版本，所以「滚完之后带着 partition 改回去」这一种经过里每个组都是这样。只算到组承诺 Pod 为止：新的组的 Pod 被拒绝的时候模板又变了，注解摘掉，等 Pod 建得出来，这个组 partition 之下的实例按基线建。`ServingGroupRollingUpdate` 下留着注解：没有成员的组整个按基线建出来，却说自己是按目标建的，策略换回来之后它丢了 partition 之下的实例按目标建，别的组按基线建 | runner F13（normal RUN-231、233、273，基线上通过）；I-77；后两种是 F13 的代码复审查出来的 | `servinggroup.TestPlanRolesOfAGroupThatWasCreatedAtTheTarget`，`TestPlanRolesRolledTogether` 里「a group created at the target」的三行和「an annotation that names another revision」一行；`sgcontroller` 的三个 `TestAGroupCreatedAtTheTarget…`（第一轮、丢了 partition 之下的实例、每个写之前停一次共 15 个停点）和 `TestCoordinatedGroupCreatedAtTheTargetCreatesALostInstanceAgainAtOnceWithRecoveryNone`、`TestCoordinatedGroupRecreatesALostInstanceBelowThePartition`（`RoleRecreate`，Role 只有一个实例、在 partition 之下：有注解的组等依赖，没有的不等）；`mscontroller`（两个 controller 一起跑）：`TestANewGroupIsCreatedFromTheTargetBelowARolePartition`（RUN-231、233 和 RUN-273 两种）、`TestAGroupThatIsDeletedUnderARolePartitionComesBackFromTheTarget`、`TestANewGroupOfCoordinatedRolesIsCreatedFromTheTarget`、`TestALostInstanceOfANewGroupOfCoordinatedRolesWaitsOnlyWhenItsRoleStartsAnew`（什么时候等依赖）、`TestANewGroupSaysWhichRevisionItWasCreatedFrom`、`TestAnObjectForPodsThatExistDoesNotSayWhereItComesFrom`；注解的摘除：`TestTheWriteThatTellsAGroupAnotherTarget`、`TestTheTargetAndTheAnnotationOfAGroupChangeInOneWrite`（一轮在每个写之后截断）、`TestAGroupThatIsToldAnotherTargetStopsSayingWhereItComesFrom`、`TestAGroupThatRanTheBaselineCreatesItsProtectedInstanceFromTheBaseline`（滚完之后改回去、灰度放弃又重做、目标走开又回来三种经过）；什么都还没有建出来的组：`servinggroup.TestBeingBorn`、`TestAGroupWithoutAMemberIsCreatedFromTheTargetItIsTold`、`TestAGroupIsToldAnotherTargetOrCommitsToItsFirstPodNotBoth`（下发和组的写，五种先后）、`TestANewGroupIsToldAnotherTargetWhileItIsBuilt`（Pod 都建不出来；建出了一部分的三种）、`TestAGroupThatWasWholeAndLostEveryPodIsNotBeingBorn`（建全过、Pod 全丢了的组）、`TestANewGroupOfCoordinatedRolesIsToldAnotherTargetWhileItIsBuilt`（协调滚动，四种）；策略切换：`TestTheStrategyIsChangedWithAGroupThatSaysWhereItComesFrom`；说自己是按目标建的、却有别的版本的 Pod 的组：`TestPlanRolesOfAGroupThatWasCreatedAtTheTarget` 的后六行、`TestPlanRolesRolledTogether` 里「a group that says the target and has decode of another revision」一行、`sgcontroller.TestCoordinatedGroupThatSaysTheTargetAndHasPodsOfAnotherRevisionIsNotTakenAtItsWord`、`mscontroller.TestAGroupThatSaysTheTargetAndHasPodsOfAnotherRevisionIsNotTakenAtItsWord` |

## 10. 没有解决的

| 项 | 内容 |
| --- | --- |
| 预算是对多个对象求和 | 两个进程并存时一个额度可能被用两次。靠独占把窗口压到失主的那几秒 |
| 同名重建依赖垃圾回收器 | 垃圾回收器停了，组停在终止中，那个序号建不出新的。有 reason，没有缓解 |
| 替代的组建不出 Pod 时 surge 组一直留着 | V12 的代价，上游也是这样。平常这正是想要的：替代没有起来，surge 组顶着它的位置。但是如果占着资源的正是 surge 组（命名空间的 Pod 配额或者 `count/podgroups` 配额只够 N 个组，滚动途中配额被调小），替代的组永远建不出 Pod，surge 组也永远不走，要调大配额或者手工删掉 surge 组（删掉之后不会再建）。那个建不出 Pod 的组自己的 condition 说明原因（`ReconcileFailed`），ModelServing 过了期限报 `RolloutBlocked`。Role 的临时实例同理。评审另外列出几种要两处同时出问题才到得了的停住状态（一个永远建不出 Pod 的 surge 组，加上 `maxSurge` 调小之后另一个失败而清不空的 surge 组，或者加上 `maxSurge` 为 0 时一个范围外永远没有成员的正式组：没有 Pod 的 surge 组也等位置补齐，而位置因为它占着容量建不出来；`maxUnavailable` 和 `maxSurge` 都是 0（webhook 拒绝这一对）时留下的 surge 组；`maxSurge` 调成 0 之后一个范围外的正式组永远没有成员；带标记的组在范围之内、正式的组已经有 N 个而其中一个在范围之外永远没有成员；目标版本记不下来的那种轮次里，原地更新、留着的 surge 组、一个没有成员的正式组三样凑在一起），和两种一处异常加上不寻常的起点的（带标记的组在范围之内而永远建不出 Pod、序号又是稀疏的：它不再被回收，随后被提升成正式的组，那个位置就停在一个没有 Pod 的组上；`maxSurge` 调成 0 的时候一个被替换的组还在终止、surge 组又一直不就绪，缺的位置建不出来，这一种从一开始就有），都原样留着。头两种（没有 Pod 的 surge 组占着容量）里手工删掉那个 surge 组就结束。见任务 010 的 `subagents/i71-code-review.md` 的 F5（X2、X3）、N2、N3、N4、SN2、SN3 和 `i71-proposal-analysis.md` 的 2.3 |
| 同名的 PodGroup 先删后建、PodGroup 的 owner 变化 | Volcano 的调度缓存按命名空间和名字认 PodGroup。读源码没有发现问题，在 Kind 上确认 |
| 只选中一种 Role 的比例型 PDB | 分母偏大（2.3） |
| 大的账本 | 没有上限，整个重写。1000 个实例约 70 KB |
| 重启故障的 grace 在新进程里重新计 | D9。重启的间隔一直短于 grace 时一直不恢复 |
| 回退与换主重叠 | 交还之前等的是本进程里各组的轮次（6.8 的读写锁）。失主的旧进程发出去而还没落地的那一个写不在其中；靠失主退出把窗口压到一次请求 |
| 组被整个删除时的 `OnRoleDelete` | 只是尽力，并且只对前台删除的组调（7.2）：对象可能在调用它的那一轮之前就没了，也可能被调不止一次。plugin 为组建的资源要以组为 owner |
| Pod 的删除事件丢失之后缓存里留着一个不存在的 Pod（runner F11，`lost-deletion` RUN-447，基线上同样失败）| Role 逐个滚动、`maxSurge: 1`、`maxUnavailable: 0`。被排空的实例的 Pod 已经从 API 里消失，watch 的 DELETED 事件被丢掉，informer 的缓存里一直留着它：实例停在 `Draining`，组的 `pods` 比实际多 1，ModelServing 报 `RolloutBlocked=GroupNotObserved`。两个 controller 共用一份缓存，client-go 不会自己重新 LIST。「同名的上一个对象的 Pod」那一处已经改成直读（9A.3），排空的结束和 `observedPods` 的比对还没有：需要对「该消失而还在缓存里的成员」和「长时间对不上的摘要」做定期的直读核对。同一套件的另外五例（RUN-443…446、448）通过 |
| roleCoordination 的两种停住，condition 不说 | 都是从上游逐字搬来的规则带来的，上游读代码也是这样；第二轮评审用临时测试复现过（R2-S7）。改规则会改变契约约束的行为，不在这次批准的范围里，原样保留。(1) 一个序号较低的旧版本实例未就绪，又不算故障（没有 `Failed`，没有计划外的重启），恢复不会动它。协调只按序号从高到低放行旧实例的替换，低序号的不越过还在等的高序号的；比它序号高的旧实例要占健康的额度，而额度被这个未就绪的实例占着（比如 `maxUnavailable` 是 1）。于是每一轮都不换任何实例。(2) 某个参加协调的 Role，基线和目标对它渲染相同，它名下却有中间版本的实例（上一次滚动没做完又改了模板留下的）。协调按「这个 Role 的模板没有变」算，给它的替换额度是 0，这些实例一直留在中间版本。两种情形下 `CoordinatedRoleRolloutBlocked` 都不是真，reason 是 `ProgressAvailable`（「可以推进」），没有 reason 说它在等什么。两种情形各自为什么停住，是读 `roleplan.go` 的 `constrain` 和 `coordination.go` 的 `calculateRoleRolloutLimits` 得出的，评审记录里只写了这两种状态 |
| 只做删除的轮次里，这一轮自己发起的排空照做 | 没有 PodGroup 的轮次（同步不上，或者布局读不出来）不建、不打补丁，但是这一轮写下的 Draining 照样删 Pod：实例恢复、Role 的滚动替换、Role 的缩容都是这样。PodGroup 一直同步不上的话，清空了的实例要等它恢复才建得回来。接受：已经失败的组要靠这条路清空才能被替换（3.7） |
| Pod 模板的协议标签是必需的 | 进程给自己的 Pod 补标签只是兜底（6.3 的 X1）。Deployment 的 Pod 模板上没有 `modelserving.volcano.sh/controller-protocol: servinggroup` 时，每个新副本从被建出来到它的进程补上标签为止没有标签；正在工作的主把它当成旧版本，退出，等它补上标签再重新参选。扩副本、滚动重启、节点故障后的重建都会这样。chart 和发布的清单带着这个标签；自己写清单部署的，升级指南写明要加 |
| 接管的各个写之后的停点没有单测 | 接管的每一步幂等、可以续做，这一点靠 Kind 的升级演练里在每一步之后杀进程来确认。4.5 的停点框架没有覆盖接管的各个写 |
| `RoleRollingUpdate` 下新的组建到一半时模板又变了（runner F13 留下的窗口，F13 的代码复审）| 新的组已经建出了一部分（有实例建成，或者有 Pod），其余的实例还没有；这时模板从 B 改成 C。组的注解摘掉（2.3），还没有的实例在 Role 的 partition 之下按基线 A 建，这个组从来没有跑过 A；它们受 partition 保护，partition 调低之前一直是 A。两个改动要落在组建全之前：Pod 被配额拒绝了一部分，或者协调滚动下依赖别的 Role 的那个 Role 还在等对方就绪（不需要任何故障，窗口是被依赖的 Role 就绪要的那几分钟）。上游整个组一次建完，这些实例是 B。组是全的，不会停住。要消除它：把组出生时的版本一直留到组建全过一次为止，partition 之下缺的实例按那个版本建（也就是上游最后的样子）。这要求那个版本在历史里留着不被清理，并且组建全的时候多一次写把它摘掉。这次没有做 |
| `RoleRollingUpdate` 下新的组刚承诺 Pod 时模板又变了（F13 的代码复审）| 新的组承诺了 Pod、Pod 还没有建出来，这时给它下发了新的目标 C。它看起来什么都没有建，注解改成 C；而承诺的那一轮接着按上一个目标 B 把 Pod 建了出来（2.3）。看到的是：组的注解 `created-revision` 等于 update revision，组里却有别的版本的 Pod。ServingGroup controller 不照这样的注解办，所以结果和上一行一样：还没有的实例在 Role 的 partition 之下按基线 A 建，组是全的，不会停住。窗口从组承诺开始，到 ModelServing controller 看到它的一个 Pod、或者组的下一次 `status` 写为止，不需要缓存落后；模板要正好在这段时间里变。想让这个组整个是 C，删掉这个 ServingGroup，它按目标整个重建。要关掉这个窗口：对什么都没有建出来的组不改注解，而是换掉对象（删掉，由 G4 按目标新建），旧对象那一轮建出来的 Pod 属于旧的 UID，被回收。这次没有做 |
