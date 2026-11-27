# [讨论] ModelServing 单元持久化状态选型：ServingGroup CR（方案 A）、ModelServing.status（方案 B），还是存在方案 C？

> [English](./modelserving-unit-state-discussion.md) | **中文**
>
> **面向 `volcano-sh/kthena` 上游的讨论草稿。** 目前社区尚未提出 `ServingGroup` CR 或 `ModelServing.status` 账本的
> proposal。在正式提交任何 API 变更提案之前，希望先和社区对齐架构方向：**方案 A（CR）**、**方案 B（`ModelServing.status`）**，
> 还是存在更好的 **方案 C**？`CONTRIBUTING.md` 建议大型架构改动先发到 Discussions，因此本文按讨论稿组织；同一份结构也适配
> Question Issue 模板（TL;DR 与第 1–5 节对应问题描述与方案对比，第 6 节对应我们的看法与提问，第 7 节对应环境与参考）。
>
> 文中对 `main` 的代码链接固定在 [`4323a9bf`][main]，对我们在 fork 上用于验证方案 A 代价的原型链接固定在 [`98fcbf01`][proto]。

---

## TL;DR（30 秒速览）

- **核心问题：** `ModelServing` 的恢复策略（`RoleRecreate`、`ServingGroupRecreate`）和滚动升级，操作对象是**大于单个 Pod 的多 Pod 单元**（`ServingGroup` 和 `Role` 实例）。#2112 / PR #2126 已经干净地解决了**实例身份**问题，但还有两类重启敏感的事实目前只存在控制器内存和 watch 事件里：单元**是否曾完整建成**（*established*），以及**是否已决定删除整个单元**（*committed deletion*）。因此控制器重启或换主时，可能出现恢复不足（本该重建整个单元，却只补一个 Pod）或删到一半的单元被中途“修回”。
- **架构选型——方案 A vs. 方案 B vs. 方案 C：**
  - **方案 A（`ServingGroup` CR）：** 每个组天然拥有 UID、独立 status 写范围和 K8s 级联回收——**但**需要线上迁移运行中 Pod 的 owner、拆成两个控制器，且**只解决了一半问题**（组内的 `Role` 实例依然要靠 status 账本管）。我们在 fork 上做了一个方案 A 原型来实测这些代价。
  - **方案 B（`ModelServing.status` 账本）：** 保留单控制器、不改 Pod owner——**但**每次变动都要在同一 `resourceVersion` 上重写整张表，Role 实例数放大后滚动写入呈平方增长，且与 #2112 希望避免给 `ModelServing.status` 增加写入和 schema 变更的目标冲突。
  - **方案 C（是否存在第三条路？）：** 能否同时避开 A 和 B 的痛点？例如**只做记录的组 CR**（单控制器、不接管 Pod owner）、**以 entry Pod 为锚点**（LeaderWorkerSet 模式）、**压缩/分片账本**，或者社区能想到其他更好的方案？
- **在提交任何 API proposal 前，想和社区对齐三件事：**
  1. **保障边界：** 社区是否要求故障恢复和进行中的单元删除在控制器重启/换主后依然生效？还是认为现有缺口可以接受？
  2. **方向选择：** 如果需要这些保障，社区倾向 **方案 A（CR）**、**方案 B（`ModelServing.status`）**，还是 **方案 C**？
  3. **先做什么：** 无论选 A/B/C 哪一条，是否可以先落地不依赖载体的改进（配合推进 #2126、把 reconcile 改造成纯水平触发的状态机、修复协调滚动上报和 watch 删除事件丢失）？

---

## 1. 问题本质：为什么光看集群里的 Pod 不够？

在当前的 `ModelServing` 中，只有最顶层和最底层是真正的 Kubernetes 对象，中间两层都是逻辑概念：

```text
ModelServing (CR)
  └─ ServingGroup（当前为逻辑概念）        ← ServingGroupRecreate 的作用范围
       └─ Role 实例（当前为逻辑概念）       ← RoleRecreate 的作用范围
            ├─ entry Pod   (K8s Pod，owner 为 ModelServing)
            └─ worker Pods (K8s Pods，owner 为 ModelServing)
```

同一个 `Role` 实例或 `ServingGroup` 内的 Pod 彼此耦合（新拉起的 worker 无法直接加入正在运行的 entry），因此 `RoleRecreate` 和 `ServingGroupRecreate` 要求在某个成员丢失时，把整个单元一起删掉重建。

### 同样的现状，两种截然相反的正确做法

假设 `llama-0-prefill-0` 应该有 1 个 entry Pod（`-0`）和 1 个 worker Pod（`-1`）。某次 reconcile 时，控制器看到集群里只有 `-0`，缺少 `-1`：

```text
控制器看到的集群现状： [ llama-0-prefill-0-0 (Running) ]   llama-0-prefill-0-1 不存在

  情况 A（首次创建中）：
    worker -1 还没来得及建出来              ──▶  正确动作：只补建 worker -1

  情况 B（曾经建成，后来丢了 worker）：
    实例已经建完整，但控制器重启/换主期间
    worker -1 被节点驱逐删掉了              ──▶  正确动作（RoleRecreate）：
                                               删掉 -0，重建整个 Role 实例
```

两种情况下，集群里现存的 Pod **完全一模一样**。

### 光看 Pod 读不出来的三样信息

