# 推论验证

## 0. 文档信息

| 项 | 内容 |
| --- | --- |
| 用途 | 说明 [02](02-design.md)、[03](03-state-machine.md)、[04](04-behavior-expectations.md) 里的结论各自有什么证据：哪些是模型穷举过的，哪些是推演，哪些留给真实验证 |
| 模型在哪 | 任务目录 `issues/features/008-modelserving-servinggroup-cr-design-IP/model/`，不在本仓库里。只用 Python 标准库，`uv` 运行 |
| 状态 | **没有在集群上运行任何东西。**模型检查的是规则，不是实现 |

## 1. 三类结论

| 类 | 含义 | 有哪些 |
| --- | --- | --- |
| 模型穷举过的 | 在小规模上把全部交错走了一遍，性质没有被违反；并且去掉对应的规则之后性质会被违反 | 组一级的预算、次序、上限、保护；ModelServing controller 在任何一次写之后被接管；在途屏障、接管屏障、升级闸门；失败的组等清空再删；账本的直读和代际号；Role 实例的账本在任何两次写之间停下 |
| 脚本重放的 | 一条确定的执行过程，由规则函数算出，和契约的过程表比可观察量 | 契约 2.12 的 35 个组合场景、RC-01、附录 B 的查表行 |
| 推演 | 按规则写出来的论证，没有机器检查 | Role 一级与账本的衔接；plugin 钩子的次序；condition；`status` 被改写；升级接管里成员一级的步骤；两个 controller 之间除了「就绪晚一跳」之外的滞后 |
| 留给真实验证 | 依赖 Kubernetes 或别的组件的实际行为 | 第 7 节的清单 |

## 2. 场景重放

`grouplevel.py` 是组一级的规则函数：输入是 `spec` 和一组 ServingGroup 的状态，输出是这一轮的写。它是纯函数，在工作副本上迭代（02 的 4.2）。`sim.py` 用它驱动一个确定的模拟器：脚本里写用户改了什么、哪个组就绪了、哪个组消失了，模拟器记下每个阶段的状态，生成过程表。`scenarios.py` 把契约的每个场景写成这样一段脚本，并且把契约的过程表里读出来的可观察量（创建几个组、删除几个组、哪些原有的组保住身份、终态各版本的个数、终态就绪数）写在旁边。

- 结论不是手写的：`run_scenarios.py` 比较两边的可观察量，相同是「一致」，不同是「有意变化」；不同而没有写原因、或者写了原因而没有不同，脚本报错。
- 每次健康的组被替换，脚本检查 Ready 没有低于 `minAvailable`；每次替换，检查被替换的是过期的组。全部场景里这两项都是 0。
- 04 第 3 节的汇总表和 04a 的全部过程表由脚本写入；`build_docs.py --check` 校验文档里的表和模型的输出相同。

结果：42 段脚本（35 个场景拆成 38 段，加 RC-01 和 3 个按契约规则补的场景），31 个一致，11 个有意变化；附录 B 的 14 行全部相同。

**每条规则是不是都有用。**把规则函数里的规则一次去掉一条，全部脚本重放一遍，看哪些场景的可观察量变了：

<!-- BEGIN GENERATED: mutants -->
| 去掉的规则 | 结果变了的场景 | 例子 |
| --- | --- | --- |
| 缩容时过期的组不优先（007 的次序） | 2：SG-C12、SG-P05 | SG-C12：creates 2 → 3；deletes 4 → 5；survivors g4 → 无 |
| cap 恒为 N（没有 surge） | 7：SG-S04、SG-C03、SG-C12、SG-C16、SG-C13、SG-C14、SG-P10 | SG-S04：creates 2 → 0；deletes 2 → 0；survivors g3、g4 → g0、g1、g3、g4 |
| 容量只数 live（终止中和失败的不占位） | 26：SG-S02、SG-S03、SG-S04、SG-S05、SG-S06、SG-S07、SG-C01、SG-C03 等 | SG-S02：max_active 2 → 3 |
| 健康的候选不受 maxHealthyScaleDown 限制 | 1：SG-P09 | SG-P09：min_ready 2 → 1；min_real_ready 2 → 1；breach 0 → 2 |
| 只保护版本等于基线的组 | 1：EX-1 | EX-1：creates 1 → 3；deletes 1 → 3；survivors g0、g1 → 无 |
| 缩容时 deletion cost 排在受保护之前 | 1：EX-2 | EX-2：survivors g0、g1、g2 → g1、g2、g3；final v1×3 → v1×2、v2×1 |
| 缩容的删除还没消失就开始替换 | 0： | — |
| 未观测的组当作未就绪的候选 | 1：EX-3 | EX-3：min_real_ready 3 → 2 |
<!-- END GENERATED: mutants -->

