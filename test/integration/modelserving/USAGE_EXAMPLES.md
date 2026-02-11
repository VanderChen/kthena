# Kubernetes 部署使用示例

本文档提供实际使用场景的详细示例。

## 场景 1：本地开发快速验证

开发完成后，在提交代码前快速验证：

```bash
# 运行冒烟测试（2-5 分钟）
make test-integration-modelserving-k8s-smoke

# 如果冒烟测试通过，运行完整测试
make test-integration-modelserving-k8s

# 清理测试资源
make test-integration-modelserving-k8s-clean
```

## 场景 2：只运行特定测试

开发了新的扩缩容功能，只需测试相关用例：

```bash
cd test/integration/modelserving

# 只运行 Scaling 测试
TEST_FILTER="TestScaling" ./deploy.sh deploy

# 或只运行特定测试用例
TEST_FILTER="TestServingGroupScale" ./deploy.sh deploy
```

## 场景 3：推送到私有镜像仓库

公司使用私有 Harbor 仓库：

```bash
cd test/integration/modelserving

# 登录 Harbor
docker login harbor.company.com

# 构建并推送镜像
DOCKER_REGISTRY=harbor.company.com \
IMAGE_NAME=kthena/modelserving-test \
IMAGE_TAG=v1.0.0 \
./deploy.sh build

# 在集群中运行测试
IMAGE_NAME=harbor.company.com/kthena/modelserving-test \
IMAGE_TAG=v1.0.0 \
./deploy.sh deploy
```

## 场景 4：多集群测试

在开发、测试、生产三个集群中运行测试：

```bash
cd test/integration/modelserving

# 开发集群
kubectl config use-context dev-cluster
NAMESPACE=kthena-dev \
TEST_TIMEOUT="15m" \
./deploy.sh smoke

# 测试集群
kubectl config use-context test-cluster
NAMESPACE=kthena-test \
TEST_TIMEOUT="30m" \
./deploy.sh deploy

# 生产集群（只运行冒烟测试）
kubectl config use-context prod-cluster
NAMESPACE=kthena-prod \
TEST_FILTER="TestLifecycleCreateDelete|TestServingGroupScale" \
./deploy.sh deploy
```

## 场景 5：CI/CD Pipeline 集成

### GitLab CI 完整示例

`.gitlab-ci.yml`:

```yaml
stages:
  - build
  - test
  - deploy

variables:
  DOCKER_DRIVER: overlay2
  IMAGE_NAME: $CI_REGISTRY_IMAGE/modelserving-test
  IMAGE_TAG: $CI_COMMIT_SHORT_SHA

# 构建测试镜像
build-test-image:
  stage: build
  image: docker:latest
  services:
    - docker:dind
  before_script:
    - docker login -u $CI_REGISTRY_USER -p $CI_REGISTRY_PASSWORD $CI_REGISTRY
  script:
    - cd test/integration/modelserving
    - |
      DOCKER_REGISTRY=$CI_REGISTRY \
      IMAGE_NAME=$IMAGE_NAME \
      IMAGE_TAG=$IMAGE_TAG \
      ./deploy.sh build
  only:
    - merge_requests
    - main

# 冒烟测试（快速反馈）
smoke-test:
  stage: test
  image: alpine:latest
  needs:
    - build-test-image
  before_script:
    - apk add --no-cache kubectl bash
    - echo "$KUBECONFIG_DEV" | base64 -d > /tmp/kubeconfig
    - export KUBECONFIG=/tmp/kubeconfig
  script:
    - cd test/integration/modelserving
    - |
      IMAGE_NAME=$IMAGE_NAME \
      IMAGE_TAG=$IMAGE_TAG \
      NAMESPACE=kthena-ci \
      ./deploy.sh smoke
  only:
    - merge_requests

# 完整测试（合并到主分支时）
full-test:
  stage: test
  image: alpine:latest
  needs:
    - build-test-image
  before_script:
    - apk add --no-cache kubectl bash
    - echo "$KUBECONFIG_TEST" | base64 -d > /tmp/kubeconfig
    - export KUBECONFIG=/tmp/kubeconfig
  script:
    - cd test/integration/modelserving
    - |
      IMAGE_NAME=$IMAGE_NAME \
      IMAGE_TAG=$IMAGE_TAG \
      NAMESPACE=kthena-test \
      TEST_TIMEOUT="60m" \
      ./deploy.sh deploy
  only:
    - main

# 生产验证（部署后）
production-validation:
  stage: deploy
  image: alpine:latest
  before_script:
    - apk add --no-cache kubectl bash
    - echo "$KUBECONFIG_PROD" | base64 -d > /tmp/kubeconfig
    - export KUBECONFIG=/tmp/kubeconfig
  script:
    - cd test/integration/modelserving
    - |
      IMAGE_NAME=$IMAGE_NAME \
      IMAGE_TAG=$IMAGE_TAG \
      NAMESPACE=kthena-prod \
      TEST_FILTER="TestLifecycleCreateDelete|TestServingGroupScale" \
      ./deploy.sh deploy
  when: manual
  only:
    - main
```

