# 推论验证

## 0. 文档信息

| 项 | 内容 |
| --- | --- |
| 用途 | 说明 [02](02-design.md)、[03](03-state-machine.md)、[04](04-behavior-expectations.md) 里的结论各自有什么证据：哪些是模型穷举过的，哪些是脚本重放的，哪些是推演，哪些留给真实验证 |
| 模型在哪 | 任务目录 `issues/features/008-modelserving-servinggroup-cr-design-IP/model/`，不在本仓库里。只用 Python 标准库，`uv` 运行。上一版（随机名字、按数量）的模型存档在 `model-random-r3/` |
| 状态 | **没有在集群上运行任何东西。**模型检查的是规则，不是实现 |

## 1. 结论的类别

| 类 | 含义 | 有哪些 |
| --- | --- | --- |
| 模型穷举过的 | 在小规模上把全部交错走了一遍，性质没有被违反；并且去掉对应的规则之后性质会被违反 | 组一级的两个额度、容量上限、缩容先于替换、转正和回收、补最低的空洞、新建的组按序号选版本、失败的组等清空再删；ModelServing controller 的缓存可以落后、可以换进程时，按直读重算不多建、不删错；`status` 晚一跳时的复核；账本的直读（把缺失当证据的那一半）和代际号（删除时的比较）；Role 实例的账本在任何两次写之间停下 |
| 模型里按构造成立的 | 模型的控制流本身不允许违反，所以「没有违反」不是检查出来的；能说明的只是去掉规则会出事 | 升级闸门（放行之前只走接管这一条边） |
| 脚本重放的 | 一条确定的执行过程，或者一个用例之内的全部交错，由规则函数算出，和契约的过程表、runner 的用例比 | 契约 2.12 的 35 个组合场景、RC-01、附录 B 的查表行；runner 的 35 个组合场景用例逐停点；`normal` 套件 SG 模式的 68 例；契约里没有过程表的补充场景（没有成员的组、找回、外来的同名对象、Role 模式下的标记、Pod 全没了的组等） |
| 逐状态对比 | 同一个状态交给两个规则函数，比较这一轮的写 | 组一级的规则函数对 005 的按序号参考模型 |
| 推演 | 按规则写出来的论证，没有机器检查 | 第 6 节 |
| 留给真实验证 | 依赖 Kubernetes 或别的组件的实际行为 | 第 7 节的清单 |

## 2. 场景重放

`grouplevel.py` 是组一级的规则函数：输入是 `spec`、一组 ServingGroup 的状态、无主的成员 Pod 和外来的同名对象，输出是这一轮的写。它是纯函数，在工作副本上迭代（02 的 4.2）。`sim.py` 用它驱动一个确定的模拟器：脚本里写用户改了什么、哪个组就绪了、哪个组消失了，模拟器记下每个阶段的状态，生成过程表。`scenarios.py` 把契约的每个场景写成这样一段脚本，并且把契约的过程表里读出来的可观察量（创建几个组、删除几个组、哪些原有的组保住身份、终态各版本的个数、终态就绪数、终态每个序号上的版本）写在旁边。

### 2.1 契约的场景

- 结论不是手写的：`run_scenarios.py` 比较两边的可观察量，前五项全部相同是「一致」，有一项不同是「不一致」。第六项（终态逐序号的版本）单列。
- 每次健康的组被替换，脚本检查就绪数没有低于 `minAvailable`。
- 04 第 3 节的汇总表、第 4 节的查表行和 04a 的全部过程表由脚本写入；`build_docs.py --check` 校验文档里的表和模型的输出相同。

结果：43 段脚本（35 个场景拆成 38 段，加 RC-01 和 4 个按契约的规则补的场景 EX-1 到 EX-4），**43 段一致，终态逐序号的版本 43 段都与契约的表相同**。上一版是 32 段一致、11 段有意变化。附录 B 的 14 行查表全部相同。

过程上与契约的表有先后不同、而可观察量相同的三处（行为分析逐行对出来的）：SG-C16 最后未就绪的 surge 早一步回收；SG-C16 开头建 surge 和删坏组在同一轮；SG-C15 最后三个旧组契约的表是一次一个，这里是先两个再一个，契约自己的预算允许。

### 2.2 runner 的组合场景用例，逐停点

`run_runner_cases.py` 直接读契约导出里 `servinggroup-compound-v2` 套件的 35 个用例文件（RUN-618 到 RUN-652），没有转写。每一步先做用例的动作，再穷举这一步之内的全部交错：controller 一次一个写、每个写之前重新算计划；终止中的组消失；组就绪（按这一步的放行方式）。停点比的是活动数、就绪数、每个序号上的版本和就绪、哪些序号的对象没有换过、是不是停住了。

<!-- BEGIN GENERATED: runner-cases -->
```text
cases: 35  every checkpoint reached: 35  not reached: 0
checkpoints reached: 188 of 188
not reached: none
checkpoints every execution passes through: 183  some execution does not: 3  not judged (release one): 2
some execution does not, and the step is not one the runner marks sourceState (a set-up it has to catch in time): none
with the fixture of the sourceState steps (a creation at an ordinal the checkpoint does not list is held back): cases with every checkpoint reached: 35  checkpoints every execution passes through: 186  some execution does not: 0  not judged (release one): 2
```
<!-- END GENERATED: runner-cases -->

- 「不判」的 2 个停点是用例里由 runner 自己一个一个放行的步骤，哪一刻放哪一个由 runner 定，只判可达。
- runner 有 4 个步骤是靠夹具布置出来的中间状态（把后面的创建挡住）。不建夹具时其中 3 个不是每一条执行都经过；照 runner 的做法把夹具建进去之后都是必达。
- 行为分析用它自己照规则抄的函数单独重放过其中的 34 例（没有 RUN-652），数的是 184 个停点、全部必达。两边去掉 RUN-652 的 4 个停点之后是同一批停点；差别只在数法（含不含 RUN-652、夹具建不建），不在规则。

runner 的源码不在导出里。放行方式、夹具的含义是从用例的文字和结构读出来的。

### 2.3 `normal` 套件 SG 模式的 68 例

`run_normal_cases.py` 读 `normal` 套件里 SG 模式的「滚动过程中扩缩容」57 例和「滚动过程中再次变更」11 例。这些用例的期望是按序号写的，这一版可以直接比。每一步之内同样穷举全部交错。

<!-- BEGIN GENERATED: normal-cases -->
```text
cases: 68  as expected: 68  not as expected: 0
  1.2.1 scaling during a rollout: 57 of 57   1.2.2 another change during a rollout: 11 of 11
steps that wait for conditions: 49  reached: 49  on every execution: 49
steps that wait until settled: 91  every execution settles: 91
steps with a structured expectation: 57  met in every final state: 57
cases whose final state is only in words: 28  met: 28  (of these, 4 state the number of groups only)
cases with a kept UID in words: 28  met: 28
not as expected: none
```
<!-- END GENERATED: normal-cases -->

- 40 例的最后一步带结构化的期望（逐序号或逐组的版本、「没有一个序号换了对象」）。另 28 例的终态只写在用例的文字里，是手工读成表的，读法在脚本里。
- 重放假设 runner 在 controller 做出反应之后才放行下一个组。去掉这个假设：

<!-- BEGIN GENERATED: normal-ungated -->
```text
cases not as expected: 1
  RUN-200: step 1 expectation: targets want {0: 'A', 1: 'B', 2: 'B'} | final states: 0:A 1:B 2:B || 0:A 1:B 3:B
```
<!-- END GENERATED: normal-ungated -->

- 在组一级看不到、所以没有核对的：每个组里 Pod 的个数、worker 的布局、PodGroup、被保住的组里 Pod 的 UID。

### 2.4 每条规则是不是都有用

把规则函数里的规则一次去掉或改掉一条，四处重放：场景脚本（看可观察量变没变）、停点和查表行（看第一轮的写变没变）、两套 runner 用例（看有没有停点到不了、不再必达、期望不满足）、穷举模型（第 3 节）。

<!-- BEGIN GENERATED: mutants -->
| 去掉或改掉的规则 | 结果变了的场景 | 例子 | 第一轮的写变了的停点、查表行 |
| --- | --- | --- | --- |
| 不转正（G0） | 7：SG-C03、SG-P08、SG-P10、NEW-11、NEW-48、NEW-48b、NEW-18 | SG-C03：surge_left 0 → 1；baseline v2 → v1 | — |
| 转正不看正式的组够不够 N 个（G0，SG 模式） | 1：NEW-13 | NEW-13：survivors g0、g1、g4 → g0、g1、g2；final_by_ordinal sg-0 v2、sg-1 v2、sg-4 v2 → sg-0 v2、sg-1 v2、sg-2 v2 | — |
| Role 模式不摘标记（G0） | 1：NEW-18 | NEW-18：surge_left 0 → 1 | — |
| 失败的组立刻删，不等 Drained（G1） | 3：RC-01、NEW-1、NEW-27 | RC-01：undrained_deletes 0 → 1 | — |
| 缩容和回收的次序按报告的就绪排，不按 Pod 一级的就绪（去掉 V-35） | 1：NEW-34 | NEW-34：survivors g1、g2 → g0、g1；final_ready 2 → 1；final_by_ordinal sg-1 v1、sg-2 v1 → sg-0 v1、sg-1 v1 | — |
| 缩容没做完就建 surge、就替换 | 0： | — | SG-C05 RUN-629、SG-C10 RUN-634、SG-C07 RUN-631、SG-C02 RUN-626、SG-C12 RUN-636 |
| 范围之外没有成员的组不删（G2′(a)） | 5：NEW-47、NEW-35、NEW-46、NEW-50、NEW-54 | NEW-47：creates 1 → 0；deletes 1 → 0；survivors g0 → g0、g3 | — |
| 范围之外没有成员的组不看建不建得出替代的就删（去掉 V-45） | 5：NEW-35、NEW-36、NEW-46、NEW-50、NEW-55 | NEW-35：deletes 0 → 1；survivors g0、g1、g3 → g0、g1；final v1×3 → v1×2 | — |
| G2′(a) 用 V-59 的「补得上」，不看当下的上限和在途的删除（去掉 V-62） | 1：NEW-55 | NEW-55：survivors g0、g3 → g0、g2；final_by_ordinal sg-0 v2、sg-3 v2 → sg-0 v2、sg-2 v2；surge_left 0 → 1 | — |
| 没有成员的组不改版本（G2′(b)） | 7：NEW-6、NEW-9、NEW-10、NEW-41、NEW-40、NEW-21、NEW-44 | NEW-6：creates 0 → 2；deletes 0 → 2；survivors g0、g1、g2 → g0 | — |
| 范围外的正式的空组也按序号改版本（去掉 V-55，上一轮的裁定） | 2：NEW-36、NEW-36b | NEW-36：final v1×3 → v1×2、v2×1；final_by_ordinal sg-0 v1、sg-1 v1、sg-3 v1 → sg-0 v1、sg-1 v1、sg-3 v2；retargets 0 → 1 | — |
| 「没有成员」要求已观测、`status` 新鲜（去掉 V-56） | 1：NEW-44 | NEW-44：creates 3 → 4；deletes 3 → 4；survivors g2 → 无 | — |
| 范围外的没有成员的组里只删空组、只有空组不改版本（去掉 V-61） | 1：NEW-54 | NEW-54：creates 1 → 0；deletes 1 → 0；survivors g0 → g0、g3 | — |
| 没有成员的组也算可替换（V-11） | 0： | 报不出来：G2′(b) 排在 G4、G3 之前：没有成员的组在同一轮里已经被改成按序号该用的版本，不再是可替换的。只有 RecreateRequired 为真并且没有成员的组能看出区别，没有写这样的场景。范围外的正式的空组 G2′(b) 不改（V-55），在这条变异下它算可替换；但它留着正是因为低位补不上，替换同样被 V-54 挡住 | — |
| 补建不取最低空洞，接在最高的序号后面（G4） | 63：SG-S01、SG-S02、SG-S03、SG-S04、SG-S05、SG-S06、SG-S07、SG-C01 等 | SG-S01：final_by_ordinal sg-0 v1、sg-1 v1、sg-2 v1、sg-3 v1 → sg-0 v1、sg-3 v1、sg-4 v1、sg-5 v1 | — |
| 基线读不到时跳到更高的序号去建（G4） | 1：NEW-17 | NEW-17：creates 0 → 1；final v2×2 → v2×3；final_ready 2 → 3 | — |
| 容量只数 live，终止中和失败的不占位（G4、G3a） | 11：SG-C01、SG-C08、SG-C14、RC-01、NEW-5、NEW-7、NEW-30、NEW-31 等 | SG-C01：creates 5 → 6；deletes 3 → 4；max_active 5 → 6 | RU-B11 |
| 新建的组不按序号和 partition 选版本，一律用目标版本（G4、G2′(b)、S1′） | 21：SG-P02、SG-P03（下半）、SG-P06、SG-P11、RC-01、NEW-3、NEW-6、NEW-9 等 | SG-P02：final v1×5、v2×1 → v1×3、v2×3；final_by_ordinal sg-0 v1、sg-1 v1、sg-2 v1、sg-3 v1、sg-4 v1、sg-5 v2 → sg-0 v1、sg-1 v1、sg-2 v1、sg-3 v2、sg-4 v2、sg-5 v2 | — |
| 找回的对象不带 Pod 上的标记（S1′） | 1：NEW-23 | NEW-23：creates 3 → 4；deletes 3 → 4；survivors g2 → 无 | — |
| 名字对不上期望成员的无主 Pod 也算无主的成员 Pod（去掉 V-44） | 3：NEW-37、NEW-39、NEW-38 | NEW-37：creates 2 → 0；deletes 3 → 0；survivors 无 → g0、g1、g2 | — |
| 名下还有无主的 Pod 没收养完的组也是缩容、回收、替换的候选（去掉 V-25） | 3：NEW-15（不收敛）、NEW-32（不收敛）、NEW-33（不收敛） | — | — |
| 滚动结束不回收 surge（G3b） | 19：SG-S04、SG-C03、SG-C12、SG-C16、SG-C13、SG-C14、SG-P10、EX-2 等 | SG-S04：deletes 3 → 2；survivors g3 → g3、g4；final v2×4 → v2×5 | — |
| 回收 surge 不看下限（G3b） | 14：SG-S04、SG-C03、SG-C12、SG-C13、SG-C14、SG-P10、NEW-11、NEW-23 等 | SG-S04：min_ready 4 → 3；min_real_ready 4 → 3；breach 0 → 1 | — |
| 回收 surge 的下限只对确认就绪的检查，未观测的、`status` 不新鲜的直接回收（G3b，V-34、V-26） | 1：NEW-29 | NEW-29：min_real_ready 3 → 2；breach 0 → 1 | — |
| G3b 也删失败的 surge 组 | 1：NEW-27 | NEW-27：undrained_deletes 0 → 1 | — |
| 没有健康额度（G3c） | 7：SG-P09、RC-01、NEW-7、NEW-14、NEW-30、NEW-25、NEW-26 | SG-P09：min_ready 2 → 1；min_real_ready 2 → 1；breach 0 → 2 | RU-B06 |
| 替换正式的组之前不看补不补得上（去掉 V-54） | 6：NEW-42、NEW-43、NEW-45、NEW-49、NEW-52、NEW-53 | NEW-42：deletes 0 → 1；survivors g0、g3 → g0；final v1×2 → v1×1 | — |
| 「补得上」逐个组看，不按整轮算（去掉 V-59） | 3：NEW-45、NEW-48b、NEW-49 | NEW-45：creates 3 → 4；deletes 3 → 4 | — |
| 有滚动要做不看补不补得上，有可替换的正式的组就算（去掉 V-60） | 2：NEW-52、NEW-53 | NEW-52：creates 0 → 1；final v1×2 → v1×2、v2×1；final_ready 2 → 3 | — |
| 未观测的组也是替换的候选（G3c） | 0： | 报不出来：V-34 之后两类候选只收 `status` 新鲜的组：未观测而按未就绪删掉的组，Pod 一定不就绪；按健康的删掉的要过健康额度，而它自己不在 `ready` 里。这条变异只让替换早一轮，不改变任何一段脚本的可观察量；按规则推，也压不到下限以下。「未观测的不是候选」剩下的用处（`status` 说的是旧的 `spec`、原地更新做到一半）不在这个模型里 | — |
| notReadyTarget 不排除可替换的组（V-11） | 1：NEW-16 | NEW-16：creates 1 → 0；deletes 1 → 0；survivors g0、g2 → g0、g1、g2 | — |
| 复核只否决：`status` 未就绪而 Pod 已经就绪的组仍按未就绪的替换、回收（去掉 V-34） | 1：NEW-30 | NEW-30：min_real_ready 2 → 1；breach 0 → 1 | — |
| 不复核：确认就绪、确认未就绪、次序都只看 `status`，不用 Pod 缓存 | 4：NEW-30、NEW-34、NEW-25、NEW-26 | NEW-30：min_real_ready 2 → 1；breach 0 → 1 | — |
| Pod 全没了的组不写 Failed，按组自己的版本补（V-13） | 1：NEW-20 | NEW-20：creates 1 → 0；deletes 1 → 0；survivors g1、g2 → g0、g1、g2 | — |
| 有空组时 G6 也推进基线（去掉 V-36 的后一半） | 1：NEW-40 | NEW-40：final v1×2、v2×1 → v2×3；final_by_ordinal sg-0 v1、sg-1 v1、sg-2 v2 → sg-0 v2、sg-1 v2、sg-2 v2；baseline v1 → v2 | — |
<!-- END GENERATED: mutants -->