「缩容的删除还没消失就开始替换」不改变这些可观察量：它只改变过程中的停点（契约的表里「规模完成」那一行出不出现）。这条规则是为了守契约 A.4 第 2 条的字面，没有脚本能把它和不守区分开。

## 3. 组一级的穷举模型

`gmodel.py` 用同一个规则函数，把 ModelServing controller 和环境的全部交错走一遍。

| 模型里有什么 | 内容 |
| --- | --- |
| ModelServing controller | 每一步只发一个写，每个写之前重新计划。看到的是缓存加上自己的在途登记；登记了而缓存里没有的组算未就绪。有在途的创建时不发别的写 |
| 接管 | 任何时刻进程可以被换掉，在途登记丢失。有接管屏障时新进程从一致的 LIST 和刷新过的组 `status` 开始；没有时（或者旧进程还在写时）它的缓存可以缺最新的组 |
| 环境 | 组就绪、失去就绪、失败、清空、消失；缓存按创建的先后追上；用户按脚本改 `spec` |
| 升级接管 | 存量的组变成对象；闸门放行之前不做别的 |
| 性质 PA | 健康的组被替换之后，真实的 Ready 不低于 `minAvailable` |
| 性质 PB | 被替换的是过期的组；缩容只在有多余时发生；缩容不在还有过期的组时删健康的好组 |
| 性质 PD | 接管完成之前不创建、不删除组 |
| 性质 PE | 每个静止的状态都是完整的：没有失败的、终止中的、没被看到的组；活动数不超过上限；N 个好组，除非有起不来的组占着预算 |
| 性质 PF | 组的个数有界 |
| 性质 PG | 失败的组清空之后才被删（D15） |
| CYCLE | 没有活锁 |
| W-PC（说明项） | 超过上限创建了组。只在「进程按一份缺了组的视图行动」时可达 |

形状：N 是 2 或 3，(U, S) 取 (1, 0)、(0, 1)、(1, 1)，至多 3 个版本。

