# Prescaler
Prescaler allows you to proactively scale up your Kubernetes HPAs minutes before anticipated spikes in load or traffic, such as those occurring on the hour. After this initial prescaling, your HPA will manage subsequent scaling, either continuing to scale up or scaling down once the spike has passed.

## Description
Imagine you have a Kubernetes deployment of `SuperHeavyApp` with 10 replicas and an associated **Horizontal Pod Autoscaler (HPA)**.
Every hour, your application experiences a brief, intense traffic spike that necessitates scaling `SuperHeavyApp` up to 200% (requiring 10 additional pods) with total 20 pods.

Here's how a typical scenario might unfold:

- **8:30:00:** You have 10 `SuperHeavyApp` pods ready, with HPA at 70% CPU utilization (based on 2000mCPU across 10 pods).
- **8:55:00:** Still 10 `SuperHeavyApp` pods ready, HPA remains at 70% CPU utilization.
- **9:00:00:** The traffic spike begins. You still have 10 `SuperHeavyApp` pods ready, but HPA CPU utilization jumps to 140%.
- **9:00:30:** HPA triggers a scale-up of 10 additional pods. However, clients experience throttling or disruptions (receiving 503/504 errors) due to the delay.
- **9:02:00:** The traffic spike ends. You have 10 `SuperHeavyApp` pods ready, but 10 new pods are stuck in a pending state, waiting for a new node. HPA is still at 140% CPU utilization (among the initial 10 pods).
- **9:03:00:** A new node is added to the cluster (by Karpenter/Cluster Autoscaler), and the 10 new pods become available. HPA now shows 70% CPU utilization (among 20 pods).
- **!?!?!?**
- **9:08:00:** HPA scales back down to 10 `SuperHeavyApp` pods, with CPU utilization at 70% (among the 10 pods).

### In this case we have two options:
- Configure **Horizontal Pod Autoscaler (HPA)** to scale based on **40% CPU usage** with a `stabilizationWindowSeconds` of `3600`. This will keep it upscaled most of the time, leading to wasted resources and money.
- Use **Prescaler**. For the `SuperHeavyApp` HPA/Deployment, configure Prescaler's Custom Resource (CR) to prescale to **200% at "56 * * * *"**. At 8:56:00, Prescaler will:
    - Change HPA's CPU `averageUtilization` from (current! CPU averageUtilization) 70% to (70*100/200) 35% and set `hpa.Spec.Behavior.ScaleUp.StabilizationWindowSeconds` to 0 (for immediate scale-up).
    - HPA will start scaling up from 10 to 20 pods (2x). Prescaler will then wait for XX seconds.
    - Once HPA has scaled to 20 pods, Prescaler will revert HPA's CPU `averageUtilization` and `hpa.Spec.Behavior.ScaleUp.StabilizationWindowSeconds` to their original values.
    - HPA will remain upscaled (with real usage at 35% across 20 pods) until `behavior.scaleDown.stabilizationWindowSeconds`. **It's better to increase this to 10 minutes** (the default is 5 minutes).
    - At the top of the hour (9:00:00), you'll experience a traffic spike, and CPU usage will naturally increase to the expected 70% (across 20 pods).
    - If the spike continues, HPA will not scale down; it will even continue to scale up natively.
    - If the spike ends, HPA will scale down after it concludes (e.g., if the spike ends at 9:03:00, HPA scales down at 9:08:00, i.e., 9:03:00 + 5 minutes).
    - This process repeats for every hour.

### If your SuperHeavyApp takes a lot of time to start:
- Increase `behavior.scaleDown.stabilizationWindowSeconds` from recommended 10 minutes (for example, to 20 minutes)
- Start prescaling earlier (for example, at `"51 * * * *"`), or use multiple schedules for different times of day.

## Deployment and Custom Resources
**Attention!**
- When you specify a cron schedule, keep in mind a **timezone**! Probably controller will run in your Kubernetes cluster in UTC timezone!
- Also check maxConcurrentReconciles parameter and logs (in case you have overdue prescaling)
- Prescaling may not be accurate/precise due to constant changes in scaling and CPU usage

[Helm Chart](dist/chart)

[Example Prescale CR](config/samples/prescaler_v1_prescale.yaml):
```yaml
spec:
  targetHpaName: nginx-project # target HPA to scale, must be in the same namespace as Prescale CR
  schedules:
    - cron: "55 * * * 6,0"
      percent: 165
    - cron: "55 0-1 * * 1-5"
      percent: 200
    - cron: "55 2-12 * * 1-5"
      percent: 500
    - cron: "55 13-23 * * 1-5"
      percent: 200 # To calculate new CPU AverageUtilization value based on formula: original * 100 / percent
  suspend: false # enable/disable prescaler
  revertWaitSeconds: 40 # max wait time before reverting back to original values. It is important, because we must provide Kubernetes time to detect and react on HPA changes (to trigger scaleup desiredReplicas)
```

**Notice** how `percent` parameter works! It is used in formula to calculate new/temporary value for CPU AverageUtilization.
For example: 
- you have 30 pods
- HPA periodically calculates averageUtilization and updates its status in `status.currentMetrics`, for example, current avg cpu = 50
- in Prescaler you set percent = 200

Than Prescaler will do math 50 * 100 / 200 = 25 and set CPU AverageUtilization = 25. HPA will scale up to 60 pods, because it will need 60 pods to maintain avg cpu about 25% (because on 50% it needed 30 pods)

## Getting Started with Controller development

### Prerequisites
- go version v1.24.0+
- docker version 17.03+.
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.11.3+ cluster.

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=<some-registry>/prescaler:tag
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/prescaler:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

>**NOTE**: Ensure that the samples has default values to test it out.

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Project Distribution

Following the options to release and provide this solution to the users.

### By providing a bundle with all YAML files

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/prescaler:tag
```

**NOTE:** The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without its
dependencies.

2. Using the installer

Users can just run 'kubectl apply -f <URL for YAML BUNDLE>' to install
the project, i.e.:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/prescaler/<tag or branch>/dist/install.yaml
```

### By providing a Helm Chart

1. Build the chart using the optional helm plugin

```sh
kubebuilder edit --plugins=helm/v1-alpha
```

2. See that a chart was generated under 'dist/chart', and users
can obtain this solution from there.

**NOTE:** If you change the project, you need to update the Helm Chart
using the same command above to sync the latest changes. Furthermore,
if you create webhooks, you need to use the above command with
the '--force' flag and manually ensure that any custom configuration
previously added to 'dist/chart/values.yaml' or 'dist/chart/manager/manager.yaml'
is manually re-applied afterwards.

## Contributing
// TODO(user): Add detailed information on how you would like others to contribute to this project

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