runner 的五个停点用例（缩容做完才开始替换）：

<!-- BEGIN GENERATED: stops -->
| 场景 | runner 用例 | 这一步 | runner 的期望 | 本设计这一轮的写 | 去掉「缩容没做完不替换」之后 |
| --- | --- | --- | --- | --- | --- |
| SG-C05 | RUN-629 | 缩容删的两个组还在终止，v2 到来 | 活动 5、就绪 3，不再有动作 | 无 | 删 sg-2（替换）：**停点出不来** |
| SG-C10 | RUN-634 | 缩容删的两个组还在终止，v2 到来（之后还有 v3） | 活动 5、就绪 3，不再有动作 | 无 | 删 sg-2（替换）：**停点出不来** |
| SG-C07 | RUN-631 | 同一请求里 N 5→3 并提交 v2 | 先到活动 3、就绪 3、三个都是旧版本 | 删 sg-4（缩容）；删 sg-3（缩容） | 删 sg-4（缩容）；删 sg-3（缩容）；删 sg-2（替换）：**停点出不来** |
| SG-C02 | RUN-626 | N 3→2，sg-2 是未就绪的 v2 | 先到活动 2、就绪 2、两个旧版本 | 删 sg-2（缩容） | 删 sg-2（缩容）；删 sg-1（替换）：**停点出不来** |
| SG-C12 | RUN-636 | N 4→3，U=0、S=1，sg-4 是临时 surge | 先缩容完成（活动 4、就绪 4），surge 的对象不变 | 删 sg-3（缩容） | 删 sg-3（缩容）；删 sg-2（替换）：**停点出不来** |
<!-- END GENERATED: stops -->

两套 runner 用例上的变异：

<!-- BEGIN GENERATED: runner-mutants -->
```text
不转正（G0）: 0 case(s): -
转正不看正式的组够不够 N 个（G0，SG 模式）: 0 case(s): -
Role 模式不摘标记（G0）: 0 case(s): -
失败的组立刻删，不等 Drained（G1）: 0 case(s): -
缩容和回收的次序按报告的就绪排，不按 Pod 一级的就绪（去掉 V-35）: 0 case(s): -
缩容没做完就建 surge、就替换: 5 case(s): RUN-626 SG-C02 step 2 not on every execution, RUN-629 SG-C05 step 5 not on every execution, RUN-631 SG-C07 step 1 not on every execution, RUN-634 SG-C10 step 5 not on every execution, RUN-636 SG-C12 step 2 not on every execution
范围之外没有成员的组不删（G2′(a)）: 0 case(s): -
范围之外没有成员的组不看建不建得出替代的就删（去掉 V-45）: 0 case(s): -
G2′(a) 用 V-59 的「补得上」，不看当下的上限和在途的删除（去掉 V-62）: 0 case(s): -
没有成员的组不改版本（G2′(b)）: 0 case(s): -
范围外的正式的空组也按序号改版本（去掉 V-55，上一轮的裁定）: 0 case(s): -
「没有成员」要求已观测、`status` 新鲜（去掉 V-56）: 0 case(s): -
范围外的没有成员的组里只删空组、只有空组不改版本（去掉 V-61）: 0 case(s): -
没有成员的组也算可替换（V-11）: 0 case(s): -
补建不取最低空洞，接在最高的序号后面（G4）: 30 case(s): RUN-618 SG-S01 step 3 not reached, RUN-619 SG-S02 step 5 not reached, RUN-620 SG-S03 step 4 not reached, RUN-621 SG-S04 step 1 not reached, RUN-622 SG-S05 step 3 not reached, RUN-623 SG-S06 step 4 not reached, RUN-624 SG-S07 step 2 not reached, RUN-625 SG-C01 step 4 not reached, RUN-626 SG-C02 step 3 not reached, RUN-627 SG-C03 step 3 not reached, RUN-628 SG-C04 step 3 not reached, RUN-629 SG-C05 step 6 not reached, RUN-630 SG-C06 step 2 not reached, RUN-631 SG-C07 step 2 not reached, RUN-632 SG-C08 step 4 not reached, RUN-633 SG-C09 step 4 not reached, RUN-634 SG-C10 step 8 not reached, RUN-636 SG-C12 step 3 not reached, RUN-637 SG-C13 step 3 not reached, RUN-638 SG-C14 step 4 not reached, RUN-639 SG-C15 step 5 not reached, RUN-640 SG-C16 step 3 not reached, RUN-641 SG-P01 step 1 not reached, RUN-643 SG-P03 step 1 not reached, RUN-645 SG-P05 step 1 not reached, RUN-646 SG-P06 step 5 not reached, RUN-647 SG-P07 step 3 not reached, RUN-649 SG-P09 step 3 not reached, RUN-650 SG-P10 step 3 not reached, RUN-652 SG-R01 step 1 not reached
基线读不到时跳到更高的序号去建（G4）: 0 case(s): -
容量只数 live，终止中和失败的不占位（G4、G3a）: 13 case(s): RUN-619 SG-S02 step 3 not on every execution, RUN-622 SG-S05 step 4 not on every execution, RUN-623 SG-S06 step 1 not on every execution, RUN-624 SG-S07 step 1 not on every execution, RUN-625 SG-C01 step 4 not on every execution, RUN-626 SG-C02 step 1 not on every execution, RUN-632 SG-C08 step 2 not on every execution, RUN-634 SG-C10 step 6 not on every execution, RUN-638 SG-C14 step 4 not on every execution, RUN-639 SG-C15 step ? not reached, RUN-640 SG-C16 step 3 not on every execution, RUN-646 SG-P06 step 2 not on every execution, RUN-647 SG-P07 step 2 not on every execution
新建的组不按序号和 partition 选版本，一律用目标版本（G4、G2′(b)、S1′）: 4 case(s): RUN-642 SG-P02 step 2 not reached, RUN-643 SG-P03 step 7 not reached, RUN-646 SG-P06 step 5 not reached, RUN-651 SG-P11 step 3 not reached
找回的对象不带 Pod 上的标记（S1′）: 0 case(s): -
名字对不上期望成员的无主 Pod 也算无主的成员 Pod（去掉 V-44）: 0 case(s): -
名下还有无主的 Pod 没收养完的组也是缩容、回收、替换的候选（去掉 V-25）: 0 case(s): -
滚动结束不回收 surge（G3b）: 7 case(s): RUN-621 SG-S04 step 1 not reached, RUN-627 SG-C03 step 3 not reached, RUN-636 SG-C12 step 3 not reached, RUN-637 SG-C13 step 3 not reached, RUN-638 SG-C14 step 6 not reached, RUN-640 SG-C16 step 5 not reached, RUN-650 SG-P10 step 3 not reached
回收 surge 不看下限（G3b）: 0 case(s): -
回收 surge 的下限只对确认就绪的检查，未观测的、`status` 不新鲜的直接回收（G3b，V-34、V-26）: 0 case(s): -
G3b 也删失败的 surge 组: 0 case(s): -
没有健康额度（G3c）: 1 case(s): RUN-649 SG-P09 step 1 not reached
替换正式的组之前不看补不补得上（去掉 V-54）: 0 case(s): -
「补得上」逐个组看，不按整轮算（去掉 V-59）: 0 case(s): -
有滚动要做不看补不补得上，有可替换的正式的组就算（去掉 V-60）: 0 case(s): -
未观测的组也是替换的候选（G3c）: 0 case(s): -
notReadyTarget 不排除可替换的组（V-11）: 0 case(s): -
复核只否决：`status` 未就绪而 Pod 已经就绪的组仍按未就绪的替换、回收（去掉 V-34）: 0 case(s): -
不复核：确认就绪、确认未就绪、次序都只看 `status`，不用 Pod 缓存: 0 case(s): -
Pod 全没了的组不写 Failed，按组自己的版本补（V-13）: 0 case(s): -
有空组时 G6 也推进基线（去掉 V-36 的后一半）: 0 case(s): -
```
<!-- END GENERATED: runner-mutants -->

<!-- BEGIN GENERATED: normal-mutants -->
```text
不转正（G0）: 0 case(s): -
转正不看正式的组够不够 N 个（G0，SG 模式）: 0 case(s): -
Role 模式不摘标记（G0）: 0 case(s): -
失败的组立刻删，不等 Drained（G1）: 0 case(s): -
缩容和回收的次序按报告的就绪排，不按 Pod 一级的就绪（去掉 V-35）: 0 case(s): -
缩容没做完就建 surge、就替换: 0 case(s): -
范围之外没有成员的组不删（G2′(a)）: 0 case(s): -
范围之外没有成员的组不看建不建得出替代的就删（去掉 V-45）: 0 case(s): -
G2′(a) 用 V-59 的「补得上」，不看当下的上限和在途的删除（去掉 V-62）: 0 case(s): -
没有成员的组不改版本（G2′(b)）: 0 case(s): -
范围外的正式的空组也按序号改版本（去掉 V-55，上一轮的裁定）: 0 case(s): -
「没有成员」要求已观测、`status` 新鲜（去掉 V-56）: 0 case(s): -
范围外的没有成员的组里只删空组、只有空组不改版本（去掉 V-61）: 0 case(s): -
没有成员的组也算可替换（V-11）: 0 case(s): -
补建不取最低空洞，接在最高的序号后面（G4）: 49 case(s): RUN-162, RUN-165, RUN-166, RUN-167, RUN-170, RUN-171, RUN-172, RUN-175, RUN-176, RUN-177, RUN-178, RUN-195, RUN-196, RUN-197, RUN-199, RUN-200, RUN-207, RUN-208, RUN-209, RUN-212, RUN-221, RUN-222, RUN-227, RUN-228, RUN-229, RUN-230, RUN-235, RUN-236, RUN-237, RUN-238, RUN-243, RUN-245, RUN-251, RUN-252, RUN-253, RUN-254, RUN-263, RUN-264, RUN-271, RUN-272, RUN-275, RUN-276, RUN-277, RUN-278, RUN-279, RUN-280, RUN-281, RUN-282, RUN-284
基线读不到时跳到更高的序号去建（G4）: 0 case(s): -
容量只数 live，终止中和失败的不占位（G4、G3a）: 27 case(s): RUN-166, RUN-167, RUN-176, RUN-177, RUN-178?, RUN-195, RUN-196, RUN-197, RUN-198, RUN-199, RUN-200, RUN-221, RUN-222, RUN-227, RUN-228, RUN-237, RUN-243?, RUN-244?, RUN-245?, RUN-246?, RUN-251?, RUN-252?, RUN-275, RUN-277, RUN-279, RUN-281, RUN-284
新建的组不按序号和 partition 选版本，一律用目标版本（G4、G2′(b)、S1′）: 1 case(s): RUN-210
找回的对象不带 Pod 上的标记（S1′）: 0 case(s): -
名字对不上期望成员的无主 Pod 也算无主的成员 Pod（去掉 V-44）: 0 case(s): -
名下还有无主的 Pod 没收养完的组也是缩容、回收、替换的候选（去掉 V-25）: 0 case(s): -
滚动结束不回收 surge（G3b）: 20 case(s): RUN-162, RUN-170, RUN-171, RUN-172, RUN-175, RUN-176, RUN-177, RUN-178, RUN-221, RUN-222, RUN-235, RUN-236, RUN-276, RUN-277, RUN-278, RUN-279, RUN-280, RUN-281, RUN-282, RUN-283
回收 surge 不看下限（G3b）: 0 case(s): -
回收 surge 的下限只对确认就绪的检查，未观测的、`status` 不新鲜的直接回收（G3b，V-34、V-26）: 0 case(s): -
G3b 也删失败的 surge 组: 0 case(s): -
没有健康额度（G3c）: 0 case(s): -
替换正式的组之前不看补不补得上（去掉 V-54）: 0 case(s): -
「补得上」逐个组看，不按整轮算（去掉 V-59）: 0 case(s): -
有滚动要做不看补不补得上，有可替换的正式的组就算（去掉 V-60）: 0 case(s): -
未观测的组也是替换的候选（G3c）: 0 case(s): -
notReadyTarget 不排除可替换的组（V-11）: 0 case(s): -
复核只否决：`status` 未就绪而 Pod 已经就绪的组仍按未就绪的替换、回收（去掉 V-34）: 0 case(s): -
不复核：确认就绪、确认未就绪、次序都只看 `status`，不用 Pod 缓存: 0 case(s): -
Pod 全没了的组不写 Failed，按组自己的版本补（V-13）: 0 case(s): -
有空组时 G6 也推进基线（去掉 V-36 的后一半）: 0 case(s): -
```
<!-- END GENERATED: normal-mutants -->

如实写的几点：

- **转正在两套 runner 用例里都看不出来。**去掉转正，35 例和 68 例全部照样通过：没转正的组带着标记留在范围里，从 Pod 看与转正了没有区别。它靠场景脚本（surge 留没留下）和穷举的性质报出来。现有的用例考的是「没有重建」，考不到「标记摘掉了」；要新增断言（04 第 8 节）。
- **只有一条脚本路径作证的规则**：转正的第二个条件；Role 模式下摘标记；基线读不到时不跳到更高的序号；找回的对象带不带标记；待收养的组不参加缩容；回收 surge 时对没有确认的组也看下限；未观测的组不是替换的候选；`notReadyTarget` 不数可替换的组；Pod 全没了的组写 Failed。这些规则没有进穷举模型。
- **「没有成员的组也算可替换」这条变异报不出来。**没有成员的组在同一轮里先被改成按序号该用的版本，不再是可替换的；「可替换要求有成员」只在 `RecreateRequired` 为真并且没有成员时有区别，没有这样的场景。