<!-- BEGIN GENERATED: group-model -->
```text
scenario                                      rules                   spec             note                                                 states  sinks  violations
G0 rollout, no takeover                       final                   N=3 U=1 S=0 P=0  -                                                    20      1      -
G0 rollout, no takeover                       final                   N=3 U=0 S=1 P=0  -                                                    20      1      -
G0 rollout, no takeover                       final                   N=3 U=1 S=1 P=0  -                                                    58      1      -
G1 rollout                                    final                   N=3 U=1 S=0 P=0  -                                                    44      3      -
G1 rollout                                    final                   N=3 U=0 S=1 P=0  -                                                    48      3      -
G1 rollout                                    final                   N=3 U=1 S=1 P=0  -                                                    145     3      -
G2 two rollouts                               final                   N=3 U=1 S=0 P=0  -                                                    180     5      -
G2 two rollouts                               final                   N=3 U=0 S=1 P=0  -                                                    222     5      -
G2 two rollouts                               final                   N=3 U=1 S=1 P=0  -                                                    705     5      -
G3 scale during rollout                       final                   N=2 U=1 S=0 P=0  -                                                    210     6      -
G3 scale during rollout                       final                   N=2 U=0 S=1 P=0  -                                                    209     6      -
G3 scale during rollout                       final                   N=2 U=1 S=1 P=0  -                                                    435     6      -
G4 partition                                  final                   N=3 U=1 S=0 P=2  -                                                    64      12     -
G4 partition                                  final                   N=3 U=0 S=1 P=2  -                                                    62      10     -
G4 partition                                  final                   N=3 U=1 S=1 P=2  -                                                    134     12     -
G5 bad target                                 final                   N=3 U=1 S=0 P=0  -                                                    49      4      -
G5 bad target                                 final                   N=3 U=0 S=1 P=0  -                                                    49      4      -
G5 bad target                                 final                   N=3 U=1 S=1 P=0  -                                                    170     4      -
G6 faults                                     final                   N=3 U=1 S=0 P=0  -                                                    1273    8      -
G6 faults                                     final                   N=3 U=0 S=1 P=0  -                                                    2167    8      -
G6 faults                                     final                   N=3 U=1 S=1 P=0  -                                                    3889    8      -
G7 failure at a canary stop                   final                   N=3 U=1 S=0 P=1  -                                                    24      5      -
G8 adoption                                   final                   N=3 U=1 S=0 P=0  -                                                    42      3      -
G8 adoption                                   final                   N=3 U=0 S=1 P=0  -                                                    44      3      -
G8 adoption                                   final                   N=3 U=1 S=1 P=0  -                                                    116     3      -
G8 adoption                                   final                   N=3 U=1 S=0 P=1  canary in progress                                   18      3      -
G8 adoption                                   final                   N=2 U=0 S=1 P=0  rollout with a surge group in progress               19      2      -
L1 status one hop late                        final                   N=3 U=1 S=0 P=0  a drop of Ready is seen late                         133     2      PAx6
L1 status one hop late                        final                   N=3 U=0 S=1 P=0  a drop of Ready is seen late                         169     2      PAx9
L1 status one hop late                        final                   N=3 U=1 S=1 P=0  a drop of Ready is seen late                         376     2      PAx20
M1 scale-down order of 007                    no outdated-first       N=4 U=0 S=1 P=0  -                                                    70      2      PBx4
M2 cap is always N                            no surge cap            N=3 U=0 S=1 P=0  -                                                    1       1      PEx1
M3 capacity counted on live groups            cap on live             N=3 U=1 S=1 P=0  -                                                    96      1      W-PCx8
M4 no healthy budget                          no maxHealthyScaleDown  N=3 U=1 S=0 P=1  -                                                    54      2      PAx2
M5 no in-flight registry                      no registry             N=3 U=1 S=0 P=0  -                                                    7822    1      W-PCx254, PFx128
M6 no barrier while a creation is in flight   no barrier              N=3 U=1 S=0 P=0  -                                                    409     4      W-PCx24, PBx10
M7 no adoption gate                           no gate                 N=3 U=1 S=0 P=0  -                                                    154     1      PDx11, PDx27
M8 a Failed group is deleted at once          no wait                 N=3 U=1 S=0 P=0  -                                                    18      2      PGx3
M9 no takeover barrier (also: two processes)  stale view              N=3 U=1 S=0 P=0  the view of the new process lacks the newest groups  321     3      W-PCx22
M9 no takeover barrier (also: two processes)  stale view              N=3 U=0 S=1 P=0  the view of the new process lacks the newest groups  356     3      W-PCx22
M9 no takeover barrier (also: two processes)  stale view              N=3 U=1 S=1 P=0  the view of the new process lacks the newest groups  935     3      W-PCx54
M9 no takeover barrier (also: two processes)  stale view              N=3 U=1 S=0 P=0  and group status is stale                            639     4      PAx12, W-PCx39
```
<!-- END GENERATED: group-model -->

读法：

