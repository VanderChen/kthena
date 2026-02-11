# Kubernetes 部署指南

本文档介绍如何在 Kubernetes 集群中运行 ModelServing 集成测试，无需本地编译。

## 概述

通过 Kubernetes Job 运行测试的优势：

1. **无需本地编译** - 直接在集群中运行，节省本地资源
2. **环境一致性** - 测试环境与生产环境一致
3. **CI/CD 集成** - 轻松集成到自动化流水线
4. **资源隔离** - 测试资源独立管理，不影响本地环境
5. **日志持久化** - 测试日志保存在集群中，方便审计

## 快速开始

### 方式一：使用 Make（推荐）

```bash
# 1. 构建镜像并运行所有测试
make test-integration-modelserving-k8s

# 2. 只运行冒烟测试
make test-integration-modelserving-k8s-smoke

# 3. 查看测试日志
make test-integration-modelserving-k8s-logs

# 4. 清理测试资源
make test-integration-modelserving-k8s-clean
```

### 方式二：使用 deploy.sh 脚本

```bash
cd test/integration/modelserving

# 构建并部署测试
./deploy.sh deploy

# 查看日志
./deploy.sh logs

# 清理资源
./deploy.sh clean
```

## 详细用法

### deploy.sh 命令详解

```bash
./deploy.sh [ACTION] [OPTIONS]
```

#### 可用命令

| 命令 | 说明 |
|------|------|
| `build` | 仅构建 Docker 镜像 |
| `deploy` | 构建镜像并部署到集群（默认） |
| `run` | 运行已存在的测试 Job |
| `logs` | 查看测试日志 |
| `clean` | 清理测试资源 |
| `smoke` | 运行冒烟测试 |
| `help` | 显示帮助信息 |

#### 环境变量配置

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `IMAGE_NAME` | Docker 镜像名称 | `modelserving-test` |
| `IMAGE_TAG` | Docker 镜像标签 | `latest` |
| `NAMESPACE` | Kubernetes 命名空间 | `default` |
| `TEST_FILTER` | 测试过滤正则表达式 | `.*` (所有测试) |
| `TEST_TIMEOUT` | 测试超时时间 | `30m` |
| `DOCKER_REGISTRY` | Docker 镜像仓库（可选） | 无 |

### 使用示例

#### 1. 运行所有测试

```bash
./deploy.sh deploy
```

这会：
1. 构建 Docker 镜像
2. 创建 Kubernetes Job
3. 自动显示测试日志

#### 2. 运行特定测试

```bash
# 只运行 Scaling 相关测试
TEST_FILTER="TestScaling" ./deploy.sh deploy

# 只运行特定测试用例
TEST_FILTER="TestServingGroupScale" ./deploy.sh deploy

# 运行多个测试
TEST_FILTER="TestLifecycle|TestScaling" ./deploy.sh deploy
```

#### 3. 调整超时时间

```bash
# 延长超时到 60 分钟
TEST_TIMEOUT="60m" ./deploy.sh deploy
```

#### 4. 部署到指定命名空间

```bash
# 部署到自定义命名空间
NAMESPACE="kthena-integration" ./deploy.sh deploy
```

#### 5. 推送到远程镜像仓库

对于无法直接访问本地 Docker daemon 的远程集群：

```bash
# 推送到 Docker Hub
DOCKER_REGISTRY=docker.io \
IMAGE_NAME=myusername/modelserving-test \
IMAGE_TAG=v1.0.0 \
./deploy.sh build

# 然后部署（使用远程镜像）
IMAGE_NAME=docker.io/myusername/modelserving-test \
IMAGE_TAG=v1.0.0 \
./deploy.sh deploy
```

#### 6. 仅构建镜像

```bash
# 构建镜像但不部署
./deploy.sh build

# 构建并推送到仓库
DOCKER_REGISTRY=myregistry.io \
IMAGE_NAME=myrepo/modelserving-test \
./deploy.sh build
```

#### 7. 查看测试日志

```bash
# 实时查看日志
./deploy.sh logs

# 或使用 kubectl
kubectl logs -f -l app=modelserving-test
```

#### 8. 重新运行测试

```bash
# 删除旧 Job 并创建新的
./deploy.sh run
```

#### 9. 清理测试资源

```bash
# 删除 Job 和测试创建的命名空间
./deploy.sh clean
```

## 架构说明

### Docker 镜像

镜像构建分为两个阶段：

1. **构建阶段** (`golang:1.21-alpine`)
   - 下载 Go 依赖
   - 编译测试二进制文件
   - 精简构建产物

2. **运行阶段** (`alpine:3.19`)
   - 安装 `kubectl`
   - 复制测试二进制、YAML 测试用例和 fixtures
   - 创建启动脚本

最终镜像大小约 100MB，包含：
- 测试二进制文件
- 所有 YAML 测试用例
- Fixture 模板
- kubectl 工具