要让多 Pod 单元在控制器重启后依然做出正确判断，控制器需要三类仅靠当前 Pod 集合无法推出来的事实：

| 需要的信息 | 用大白话说就是 | 上游当前进展 |
| --- | --- | --- |
| **1. 实例身份（Identity）** | Pod 名字按位置复用（`<ms>-<group>-<role>-<index>-<n>`）。*这个同名 Pod 属于哪一代实例？* | **推进中（#2112 / PR #2126）：** 给 Pod、Service、PodGroup 打不可变的 `group-instance-id` / `role-instance-id` 标签，删除时校验 owner UID、实例 ID 和 UID 前置条件。我们认同这是身份问题的正解。 |
| **2. 是否曾建成（Established）** | *这个单元以前完整建成过吗，还是仍在首次拉起途中？* | **未解决。** |
| **3. 已决定的删除范围（Committed scope）** | *重启之前，我们是不是已经决定要删掉整个单元（以及删哪一代）？* 删到一半时，剩下的 Pod 看起来就像一个“缺了成员的正常单元”。 | **未解决。** |

> **本次讨论的聚焦范围：** #1999（已合入）解决了丢租约时进程退出；#2112 与 PR #2126 干净地解决了**第 1 项（实例身份）**，不引入新对象也不增加 status 写入。就我们读到的 #2126 当前实现而言，**第 2 项和第 3 项仍未覆盖**：控制器离线期间如果丢了一个 worker（entry 还在、没有 Terminating Pod），恢复后仍会只补单个 Pod；重启时进行中的删除范围也仍按当前策略重新推导。本次讨论聚焦于社区是否需要第 2、3 项保障，以及状态该放在哪里。

### `main` 分支当前会出什么问题

因为这些信息目前存在控制器内存和 informer 回调里：

1. **控制器不在时的成员丢失恢复不足：** 节点驱逐恰好最常发生在升级控制器或主备切换窗口期。如果此时一个完整实例的 worker 被删，新主没收到 delete 事件，只会单独补建这一个 Pod，违背 `RoleRecreate`/`ServingGroupRecreate`。
2. **删到一半的单元被“修活”：** 删到中途控制器重启，内存里的删除映射丢失；正如 #2112 所描述的，正在删除的 Role 可能被误判为“缺了 entry Pod 的健康 Role”，反而被补齐。
3. **迟到的删除事件误杀健康替代者（#1957）：** 靠秒级时间戳判断名字是否被复用，存在时间窗口误差。
4. **行为依赖进程记忆，不具确定性：** 同样的集群状态，会因当前进程有没有赶上某个事件而执行不同动作。

<details>
<summary>展开详情：这些信息目前在 <code>main</code> 代码中的位置，以及原型与 <code>main</code> 的一致性测试对比</summary>

#### 各项事实在 `main`（[`4323a9bf`][main]）中的位置

| 事实 | `main` 上的存放方式 | 控制器重启或换主后会发生什么 |
| --- | --- | --- |
| 某个成员丢失了 | Informer 删除回调：[`deletePod`][main-deletepod] 调用 [`handleDeletedPod`][main-handledeleted] 删 Role 或整个组 | 离线期间错过的事件永久丢失；后续 reconcile 只看到缺口，补建 1 个 Pod |
| Group / Role 实例状态（`Creating`、`Running`、`Scaling`、`Deleting`） | 内存 [`datastore`][main-states] | 由 [`syncAll`][main-syncall] 把现存 Pod 当作 add 事件重放来重建 |
| 本次删除选中的 Pod 与 Role 清理队列 | 两个 `sync.Map`：[`podDeletions` 和 `roleCleanups`][main-maps] | 丢失；如 #2112 所述，正在删除的 Role 可能被误认为缺 entry 的健康 Role 而补建 |
| 成员自 *t* 时刻起持续异常 | [`graceMap`][main-maps] 和 [休眠 goroutine][main-grace] | 丢失；宽限期重新计时 |
| 新实例复用了旧 Pod 名 | [`recreatedSinceDeletion`][main-recreated] 按 1 秒精度比较时间戳 | #1957：迟到的删除事件可能删掉健康的新 Pod |
| 单元属于临时 surge 容量 | 每个 Pod 上的 [annotation][main-surge] | 能持久化，但可能只落在部分 Pod 上；单元没有 Pod 时无处附着 |

#### 一致性套件对比：`main` vs 原型

我们在 Kind（Kubernetes v1.37.0、Volcano 1.16.0）上用自建的 ModelServing 滚动与恢复一致性套件（[`kthena-rollout-runner`][runner]），对比了 `main`（`bc26f686`）与原型（`9ac97725`）：

- 原型共有 **778 个** 给出判定的用例：**690 通过**、**70 失败**、**18 不确定**。
- 其中 **93 个** 在 `main` 上失败、在原型上通过（**59 个**在恢复套件中）。
- **68 个** 两边都失败。
- **2 个** 在 `main` 上通过、在原型上失败（均为时序类，单独重跑均通过）。

*说明：* 这组数据测的是原型整体，不能拆分出多少收益来自 CRD、多少来自水平触发状态机重构；且测试套件及其契约是我们自定的，并非上游标准。

</details>

---

## 2. 为什么不能直接把这些状态写在 Pod 上？