- **G0 到 G8 没有任何违反，也没有 W-PC。**包括滚动、连续两次滚动、滚动途中扩缩容、partition 途中改变、坏版本之后修正、组失去就绪和组失败、灰度停点上组失败、升级接管之后滚动。每一项都允许在任何一次写之后接管。
- **L1 是已知的局限，不是规则的错。**一个组失去就绪、`status` 还没到 ModelServing controller 的那一段时间里，健康额度是按旧值算的，可能多删一个健康的组（02 的 6.5）。模型把它报出来，是为了说明它可达。
- **M1 到 M9 各去掉一条规则。**每一条都有性质报出来：007 的缩容次序（PB）；上限恒为 N（滚动推不动，PE）；容量只数 `live`（超过上限创建）；没有健康额度（PA）；没有在途登记（组无界地增长，PF）；没有在途屏障（PB）；没有升级闸门（PD）；失败的组立刻删（PG）；没有接管屏障（多建组，以及 `status` 是旧的时候 PA）。

接管前后终局是否相同（静止状态的集合，不看名字）：

<!-- BEGIN GENERATED: group-equivalence -->
```text
scenario       budget    without takeover  with takeover  same set
G2             U=1 S=0   3                 3              yes
G2 no barrier  U=1 S=0   3                 3              yes
G3             U=1 S=0   2                 2              yes
G2             U=0 S=1   3                 3              yes
G2 no barrier  U=0 S=1   3                 3              yes
G3             U=0 S=1   2                 2              yes
G2             U=1 S=1   3                 3              yes
G2 no barrier  U=1 S=1   3                 3              yes
G3             U=1 S=1   2                 2              yes
```
<!-- END GENERATED: group-equivalence -->

没有接管屏障时会多建组（M9 的 W-PC），但终局的集合仍然相同：多出来的被缩回。

模型没有的：未观测（它有一个场景脚本 EX-3，没有穷举）；deletion cost；两个进程同时在写组一级（只有「视图缺组」这一种后果被建模）；`RoleRollingUpdate` 下的组一级；序号命名。

## 4. 账本的两条防护规则

007 里「把缺失当丢失之前直读」和「代际号」两条规则没有模型证据。`stalemodel.py` 是为它们写的小模型：一个 Role 实例，两个成员，`RoleRecreate`；两个 ServingGroup controller 进程。

- F 是现任。账本是最新的；刚接管时它的 Pod 缓存可以缺已经存在的 Pod。
- S 是失主而还没退出的进程。它按一份旧的账本行动，Pod 的缓存可以是旧的也可以是新的。它的 status 写带着旧的 resourceVersion，账本变过之后被拒绝；它的 Pod 删除只带 UID。S 的写在模型里至多 4 次。

| 性质 | 内容 |
| --- | --- |
| Q1 | Draining 只在真的丢了成员之后写 |
| Q2 | 一个 Pod 被删，一定有一次代际号不小于它的 Draining |
| Q3 | 静止时账本是 Established，两个成员都在 |
| Q4 | 代际号不会涨到丢失的次数解释不了的地步 |

<!-- BEGIN GENERATED: stale-model -->
```text
scenario                                                     rules           states  sinks  violations
K1 established, takeover with a Pod cache that lacks Pods    final           41      4      -
K1 established, takeover with a Pod cache that lacks Pods    no direct read  144     10     Q1x3
K1 established, takeover with a Pod cache that lacks Pods    no generation   41      4      -
K2 creating, takeover with a Pod cache that lacks Pods       final           31      4      -
K2 creating, takeover with a Pod cache that lacks Pods       no direct read  31      4      -
K2 creating, takeover with a Pod cache that lacks Pods       no generation   31      4      -
K3 a loss, a process with an old snapshot keeps running      final           657     13     -
K3 a loss, a process with an old snapshot keeps running      no direct read  657     13     -
K3 a loss, a process with an old snapshot keeps running      no generation   5171    36     Q2x1074, Q4x115
K4 two losses, a process with an old snapshot keeps running  final           5439    82     -
K4 two losses, a process with an old snapshot keeps running  no direct read  5439    82     -
K4 two losses, a process with an old snapshot keeps running  no generation   19429   113    Q2x3539, Q4x406
```
<!-- END GENERATED: stale-model -->

