// Copyright 2018 The Kubeflow Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package trainer is to manage TensorFlow training jobs.
package trainer

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
	"k8s.io/api/policy/v1beta1"
	k8s_errors "k8s.io/apimachinery/pkg/api/errors"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"

	"github.com/kubeflow/tf-operator/pkg/apis/tensorflow/helper"
	tfv1alpha1 "github.com/kubeflow/tf-operator/pkg/apis/tensorflow/v1alpha1"
	"github.com/kubeflow/tf-operator/pkg/apis/tensorflow/validation"
	tfjobclient "github.com/kubeflow/tf-operator/pkg/client/clientset/versioned"
	"github.com/kubeflow/tf-operator/pkg/client/clientset/versioned/scheme"
	"github.com/kubeflow/tf-operator/pkg/util"
)

// TODO(jlewi): We should switch a New pattern and make trainingJob private so we can
// TrainingJob represents a training job specification.
// ensure correctness on creation.
type TrainingJob struct {
	job *tfv1alpha1.TFJob

	KubeCli kubernetes.Interface

	recorder record.EventRecorder

	Replicas []*TFReplicaSet

	tfJobClient tfjobclient.Interface

	// in memory state of the job.
	// status is the source of truth after job struct is materialized. Changes to the status to be persisted
	// should be made here.
	status tfv1alpha1.TFJobStatus

	memberCounter int

	pdb *v1beta1.PodDisruptionBudget

	// contextLogger is a logger to use for logging information about this replica.
	contextLogger *log.Entry
}

// ClusterSpec represents a cluster TensorFlow specification.
// https://www.tensorflow.org/deploy/distributed#create_a_tftrainclusterspec_to_describe_the_cluster
// It is a map from job names to network addresses.
type ClusterSpec map[string][]string

// TaskSpec represents a Task specification.
type TaskSpec struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

//initJob initiate a training job and returns the job specifications.
func initJob(kubeCli kubernetes.Interface, tfJobClient tfjobclient.Interface,
	recorder record.EventRecorder, job *tfv1alpha1.TFJob) (*TrainingJob, error) {
	j := &TrainingJob{
		KubeCli:     kubeCli,
		tfJobClient: tfJobClient,
		recorder:    recorder,
		Replicas:    make([]*TFReplicaSet, 0),
		job:         job,
		status:      *job.Status.DeepCopy(),

		contextLogger: log.WithFields(log.Fields{
			// We use job to match the key used in controller.go
			// In controller.go we log the key used with the workqueue.
			"job": job.ObjectMeta.Namespace + "/" + job.ObjectMeta.Name,
			"uid": job.ObjectMeta.UID,
		}),
	}

	return j, nil
}

// NewJob method invokes the initJob and check for error
func NewJob(kubeCli kubernetes.Interface, tfJobClient tfjobclient.Interface, recorder record.EventRecorder, job *tfv1alpha1.TFJob, config *tfv1alpha1.ControllerConfig) (*TrainingJob, error) {
	j, err := initJob(kubeCli, tfJobClient, recorder, job)
	if err != nil {
		return nil, err
	}

	return j, nil
}

// Update replaces the TFJob corresponding to TrainingJob with the provided job.
// This function is used when the Spec/Status of the job is modified outside the controller.
// For example, if the user issues a delete request. This will update the metadata on the object
// so we need to replace the spec.
func (j *TrainingJob) Update(newJob *tfv1alpha1.TFJob) {
	//j.contextLogger.Infof("Updating job to %+v\n", *newJob)
	j.job = newJob
}

// UID returns the user ID of the requesting user
func (j *TrainingJob) UID() types.UID {
	return j.job.ObjectMeta.UID
}