### 2.5 与 005 的参考模型逐状态对比

组一级的规则函数与 [005](../modelserving-rollout-final/README.md) 的按序号参考模型（它是 v2 和契约 2.12 的规则的可执行版本）在随机状态上逐个比较这一轮的写。只比两边都有的部分：每个组已观测、`status` 新鲜、有成员、没有无主的 Pod、没有外来对象。

两边的规则有一处不同：转正的第二个条件（正式的组少于 N）参考模型没有。这个条件起作用的状态（范围里有带标记的组，而正式的组已经有 N 个），带着它比全部不同：参考模型转正，这里不转正；其中一部分只差那个标记，其余的这一轮的删除或创建也跟着不同。所以这些状态是关掉第二个条件之后比的，个数在下面的输出里。结论是：**除转正的第二个条件之外，200 000 个状态相同。**第二个条件在规则自己走得到的状态里总是成立（02 的 4.2），它由场景脚本作证，不由这个对比作证。

<!-- BEGIN GENERATED: reference-diff -->
```text
states: 200000  seed: 8
same objects and baseline after the round: 200000
different: 0
compared without the count condition of G0 (a marked group in range while N regular groups exist), included above: 25451
  the same 25451 states compared with the condition (the rules as decided): same 0, different 25451
    in every different one the reference model promoted a marked group that keeps its mark here: yes (25451)
    nothing else differs: 7445
    the deletions of the round differ as well (one model has a regular group more than the other): 17293
    a creation differs as well: 1859
    different in any other way (a revision, the baseline, a mark the other way round): 0
```
<!-- END GENERATED: reference-diff -->

### 2.6 契约里没有过程表的场景

过程表在 [04a](04a-scenario-process-tables.md) 第 2 节，每段是脚本的一条路径，不与契约比。它们覆盖这一版自己的规则：整组恢复在原序号上重建；没有成员的组改版本、范围外的空组；转正的两个条件；Pod 全没了的组；对象建了而 Pod 建不出来时提交新模板；序号被外来的对象占着；找回和待收养；Role 模式下留着的标记；基线读不到；直读和复核的几个单轮。

按规则的字面、或者按另一种读法会出什么事的场景：

<!-- BEGIN GENERATED: findings -->
| 场景 | 观察到的 | 反例 |
| --- | --- | --- |
| （没有） | — | — |
| （换一种规则）V-25 的另一种读法：待收养的组也计入缩容的个数，缩容从其余的组里选（不是定稿的读法） | 结果变了的场景：NEW-15、NEW-33 | — |
| （换一种规则）V-59 按字面：算「补得上」时 G4 也停在 `active` = N + S（已经裁定不取） | 结果变了的场景：NEW-51 | — |
| （换一种规则）V-62 的第 1 条按字面：只有终止中的、失败的组算删除在途，删完了还没补上的位置不算（没有取这种读法，请确认） | 结果变了的场景：NEW-46、NEW-50 | — |
| （换一种规则）「补得上」不按 V-59 的定义算，用第五轮的计数法：一个空闲序号只算给一处用，但不把转正算作着落（不是定稿的定义） | 结果变了的场景：NEW-48b | — |
<!-- END GENERATED: findings -->

## 3. 组一级的穷举模型

`gmodel.py`、`run_group.py`。规则就是 `grouplevel.py` 的规则函数；这个模型问的是另一件事：ModelServing controller 拿着它**看到的东西**去执行这些规则时会怎样。性质全部按对象和 Pod 的真实状态判断，算式在模型里另写，不用被检查的规则自己的分类。

### 3.1 建了什么

| 项 | 内容 |
| --- | --- |
| 名字 | 确定，创建幂等：遇到同名对象是「已经存在」，不产生新对象。删除带 UID 和 resourceVersion，转正带 UID：对象换过或者变过，写被拒绝 |
| 进程里的记忆 | 没有。不记发过什么 |
| 缓存 | 组对象的一个过去的快照，可以落后若干个事件。`spec` 总是最新的 |
| 换进程 | 任何一次写之后可以换进程，这一轮剩下的写丢掉。新进程的缓存可以比旧进程的还旧（旧的 LIST） |
| 发写的方式 | **按直读重算**（本设计，02 的 6.3）：按缓存算计划；有写要发，就读最新的组和 `spec`，按读到的重算，发重算出来的写；按缓存算出来没有写的轮次不读、不写。**按缓存直接发**（变异） |
| 就绪 | 两个量。一个是按组现在的 Pod 算出来的就绪（下面叫 Pod 一级的就绪）；另一个是 `status` 里写着的 Ready，可以晚一跳，没有人写的时候不变。两个量不同，就是 `status` 不新鲜。ModelServing controller 的 Pod 缓存按最新算 |
| 环境 | 组的 Pod 就绪、掉就绪；组失败、清空、消失；用户改目标版本、N、P |
| 升级接管 | 升级之前的组变成对象，surge 标记搬到对象上；接管完成之前不创建、不删除 |

范围：N 从 2 到 4；U、S 取 (1, 0)、(0, 1)、(1, 1)；缓存落后至多 2 个事件（两个形状 3 个）；换进程至多 2 次；掉就绪至多 2 次、失败至多 1 次；`status` 至多晚一跳。形状：一次滚动；滚动途中再换目标、换走又换回；滚动途中 N 调大再调小；partition 调小再调大；起不来的目标版本；滚动加掉就绪和失败；灰度停点上的失败和第三个版本；升级接管的四种起点；转正；稀疏的集合 `{0, 3}`；`status` 晚一跳。

| 性质 | 内容 |
| --- | --- |
| PA | Pod 就绪的组被当作健康的删掉（替换、回收 surge）之后，真实的就绪数不低于 N − U |
| PN | Pod 就绪的组被当作未就绪的删掉之后，真实的就绪数不低于 N − U |
| PR | 一轮之后就绪数不低于 min(之前的就绪数, N − U) |
| PB | 被替换的组确实可替换；缩容只在正式的运行的组多于 N 时发生；只回收带标记的组 |
| PS | 正式的组多于 N 时不替换、不建 surge |
| PM | 只在 `maxScaleDown` 大于 0 时替换 |
| PC | 每一次写之后 `active` 不超过 max(N + S, 这次写之前的 `active`) |
| PD | 接管完成之前不创建、不删除组 |
| PE | 静止的状态是完整的：没有失败的、终止中的、没接管的；没有带标记的组，除非滚动被挡住；全部就绪时正式的组恰好 N 个、可替换的为零 |
| PG | 失败的组清空之后才删 |
| PO | 初始序号连续、途中 N 没有调小过时，静止并且完成的状态里序号集合是 `[0, N)` |
| PQ | 缩容和回收选中的组，在候选里没有比它更该先删的（Pod 一级不就绪的先） |
| PW | 缩容单独把就绪数压到 min(之前的就绪数, N − U) 以下，而被缩掉的是 Pod 就绪的组、留下的有 Pod 不就绪的 |
| PV | 新建的正式的组的版本按序号：小于 P 并且 B ≠ T 用 B，否则 T |
| CYCLE、PF | 没有活锁；组数有界 |

### 3.2 结果