- 去掉直读（K1）：新进程的缓存里缺 Pod，把它当成丢失，没有丢失而写了 Draining（Q1）。
- 去掉代际号（K3、K4）：S 拿着旧的 Draining(1) 和新的 Pod 缓存，把新一批的 Pod 删掉（Q2）；F 看到丢失，再清空、再建，S 再删，恢复反复进行（Q4）。
- 两条都在：没有违反。

模型在写的过程中发现了一种 r0 没有写的形态：S 以为实例在创建中，把 F 刚清空的位置又建出一个 Pod，F 再把它删掉。只要 S 还活着就可以一直重复；S 退出之后收敛，没有错误的终局。所以模型里 S 的写不算进展，并且有上限。它写进了 02 的 6.4。

## 5. Role 实例的账本和两个 controller

007 的组级模型（`sgmodel.py`、`run_v4.py`）原样重跑，输出与 007 的结果文件逐行相同。它覆盖：两个 controller 各自计划、各自执行、任意交错；任何两次写之间可以停下；账本的三个值；失败的组；策略变更；Role 副本数和组副本数的变化；组被人直接删除；`status` 被清空。

<!-- BEGIN GENERATED: ledger-model -->
```text
scenario            design                           policy  shape  crash  contract     note        states  sinks  violations
T1 base             v4                               None    1x2x2  yes    established  -           11670   3      -
T1 base             v4                               Role    1x2x2  yes    established  -           46032   3      -
T1 base             v4                               Group   1x2x2  yes    established  -           88697   6      -
T1 base             v4                               Role    2x1x2  yes    established  two groups  6982    2      -
T1 base             v4                               Group   2x1x2  yes    established  two groups  12532   4      -
T2 strict contract  v4                               Role    1x2x2  yes    strict       -           54353   15     P2x12
T2 strict contract  v4                               Group   1x2x2  yes    strict       -           104368  37     P2x31
T3 policy change    v4                               None    1x2x2  yes    established  -           27143   7      -
T3 policy change    v4                               Role    1x2x2  yes    established  -           48971   11     -
T3 policy change    v4                               Group   1x2x2  yes    established  -           106005  20     -
T4 Role scale       v4                               Role    1x2x2  yes    established  no loss     2128    3      -
T4 Role scale       v4                               Role    1x2x2  yes    established  -           83115   6      -
T4 Role scale       v4                               Group   1x2x2  yes    established  no loss     2128    3      -
T4 Role scale       v4                               Group   1x2x2  yes    established  -           148397  11     -
T5 group scale      v4                               Role    1x1x2  yes    established  no loss     2483    5      -
T5 group scale      v4                               Role    1x1x2  yes    established  -           65644   10     -
T5 group scale      v4                               Group   1x1x2  yes    established  no loss     2483    5      -
T5 group scale      v4                               Group   1x1x2  yes    established  -           100407  16     -
T6 group deleted    v4                               None    1x2x2  yes    established  -           46734   6      -
T6 group deleted    v4                               Group   1x2x2  yes    established  -           57473   6      -
T7 status wiped     v4                               Role    1x2x2  yes    established  -           16392   7      P2x4
T8 one step         v4 (one step)                    Role    1x2x2  yes    established  -           54932   3      P8x6278
T8 one step         v4 (one step)                    Group   1x2x2  yes    established  -           172257  6      P8x13553
T9 mutants          v4 mutant: no-ledger             Role    1x2x2  yes    established  no loss     630     1      P4x62
T9 mutants          v4 mutant: delete-before-ledger  Group   1x2x2  yes    established  no loss     3365    4      P4x60
```
<!-- END GENERATED: ledger-model -->

读法沿用 007 的 04 第 13 节：T2 是按「任何丢失都要整范围重建」这个更严的口径检查，报出来的全部在实例建成之前，就是 03 第 11 节的 U1；T7 是 `status` 被清空之后退化成只补；T8 去掉了两步删除，报的是 D15 本身；T9 去掉账本、或者先删后写，报的是没有丢失而整范围删除。