大家听到这里的第一反应通常是：*“既然 #2126 能把实例 ID 打在 Pod 标签上，为什么不把 `established` 或 `deleting` 也做成 Pod 标签/注解？”*

根本矛盾在于：**“曾经建成过”和“已决定整单元删除”描述的都是成员丢失*之前*的单元状态。** 如果记录只附着在成员 Pod 身上，而成员 Pod 本身可以无痕消失，那这份记录就暴露在它所要防御的同一个故障之下。

我们检查过三种最直观的“只写 Pod”做法，每一种都能构造出反例：

- **只在 entry Pod 上打 `established` 标记** → 如果 entry Pod 自己丢了就失效（“还没建完时丢了 entry”和“建成后丢了 entry 和另一个 worker”留下的幸存 Pod 完全一样）。
- **把删第一个 Pod 当作“已提交删除”** → 如果删完 entry 后控制器崩溃、恢复前策略被改成 `None`，新主分不清是要把旧删除做完，还是在 `None` 下只补 entry。
- **逐 Pod 摘除 `surge` 标记** → 如果刚摘完 entry 控制器就崩溃，随后最后一个带标的 worker 消失，单元会被误读为已经转正，尽管最后一次摘标从未发生。

<details>
<summary>展开详情：三种“只写 Pod”方案的反例推演</summary>

| 放在 Pod 上的记录 | 打破它的时序 |
| --- | --- |
| 只在 entry Pod 上打 `established`（约定“缺 entry 但有 worker = 丢失”） | **历史 1：** entry 和 worker-1 已建，worker-2 还没建，此时 entry 被删。**历史 2：** 实例已完整建成，随后 entry 和 worker-2 一起丢失。两者最后都只剩 worker-1，但历史 1 应该继续补齐，历史 2 应该整实例重建。 |
| 把第一次 `DELETE` 当作范围提交 | 决定整单元删除 → 删掉 entry Pod → 控制器崩溃 → 用户把策略改为 `None`。新主分不清“继续完成已提交的删除”和“`None` 策略下丢了 entry，只补建 entry”。 |
| 逐 Pod 摘除 surge 标记（约定“任一 Pod 带标即视为 surge”） | 先摘掉 entry 的标记 → 控制器崩溃 → 最后一个带标的 worker 离线消失。此时看起来已转正，但最后一次写操作从未发生。 |

</details>

我们没有从数学上证明“任何写在 Pod 上的方案都不可能成立”，只验证了上述三种都会失效。能满足“证据比丢失成员活得久”这一约束的机制有三类：

1. **Pod finalizer**（拦住被删的 Pod 直到控制器记下事实）：**已排除。** 一旦控制器停止或被卸载，受保护的 Pod 会一直卡在 `Terminating`，进而阻塞 namespace 删除以及 `ModelServing` 的前台删除。
2. **成员之外的独立记录：** 放在每组一个对象里（**方案 A**）、放进 `ModelServing.status`（**方案 B**），或采用折中变体（**方案 C**）。
3. **让一个成员拥有其余成员（LeaderWorkerSet 模式）：** entry Pod 作为 owner 拥有所有 worker，entry 丢失就等同于整个单元丢失，记录可以安全放在 entry 上。当前 `ModelServing` 里 entry 和 worker 是平级兄弟（都归 `ModelServing` 管）；采用这种模式意味着**在任何策略下（包括 `None`）丢 entry 都会重建整个实例**，且依然覆盖不了跨 Role 的 `ServingGroup`。

---

## 3. 方案 A vs. 方案 B vs. 方案 C

先看基线（#2126）与 **方案 A**、**方案 B** 的总览对比（潜在的 **方案 C** 候选列在紧随其后的小节）：

| 对比维度 | 仅子对象标签（#2126） | 方案 A：`ServingGroup` CR（fork 原型评估） | 方案 B：`ModelServing.status` 账本（未实现） |
| --- | --- | --- | --- |
| **实例身份** | 是，两层统一机制 | Group：UID · Role 实例：靠规则 | 两层均靠规则 |
| **重启后保留“曾建成”** | 否 | 是 | 是 |
| **重启后保留“已提交删除”** | 按当前 Pod 和策略重推 | 是 | 是 |
| **记录不随成员丢失** | 不适用 | 是 | 是 |
| **Role 实例层怎么管** | 与组层相同机制 | 放在 `ServingGroup.status` 账本里，靠规则维护 | 与组层相同机制 |
| **跨组滚动预算** | 同现状 | 跨对象求和（非原子） | 可做成一次条件写 |
| **状态写入范围** | 无新增 status 写 | 单个组 | 整个 `ModelServing` |
| **kthena 离线时单组清理** | 等 kthena 恢复 | K8s 垃圾回收器直接完成 | 等 kthena 恢复 |
| **存量业务升级** | 不改 Pod owner | 原地接管 Pod owner、需独占切换与专门回退流程 | 不改 Pod owner；但旧二进制会抹掉不认识的 status 字段 |
| **新增 API 面** | 标签 + 1 个校验 webhook | 新 CRD、RBAC、第二个控制器 | `ModelServing.status` 新增字段 |
| **当前验证程度** | PR 评审中 | fork 原型：E2E、一致性套件、Kind 升级演练 | 仅设计估算 |

---

### 方案 A：把 `ServingGroup` 做成 CRD