// ClusterSpec returns the cluster specification for the training job provided
func (j *TrainingJob) ClusterSpec() ClusterSpec {
	clusterSpec := make(ClusterSpec)

	for _, p := range j.Replicas {
		replicaNames := make([]string, 0, *p.Spec.Replicas)

		for i := int32(0); i < *p.Spec.Replicas; i++ {
			replicaNames = append(replicaNames, fmt.Sprintf("%v:%v", p.genName(i), *p.Spec.TFPort))
		}

		clusterSpec[strings.ToLower(string(p.Spec.TFReplicaType))] = replicaNames
	}

	return clusterSpec
}

// cleanResourcesByCleanPolicy deletes the replicas by following the policy CleanupAll, CleanupNone, CleanupRunning, the default is CleanupAll
func (j *TrainingJob) deleteResourcesByCleanPolicy() error {
	log.Infof("deleteResourcesByCleanPolicy for %s, %v", j.job.ObjectMeta.Name, j.Replicas)
	for _, r := range j.Replicas {
		log.Infof("deleteResourcesByCleanPolicy for %s, %v", j.job.ObjectMeta.Name, r)
		if err := r.DeleteResourcesByCleanPolicy(j.CleanUpPolicy()); err != nil {
			return err
		}
	}

	return nil
}

// deleteResources deletes the replicas it it was created
func (j *TrainingJob) deleteResources() error {
	for _, r := range j.Replicas {
		if err := r.Delete(); err != nil {
			return err
		}
	}

	return nil
}

// GetStatus returns the status of training job provided
// 返回TFJobStatus作为整体的status(use chief), 和各个TFReplicaStatus
func (j *TrainingJob) GetStatus() (tfv1alpha1.TFJobStatus, []*tfv1alpha1.TFReplicaStatus, error) {
	chief := j.job.Spec.TerminationPolicy.Chief
	chiefState := tfv1alpha1.ReplicaStateUnknown
	chiefReason := ""
	chiefMessage := ""

	state := tfv1alpha1.StateUnknown
	replicaStatuses := make([]*tfv1alpha1.TFReplicaStatus, 0)

	// The state for each replica.
	// TODO(jlewi): We will need to modify this code if we want to allow multiples of a given type of replica.
	replicaSetStates := make(map[tfv1alpha1.TFReplicaType]tfv1alpha1.ReplicaState)

	for _, r := range j.Replicas {
		rStatus, err := r.GetStatus()
		if err != nil {
			log.Errorf("GetStatus() for %v returned error; %v", r.Spec.TFReplicaType, err)
		}

		replicaSetStates[r.Spec.TFReplicaType] = rStatus.State

		replicaStatuses = append(replicaStatuses, &rStatus)

		if string(r.Spec.TFReplicaType) == chief.ReplicaName {
			chiefState, chiefReason, chiefMessage = rStatus.State, rStatus.Reason, rStatus.Message
		}
	}

	if chiefState == tfv1alpha1.ReplicaStateRunning {
		state = tfv1alpha1.StateRunning
	} else if chiefState == tfv1alpha1.ReplicaStateFailed {
		state = tfv1alpha1.StateFailed
	} else if chiefState == tfv1alpha1.ReplicaStateSucceeded {
		state = tfv1alpha1.StateSucceeded
		chiefReason = ""
		chiefMessage = ""
	}

	return tfv1alpha1.TFJobStatus{
		State:   state,
		Reason:  chiefReason,
		Message: chiefMessage,
	}, replicaStatuses, nil
}

