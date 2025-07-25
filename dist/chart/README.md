# Prescaler Helm Chart

This Helm chart deploys the Prescaler controller to manage prescale operations on HorizontalPodAutoscalers.

## Configuration

### MaxConcurrentReconciles

The `maxConcurrentReconciles` setting controls how many prescalers can be processed simultaneously. This is important for performance and resource management.

**Default Value**: 5

**Configuration**:
```yaml
controllerManager:
  maxConcurrentReconciles: 5
```

**Usage Examples**:

1. **Default installation** (5 concurrent reconciliations):
```bash
helm install prescaler ./dist/chart
```

2. **Custom concurrent reconciliations** (10 concurrent reconciliations):
```bash
helm install prescaler ./dist/chart --set controllerManager.maxConcurrentReconciles=10
```

3. **Conservative setting** (1 concurrent reconciliation):
```bash
helm install prescaler ./dist/chart --set controllerManager.maxConcurrentReconciles=1
```

4. **Using values file**:
```yaml
# custom-values.yaml
controllerManager:
  maxConcurrentReconciles: 8
  replicas: 1
  container:
    image:
      repository: altuhovsu/prescaler
      tag: latest
```

```bash
helm install prescaler ./dist/chart -f custom-values.yaml
```

## Important Notes

- **Per-HPA Concurrency**: The controller implements per-HPA locking, so multiple prescalers targeting different HPAs can run concurrently
- **Resource Usage**: Higher values increase CPU and memory usage
- **Recommended Range**: 1-10 for most deployments
- **Safety**: The controller includes deadlock prevention mechanisms

## Complete Configuration Example

```yaml
controllerManager:
  replicas: 1
  maxConcurrentReconciles: 5
  container:
    image:
      repository: altuhovsu/prescaler
      tag: latest
    args:
      - "--leader-elect"
      - "--metrics-bind-address=:8443"
      - "--health-probe-bind-address=:8081"
    resources:
      limits:
        cpu: 500m
        memory: 128Mi
      requests:
        cpu: 10m
        memory: 64Mi

rbac:
  enable: true

crd:
  enable: true
  keep: true

metrics:
  enable: true

prometheus:
  enable: false

certmanager:
  enable: false

networkPolicy:
  enable: false
``` 