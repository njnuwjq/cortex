/*
Copyright 2019 Cortex Labs, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package operator

import (
	"time"

	"github.com/cortexlabs/cortex/pkg/lib/errors"
	"github.com/cortexlabs/cortex/pkg/lib/k8s"
	"github.com/cortexlabs/cortex/pkg/lib/parallel"
	s "github.com/cortexlabs/cortex/pkg/lib/strings"
	"github.com/cortexlabs/cortex/pkg/operator/config"
	"github.com/cortexlabs/cortex/pkg/types/status"
	kapps "k8s.io/api/apps/v1"
	kcore "k8s.io/api/core/v1"
)

// will return nil if API does not exist
func GetStatus(apiName string) (*status.Status, error) {
	var deployment *kapps.Deployment
	var pods []kcore.Pod

	err := parallel.RunFirstErr(
		func() error {
			var err error
			deployment, err = config.K8s.Default.GetDeployment(apiName)
			return err
		},
		func() error {
			var err error
			pods, err = config.K8s.Default.ListPodsWithLabelKeys("apiName", apiName)
			return err
		},
	)

	if err != nil {
		return nil, err
	}

	return apiStatus(deployment, pods)
}

func GetAllStatuses() ([]status.Status, error) {
	var deployments []kapps.Deployment
	var pods []kcore.Pod

	err := parallel.RunFirstErr(
		func() error {
			var err error
			deployments, err = config.K8s.Default.ListDeploymentsWithLabelKeys("apiName")
			return err
		},
		func() error {
			var err error
			pods, err = config.K8s.Default.ListPodsWithLabelKeys("apiName")
			return err
		},
	)

	if err != nil {
		return nil, err
	}

	// TODO sort

	statuses := make([]status.Status, len(deployments))
	for i, deployment := range deployments {
		status, err := apiStatus(&deployment, pods)
		if err != nil {
			return nil, err
		}
		statuses[i] = *status
	}

	return statuses, nil
}

func apiStatus(deployment *kapps.Deployment, allPods []kcore.Pod) (*status.Status, error) {
	if deployment == nil {
		return nil, nil
	}

	minReplicas, ok := s.ParseInt32(deployment.Labels["apiName"])
	if !ok {
		return nil, errors.New("unable to parse min replicas from " + deployment.Labels["apiName"]) // unexpected
	}

	status := &status.Status{}
	status.APIName = deployment.Labels["apiName"]
	status.APIID = deployment.Labels["apiID"]
	status.ReplicaCounts = *getReplicaCounts(deployment, allPods)
	status.Code = getStatusCode(&status.ReplicaCounts, minReplicas)

	return status, nil
}

func getReplicaCounts(deployment *kapps.Deployment, allPods []kcore.Pod) *status.ReplicaCounts {
	counts := &status.ReplicaCounts{}

	counts.Requested = *deployment.Spec.Replicas

	for _, pod := range allPods {
		if pod.Labels["apiName"] != deployment.Labels["apiName"] {
			continue
		}
		updateReplicaCounts(counts, deployment, &pod)
	}

	return counts
}

func updateReplicaCounts(counts *status.ReplicaCounts, deployment *kapps.Deployment, pod *kcore.Pod) {
	var subCounts *status.SubReplicaCounts
	if k8s.IsPodSpecLatest(deployment, pod) {
		subCounts = &counts.Updated
	} else {
		subCounts = &counts.Stale
	}

	if k8s.IsPodReady(pod) {
		subCounts.Ready++
		return
	}

	switch k8s.GetPodStatus(pod) {
	case k8s.PodStatusPending:
		if time.Since(pod.CreationTimestamp.Time) > 10*time.Minute {
			subCounts.Stalled++
		} else {
			subCounts.Pending++
		}
	case k8s.PodStatusInitializing:
		subCounts.Initializing++
	case k8s.PodStatusRunning:
		subCounts.Initializing++
	case k8s.PodStatusTerminating:
		subCounts.Terminating++
	case k8s.PodStatusFailed:
		subCounts.Failed++
	case k8s.PodStatusKilled:
		subCounts.Killed++
	case k8s.PodStatusKilledOOM:
		subCounts.KilledOOM++
	default:
		subCounts.Unknown++
	}
}

func getStatusCode(counts *status.ReplicaCounts, minReplicas int32) status.Code {
	if counts.Updated.Ready >= counts.Requested {
		return status.Live
	}

	if counts.Updated.Failed > 0 || counts.Updated.Killed > 0 {
		return status.Error
	}

	if counts.Updated.KilledOOM > 0 {
		return status.OOM
	}

	if counts.Updated.Stalled > 0 {
		return status.Stalled
	}

	if counts.Updated.Ready >= minReplicas {
		return status.Live
	}

	if counts.Updated.Pending+counts.Updated.Initializing+counts.Updated.Ready+counts.Updated.Stalled+
		counts.Stale.Pending+counts.Stale.Initializing+counts.Stale.Ready+counts.Stale.Stalled == 0 {
		return status.Stopping
	}

	return status.Updating
}