// isRetryableTerminationState returns true if a container terminated in a state
// that we consider retryable.
func isRetryableTerminationState(s *v1.ContainerStateTerminated) bool {
	// TODO(jlewi): Need to match logic in
	// https://cs.corp.google.com/piper///depot/google3/cloud/ml/beta/job/training_job_state_util.cc?l=88
	if s.Reason == "OOMKilled" {
		// If the user's process causes an OOM and Docker kills the container,
		// the termination reason of ContainerState will be specified to
		// 'OOMKilled'. In this case, we can't assume this to be a retryable error.
		//
		// This check should happen before checking the termination log, since
		// if the container terminated with an OOM, the termination log may not
		// be written.
		return false
	}

	// TODO(jlewi): Should we use the exit code reported in the termination
	// log message and not the ExitCode reported by the container.

	if s.ExitCode >= 0 && s.ExitCode <= 127 {
		// For the exit_code in [0, 127]:
		//   0 means success,
		//   1 - 127 corresponds to permanent user errors.
		// We don't want to retry for both cases.
		// More info about exit status can be found in:
		// https://www.gnu.org/software/bash/manual/html_node/Exit-Status.html
		return false
	}

	// For the remaining cases that exit_code from workers that doesn't
	// fall into [0, 127]. They can be:
	//   137 corresponds to SIGKILL,
	//   143 corresponds to SIGTERM,
	//   other values that have undefined behavior.
	// We treat them as internal errors for now and all the internal errors
	// will be retired.
	return true
}

// masterName returns the name of master replica of provided training job
func (j *TrainingJob) masterName() string {
	return fmt.Sprintf("master-%v-0", j.job.Spec.RuntimeId)
}

// setup the training job.
func (j *TrainingJob) setup(config *tfv1alpha1.ControllerConfig) {
	err := func() error {
		// If the job has already started we shouldn't set it up again.
		if j.status.Phase != tfv1alpha1.TFJobPhaseNone {
			log.Warningf("Job %v has already been setup.", j.name())
			return nil
		}

		// Set defaults.
		scheme.Scheme.Default(j.job)

		err := validation.ValidateTFJobSpec(&j.job.Spec)
		if err != nil {
			return fmt.Errorf("invalid job spec: %v", err)
		}

		if err := helper.ConfigureAcceleratorsForTFJobSpec(&j.job.Spec, config.Accelerators); err != nil {
			return fmt.Errorf("ConfigureAccelerators(...) error; %v", err)
		}

		if j.job.Spec.RuntimeId == "" {
			j.job.Spec.RuntimeId = util.RandString(4)
		}
		return nil
	}()

	if err != nil {
		j.status.Reason = "setupFail"
		j.status.Message = err.Error()
		j.status.Phase = tfv1alpha1.TFJobPhaseFailed
		j.status.State = tfv1alpha1.StateFailed
	} else {
		j.status.Reason = ""
		j.status.Message = ""
		j.status.Phase = tfv1alpha1.TFJobPhaseCreating
		j.status.State = tfv1alpha1.StateRunning
	}
}

// // setupReplicas creates in memory data structures corresponding to the replicas.
func (j *TrainingJob) setupReplicas() error {
	if len(j.Replicas) != len(j.job.Spec.ReplicaSpecs) {
		j.Replicas = make([]*TFReplicaSet, 0, len(j.job.Spec.ReplicaSpecs))
		for _, t := range j.job.Spec.ReplicaSpecs {
			r, err := NewTFReplicaSet(j.KubeCli, j.recorder, *t, j)
			if err != nil {
				return err
			}
			j.Replicas = append(j.Replicas, r)
		}
	}

	return nil
}

// Delete methods deletes the pods and frees the allocated resources
func (j *TrainingJob) Delete() {
	// TODO(jlewi): Delete is what should cause us to delete the Pods.
	// we shouldn't delete the pods when the jobs finish because leaving the pods
	// allows us to get the logs from the pods after the job finishes.
	//
	j.contextLogger.Infof("TFJob %v deleted by the user", j.fullname())
	// TODO(jlewi): This logic is probably insufficient.
	if j.job.Status.Phase != tfv1alpha1.TFJobPhaseCleanUp {
		j.status.Phase = tfv1alpha1.TFJobPhaseCleanUp
	}

	// TODO(jlewi): Does it make sense to explicitly delete the resources? Should
	// we just rely on K8s garbage collection to delete the resources before
	// deleting TFJob?
	if cErr := j.deleteResources(); cErr != nil {
		j.contextLogger.Errorf("trainingJob.deleteResources() error; %v", cErr)
	}

	if j.pdb != nil {
		// if the job has PDB for gang scheduling, delete it
		err := j.KubeCli.PolicyV1beta1().PodDisruptionBudgets(j.job.ObjectMeta.Namespace).Delete(j.pdb.ObjectMeta.Name, &meta_v1.DeleteOptions{})
		if err != nil {
			j.contextLogger.Errorf("Error deleting PDB %v; %v", j.pdb.ObjectMeta.Name, err)
		}
	}
}