**机制简介：** 新增 `ServingGroup` CRD。`ModelServing` 控制器只管 `ServingGroup` 的数量和版本；第二个控制器按组调谐并作为组内 Pod 的 owner。组级状态存在 `ServingGroup` 对象上，组内每个 `Role` 实例的状态记在 `ServingGroup.status.roleInstances[]` 账本里（阶段 `Creating`/`Established`/`Draining` + 代际号）。

为了拿到实测数据来评估这条路线，我们在 fork 上实现了一个原型：[分支][proto]（相对 `bc26f686` 变动 +51,837/−31,595 行、268 个文件）、[proposal][proto-proposal]、[设计笔记][design]（中文）。

**优势：**
- 每个组天然拥有 Kubernetes **UID**，同名重建的前后两代 owner 不同。
- 删除单个组就是对该对象执行一次前台删除——**即使 kthena 不在线，K8s 垃圾回收器也能把整组清干净**（只要 `ModelServing` 还在）。
- 状态写入和队列键按组分片，单次写入只涉及一个组。
- 运维可视化：`kubectl get servinggroup` 一行看一个组，`kubectl delete servinggroup` 直接重建单组。

**原型验证暴露出的代价与局限：**

1. **只解决了一半问题——Role 实例层依然要靠规则账本。**
   这是方案 A 最核心的结构局限：

   ```text
   ServingGroup (CRD)         ──▶ 获得 UID、独立 status、K8s 级联回收
     └─ Role 实例（仍为逻辑）   ──▶ 无 UID、无 GC；仍要靠 status 账本 + 自定义清理钩子
   ```

   因此方案 A 实际上要同时维护**两套机制**，其设计笔记也明确提到 Role 层和原地更新并不受益于这个 CRD。实战中，靠账本手工维护代际号还踩出了真实缺陷：缩容时账本项被移除（[`ledger.go`][proto-ledger-remove]），同一序号再扩回时又从第 `0` 代重新计数（[`ledger.go`][proto-ledger-gen]）；一个来自旧第 `1` 代的迟到清理钩子，把刚扩回的第 `0` 代实例的两个 headless Service 全删了（[`headless_service_plugin.go`][proto-plugin]）。我们在插件和组控制器的单测里用假 API server 复现了这一问题（真实集群里需要旧主丢租约后继续执行，目前未在真实集群看到；测试中后续轮次会补建 Service、Pod UID 不变，但集群恢复耗时和对流量的影响未测量）。修法很明确：改成每次实例化随机 ID 做等值比较（正如 #2126 所做的）。但核心教训是——**引入 `ServingGroup` CRD 并不能省掉 Role 层的身份和清理规则**，协调滚动规则也几乎原封不动搬了过去。

2. **存量升级侵入性强，且与 #2126 冲突：**
   运行中的 Pod 目前由 `ModelServing` 拥有。升级必须原地改写所有 Pod 的 controller owner、保证新旧控制器绝不重叠，并准备独立的回退流程。（我们在 Kind 上从 v1.1.0 和 `main` 演练过升级：使用占位镜像时 Pod UID 和重启次数不变，但没有真实流量数据。）此外，PR #2126 的校验 webhook 禁止修改 Pod 上指向 `ModelServing` 的 controller owner，和原地接管直接冲突，两者必改其一。

3. **双控制器交接成本，以及一个逻辑模型原本没有的新状态：**
   拆成两个控制器后，双方通过 `spec`/`status` 交换信息，彼此慢一拍，并且多出了一种状态：**`ServingGroup` 对象已存在，但它的 Pod 还没建出来。** 后期多个缺陷都源于此（例如旧 spec 晚到一步导致按旧副本数重建实例；需要加注解和信任规则判断组是否生于本次滚动；surge 组回收规则返工了三轮，因为刚建出来的 surge 组同样没有 Pod；连一致性测试套件都不得不专门适配“无 Pod 的组对象”）。

4. **跨组预算非原子、同名重建依赖 GC：**
   `maxUnavailable` 变成跨对象求和（两进程并存时可能超额花费，靠选主把窗口压小），且同名重建一个组需要等待垃圾回收完成。

<details>
<summary>展开详情：方案 A 原型的实测数据</summary>

| 指标 | 实测结果 |
| --- | --- |
| 一致性套件（`main` vs 原型） | 见第 1 节详情 |
| `make test-e2e-controller-manager` | 全部通过（50 个测试） |
| 单次 e2e 的 API 写入量 | 164 个 `ServingGroup` 共更新 **1,573** 次，同期 51 个 `ModelServing` 更新 **668** 次（**2.35 倍**）。其中 1,427 次是 `status` 写入，对应 360 个 Pod（约每个 Pod 全生命周期 4 次 status 写） |
| 单个 `ServingGroup` 对象大小（≤ 4 个 Role 实例） | 1.3–1.6 KB；单条账本项 48–63 字节（紧凑 JSON，不含 `managedFields`） |
| 单组大量 Role 实例 | 未测试（测过的最大组只有 6 个实例） |
| 多组并行 vs 单队列键收益 | 未测量，暂无数据证明收益 |

</details>

---

### 方案 B：保持 `ServingGroup` 为逻辑概念，把账本放进 `ModelServing.status`

**机制简介：** 保留单个控制器、Pod 仍归 `ModelServing` 所有，在 `ModelServing.status` 里放一张表，记录每个 Role 实例和必要的组级状态。*（尚未实现。）*