<!-- BEGIN GENERATED: group-model -->
```text
scenario                                                     rules                  spec             cache and status                                                              note                                                                           states  sinks  outcomes  findings
A G0 rollout, rounds never cut                               final                  N=3 U=1 S=0 P=0  exact, no takeover                                                            -                                                                              14      1      1         -
A G0 rollout, rounds never cut                               final                  N=3 U=0 S=1 P=0  exact, no takeover                                                            -                                                                              18      1      1         -
A G0 rollout, rounds never cut                               final                  N=3 U=1 S=1 P=0  exact, no takeover                                                            -                                                                              80      1      1         -
A G1 rollout                                                 final                  N=3 U=1 S=0 P=0  exact, 2 takeover(s)                                                          -                                                                              14      1      1         -
A G1 rollout                                                 final                  N=3 U=0 S=1 P=0  exact, 2 takeover(s)                                                          -                                                                              18      1      1         -
A G1 rollout                                                 final                  N=3 U=1 S=1 P=0  exact, 2 takeover(s)                                                          -                                                                              234     3      1         -
A G2 two rollouts                                            final                  N=3 U=1 S=0 P=0  exact, 1 takeover(s)                                                          -                                                                              83      3      3         -
A G2 two rollouts                                            final                  N=3 U=0 S=1 P=0  exact, 1 takeover(s)                                                          -                                                                              126     3      3         -
A G2 two rollouts                                            final                  N=3 U=1 S=1 P=0  exact, 1 takeover(s)                                                          -                                                                              1462    5      3         -
A G2b target moved on and back                               final                  N=3 U=1 S=0 P=0  exact, 1 takeover(s)                                                          v2 rollout half done                                                           210     3      2         -
A G2b target moved on and back                               final                  N=3 U=0 S=1 P=0  exact, 1 takeover(s)                                                          v2 rollout half done                                                           337     3      2         -
A G2b target moved on and back                               final                  N=3 U=1 S=1 P=0  exact, 1 takeover(s)                                                          v2 rollout half done                                                           4054    6      2         -
A G3 scale during rollout                                    final                  N=2 U=1 S=0 P=0  exact, 1 takeover(s)                                                          -                                                                              131     9      4         - (notes: W-SRx7, W-SPx4)
A G3 scale during rollout                                    final                  N=2 U=0 S=1 P=0  exact, 1 takeover(s)                                                          -                                                                              233     9      4         - (notes: W-SRx19, W-SPx4)
A G3 scale during rollout                                    final                  N=2 U=1 S=1 P=0  exact, 1 takeover(s)                                                          -                                                                              771     10     4         - (notes: W-SRx20, W-SPx4)
A G4 partition                                               final                  N=3 U=1 S=0 P=2  exact, 1 takeover(s)                                                          -                                                                              25      5      5         -
A G4 partition                                               final                  N=3 U=0 S=1 P=2  exact, 1 takeover(s)                                                          -                                                                              44      5      5         -
A G4 partition                                               final                  N=3 U=1 S=1 P=2  exact, 1 takeover(s)                                                          -                                                                              301     12     6         -
A G5 bad target                                              final                  N=3 U=1 S=0 P=0  exact, 1 takeover(s)                                                          -                                                                              20      2      2         -
A G5 bad target                                              final                  N=3 U=0 S=1 P=0  exact, 1 takeover(s)                                                          -                                                                              22      2      2         -
A G5 bad target                                              final                  N=3 U=1 S=1 P=0  exact, 1 takeover(s)                                                          -                                                                              250     4      2         -
A G6 faults                                                  final                  N=3 U=1 S=0 P=0  exact, 1 takeover(s)                                                          -                                                                              951     6      1         -
A G6 faults                                                  final                  N=3 U=0 S=1 P=0  exact, 1 takeover(s)                                                          -                                                                              2149    7      1         -
A G6 faults                                                  final                  N=3 U=1 S=1 P=0  exact, 1 takeover(s)                                                          -                                                                              7074    8      1         -
A G7 failure at a canary stop                                final                  N=3 U=1 S=0 P=1  exact, no takeover                                                            -                                                                              17      2      1         -
A G8 adoption                                                final                  N=3 U=1 S=0 P=0  exact, 1 takeover(s)                                                          -                                                                              21      2      2         -
A G8 adoption                                                final                  N=3 U=0 S=1 P=0  exact, 1 takeover(s)                                                          -                                                                              25      2      2         -
A G8 adoption                                                final                  N=3 U=1 S=1 P=0  exact, 1 takeover(s)                                                          -                                                                              170     3      2         -
A G8 adoption                                                final                  N=3 U=1 S=0 P=1  exact, 1 takeover(s)                                                          canary in progress                                                             13      2      2         -
A G8 adoption                                                final                  N=2 U=0 S=1 P=0  exact, 1 takeover(s)                                                          rollout in progress, sg-2 carries the surge mark                               16      1      1         -
A G8 adoption                                                final                  N=2 U=0 S=1 P=0  exact, 1 takeover(s)                                                          the adopted surge group is promoted when N is raised                           77      3      2         -
A G9 third revision at a canary stop                         final                  N=3 U=1 S=0 P=1  exact, 1 takeover(s)                                                          -                                                                              170     5      2         -
A G9 third revision at a canary stop                         final                  N=3 U=0 S=1 P=1  exact, 1 takeover(s)                                                          -                                                                              319     5      2         -
A G9 third revision at a canary stop                         final                  N=3 U=1 S=1 P=1  exact, 1 takeover(s)                                                          -                                                                              1152    6      2         -
A G10 promotion                                              final                  N=3 U=0 S=1 P=0  exact, 1 takeover(s)                                                          N raised during a rollout, then lowered                                        333     11     5         - (notes: W-SRx24, W-SPx6)
A G10 promotion                                              final                  N=3 U=1 S=1 P=0  exact, 1 takeover(s)                                                          N raised during a rollout, then lowered                                        1540    12     5         - (notes: W-SRx25, W-SPx6)
A G11 sparse set                                             final                  N=2 U=1 S=0 P=0  exact, 1 takeover(s)                                                          {0, 3}, rollout and two scale-ups                                              175     5      3         -
A G11 sparse set                                             final                  N=2 U=0 S=1 P=0  exact, 1 takeover(s)                                                          {0, 3}, rollout and two scale-ups                                              333     5      3         -
A G11 sparse set                                             final                  N=2 U=1 S=1 P=0  exact, 1 takeover(s)                                                          {0, 3}, rollout and two scale-ups                                              1129    6      3         -
A G11 sparse set                                             final                  N=2 U=1 S=0 P=0  exact, 1 takeover(s)                                                          {0, 3}, sg-3 is healthy and of the target revision                             31      3      2         - (notes: W-SPx3)
A G11 sparse set                                             final                  N=2 U=1 S=0 P=2  exact, 1 takeover(s)                                                          {0, 3}, P = N (SG-P11)                                                         13      2      2         -
D G1 rollout                                                 final                  N=3 U=1 S=0 P=0  up to 2 behind, read before writing, 2 takeover(s)                            -                                                                              112     3      1         -
D G1 rollout                                                 final                  N=3 U=0 S=1 P=0  up to 2 behind, read before writing, 2 takeover(s)                            -                                                                              148     3      1         -
D G1 rollout                                                 final                  N=3 U=1 S=1 P=0  up to 2 behind, read before writing, 2 takeover(s)                            -                                                                              1724    22     1         -
D G2 two rollouts                                            final                  N=3 U=1 S=0 P=0  up to 2 behind, read before writing, 2 takeover(s)                            -                                                                              816     9      3         -
D G2 two rollouts                                            final                  N=3 U=0 S=1 P=0  up to 2 behind, read before writing, 2 takeover(s)                            -                                                                              1236    7      3         -
D G2 two rollouts                                            final                  N=3 U=1 S=1 P=0  up to 2 behind, read before writing, 2 takeover(s)                            -                                                                              18400   57     3         -
D G2b target moved on and back                               final                  N=3 U=1 S=0 P=0  up to 2 behind, read before writing, 2 takeover(s)                            v2 rollout half done                                                           2544    15     2         -
D G2b target moved on and back                               final                  N=3 U=0 S=1 P=0  up to 2 behind, read before writing, 2 takeover(s)                            v2 rollout half done                                                           3946    13     2         -
D G3 scale during rollout                                    final                  N=2 U=1 S=0 P=0  up to 2 behind, read before writing, 2 takeover(s)                            -                                                                              1401    35     4         - (notes: W-SRx92, W-SPx12)
D G3 scale during rollout                                    final                  N=2 U=0 S=1 P=0  up to 2 behind, read before writing, 2 takeover(s)                            -                                                                              2312    48     4         - (notes: W-SRx207, W-SPx19)
D G3 scale during rollout                                    final                  N=2 U=1 S=1 P=0  up to 2 behind, read before writing, 2 takeover(s)                            -                                                                              12333   184    4         - (notes: W-SRx609, W-SPx79)
D G4 partition                                               final                  N=3 U=1 S=0 P=2  up to 2 behind, read before writing, 2 takeover(s)                            -                                                                              202     18     5         -
D G4 partition                                               final                  N=3 U=0 S=1 P=2  up to 2 behind, read before writing, 2 takeover(s)                            -                                                                              458     20     5         -
D G6 faults                                                  final                  N=3 U=1 S=0 P=0  up to 2 behind, read before writing, 1 takeover(s)                            -                                                                              16830   33     1         -
D G6 faults                                                  final                  N=3 U=0 S=1 P=0  up to 2 behind, read before writing, 1 takeover(s)                            -                                                                              41433   57     1         -
D G8 adoption                                                final                  N=3 U=1 S=0 P=0  up to 2 behind, read before writing, 1 takeover(s)                            -                                                                              109     4      2         -
D G8 adoption                                                final                  N=3 U=0 S=1 P=0  up to 2 behind, read before writing, 1 takeover(s)                            -                                                                              133     4      2         -
D G8 adoption                                                final                  N=3 U=1 S=1 P=0  up to 2 behind, read before writing, 1 takeover(s)                            -                                                                              1045    13     2         -
D G8s adoption                                               final                  N=2 U=0 S=1 P=0  up to 2 behind, read before writing, 1 takeover(s)                            the adopted surge group is promoted when N is raised                           438     8      2         -
D G10 promotion                                              final                  N=3 U=0 S=1 P=0  up to 2 behind, read before writing, 1 takeover(s)                            N raised during a rollout, then lowered                                        1917    28     5         - (notes: W-SRx156, W-SPx12)
D G10 promotion                                              final                  N=3 U=1 S=1 P=0  up to 2 behind, read before writing, 1 takeover(s)                            N raised during a rollout, then lowered                                        14181   134    5         - (notes: W-SRx498, W-SPx64)
D G11 sparse set                                             final                  N=2 U=1 S=0 P=0  up to 2 behind, read before writing, 1 takeover(s)                            {0, 3}, rollout and two scale-ups                                              1235    16     3         -
D G11 sparse set                                             final                  N=2 U=0 S=1 P=0  up to 2 behind, read before writing, 1 takeover(s)                            {0, 3}, rollout and two scale-ups                                              2240    22     3         -
D G2 two rollouts                                            final                  N=3 U=1 S=0 P=0  up to 3 behind, read before writing, 2 takeover(s)                            -                                                                              1112    11     3         -
D G3 scale during rollout                                    final                  N=2 U=1 S=0 P=0  up to 3 behind, read before writing, 2 takeover(s)                            -                                                                              2427    49     4         - (notes: W-SRx148, W-SPx16)
D G2 two rollouts                                            final                  N=3 U=0 S=1 P=0  up to 3 behind, read before writing, 2 takeover(s)                            -                                                                              1689    9      3         -
D G3 scale during rollout                                    final                  N=2 U=0 S=1 P=0  up to 3 behind, read before writing, 2 takeover(s)                            -                                                                              3977    66     4         - (notes: W-SRx339, W-SPx22)
B G1 rollout                                                 no-verify              N=3 U=1 S=0 P=0  up to 2 behind, NO read, 2 takeover(s)                                        -                                                                              108     3      1         - (notes: W-RJx18, W-AEx20)
B G1 rollout                                                 no-verify              N=3 U=0 S=1 P=0  up to 2 behind, NO read, 2 takeover(s)                                        -                                                                              142     3      1         - (notes: W-AEx24, W-RJx26)
B G1 rollout                                                 no-verify              N=3 U=1 S=1 P=0  up to 2 behind, NO read, 2 takeover(s)                                        -                                                                              1974    22     1         - (notes: W-AEx423, W-RJx345)
B G2 two rollouts                                            no-verify              N=3 U=1 S=0 P=0  up to 2 behind, NO read, 2 takeover(s)                                        -                                                                              1340    9      3         PAx12, PLx12, PMx12, PRx12 (notes: W-RJx159, W-AEx310)
B G2 two rollouts                                            no-verify              N=3 U=0 S=1 P=0  up to 2 behind, NO read, 2 takeover(s)                                        -                                                                              2036    7      3         PAx18, PLx18, PMx18, PRx18 (notes: W-AEx460, W-RJx243)
B G2 two rollouts                                            no-verify              N=3 U=1 S=1 P=0  up to 2 behind, NO read, 2 takeover(s)                                        -                                                                              35905   70     3         PAx126, PLx126, PMx126, PRx123 (notes: W-AEx8699, W-RJx4691)
B G2b target moved on and back                               no-verify              N=3 U=1 S=0 P=0  up to 2 behind, NO read, 2 takeover(s)                                        v2 rollout half done                                                           6457    23     2         PAx88, PLx88, PMx88, PRx88 (notes: W-RJx627, W-AEx1575)
B G2b target moved on and back                               no-verify              N=3 U=0 S=1 P=0  up to 2 behind, NO read, 2 takeover(s)                                        v2 rollout half done                                                           9698    31     2         PAx126, PLx126, PMx114, PRx126 (notes: W-AEx2222, W-RJx1093)
B G3 scale during rollout                                    no-verify              N=2 U=1 S=0 P=0  up to 2 behind, NO read, 2 takeover(s)                                        -                                                                              2195    47     4         PSx26, PBx34, PCx14, PAx6, PLx6, PMx6, PRx6 (notes: W-RJx292, W-AEx310, W-SRx46, W-SPx18)
B G3 scale during rollout                                    no-verify              N=2 U=0 S=1 P=0  up to 2 behind, NO read, 2 takeover(s)                                        -                                                                              5580    66     4         PSx29, PBx66, PRx63, PCx17, PAx29, PLx29, PMx9 (notes: W-AEx534, W-RJx1335, W-SRx273, W-SPx24)
B G3 scale during rollout                                    no-verify              N=2 U=1 S=1 P=0  up to 2 behind, NO read, 2 takeover(s)                                        -                                                                              25085   207    4         PCx145, PSx136, PBx178, PRx23, PAx20, PLx20, PMx8 (notes: W-AEx2881, W-RJx5872, W-SRx478, W-SPx96)
B G4 partition                                               no-verify              N=3 U=1 S=0 P=2  up to 2 behind, NO read, 2 takeover(s)                                        -                                                                              200     18     5         - (notes: W-RJx18, W-AEx42)
B G4 partition                                               no-verify              N=3 U=0 S=1 P=2  up to 2 behind, NO read, 2 takeover(s)                                        -                                                                              1385    48     5         PAx20, PLx20, PRx20, PMx6 (notes: W-AEx231, W-RJx251)
B G6 faults                                                  no-verify              N=3 U=1 S=0 P=0  up to 1 behind, NO read, 1 takeover(s)                                        -                                                                              7318    14     1         PAx68, PLx68, PRx68, PMx92 (notes: W-RJx479, W-AEx611)
B G6 faults                                                  no-verify              N=3 U=0 S=1 P=0  up to 1 behind, NO read, 1 takeover(s)                                        -                                                                              16052   19     1         PAx132, PLx132, PRx132, PMx144 (notes: W-AEx1317, W-RJx1125)
B G8 adoption                                                no-verify              N=3 U=1 S=0 P=0  up to 2 behind, NO read, 1 takeover(s)                                        -                                                                              104     4      2         - (notes: W-AEx22, W-RJx12)
B G8 adoption                                                no-verify              N=3 U=0 S=1 P=0  up to 2 behind, NO read, 1 takeover(s)                                        -                                                                              126     4      2         - (notes: W-AEx25, W-RJx17)
B G8 adoption                                                no-verify              N=3 U=1 S=1 P=0  up to 2 behind, NO read, 1 takeover(s)                                        -                                                                              1202    13     2         - (notes: W-AEx275, W-RJx215)
B G8s adoption                                               no-verify              N=2 U=0 S=1 P=0  up to 2 behind, NO read, 1 takeover(s)                                        the adopted surge group is promoted when N is raised                           616     11     2         - (notes: W-AEx137, W-RJx83)
B G10 promotion                                              no-verify              N=3 U=0 S=1 P=0  up to 2 behind, NO read, 1 takeover(s)                                        N raised during a rollout, then lowered                                        4666    37     5         PSx29, PBx63, PRx54, PCx17, PAx23, PLx23, PMx10 (notes: W-AEx539, W-RJx1036, W-SRx193, W-SPx15)
B G10 promotion                                              no-verify              N=3 U=1 S=1 P=0  up to 2 behind, NO read, 1 takeover(s)                                        N raised during a rollout, then lowered                                        29742   159    5         PCx211, PSx283, PBx224, PRx30, PAx25, PLx25, PMx17 (notes: W-AEx4204, W-RJx6188, W-SRx379, W-SPx85)
B G11 sparse set                                             no-verify              N=2 U=1 S=0 P=0  up to 2 behind, NO read, 1 takeover(s)                                        {0, 3}, rollout and two scale-ups                                              1271    16     3         - (notes: W-RJx28, W-AEx412)
B G11 sparse set                                             no-verify              N=2 U=0 S=1 P=0  up to 2 behind, NO read, 1 takeover(s)                                        {0, 3}, rollout and two scale-ups                                              8781    44     3         PCx20, PSx7, PAx12, PLx12, PMx12, PRx12 (notes: W-AEx1624, W-RJx1009, W-SRx38)
B+ G1 rollout                                                final                  N=3 U=1 S=0 P=0  up to 2 behind, NO read, 2 takeover(s)                                        -                                                                              108     3      1         - (notes: W-AEx20)
B+ G1 rollout                                                final                  N=3 U=0 S=1 P=0  up to 2 behind, NO read, 2 takeover(s)                                        -                                                                              142     3      1         - (notes: W-AEx24, W-RJx8)
B+ G1 rollout                                                final                  N=3 U=1 S=1 P=0  up to 2 behind, NO read, 2 takeover(s)                                        -                                                                              1974    22     1         - (notes: W-AEx423, W-RJx176)
B+ G2 two rollouts                                           final                  N=3 U=1 S=0 P=0  up to 2 behind, NO read, 2 takeover(s)                                        -                                                                              783     9      3         - (notes: W-AEx144, W-RJx20)
B+ G2 two rollouts                                           final                  N=3 U=0 S=1 P=0  up to 2 behind, NO read, 2 takeover(s)                                        -                                                                              1188    7      3         - (notes: W-AEx208, W-RJx50)
B+ G2 two rollouts                                           final                  N=3 U=1 S=1 P=0  up to 2 behind, NO read, 2 takeover(s)                                        -                                                                              22435   58     3         - (notes: W-AEx4719, W-RJx1602)
B+ G2b target moved on and back                              final                  N=3 U=1 S=0 P=0  up to 2 behind, NO read, 2 takeover(s)                                        v2 rollout half done                                                           2431    15     2         - (notes: W-AEx448, W-RJx112)
B+ G2b target moved on and back                              final                  N=3 U=0 S=1 P=0  up to 2 behind, NO read, 2 takeover(s)                                        v2 rollout half done                                                           3775    13     2         - (notes: W-AEx660, W-RJx228)
B+ G3 scale during rollout                                   final                  N=2 U=1 S=0 P=0  up to 2 behind, NO read, 2 takeover(s)                                        -                                                                              1798    41     4         PSx26, PBx18, PCx14 (notes: W-AEx218, W-RJx213, W-SRx44, W-SPx18)
B+ G3 scale during rollout                                   final                  N=2 U=0 S=1 P=0  up to 2 behind, NO read, 2 takeover(s)                                        -                                                                              3761    60     4         PSx26, PCx13, PBx6 (notes: W-AEx317, W-RJx921, W-SRx330, W-SPx24)
B+ G3 scale during rollout                                   final                  N=2 U=1 S=1 P=0  up to 2 behind, NO read, 2 takeover(s)                                        -                                                                              21946   202    4         PCx142, PSx136, PBx108 (notes: W-AEx2430, W-RJx4367, W-SRx688, W-SPx96)
B+ G4 partition                                              final                  N=3 U=1 S=0 P=2  up to 2 behind, NO read, 2 takeover(s)                                        -                                                                              200     18     5         - (notes: W-AEx42)
B+ G4 partition                                              final                  N=3 U=0 S=1 P=2  up to 2 behind, NO read, 2 takeover(s)                                        -                                                                              442     20     5         - (notes: W-AEx60, W-RJx58)
B+ G6 faults                                                 final                  N=3 U=1 S=0 P=0  up to 1 behind, NO read, 1 takeover(s)                                        -                                                                              6173    14     1         PMx16 (notes: W-RJx338, W-AEx462)
B+ G6 faults                                                 final                  N=3 U=0 S=1 P=0  up to 1 behind, NO read, 1 takeover(s)                                        -                                                                              13918   19     1         PMx16 (notes: W-AEx1035, W-RJx805)
B+ G8 adoption                                               final                  N=3 U=1 S=0 P=0  up to 2 behind, NO read, 1 takeover(s)                                        -                                                                              104     4      2         - (notes: W-AEx22)
B+ G8 adoption                                               final                  N=3 U=0 S=1 P=0  up to 2 behind, NO read, 1 takeover(s)                                        -                                                                              126     4      2         - (notes: W-AEx25, W-RJx5)
B+ G8 adoption                                               final                  N=3 U=1 S=1 P=0  up to 2 behind, NO read, 1 takeover(s)                                        -                                                                              1202    13     2         - (notes: W-AEx275, W-RJx108)
B+ G8s adoption                                              final                  N=2 U=0 S=1 P=0  up to 2 behind, NO read, 1 takeover(s)                                        the adopted surge group is promoted when N is raised                           603     11     2         - (notes: W-AEx136, W-RJx49)
B+ G10 promotion                                             final                  N=3 U=0 S=1 P=0  up to 2 behind, NO read, 1 takeover(s)                                        N raised during a rollout, then lowered                                        2891    34     5         PSx27, PCx11, PBx8 (notes: W-AEx296, W-RJx639, W-SRx226, W-SPx15)
B+ G10 promotion                                             final                  N=3 U=1 S=1 P=0  up to 2 behind, NO read, 1 takeover(s)                                        N raised during a rollout, then lowered                                        25555   156    5         PCx207, PSx288, PBx159 (notes: W-AEx3421, W-RJx4252, W-SRx593, W-SPx85)
B+ G11 sparse set                                            final                  N=2 U=1 S=0 P=0  up to 2 behind, NO read, 1 takeover(s)                                        {0, 3}, rollout and two scale-ups                                              1271    16     3         - (notes: W-AEx412)
B+ G11 sparse set                                            final                  N=2 U=0 S=1 P=0  up to 2 behind, NO read, 1 takeover(s)                                        {0, 3}, rollout and two scale-ups                                              8669    44     3         PCx20, PSx7 (notes: W-AEx1586, W-RJx693, W-SRx101)
V L1 one loss of Ready                                       final                  N=3 U=1 S=0 P=0  exact, no takeover; status one hop late                                       -                                                                              168     2      1         -
V L1 one loss of Ready                                       final                  N=3 U=0 S=1 P=0  exact, no takeover; status one hop late                                       -                                                                              273     2      1         -
V L1 one loss of Ready                                       final                  N=3 U=1 S=1 P=0  exact, no takeover; status one hop late                                       -                                                                              1125    2      1         -
V L2 two losses of Ready                                     final                  N=3 U=1 S=0 P=0  exact, no takeover; status one hop late                                       -                                                                              672     3      1         -
V L2 two losses of Ready                                     final                  N=3 U=0 S=1 P=0  exact, no takeover; status one hop late                                       -                                                                              1397    3      1         -
V G6 faults                                                  final                  N=3 U=1 S=0 P=0  exact, no takeover; status one hop late                                       -                                                                              1617    4      1         -
V G6 faults                                                  final                  N=3 U=0 S=1 P=0  exact, no takeover; status one hop late                                       -                                                                              3740    4      1         -
V1 L1 one loss of Ready                                      one-way                N=3 U=1 S=0 P=0  exact, no takeover; status one hop late                                       -                                                                              168     2      1         -
V1 L1 one loss of Ready                                      one-way                N=3 U=0 S=1 P=0  exact, no takeover; status one hop late                                       -                                                                              273     2      1         PNx3, PRx3
V1 L1 one loss of Ready                                      one-way                N=3 U=1 S=1 P=0  exact, no takeover; status one hop late                                       -                                                                              1069    2      1         PNx22, PRx22
V1 L2 two losses of Ready                                    one-way                N=3 U=1 S=0 P=0  exact, no takeover; status one hop late                                       -                                                                              672     3      1         PNx12, PRx12
V1 L2 two losses of Ready                                    one-way                N=3 U=0 S=1 P=0  exact, no takeover; status one hop late                                       -                                                                              1397    3      1         PNx47, PRx47, PLx6
V1 G6 faults                                                 one-way                N=3 U=1 S=0 P=0  exact, no takeover; status one hop late                                       -                                                                              1617    4      1         -
V1 G6 faults                                                 one-way                N=3 U=0 S=1 P=0  exact, no takeover; status one hop late                                       -                                                                              3740    4      1         PNx84, PLx41, PRx84
V- L1 one loss of Ready                                      no-verify              N=3 U=1 S=0 P=0  exact, no takeover; status one hop late                                       -                                                                              168     2      1         PAx6, PRx6, PMx3
V- L1 one loss of Ready                                      no-verify              N=3 U=0 S=1 P=0  exact, no takeover; status one hop late                                       -                                                                              273     2      1         PAx12, PRx15, PMx6, PNx3
V- L1 one loss of Ready                                      no-verify              N=3 U=1 S=1 P=0  exact, no takeover; status one hop late                                       -                                                                              1039    2      1         PAx44, PRx66, PMx20, PNx22
V- L2 two losses of Ready                                    no-verify              N=3 U=1 S=0 P=0  exact, no takeover; status one hop late                                       -                                                                              672     3      1         PAx21, PRx37, PNx16, PMx23
V- L2 two losses of Ready                                    no-verify              N=3 U=0 S=1 P=0  exact, no takeover; status one hop late                                       -                                                                              1397    3      1         PAx45, PRx102, PMx53, PNx57, PLx6
V- G6 faults                                                 no-verify              N=3 U=1 S=0 P=0  exact, no takeover; status one hop late                                       -                                                                              1617    4      1         PAx16, PRx16, PMx10
V- G6 faults                                                 no-verify              N=3 U=0 S=1 P=0  exact, no takeover; status one hop late                                       -                                                                              3740    4      1         PAx33, PRx117, PMx19, PNx84, PLx41
V G6 faults                                                  final                  N=3 U=0 S=1 P=0  up to 2 behind, read before writing, 1 takeover(s); status one hop late       and the cache is late, mode A                                                  91729   57     1         -
V L2 two losses of Ready                                     final                  N=3 U=1 S=1 P=0  exact, no takeover; status one hop late                                       -                                                                              4009    3      1         -
V G6 faults                                                  final                  N=3 U=1 S=1 P=0  exact, no takeover; status one hop late                                       -                                                                              10441   4      1         -
V G2 two rollouts, one loss of Ready                         final                  N=3 U=1 S=0 P=0  exact, no takeover; status one hop late                                       -                                                                              981     6      3         -
V G4 partition, one loss of Ready                            final                  N=3 U=1 S=0 P=2  exact, no takeover; status one hop late                                       -                                                                              278     11     6         -
V G2 two rollouts, one loss of Ready                         final                  N=3 U=0 S=1 P=0  exact, no takeover; status one hop late                                       -                                                                              2053    6      3         -
V G4 partition, one loss of Ready                            final                  N=3 U=0 S=1 P=2  exact, no takeover; status one hop late                                       -                                                                              634     11     6         -
VS L3 scale-down, one loss of Ready                          final                  N=3 U=1 S=0 P=0  exact, no takeover; status one hop late                                       -                                                                              42      6      4         - (notes: W-SRx5, W-SPx2)
VS G3 scale during rollout, one loss of Ready                final                  N=2 U=1 S=0 P=0  exact, no takeover; status one hop late                                       -                                                                              826     10     4         - (notes: W-SRx44, W-SPx4)
VS L3 scale-down, one loss of Ready                          final                  N=3 U=0 S=1 P=0  exact, no takeover; status one hop late                                       -                                                                              42      6      4         - (notes: W-SRx5, W-SPx2)
VS G3 scale during rollout, one loss of Ready                final                  N=2 U=0 S=1 P=0  exact, no takeover; status one hop late                                       -                                                                              1625    10     4         - (notes: W-SRx95, W-SPx4)
VS G10 promotion, one loss of Ready                          final                  N=3 U=0 S=1 P=0  exact, no takeover; status one hop late                                       -                                                                              3241    12     5         - (notes: W-SRx162, W-SPx6)
VS1 L3 scale-down, one loss of Ready                         order-by-status        N=3 U=1 S=0 P=0  exact, no takeover; status one hop late                                       -                                                                              42      6      4         PQx2 (notes: W-SRx5, W-SPx2)
VS1 G3 scale during rollout, one loss of Ready               order-by-status        N=2 U=1 S=0 P=0  exact, no takeover; status one hop late                                       -                                                                              812     10     4         PQx43, PWx4 (notes: W-SRx44, W-SPx4)
VS1 L3 scale-down, one loss of Ready                         order-by-status        N=3 U=0 S=1 P=0  exact, no takeover; status one hop late                                       -                                                                              42      6      4         PQx2, PWx2 (notes: W-SRx5, W-SPx2)
VS1 G3 scale during rollout, one loss of Ready               order-by-status        N=2 U=0 S=1 P=0  exact, no takeover; status one hop late                                       -                                                                              1573    10     4         PQx59, PWx29 (notes: W-SRx95, W-SPx4)
VS1 G10 promotion, one loss of Ready                         order-by-status        N=3 U=0 S=1 P=0  exact, no takeover; status one hop late                                       -                                                                              3102    12     5         PQx116, PWx53 (notes: W-SRx162, W-SPx6)
ALT1 G2 two rollouts                                         no-verify              N=3 U=1 S=0 P=0  up to 2 behind, NO read, own writes awaited, no takeover                      own writes awaited, no takeover                                                211     3      3         - (notes: W-RJx3)
ALT1 G3 scale during rollout                                 no-verify              N=2 U=0 S=1 P=0  up to 2 behind, NO read, own writes awaited, no takeover                      own writes awaited, no takeover                                                448     5      4         - (notes: W-RJx13, W-SRx11, W-SPx2)
ALT1 G6 faults                                               no-verify              N=3 U=1 S=0 P=0  up to 2 behind, NO read, own writes awaited, no takeover                      own writes awaited, no takeover                                                6458    4      1         PAx70, PLx70, PRx70, PMx88 (notes: W-RJx109)
ALT2 G2 two rollouts                                         no-verify              N=3 U=1 S=0 P=0  up to 2 behind, NO read, own writes awaited, 1 takeover(s)                    own writes awaited, a new process starts from an older LIST                    700     6      3         PAx4, PLx4, PMx4, PRx4 (notes: W-RJx55, W-AEx47)
ALT2 G3 scale during rollout                                 no-verify              N=2 U=0 S=1 P=0  up to 2 behind, NO read, own writes awaited, 1 takeover(s)                    own writes awaited, a new process starts from an older LIST                    1767    30     4         PSx9, PBx7, PCx3, PRx1 (notes: W-AEx80, W-RJx168, W-SRx59, W-SPx11)
ALT2 G6 faults                                               no-verify              N=3 U=1 S=0 P=0  up to 2 behind, NO read, own writes awaited, 1 takeover(s)                    own writes awaited, a new process starts from an older LIST                    22843   57     1         PAx164, PLx164, PRx164, PMx224 (notes: W-RJx1117, W-AEx1071)
ALT3 G2 two rollouts                                         no-verify              N=3 U=1 S=0 P=0  up to 2 behind, NO read, own writes awaited, consistent start, 1 takeover(s)  own writes awaited, a new process starts from a consistent read                211     3      3         - (notes: W-RJx3)
ALT3 G3 scale during rollout                                 no-verify              N=2 U=0 S=1 P=0  up to 2 behind, NO read, own writes awaited, consistent start, 1 takeover(s)  own writes awaited, a new process starts from a consistent read                819     9      4         - (notes: W-RJx25, W-SRx25, W-SPx4)
ALT3 G6 faults                                               no-verify              N=3 U=1 S=0 P=0  up to 2 behind, NO read, own writes awaited, consistent start, 1 takeover(s)  own writes awaited, a new process starts from a consistent read                7884    6      1         PAx74, PLx74, PRx74, PMx92 (notes: W-RJx113)
M1 no promotion                                              no-promote             N=3 U=0 S=1 P=0  exact, no takeover                                                            -                                                                              82      3      3         PEx1, PEx1
M2 capacity counted on live groups                           cap-on-live            N=3 U=1 S=1 P=0  exact, no takeover                                                            -                                                                              854     1      1         PCx310
M3 no healthy budget                                         no-healthy-budget      N=3 U=1 S=0 P=1  exact, no takeover                                                            -                                                                              43      2      1         PAx2, PLx2, PRx2
M4 a Failed group is deleted at once                         no-wait-drained        N=3 U=1 S=0 P=0  exact, no takeover                                                            -                                                                              20      2      1         PGx3
M5 no upgrade gate                                           final                  N=3 U=1 S=0 P=0  exact, no takeover                                                            -                                                                              11      1      1         PDx6
M6 surge is not reclaimed                                    no-reclaim             N=3 U=0 S=1 P=0  exact, no takeover                                                            -                                                                              15      1      1         PEx1, POx1
M7 surge is reclaimed without looking at the floor           no-reclaim-floor       N=3 U=0 S=1 P=0  exact, no takeover                                                            -                                                                              22      1      1         PAx2, PLx2, PRx2
M8 replacement before a scale-down is finished               no-scale-first         N=3 U=1 S=0 P=0  exact, no takeover                                                            -                                                                              56      4      4         PSx7 (notes: W-SRx5, W-SPx2)
M9 a regular place is not filled at the lowest hole          no-lowest-hole         N=3 U=1 S=0 P=0  exact, no takeover                                                            -                                                                              14      1      1         POx1
M10 the revision of a new group ignores its ordinal          no-version-by-ordinal  N=3 U=1 S=0 P=1  exact, no takeover                                                            -                                                                              18      3      2         PVx1
M11 promotion does not look at the number of regular groups  no-promote-needs-room  N=3 U=1 S=1 P=0  exact, no takeover                                                            a state the rules do not reach: sg-2 carries the mark although it is in range  3       1      1         - (notes: W-SRx1)
M12 G3b also deletes a Failed marked group                   reclaim-failed         N=3 U=1 S=1 P=0  exact, no takeover                                                            -                                                                              979     2      1         PGx10
```
<!-- END GENERATED: group-model -->