这个模型里账本对每个实例都记，与本设计相同。它的组一级只有「按数量补」，没有版本和滚动；那一部分由第 3 节的模型覆盖。两个模型没有合成一个：组一级的滚动和实例一级的恢复在同一个状态空间里的交错没有穷举，靠的是对象的边界（组失败或终止中，里面的一切就停）这条论证。

## 6. 推演的部分

下面这些没有机器检查。

| 内容 | 依据 |
| --- | --- |
| Role 一级的滚动、扩缩容、协调，与账本的衔接 | Role 一级的规则沿用 v2，由 005 的参考模型覆盖。衔接点只有一个：被 Role 一级选中删除的实例，先写 Draining（E4） |
| 原地更新 | 沿用 007 的 05，没有模型 |
| plugin 钩子的次序；`OnRoleDelete` 在离开 Draining 之前调过 | 03 的 E2 |
| 槽位被外来 Pod 占着（L7） | 03 的 E7 |
| `sourceGeneration`；在途的 `spec` 下发 | 02 的 A2、4.5 |
| condition 和计数字段 | 03 第 6 节 |
| `status` 被改写的各种情形 | 03 的 10.5；其中「账本被清空」有 007 的模型 |
| 升级接管里成员一级的步骤：换 owner、待接管的成员、带删除标记的 Pod | [06](06-live-upgrade.md)。组一级的闸门有模型 |
| 两种命名策略 | 02 的 2.2 |
| 用例目录里按规则归类的部分 | 04 第 8 节 |

## 7. 留给真实验证的

在 Kind 上要确认的事，按依赖的对象分。升级的实验另见 [06](06-live-upgrade.md) 第 12 节。

**依赖 Kubernetes 的行为**

1. 前台级联删除一个 ServingGroup：组对象在 Pod、PodGroup、Service 都消失之前一直在；PodGroup 带上 `blockOwnerDeletion` 之后才等它。
2. 对正在前台删除的组再发一次后台的 DELETE，组对象是否立刻消失。
3. 带 UID 和 resourceVersion 前置条件的 DELETE 在对象变过之后返回冲突。
4. 一致性的 LIST（不带 `resourceVersion`）之后，informer 追到这个版本要多久。
5. 用百分比的 PDB 在 owner 是带 `scale` 子资源的 ServingGroup 时怎样算期望数。
6. 删除 ServingGroup 的 CRD 时，实例和它们名下的 Pod 被删的次序。

**依赖实现的行为**

7. 契约 2.12 的全量用例在新实现上跑一遍，按 04 第 8 节改过的断言。先跑 04 里标成「有意变化」和「待定」的。
8. 03 的 10.2 的每一行：在那次写之后杀掉 controller，看新进程的动作。
9. 两个进程并存：关掉「失主退出」，看多建的组被缩回、没有健康的成员被误删。
10. 就绪晚一跳的窗口：一个组失去就绪之后多久 ModelServing controller 才看到；这期间滚动会不会多删一个。
11. 多建的组的 PodGroup 在资源紧张时会不会挤掉别的作业（Volcano）。
12. 写量：稳态下每次就绪变化的 status 写、每个实例建成时的 status 写，对 apiserver 的压力。

## 8. 怎样重跑

在任务目录的 `model/` 下：

```sh
uv run --no-project python run_scenarios.py      # 场景重放：scenario-tables.md、scenario-results.txt
uv run --no-project python run_group.py > group-results.txt      # 组一级穷举，约 10 秒
uv run --no-project python stalemodel.py > stale-results.txt     # 账本的两条规则，约 1 秒
uv run --no-project python run_v4.py > ledger-results.txt        # 007 的组级模型，约 2 分钟
uv run --no-project python ../evidence/resolve_case_verdicts.py  # 用例目录的结论
uv run --no-project python build_docs.py         # 把生成的表写进 04、04a、05
uv run --no-project python build_docs.py --check
```