### Kubernetes 资源

#### ServiceAccount

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: modelserving-test-runner
```

测试 Pod 使用此 ServiceAccount 来访问 Kubernetes API。

#### ClusterRole

授予以下权限：
- ModelServing 资源的完全控制
- Pod 的创建、删除、查看、exec
- Namespace 的创建和删除
- ConfigMap 和 Service 的管理
- Event 的查看（用于断言）

#### Job

- **重启策略**: `Never` - 失败不重试
- **TTL**: 3600 秒（1 小时后自动清理）
- **资源限制**:
  - CPU: 100m ~ 1000m
  - Memory: 256Mi ~ 1Gi

## 定时测试（CronJob）

### 部署 CronJob

```bash
# 应用 CronJob 配置（每天凌晨 2 点运行）
kubectl apply -f test/integration/modelserving/k8s/cronjob.yaml
```

### 自定义调度时间

编辑 `k8s/cronjob.yaml`，修改 `schedule` 字段：

```yaml
spec:
  # Cron 格式: 分 时 日 月 周
  schedule: "0 2 * * *"  # 每天 2:00 AM
  # schedule: "0 */4 * * *"  # 每 4 小时
  # schedule: "0 0 * * 1"  # 每周一午夜
```

### 手动触发定时任务

```bash
# 从 CronJob 创建一次性 Job
kubectl create job --from=cronjob/modelserving-integration-test manual-test-$(date +%s)

# 查看运行状态
kubectl get jobs -l app=modelserving-test

# 查看日志
kubectl logs -l app=modelserving-test --tail=100
```

### 管理 CronJob

```bash
# 查看 CronJob
kubectl get cronjob modelserving-integration-test

# 暂停 CronJob
kubectl patch cronjob modelserving-integration-test -p '{"spec":{"suspend":true}}'

# 恢复 CronJob
kubectl patch cronjob modelserving-integration-test -p '{"spec":{"suspend":false}}'

# 删除 CronJob
kubectl delete cronjob modelserving-integration-test
```

## CI/CD 集成

### GitLab CI

`.gitlab-ci.yml`:

```yaml
stages:
  - test

modelserving-integration-test:
  stage: test
  image: docker:latest
  services:
    - docker:dind
  before_script:
    - apk add --no-cache kubectl bash
    - echo "$KUBECONFIG_CONTENT" | base64 -d > /tmp/kubeconfig
    - export KUBECONFIG=/tmp/kubeconfig
  script:
    - cd test/integration/modelserving
    - |
      # 推送到 GitLab Registry
      docker login -u $CI_REGISTRY_USER -p $CI_REGISTRY_PASSWORD $CI_REGISTRY
      DOCKER_REGISTRY=$CI_REGISTRY \
      IMAGE_NAME=$CI_REGISTRY_IMAGE/modelserving-test \
      IMAGE_TAG=$CI_COMMIT_SHORT_SHA \
      ./deploy.sh build
    - |
      # 运行测试
      IMAGE_NAME=$CI_REGISTRY_IMAGE/modelserving-test \
      IMAGE_TAG=$CI_COMMIT_SHORT_SHA \
      ./deploy.sh deploy
  artifacts:
    when: always
    reports:
      junit: test-results/*.xml
  only:
    - merge_requests
    - main
```

### GitHub Actions

`.github/workflows/integration-test.yml`:

```yaml
name: ModelServing Integration Tests

on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

jobs:
  integration-test:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout code
        uses: actions/checkout@v3

      - name: Set up kubectl
        uses: azure/setup-kubectl@v3

      - name: Configure kubeconfig
        run: |
          mkdir -p ~/.kube
          echo "${{ secrets.KUBE_CONFIG }}" | base64 -d > ~/.kube/config

      - name: Login to GitHub Container Registry
        uses: docker/login-action@v2
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - name: Build and push test image
        run: |
          cd test/integration/modelserving
          DOCKER_REGISTRY=ghcr.io \
          IMAGE_NAME=${{ github.repository }}/modelserving-test \
          IMAGE_TAG=${{ github.sha }} \
          ./deploy.sh build

      - name: Run integration tests
        run: |
          cd test/integration/modelserving
          IMAGE_NAME=ghcr.io/${{ github.repository }}/modelserving-test \
          IMAGE_TAG=${{ github.sha }} \
          ./deploy.sh deploy

      - name: Upload test logs
        if: always()
        uses: actions/upload-artifact@v3
        with:
          name: test-logs
          path: test-results/
```

### Jenkins Pipeline

`Jenkinsfile`:

```groovy
pipeline {
    agent any

    environment {
        DOCKER_REGISTRY = 'myregistry.io'
        IMAGE_NAME = "${DOCKER_REGISTRY}/modelserving-test"
        IMAGE_TAG = "${BUILD_NUMBER}"
    }

    stages {
        stage('Build Test Image') {
            steps {
                script {
                    dir('test/integration/modelserving') {
                        sh '''
                            ./deploy.sh build
                        '''
                    }
                }
            }
        }

        stage('Run Integration Tests') {
            steps {
                script {
                    dir('test/integration/modelserving') {
                        sh '''
                            ./deploy.sh deploy
                        '''
                    }
                }
            }
        }

        stage('Cleanup') {
            steps {
                script {
                    dir('test/integration/modelserving') {
                        sh '''
                            ./deploy.sh clean
                        '''
                    }
                }
            }
            post {
                always {
                    sh './deploy.sh clean'
                }
            }
        }
    }

    post {
        always {
            archiveArtifacts artifacts: 'test-results/**/*', allowEmptyArchive: true
        }
    }
}
```

## 故障排查

### 1. 镜像拉取失败

**问题**: `ImagePullBackOff` 错误

**解决方案**:

```bash
# 检查镜像是否存在
docker images | grep modelserving-test