### GitHub Actions 完整示例

`.github/workflows/integration-test.yml`:

```yaml
name: ModelServing Integration Tests

on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

env:
  REGISTRY: ghcr.io
  IMAGE_NAME: ${{ github.repository }}/modelserving-test

jobs:
  smoke-test:
    name: Smoke Test
    runs-on: ubuntu-latest
    permissions:
      contents: read
      packages: write
    steps:
      - name: Checkout
        uses: actions/checkout@v3

      - name: Set up kubectl
        uses: azure/setup-kubectl@v3
        with:
          version: 'v1.28.0'

      - name: Configure kubeconfig
        run: |
          mkdir -p ~/.kube
          echo "${{ secrets.KUBE_CONFIG_DEV }}" | base64 -d > ~/.kube/config

      - name: Log in to GitHub Container Registry
        uses: docker/login-action@v2
        with:
          registry: ${{ env.REGISTRY }}
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - name: Build test image
        run: |
          cd test/integration/modelserving
          DOCKER_REGISTRY=${{ env.REGISTRY }} \
          IMAGE_NAME=${{ env.IMAGE_NAME }} \
          IMAGE_TAG=${{ github.sha }} \
          ./deploy.sh build

      - name: Run smoke tests
        run: |
          cd test/integration/modelserving
          IMAGE_NAME=${{ env.REGISTRY }}/${{ env.IMAGE_NAME }} \
          IMAGE_TAG=${{ github.sha }} \
          NAMESPACE=kthena-ci \
          ./deploy.sh smoke

      - name: Upload test logs
        if: always()
        uses: actions/upload-artifact@v3
        with:
          name: smoke-test-logs
          path: /tmp/test-logs/

  full-test:
    name: Full Test
    runs-on: ubuntu-latest
    needs: smoke-test
    if: github.ref == 'refs/heads/main'
    permissions:
      contents: read
      packages: write
    steps:
      - name: Checkout
        uses: actions/checkout@v3

      - name: Set up kubectl
        uses: azure/setup-kubectl@v3

      - name: Configure kubeconfig
        run: |
          mkdir -p ~/.kube
          echo "${{ secrets.KUBE_CONFIG_TEST }}" | base64 -d > ~/.kube/config

      - name: Log in to Registry
        uses: docker/login-action@v2
        with:
          registry: ${{ env.REGISTRY }}
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - name: Run full tests
        run: |
          cd test/integration/modelserving
          IMAGE_NAME=${{ env.REGISTRY }}/${{ env.IMAGE_NAME }} \
          IMAGE_TAG=${{ github.sha }} \
          NAMESPACE=kthena-test \
          TEST_TIMEOUT="60m" \
          ./deploy.sh deploy

      - name: Upload test results
        if: always()
        uses: actions/upload-artifact@v3
        with:
          name: full-test-results
          path: /tmp/test-results/
```

## 场景 6：定期回归测试

设置每天凌晨 2 点自动运行完整测试：

```bash
# 1. 构建并推送测试镜像到仓库
cd test/integration/modelserving
DOCKER_REGISTRY=harbor.company.com \
IMAGE_NAME=kthena/modelserving-test \
IMAGE_TAG=stable \
./deploy.sh build

# 2. 编辑 CronJob 配置
vi k8s/cronjob.yaml
# 修改 image 为你的镜像地址

# 3. 部署 CronJob
kubectl apply -f k8s/cronjob.yaml -n kthena-system

# 4. 验证 CronJob
kubectl get cronjob -n kthena-system

# 5. 手动触发一次测试
kubectl create job --from=cronjob/modelserving-integration-test \
  manual-test-$(date +%s) -n kthena-system

# 6. 查看测试结果
kubectl logs -l app=modelserving-test -n kthena-system --tail=100
```