读法：

- **A 段是规则本身**：缓存精确、`status` 精确，可以换进程。各条性质没有被违反。
- **D 段是本设计**：缓存落后，新进程从旧的 LIST 起，按直读重算。

<!-- BEGIN GENERATED: group-mode-a -->
```text
runs: 23  states: 125193
creations that met an object of the same name (W-AE, no new object): 0
writes rejected by their precondition (W-RJ): 0
a group created beyond N + S (PC): 0  in: -
scale-down without an excess (PB): 0  in: -
a group that is not replaceable was replaced, a group without the mark was reclaimed (PB): 0
replaced or surged while more than N regular groups exist (PS): 0  in: -
the clean-up budget claimed twice (PM): 0  in: -
Ready below N - U after a healthy deletion (PA): 0  in: -
Ready below N - U after a group with Ready Pods was deleted as not Ready (PN): 0  in: -
Ready after a round below min(Ready before, N - U) (PR): 0  in: -
the same by a scale-down alone, its order took a group with Ready Pods (PW, the W-OR of before): 0  in: -
a scale-down took a group with Ready Pods although one with Pods that are not Ready was left (PQ): 0  in: -
a scale-down took a group with Ready Pods, every regular live group had Ready Pods (W-SR, a note): 1562
not converged, unbounded, livelock (PE, PF, CYCLE): 0
ordinals not 0..N-1 although N was never lowered (PO): 0
a new group with the wrong revision for its ordinal (PV): 0
```
<!-- END GENERATED: group-mode-a -->

- **B 段是变异**：按缓存直接发，不直读，进程里也不记。多建、删错、先替换后缩容、越过下限都报出来。

<!-- BEGIN GENERATED: group-mode-b -->
```text
runs: 23  states: 161983
creations that met an object of the same name (W-AE, no new object): 26897
writes rejected by their precondition (W-RJ): 25162
a group created beyond N + S (PC): 424  in: G10 U=0 S=1, G10 U=1 S=1, G11 U=0 S=1, G3 U=0 S=1, G3 U=1 S=0, G3 U=1 S=1
scale-down without an excess (PB): 565  in: G10 U=0 S=1, G10 U=1 S=1, G3 U=0 S=1, G3 U=1 S=0, G3 U=1 S=1
a group that is not replaceable was replaced, a group without the mark was reclaimed (PB): 0
replaced or surged while more than N regular groups exist (PS): 510  in: G10 U=0 S=1, G10 U=1 S=1, G11 U=0 S=1, G3 U=0 S=1, G3 U=1 S=0, G3 U=1 S=1
the clean-up budget claimed twice (PM): 662  in: G10 U=0 S=1, G10 U=1 S=1, G11 U=0 S=1, G2 U=0 S=1, G2 U=1 S=0, G2 U=1 S=1, G2b U=0 S=1, G2b U=1 S=0, G3 U=0 S=1, G3 U=1 S=0, G3 U=1 S=1, G4 U=0 S=1, G6 U=0 S=1, G6 U=1 S=0
Ready below N - U after a healthy deletion (PA): 705  in: G10 U=0 S=1, G10 U=1 S=1, G11 U=0 S=1, G2 U=0 S=1, G2 U=1 S=0, G2 U=1 S=1, G2b U=0 S=1, G2b U=1 S=0, G3 U=0 S=1, G3 U=1 S=0, G3 U=1 S=1, G4 U=0 S=1, G6 U=0 S=1, G6 U=1 S=0
Ready below N - U after a group with Ready Pods was deleted as not Ready (PN): 0  in: -
Ready after a round below min(Ready before, N - U) (PR): 775  in: G10 U=0 S=1, G10 U=1 S=1, G11 U=0 S=1, G2 U=0 S=1, G2 U=1 S=0, G2 U=1 S=1, G2b U=0 S=1, G2b U=1 S=0, G3 U=0 S=1, G3 U=1 S=0, G3 U=1 S=1, G4 U=0 S=1, G6 U=0 S=1, G6 U=1 S=0
the same by a scale-down alone, its order took a group with Ready Pods (PW, the W-OR of before): 0  in: -
a scale-down took a group with Ready Pods although one with Pods that are not Ready was left (PQ): 0  in: -
a scale-down took a group with Ready Pods, every regular live group had Ready Pods (W-SR, a note): 1407
not converged, unbounded, livelock (PE, PF, CYCLE): 0
ordinals not 0..N-1 although N was never lowered (PO): 0
a new group with the wrong revision for its ordinal (PV): 0
```
<!-- END GENERATED: group-mode-b -->