# 如果使用远程仓库，检查镜像仓库凭证
kubectl create secret docker-registry regcred \
  --docker-server=myregistry.io \
  --docker-username=myuser \
  --docker-password=mypass

# 更新 Job 配置使用镜像拉取凭证
kubectl patch job modelserving-integration-test \
  -p '{"spec":{"template":{"spec":{"imagePullSecrets":[{"name":"regcred"}]}}}}'
```

### 2. 权限不足

**问题**: 测试失败，日志显示权限错误

**解决方案**:

```bash
# 检查 RBAC 权限
kubectl describe clusterrole modelserving-test-runner

# 确保 ClusterRoleBinding 正确
kubectl get clusterrolebinding modelserving-test-runner -o yaml

# 重新应用 RBAC
kubectl apply -f test/integration/modelserving/k8s/job.yaml
```

### 3. 测试超时

**问题**: 测试运行时间过长

**解决方案**:

```bash
# 增加超时时间
TEST_TIMEOUT="60m" ./deploy.sh deploy

# 或只运行特定测试
TEST_FILTER="TestServingGroupScale" ./deploy.sh deploy
```

### 4. Pod 无法启动

**问题**: Pod 一直处于 Pending 状态

**解决方案**:

```bash
# 查看 Pod 详情
kubectl describe pod -l app=modelserving-test

# 检查资源是否充足
kubectl top nodes

# 降低资源请求
# 编辑 k8s/job.yaml 中的 resources 部分
```

### 5. 查看详细日志

```bash
# 查看 Pod 事件
kubectl get events --sort-by='.lastTimestamp' -n default

# 查看测试 Pod 日志
kubectl logs -l app=modelserving-test --tail=500

# 查看之前的 Pod 日志（如果重启）
kubectl logs -l app=modelserving-test --previous
```

### 6. 清理卡住的资源

```bash
# 强制删除 Job
kubectl delete job modelserving-integration-test --grace-period=0 --force

# 清理测试命名空间
kubectl get ns | grep kthena-integration | awk '{print $1}' | xargs kubectl delete ns

# 清理挂起的 Pod
kubectl delete pods --field-selector=status.phase=Pending -n default
```

## 最佳实践

### 1. 资源管理

- 定期清理旧的 Job：`kubectl delete jobs -l app=modelserving-test --field-selector=status.successful=1`
- 使用 TTL 自动清理：Job 配置中的 `ttlSecondsAfterFinished`
- 限制并发测试：CronJob 使用 `concurrencyPolicy: Forbid`

### 2. 镜像管理

- 使用语义化版本标签：`v1.0.0`，`v1.1.0` 等
- 保留最近的镜像，删除旧版本
- 对于 CI，使用 commit SHA 作为标签

### 3. 监控和告警

- 配置 CronJob 失败告警
- 收集测试指标（成功率、耗时等）
- 保存测试报告到持久化存储

### 4. 安全性

- 使用专用 ServiceAccount，最小化权限
- 不要在镜像中硬编码敏感信息
- 使用 Secret 管理镜像仓库凭证

## 总结

通过 Kubernetes Job 运行集成测试的优势：

✅ **快速部署** - 一条命令即可运行测试
✅ **环境一致** - 测试环境与生产环境相同
✅ **易于集成** - 轻松集成到 CI/CD 流程
✅ **资源隔离** - 独立的测试环境，互不干扰
✅ **自动清理** - TTL 机制自动清理过期资源
✅ **灵活配置** - 支持各种测试场景和参数

建议的使用场景：

- **本地开发**: 使用 `go test` 或 `make test-integration-modelserving`
- **CI/CD**: 使用 Kubernetes Job 方式
- **定期回归**: 使用 CronJob 定时运行
- **生产验证**: 部署后运行冒烟测试验证

如有问题，请查看 [README.md](README.md) 或提交 Issue。
