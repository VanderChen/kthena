# ModelServing 滚动实现检视意见

## 0. 文档信息

| 项 | 内容 |
| --- | --- |
| 状态 | 草案。行为规范的 D1（转正并清除标记）、D2（目前保持占用）、D3（立即应用）已由维护者在 2026-11-12 决定，本文按这三项对照。本文只记录检视结论，没有修改任何代码 |
| 检视对象 | `volcano-sh/kthena` `main` @ `b4e536bb`，`pkg/model-serving-controller/` 下的 controller、datastore、utils，以及 webhook 中与滚动预算有关的部分。plugin 的实现不在范围内 |
| 对照的规范 | 现规范：`kthena-rollout-runner` `main` @ `3929bf5` 的契约 2.7。修订规则：[行为规范修订草案](modelserving-rollout-behavior-spec.md)的 R1～R38 |
| 配套文档 | [状态设计](modelserving-rollout-state-design.md) |

### 0.1 方法

白盒走读，加单元级探针。走读分四块：ServingGroup 路径与事件队列；Role 路径与 roleCoordination；InPlace；故障恢复、重启与 revision 历史。后三块由 subagent 完成。另有一个 subagent 把 35 个组合场景逐个在真实的 `syncModelServing` 上跑了一遍（47 个探针）。报告都在任务目录 `issues/features/005-modelserving-rollout-spec-state-review-IP/subagents/` 下。

每条意见标注证据类型：

| 标注 | 含义 |
| --- | --- |
| 探针 | 有单元级探针的输出佐证。探针用 fake clientset，直接调用 controller 内部函数，证明的是「给定这份状态，代码会做出这个决定」。源码和输出在任务目录 `probes/` 下，本文写作时全部重跑过 |
| 走读 | 只凭读代码得出 |
| 已有单测 | 仓库里现有的单测直接断言了这个行为 |
| Kind | 有真实集群上的观察，注明来源和当时的 commit |
| 未验证 | 走读认为成立，但需要探针或 Kind 才能确认 |

本任务没有在 Kind 上运行任何东西。

### 0.2 严重度

| 级别 | 含义 |
| --- | --- |
| P0 | 滚动动作让 Ready 低于 `N − U`，或让容量超过 `N + S` |
| P1 | 行为与规范不一致（过程或终态不同），或者会卡住、需要人工介入 |
| P2 | 健壮性问题：时序、重启、事件丢失下的错误或滞后；只造成浪费的偏离 |
| P3 | 可读性和结构 |

行为与规范有出入的意见，另外标出它相对两份规范的位置。标题括号里只有严重度的意见是健壮性、结构或规范没有涉及的问题，不涉及这个位置。标题里的「未验证」是证据类型，不是规范位置。

| 标注 | 含义 |
| --- | --- |
| 两者 | 现规范和修订规则都不允许这个行为 |
| 分歧 | 实现是有意这样做的，和规范的口径不同。口径已经定下来的，在意见里写明 |
| 符合 | 只用在 ORD-3 上：它记录的是一处符合规范的行为，不是缺陷 |
| 缺口 | 规范没有覆盖，或规范自己有问题 |

文中的代码位置沿用状态设计文档的缩写：`msc.go`、`surge.go`、`obs.go`、`coord.go`、`ipu.go`、`aux.go`、`store.go`，都在 `pkg/model-serving-controller/` 下。

## 1. 结论