// updateCRDStatus updates the job status based on TraingingJob.status.
func (j *TrainingJob) updateCRDStatus() error {
	// If the status hasn't changed then there's no reason to update the CRD.
	j.contextLogger.Infof("updating crd status: %s : %s", util.Pformat(j.job.Status), util.Pformat(j.status))
	if reflect.DeepEqual(j.job.Status, j.status) {
		return nil
	}

	if j.status.State == tfv1alpha1.StateRunning {
		if j.job.Status.StartTime == nil {
			now := metav1.Now()
			j.job.Status.StartTime = &now
		}
	}

	newJob := j.job
	if j.status.State == tfv1alpha1.StateSucceeded || j.status.State == tfv1alpha1.StateFailed {
		if j.job.Annotations == nil {
			j.job.Annotations = map[string]string{}
		}
		if j.job.Annotations["JOBEND"] == "" {
			j.job.Annotations["JOBEND"] = time.Now().Format("2006-01-02 15:04:05 -0700 MST")
		}
		if j.job.Status.CompletionTime == nil {
			now := metav1.Now()
			j.job.Status.CompletionTime = &now
		}
	}

	newJob.Status = j.status
	newJob, err := j.tfJobClient.KubeflowV1alpha1().TFJobs(j.job.ObjectMeta.Namespace).Update(newJob)
	if err != nil {
		return err
	}

	j.job = newJob

	return nil
}

type CleanUpPolicy string

const (
	CleanupPodUndefined CleanUpPolicy = ""
	CleanupPodAll       CleanUpPolicy = "All"
	CleanupPodRunning   CleanUpPolicy = "Running"
	CleanupPodNone      CleanUpPolicy = "None"
)

var (
	DefaultCleanUpPolicy = DefaultEnv("CleanUpPolicy", "Running")
)

func DefaultEnv(key, defaultval string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultval
}

// CleanUpPolicy ...
func (j *TrainingJob) CleanUpPolicy() CleanUpPolicy {
	policy := CleanUpPolicy(j.job.Annotations["CleanUpPolicy"])
	if policy == "" {
		return CleanUpPolicy(DefaultCleanUpPolicy)
	}
	return policy
}

