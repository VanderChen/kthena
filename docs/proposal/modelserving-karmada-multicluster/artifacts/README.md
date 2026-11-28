# 产物：解释器、脚本、探针和日志

这里是 [04-evidence.md](../04-evidence.md) 里各项结论的原始材料。全部针对 Karmada `6d007132` 和 kthena `1a5145e9`。

| 目录 | 内容 |
| --- | --- |
| `interpreter/` | 方案 M 的 `ResourceInterpreterCustomization` 草稿、离线测试输入、`run-offline.sh` |
| `unit/` | 按组分发（方案 G）用的解释器、单元生成脚本 `gen-unit.sh`、策略 |
| `kind/` | Kind 环境脚本和各场景的 YAML |
| `estimator-probe/` | 三个包级探针的测试源码（`.go.txt`，不参与本仓库的构建） |
| `logs/` | 离线实测、包级探针和 Kind 各场景的输出 |

两份解释器不能同时装在一个 Karmada 控制面上：它们针对同一个 kind。

## 日志和场景的对应

| 日志 | 场景 |
| --- | --- |
| `interpreter-offline.log`、`unit-interpreter-offline.log` | 两份解释器的 `karmadactl interpret` 结果 |
| `estimator-probe.log` | 三种资源需求建模在各估算器下的数值；默认资源模型下 GPU 请求的估算 |
| `kind-00-*`、`kind-01-*`、`kind-02-*` | 方案 M 的环境：集群、镜像 digest、特性门控、成员准备 |
| `kind-C0-*` … `kind-C10b-*` | 方案 M 的场景，编号见 [04-evidence.md](../04-evidence.md) 第 4 节 |
| `kind-G-01-*`、`kind-G-02-*` | 方案 G 的环境 |
| `kind-G0-*` … `kind-G7-*` | 方案 G 的场景，编号见 [04-evidence.md](../04-evidence.md) 第 5 节 |

每个场景日志的开头说明了场景，并回显了关键命令；有些场景的命令没有逐条回显，步骤以 [04-evidence.md](../04-evidence.md) 的场景表为准。
日志是当时的原始输出，发布时只改了两处：一个集群内部的 Service IP 换成 `<cluster-ip>`；本机上 `karmadactl` 的绝对路径改成命令名。

## 重跑

需要 docker、kind、kubectl、helm、go、jq。会创建 4 个 Kind 集群共 6 个节点；`fs.inotify.max_user_instances` 是 128 时不够用。

```bash
export WS=<包含 karmada/ 和 kthena/ 两个检出目录的目录>

# kthena 的 controller-manager 镜像
(cd "$WS/kthena" && docker build -t ghcr.io/volcano-sh/kthena-controller-manager:dev-014 \
  -f docker/Dockerfile.kthena-controller-manager .)

kind/01-local-up-karmada.sh        # 从源码构建 Karmada，建 k014-host、k014-m1、k014-m2、k014-m3
kind/03-reset-feature-gates.sh     # local-up 默认开 AllAlpha，这里恢复成默认
kind/02-setup-members.sh           # 假 GPU、kthena、控制面上的 ModelServing CRD

source kind/00-env.sh && source kind/lib.sh    # kk / kh / km 三个 kubectl 快捷方式，snapshot 等函数
kk apply -f interpreter/modelserving-customization.yaml
# 之后按各日志开头的命令执行

kind/99-cleanup.sh                 # 删除集群、kubeconfig 和 local-up 构建的镜像
```

注意：

- `01-local-up-karmada.sh` 在运行期间会临时修改 Karmada 检出目录里的 `artifacts/kindClusterConfig/member1.yaml`（给 member1 加两个 worker 节点），退出时用 `git checkout` 还原。
- `local-up` 会把镜像打成 `docker.io/karmada/*:latest`，往 `GOPATH/bin` 里装 `cfssl` 和 `karmadactl`，并在 Karmada 检出目录里留下 `_output/`。
  `99-cleanup.sh` 只清理镜像；后两样需要手工删除。
- 方案 G 的场景另外需要 `kind/04-set-feature-gate.sh MultiplePodTemplatesScheduling=true`，把 `interpreter/` 的解释器换成 `unit/unit-customization.yaml`，并 `source unit/lib-unit.sh`。
  G-4b 起的场景还打开了 `SchedulingOvercommitProtection`：`kind/04-set-feature-gate.sh MultiplePodTemplatesScheduling=true,SchedulingOvercommitProtection=true`，
  并在三个 `karmada-scheduler-estimator-*` 的 Deployment 上手工加了同样的 `--feature-gates`（脚本不覆盖 estimator）。
- 成员集群没有装 Volcano。要验证 gang 调度下的行为，需要自己在成员里装 Volcano 并把单元的 `schedulerName` 改回 `volcano`。

包级探针：把 `estimator-probe/` 下的文件复制到 Karmada 的对应包里再运行，跑完删掉。

| 文件 | 复制到 | 运行 |
| --- | --- | --- |
| `accurate_estimator_probe_test.go.txt` | `pkg/estimator/server/zz_probe_test.go` | `go test ./pkg/estimator/server/ -run TestK014ProbeAccurateEstimator -v -count=1` |
| `general_estimator_probe_test.go.txt` | `pkg/estimator/client/zz_probe_test.go` | `go test ./pkg/estimator/client/ -run TestK014ProbeGeneralEstimator -v -count=1` |
| `g1_default_resource_models_probe_test.go.txt` | `pkg/controllers/status/zz_probe_test.go` | `go test ./pkg/controllers/status/ -run TestK014ProbeG1DefaultResourceModels -v -count=1` |

解释器的离线检查不需要集群：`interpreter/run-offline.sh`（`KARMADACTL` 指向从 Karmada 检出目录构建的 `karmadactl`）。