实现在三个方面做得扎实：模板比较是三态的，读不到历史时既不删除也不当作可用；删除用一次性的 UID 快照，重试不会扩大到同名的新 Pod；独立 Role 模式的预算把总额度和健康额度分开，附录 B 里能构造的 11 行查表逐行一致。完整列表见[第 4 节](#4-做得好的地方)。

问题集中在五处。

1. **SG 模式的预算只有一个额度。** 它没有「删除健康单元后 Ready 不得低于底线」这一道检查，受 partition 保护的 NotReady 组也不计入不可用。两个探针里 Ready 都被滚动压到了 `N − U` 以下（BG-1、BG-2）。其中 BG-1 是有意的设计，有单测断言。D2 已定为目前保持占用，按这个口径它需要改。
2. **阶段没有显式表达，组合场景因此出错。** 缩容没结束就开始替换（BG-3）；稀疏布局下 surge 容量落进低位洞，滚动结束后空洞从高位移到了 0 号（ORD-1）；滚动途中缩容先删 surge（ORD-2）。
3. **基线版本推进得太早。** 缩容删掉全部旧版本单元后，基线被推进到还没全量完成的目标版本，之后受保护的槽位按新版本重建（VER-1）。
4. **协调模式有四个会走偏或卡住的路径**（CO-1～CO-4）。CO-1、CO-2、CO-4 有探针。CO-3 是规范自己的缺口，结论来自对限额函数的推演，没有在完整的 reconcile 路径上复现。
5. **故障恢复和 InPlace 依赖进程内状态和特定事件**，重启、配置变更、事件乱序时会出错。REC-1～REC-3 是走读结论，IP-1～IP-3 有探针；REC-4、REC-5 走读认为成立，还没有验证。

35 个组合场景里，当前实现有 26 个符合修订规则，7 个部分符合，2 个不符合（SG-C12、SG-P09）。逐场景的结果见 [2.2](#22-场景对照)，都是单元级探针的结论。

带严重度的意见共 49 条：P0 2 条，P1 18 条，P2 16 条，P3 13 条。另有 2 条不定级（ORD-3 记录的是一处符合规范的行为，CO-5 是规范没有覆盖的地方），以及 4 条关于测试的意见。

建议的处理顺序见[第 5 节](#5-建议的整改顺序)。

## 2. 规范符合度

### 2.1 修订规则逐条对照

「符合」表示没有发现偏离，不表示已被完整验证；证据类型见对应的意见。相关意见里的探针大多是 SG 模式的。独立 Role、协调 Role 两列，除了 ORD-1、ORD-5、ORD-6、CO-1、CO-2、CO-4 和附录 B 的 11 行查表有 Role 路径的探针之外，都是走读结论。

| 规则 | 内容 | SG 模式 | 独立 Role | 协调 Role | 相关意见 |
| --- | --- | --- | --- | --- | --- |
| R1 | 正式单元取 `[0, N)` 最低空闲序号 | 部分 | 部分 | 符合 | ORD-1 |
| R2 | surge 只建在 ≥ N 并带标记 | 部分 | 部分 | 符合 | ORD-1 |
| R3 | 被 N 覆盖的 surge 清除标记并转正 | 符合 | 符合 | 符合 | ORD-3（符合） |
| R4 | 只有显式缩容产生空洞 | 不符合 | 不符合 | 符合 | ORD-1、ORD-4 |
| R5 | 不为整理序号重建健康单元 | 符合 | 符合 | 符合 | — |
| R6 | 基数、取整、拒绝 U 和 S 同时为 0 | 部分 | 部分 | 部分 | BG-5、EV-3。取整符合；全部受保护时拒绝，零副本时不检查；运行中解析成双零时不报告 |
| R7 | 容量上限 | 符合 | 符合 | 符合 | — |
| R8、R9 | 两个额度 | **不符合** | 符合 | 符合 | BG-1、BG-2、BG-4 |
| R10 | 逐个 Ready 重算 | 符合 | 符合 | 符合 | — |
| R11(a)、R12、R13 | 候选与顺序 | 符合 | 符合 | 符合 | — |
| R14 | 连续提交按最新目标重判 | 符合 | 符合 | 不符合 | CO-1、CO-2 |
| R15 | 创建时按槽位选版本 | 符合 | 不符合（新建 SG 时） | 不符合（同左） | ORD-6 |
| R16 | partition 只限制模板更新 | 符合 | 符合 | 符合 | — |
| R17 | 基线只在全量完成时推进 | **不符合** | 不符合 | 部分 | VER-1 |
| R18 | 局部修复沿用已应用版本 | 符合 | 符合 | 符合 | —（原地更新中的重建见 R37、IP-2） |
| R19 | 历史读不到时等待 | 符合 | 符合 | 符合，但整轮中止 | VER-3 |
| R20 | 缩容只选正式单元，先 NotReady | 部分 | 部分 | 部分 | ORD-2、ORD-5 |
| R21 | 扩容补最低洞 | 符合 | 符合 | 符合 | — |
| R22 | SG 模式 Role 副本数变更立即应用 | 符合 | — | — | — |
| R23、R24 | 阶段顺序与缩容门控 | **不符合** | 不符合 | 部分 | BG-3 |
| R25～R27 | surge 的创建与回收 | 符合（无洞时） | 符合（无洞时） | 符合 | ORD-1 |
| R28、R29 | 完成与阻塞的判定和报告 | 部分 | 部分 | 部分 | EV-3 |
| R30、R31 | Role 同构与协调 | — | 符合 | 部分 | CO-1～CO-6 |
| R32～R34 | 恢复与重启 | 部分 | 部分 | 部分 | REC-1～REC-7 |
| R35 | 原地更新的适用条件 | 符合 | 符合 | 符合 | IP-1 |
| R36 | 原地更新中的单元计入 `notReadyTarget` | 部分（隐式） | 部分（隐式） | 符合 | BG-4 |
| R37 | 原地更新中重建用目标版本、仍受预算约束 | 不符合 | 不符合 | 未验证 | IP-2、BG-6 |
| R38 | 回退为重建时照常用 surge | 不符合 | 不符合 | 未验证 | IP-5（分歧） |

### 2.2 场景对照

每一行的「当前实现」都来自探针：在 fake clientset 上驱动真实的 `syncModelServing`，按场景脚本投递 spec 变更、Ready、删除完成这几种事件，记录每一步的单元、UID 和 status。探针源码和输出在 `probes/behavior-regression/`，探针名是 `TestProbe005BR_` 加场景号。这不是 Kind 证据：没有 kubelet 和调度器，每组只有一个单 Pod 的 Role，事件按确定的顺序投递。

结论的口径：**符合**指探针轨迹是修订规则允许的一条过程，每一步的删除对象、创建的序号和版本、UID、Ready 下限、活动数上限都一致。**部分符合**指终态、UID 和两条界限一致，中间步骤不同，或者主表一致而正文里的分支不一致。**不符合**指终态、UID、Ready 下限三者至少有一项不同。

| 场景 | 当前实现与修订规则的差别 | 结论 | 意见 |
| --- | --- | --- | --- |
| SG-S01、S02 | 无 | 符合 | — |
| SG-S03 | 主表相同。带 `maxSurge=1` 时额外容量建在洞 1、不带标记，`sg-0` 删除后不再重建，终态 `{1, 3}` | 部分符合 | ORD-1 |
| SG-S04 | 场景的初始状态走不出来：从 `{0, 1, 3}` 开始滚动时额外容量直接建在洞 2、不带标记，不会出现 `sg-4`。此后扩到 4 的终态相同；不扩容时终态是 `{1, 2, 3}` | 部分符合 | ORD-1 |
| SG-S05、S06、S07 | 无 | 符合 | — |
| SG-C01 | 无 | 符合 | — |
| SG-C02 | 缩容对象 `sg-2` 和滚动对象 `sg-1` 在同一轮进入删除。Ready 降到 1，等于底线 | 部分符合 | BG-3 |
| SG-C03 | `sg-3` 转正，标记被清除，UID 不变，5 次创建、4 次删除。与现规范相同 | 符合 | ORD-3 |
| SG-C04 | 无 | 符合 | — |
| SG-C05 | 新版本到来的那一轮就删 `sg-2`，此时 `sg-3`、`sg-4` 还在删除 | 部分符合 | BG-3 |
| SG-C06 | 无 | 符合 | — |
| SG-C07 | `sg-4`、`sg-3`（缩容）和 `sg-2`（滚动）在同一轮进入删除 | 部分符合 | BG-3 |
| SG-C08、C09 | 无 | 符合 | — |
| SG-C10 | `sg-3` 还在删除时就删 `sg-2` | 部分符合 | BG-3 |
| SG-C11 | 无 | 符合 | — |
| SG-C12 | 同一轮里 `sg-4`（Ready 的 surge，被缩容选中）和 `sg-3`（被滚动选中）一起删除，Ready 从 5 降到 3；之后在 3 号重建 surge。4 次创建、6 次删除，规范是 3 和 5 | **不符合** | ORD-2 |
| SG-C13～C16 | 无 | 符合 | — |
| SG-P01、P02 | 无 | 符合 | — |
| SG-P03 | 两张表都相同。场景之外的两个变体不同：P 大于 0 时缩容后剩下的组全是新版本，或者缩到 0，基线都被推进 | 符合 | VER-1 |
| SG-P04 | 无 | 符合 | — |
| SG-P05 | `sg-3`、`sg-4` 还在删除时就删 `sg-2` | 部分符合 | BG-3 |
| SG-P06、P07 | 无 | 符合 | — |
| SG-P08 | `sg-3` 转正，只建 `sg-4`，1 次创建。与现规范相同 | 符合 | ORD-3 |
| SG-P09 | 删除健康的 `sg-2`，Ready 降到 1，底线是 2 | **不符合** | BG-1 |
| SG-P10 | `sg-3` 转正，3 次创建、3 次删除。与现规范相同 | 符合 | ORD-3 |
| SG-P11 | 无。停点的 `currentRevision`、`updatedReplicas` 也一致 | 符合 | — |
| SG-R01 | 无。一轮里三个组同时增员，`availableReplicas` 从 3 降到 0；受保护的组用旧模板。这与现规范不同，与修订规则 R22 相同 | 符合 | — |

合计：26 个符合，7 个部分符合，2 个不符合。

Role 模式下的同构场景没有逐个跑。独立 Role 模式有一个稀疏布局的探针（ORD-1），协调模式的 surge 走的是另一段代码，没有覆盖。

## 3. 意见明细

### 3.1 预算与可用性

#### BG-1 SG 模式下受保护的 NotReady 组不占预算，健康删除没有底线检查（P0，分歧，D2 已定为保持占用）

- **位置**：`msc.go:1765-1783`（不可用计数）、`msc.go:1822-1827`（额度）、`msc.go:1858-1878`（删除循环）。
- **现象**：`maxScaleDown = 组数 − (N − U) − unavailable`。受保护的组只有在「非 Running 且模板未知」时才计入 `unavailable`；非 Running 但历史已知的受保护组不计。删除循环只看 `maxScaleDown`，没有 `Running 数 − minAvailable` 这道上限。
- **探针**（`TestProbe005_P09_ProtectedNotReady`）：N=3、P=1、U=1，sg-0 旧版本 NotReady，sg-1、sg-2 旧版本 Running。一次滚动步骤后 sg-2 被删，Running 从 2 降到 1，底线是 2。InPlace 路径相同（`probes/whitebox-inplace` 的 P4：N=4、P=2、U=1，Running 剩 2，底线 3）。
- **已有单测**：`TestServingGroupRollingUpdateIgnoresUnavailableProtectedGroups`（`controller/model_serving_controller_test.go:8556-8599`）断言的正是这个行为：N=3、U=0、S=1、P=2，sg-0 NotReady，期望 sg-2 被删，Running 从 3 降到 2，底线是 3。所以这是有意的设计。
- **规范**：R8、R9；现规范 API §2.3、SG-P09。
- **为什么是问题**：
  - 开发者文档写的是 `N_available ≥ replicas − maxUnavailable`（`docs/kthena/docs/developer-guide/model-serving-rolling-update.md`），实现不满足自己的文档。
  - 实现内部不一致。这个函数调用的 surge 回收函数里专门取了 `min(budget, Running 数 − minAvailable)`，注释写着「The budget does not see unready groups protected by partition」（`surge.go:209-216`）；Role 模式的预算有健康额度（`obs.go:112-127`）。只有 SG 模式删旧组这一条路径没有。
  - 重启收敛用例 RC-01、RC-02 预计因此不通过：低位受保护组补齐之前，高位健康组就会被滚动，Ready 从 1 降到 0（走读推演，未在 Kind 上验证）。
- **口径**：这是行为规范的 D2，已定为目前保持占用，所以这一条按缺陷处理。Kubernetes StatefulSet 在 2026-10-30 改成了不计 partition 以下的不可用 Pod（[kubernetes/kubernetes#143042](https://github.com/kubernetes/kubernetes/issues/143042)、[#143114](https://github.com/kubernetes/kubernetes/pull/143114)，里程碑 v1.38），同时把百分比的基数改成了 `replicas − partition`。当前实现只做了前一半，基数仍是 N，所以它和上游改完之后的做法也不一样。上游的理由和以后重新评估的条件记在行为规范 D2 里。
- **建议**：在删除循环里加健康额度 `Running 数 − minAvailable`，和 Role 模式、surge 回收保持一致；改写上面那条单测。受保护的 NotReady 组因此占用健康删除额度。它仍然不计入 `unavailable`，滚动区里 NotReady 的旧单元可以照常在 `maxScaleDown` 内替换（现规范 RU-B06：`maxScaleDown=1`、`maxHealthyScaleDown=0`）。保护区的故障会因此卡住灰度，要同时给出看得见的阻塞原因（EV-3）。

#### BG-2 受保护的组被缩容选中后，预算多算一个（P0，两者）

- **位置**：同 BG-1，加上 `msc.go:3040-3089`（缩容选择）。
- **现象**：正在删除的受保护组仍计入组数，但不计入 `unavailable`。
- **探针**（`TestProbe005_ProtectedVictimInflatesBudget`）：N 从 4 缩到 3，P=2，U=1，四个组都是旧版本 Running，sg-1 的 deletion cost 最低。数量同步把 sg-1 置为 `Deleting`；同一轮的滚动步骤算出 `maxScaleDown = 4 − 2 − 0 = 2`，把 sg-2 和 sg-3 一起删掉。Running 只剩 sg-0 一个，底线是 2。
- **规范**：R8、R9、R24。无论 D2 取哪种口径，这个结果都不对：滚动区此时有两个单元，U=1，最多只能动一个。
- **建议**：BG-1 的健康额度可以挡住它。按 R24 在缩容没结束时不替换，也可以挡住。两处都应当加。

#### BG-3 缩容没结束就开始替换（P1，两者）

- **位置**：`msc.go:795-868` 与 `msc.go:1729-1853` 在同一轮先后执行，中间没有门控。
- **探针**（`TestProbe005_C07_ScaleDownThenRoll`）：5 个旧版本组，同一次请求把 N 改成 3 并换模板，U=1。数量同步把 sg-3、sg-4 置为 `Deleting`；同一轮里 sg-2 也被置为 `Deleting`，此时 sg-3、sg-4 还在。Running 为 2，等于底线，可用性没有被突破。
- **Kind**：任务 `issues/bugs/003` 在 release-1.1 @ `a8be7ff1` 上观察到同样的现象（RUN-629、RUN-631）。main 上的代码路径相同，由上面的探针确认；main 上没有 Kind 结果。
- **规范**：R23、R24；现规范 SG-C05、SG-C07。
- **影响**：被删的 sg-2 要等缩容的组彻底消失、容量降到上限以内才能重建，它不可用的时间比先缩容再滚动更长。和 BG-2 叠加时会突破底线。
- **缩容对象卡住时更严重**，有两个探针。按 R24，两种情形里健康的组都不会被删，滚动同样要等，但没有容量损失。
  - `TestProbe005BR_X1_StuckScaleDownVictims`：N 从 5 缩到 3，`sg-3`、`sg-4` 的删除一直不结束，此时提交 v2。`sg-2` 被删除，之后因为组数把正在删除的组也算进去，`sg-2` 补不回来，Ready 停在 2。
  - `TestProbe005BR_X2_FixForwardWhileScaleDownStuck`：N 从 5 缩到 4，同一次请求提交一个起不来的 v2，`sg-4` 的删除不结束。`sg-3` 被删且补不回来，Ready 停在 3；再提交修正版本 v3 也不起作用。
- 受影响的场景：SG-C02、C05、C07、C10、P05。
- **建议**：不带 surge 标记的组（含 `Deleting`）多于 N 时，本轮不创建 surge、不替换。

#### BG-4 SG 模式没有显式的在途额度（P2，两者）

- **位置**：`msc.go:1776-1789`、`msc.go:1840`、`msc.go:1868`；InPlace 的占位见 `ipu.go:340`。
- **现象**：正在删除的旧组和正在原地更新的旧组，都靠「下一轮作为非 Running 的旧组被重新选中」占掉一个额度。重新选中时删除函数或更新函数是幂等的。
- **探针**（`probes/whitebox-inplace` 的 P10）：N=4、U=1，sg-1 正在原地更新，随后旧的 sg-3 变成 NotReady。从尾部向前取时先取到 sg-3，sg-3 也被打补丁。同时处于更新中的单元是 2 个。Ready 没有再下降，因为 sg-3 本来就坏了。
- **规范**：R8、R36。
- **建议**：把「正在删除」和「原地更新中」的单元直接计入不可用，不再作为候选。协调模式已经是这样做的（`coord.go:641-646`）。

#### BG-5 四处边界情形（P3，两者）

- `msc.go:1822` 的 `minAvailable = N − U` 没有取 `max(0, ·)`。U 大于 N 时为负，`maxScaleDown` 偏大。admission 限制了 U 不超过 N，所以只影响绕过 admission 的对象。走读。
- Role 的 `maxUnavailable` 为 nil 时预算取 `replicas`，等于没有底线（`obs.go:133-135`）；SG 路径的默认是 1（`utils/utils.go:600-610`）。CRD 的默认值会补成 1，所以只影响没有经过默认化的对象。走读。
- U 和 S 同时解析为 0 的校验在副本数为 0 时被跳过（`webhook/validator.go:234`，类型注释 `pkg/apis/workload/v1alpha1/model_serving_types.go:281-282` 也这样写）。现规范 API §3.1、§4.1 和 A.2 要求零副本也拒绝。全部受 partition 保护时实现是拒绝的（`webhook/validator.go:245-250`，不看 partition），与现规范一致；开发者文档里「partition 留有可更新的副本时不能同时为 0」的说法落后于代码。走读。
- `/scale` 子资源不经过这个 webhook，百分比的 U 在缩容后可能解析成 0。此时 SG 路径在 `maxScaleDown <= 0` 处只打一条 V(4) 日志就返回（`msc.go:1833-1837`），没有 Event，也没有 condition。R6、R29 要求报告。走读。

#### BG-6 InPlace 目标回退时不经预算直接删除单元（P1，缺口）

- **位置**：`ipu.go:490-495`、`ipu.go:667-680`、`ipu.go:765-770`。
- **探针**（`probes/whitebox-inplace` 的 P6）：N=2、U=1。sg-0 已经在 v2 镜像上 Ready 但还没翻标签，sg-1 因别的原因 NotReady。把 spec 改回 v1 后，sg-0 被直接置为 `Deleting`，Running 从 1 降到 0，底线是 1。这里的 Running 是 datastore 里的状态，探针的 fake client 观察不到 Pod 被删除，限制见该目录的 `README.md`。
- **规范**：现规范没有覆盖 InPlace。修订规则 R37 要求回退后的单元重新成为普通待替换单元，受预算约束。
- **建议**：回退不走单独的删除路径，让它回到候选里，由预算决定何时处理；能原地改回旧镜像的就原地改。

### 3.2 序号与 surge

#### ORD-1 稀疏布局下 surge 容量落进低位洞，滚动结束后空洞移位（P1，两者）

- **位置**：`msc.go:982-1024` 和 `msc.go:3012-3034`（在 `[0, expected)` 里补最低的缺失序号）；`msc.go:1013-1015`、`msc.go:1279-1281`（只有序号 ≥ N 才打标记）；`surge.go:193-198`（没有带标记的单元时期望数回到 N）。
- **现象**：有可更新的旧单元时期望数是 `N + S`。原来就有洞时，多出来的那个单元建在洞里，序号小于 N，不带标记。最后一个旧单元被删之后，没有带标记的单元，期望数回到 N，数量已经够了，最后腾出的那个序号不再补建。
- **探针**：
  - SG 模式（`TestProbe005_S04_HoleAndSurge`）：N=3、S=1、U=0，初始 `{0:旧, 1:旧, 3:新}`。第一轮创建的 `sparse-2` 不带标记。终态 `{1, 2, 3}`，0 号消失。
  - 独立 Role 模式（`probes/whitebox-role-coordination` 的 P4）：初始 `{0, 1, 5}`，终态 `{1, 2, 5}`。
  - SG-S03 加 `maxSurge=1`（`TestProbe005BR_S03_WithSurge`）：初始 `{0:旧, 3:新}`、N=2，终态 `{1, 3}`。
- **规范**：R1、R2、R4；现规范 §0.6「只有显式缩容可以新增稳定空洞」。两份规范期望的终态都是原来的序号集合（`{0, 1, 3}` 和 `{0, 1, 5}`）。
- **影响**：可用性没有问题，Ready 全程不低于 3。空洞数没有增加，但位置变了，而且变到了 0 号。`status.labelSelector` 固定选每个组里第一个 Role 的 0 号实例（`msc.go:2904-2912`），Role 模式下该实例消失后这个组不再被 selector 选中，对 HPA 的实际影响没有验证。
- **说明**：协调模式用 `stableSlots` 区分了正式空槽和 surge 槽（`obs.go:228-249`），没有这个问题。
- **建议**：创建分两步。正式单元在 `[0, N)` 里补，数量是 N 减去现有的正式单元数；surge 从 N 起往上找空位并打标记。

#### ORD-2 滚动途中缩容先删 surge（P2，两者）

- **位置**：`msc.go:3040-3089`。缩容的候选是所有非 `Deleting` 的组，按（NotReady，cost，序号从高到低）排序。
- **探针**（`TestProbe005_C12_ScaleDownWithSurge`）：N 从 4 缩到 3，U=0，S=1，sg-0～3 旧版本 Running，sg-4 是带标记的新版本 Running。数量同步删的是 sg-4。
- **规范**：R20；现规范 SG-C12 期望删 sg-3，保留已经 Ready 的 surge。
- **影响**：只是浪费。完整的一轮里 `sg-4` 被缩容选中、`sg-3` 被滚动选中，Ready 从 5 降到 3（等于底线，没有突破）；之后在 3 号新建一个 surge，终态仍是 `{0, 1, 2}`，多一次创建和一次删除（`TestProbe005BR_C12`）。开发者文档写明了「may remove a surge group」。
- **建议**：缩容只在正式单元里选。

#### ORD-3 N 扩大覆盖 surge 时清除标记并转正（符合，不是缺陷）

- **位置**：`surge.go:135-186`；Role 模式 `surge.go:150-162`、`obs.go:251-273`。
- 这符合修订规则 R3（D1 已定为转正并清除标记），也符合现规范的 SG-C03、SG-P08、SG-P10。LWS、RBG 读代码也是保留被 N 覆盖的 surge，没有找到文档或测试说明。这里记一笔，是因为它是 D1 比较过的三种做法之一，不需要改。
- 探针 `TestProbe005BR_X6_ScaleDownAfterPromotion` 是当前实现的对照：N 扩大覆盖 surge 之后再缩容、另有一个故障单元时，当前实现删的是故障单元，转正过的单元保留。如果转正时不清除标记，这里会删错对象，这一点来自参考模型的「转正但保留标记」变体，不是探针，见行为规范 D1。

#### ORD-4 surge 单元的 Pod 被重建后不带标记（P2，两者）

- **现象**：`RoleRecreate` 或 `None` 策略下，surge 组丢了全部 Pod 但组本身还在时，重建的 Pod 不带标记。滚动结束时它不会被当作 surge 回收，终态是 `{1, 2, 3}`。
- **证据**：开发者文档把它列为已知限制。任务 `issues/bugs/003` 在 PR #2016 的验证记录里有 Kind 观察（「M2 单独」一行，commit `cbf37e4a`）。本次没有重跑。
- **规范**：R4。按修订规则，单元内后来补建的 Pod 继承单元的标记（行为规范第 3 节），整个单元消失后由 R25 重新按 surge 创建，不会出现这种情况。
- **建议**：局部重建 Pod 时继承同一单元其他 Pod 的标记；单元没有任何 Pod 时按「序号 ≥ N」重新判定。

#### ORD-5 缩容按刷新之前的状态选对象（P2，两者）

- **位置**：独立 Role 模式 `msc.go:1432-1444` 早于 `msc.go:1527-1536`；SG 模式 `syncServingGroupReplicas` 早于 `syncRoleReplicas`，打分读的是 datastore 状态（`controller/binpack_scaledown.go:158-190`）。
- **探针**（`probes/whitebox-role-coordination` 的 P5）：3 个实例缩到 2 个，`decode-0` 的 Pod 变成 NotReady 但没有重启，datastore 里仍是 `Running`。被删的是健康的 `decode-2`，只剩一个 Ready。SG 模式的顺序相同，是走读结论。
- **规范**：R20「先 NotReady」。
- **原因**：见 EV-1。Pod 只是变成 NotReady 时 datastore 不更新，要等同一轮后面的 Role 同步才刷新，而缩容在刷新之前。
- **建议**：把状态刷新移到缩容之前。

#### ORD-6 Role 模式下新建的 ServingGroup 不看 Role 的 partition（P1，两者）

- **位置**：`msc.go:996-999`（只看 SG 层的 partition，Role 模式下恒为 0）、`msc.go:3131-3152`。
- **探针**（`probes/whitebox-role-coordination` 的 P6）：Role 的 partition 为 1，灰度停在 `{decode-0:旧, decode-1:新}`。把 `spec.replicas` 从 1 改成 2，新组的 `decode-0` 是新版本。而在已有的组里把 `decode-0` 删掉重建，得到的是旧版本。
- **规范**：R15；现规范 §0.5、API §4.3。现规范没有专门举这个例子。
- **影响**：灰度期间扩容会扩大新版本的范围。
- **建议**：新建 ServingGroup 时逐个 Role 实例按 `序号 < Role 的 partition` 选版本，复用组内补建的规则（`msc.go:1301-1352`）。

### 3.3 版本与基线

#### VER-1 缩容删掉全部旧版本单元后基线被推进（P1，两者）

- **位置**：`msc.go:2826-2857`。推进条件是 `updated == N && available == N && 组数 == N`，只看现存的组，不看 partition。
- **探针**（`TestProbe005_BaselineAfterScaleDownThroughProtectedZone`）：N=3、P=1，sg-0 旧版本 NotReady（受保护），sg-1、sg-2 新版本 Running，`currentRevision` 是旧版本。缩到 2，NotReady 的 sg-0 被优先删掉。此时 `currentRevision` 变成新版本，而 P 仍是 1。再扩回 3，sg-0 按新版本创建。
- **规范**：R17；现规范 SG-P03 第二张表期望受保护的缺位按历史版本重建（那张表里是 sg-1，探针里是 sg-0）。
- **另一个触发方式**（探针 `TestProbe005BR_X5_BaselineAfterScaleToZero`）：P=3 时提交新模板，没有任何单元更新；把 N 改成 0 再改回 3，三个受保护的槽位都按新版本创建。协调模式对缩到 0 专门保留了基线（`msc.go:2709-2713`），非协调路径没有。
- P 为 0 时缩容删掉最后的旧版本单元后推进基线，这符合 R17，不是问题。
- **建议**：推进条件加上「P 解析为 0」。缩到 0 时保留原值。

#### VER-2 `revisionHistoryLimit` 没有实现（P2）

- **位置**：`msc.go:2931-2936`、`msc.go:2945-2956`；`utils/controller_revision.go:228-285`。
- **现象**：controller 里没有读取 `spec.revisionHistoryLimit` 的地方。历史清理要么把不再被引用的 revision 全部删掉，要么在配置了非零 partition 时永远不执行。走读。
- **建议**：按字段保留最近若干个；有 partition 时至少保留基线和目标。

#### VER-3 历史读不到时阻塞的范围偏大，只靠 Event 报告（P2，两者）

- 受保护的序号补建时解析失败，`scaleUpServingGroups` 返回错误，整轮在 Role 同步之前返回，`status` 也不更新（`msc.go:1002-1005`、`msc.go:704-706`）。走读。
- 协调模式下任一实例的历史解析失败会中止整轮，包括与它无关的 Role 的显式缩容（`msc.go:699-701`、`obs.go:88-91`）；独立模式是按单元降级后继续。走读。
- 两种情况都只有 Warning Event 和日志，没有 condition。
- **规范**：R19、R29 要求等待并报告，没有要求阻塞无关的动作。

#### VER-4 Role 模式下 datastore 里 ServingGroup 的 revision 不可靠（P3）

- 独立模式下，最后一个旧实例还在删除、尚未补建时，组的 revision 就被改成目标（`msc.go:1897-1899`）。重启后它取自第一个被处理的 Pod 的标签。走读。
- 它被 CO-2 的路径当作基线使用。

### 3.4 roleCoordination

本节的探针都在 `probes/whitebox-role-coordination/`。编号 P1～P11 与测试函数名的对应表在 `subagents/whitebox-role-coordination.md` 末尾，输出文件 `probe-output.txt` 里只有函数名。

P1、P2、P11 构造的 ModelServing 没有 `resourceVersion`，走的是 CO-7 所说的非生产分支。结论依赖的代码（`coord.go:853-867`、`coord.go:807-829`、`coord.go:638-640`）不在这个分支判断之内。独立评审把这三个探针改成生产分支（设置 `resourceVersion`，在 fake clientset 里建出各实例的 Pod）重跑，选择结果和 condition 与下文逐项相同。源码和输出在 `probes/independent-review/`。

#### CO-1 模板改回基线的 Role 永久不更新，也不报阻塞（P1，两者）

- **位置**：`coord.go:853-867`、`coord.go:717`、`coord.go:144`。
- **现象**：一个 Role 是否「有变化」由基线 revision 和目标 revision 的模板是否相同决定，不看实例实际跑的是什么版本。相同就不进入限额计算，`remainingDeletions` 留在 0，而删除额度对所有在协调名单里的 Role 都取 `min(预算, remainingDeletions)`。
- **探针 P1**：a、b 各 2 个实例，`maxSkew=50%`。v1→v2 进行到 `b-1` 是 v2 时提交 v3：a 再改一次，b 改回 v1 的模板。a 完成后 `b-1` 没有被选中，没有阻塞原因，condition 是 `RolloutComplete`。`b-1` 永远停在 v2。对照组里 b 改成第三个模板时正常。
- **规范**：R14；现规范 §0.4。
- **场景**：协调滚动途中撤回其中一个 Role 的改动。修坏版本时很自然会这样做。
- **建议**：是否有待更新的工作，按观察到的实例版本判断。

#### CO-2 回滚到 `currentRevision` 时协调不生效，结果取决于内存里的 revision（P1，两者）

- **位置**：`coord.go:807-829`。
- **现象**：目标等于 `status.currentRevision` 时，基线改用 datastore 里 ServingGroup 的 revision。两者相同就跳过这个组，组的协调策略为空。
- **探针 P2**：a 依赖 b，v1→v2 进行到 `a-1`、`b-1` 是 v2 时把 spec 改回 v1。controller 没有重启过时，`a-1` 和 `b-1` 同时被选中，依赖保留和 `maxSkew` 都没有生效，condition 是 `RolloutComplete`。把 datastore 里组的 revision 设成 v2（模拟重启后先看到 v2 的 Pod）时，只选中 `a-1`，b 被 `OldVersionDependencyPresent` 挡住。
- **规范**：R31；现规范 §0.7。
- **附带影响**：webhook 用这个 condition 判断协调滚动是否在进行（`webhook/role_coordination_validator.go:276-285`），此时会放开依赖 Role 的缩容和提高 partition。走读。
- **建议**：同 CO-1。不要用内存里的组 revision 当基线。

#### CO-3 `maxSkew` 与旧依赖保留互相等待（P1，缺口）

- **位置**：`coord.go:966-982`、`coord.go:996-1007`、`coord.go:1065-1086`；`webhook/role_coordination_validator.go:214-271` 没有相应校验。
- **证据**：走读加推演，不是完整路径上的探针。探针 P8 循环调用限额函数 `calculateRoleRolloutLimits`，候选过滤是探针里按规则重写的，没有经过 `resolveRoleRolloutPolicy` 和真实的选择函数。独立评审按 `coord.go:1044-1063` 的公式手算了下表 7 行，结果相同。设定：a 依赖 b，两者都改模板，partition 都是 0，每个启动的实例立即 Ready。

  | a 的实例数 | b 的实例数 | `maxSkew` | 结果 |
  | --- | --- | --- | --- |
  | 8 | 4 | 10% | 完成 |
  | 4 | 2 | 10% | 完成，靠末步许可 |
  | 5 | 2 | 10% | 停在 a 3/5、b 1/2 |
  | 8 | 2 | 10% | 停在 a 5/8、b 1/2 |
  | 8 | 2 | 25% | 停在 a 6/8、b 1/2 |
  | 8 | 2 | 40% | 完成 |
  | 10 | 3 | 10% | 停在 a 8/10、b 2/3 |

- **原因和修法**见行为规范的 D7。实现和规范在这一点上一致，问题在规范。这些配置都能通过 admission。
- **待确认**：用完整的 reconcile 路径跑 8/2/10%，看是否同样停在 a 5/8、b 1/2。

#### CO-4 依赖启动门控会被一次与滚动无关的删除打开（P1，两者）

- **位置**：`coord.go:638-640`、`coord.go:701-706`、`coord.go:954`。稳定范围内任何正在删除的实例都被算作「已承诺的替换」。
- **探针 P11**：a 依赖 b，a 有 4 个实例，U=2，`maxSkew=100%`，b 还没有目标版本的 Ready 实例，a 应当整体等待。

  | `a-3` 的状态 | `allowTargetStart` | `remainingDeletions` | 被选中 |
  | --- | --- | --- | --- |
  | Running | false | 0 | `b-1` |
  | 正在删除（例如故障恢复） | true | 3 | `a-2`、`b-1` |
  | 已消失 | false | 0 | `b-1` |

- **规范**：R31；现规范 API §5.2。
- **影响**：健康的旧 `a-2` 在依赖的目标版本就绪之前被删，这是探针看到的。之后的发展是走读推论：`a-3` 消失后门控重新关上，`a-2`、`a-3` 都不补建，a 停在预算底线，直到 b 的目标版本 Ready。
- **建议**：只统计滚动发起的替换；或者门控直接看是否存在目标版本的实例。

#### CO-5 依赖启动门控只作用于根 Role（缺口）

- **位置**：`coord.go:954-964`；CRD 注释 `pkg/apis/workload/v1alpha1/model_serving_types.go:325-328`；单测 `controller/role_rolling_update_coordinator_test.go:678-688`。
- 三级链 a→b→c 里，b 不等 c 的目标版本 Ready 就开始替换。实现和 CRD 注释一致，现规范 API §5.2 的文字没有区分。见行为规范 D8。走读加已有单测。

#### CO-6 根 Role 被门控期间，故障实例不补建（P2，缺口）

- **位置**：`msc.go:1354-1358`、`msc.go:1503-1505`；单测 `controller/coordinated_role_regression_test.go:53-63`。
- 根 Role 还没有目标版本实例、依赖的目标版本又起不来时，根 Role 每坏一个实例就少一个，普通扩容也被挡住。现规范「整实例重建用目标版本」和「目标版本要等依赖」两条合起来就是这个结果。走读加已有单测。
- **建议**：在规范里确认是否接受。可选的做法是门控期间按已应用的版本补建。

#### CO-7 生产分支由 `ms.ResourceVersion != ""` 切换，单测不走生产分支（P2）

- **位置**：共 13 处，例如 `obs.go:40`、`obs.go:65`、`controller/coordinated_round.go:60`、`msc.go:1906`、`msc.go:2429`、`controller/revision_history.go:495`。
- **探针 P3**：同一个目标版本、正在删除的 Pod，`resourceVersion` 为空时比较结果是「相同」，非空时是「未知」。原因是 `terminatingRoleReplicas` 用一个名字为空的记录去找 live Pod（`coord.go:755-757`），找不到。
- **影响**：目标版本实例删除期间，该 Role 被当成还有旧版本，它的依赖多保留一个旧实例，末步许可也不成立。Pod 消失后恢复。更大的问题是协调器的主要单测都没有设 `resourceVersion`，测的不是生产路径。
- **建议**：用显式注入的观察接口代替这个判断。

#### CO-8 协调模式的开销（P2）

- 每个删除候选、每次创建、每次 surge 回收之前，都重新 LIST 该 ModelServing 的全部 Pod、刷新全部实例、重算全部组的策略（`msc.go:1906-1925`、`controller/coordinated_round.go:148-231`、`obs.go:336-362`）。
- 任何非 Ready Pod 的更新都入队一次（`msc.go:413-415`）；每轮开头还有一次 Service LIST（`aux.go:78`）。走读，没有测量。

### 3.5 故障恢复与重启

本节除特别注明外都是走读结论，详细推演见 `subagents/whitebox-recovery-restart.md`。

#### REC-1 故障起点不跨重启（P1，两者）

- **位置**：`msc.go:2257-2271`、`msc.go:2309-2313`、`msc.go:767-776`。
- 起点只在内存的 `graceMap` 里，而且它的值从来没有被读过。重启后 `syncAll` 重新登记，从重启时刻起计满一个 grace。
- **规范**：R32；现规范 API §6.2 的例子：grace=60 秒，t=0 故障，t=40 controller 退出，t=50 重新运行，应当在 t≈60 处理，代码在 t≈110。
- **建议**：起点写到 Pod 注解。见状态设计 4.7。

#### REC-2 grace 到期时不读最新配置（P1，两者）

- **位置**：`msc.go:2269`、`msc.go:2308-2350`。
- 计时 goroutine 持有事件发生时的 ModelServing 对象，睡眠时长取自它，醒来后只复核 Pod。计时期间把策略改成 `None`、把 grace 改成 `-1` 或改短，都不生效。
- **规范**：现规范 API §6.2「延迟任务执行前读取最新 policy/grace」。

#### REC-3 恢复之后再次故障，沿用旧的计时（P1，两者）

- **位置**：`msc.go:2259-2263`。Ready 分支里没有清除 `graceMap` 的操作。
- Pod 在 grace 内恢复 Ready 后，记录还在。同一个 Pod 再次故障时命中旧记录，在标记不可用之前就返回了。结果有两个：第二次故障只等了旧计时剩下的时间；这段时间里 datastore 仍把它算作 Running，也没有入队。
- **规范**：现规范 API §6.2「恢复 Ready 结束该次故障；后续新故障、新 Pod UID 不能继承旧删除许可」。

#### REC-4 重启时全部 Pod 处于故障态的单元不进 datastore（P1，未验证）

- **位置**：`msc.go:406-411`、`msc.go:2247-2306`、`msc.go:982-1024`、`msc.go:3270-3311`。
- 故障分支只修改已经登记的单元，不登记新单元。重启时如果某个单元的所有 Pod 都是 Failed 或在崩溃循环，datastore 里没有它。第一轮 reconcile 把它当成缺失的序号去创建，创建时遇到同名 Pod：版本不同就报错并中止整轮；策略是 `None` 或 grace 是 `-1` 时，没有人会删掉这些 Pod，可能长期不解。
- 需要一个探针或 Kind 用例确认。

#### REC-5 重启时仍在删除的 Pod（P1，未验证）

- **位置**：`msc.go:355-359`、`msc.go:3270-3311`、`msc.go:467-489`。
- 正在删除的 Pod 不进 datastore。它所属的序号被当成缺失去创建，创建遇到同名 Pod 时不检查 `DeletionTimestamp`：版本相同就当作已经存在。之后这个 Pod 消失，删除事件在新进程里被当成一次真实的删除，按恢复策略重建整个范围，可能删掉健康的幸存成员。
- **规范**：R34；现规范 A.4「旧 Pod 仍 Terminating：不重新取得删除额度」。
- 需要一个探针或 Kind 用例确认。

#### REC-6 恢复只由事件触发，没有兜底（P2）

- reconcile 路径不扫描故障 Pod。grace 到期时删除失败只打日志，不重试、不入队；终态 Failed 的 Pod 通常不会再有事件（`msc.go:2332-2336`）。两个 informer factory 的 resync 都是 0。

#### REC-7 单元遇到快照之外的 Pod 后永远停在 `Deleting`（P1，两者）

- **位置**：`msc.go:3362-3367`（组）、`msc.go:1694-1699`（Role 实例）。
- 删除过程中发现快照之外的同名 Pod 时，发一个 `DeletionIdentityChanged` 事件并返回 nil，单元留在 `Deleting`。之后每轮重复同样的结果；`Deleting` 的单元不补 Pod，Ready 事件也不改它的状态。直到 controller 重启。
- **证据**：Role 实例的终态有探针（`probes/whitebox-role-coordination` 的 P7，连续三轮不变）。已有单测 `TestReplacementPodIsPreservedAndDiagnosedDuringCleanupRetry` 断言的也是保持 `Deleting`。触发它的竞态是走读得出的，没有复现：worker 在实例变 `Deleting` 之前读了列表，随后补建了缺失的 Pod。
- **影响**：该单元一直占用不可用额度，挡住后续滚动。
- **建议**：遇到替代 Pod 时放弃这次删除、把状态交还给下一轮重新判断。

#### REC-8 旧的删除事件可能重建新单元（P2）

- **位置**：`msc.go:491-532`。函数上方的注释自己说明了这个窗口：新 Pod 还没进 informer 缓存时，`recreatedSinceDeletion` 返回 false，旧 Pod 的删除事件会按策略把刚建的单元再删一遍。时间戳的粒度是 1 秒。

#### 重启用例的推演

| 用例 | 补建的版本和幸存者 | 缺员期间不滚高位 | 预计 |
| --- | --- | --- | --- |
| RC-01 SG + ServingGroupRecreate | 符合 | 不符合，原因是 BG-1 | 不通过 |
| RC-02 SG + RoleRecreate | 符合 | 不符合，原因是 BG-1 | 不通过 |
| RC-03 独立 Role + RoleRecreate | 符合 | 符合，Role 模式有健康额度 | 通过 |
| RC-04 协调 Role + RoleRecreate | 补建路径同 RC-03 | 没有读完协调器的相关路径 | 不下结论 |

这是走读推演，没有运行这些用例。

### 3.6 InPlace

本节的探针都在 `probes/whitebox-inplace/`，编号与测试函数名的对应表在该目录的 `README.md`。现规范没有覆盖 InPlace，所以这里的「规范」指修订规则 R35～R38。

#### IP-1 升级前就有重启记录的容器会让 Pod 被判为崩溃（P1）

- **位置**：`inplace/state.go:215-240`、`inplace/state.go:395-399`。
- 开始原地更新时，只给被打补丁的容器登记了允许的重启次数。没被打补丁的容器（sidecar、init 容器、SG 单元里模板没变的那个 Role 的 Pod）如果之前有过重启，`RestartCount` 大于 0，一打补丁就被判为计划外重启。
- **探针 P1、P1b、P2**：带一个历史重启次数为 1 的 sidecar 的 Pod，在目标镜像上 Ready 之后仍被 `updatePod` 删除（默认 grace 为 0）。
- **影响**：原地更新退化成重建，而且是在 Pod 已经更新成功之后。
- **建议**：开始时给单元内所有容器记下当时的重启次数作为基准。

#### IP-2 SG 模式加 `RoleRecreate`：超时后的 Role 实例被反复删除重建（P1，缺口）

- **位置**：`ipu.go:732-756`、`ipu.go:509-518`。
- **探针 P3、P9**：单元内 `decode-0` 已完成，`decode-1` 超时。第一轮删除 `decode-1`；第二轮按组的旧 revision 把它重建出来；第三轮又把这个「未打补丁的旧成员」删掉。循环不止，已完成的 `decode-0` 永远不翻标签。
- **规范**：R37：原地更新中的单元发生重建时直接用目标版本。
- **建议**：同上。或者超时后把整个单元转为重建。

#### IP-3 一个单元的完成操作做到一半时重启，可能永久卡住（P1）

- **位置**：`ipu.go:538-549`（逐个 Pod 翻标签，不是原子的）；`store.go:393-405`（组的 revision 取第一个被处理的 Pod）。
- **探针 P5**：重启后如果先处理到已经翻过标签的 Pod，组在 datastore 里的 revision 是目标版本，之后每轮都在 Role 同步里报「身份不匹配」并提前返回，到不了完成 InPlace 的那一步。先处理到没翻标签的 Pod 时能收敛。
- **建议**：完成 InPlace 的步骤放到 Role 同步之前；或者 datastore 里组的 revision 不依赖 Pod 的处理顺序。

#### IP-4 见 BG-6

#### IP-5 启用 InPlace 时 `maxSurge` 整体不生效（P2，分歧）

- **位置**：`msc.go:819-823`、`msc.go:1408-1416`。开关是 ModelServing 级和 Role 级的，不看这次变更是否满足原地条件。
- 一次不满足原地条件的变更会在没有 surge 的情况下按 U 重建。用户文档写明了这一点（`docs/kthena/docs/user-guide/multi-node-inference.md:432-433`），有单测 `TestSurgeDisabledWhileUpdatingInPlace`。见行为规范 D6。

#### IP-6 三个较小的问题（P2）

- 在途的 SG 单元里，已经完成的成员每一轮都被重新标成不可用，Role 状态和事件来回抖动（探针 P7）。
- 完成后立即入队，下一轮如果早于 informer 收到标签翻转，会报一次身份不匹配并多发一次创建请求（探针 P8）。
- 关闭恢复时，超时的单元永久占着额度，只有 Event，没有 condition（走读）。

### 3.7 事件、队列与 status

#### EV-1 Pod 只是变成 NotReady 时不入队，也不更新 datastore（P2，两者）

- **位置**：`msc.go:412-418`、`msc.go:421-431`。
- **探针**（`TestProbe005_PlainNotReadyEvent`）：一个 Ready 的 Pod 变成 NotReady（没有重启）。事件处理后队列长度不变，组在 datastore 里仍是 `Running`。
- **影响**：`status.availableReplicas` 一直是旧值，直到别的事件触发 reconcile。滚动的删除决定不受影响，因为每轮会先按 Pod 重算。缩容选择受影响，见 ORD-5。协调模式下这类事件会入队。
- **建议**：任何 Pod 变化都入队；打开 informer 的周期 resync 作兜底。

#### EV-2 informer 回调里执行删除和 API 调用（P2）

- Pod 删除事件的处理器直接调用 `deleteServingGroup`、`DeleteRole`，里面有 LIST、GET、DELETE 和 plugin 调用（`msc.go:2352-2386`）。每个 Ready 事件会直接 GET 若干次 ControllerRevision（`msc.go:2144`、`msc.go:2184`）。
- 回调是串行的，一个回调慢了后面的事件都跟着晚。它也让 datastore 有了第二个写入者，`Deleting` 的两处保护都是先读后写、没有锁。走读。

#### EV-3 `status` 表达不了规范要求区分的几种状态（P2，两者）

- 灰度停点和全量完成是同一个 condition、同一个 reason，只能靠 `currentRevision != updateRevision` 推断。
- 规模完成没有专门的信号。扩缩容中、故障恢复中、初次创建中都是 `Progressing/GroupProgressing`。
- 非协调模式下，阻塞（坏目标、历史缺失、预算用尽）只有 Event。`UpdateInProgress` 会一直保持 True，无法和「正常但慢」区分。
- `UpdateInProgress` 和 `Progressing` 之间不互斥，切换时旧的那个保持 True（SG-P08、SG-P09 的探针里两者同时为 True）；condition 已经是 True 时不刷新 message，里面的序号列表会过期（`utils/utils.go:437-500`）。
- `status.replicas` 是 datastore 里的组数，包含 surge 和正在删除的组，滚动期间会大于 `spec.replicas`（探针 `TestProbe005BR_X4_StatusDuringSurgeRollout`）。
- Role 模式下，组的 Ready 要求每个 Role 的实例数恰好等于 `replicas`（`msc.go:2403-2407`）。surge 实例存在期间组不是 Running，所以 U=0、S=1 的 Role 滚动全程容量不降，`status.availableReplicas` 却会下降。
- datastore 里没有该 ModelServing 的记录时（例如 N=0 且刚重启），`status` 只更新 revision 和 selector，不更新计数、condition 和 `observedGeneration`（`msc.go:2693-2733`）。未验证。
- **规范**：R28、R29。

#### EV-4 等待和出错的处理不统一（P3）

- 等 Pod 消失、等单元 Ready 返回 nil；历史读不到返回错误并限速重试；同名 Pod 身份不符既返回错误又自己设 1 秒定时。协调路径上有十余处固定 1 秒的重入队。
- 提前返回会跳过 `status` 更新，包括 `observedGeneration`。

#### EV-5 grace goroutine 的几个细节（P3）

- goroutine 不接收 context，不可取消，leader 切换后继续运行。靠 UID 前置条件保证不误删。
- `deleteServingGroup` 里 `replacements()` 的 LIST 出错时，错误没有赋给 defer 检查的那个变量，不会触发补偿入队（`msc.go:3362-3364`）。

### 3.8 可读性与结构

这一节都是 P3。它们不是行为问题，但它们是上面多数问题的成因：规则散在多处时，每次改动只照顾到了其中一处。

| 编号 | 问题 | 位置 |
| --- | --- | --- |
| RD-1 | 滚动的阶段没有显式表达。顺序由 `syncServingGroupReplicas`、`syncRoleReplicas`、`manageRollingUpdateWithPolicy` 的调用顺序加 `expectedCount` 公式隐含决定。BG-3、ORD-1、ORD-2 都出在阶段的交界上 | `msc.go:702-723`、`msc.go:795-824` |
| RD-2 | 预算有两套算法。SG 模式内联在函数里，一个额度；Role 模式用 `rolloutBudget`，两个额度。BG-1 只存在于前者 | `msc.go:1814-1841`、`obs.go:112-143` |
| RD-3 | surge 有两套实现。独立模式在 `surge.go`，在删除阶段回收；协调模式在 `obs.go:214-379`，在副本同步阶段回收。识别来源、放置序号、回收入口都不同。ORD-1 只存在于前者 | — |
| RD-4 | 候选顺序靠列表的拼接顺序和 datastore 的排序实现，没有显式的排序键 | `msc.go:1840`、`msc.go:1868`、`store.go:124-128` |
| RD-5 | `manageRoleReplicasPerGroup` 一个函数做七件事，约 200 行，三种模式的分支交织。`resolveRoleRolloutState` 用十几个局部计数器在一个循环里同时维护两本账 | `msc.go:1378-1574`、`coord.go:598-725` |
| RD-6 | `msc.go` 里有 31 处 `roleCoordination(ms)` 分支。独立模式也在用的 `rolloutBudget`、surge 注解常量等通用类型放在 `coordinated_role_observation.go` 里 | — |
| RD-7 | 隐式状态通过 `context.Context` 传递：revision 历史、协调 round、InPlace round、surge 作用域。`outdatedRolesInRound` 修改调用者传入的切片，后面的预算计算依赖这个副作用 | `msc.go:2100-2101` |
| RD-8 | InPlace 的预算占位是隐式约定，没有注释或类型表达；函数名和日志仍是「delete」 | `ipu.go:144-170`、`msc.go:1858-1878` |
| RD-9 | 只有测试调用的生产函数、没有用到的参数、和实现不符的注释 | `msc.go:3487-3572`、`msc.go:3154-3157`、`msc.go:1721-1722` |

### 3.9 测试

| 编号 | 问题 |
| --- | --- |
| TS-1 | 有现有单测断言了与规范不符的行为。改代码时需要一起改：`TestServingGroupRollingUpdateIgnoresUnavailableProtectedGroups`（BG-1）、`TestReplacementPodIsPreservedAndDiagnosedDuringCleanupRetry`（REC-7）、`TestSurgeDisabledWhileUpdatingInPlace`（IP-5，取决于 D6） |
| TS-2 | 协调器的主要单测不走生产分支（CO-7） |
| TS-3 | 没有组合场景层面的单测。`surge_rollout_test.go` 的 `TestServingGroupRolloutRestoresOrdinals` 覆盖了从 `[0, N)` 和三种稀疏布局出发的 surge 滚动，并断言了终态序号；但稀疏用例里范围外的单元都是旧版本，会被替换掉。没有「范围外的单元已经是目标版本」的用例，所以 ORD-1 没有被发现。缩容与滚动交错、受保护单元 NotReady、滚动途中缩容都没有用例 |
| TS-4 | 35 个组合场景在 main 上没有在 Kind 上跑过。已有的 Kind 结果来自 release-1.1（6 个简单组合用例）和 PR #2016 合入前后的提交（基础矩阵），与 `b4e536bb` 都不是同一个 commit。建议先在 `b4e536bb` 上跑一遍 RUN-618～652，把 2.2 的探针结论换成 Kind 证据。按探针推断，RUN-636（SG-C12）和 RUN-649（SG-P09）不会通过 |

整改时需要的回归用例（单元级 19 项、Kind 级 9 项，以及哪些现有 runner 用例已经覆盖）列在 `subagents/behavior-regression-analysis.md` 第 4 节。

## 4. 做得好的地方

这一节都是走读结论，标了探针的除外。

- **模板比较是三态的。**「未知」既不授权删除，也不提供可用性余量，也不算已更新（`controller/revision_history.go:37-43`、`msc.go:1776-1783`）。
- **目标 revision 在任何 Pod 创建之前持久化**（`msc.go:673-682`）。比较用语义相等，不只看 hash；hash 冲突有处理。
- **删除用一次性的 UID 快照。**重试只删快照里的 UID，不扩大到同名的新 Pod；DELETE 带 UID 前置条件（`controller/pod_deletion_targets.go:49-101`）。
- **Ready 按单元已应用的布局逐个成员核对。**`workerReplicas` 变化不会让还没轮到的旧实例被误判为缺 Pod（`controller/revision_history.go:274-291`）。
- **局部修复沿用已应用的版本**，整单元重建才看 partition。与 R15、R18 一致。
- **每轮先按 Pod 重算 Ready，再算预算**（`msc.go:1114-1126`、`msc.go:1527-1536`）。删除决定不依赖事件处理器是否及时。
- **独立 Role 模式的预算**把总额度和健康额度分开，一次扣减同时处理两者（`obs.go:112-127`）。探针 P10 对照了附录 B 的 11 行，全部一致。
- **partition 在所有路径上都是绝对序号**，百分比向上取整。
- **surge 的身份持久化在 Pod 注解上**，重启后回收能继续。
- **协调路径**每次变更前用 live 观察复核；skew 计算防溢出；阻塞原因的选取是确定的；末步许可的条件和规范逐条对应，且不持久化。
- **延迟事件的三处防护**（PR #2016 的第三个提交）：Pod 事件用 lister 复核 UID；Ready 事件不复活 `Deleting` 的组；删除事件识别已经重建的单元。

## 5. 建议的整改顺序

下表按建议的先后排列。最后一列是它对应[状态设计第 6 节](modelserving-rollout-state-design.md#6-迁移路径)迁移路径里的哪一步。第 2、3、4、6 组都是在现有结构里做的局部修改，合起来是迁移路径的步骤 1，彼此之间没有依赖。D2 已经定下来，第 2 组可以开始。

| 顺序 | 内容 | 意见 | 前置 | 迁移步骤 |
| --- | --- | --- | --- | --- |
| 1 | 口径已定（2026-11-12）：D2 目前保持占用，BG-1 按缺陷改；D1 转正并清除标记，当前实现已经符合，不用改 | BG-1、ORD-3 | 已完成 | — |
| 2 | SG 模式补健康额度；缩容没结束不替换；基线只在 P 为 0 时推进 | BG-1、BG-2、BG-3、VER-1 | 1 | 1 |
| 3 | 正式单元和 surge 分开创建；缩容只选正式单元；缩容前先刷新状态；重建的 Pod 继承标记；Role 模式新建组按 Role 的 partition 选版本 | ORD-1、ORD-2、ORD-4、ORD-5、ORD-6 | — | 1 |
| 4 | 协调：按实例实际版本判断是否有工作；依赖门控不看无关的删除。`maxSkew` 走不通的配置另行立项 | CO-1、CO-2、CO-4；CO-3 | CO-3 要等 D7、D8 | 1；CO-3 不在迁移路径里 |
| 5 | grace 改为注解加定时入队；Ready 时清除计时；恢复动作移出 informer 回调 | REC-1～REC-3、REC-6、EV-2 | — | 4 |
| 6 | InPlace：重启次数基准覆盖全部容器；重建用目标版本；回退走预算；完成步骤放到 Role 同步之前 | IP-1～IP-3、BG-6 | — | 1 |
| 7 | 把决定抽成纯函数，SG 层和 Role 层共用；合并两套 surge 和两套预算；补组合场景单测 | RD-1～RD-5、TS-2、TS-3 | 2、3 | 2、3 |
| 8 | 任何 Pod 变化都入队；周期 resync；condition 增加阶段和阻塞原因；用观察替换 datastore 状态 | EV-1、EV-3、REC-4、REC-5、REC-7 | 5、7 | 5 |

第 2 组里的三项改动都很小，而且各自对应一个 P0 或 P1，建议最先做。

## 6. 与已有代码检视报告的关系

分支 `review/modelserving-controller-code-review` 上有一份通用的代码检视报告（`docs/proposal/modelserving-controller-code-review.md`），基于较早的 main（`2eff0aee`），关注死代码、文件组织、重复实现和一般性的健壮性。本文只看滚动行为，两者重叠的部分如下。

| 那份报告的条目 | 在 `b4e536bb` 上的情况 | 本文 |
| --- | --- | --- |
| C-R01 `pod.OwnerReferences[0]` 越界 | 这段代码已经不在了 | — |
| C-R02 `deleteServingGroup` 的补偿入队遗漏 | 仍在 | EV-5 |
| C-R04 `outdatedRolesInRound` 修改入参 | 仍在 | RD-7 |
| C-R06 grace 的 goroutine 加 `time.Sleep` | 仍在 | REC-1～REC-3、EV-5 |
| C-R12、D-01 对 datastore 顺序的隐式依赖 | 仍在 | RD-4 |
| C-T01 用 `ResourceVersion != ""` 区分测试和生产 | 仍在，13 处 | CO-7 |
| C-S01、C-S02、C-S04 文件和包装层的组织 | 仍在。`msc.go` 现在是 3636 行 | RD-5、RD-6、RD-9 |

其余条目（子包、webhook、plugin、静态分析结果）本文没有复核。

## 7. 没有验证的内容

- **Kind**：本文所有结论都没有在真实集群上验证。探针用的是 fake clientset，没有 kubelet，没有优雅终止，也没有真实的 informer 时序。
- **需要探针或 Kind 才能确认的意见**：REC-4、REC-5、REC-7 的触发路径、EV-3 的最后一项、RC-01～RC-04 的预计结果、CO-3 在完整 reconcile 路径上的表现、CO-4 和 BG-6 在探针观察点之后的发展。
- **没有读的代码**：plugin 的实现；PodGroup 管理器内部；`role_coordinated_expansion.go` 对 PodGroup 的效果；webhook 中除滚动预算和协调校验以外的部分。
- **admission 的其他规则**：U 和 S 同时为 0 的校验核对过，结论在 BG-5。webhook 里其余与滚动预算有关的校验没有逐条对照现规范。
- **Role 模式的组合场景**：2.2 的 35 行都是 SG 模式。
- **性能**：CO-8、EV-2 是从代码结构得出的，没有测量。