**优势：**
- **无需迁移 Pod owner**（但升级仍涉及 schema 变更和新旧二进制并存处理，见下）。
- **单写者、单快照：** 没有双控制器交接延迟，也没有“对象在但 Pod 不在”的中间态。
- **跨组预算可原子化：** 跨组扣减滚动预算可以做成一次条件写。

**代价与风险：**
- **规模放大下的对象体积与写入平方增长：** 每次状态变化都要重写整张表并推送给所有 watch 该 `ModelServing` 的客户端。当前一个 `ModelServing` 仅约 2 KB；即便到了 1,000 个 Role 实例，单对象体积仍远小于 etcd 上限（纯账本约 48–63 KB，计入 `managedFields` 估算约 128–153 KB），但**一次全量滚动的总写入字节数随实例数呈平方增长**——在简单模型下，几十个实例处就超过方案 A，300 个实例时约 6 倍，1,000 个实例时约 20 倍。*（仅为估算，方案 B 尚未实测。）*
- **全局共用一个 `resourceVersion`：** 所有组竞争同一个版本，要么单 worker 串行写，要么多写者冲突重试。
- **旧版本二进制会静默抹掉账本：** 不认识新字段的旧控制器在调用 `UpdateStatus` 时会把账本字段丢弃；因此零值必须落在安全侧，且新旧版本并存或回退时，进行中的已提交删除仍可能被打断。
- **两层都靠规则，且与 #2112 目标冲突：** 没有组 UID 可兜底，且 #2112 明确提出希望避免往 `ModelServing.Status` 增加写入和 schema 变更。

<details>
<summary>展开详情：<code>ModelServing.status</code> 体积估算与滚动写入量模型</summary>

按方案 A 原型实测的单条账本大小（48–63 字节紧凑 JSON，不含 `managedFields`）加 `managedFields` 估算：

| 单个 `ModelServing` 内的 Role 实例总数 | 仅账本 | 含 `managedFields`（估算） |
| --- | --- | --- |
| 100 | 5–6 KB | 13–15 KB |
| 300 | 14–19 KB | 38–46 KB |
| 1,000 | 48–63 KB | 128–153 KB |

**写入量对比模型假设：** 假设方案 B 每个实例有 2 次无法合并的账本写、每实例 2 个 Pod；方案 A 采用 e2e 实测的每 Pod 约 4 次 status 写与小组对象大小。在该模型下，方案 B 在几十个实例处超过方案 A，300 实例约 6 倍，1,000 实例约 20 倍。实际批量合并写入与单组规模会显著改变数值；方案 B 尚未实现和实测。

</details>

---

### 方案 C：是否存在第三种选择？

既然方案 A 和方案 B 各有明显硬伤，我们也想向社区抛出一个开放问题：**是否存在更适合 Kthena 的方案 C？** 下面是四个我们**尚未设计、未测试**的候选方向，抛砖引玉或一起排除：

| 方案 C 候选 | 核心思路 | 避开了什么 | 主要代价 |
| --- | --- | --- | --- |
| **C1. 只做记录的组 CR** | 每组一个对象、owner 是 `ModelServing`，仍由**单个** `ModelServing` 控制器读写，**不拥有 Pod** | 避开方案 A 的 Pod owner 迁移与双控制器，同时避开方案 B 的父 status 写入放大 | 仍需引入新 CRD；放弃 kthena 离线时单组的 K8s 级联回收 |
| **C2. 以 entry Pod 为锚点的 Role 实例（LWS 风格）** | entry Pod 拥有 worker Pods、提供 UID、承载记录，前台删 entry 即提交删除 | Role 实例层彻底省掉账本 | **任何策略下（含 `None`）丢 entry 都会重建整个实例**；无法覆盖跨 Role 的 `ServingGroup` |
| **C3. 压缩账本（Indexed Job 风格）** | 在 status 中把稳态下的 `established` 实例压缩为连续区间 | 大幅缩小稳态 status 体积 | 与“记录需绑定到具体哪一代实例”存在冲突 |
| **C4. 分片账本对象** | 把状态表拆到多个辅助对象上 | 缓解单对象写入放大 | 再往前走一步其实就是 C1（只做记录的组 CR） |

如果大家有其他 **方案 C** 的思路（或是以上思路的组合，又或者认为可以接受重启缺口、止步于 #2126 + 状态机重构），非常欢迎提出讨论。

---

## 4. 同类项目是怎么处理的？（RBG、Grove、LeaderWorkerSet）

我们阅读了三个同样管理“同名复用多 Pod 单元”的开源项目源码、文档和 issue：**RoleBasedGroup**（[`sgl-project/rbg`][rbg] @ `a19d2c83`）、**NVIDIA Dynamo Grove**（[`ai-dynamo/grove`][grove] @ `15411d9c`）和 **LeaderWorkerSet / DisaggregatedSet**（[`kubernetes-sigs/lws`][lws] @ `1cb7505e`）。*（静态阅读，未实际运行；逐行代码引用的详细笔记见 [`modelserving-unit-state-discussion-peers.md`](modelserving-unit-state-discussion-peers.md)。）*

### 五条核心启示

三家项目都踩到了完全相同的三类问题，而且很多都是最近几个月的 issue：