配置 Slack 通知（可选）：

```yaml
# 在 CronJob 的 spec.jobTemplate.spec.template.spec 中添加
containers:
  - name: test-runner
    # ... 现有配置 ...
  - name: slack-notifier
    image: curlimages/curl:latest
    command:
      - sh
      - -c
      - |
        if [ -f /results/failed ]; then
          curl -X POST $SLACK_WEBHOOK_URL \
            -H 'Content-Type: application/json' \
            -d '{"text":"❌ ModelServing 集成测试失败！查看日志：kubectl logs -l app=modelserving-test"}'
        else
          curl -X POST $SLACK_WEBHOOK_URL \
            -H 'Content-Type: application/json' \
            -d '{"text":"✅ ModelServing 集成测试全部通过！"}'
        fi
    env:
      - name: SLACK_WEBHOOK_URL
        valueFrom:
          secretKeyRef:
            name: slack-webhook
            key: url
```

## 场景 7：调试失败的测试

测试失败时，如何快速定位问题：

```bash
# 1. 查看 Job 状态
kubectl get job modelserving-integration-test

# 2. 查看 Pod 详情
kubectl describe pod -l app=modelserving-test

# 3. 查看完整日志
kubectl logs -l app=modelserving-test --tail=1000 > test.log

# 4. 搜索错误信息
grep -i "error\|fail" test.log

# 5. 如果需要进入 Pod 调试
POD_NAME=$(kubectl get pods -l app=modelserving-test -o jsonpath='{.items[0].metadata.name}')
kubectl exec -it $POD_NAME -- sh

# 6. 在 Pod 中手动运行测试
./modelserving-test -test.v -test.run TestServingGroupScale

# 7. 查看测试创建的资源
kubectl get modelservings -A | grep kthena-integration
kubectl get pods -A | grep kthena-integration

# 8. 清理测试资源
./deploy.sh clean
```

## 场景 8：性能基准测试

测量测试执行时间：

```bash
cd test/integration/modelserving

# 运行测试并记录时间
time ./deploy.sh deploy

# 或在 CronJob 中添加时间记录
kubectl logs -l app=modelserving-test | grep "Test.*PASS\|Test.*FAIL" | \
  awk '{print $1,$2,$NF}' > test-timings.txt
```

## 场景 9：跨命名空间测试

在不同命名空间中并行运行测试：

```bash
# 终端 1：在 ns-1 中运行
NAMESPACE=kthena-test-1 \
TEST_FILTER="TestLifecycle" \
./deploy.sh deploy &

# 终端 2：在 ns-2 中运行
NAMESPACE=kthena-test-2 \
TEST_FILTER="TestScaling" \
./deploy.sh deploy &

# 终端 3：在 ns-3 中运行
NAMESPACE=kthena-test-3 \
TEST_FILTER="TestRecovery" \
./deploy.sh deploy &

# 等待所有测试完成
wait

# 汇总结果
for ns in kthena-test-{1..3}; do
  echo "=== Namespace: $ns ==="
  kubectl get job -n $ns
done
```

## 场景 10：金丝雀测试

新版本部署前的验证：

```bash
# 1. 部署新版本的 ModelServing Controller（金丝雀）
kubectl apply -f new-controller-canary.yaml

# 2. 运行完整测试验证新版本
cd test/integration/modelserving
NAMESPACE=canary-test \
TEST_TIMEOUT="60m" \
./deploy.sh deploy

# 3. 如果测试通过，查看详细结果
kubectl logs -l app=modelserving-test -n canary-test

# 4. 如果测试失败，回滚
kubectl rollout undo deployment/model-serving-controller

# 5. 清理金丝雀测试
NAMESPACE=canary-test ./deploy.sh clean
```

## 总结

这些示例涵盖了从本地开发到生产部署的各种场景。根据实际需求选择合适的方式：

- **本地开发**: 使用 `make` 命令快速验证
- **CI/CD**: 集成到流水线自动化测试
- **定期测试**: 使用 CronJob 定时回归
- **生产验证**: 部署后运行冒烟测试
- **问题调试**: 使用日志和 Pod exec 排查问题

更多信息请参考：
- [DEPLOY_GUIDE.md](DEPLOY_GUIDE.md) - 完整部署指南
- [README.md](README.md) - 测试框架文档
- [QUICKSTART.md](QUICKSTART.md) - 快速开始