<!-- BEGIN GENERATED: group-mode-b-checked -->
```text
runs: 23  states: 121495
creations that met an object of the same name (W-AE, no new object): 17383
writes rejected by their precondition (W-RJ): 14644
a group created beyond N + S (PC): 407  in: G10 U=0 S=1, G10 U=1 S=1, G11 U=0 S=1, G3 U=0 S=1, G3 U=1 S=0, G3 U=1 S=1
scale-down without an excess (PB): 299  in: G10 U=0 S=1, G10 U=1 S=1, G3 U=0 S=1, G3 U=1 S=0, G3 U=1 S=1
a group that is not replaceable was replaced, a group without the mark was reclaimed (PB): 0
replaced or surged while more than N regular groups exist (PS): 510  in: G10 U=0 S=1, G10 U=1 S=1, G11 U=0 S=1, G3 U=0 S=1, G3 U=1 S=0, G3 U=1 S=1
the clean-up budget claimed twice (PM): 32  in: G6 U=0 S=1, G6 U=1 S=0
Ready below N - U after a healthy deletion (PA): 0  in: -
Ready below N - U after a group with Ready Pods was deleted as not Ready (PN): 0  in: -
Ready after a round below min(Ready before, N - U) (PR): 0  in: -
the same by a scale-down alone, its order took a group with Ready Pods (PW, the W-OR of before): 0  in: -
a scale-down took a group with Ready Pods although one with Pods that are not Ready was left (PQ): 0  in: -
a scale-down took a group with Ready Pods, every regular live group had Ready Pods (W-SR, a note): 1982
not converged, unbounded, livelock (PE, PF, CYCLE): 0
ordinals not 0..N-1 although N was never lowered (PO): 0
a new group with the wrong revision for its ordinal (PV): 0
```
<!-- END GENERATED: group-mode-b-checked -->

- **V 段是 `status` 晚一跳**，分三种：本设计的复核（不新鲜的按未观测算，两个方向都挡）；只否决「报告就绪」的单向复核；不复核。
- **ALT 段是 02 的 6.3 没有选的另一条路**：不直读，等自己上一次的写进了缓存再开始下一轮；新进程从旧的 LIST 起，或者从一致性的读起。它对「自己的写」和「换进程」够了，环境事件（掉就绪、失败）晚到时照样越界。直读把两样一起挡住。
- **M 段各去掉一条规则**，对应 2.4 表里「穷举」一栏。

**终局相同。**各段静止状态的集合（逐序号的版本、就绪、标记）与 A 段逐形状比较：

<!-- BEGIN GENERATED: group-equivalence -->
```text
scenario       budget    exact        other       same set  the other is
G1             U=1 S=0   1            1           yes       mode A, cache up to 2 behind
G1             U=1 S=0   1            1           yes       mode B
G1             U=0 S=1   1            1           yes       mode A, cache up to 2 behind
G1             U=0 S=1   1            1           yes       mode B
G1             U=1 S=1   1            1           yes       mode A, cache up to 2 behind
G1             U=1 S=1   1            1           yes       mode B
G2             U=1 S=0   3            3           yes       mode A, cache up to 2 behind
G2             U=1 S=0   3            3           yes       mode A, cache up to 3 behind
G2             U=1 S=0   3            3           yes       mode B
G2             U=0 S=1   3            3           yes       mode A, cache up to 2 behind
G2             U=0 S=1   3            3           yes       mode A, cache up to 3 behind
G2             U=0 S=1   3            3           yes       mode B
G2             U=1 S=1   3            3           yes       mode A, cache up to 2 behind
G2             U=1 S=1   3            3           yes       mode B
G2b            U=1 S=0   2            2           yes       mode A, cache up to 2 behind
G2b            U=1 S=0   2            2           yes       mode B
G2b            U=0 S=1   2            2           yes       mode A, cache up to 2 behind
G2b            U=0 S=1   2            2           yes       mode B
G3             U=1 S=0   4            4           yes       mode A, cache up to 2 behind
G3             U=1 S=0   4            4           yes       mode A, cache up to 3 behind
G3             U=1 S=0   4            4           yes       mode B
G3             U=0 S=1   4            4           yes       mode A, cache up to 2 behind
G3             U=0 S=1   4            4           yes       mode A, cache up to 3 behind
G3             U=0 S=1   4            4           yes       mode B
G3             U=1 S=1   4            4           yes       mode A, cache up to 2 behind
G3             U=1 S=1   4            4           yes       mode B
G4             U=1 S=0   5            5           yes       mode A, cache up to 2 behind
G4             U=1 S=0   5            5           yes       mode B
G4             U=0 S=1   5            5           yes       mode A, cache up to 2 behind
G4             U=0 S=1   5            5           yes       mode B
G6             U=1 S=0   1            1           yes       mode A, cache up to 2 behind
G6             U=1 S=0   1            1           yes       mode B
G6             U=1 S=0   1            1           yes       status one hop late
G6             U=0 S=1   1            1           yes       mode A, cache up to 2 behind
G6             U=0 S=1   1            1           yes       mode B
G6             U=0 S=1   1            1           yes       status one hop late
G6             U=1 S=1   1            1           yes       status one hop late
G8             U=1 S=0   2            2           yes       mode A, cache up to 2 behind
G8             U=1 S=0   2            2           yes       mode B
G8             U=0 S=1   2            2           yes       mode A, cache up to 2 behind
G8             U=0 S=1   2            2           yes       mode B
G8             U=1 S=1   2            2           yes       mode A, cache up to 2 behind
G8             U=1 S=1   2            2           yes       mode B
G8s            U=0 S=1   2            2           yes       mode A, cache up to 2 behind
G8s            U=0 S=1   2            2           yes       mode B
G10            U=0 S=1   5            5           yes       mode A, cache up to 2 behind
G10            U=0 S=1   5            5           yes       mode B
G10            U=1 S=1   5            5           yes       mode A, cache up to 2 behind
G10            U=1 S=1   5            5           yes       mode B
G11            U=1 S=0   3            3           yes       mode A, cache up to 2 behind
G11            U=1 S=0   3            3           yes       mode B
G11            U=0 S=1   3            3           yes       mode A, cache up to 2 behind
G11            U=0 S=1   3            3           yes       mode B
```
<!-- END GENERATED: group-equivalence -->

按缓存直接发在途中越界，最后到的地方相同。所以直读保的是过程，不是终局。

**序号集合不总是回到 `[0, N)`。**反例（A 段，U = 1、S = 0，没有 deletion cost）：N = 2，滚动中 `sg-1` 已经重建；N 调到 3，建 `sg-2`；轮到 `sg-0`，删掉；它消失之后、重建之前 N 调回 2。正式的组 `sg-1`、`sg-2` 正好 2 个，不再补 `sg-0`。终态 `{1, 2}`，完整、合法（契约允许稀疏集合）。另一个来源是缩容的「未就绪的先」。N 没有调小过的运行里 PO 没有被违反。

### 3.3 四个单轮：名字确定为什么不够

02 的 6.3 的三个例子，加一个多建的例子，各在三种设置下走一轮：

<!-- BEGIN GENERATED: group-scripted -->
```text
R1: a new process, its LIST is older than the last writes of the old one. N=3 U=1 S=0, three groups of v0, target v1. After the LIST sg-1 lost Ready and the old process deleted it (not Ready first)
  mode B, no check against the Pods        writes: delete sg-2 replace                                        violations: PA, PL, PM, PR
                                           PA [delete sg-2 replace: ready 2 floor 2]; PL [delete sg-2 replace: ready 2 floor 2 late 0]; PM [delete sg-2 replace: live 2 floor 2]; PR [delete sg-2 replace: ready 2 -> 1 floor 2]
  mode B, with the check (V-34)            writes: nothing                                                    violations: -
  mode A, no check against the Pods        writes: nothing                                                    violations: -
  mode A, the check only vetoes (V-4)      writes: nothing                                                    violations: -
  mode A (the decision)                    writes: nothing                                                    violations: -

L1: one process, its own DELETE is not in the cache yet, and the target is moved back. N=3 U=1 S=0, {0: v1, 1: v1, 2: v0}; target v1: sg-2 deleted; target back to v0
  mode B, no check against the Pods        writes: delete sg-1 replace                                        violations: PA, PL, PM, PR
                                           PA [delete sg-1 replace: ready 2 floor 2]; PL [delete sg-1 replace: ready 2 floor 2 late 0]; PM [delete sg-1 replace: live 2 floor 2]; PR [delete sg-1 replace: ready 2 -> 1 floor 2]
  mode B, with the check (V-34)            writes: nothing                                                    violations: -
  mode A, no check against the Pods        writes: nothing                                                    violations: -
  mode A, the check only vetoes (V-4)      writes: nothing                                                    violations: -
  mode A (the decision)                    writes: nothing                                                    violations: -

G0-lag: the promotion of sg-3 is not in the cache yet, and N is lowered again. N 4 -> 3, U=0 S=1, target v1; sg-4 is the surge created at N=4
  mode B, no check against the Pods        writes: delete sg-4 reclaim-surge; delete sg-2 replace             violations: PS
                                           PS [delete sg-2 replace: regular 4 N 3]
  mode B, with the check (V-34)            writes: delete sg-4 reclaim-surge; delete sg-2 replace             violations: PS
                                           PS [delete sg-2 replace: regular 4 N 3]
  mode A, no check against the Pods        writes: delete sg-3 scale-down                                     violations: -
  mode A, the check only vetoes (V-4)      writes: delete sg-3 scale-down                                     violations: -
  mode A (the decision)                    writes: delete sg-3 scale-down                                     violations: -

one too many: an old LIST {0, 1, 3}, sg-3 a regular group of v0. Since then sg-3 was replaced: its capacity went to the hole 2, and a surge was created at 3. N=3 U=0 S=1, target v1
  mode B, no check against the Pods        writes: create sg-4 v1 surge                                       violations: PC
                                           PC [create sg-4 v1 surge: active 4 N+S 4]
  mode B, with the check (V-34)            writes: create sg-4 v1 surge                                       violations: PC
                                           PC [create sg-4 v1 surge: active 4 N+S 4]
  mode A, no check against the Pods        writes: nothing                                                    violations: -
  mode A, the check only vetoes (V-4)      writes: nothing                                                    violations: -
  mode A (the decision)                    writes: nothing                                                    violations: -

5.1: the check, a new process, exact cache. N=5 U=1, five groups of v1, target v2. While nobody ran, the Pods of sg-0 and sg-1 stopped being Ready; their status still says Ready
  mode B, no check against the Pods        writes: delete sg-4 replace                                        violations: PA, PR
                                           PA [delete sg-4 replace: ready 3 floor 4]; PR [delete sg-4 replace: ready 3 -> 2 floor 4]
  mode B, with the check (V-34)            writes: nothing                                                    violations: -
  mode A, no check against the Pods        writes: delete sg-4 replace                                        violations: PA, PR
                                           PA [delete sg-4 replace: ready 3 floor 4]; PR [delete sg-4 replace: ready 3 -> 2 floor 4]
  mode A, the check only vetoes (V-4)      writes: nothing                                                    violations: -
  mode A (the decision)                    writes: nothing                                                    violations: -

BA4-20: the check, exact cache. N=3 U=1, three groups of v1. The new template v2 and the loss of one Pod of sg-0 arrive together; the status of sg-0 still says Ready
  mode B, no check against the Pods        writes: delete sg-2 replace                                        violations: PA, PR
                                           PA [delete sg-2 replace: ready 2 floor 2]; PR [delete sg-2 replace: ready 2 -> 1 floor 2]
  mode B, with the check (V-34)            writes: nothing                                                    violations: -
  mode A, no check against the Pods        writes: delete sg-2 replace                                        violations: PA, PR
                                           PA [delete sg-2 replace: ready 2 floor 2]; PR [delete sg-2 replace: ready 2 -> 1 floor 2]
  mode A, the check only vetoes (V-4)      writes: nothing                                                    violations: -
  mode A (the decision)                    writes: nothing                                                    violations: -

NEW-30: the check in the other direction, a new process, exact cache. N=3 U=1, three groups of v1, target v2. While nobody ran, the Pods of sg-0 became Ready again (its status still says not Ready) and the Pods of sg-1 stopped being Ready (its status still says Ready)
  mode B, no check against the Pods        writes: delete sg-0 replace-notready                               violations: PN, PR
                                           PN [delete sg-0 replace-notready: ready 2 floor 2]; PR [delete sg-0 replace-notready: ready 2 -> 1 floor 2]
  mode B, with the check (V-34)            writes: nothing                                                    violations: -
  mode A, no check against the Pods        writes: delete sg-0 replace-notready                               violations: PN, PR
                                           PN [delete sg-0 replace-notready: ready 2 floor 2]; PR [delete sg-0 replace-notready: ready 2 -> 1 floor 2]
  mode A, the check only vetoes (V-4)      writes: delete sg-0 replace-notready                               violations: PN, PR
                                           PN [delete sg-0 replace-notready: ready 2 floor 2]; PR [delete sg-0 replace-notready: ready 2 -> 1 floor 2]
  mode A (the decision)                    writes: nothing                                                    violations: -

NEW-30b: the same for a surge group at the end of a rollout. N=3 U=0 S=1, target v2: sg-0 is being recreated, sg-3 is the surge; its Pods are Ready again, its status still says not Ready
  mode B, no check against the Pods        writes: delete sg-3 reclaim-surge                                  violations: PN, PR
                                           PN [delete sg-3 reclaim-surge: ready 3 floor 3]; PR [delete sg-3 reclaim-surge: ready 3 -> 2 floor 3]
  mode B, with the check (V-34)            writes: nothing                                                    violations: -
  mode A, no check against the Pods        writes: delete sg-3 reclaim-surge                                  violations: PN, PR
                                           PN [delete sg-3 reclaim-surge: ready 3 floor 3]; PR [delete sg-3 reclaim-surge: ready 3 -> 2 floor 3]
  mode A, the check only vetoes (V-4)      writes: delete sg-3 reclaim-surge                                  violations: PN, PR
                                           PN [delete sg-3 reclaim-surge: ready 3 floor 3]; PR [delete sg-3 reclaim-surge: ready 3 -> 2 floor 3]
  mode A (the decision)                    writes: nothing                                                    violations: -
```
<!-- END GENERATED: group-scripted -->