1. **给每一层都建 CRD，并没有自动消除这些问题。** RBG 每一层都是 CRD，但控制器离线时丢 Pod 依然只补不重建（[rbg#562][rbg562]、[rbg#566][rbg566]），默认路径也依然是先删后写状态。在我们读过的代码路径里，真正堵住缺口的是三件事：比丢失成员活得久的“曾建成”记录、破坏性写入带 UID 前置条件、先记再删。
2. **每家项目在对象之下依然有一层靠规则管理**——RBG 的 components、Grove 的 replica 序号、LWS 的 workers——和方案 A 里的 Role 层处境一致。
3. **LWS 靠“一个成员拥有其他成员”省掉了账本。** 它的 leader Pod 拥有 worker StatefulSet、提供 UID、存放记录，并靠前台删除 leader + GC 完成收尾。这之所以成立，是因为在 LWS 里丢 leader 就等于丢整组（映射到 ModelServing 就会改变 `None` 策略下丢 entry 的语义），而且管不到跨 Role 单元。另外，LWS 维护者也基于和我们同样的理由否决了给 worker 加 finalizer（[lws#1319 评审][lws1319-review]）。
4. **三家都没有在父对象 status 里存全量逐单元账本。** Grove 只放进行中的选择；LWS 仅为可选的重启预算/退避功能在每个 LWS 的 annotations 里存两个 JSON map（4,000 组约 221 KiB，紧贴 256 KiB 上限）。这印证了我们对方案 B 规模的顾虑。
5. **时间戳启发式在实践里踩坑，且读过的迁移都避开了原地改运行中 Pod 的 owner。** Grove 用持久化 `lastScheduled` 替换了误杀健康副本的时间戳启发式（[grove#789][grove789]）；在我们读过的历史迁移里，没有一家去原地改运行中 Pod 的 owner（RBG 靠重建，Grove 只改 metadata，LWS 靠滚动替换；我们未穷尽三家全部历史）。

### 三家项目速查表

| 维度 | RoleBasedGroup (RBG) | NVIDIA Dynamo Grove | LeaderWorkerSet / DisaggregatedSet |
| --- | --- | --- | --- |
| **对象层次** | 每一层都是 CRD（`RBGSet` → `RBG` → `RIS` → `RI`） | `PCS`、`PCSG`、`PodClique`、`PodGang`、`PodGangMap` 为 CRD；但 gang 终止实际重建的 **replica 序号**不是对象 | `LWS` 与 `DisaggregatedSet`；组由 leader Pod 拥有 worker StatefulSet 组成；`DisaggregatedSet` 无跨 Role 单元（每个 slice/rev/role 一个 LWS） |
| **实例身份** | 按 owner UID 列 Pod + 同名旧 owner 阻塞创建（[rbg#496][rbg496]、[rbg#511][rbg511]）。重启/扩缩删 Pod 带 UID 前置条件，但原地更新回退和删 `RoleInstance` 不带 | 按 owner UID 读 + 随机 Pod 名。删除不带 UID 前置条件（[代码注释][grove-retry]接受重试删除误伤新 `PodClique`）。按名字做 key 两次出过问题（[grove#782][grove782]、[grove#943][grove943]） | leader Pod 的 UID；队列键带 UID（[lws#998][lws998]）。按复用槽位计数曾分不清代际（[lws#1247][lws1247]）；v1.0.0 后陆续补了多个 UID 前置条件 |
| **“曾建成”记录** | 持久字段 `RoleInstance.status.lastReadyGeneration`，但仅从现存 Pod 检测丢失——离线丢失仍只补不重建（[rbg#562][rbg562]、[rbg#566][rbg566]） | 持久字段 `PodClique.status.lastScheduled` 替代了时间戳启发式（[grove#789][grove789]，贡献者称“显式持久证据更安全”）。scaling group 仍用“创建后 5 秒” | 此前没有，离线丢 worker 不会重建组（[lws#1301][lws1301]）。`main` 刚通过 [lws#1319][lws1319] 在 leader Pod 上记 worker UID 摘要与 `started` 标记（尚未发版） |
| **已提交删除** | 仅开启 `maxGroupRestarts` 时先记后删；默认路径先删后写 status | 缩容/升级选择先写后删；gang 终止按代码原话是 *"action-first / flag-second"*（接受崩溃后多一次重建） | 带 UID 前置条件前台 `DELETE` leader Pod，GC 收尾 |
| **父对象逐单元状态** | 无；代价转移成跨层读写（[rbg#520][rbg520]） | 仅存进行中的缩容/升级序号（原 `PodGang` 列表未用已删）。[grove#781][grove781] 报告 churn 基准退化约 35%，指向每个新子对象多了一次 status 写入 | 每个 LWS 两个 JSON annotation map（按版本和组序号记重启次数与记账 leader），仅用于可选预算/退避（4,000 组约 221 KiB vs 256 KiB 上限） |
| **对象模型迁移** | 靠重建（有无 Pod 窗口，设计上不提供迁移工具） | 通过 status 门控做原地 metadata patch | 按组滚动；`DisaggregatedSet` 绝不接管已有 LWS |
| **协调滚动** | role 依赖与 `maxSkew` 间有卡住 PR（[rbg#638][rbg638]） | 卡住与过早报完成（[grove#786][grove786]、[grove#916][grove916]、[grove#952][grove952]） | 每次 reconcile 按子对象重算；存在未关闭死锁（[lws#1292][lws1292]）；无专门 stalled 信号 |

---

## 5. 不管选方案 A、B 还是 C，哪些改进可以先落地？

下面五件事不依赖方案 A、B、C 的最终选型，可以先行推进：

1. **实例身份（PR #2126）：** 我们可以用整理好的边界用例协助评审 #2126 并补充测试。
2. **水平触发的 reconcile 结构重构（无 API 变更）：** 虽然尚未单独拆分测量，但我们认为原型在一致性测试上的大部分收益来自两项结构改进，而不是 CRD 本身：
   - Informer 回调**只负责入队**，不再直接改集群状态；
   - 每轮 reconcile 基于当前对象快照用纯函数算出要执行的写操作：组和 Role 实例共用一套规划器（[`grouplevel`][proto-grouplevel]，已用参考模型的 17,504 组向量校验），单个 Role 实例由纯状态转移函数（[`ledger.go`][proto-ledger]）根据抽象状态记录、观测成员和策略计算动作。
   这部分可以作为现有控制器的内部重构落地——状态先留在原位、**零 API 变更**，未来若确定持久化载体可以直接接进去。由于改动量较大，发系列 PR 前希望先和社区达成共识。
3. **协调 Role 滚动的状态上报边界问题：** 通过阅读 [`role_rolling_update_coordinator.go`][main-coord] 并在原型移植的同套规则上运行，我们看到两个“实际不再替换任何实例，却报可推进或已完成”的场景：
   - *(a)* 低序号旧实例未就绪但未触发故障恢复，占满 `maxUnavailable` 预算，同时降序替换规则不允许越过它去换高序号健康实例——但状态仍报 `ProgressAvailable`；
   - *(b)* 某个 Role 在基线和目标版本间的模板相同，但仍有一个实例跑在中间版本上——此时该 Role 的 `inProgress` 为 `false`、原因为 `RolloutComplete`，而 [校验 webhook][main-webhook] 会据此判定“当前没有进行中的协调滚动”。
   *（两者目前尚未在 `main` 或真实集群上复现，复现后会单独提 issue。）*
4. **Watch 丢失 `DELETED` 事件：** 如果正在排空的 Pod 丢了 watch 删除事件，informer 缓存会一直留着这个 Pod，导致单元卡死在删除态（`main` 和原型都会遇到）。处于等待 Pod 消失阶段的单元需要定期直读 API 核对。
5. **共享的一致性测试套件：** 无论未来对比哪种方案，都需要双方认可的用例集。我们可以提供 [`kthena-rollout-runner`][runner] 及其用例作为起点（其背后的行为契约目前由我们定义，需要社区一起评审）。

---

## 6. 我们的当前看法与待讨论问题

### 我们的立场

- **方案 A 和方案 B 目前都没有明显压倒另一方的优势**——两者各有不小的结构成本，这也是我们在提交任何 CRD 或 status schema 提案之前，想先和社区一起讨论 **方案 A vs. 方案 B vs. 潜在方案 C** 的原因。
- **实例身份适合用子对象上的随机每代 ID**（PR #2126），我们认同这个方向，也会放弃原型里的代际计数器。
- **方案 A、B、C 的取舍强依赖于保障边界和目标规模。** “曾建成”和“已提交删除”的证据必须比丢失成员活得久（放在 Pod 之外，或者靠一个成员拥有其他成员但改变丢 entry 的语义）。方案 A 太重且 Role 层仍需账本；方案 B 引入更轻但在大实例数下放大写入。听取社区的预期规模以及对方案 C 的想法，对收窄方向非常关键。
- **倾向的推进节奏：** 先落地第 5 节中不依赖载体的改进，有了更清晰的需求、规模预期和方案 C 讨论后再敲定持久化方案。

### 希望社区反馈的六个问题

1. **重启后保障边界：** 控制器离线期间丢失成员是否必须严格按 `recoveryPolicy` 恢复？重启前已决定的整单元删除是否必须在重启后继续完成？还是认为这些缺口可以接受？
2. **方案 A vs. 方案 B vs. 方案 C：** 如果需要上述保障，社区认为哪条路更适合 Kthena：**方案 A**（`ServingGroup` CR）、**方案 B**（`ModelServing.status`）、上面的某个 **方案 C** 候选（如只做记录的组 CR、entry Pod 锚定），还是我们没想到的其他 **方案 C**？
3. **组 CR 本身的运维价值：** 抛开恢复状态不谈，社区本身是否想要 `ServingGroup` CR（例如用 `kubectl` 按组查看/删除/重建，或在 kthena 离线时由 GC 清理单个组）？
4. **设计目标规模：** 设计应按单个 `ModelServing` 多少个 `ServingGroup`、多少个 `Role` 实例来假设？
5. **先做水平触发重构：** 在问题 1–4 尚未定论前，社区是否欢迎以系列 PR 的形式把 `ModelServing` 控制器重构为水平触发的规划器 + 状态机？
6. **Role 实例需要 CRD 吗？** 有人觉得 Role 实例也应该做成 CRD 吗？*（我们认为不需要。）*

---

## 7. 环境、参考链接与说明

- **Kthena：** `main` 固定在 [`4323a9bf`][main]（一致性对比使用 `main` @ `bc26f686` vs 原型 @ `9ac97725`）。
- **测试环境：** Kind，Kubernetes v1.37.0，Volcano 1.16.0。
- **原型资料：** [代码分支][proto]、[proposal][proto-proposal]、[实现设计][proto-design]（中文）、[设计笔记][design]（中文）。
- **一致性测试工具：** [`kthena-rollout-runner`][runner]。
- **同类项目调研附录：** [`modelserving-unit-state-discussion-peers.md`](modelserving-unit-state-discussion-peers.md)。
- **相关 Issue / PR：** #1957、#2112、#2126、#1999、#1950。

*AI 协助说明：* 原型与本分析的部分内容在 AI 协助下完成。所有测量数据均来自 Kind 集群实测和导出的集群对象，所有代码引用均可在固定提交上核验；未复现或未测量的条目均已明确标注。

[main]: https://github.com/volcano-sh/kthena/tree/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89
[main-deletepod]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/controller/model_serving_controller.go#L462-L509
[main-handledeleted]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/controller/model_serving_controller.go#L2355-L2386
[main-recreated]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/controller/model_serving_controller.go#L511-L529
[main-syncall]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/controller/model_serving_controller.go#L791-L810
[main-maps]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/controller/model_serving_controller.go#L114-L119
[main-grace]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/controller/model_serving_controller.go#L2250-L2322
[main-states]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/datastore/store.go#L84-L97
[main-surge]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/controller/coordinated_role_observation.go#L179
[main-coord]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/controller/role_rolling_update_coordinator.go#L717
[main-webhook]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/webhook/role_coordination_validator.go#L284
[proto]: https://github.com/VanderChen/kthena/tree/98fcbf01b93c812539d0651e20b20a790a7a4bb1
[proto-proposal]: https://github.com/VanderChen/kthena/blob/98fcbf01b93c812539d0651e20b20a790a7a4bb1/docs/proposal/modelserving-servinggroup-cr.md
[proto-design]: https://github.com/VanderChen/kthena/blob/98fcbf01b93c812539d0651e20b20a790a7a4bb1/docs/proposal/modelserving-servinggroup-cr-design.zh-CN.md
[proto-ledger]: https://github.com/VanderChen/kthena/blob/98fcbf01b93c812539d0651e20b20a790a7a4bb1/pkg/model-serving-controller/servinggroup/ledger.go#L226-L328
[proto-ledger-gen]: https://github.com/VanderChen/kthena/blob/98fcbf01b93c812539d0651e20b20a790a7a4bb1/pkg/model-serving-controller/servinggroup/ledger.go#L72-L81
[proto-ledger-remove]: https://github.com/VanderChen/kthena/blob/98fcbf01b93c812539d0651e20b20a790a7a4bb1/pkg/model-serving-controller/servinggroup/ledger.go#L263
[proto-plugin]: https://github.com/VanderChen/kthena/blob/98fcbf01b93c812539d0651e20b20a790a7a4bb1/pkg/model-serving-controller/plugins/headless_service_plugin.go#L260-L284
[proto-grouplevel]: https://github.com/VanderChen/kthena/tree/98fcbf01b93c812539d0651e20b20a790a7a4bb1/pkg/model-serving-controller/grouplevel
[design]: https://github.com/VanderChen/kthena/tree/d5686ba8e7e57bf143c1deb1b63b89be93059d32/docs/proposal/modelserving-servinggroup-cr
[runner]: https://github.com/VanderChen/kthena-rollout-runner/tree/6b0af07ec21926824b0d813456985e736831d9c9
[rbg]: https://github.com/sgl-project/rbg/tree/a19d2c83fc1aca4f57a23cf7eee56f687e2da023
[rbg496]: https://github.com/sgl-project/rbg/issues/496
[rbg511]: https://github.com/sgl-project/rbg/issues/511
[rbg520]: https://github.com/sgl-project/rbg/issues/520
[rbg562]: https://github.com/sgl-project/rbg/issues/562
[rbg566]: https://github.com/sgl-project/rbg/issues/566
[rbg638]: https://github.com/sgl-project/rbg/pull/638
[grove]: https://github.com/ai-dynamo/grove/tree/15411d9ccb37cfbae9e7cded612762e1ad41777c
[grove-retry]: https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/components/podcliquesetreplica/gangterminate.go#L302-L304
[grove781]: https://github.com/ai-dynamo/grove/issues/781
[grove782]: https://github.com/ai-dynamo/grove/issues/782
[grove786]: https://github.com/ai-dynamo/grove/issues/786
[grove789]: https://github.com/ai-dynamo/grove/issues/789#issuecomment-5395204069
[grove916]: https://github.com/ai-dynamo/grove/issues/916
[grove943]: https://github.com/ai-dynamo/grove/issues/943
[grove952]: https://github.com/ai-dynamo/grove/issues/952
[lws]: https://github.com/kubernetes-sigs/lws/tree/1cb7505e8bfe98dd0b240439c12436d52deb500e
[lws998]: https://github.com/kubernetes-sigs/lws/issues/998
[lws1247]: https://github.com/kubernetes-sigs/lws/issues/1247
[lws1292]: https://github.com/kubernetes-sigs/lws/issues/1292
[lws1301]: https://github.com/kubernetes-sigs/lws/issues/1301
[lws1319]: https://github.com/kubernetes-sigs/lws/pull/1319
[lws1319-review]: https://github.com/kubernetes-sigs/lws/pull/1319#discussion_r4663692205