// Reconcile tries to get the job into the desired state.
// state, reason -> out, phase -> inner
func (j *TrainingJob) Reconcile(config *tfv1alpha1.ControllerConfig, enableGangScheduling bool) error {
	// TODO(jlewi): This doesn't seem to be a reliable way to detect deletion.
	if j.job.ObjectMeta.DeletionTimestamp != nil {
		j.contextLogger.Info("Deletion timestamp set; skipping reconcile")
		// Job is in the process of being deleted so do nothing.
		// We especially don't want to create new resources as that could block deletion.
		return nil
	}

	if j.job.Status.Phase == tfv1alpha1.TFJobPhaseNone {
		// The job hasn't been setup.
		j.setup(config)

		if err := j.updateCRDStatus(); err != nil {
			j.contextLogger.Warningf("failed to update CRD status: %v", err)
			return err
		}
	}

	// setupreplicas initializes data structures inside TrainingJob representing the replicas.
	// These are go-lang structures which aren't preserved in the APIServer. So we always need to call setupReplicas
	// unlike setup which only needs to be called once during the lifecycle of the job.
	if err := j.setupReplicas(); err != nil {
		j.contextLogger.Errorf("failed to create replicas: %v", err)
		j.status.Phase = tfv1alpha1.TFJobPhaseFailed
		j.status.State = tfv1alpha1.StateFailed
		j.status.Reason = fmt.Sprintf("Could not create in memory datastructures; %v", err)
		if uErr := j.updateCRDStatus(); uErr != nil {
			j.contextLogger.Warningf("Job %v; failed to update status error: %v", j.job.ObjectMeta.Name, uErr)
		}
		return err
	}

	// sync PDB for gang scheduling
	// TODO(mitake): replace PDB with a newer mechanism if it is replaced
	if enableGangScheduling {
		err := j.syncPdb()
		if err != nil {
			j.contextLogger.Errorf("SyncPdb error: %v", err)
		}
	}

	status, replicaStatuses, err := j.GetStatus()
	if err != nil {
		j.contextLogger.Errorf("GetStatus() for job %v returned error: %v", j.job.ObjectMeta.Name, err)
		return err
	}
	j.status.ReplicaStatuses = replicaStatuses

	// Only sync pods and services if we are running.
	if j.status.Phase == tfv1alpha1.TFJobPhaseCreating && status.State == tfv1alpha1.StateUnknown {
		// sync pods
		success := true
		for _, rc := range j.Replicas {
			err := rc.SyncPods()
			if err != nil {
				j.status.Reason = "SyncPodFail"
				j.status.Message = err.Error()
				j.contextLogger.Errorf("SyncPods error: %v", err)
				success = false
				break
			}
		}

		// sync services
		for _, rc := range j.Replicas {
			err := rc.SyncServices()
			if err != nil {
				j.status.Reason = "SyncServiceFail"
				j.status.Message = err.Error()
				j.contextLogger.Errorf("SyncServices error: %v", err)
				success = false
				break
			}
		}
		if success {
			j.status.Phase = tfv1alpha1.TFJobPhaseRunning
			j.status.State = tfv1alpha1.StateRunning
			j.status.Reason = ""
			j.status.Message = ""
		} else {
			j.status.State = tfv1alpha1.StateFailed
			j.status.Phase = tfv1alpha1.TFJobPhaseCleanUp
		}

		if err := j.updateCRDStatus(); err != nil {
			j.contextLogger.Warningf("Job %v; failed to update status error: %v", j.job.ObjectMeta.Name, err)
			return err
		}
	}

	if j.status.Phase == tfv1alpha1.TFJobPhaseRunning {

		// TODO(jlewi): We should update the Phase if we detect the job is done.
		if status.State == tfv1alpha1.StateFailed {
			j.contextLogger.Errorf("Master failed Job: %v.", j.job.ObjectMeta.Name)
			j.status.Phase = tfv1alpha1.TFJobPhaseCleanUp
			j.status.State = tfv1alpha1.StateFailed
			j.status.Reason = status.Reason
			j.status.Message = status.Message
		} else if status.State == tfv1alpha1.StateSucceeded {
			j.contextLogger.Infof("Master succeeded Job: %v.", j.job.ObjectMeta.Name)
			j.status.Phase = tfv1alpha1.TFJobPhaseCleanUp
			j.status.State = tfv1alpha1.StateSucceeded
			j.status.Reason = ""
			j.status.Message = ""
		} else if status.State == tfv1alpha1.StateRunning {
			j.contextLogger.Infof("Master running Job: %v.", j.job.ObjectMeta.Name)
			j.status.Phase = tfv1alpha1.TFJobPhaseRunning
			j.status.State = tfv1alpha1.StateRunning
			j.status.Reason = status.Reason
			j.status.Message = status.Message
		} else {
			j.contextLogger.Infof("Job %v status=%v", j.job.ObjectMeta.Name, util.Pformat(j.status))
		}

		// If the phase changed we should update the CRD.
		if err := j.updateCRDStatus(); err != nil {
			j.contextLogger.Warningf("Job %v, failed to update CRD status error: %v", j.job.ObjectMeta.Name, err)
			return err
		}
	}

	if j.job.Status.Phase == tfv1alpha1.TFJobPhaseCleanUp {
		if cErr := j.deleteResourcesByCleanPolicy(); cErr != nil {
			j.contextLogger.Errorf("Job %v trainingJob.Delete() error; %v", j.job.ObjectMeta.Name, cErr)
			// Return an error so that we stay in phase cleanup and retry.
			return cErr
		}
		j.status.Phase = tfv1alpha1.TFJobPhaseDone
		for _, r := range replicaStatuses {
			if r.State == tfv1alpha1.ReplicaStateRunning || r.State == tfv1alpha1.ReplicaStateUnknown {
				r.State = tfv1alpha1.ReplicaStateCleaned
			}
			if n, ok := r.ReplicasStates[tfv1alpha1.ReplicaStateRunning]; ok {
				delete(r.ReplicasStates, tfv1alpha1.ReplicaStateRunning)
				r.ReplicasStates[tfv1alpha1.ReplicaStateCleaned] = n
			}
			if m, ok := r.ReplicasStates[tfv1alpha1.ReplicaStatePending]; ok {
				delete(r.ReplicasStates, tfv1alpha1.ReplicaStatePending)
				r.ReplicasStates[tfv1alpha1.ReplicaStateCleaned] += m
			}
		}
	}

	// updateCRDStatus will update the status of the CRD with c.Status if c.Status
	// doesn't match c.Cluster.status. So you can change c.Status in order to propagate
	// changes to the CRD status.
	if err := j.updateCRDStatus(); err != nil {
		j.contextLogger.Warningf("Job %v; failed to update CRD status error: %v", j.job.ObjectMeta.Name, err)
		return err
	}

	return nil
}

