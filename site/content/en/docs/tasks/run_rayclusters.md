---
title: "Run A RayCluster"
date: 2024-01-04
weight: 6
description: >
  Run a RayCluster on Kueue.
---

This page shows how to leverage Kueue's scheduling and resource management capabilities when running [RayCluster](https://docs.ray.io/en/latest/cluster/getting-started.html).

This guide is for [batch users](/docs/tasks#batch-user) that have a basic understanding of Kueue. For more information, see [Kueue's overview](/docs/overview).

## Before you begin

1. By default, the integration for `ray.io/raycluster` is not enabled.
  Learn how to [install Kueue with a custom manager configuration](/docs/installation/#install-a-custom-configured-released-version)
  and enable the `ray.io/raycluster` integration.

2. Check [Administer cluster quotas](/docs/tasks/administer_cluster_quotas) for details on the initial Kueue setup.

3. See [KubeRay Installation](https://ray-project.github.io/kuberay/deploy/installation/) for installation and configuration details of KubeRay.

## RayCluster definition

When running [RayClusters](https://docs.ray.io/en/latest/cluster/getting-started.html) on
Kueue, take into consideration the following aspects:

### a. Queue selection

The target [local queue](/docs/concepts/local_queue) should be specified in the `metadata.labels` section of the RayJob configuration.

```yaml
metadata:
  name: raycluster-sample
  namespace: default
  labels:
    kueue.x-k8s.io/queue-name: local-queue
```

### b. Configure the resource needs

The resource needs of the workload can be configured in the `spec`.

```yaml
    headGroupSpec:
       spec:
        affinity: {}
        containers:
        - env: []
          image: rayproject/ray:2.7.0
          imagePullPolicy: IfNotPresent
          name: ray-head
          resources:
            limits:
              cpu: "1"
              memory: 2G
            requests:
              cpu: "1"
              memory: 2G
          securityContext: {}
          volumeMounts:
          - mountPath: /tmp/ray
            name: log-volume
    workerGroupSpecs:
      template:
        spec:
          affinity: {}
          containers:
          - env: []
          image: rayproject/ray:2.7.0
          imagePullPolicy: IfNotPresent
          name: ray-worker
          resources:
            limits:
            cpu: "1"
            memory: 1G
            requests:
            cpu: "1"
            memory: 1G
```

### c. Limitations
- Because a Kueue workload can have a maximum of 8 PodSets, the maximum number of `spec.workerGroupSpecs` is 7.

## Example RayJob

The RayCluster looks like the following:

{{< include "examples/jobs/ray-cluster-sample.yaml" "yaml" >}}

You can submit a Ray Job using the [CLI](https://docs.ray.io/en/latest/cluster/running-applications/job-submission/quickstart.html) or log into the Ray Head and execute a job following this [example](https://ray-project.github.io/kuberay/deploy/helm-cluster/#end-to-end-example) with kind cluster. 
