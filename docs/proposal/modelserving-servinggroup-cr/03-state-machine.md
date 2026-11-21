# 状态机

## 0. 文档信息

| 项 | 内容 |
| --- | --- |
| 用途 | 重画 [007 的 v4](../modelserving-state-machine-v3-v4/04-v4-servinggroup-cr.md) 第 4、5 节的状态机。读完这一份应当能回答三个问题：一轮 reconcile 做了什么；一个 ServingGroup、一个 Role 实例现在是什么状态、凭什么这样判定、下一步去哪；任何一个 controller 在任何一步换主或被接管，新进程会怎样 |
| 以谁为准 | 规则和理由以 [02](02-design.md) 为准，这里是它的逐条画法。两者有出入时是本文的错误 |
| 状态 | 设计提案，没有实现。每条边的证据来源在[第 12 节](#12-证据索引)：模型穷举过的，还是推演 |
| 记号 | E 开头是 Role 实例的转换，S 开头是 ServingGroup 的转换，G1–G6 是组一级一轮计划的六步（02 的 4.2），M、R 开头是两个 controller 一轮里的步骤，W 开头是写，L1–L7 是账本的规则（02 的 5.2），A1–A6 是两个 controller 之间的约定（02 第 3 节） |

## 1. 怎样读

| 想知道 | 看 |
| --- | --- |
| 一个状态是凭什么判定的 | 第 3–6 节每一层的判定表。条件都只用对象上的字段写 |
| 一个状态能去哪、谁推动、要什么条件 | 第 4.3、5.3 节的转换表 |
| 一轮 reconcile 的完整过程 | 第 7、8 节，两个 controller 各一条线 |
| 两个 controller 怎样配合 | 第 9 节，四张时序图 |
| 在任何一步停下会怎样 | 第 10 节。10.2 把每一次写当作一个可能停下的点 |

三条贯穿全文的性质：

1. **状态不存储，只判定。**每一轮从对象重新得出。写在对象上的只有事实（账本的值、Failed、`deletionTimestamp`、版本），不是「我做到第几步」。
2. **每个状态的动作只取决于这个状态。**不取决于是怎样到达的、是哪个进程到达的。
3. **动作之前的提交点是一次带条件的写。**提交之后停下，下一个进程读到提交的事实，接着做；提交之前停下，下一个进程重新判断。

## 2. 状态在哪里

| 层 | 状态由什么得出 | 存在哪个对象上 | 谁写 |
| --- | --- | --- | --- |
| Pod 成员 | Pod 的 `deletionTimestamp`、`phase`、容器状态、原地更新的注解 | Pod | kubelet、环境；ServingGroup controller 只创建和删除 |
| Role 实例 | 账本的值 × 成员的观察 × 组的 `spec` | `ServingGroup.status.roleInstances[]` | ServingGroup controller |
| ServingGroup | `deletionTimestamp`；`status` 的 Failed、Drained、Adopted、Ready；`observedGeneration` | ServingGroup | 删除：ModelServing controller。`status`：ServingGroup controller |
| ModelServing | 名下各组的状态和版本；`spec` | 不存储。只有基线在 `status.currentRevision` | ModelServing controller |

内存里没有状态。内存里的三样东西（在途的写、故障起点、队列的退避）只会让动作晚一点（02 的 6.1）。

## 3. 第一层：Pod 成员

ServingGroup controller 对名下每个期望的位置判定一个成员状态，自上而下取第一个成立的。与 v2 相同，没有 finalizer。

| 成员状态 | 条件 | 对所在实例的含义 |
| --- | --- | --- |
| 待接管 | 标签是这个组的，controller owner 还是 ModelServing（只在升级接管时出现，[06](06-live-upgrade.md)） | 是成员。不为这个名字创建，也不删除它；换 owner |
| 终止中 | `deletionTimestamp` 非空 | 已建成的实例：丢失。创建中的实例：等它消失 |
| 终态 | `phase` 是 `Failed` 或 `Succeeded` | 故障，不等 grace |
| 原地更新中 | 原地更新的进度注解没有完成 | 不是故障。走原地更新的接口点 |
| 重启故障（到期） | 有计划外的容器重启、不 Ready，恢复生效，并且满了 grace | 故障 |
| 重启故障（未到期） | 同上，没满 grace；或者恢复不生效 | 未就绪。满 grace 的时刻定时入队；恢复不生效时只报告 |
| 就绪 | `Running` 并且 Ready | — |
| 未就绪 | 其余 | 等待 |
| 缺失 | 该有这个成员而没有 Pod | 已建成的实例：丢失。创建中的实例：创建 |
| 外来 | 同名的 Pod 不属于这个组（owner 不对） | 不删它。自己的这个成员按缺失算；创建撞名字时报告并等待 |

「恢复生效」指 `recoveryPolicy` 不是 `None`，并且 grace 不是 `-1`。故障起点在内存里（D9）：丢了，重新计满一个 grace。

## 4. 第二层：Role 实例

### 4.1 状态

一个 Role 实例是「组里某种 Role 的第 i 个」。它不是对象，状态记在组的账本里。

| 状态 | 判定 | 属性 |
| --- | --- | --- |
| 不存在 | 没有账本项，没有任何 Pod | — |
| 创建中 Creating(g) | 账本项是 Creating；或者没有账本项而有 Pod、或者在期望之内（零值） | — |
| 已建成 Established(g) | 账本项是 Established | 就绪：成员齐全并且全部就绪。否则未就绪 |
| 清空中 Draining(g) | 账本项是 Draining | — |

g 是代际号。没有账本项时 g 取现有 Pod 的最大代际号，没有 Pod 时取 0。

「在期望之内」：序号小于组 `spec` 里这种 Role 的 `replicas`，并且没有被 Role 一级的计划选中删除（Role 缩容、按重建执行的滚动替换、回收 Role 一级的 surge）。Role 一级的计划沿用 v2，它把清空中当作 v2 的「终止中」，创建中当作「未就绪」。

### 4.2 图

```mermaid
stateDiagram-v2
    direction LR
    [*] --> 创建中: 在期望之内 E7
    创建中 --> 已建成: 成员齐全，写 Established E8
    已建成 --> 清空中: 丢失或故障，RoleRecreate，写 Draining E5 E6
    创建中 --> 清空中: 故障，RoleRecreate E5。不再期望 E4
    已建成 --> 清空中: 不再期望 E4
    清空中 --> 创建中: 清空了，还在期望之内，写 Creating(g+1) E3
    清空中 --> [*]: 清空了，不在期望之内，去掉账本项 E3
    已建成 --> 已建成: None 下补一个 E6
```

`ServingGroupRecreate` 下的丢失和故障不走实例的边：它让整个组失败（[5.3](#53-转换) 的 S3），之后实例冻结。

### 4.3 转换

每一轮对每个实例自上而下取第一个成立的触发。每一格恰好一个反应。

| # | 触发 | 适用的状态 | 反应 | 写 | 之后 |
| --- | --- | --- | --- | --- | --- |
| E0 | 组在终止中 | 全部 | 不做事。垃圾回收器在删 | — | 不变 |
| E1 | 组已失败 | 全部 | 只删这个实例里故障的成员 | DELETE Pod | 不变 |
| E2 | 账本是 Draining(g)，还有代际号不大于 g 的 Pod | 清空中 | 本进程还没为它成功调过 `OnRoleDelete` 的先调。然后两步删除：先删故障的成员；没有故障的成员、也没有正在终止的 Pod 之后，删其余的 | DELETE Pod | 清空中 |
| E3 | 账本是 Draining(g)，没有代际号不大于 g 的 Pod | 清空中 | 还在期望之内：写 Creating(g′)，g′ = max(g, 现有 Pod 的最大代际号) + 1。不在：去掉账本项 | status | 创建中，或不存在 |
| E4 | 不在期望之内，并且有账本项或有 Pod | 创建中、已建成 | 写 Draining(g) | status | 清空中 |
| E5 | 有故障的成员；证据直读确认过 | 创建中、已建成 | `None`：终态的删这一个，重启故障只报告。`RoleRecreate`：写 Draining(g)。`ServingGroupRecreate`：写组的 Failed | DELETE Pod，或 status | 不变；清空中；组失败 |
| E6 | 已建成，有成员缺失或在终止；证据直读确认过 | 已建成 | `None`：缺失的创建，在终止的等它消失。`RoleRecreate`：写 Draining(g)。`ServingGroupRecreate`：写组的 Failed | 创建 Pod，或 status | 已建成；清空中；组失败 |
| E7 | 创建中，有成员缺失 | 创建中 | 缺失的创建，带代际号 g。位置上有终止中的 Pod：等。位置被外来 Pod 占着：报告，等 | 创建 Pod | 创建中 |
| E8 | 创建中，成员齐全，没有在终止的，都在缓存里看到了 | 创建中 | 写 Established(g) | status | 已建成 |
| E9 | 已建成，成员齐全 | 已建成 | 不做事 | — | 已建成 |

几点说明。

- **E5、E6 的直读（L4）。**把缺失当证据：按名字读，要读到 NotFound。把终止中、终态当证据：要读回同一个 UID 并且仍然如此。读到的和缓存不一样，这一轮不动，等缓存追上。
- **E4 到 E6 先写再删（L1）。**status 写被拒绝（resourceVersion 冲突），本轮后面的删除和创建都不发。
- **E8 看的是缓存，不是创建请求的返回（L3）。**
- **原地更新中的成员**不触发 E5、E6；超过进度期限并且恢复生效时按故障算（007 的 05）。
- **策略取动手那一刻的 `spec`。**账本的值不记策略：Draining 写上之后策略改了，清空照样做完；清空之后建不建，看当时的期望。

### 4.4 每个状态一张卡片

**不存在**

| 项 | 内容 |
| --- | --- |
| 判定 | 没有账本项，没有 Pod |
| 进入 | E3：清空了并且不在期望之内 |
| 离开 | `spec` 把它纳入期望：它就是创建中（零值），E7 开始创建 |
| 不允许 | — |
| 接管时 | 新进程看到的和原进程相同 |

**创建中 Creating(g)**

| 项 | 内容 |
| --- | --- |
| 判定 | 账本项是 Creating，或者没有账本项而在期望之内或有 Pod |
| 进入 | 新的期望；E3 清空之后重建 |
| 离开 | E8 → 已建成；E4、E5 → 清空中；E5（`ServingGroupRecreate`）→ 组失败 |
| 这个状态下缺成员的含义 | **没建完。只补。** |
| 不允许 | 因为缺失而删除任何 Pod |
| 接管时 | 新进程看到 Creating 和若干成员：缺的补（名字确定，重复创建撞名字），齐了写 Established。原进程写了 Established 而新进程的缓存还没看到时，新进程按 Creating 处理：这段时间里的丢失只补，比原进程保守 |

**已建成 Established(g)**

| 项 | 内容 |
| --- | --- |
| 判定 | 账本项是 Established |
| 进入 | E8 |
| 离开 | E4、E5、E6 → 清空中；E5、E6（`ServingGroupRecreate`）→ 组失败 |
| 这个状态下缺成员的含义 | **丢失。按策略的范围恢复。**与 controller 当时在不在、换没换主无关 |
| 不允许 | 不经直读就把缺失当丢失 |
| 接管时 | 新进程读到 Established。成员在不在，直读确认。结果与原进程相同 |

**清空中 Draining(g)**

| 项 | 内容 |
| --- | --- |
| 判定 | 账本项是 Draining |
| 进入 | E4、E5、E6。**进入是一次 status 写，它是提交点** |
| 离开 | E3 |
| 这个状态下的动作 | 删代际号不大于 g 的 Pod，两步；不创建 |
| 不允许 | 删代际号大于 g 的 Pod（L5）；在这个实例里创建 |
| 接管时 | 新进程读到 Draining(g)，接着删同一批。不问原因、不问是谁写的。`OnRoleDelete` 可能被再调一次。原进程已经写了 Creating(g+1) 并建了新 Pod，而另一个进程读到的还是 Draining(g)：它只能删代际号不大于 g 的，碰不到新的 |

## 5. 第三层：ServingGroup

### 5.1 状态

生命周期五个状态。ModelServing controller 自上而下判定。

| 状态 | 判定 | 计入 |
| --- | --- | --- |
| 终止中 | `deletionTimestamp` 非空 | `active` |
| 失败，已清空 | Failed 为真，Drained 为真 | `active` |
| 失败，清理中 | Failed 为真，Drained 不为真 | `active` |
| 接管中 | 带存量注解，Adopted 不为真（只在升级接管时出现） | `active`、`live`。不是缩容和替换的候选 |
| 运行 | 其余 | `active`、`live` |

「运行」的组另有两组派生的属性，它们不是状态，只出现在边的条件里：

| 属性 | 取值 | 判定 |
| --- | --- | --- |
| 可用性 | 未观测 | `observedGeneration` 小于 `generation`。占位置，不算就绪；不是替换候选，缩容时不排在「未就绪的先」里 |
| | 就绪 | 已观测，Ready 为真 |
| | 未就绪 | 已观测，Ready 不为真。包括还在创建的 |
| 版本类 | 目标、受保护、过期 | 02 的 4.1 |

给人看的阶段（`kubectl get servinggroup` 的一列）按同一张表，把「运行」细分成 Creating（还有期望的实例没建成）、Ready、NotReady。它不参与任何判断。

### 5.2 图

```mermaid
stateDiagram-v2
    direction LR
    [*] --> 运行: 创建 S1
    [*] --> 接管中: 接管存量的组 S1′
    接管中 --> 运行: 成员都换了 owner S2
    运行 --> 失败清理中: ServingGroupRecreate 下丢失或故障，写 Failed S3
    失败清理中 --> 失败已清空: 没有故障成员和终止中的 Pod，写 Drained S4
    失败已清空 --> 失败清理中: 又有成员故障 S4′
    失败已清空 --> 终止中: ModelServing controller 删除 S5
    运行 --> 终止中: 缩容、替换 S6
    终止中 --> [*]: Pod、PodGroup、Service 都消失 S7
```

### 5.3 转换

| # | 从 | 到 | 谁推动 | 条件 | 写和前置条件 |
| --- | --- | --- | --- | --- | --- |
| S1 | — | 运行 | ModelServing controller | G4 | 创建 ServingGroup。名字先登记；没有在途的创建 |
| S1′ | — | 接管中 | ModelServing controller | 升级接管（06） | 创建 ServingGroup，名字是存量的名字。撞名字是幂等的 |
| S2 | 接管中 | 运行 | ServingGroup controller | 直读：名下没有 owner 还是 ModelServing 的成员 | status：Adopted 为真 |
| S3 | 运行 | 失败，清理中 | ServingGroup controller | E5 或 E6，策略是 `ServingGroupRecreate` | status：Failed 为真。带 resourceVersion。**提交点** |
| S4 | 失败，清理中 | 失败，已清空 | ServingGroup controller | 没有故障的成员，没有正在终止的 Pod | status：Drained 为真 |
| S4′ | 失败，已清空 | 失败，清理中 | ServingGroup controller | 又有成员故障 | status：Drained 为假 |
| S5 | 失败，已清空 | 终止中 | ModelServing controller | G1 | DELETE，前台级联，带 UID 和 resourceVersion。S4′ 发生过，这次删除被拒绝 |
| S6 | 运行 | 终止中 | ModelServing controller | G2 或 G3 | 同上 |
| S7 | 终止中 | — | 垃圾回收器 | 带 `blockOwnerDeletion` 的子对象都消失 | — |
| S8 | 任何 | 终止中，或直接消失 | 用户 | `kubectl delete servinggroup` | 02 的 5.5 |

失败不可逆：没有从失败回到运行的边。策略在 S3 之后改了，照样删。

### 5.4 每个状态一张卡片

**运行**

| 项 | 内容 |
| --- | --- |
| 判定 | 没有 `deletionTimestamp`，Failed 不为真，不在接管中 |
| ServingGroup controller 在里面做 | 第 8 节的全部步骤 |
| ModelServing controller 对它可以做 | G2 缩容、G3 替换、G5 下发 `spec` |
| 离开 | S3、S6、S8 |
| 接管时 | 两个 controller 的新进程都从对象重新判定。未观测的组等 ServingGroup controller 过一遍 |

**失败，清理中**

| 项 | 内容 |
| --- | --- |
| 判定 | Failed 为真，Drained 不为真 |
| ServingGroup controller 在里面做 | 只删故障的成员（E1）。不创建，不推进账本，不做 Role 一级的任何事 |
| ModelServing controller 对它可以做 | 什么都不做，等。它占着一个位置，替换的组不建（没有滚动在进行时） |
| 为什么等 | D15：故障的成员停下并且消失之前，其余成员继续运行 |
| 离开 | S4 |
| 接管时 | 新进程读到 Failed：ServingGroup controller 接着删故障的成员，ModelServing controller 接着等。谁写的、因为什么写的，都不需要知道 |

**失败，已清空**

| 项 | 内容 |
| --- | --- |
| 判定 | Failed 和 Drained 都为真 |
| ModelServing controller 对它可以做 | G1：删除 |
| 离开 | S5；S4′ |
| 接管时 | 新进程的 ModelServing controller 直接删。DELETE 带 resourceVersion，读到的 `status` 不是最新时被拒绝 |

**终止中**

| 项 | 内容 |
| --- | --- |
| 判定 | `deletionTimestamp` 非空 |
| ServingGroup controller 在里面做 | 不做事。尽力调一次 `OnRoleDelete`（02 第 7 节） |
| ModelServing controller 对它可以做 | 不做事。它占着一个位置直到消失 |
| 谁在推进 | 垃圾回收器。kthena 不在也照样推进 |
| 接管时 | 没有需要接着做的事 |

**接管中**：见 [06](06-live-upgrade.md) 第 5 节。

## 6. 第四层：ModelServing

ModelServing 一级没有要存储的状态。对外是 `status` 的计数和 condition，全部按本轮的观察算；只有基线 `status.currentRevision` 是下一轮的输入。

计数：

| 字段 | 定义 |
| --- | --- |
| `replicas` | 没有 `deletionTimestamp` 的组（`live` 加失败的） |
| `updatedReplicas` | 目标版本的组 |
| `currentReplicas` | `live` 里不是目标版本的组 |
| `availableReplicas` | Ready 为真的 `live` 组。未观测的组按它上一次报告的 Ready 计：对外的可用数不因为 `spec` 下发而抖动，预算里才按未就绪算 |
| `currentRevision`、`updateRevision` | B、T |

condition 各自独立判定，可以同时为真：

| 判定式 | 含义 | 写在哪 |
| --- | --- | --- |
| 名下还有存量对象，或者有组在接管中 | 接管中 | `Progressing`，reason `Adopting` |
| 有失败的组 | 整组恢复中 | `Progressing`，reason `Recovering` |
| `live` > `cap`，或者 N − `good` − 过期的组 > 0 | 扩缩中 | `Progressing`，reason `Scaling` |
| 有过期的组，或者有未就绪的目标组而 B ≠ T | 滚动中 | `UpdateInProgress` 为真 |
| B ≠ T，没有过期的组，有受保护的组，`good` = N，全部就绪 | 灰度停点 | `UpdateInProgress` 为假，reason `PartitionPaused` |
| 有过期的组，G3 选不出候选，G4 建不了 | 阻塞 | `Progressing` 为假，reason 是原因：`TargetNotReady`（目标组未就绪占着额度）、`BudgetExhausted`、`BaselineUnknown`、`WaitingForDeletion` |
| `ready` ≥ `minAvailable` | 可用 | `Available` |

给人看的阶段按固定的优先级取一个：接管中、恢复中、阻塞、扩缩中、滚动中、灰度停点、稳定。

## 7. ModelServing controller 的一轮

key 是一个 ModelServing。

| 步 | 做什么 | 读 | 写 | 这一步停下 |
| --- | --- | --- | --- | --- |
| M0 入队 | ModelServing 的新增、`generation` 变化、删除；名下任何 ServingGroup 的新增、更新、删除；在途登记到期；周期 resync。接管期间另有存量 Pod 的事件 | — | — | — |
| M1 接管屏障 | 本进程的屏障没过：重新入队，返回 | 进程内的信号 | — | 无影响 |
| M2 取对象 | ModelServing；它在删除中则返回，垃圾回收器会删名下的组 | 缓存 | — | 无影响 |
| M3 升级闸门 | 这个 ModelServing 还没有「接管完成」的标记：走 [06](06-live-upgrade.md) 的接管，本轮不做 M6 之后的写 | 直读 | 06 | 06 |
| M4 观察 | 名下的组，叠加在途登记。**有在途的创建：本轮不发组的写**，等缓存或期限 | 缓存、内存 | — | 无影响 |
| M5 版本 | 解析 T：先在已有的 ControllerRevision 里找语义相同的，没有就创建。取 B（02 的 4.8） | 缓存 | W1 创建 ControllerRevision | 重复创建撞名字 |
| M6 分类和记账 | 02 的 4.1 | — | — | 纯函数 |
| M7 计划 | G1 到 G6，在工作副本上迭代 | — | — | 纯函数 |
| M8 执行 | 按 G1、G2、G3、G4、G5 的次序发。每个写之前按最新的缓存重算一次，这个写不再在计划里就不发 | 缓存 | W2 DELETE 组；W3 创建组；W4 PATCH 组的 `spec` | 第 10.2 节 |
| M9 写 status | 每一轮都写，出错的轮次也写。G6 的基线在这一次写里 | — | W5 `ModelServing.status` | 第 10.2 节 |
| M10 清理 | 清理不再引用的 ControllerRevision（02 的 8.5） | 缓存 | W6 DELETE ControllerRevision | 下一轮重做 |

等待都是返回：等组就绪、等组消失，由组的事件带来下一轮。等在途登记到期用定时入队。

## 8. ServingGroup controller 的一轮

key 是一个 ServingGroup。

| 步 | 做什么 | 读 | 写 | 这一步停下 |
| --- | --- | --- | --- | --- |
| R0 入队 | 这个组的新增、更新、删除；名下任何 Pod 的新增、更新、删除（包括 owner 被改）；PodGroup、Service 的删除；grace 到期、原地更新期限到期、在途的写到期；周期 resync | — | — | — |
| R1 取对象 | 这个组。它在终止中：尽力调 `OnRoleDelete`，返回 | 缓存 | — | 无影响 |
| R2 接管成员 | 名下有待接管的成员：换 owner（06） | 直读 | W7 PATCH Pod、PodGroup、Service 的 owner | 06 |
| R3 观察 | 名下的 Pod，叠加在途的创建和删除。读 `spec` 引用的 ControllerRevision | 缓存、内存 | — | 无影响 |
| R4 分类 | 成员（第 3 节）、实例（4.1）、组（5.1） | — | — | 纯函数 |
| R5 组已失败 | 只做 E1；算 Drained；跳到 R11 | — | W10 DELETE Pod | 第 10.2 节 |
| R6 Role 一级的计划 | v2 的 Role 层规则：扩缩容、滚动（重建或原地）、协调。得出哪些实例不再期望、哪些成员要打补丁 | — | — | 纯函数 |
| R7 账本的转换 | 对每个实例走 4.3 的表，得出这一轮的全部转换。E5、E6 的证据在这里直读 | 直读 | — | 无影响 |
| R8 提交 | 有转换（E3、E4、E5、E6、E8，或 S3）：写 status。**被拒绝：返回，本轮后面不发任何删除和创建** | — | W8 `ServingGroup.status`，带 resourceVersion | 第 10.2 节 |
| R9 钩子和删除 | 清空中的实例：`OnRoleDelete`（本进程第一次），两步删除。`None` 下终态的成员：删 | — | W9 plugin 钩子；W10 DELETE Pod，带 UID | 第 10.2 节 |
| R10 创建 | PodGroup、Service 不在的建；E6（`None`）、E7 的成员：`OnPodCreate`，创建。原地更新的补丁 | — | W11 创建 Pod、PodGroup、Service；W12 Pod 的注解和镜像 | 第 10.2 节 |
| R11 写 status | `observedGeneration`、Ready、Drained、版本、引用到的版本、deletion cost、计数。这一轮没有 R8 时两次写合成一次 | — | W8 | 下一轮重写 |

`observedGeneration` 在 R8 或 R11 的写里推进，条件是「这一轮的决定是按这份 `spec` 做的」，不等动作做完。每次 status 写都把 Ready、版本按这一轮读到的 `spec` 重算，不带着上一份 `spec` 的结论。

## 9. 跨 controller 的协议

### 9.1 下发 `spec`

```mermaid
sequenceDiagram
    participant U as 用户
    participant M as ModelServing controller
    participant G as ServingGroup 对象
    participant S as ServingGroup controller
    U->>M: 改 ModelServing（generation 12）
    M->>G: PATCH spec，sourceGeneration=12
    Note over G: generation 变大，observedGeneration 落后：未观测
    Note over M: 这个组占位置，不算就绪，不是候选
    S->>G: 读 spec，按它计划
    S->>G: UPDATE status，observedGeneration 追上
    Note over M: 重新按 Ready 计
```

### 9.2 整组恢复

```mermaid
sequenceDiagram
    participant S as ServingGroup controller
    participant G as ServingGroup 对象
    participant M as ModelServing controller
    participant GC as 垃圾回收器
    Note over S: 已建成的实例丢了成员，策略是 ServingGroupRecreate。直读确认
    S->>G: status：Failed=真（提交点）
    S->>S: 只删故障的成员
    Note over M: 失败，清理中：等
    S->>G: status：Drained=真
    M->>G: DELETE，前台级联，带 UID 和 resourceVersion
    GC->>GC: 删这个组的 Pod、PodGroup、Service
    Note over M: 终止中：仍占一个位置
    GC->>G: 子对象都消失，组对象消失
    M->>M: G4：建一个新的组，新名字，版本按数量选
```

### 9.3 清空一个 Role 实例

```mermaid
sequenceDiagram
    participant S as ServingGroup controller
    participant G as ServingGroup 对象
    participant P as Pod
    Note over S: E4、E5 或 E6
    S->>G: status：这个实例 Draining(g)（提交点）
    S->>S: OnRoleDelete
    S->>P: 删故障的成员（代际号不大于 g）
    Note over S: 等故障的和终止中的都消失
    S->>P: 删其余的（代际号不大于 g）
    Note over S: 没有代际号不大于 g 的 Pod
    S->>G: status：Creating(g+1)，或去掉这一项
    S->>P: 创建，带代际号 g+1
    S->>G: status：Established(g+1)
```

### 9.4 `RoleRollingUpdate` 下推进基线

```mermaid
sequenceDiagram
    participant M as ModelServing controller
    participant G as 每个 ServingGroup 对象
    participant S as ServingGroup controller
    M->>G: PATCH spec.targetRevision = T
    S->>S: 组内按 Role 一级的规则滚动
    S->>G: status.revision = T，Ready
    Note over M: 全部组报告 T、已观测、就绪，各 Role 的 partition 都是 0
    M->>M: status.currentRevision := T
    M->>G: PATCH spec.baselineRevision = T
```

## 10. 接管

「接管」包括：进程重启；换主；升级到同一设计的新版本。从旧版本升级过来的接管在 [06](06-live-upgrade.md)。

### 10.1 屏障

新进程在发第一个写之前（02 的 6.3）：三种对象各一次一致性的 LIST 并等缓存追上；ServingGroup controller 先把每个组过一遍；然后 ModelServing controller 才发组的删除和创建。

屏障之后，新进程面对的就是第 3–6 节的判定表，没有别的输入。下面的表说明每一个可能停下的点落在哪一格。

### 10.2 每一次写都是一个可能停下的点

三栏分别是：停在这次写之前；写被 apiserver 接受了，进程在看到结果之前停下；写之后、同一步的下一个写之前停下。

**ModelServing controller**

| 写 | 之前停下 | 接受了、没看到 | 新进程的动作与原进程 |
| --- | --- | --- | --- |
| W2 DELETE 失败的组（G1） | 组是「失败，已清空」：再删 | 组是「终止中」：等 | 相同 |
| W2 DELETE 缩容的组（G2） | `live` 仍大于 `cap`：按同一个次序再选。对象没变，选中的是同一个；变了，按新的选 | 终止中；`live` 少了一个 | 相同，或者按更新的事实 |
| W2 DELETE 替换的组（G3） | 预算重算；候选没变则选同一个 | 终止中 | 相同 |
| W3 创建组（G4） | `good` 仍小于 N：再建。**名字是新的** | 屏障的一致性 LIST 里有这个组：不再建 | 相同。没有屏障时会多建一个，事后缩回 |
| W4 PATCH 组的 `spec`（G5） | 再发，内容相同 | 组是未观测：等 | 相同 |
| W5 `ModelServing.status`，含基线 | 条件还成立：再写 | 已经推进 | 相同。G6 的条件只有在全部完成时才成立，重写是幂等的 |
| W1、W6 ControllerRevision | 重做 | 撞名字，或已删除 | 相同 |
| 一轮里 G2 删了两个中的一个 | 剩下的一个：`live` 仍大于 `cap`，再选 | — | 相同 |
| 一轮里 G3 删了、G4 还没建 | 位置空着：G4 的条件成立，建 | — | 相同 |

**ServingGroup controller**

| 写 | 之前停下 | 接受了、没看到 | 新进程的动作与原进程 |
| --- | --- | --- | --- |
| W8 写 Established（E8） | 创建中，成员齐全：再写 | 已建成 | 相同 |
| W8 写 Draining（E4–E6） | 证据还在：直读，再写。证据不在了（Pod 回来了）：不写 | 清空中：E2 | 相同，或者按更新的事实不删 |
| W8 写 Failed（S3） | 同上 | 失败，清理中 | 相同 |
| W8 写 Creating(g+1) 或去掉项（E3） | 清空中、已空：再写 | 创建中：E7 | 相同 |
| W8 写 Drained（S4） | 条件还成立：再写 | 失败，已清空 | 相同 |
| W10 DELETE Pod，第一步里的一个 | 清空中：E2 接着删 | 那个 Pod 在终止：等 | 相同 |
| W10 第一步做完、第二步之前 | 清空中，没有故障成员和终止中的：删其余的 | — | 相同 |
| W10 第二步删到一半 | 清空中，还有代际号不大于 g 的：接着删 | — | 相同 |
| W11 创建 Pod，一个实例建到一半 | 创建中，缺的补 | 撞名字，核对是自己的 | 相同 |
| W11 创建 PodGroup、Service | 不在就建 | 撞名字 | 相同 |
| W9 `OnRoleDelete` | 清空中：新进程再调一次，再删 | — | 钩子多调一次 |
| W9 `OnPodCreate`、`OnPodReady` | 重调 | — | 钩子多调一次 |
| W12 原地更新的补丁 | 进度在 Pod 注解上：按注解续做 | — | 相同（007 的 05） |
| R8 被拒绝之后 | 没有发过删除：下一轮重新判定 | — | 相同 |

表里没有「取决于原进程记得什么」的格子。「按更新的事实」的格子是水平触发的正常结果：两次观察之间对象变了，动作跟着变。

### 10.3 屏障挡住的两种情形

| 情形 | 没有屏障 | 有屏障 |
| --- | --- | --- |
| 第一次 LIST 是旧的 | 缓存里缺最新建的组：再建一批。缓存里的组是旧的：按旧的 Ready 选删除对象 | 一致性的 LIST 不会旧 |
| 停机期间 Pod 坏了，组的 `status` 没人更新 | ModelServing controller 按过期的 Ready 算健康额度，多删一个健康的组 | ServingGroup controller 先把 `status` 写新 |

### 10.4 两个进程同时在写

失主的进程在发现失主之前还可能发出写。这时没有屏障可言，靠每个写自己的前置条件（02 的 6.2、6.4）。结果：不会把创建中的实例当成丢失去删；不会用旧的 Draining 删掉新一批的 Pod；可能多建组、一个额度删两个、钩子重复、`ModelServing.status` 被来回覆盖；旧的一边退出之后收敛到同一个终局。

### 10.5 `status` 被清空或改写

| 被改的 | 后果 |
| --- | --- |
| 账本被清空 | 全部实例回到创建中。齐全的重新写上已建成；这期间的丢失只补 |
| 账本的代际号丢失 | 取现有 Pod 的最大代际号（L5） |
| Failed 被清掉 | 组回到运行。实例的账本还在：丢失的证据还在的话，重新判定，重新写 Failed |
| Drained 被错误地写成真 | 挡不住：故障成员还没停，组就被删。`status` 端点要用 RBAC 保护 |
| `observedGeneration` 被改小 | 未观测，直到 ServingGroup controller 重写 |
| `ModelServing.status.currentRevision` 被清空 | 从各组的 `spec.baselineRevision` 读回（02 的 4.8） |

## 11. 分不清的情形

设计明确接受的几处：

| # | 情形 | 反应 |
| --- | --- | --- |
| U1 | 实例建成之前成员被删 | 只补。与「没建完」是同一个反应 |
| U2 | Established 的写被接受、别的进程还没看到，这时丢了成员 | 那个进程按创建中只补。只在两个进程并存时出现 |
| U3 | 策略从 `None` 改成范围策略的那一刻，已建成的实例里正好有一个成员在补 | 按新策略整范围恢复 |
| U4 | Failed 或 Draining 写上之后策略改成 `None` | 照样做完。提交之后不撤销 |
| U5 | 一直不就绪的目标版本的组 | 留在 N 个里面，滚动算完成不了（基线不推进），报告 `TargetNotReady`。它是不是故障由成员状态和恢复策略决定，不由滚动决定 |
| U6 | 健康额度按晚一跳的 `status` | 02 的 6.5 |

## 12. 证据索引

| 内容 | 证据 | 位置 |
| --- | --- | --- |
| G1–G6：预算、次序、cap、保护 | 模型穷举；41 个场景脚本；每条规则去掉之后有场景或性质报出来 | [05](05-inference-verification.md) 第 2、3 节 |
| ModelServing controller 在任何一次写之后被接管，终局集合相同 | 模型穷举 | 05 第 3 节 |
| 在途屏障、接管屏障、升级闸门 | 模型：去掉之后分别报多建、删错、接管完成之前动了组 | 05 第 3 节 |
| S3–S5：失败、清空、删除的次序（D15） | 模型：去掉等待之后报出来；007 的组级模型 | 05 第 3、5 节 |
| E3–E8：账本；ServingGroup controller 在任何两次写之间停下 | 007 的组级模型，原样重跑，结果相同 | 05 第 5 节 |
| L4 直读、L5 代际号 | 小模型：去掉之后分别报「没有丢失而清空」「新一批被旧的清空删掉」 | 05 第 4 节 |
| E0、E1、E2 的钩子次序；E7 的外来 Pod | 推演 | — |
| 第 6 节的 condition | 推演 | — |
| A1、A2：未观测、`sourceGeneration` | 未观测的处理有一个场景脚本（EX-3）；其余推演 | 05 |
| 10.5 | 推演；007 的模型里有「`status` 被清空」一项 | 05 第 5 节 |
| Role 一级的规则 | 沿用 v2，由 005 的参考模型覆盖。与账本的衔接是推演 | — |
| 关于 Kubernetes 行为的前提（前台级联、垃圾回收、一致性 LIST） | 按文档和源码，没有在集群上验证 | 05 第 7 节 |