- **创建幂等只挡得住同一个序号上的重复。**按缓存直接发时，创建撞名字的那些都没有产生新对象；多建出来的全部建在**另一个**序号上（旧的视图里空闲的序号不同）。所以名字确定本身不能代替直读。
- **复核会遮住「按缓存直接发」的一部分问题**：被删掉的组的 Pod 在终止，Pod 缓存按最新算时它通不过复核。这是「Pod 缓存按最新算」的结果，不能当作不直读也行的理由；先替换后缩容、多建照旧。
- 一个细节：`spec` 不变、单个进程时，旧缓存算出来的写也可以与新缓存不同。缓存已经越过了一个环境事件、还没有越过 controller 自己在那之前发的写时就会这样（一个组掉就绪；controller 删另一个组；缓存看到了掉就绪、没看到删除；controller 再删掉就绪的那个）。直读挡住了它。

### 3.4 复核

`status` 晚一跳时，`status` 里的 Ready 可以在两个方向上落后于 Pod。本设计的规则（`status` 新鲜才算数，不新鲜的按未观测算；缩容和回收的次序看 Pod）：

<!-- BEGIN GENERATED: group-status-late -->
```text
runs: 14  states: 119117
creations that met an object of the same name (W-AE, no new object): 0
writes rejected by their precondition (W-RJ): 0
a group created beyond N + S (PC): 0  in: -
scale-down without an excess (PB): 0  in: -
a group that is not replaceable was replaced, a group without the mark was reclaimed (PB): 0
replaced or surged while more than N regular groups exist (PS): 0  in: -
the clean-up budget claimed twice (PM): 0  in: -
Ready below N - U after a healthy deletion (PA): 0  in: -
Ready below N - U after a group with Ready Pods was deleted as not Ready (PN): 0  in: -
Ready after a round below min(Ready before, N - U) (PR): 0  in: -
the same by a scale-down alone, its order took a group with Ready Pods (PW, the W-OR of before): 0  in: -
a scale-down took a group with Ready Pods although one with Pods that are not Ready was left (PQ): 0  in: -
a scale-down took a group with Ready Pods, every regular live group had Ready Pods (W-SR, a note): 0
not converged, unbounded, livelock (PE, PF, CYCLE): 0
ordinals not 0..N-1 although N was never lowered (PO): 0
a new group with the wrong revision for its ordinal (PV): 0
```
<!-- END GENERATED: group-status-late -->

复核只否决「报告就绪」（变异）：`status` 还写着未就绪而 Pod 已经恢复的组，被当成未就绪的旧组删掉。

<!-- BEGIN GENERATED: group-status-late-one-way -->
```text
runs: 7  states: 8936
creations that met an object of the same name (W-AE, no new object): 0
writes rejected by their precondition (W-RJ): 0
a group created beyond N + S (PC): 0  in: -
scale-down without an excess (PB): 0  in: -
a group that is not replaceable was replaced, a group without the mark was reclaimed (PB): 0
replaced or surged while more than N regular groups exist (PS): 0  in: -
the clean-up budget claimed twice (PM): 0  in: -
Ready below N - U after a healthy deletion (PA): 0  in: -
Ready below N - U after a group with Ready Pods was deleted as not Ready (PN): 168  in: G6 U=0 S=1, L1 U=0 S=1, L1 U=1 S=1, L2 U=0 S=1, L2 U=1 S=0
Ready after a round below min(Ready before, N - U) (PR): 168  in: G6 U=0 S=1, L1 U=0 S=1, L1 U=1 S=1, L2 U=0 S=1, L2 U=1 S=0
the same by a scale-down alone, its order took a group with Ready Pods (PW, the W-OR of before): 0  in: -
a scale-down took a group with Ready Pods although one with Pods that are not Ready was left (PQ): 0  in: -
a scale-down took a group with Ready Pods, every regular live group had Ready Pods (W-SR, a note): 0
not converged, unbounded, livelock (PE, PF, CYCLE): 0
ordinals not 0..N-1 although N was never lowered (PO): 0
a new group with the wrong revision for its ordinal (PV): 0
```
<!-- END GENERATED: group-status-late-one-way -->

不复核（变异）：

<!-- BEGIN GENERATED: group-status-late-no-check -->
```text
runs: 7  states: 8906
creations that met an object of the same name (W-AE, no new object): 0
writes rejected by their precondition (W-RJ): 0
a group created beyond N + S (PC): 0  in: -
scale-down without an excess (PB): 0  in: -
a group that is not replaceable was replaced, a group without the mark was reclaimed (PB): 0
replaced or surged while more than N regular groups exist (PS): 0  in: -
the clean-up budget claimed twice (PM): 134  in: G6 U=0 S=1, G6 U=1 S=0, L1 U=0 S=1, L1 U=1 S=0, L1 U=1 S=1, L2 U=0 S=1, L2 U=1 S=0
Ready below N - U after a healthy deletion (PA): 177  in: G6 U=0 S=1, G6 U=1 S=0, L1 U=0 S=1, L1 U=1 S=0, L1 U=1 S=1, L2 U=0 S=1, L2 U=1 S=0
Ready below N - U after a group with Ready Pods was deleted as not Ready (PN): 182  in: G6 U=0 S=1, L1 U=0 S=1, L1 U=1 S=1, L2 U=0 S=1, L2 U=1 S=0
Ready after a round below min(Ready before, N - U) (PR): 359  in: G6 U=0 S=1, G6 U=1 S=0, L1 U=0 S=1, L1 U=1 S=0, L1 U=1 S=1, L2 U=0 S=1, L2 U=1 S=0
the same by a scale-down alone, its order took a group with Ready Pods (PW, the W-OR of before): 0  in: -
a scale-down took a group with Ready Pods although one with Pods that are not Ready was left (PQ): 0  in: -
a scale-down took a group with Ready Pods, every regular live group had Ready Pods (W-SR, a note): 0
not converged, unbounded, livelock (PE, PF, CYCLE): 0
ordinals not 0..N-1 although N was never lowered (PO): 0
a new group with the wrong revision for its ordinal (PV): 0
```
<!-- END GENERATED: group-status-late-no-check -->

`status` 晚一跳又赶上缩容。本设计的次序：

<!-- BEGIN GENERATED: group-status-late-scale-down -->
```text
runs: 5  states: 5776
creations that met an object of the same name (W-AE, no new object): 0
writes rejected by their precondition (W-RJ): 0
a group created beyond N + S (PC): 0  in: -
scale-down without an excess (PB): 0  in: -
a group that is not replaceable was replaced, a group without the mark was reclaimed (PB): 0
replaced or surged while more than N regular groups exist (PS): 0  in: -
the clean-up budget claimed twice (PM): 0  in: -
Ready below N - U after a healthy deletion (PA): 0  in: -
Ready below N - U after a group with Ready Pods was deleted as not Ready (PN): 0  in: -
Ready after a round below min(Ready before, N - U) (PR): 0  in: -
the same by a scale-down alone, its order took a group with Ready Pods (PW, the W-OR of before): 0  in: -
a scale-down took a group with Ready Pods although one with Pods that are not Ready was left (PQ): 0  in: -
a scale-down took a group with Ready Pods, every regular live group had Ready Pods (W-SR, a note): 311
not converged, unbounded, livelock (PE, PF, CYCLE): 0
ordinals not 0..N-1 although N was never lowered (PO): 0
a new group with the wrong revision for its ordinal (PV): 0
```
<!-- END GENERATED: group-status-late-scale-down -->

缩容的次序按报告的就绪排（变异）：被缩掉的是 Pod 已经就绪的组，留下的是坏的。

<!-- BEGIN GENERATED: group-status-late-scale-down-old-order -->
```text
runs: 5  states: 5571
creations that met an object of the same name (W-AE, no new object): 0
writes rejected by their precondition (W-RJ): 0
a group created beyond N + S (PC): 0  in: -
scale-down without an excess (PB): 0  in: -
a group that is not replaceable was replaced, a group without the mark was reclaimed (PB): 0
replaced or surged while more than N regular groups exist (PS): 0  in: -
the clean-up budget claimed twice (PM): 0  in: -
Ready below N - U after a healthy deletion (PA): 0  in: -
Ready below N - U after a group with Ready Pods was deleted as not Ready (PN): 0  in: -
Ready after a round below min(Ready before, N - U) (PR): 0  in: -
the same by a scale-down alone, its order took a group with Ready Pods (PW, the W-OR of before): 88  in: G10flip U=0 S=1, G3flip U=0 S=1, G3flip U=1 S=0, L3 U=0 S=1
a scale-down took a group with Ready Pods although one with Pods that are not Ready was left (PQ): 222  in: G10flip U=0 S=1, G3flip U=0 S=1, G3flip U=1 S=0, L3 U=0 S=1, L3 U=1 S=0
a scale-down took a group with Ready Pods, every regular live group had Ready Pods (W-SR, a note): 311
not converged, unbounded, livelock (PE, PF, CYCLE): 0
ordinals not 0..N-1 although N was never lowered (PO): 0
a new group with the wrong revision for its ordinal (PV): 0
```
<!-- END GENERATED: group-status-late-scale-down-old-order -->

三个脚本化的过程，各在三种规则下走第一轮（3.3 的生成表的后半）：

| 方向 | 过程 | 不复核 | 只否决报告就绪的 | 双向（本设计） |
| --- | --- | --- | --- | --- |
| `status` 还写着就绪，Pod 已经坏了 | N = 5、U = 1，controller 停着的时候两个组的 Pod 坏了，`status` 没更新。新进程的第一轮 | 删一个健康的组，就绪低于下限 | 不写 | 不写 |
| 同上 | N = 3、U = 1，新模板和「一个组丢了一个成员」同时到 | 删一个健康的组 | 不写 | 不写 |
| `status` 还写着未就绪，Pod 已经恢复 | N = 3、U = 1，停机期间一个组恢复了、另一个坏了，`status` 都没更新 | 越界 | 把恢复了的那个当成未就绪的旧组删掉，就绪低于下限 | 两个组都按未观测算，这一轮不写 |

本设计的规则下缩容仍然会删 Pod 就绪的组：正式的、运行的组里已经没有 Pod 一级不就绪的可删时。这是缩容本来的含义，不是次序选错。

上一版的模型在这个形状里检查的是带宽限的一条：真实的就绪数不低于 N − U − `status` 与 Pod 不符的组数。那是没有复核时能成立的最强的一条。这一版在这个形状里检查不带宽限的 PA、PN、PR。

这一节的结论都依赖一个没有建模的理想化：**ModelServing controller 的 Pod 缓存按最新算。**Pod 缓存自己落后时复核还剩多少，模型没有回答；今天单个 controller 直接读 Pod 缓存，有同样的滞后。

**模型与设计在复核上的对应。**设计里判断 `status` 落后的办法是比摘要：`status` 记着它是按哪些成员 Pod 算的，与 Pod 缓存算出来的不同就是不新鲜（02 的 4.1）。模型里没有 Pod 和摘要，每个组只有两个量：按现在的 Pod 算出来的就绪，和 `status` 里写着的 Ready；两个量不同就是不新鲜。

设计判成不新鲜的状态比模型多：成员 Pod 的状态变了而就绪没有变时，设计先按未观测算，模型按确认的算。这不影响上面各条关于删除的性质，理由是包含关系：设计里确认就绪的组是模型里确认就绪的组的子集，确认未就绪的同样；替换的候选和 `ready` 只会变少，`notReadyTarget` 只会变多；多出来的按未观测算的组在回收时要过下限。反过来，设计判成新鲜的状态模型也判成新鲜：摘要相同时，Ready 就是按这些 Pod 算的。

两点限定。这是论证，不是模型的结果：穷举模型里没有「按未观测算而其实一致」的状态，「未观测」本身也不在穷举模型里。设计比模型多等的是 ServingGroup controller 的一次 status 写，前提是让摘要变的 Pod 事件一定让它跑一轮；这靠入队的范围和摘要的范围是同一个集合（02 的 6.1），同样是推演。摘要的算法、开销、Pod 启动期间多出来的 status 写，模型都没有回答。缩容和回收的次序在模型里看的是「按现在的 Pod 算出来的就绪」，设计里看的是 Pod 缓存上一个更粗的判断（没有成员 Pod、有不就绪的、有在终止的）；两者只在次序上有差别，次序是偏好。

### 3.5 模型没有的

- Pod 缓存的滞后；缓存落后超过 3 个事件；`spec` 的缓存滞后；两个进程同时在写；直读之后到写落地之间对象又变（模型里读和写是原子的）；`ModelServing.status`（基线）的滞后。
- 只有场景脚本、没有进穷举模型的规则：没有成员的组；替换之前看补不补得上；找回和待收养；外来的同名对象；Role 模式；未观测；基线读不到。
- G5（`spec` 的下发和 `sourceGeneration`）和原地更新：不改变计数，没有建模。deletion cost 只在场景脚本里。
- Drained 经 `status` 晚一跳；G1 要 `status` 新鲜这一条（02 的 4.2）。
- 评审之后加的规则里与 Pod 有关的：复核用的摘要取哪些字段、入队与摘要是同一个集合；先写后建和它与改版本的先后（02 的 5.2 的 L8），模型里「对象建了、PodGroup 被挡住」的组直接是一个没有成员的组；G1 要 `status` 新鲜；写 Failed、Draining 之前直读策略；没有 owner 的组不管。
- 一个写失败之后这一轮后面的创建、删除、摘注解和改版本都不发：穷举模型里按这条执行；场景脚本里每一轮的写都成功。设计里创建撞名字不算失败（02 的 4.5），G5 的下发照发；模型里撞名字之后这一轮停下，G5 没有建模，是模型的简化。
- 模式 A 的「没有违反」是在上面的范围里穷举出来的，不是证明。

## 4. 账本的两条防护规则

007 里「把缺失当丢失之前直读」和「代际号」两条规则没有模型证据。`stalemodel.py` 是为它们写的小模型：一个 Role 实例，两个成员，`RoleRecreate`；两个 ServingGroup controller 进程。这个模型与组叫什么名字无关，这一版没有改它，原样重跑。

- F 是现任。账本是最新的；刚接管时它的 Pod 缓存可以缺已经存在的 Pod。
- S 是失主而还没退出的进程。它按一份旧的账本行动，Pod 的缓存可以是旧的也可以是新的。它的 status 写带着旧的 resourceVersion，账本变过之后被拒绝；它的 Pod 删除只带 UID。S 的写在模型里至多 4 次。

| 性质 | 内容 |
| --- | --- |
| Q1 | Draining 只在真的丢了成员之后写 |
| Q2 | 一个 Pod 被删，一定有一次代际号不小于它的 Draining |
| Q3 | 静止时账本是 Established，两个成员都在 |
| Q4 | 代际号不会涨到丢失的次数解释不了的地步 |

范围：L4 只覆盖「把缺失当证据」这一半，「把终止中、终态当证据要读回同一个 UID」没有；L5 的四句里只覆盖「删除时比较代际号」和「清空之后代际号加一」，「没有标签按 0」「没有账本项时取现有 Pod 的最大值」没有。实例只有两个成员。这一版把直读从「写 Draining、Failed 的证据」扩大到「要写 Draining、Failed 或者删 Pod 的轮次」（02 的 6.3）；扩大的部分没有模型。

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

