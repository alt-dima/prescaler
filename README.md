# Prescaler
Prescale/ScaleUp your HPAs in Kubernetes minutes before regular spikes of load/traffic (like on round hours) and let HPA do the rest after (to continue scale up or scale down after spike)

## Description
Imagine you have a Deployment of SuperHeavyApp in your Kubernetes with 10 replicas and HPA.
Every hour you have short/fast/temporary spike of traffic that requires to scale SuperHeavyApp up to +50% of pods.
For example,
- At 8:30:00 = 10 SuperHeavyApp ready pods, HPA at 70% from 2000mCPU among 10 pods
- At 8:55:00 = 10 SuperHeavyApp ready pods, HPA at 70% from 2000mCPU among 10 pods
- At 9:00:00 = 10 SuperHeavyApp ready pods, HPA at 140% from 2000mCPU among 10 pods, spike of traffic begins
- At 9:00:30 = HPA triggers to scale +10 pods, clients get throttled/distrupted (getting 503/504)
- At 9:02:00 = 10 SuperHeavyApp ready pods, 10 pods in pending state waiting for node, HPA at 140% from 2000mCPU among 10 pods, spike of traffic ends
- At 9:03:00 = New node added to cluster (by Karpenter/CA), 10 new pods become available, HPA at 70% from 2000mCPU among 20 pods
- !?!?!?
- At 9:08:00 = HPA scales back to 10 SuperHeavyApp pods, HPA at 70% from 2000mCPU among 10 pods

In this case we have two options
- Configure HPA to scale on 40% of CPU usage and `stabilizationWindowSeconds: 3600` so it will be upscaled most of the time with wasted resources/money
- Use Prescaler! And configure Prescaler CR for SuperHeavyApp HPA/Deployment to prescale by +50% at `"56 * * * *"`, and Prescaler will at 8:56:00 do:
  - Change HPA's CPU AverageUtilization from 70% to 35% and hpa.Spec.Behavior.ScaleUp.StabilizationWindowSeconds = 0 (to scaleUp now)
  - HPA will begin scaling X2 from 10 to 20 pods, Prescaler will begin to wait XX sec
  - HPA scaled to 20 pods, Prescaler waited and reverts back HPA's CPU AverageUtilization and hpa.Spec.Behavior.ScaleUp.StabilizationWindowSeconds to original values
  - HPA remains upscaled (real usage remains 35% among 20 pods) up to `behavior.scaleDown.stabilizationWindowSeconds`, which by default is 5 minutes
  - At round hour (9:00:00) you receive a spike of traffic and cpu usage naturally increase to expected 70% (among 20 pods)
  - If the spike continue, HPA will not scale down, event continue to natively scale up
  - If the spike ends, HPA will scale down after spike ends (spike ended at 9:03:00 +5 minutes => at 9:08:00 hpa scales down)
  - And so on for every round hour

Important!
If your SuperHeavyApp takes a lot of time to start:
- Increase `behavior.scaleDown.stabilizationWindowSeconds` from default 5 minutes (for example, to 10 minutes)
- Start prescaling earlier (for example, at `"51 * * * *"`)
In this case you will provide 9 minutes before round hour for pods to become ready

[Example Prescale CR](config/samples/prescaler_v1_prescale.yaml): 
```
spec:
  targetHpaName: nginx-project - target HPA to scale, must be in the same namespace as Prescale CR
  schedule: "55 * * * *" - Cron shedule to trigger prescale
  percent: 70 - prectent to decrease CPU AverageUtilization from current/original CPU AverageUtilization (if current/original AverageUtilization=70, percent=70, then Prescaler will temporary set AverageUtilization=70-70%=21%)
  suspend: false - enable/disable prescaler
  revertWaitSeconds: 40 - max wait time before reverting back to original values. It is important, because we must provide Kubernetes time to detect and react on HPA changes (to trigger scaleup desiredReplicas)
```

P.S. Cron/schedule logic taken from [book.kubebuilder.io/cronjob-tutorial](https://book.kubebuilder.io/cronjob-tutorial/controller-implementation#5-get-the-next-scheduled-run)

## Getting Started

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