// name returns the name of the given training job.
func (j *TrainingJob) name() string {
	return j.job.ObjectMeta.GetName()
}

// fullname returns the namespace and name for the job.
func (j *TrainingJob) fullname() string {
	return j.job.ObjectMeta.GetNamespace() + ":" + j.job.ObjectMeta.GetName()
}

// SchedulerName returns the scheduler name for the job.
func (j *TrainingJob) SchedulerName() string {
	return j.job.Spec.SchedulerName
}

// SyncPdb will create a PDB for gang scheduling by kube-arbitrator.
func (j *TrainingJob) syncPdb() error {
	nrReplicas := int32(0)
	for _, r := range j.Replicas {
		nrReplicas += *r.Spec.Replicas
	}

	if nrReplicas == 1 {
		// gang scheduling isn't required by a non distributed training process
		return nil
	}

	minAvailable := intstr.FromInt(int(nrReplicas))
	pdb := &v1beta1.PodDisruptionBudget{
		ObjectMeta: meta_v1.ObjectMeta{
			GenerateName: "tf-job-pdb-",
		},
		Spec: v1beta1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector: &meta_v1.LabelSelector{
				MatchLabels: map[string]string{
					"runtime_id":  j.job.Spec.RuntimeId,
					"tf_job_name": j.job.ObjectMeta.Name,
				},
			},
		},
	}

	createdPdb, err := j.KubeCli.PolicyV1beta1().PodDisruptionBudgets(j.job.ObjectMeta.Namespace).Create(pdb)
	if err != nil {
		if k8s_errors.IsAlreadyExists(err) {
			j.contextLogger.Infof("PDB: %v already exists.", j.job.ObjectMeta.Name)
			return nil
		}

		j.recorder.Eventf(j.job, v1.EventTypeWarning, FailedCreateReason, "Error creating: %v", err)
		return err
	}

	j.pdb = createdPdb

	j.recorder.Eventf(j.job, v1.EventTypeNormal, SuccessfulCreateReason, "Created PDB: %v", createdPdb.Name)
	return nil
}