模型里有一种形态：S 以为实例在创建中，把 F 刚清空的位置又建出一个 Pod，F 再把它删掉。只要 S 还活着就可以一直重复；S 退出之后收敛，没有错误的终局。所以模型里 S 的写不算进展，并且有上限。它写在 02 的 6.4。

## 5. Role 实例的账本和两个 controller

007 的组级模型（`sgmodel.py`、`run_v4.py`）原样重跑，输出与 007 的结果文件相同，只有括号里的用时不同（状态数、违反数都相同）。它覆盖：两个 controller 各自计划、各自执行、任意交错；任何两次写之间可以停下；账本的三个值；失败的组；策略变更；Role 副本数和组副本数的变化；组被人直接删除；`status` 被清空。

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

这个模型里账本对每个实例都记，与本设计相同。它的组一级是 007 的规则：只有「按数量补」，没有版本、序号和滚动，失败的组在缩容里排第一，ModelServing controller 直接读 Pod；这些与本设计不同的地方由第 3 节的模型覆盖。它没有「实例只有一个 Pod」的形状，没有「整个组的 Pod 都没了就让组失败」（02 的 5.3），没有故障起点和它的身份。两个模型没有合成一个：组一级的滚动和实例一级的恢复在同一个状态空间里的交错没有穷举，靠的是对象的边界（组失败或终止中时，它里面的一切就停）。

## 6. 推演的部分

下面这些没有机器检查。

| 内容 | 依据 |
| --- | --- |
| Role 一级的滚动、扩缩容、协调，与账本的衔接 | Role 一级的规则沿用 v2，由 005 的参考模型覆盖。衔接点只有一个：被 Role 一级选中删除的实例，先写 Draining（E4）。Role 一级的转正多一个在正常运行时总是成立的条件，没有重放 |
| 原地更新 | 沿用 007 的 05，没有模型 |
| SG 模式配原地更新：改 `spec.revision`、`RecreateRequired` 之后回到重建；这时建不建 surge | 02 的 4.2、第 9 节 |
| plugin 钩子的次序；`OnRoleDelete` 在离开 Draining 之前调过；调用之前的直读；传给钩子的身份带组的 UID | 03 的 E2、E3；02 第 7 节 |
| 槽位被外来 Pod 占着（L7），判断来自直读；待接管的成员 | 03 第 3 节、E7 |
| `sourceGeneration`；`spec` 的下发 | 02 的 A2 |
| 同名重建的各条：每一处按 UID；一次更新事件里新旧对象 UID 不同；队列的退避；后台删组之后旧一代的 Pod 按外来等；终止中的组超期直读 | 02 的 4.5、6.2 |
| 外来的同名对象之外的撞名字（PodGroup、Service） | 02 的 4.5 |
| ServingGroup controller 这一侧的直读：要写 Failed、Draining、删 Pod 或者打原地更新的补丁的轮次；写 Failed、Draining 之前直读 ModelServing 上的策略 | 02 的 6.3、5.3 |
| 先写后建：`status.pods` 是 0 的组，PodGroup 建成之后、第一个 Pod 之前先写 `status`；它和改版本的 PATCH 以同一个 resourceVersion 分先后。「没有成员」不要求已观测 | 02 的 5.2 的 L8、4.1、4.2 |
| 复核用的摘要：对成员 Pod 取判定用到的字段；两个 controller 用同一个 Pod informer 和同一个函数；入队的范围与摘要的范围是同一个集合；直读的轮次按直读到的 Pod 算 | 02 的 4.1、6.1、6.3。模型里只有「新不新鲜」这一个量（3.4） |
| 创建撞名字之后这一轮怎样接着做；一个写失败时 G5 照发 | 02 的 4.2、4.5 |
| G1 要 `status` 新鲜；空组算就绪而基线不推进以外的 `status` 口径；没有 owner 的组不管 | 02 的 4.2、2.4；03 第 6 节。空组不推进基线、带标记的空组的回收有场景脚本 |
| 故障的起点写在账本里：身份、作废、`None` 和 `-1`、清理 | 02 的 5.3 |
| Pod 上 surge 注解的副本：跟随组对象；找回和升级接管时按它判定 | 02 的 2.3、5.5 |
| 无主的成员 Pod 的收养、版本不同回到重建，和稀疏布局下的竞态 | 02 的 5.5。找回、待收养、surge 的组找回来仍是 surge 各有一个场景脚本 |
| 整个组的 Pod 都没了：ServingGroup controller 写 Failed 的条件 | 02 的 5.3。模拟器里只有「之后怎样重建」这一半 |
| `RoleRollingUpdate` 下的组一级；模式切换；组的版本取哪个字段 | 02 的 4.1、4.6、4.7。Role 模式下摘标记有一个场景脚本 |
| 计数字段、condition、`RolloutBlocked` 的六个 reason 和期限；`GroupNotObserved` 的起点在内存里 | 03 第 6 节。上一版的评审用小状态枚举查过当时的五个 reason；这一版新增的 `OrdinalOccupied` 和改过的「有事要做」没有再枚举。期限没有建模 |
| 事件记在哪；拓扑和亲和 | 02 的 8.8、8.9 |
| 接管表里钩子、owner、surge 标记和副本、故障的起点、PodGroup、接管注解的各行 | 03 的 10.2 |
| `status` 被改写的各种情形 | 03 的 10.5；其中「账本被清空」有 007 的模型 |
| 升级接管里成员一级的步骤：换 owner、待接管的成员、带删除标记的 Pod；T3 对 surge 注解的判定；回退时把副本写对 | [06](06-live-upgrade.md)。组一级的闸门、接管来的带标记的组有模型 |
| 用例目录里按规则归类的部分 | 04 第 8 节 |

## 7. 留给真实验证的

在 Kind 上要确认的事，按依赖的对象分。升级的实验另见 [06](06-live-upgrade.md) 第 12 节。

**依赖 Kubernetes 的行为**

1. 前台级联删除一个 ServingGroup：组对象在 Pod、PodGroup、Service 都消失之前一直在；PodGroup 带上 `blockOwnerDeletion` 之后才等它。对象消失之前同名的创建返回 AlreadyExists。
2. 对正在前台删除的组再发一次后台的 DELETE，组对象是否立刻消失。
3. 带 UID 和 resourceVersion 前置条件的 DELETE 在对象变过、或者同名的对象换了一个之后返回冲突。整个对象的 status UPDATE、带 `metadata.uid` 的 PATCH 落在同名的新对象上时被拒绝。body 里带 `metadata.resourceVersion` 的主资源 PATCH 和 `status` 子资源的 UPDATE 互相排他（两者写的是同一个存储对象，共用一个 resourceVersion）：先写后建全靠它。
4. 后台删一个组：同名的新对象立刻建得出来；旧 UID 的 Pod 被垃圾回收器删掉之前不被新对象数进去；垃圾回收器把「同名而 UID 不同的 owner」按 owner 不存在处理。
5. orphan 删一个组：垃圾回收器先摘 Pod 的 owner、再让组对象消失。
6. 一致性的读（不带 `resourceVersion` 的 LIST）的代价：组很多时每个有写的轮次一次。
7. 用百分比的 PDB 在 owner 是带 `scale` 子资源的 ServingGroup 时怎样算期望数。用户后台删一个组之后、旧一代的 Pod 被回收之前（owner 是一个不存在的 UID），选中它们的 PDB 怎样表现。
8. 删除 ServingGroup 的 CRD 时，实例和它们名下的 Pod 被删的次序。
9. `kubectl wait --for=delete`、`--for=condition=Ready` 在同名重建时的行为。
10. 同名的 PodGroup、Service 先删后建时 Volcano、kube-proxy、DNS 的反应。

**依赖实现的行为**

11. 契约 2.12 的全量用例在新实现上跑一遍。先跑 04 里标成「有意变化」和「待定」的；runner 要先按 04 第 8 节改夹具和观测。
12. 03 的 10.2 的每一行：在那次写之后杀掉 controller，看新进程的动作。
13. 新进程的第一轮：停机期间让几个组的 Pod 变坏、让一个坏的组恢复，`status` 都不更新；新进程起来之后没有健康的组被多删，恢复了的组没有被当成坏的删掉。
14. 复核：一个组刚丢了成员、同时提交新模板（`recovery` 套件的夹具），健康的组没有被多删。Pod 缓存落后时的窗口有多大。摘要：Pod 启动期间组有多少时间是不新鲜的，多出多少 status 写。两种别的组件频繁改 Pod 的情形不让组不新鲜：周期性地写 `pod-deletion-cost` 的组件；调度不上的 Pod（调度器每个周期改它的 condition 的 message）。抄了标签的 Pod 在变时，它所在的组照常是候选。
15. 直读：缓存落后时（给 informer 注入延迟）滚动途中回滚、转正之后马上缩容，写与不落后时相同。
16. 两个进程并存：关掉「失主退出」，同一个 N 之下不多建组；没有健康的成员被误删；一个额度删两个的情形有多常见。
17. 原进程退出之后它的创建、删除请求才落地。
18. 写量和读量：成员 Pod 的状态每变一次的 status 写、每个实例建成时的 status 写、先写后建的那一次、每个有写的轮次的直读，对 apiserver 的压力。
19. 耗时：每一步多一跳之后一次滚动要多久；原位重建等垃圾回收器要多久；`spec` 下发之后未观测的窗口多长。
20. 垃圾回收器停了或者很慢：组停在终止中，报 `WaitingForDeletion`；恢复之后接着做。
21. 一个组卡在终止中（加一个 finalizer）：它的序号不重建；带 S 的滚动照常做完；N 调小之后滚动被挡住。
22. 外来的同名对象：同名的 ModelServing 后台删除之后立刻重建，新的 ModelServing 报 `OrdinalOccupied`，旧对象被回收之后建得出来。
23. 一个组刚变坏、`status` 还没写出来时缩容：删的是它。刚设完 deletion cost 马上缩容：按 cost 删。
24. Drained 晚一跳：Drained 为真之后再让一个成员出故障，G1 等 `status` 跟上；Pod 缓存还没看到那次故障的窗口有多大。
25. 无主的成员 Pod：`--cascade=orphan` 删一个组之后被找回、被收养；surge 的组找回来仍是 surge；稀疏布局下先多建一个组、再缩掉一个的竞态；待收养的组不参加缩容。
26. 各 Role 的副本数缩到 0、改模板、再扩回来：空组直接改版本，范围外的空组被删掉补到低位，没有先起旧版本的 Pod。各 Role 都是 0 时可用数是 N、`Available` 为真，基线不推进。对象建了而 PodGroup 建不出来时提交新模板：放开之后这个组直接按新版本起，组对象不换。Pod 的创建被拒绝时提交新模板：这个组在预算之内被替换。已经有 Pod 的组的 `spec.revision` 没有被改过（先写后建）。
27. 每组一个 Pod，滚动途中驱逐一个旧版本的 Pod：组对象被换掉，Pod 直接按序号和 partition 的版本起。
28. 重启故障的 grace：换主之后接着算；停机期间恢复过又再坏的重新计；`-1` 改回有限值从新观察起算；停机超过 grace 时回来第一轮恢复的范围。等待中改策略：不等下发就生效。
29. 转正：滚动途中调大 N，组对象上的注解被摘掉，Pod 上的副本随后摘掉，Pod 的 UID 不变。
30. `ServingGroupRollingUpdate` 配原地更新：组的版本原地变；plugin 不支持、超期、有终态成员时 `RecreateRequired` 为真，组被重建。
31. `groupPolicy` 和按 Role 的网络拓扑：PodGroup 的 owner 换成 ServingGroup 之后调度结果不变。
32. `OnRoleDelete` 一直失败：实例停在清空中，组报 `DrainBlocked`；恢复之后接着做。
33. `RolloutBlocked` 的六个 reason 各造一次；期限取多少；正常的缩容和滚动收尾不会报。
34. 滚动模式 ServingGroup → Role → ServingGroup 来回切：切到 Role 模式时 `spec.revision` 被清空、留着的 surge 注解被摘掉；切回来时按 `status.revision` 分类。
35. ServingGroup controller 对一个组一直出错：`status` 照实写出来，带 `ReconcileFailed`，过期的话照常被替换；`status` 写不出来时超过期限报 `GroupNotObserved`。
36. 抄了标签的 Pod（`kubectl debug --copy-to --keep-labels`）：不为它建组，它所在的组照常缩容、替换。待收养一直收养不完时报 `GroupNotObserved`。
37. 用 orphan 方式删 ModelServing：留下的组和 Pod 没有人管；再建同名的 ModelServing 时报 `OrdinalOccupied`。
38. `RoleRecreate` 清空了组里全部 Pod（每组一个实例）：按组自己的版本重建，版本没有被改。
39. 整个组的 Pod 都没了：PodGroup、headless Service 跟着重建；排队的集群里 PodGroup 重新入队要等多久。
40. 替换之前看补不补得上：基线读不到、或者低位被外来的对象占满时，要替换的组不被删；两个范围外的组指望同一个空闲序号时只删一个；基线读不到期间的范围外空组不改版本，终局与基线一直读得到时相同。
41. 组一级的写一直被拒绝（对象配额、准入 webhook）：有事件，condition 的 message 里写着哪个写失败；Role 副本数的变化照常下发到已有的组。
42. 打开恢复策略、缩短 grace：下发到组之后才生效。

## 8. 怎样重跑

在任务目录的 `model/` 下：

```sh
uv run --no-project python run_scenarios.py        # 场景重放：scenario-tables.md、scenario-results.txt
uv run --no-project python diff_reference.py       # 与 005 的参考模型对比：reference-diff-results.txt
uv run --no-project python run_runner_cases.py     # runner 的 35 个组合场景用例：runner-cases-results.txt
uv run --no-project python run_normal_cases.py     # normal 套件的 68 例：normal-cases-results.txt
uv run --no-project python run_group.py > group-results.txt      # 组一级穷举
uv run --no-project python stalemodel.py > stale-results.txt     # 账本的两条规则
uv run --no-project python run_v4.py > ledger-results.txt        # 007 的组级模型
(cd ../evidence && uv run --no-project python resolve_case_verdicts.py)   # 用例目录的结论
uv run --no-project python build_docs.py           # 把生成的表写进 04、04a、05
uv run --no-project python build_docs.py --check
```

前四个带 `--check` 时不写文件，只比较磁盘上的文件与重新算出来的是否相同。两套 runner 用例的重放要读契约 2.12 的只读导出（`kthena-rollout-runner` `main` @ `3772012` 的 `cases/`），放在任务目录的 `.contract-2.12/` 下；导出用完即删，要重跑时重新导出。

| 文件 | 内容 |
| --- | --- |
| `grouplevel.py` | 组一级的规则函数（02 的 4.1–4.6） |
| `sim.py`、`scenarios.py`、`run_scenarios.py` | 确定的模拟器；契约的场景脚本和补充场景；重放、变异、生成过程表 |
| `run_runner_cases.py`、`run_normal_cases.py` | 两套 runner 用例的逐停点重放 |
| `diff_reference.py` | 与 005 的参考模型逐状态对比 |
| `gmodel.py`、`run_group.py` | 组一级的穷举模型 |
| `stalemodel.py` | 账本的直读和代际号 |
| `sgmodel.py`、`podmodel.py`、`run_v4.py` | 007 的组级模型，原样 |
| `build_docs.py` | 把生成的表写进文档并校验 |
