package testlib

import (
	"context"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/stretchr/testify/require"
	"io"
	v1 "k8s.io/api/batch/v1"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

/** Used to wait for all async calls of GetAppLog() routine to finish before the test finishes */
var appLogCollectorsWg sync.WaitGroup

// GetAppLog
/**
 * GetAppLog retrieves the log output from a specified pod within a Kubernetes namespace and writes it to a file.
 *
 */
func GetAppLog(t *testing.T, namespace string, podName string, fileNameSuffix string, podLogOptions *corev1.PodLogOptions) string {
	defer appLogCollectorsWg.Done()
	appLogCollectorsWg.Add(1)
	dirPath := filepath.Join(RESULT_DIR, namespace)
	filePath := filepath.Join(dirPath, podName+fileNameSuffix+".log")

	_ = os.MkdirAll(dirPath, 0700)

	f, err := os.Create(filePath)
	require.NoError(t, err)
	defer func(f *os.File) {
		_ = f.Close()
	}(f)

	writer := io.Writer(f)

	reader, err := getAppLogStreamE(t, namespace, podName, podLogOptions)
	// avoid generating test failure just because container logs are not available
	if _, ok := err.(*ContainersNotStarted); ok {
		t.Logf("Skipping log collection for pod %s because no container has been started", podName)
		return ""
	}
	require.NoError(t, err)
	require.NotNil(t, reader)
	_, err = io.Copy(writer, reader)
	require.NoError(t, err)

	t.Logf("Finished reading log file %s", filePath)

	return filePath
}

func getAppLogStreamE(t *testing.T, namespace string, podName string, podLogOptions *corev1.PodLogOptions) (io.ReadCloser, error) {
	options := k8s.NewKubectlOptions("", "", namespace)

	client, err := k8s.GetKubernetesClientFromOptionsE(t, options)
	require.NoError(t, err)

	if podLogOptions.Container == "" {
		// Select first container if not specified; otherwise the GetLogs method will fail if there are sidecars
		pod, err := client.CoreV1().Pods(options.Namespace).Get(context.TODO(), podName, metav1.GetOptions{})
		require.NoError(t, err)

		container := findXmtpContainer(pod)
		if container == nil {
			return nil, &ContainersNotStarted{}
		}
		podLogOptions.Container = container.Name
		for _, containerStatus := range pod.Status.ContainerStatuses {
			// if the container is in Waiting state (e.g. because
			// the pod is in CrashLoopBackOff state), then get the
			// logs from the previous invocation of the container
			if containerStatus.Name == container.Name && containerStatus.State.Waiting != nil {
				podLogOptions.Previous = true
			}
		}
		if podLogOptions.Previous {
			t.Logf("Multiple containers found in pod %s. Getting logs from previous container %s.", podName, podLogOptions.Container)
		} else {
			t.Logf("Multiple containers found in pod %s. Getting logs from container %s.", podName, podLogOptions.Container)
		}
	}

	return client.CoreV1().Pods(options.Namespace).GetLogs(podName, podLogOptions).Stream(context.TODO())
}

func doesContainerHaveLogs(container *corev1.Container, containerStatuses []corev1.ContainerStatus) bool {
	for _, status := range containerStatuses {
		// check the status of the container; if it is in Waiting state,
		// then check that it has a non-0 restart count; otherwise the
		// container has no logs to retrieve
		if status.Name == container.Name && (status.State.Waiting == nil || status.RestartCount > 0) {
			return true
		}
	}
	return false
}

func findXmtpContainer(pod *corev1.Pod) *corev1.Container {
	// look for any container named "xmtp" that has logs
	for _, container := range pod.Spec.Containers {
		if (container.Name == "xmtpd") && doesContainerHaveLogs(&container, pod.Status.ContainerStatuses) {
			return &container
		}
	}
	// look for any container that has logs
	for _, container := range pod.Spec.Containers {
		if doesContainerHaveLogs(&container, pod.Status.ContainerStatuses) {
			return &container
		}
	}
	return nil
}

// Await
/**
 * Await repeatedly evaluates a provided lambda function until it returns true, or a specified timeout duration is reached.
 *
 * @param t *testing.T - The testing context.
 * @param lmbd func() bool - A lambda function that returns a boolean indicating a condition.
 * @param timeout time.Duration - The maximum duration to wait for the lambda function to return true.
 *
 * The function checks the lambda function's return value at regular intervals (1 second). If the lambda
 * function returns true within the timeout period, the function returns and the test continues. If the
 * lambda function does not return true within the timeout period, it logs the full stack trace and fails
 * the test with a timeout error.
 */
func Await(t *testing.T, lmbd func() bool, timeout time.Duration) {
	now := time.Now()
	for timeExpired := time.After(timeout); ; {
		select {
		case <-timeExpired:
			t.Logf("Function %s timed out", runtime.FuncForPC(reflect.ValueOf(lmbd).Pointer()).Name())
			t.Logf("Full stack trace of caller:\n%s", string(debug.Stack()))
			t.Fatalf("function call timed out after %f seconds. Start of await was '%s'", timeout.Seconds(), now)
		default:
			if lmbd() {
				return
			}

			time.Sleep(1 * time.Second)
		}
	}
}

// AwaitNrReplicasCreated
/**
 * AwaitNrReplicasCreated waits until the specified number of replicas of a pod are created in the given namespace.
 *
 * @param t *testing.T - The testing context.
 * @param namespace string - The namespace of the Kubernetes cluster.
 * @param expectedName string - The expected name substring of the pods to check for.
 * @param nrReplicas int - The number of replicas expected to be created.
 *
 * The function waits for a maximum of 1 minute, checking once per second, to find the expected number of replicas that
 * are created. If the expected number is found within the timeout, the function returns; otherwise, it logs the error.
 */
func AwaitNrReplicasCreated(t *testing.T, namespace string, expectedName string, nrReplicas int) {
	// the cluster might be downloading the docker images, so this might take a while the first time
	timeout := 1 * time.Minute

	Await(t, func() bool {
		var pods []corev1.Pod
		var podNames string
		for _, pod := range FindAllPodsInSchema(t, namespace) {
			if strings.Contains(pod.Name, expectedName) {
				pods = append(pods, pod)
			}
		}

		t.Logf("%d pods CREATED for name '%s': expected=%d, pods=[%s]\n", len(pods), expectedName, nrReplicas, podNames)

		return len(pods) == nrReplicas
	}, timeout)
}

// AwaitNrReplicasScheduled
/**
 * AwaitNrReplicasScheduled waits until the specified number of replicas of a pod are scheduled in the given namespace.
 *
 * @param t *testing.T - The testing context.
 * @param namespace string - The namespace of the Kubernetes cluster.
 * @param expectedName string - The expected name substring of the pods to check for.
 * @param nrReplicas int - The number of replicas expected to be scheduled.
 *
 * The function waits for a maximum of 1 minute, checking once per second, to find the expected number of replicas that
 * are scheduled. If the expected number is found within the timeout, the function returns; otherwise, it logs the error.
 */
func AwaitNrReplicasScheduled(t *testing.T, namespace string, expectedName string, nrReplicas int) {
	// the cluster might be downloading the docker images, so this might take a while the first time
	timeout := 1 * time.Minute

	Await(t, func() bool {
		var pods []corev1.Pod
		var podNames string
		for _, pod := range FindAllPodsInSchema(t, namespace) {
			if strings.Contains(pod.Name, expectedName) {
				//ignore all completed pods
				if pod.Status.Phase == corev1.PodSucceeded {
					continue
				}

				if arePodConditionsMet(&pod, corev1.PodScheduled, corev1.ConditionTrue) {
					// build array of scheduled pods
					pods = append(pods, pod)

					// build formatted list of pod names
					if podNames != "" {
						podNames += ", "
					}
					podNames += pod.Name

					// log any pods not in Pending or Running phase
					if pod.Status.Phase != corev1.PodPending && pod.Status.Phase != corev1.PodRunning {
						t.Logf("Unexpected phase for pod %s: %s", pod.Name, pod.Status.Phase)
					}
				}
			}
		}

		t.Logf("%d pods SCHEDULED for name '%s': expected=%d, pods=[%s]\n", len(pods), expectedName, nrReplicas, podNames)

		return len(pods) == nrReplicas
	}, timeout)
}

// AwaitNrReplicasReady
/**
 * AwaitNrReplicasReady waits until the specified number of replicas of a pod are ready in the given namespace.
 *
 * @param t *testing.T - The testing context.
 * @param namespace string - The namespace of the Kubernetes cluster.
 * @param expectedName string - The expected name substring of the pods to check for.
 * @param nrReplicas int - The number of replicas expected to be ready.
 *
 * The function waits for a maximum of 30 seconds, checking once per second, to find the expected number of replicas that
 * are ready. If the expected number is found within the timeout, the function returns; otherwise, it logs the error.
 */
func AwaitNrReplicasReady(t *testing.T, namespace string, expectedName string, nrReplicas int) {
	timeout := 30 * time.Second

	Await(t, func() bool {
		var cnt int
		for _, pod := range FindAllPodsInSchema(t, namespace) {
			if strings.Contains(pod.Name, expectedName) {
				if arePodConditionsMet(&pod, corev1.PodReady, corev1.ConditionTrue) {
					cnt++
				}
			}
		}

		t.Logf("%d pods READY for name '%s'\n", cnt, expectedName)

		return cnt == nrReplicas
	}, timeout)
}

func AwaitPodTerminated(t *testing.T, namespace string, expectedName string) {
	timeout := 30 * time.Second

	Await(t, func() bool {
		for _, pod := range FindAllPodsInSchema(t, namespace) {
			if strings.Contains(pod.Name, expectedName) {
				// Check if pod is in terminal phase
				if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
					t.Logf("Pod %s has terminated with phase: %s", pod.Name, pod.Status.Phase)
					return true
				}
			}
		}
		return false
	}, timeout)
}

// FindAllPodsInSchema
/**
 * FindAllPodsInSchema retrieves all pods in the specified namespace and sorts them by creation timestamp.
 *
 * @param t *testing.T - The testing context.
 * @param namespace string - The namespace of the Kubernetes cluster.
 *
 * @return []corev1.Pod - A sorted slice of all pods in the specified namespace.
 */
func FindAllPodsInSchema(t *testing.T, namespace string) []corev1.Pod {
	options := k8s.NewKubectlOptions("", "", namespace)
	filter := metav1.ListOptions{}
	pods := k8s.ListPods(t, options, filter)
	sort.SliceStable(pods, func(i, j int) bool {
		return pods[j].CreationTimestamp.Before(&pods[i].CreationTimestamp)
	})
	return pods
}

func arePodConditionsMet(pod *corev1.Pod, condition corev1.PodConditionType,
	status corev1.ConditionStatus) bool {
	for _, cnd := range pod.Status.Conditions {
		if cnd.Type == condition && cnd.Status == status {
			return true
		}
	}

	return false
}

// FindPodsFromChart
/**
 * FindPodsFromChart retrieves pods whose names contain the expected substring in the specified namespace.
 *
 * @param t *testing.T - The testing context.
 * @param namespace string - The namespace of the Kubernetes cluster.
 * @param expectedName string - The expected name substring of the pods to find.
 *
 * @return []corev1.Pod - A slice of pods whose names contain the expected substring.
 */
func FindPodsFromChart(t *testing.T, namespace string, expectedName string) []corev1.Pod {

	var pods []corev1.Pod

	for _, pod := range FindAllPodsInSchema(t, namespace) {
		if strings.Contains(pod.Name, expectedName) {
			pods = append(pods, pod)
		}
	}
	return pods
}

// FindAllCronJobsInSchema
/**
 * FindAllCronJobsInSchema retrieves all CronJobs in the specified namespace and sorts them by creation timestamp.
 *
 * @param t *testing.T - The testing context.
 * @param namespace string - The namespace of the Kubernetes cluster.
 *
 * @return []v1.CronJob - A sorted slice of all CronJobs in the specified namespace.
 */
func FindAllCronJobsInSchema(t *testing.T, namespace string) []v1.CronJob {
	options := k8s.NewKubectlOptions("", "", namespace)
	clientset, err := k8s.GetKubernetesClientFromOptionsE(t, options)
	require.NoError(t, err)

	cronJobsList, err := clientset.BatchV1().CronJobs(namespace).List(t.Context(), metav1.ListOptions{})
	require.NoError(t, err)

	cronJobs := cronJobsList.Items

	// Sort newest first (similar to your pod function)
	sort.SliceStable(cronJobs, func(i, j int) bool {
		return cronJobs[j].CreationTimestamp.Before(&cronJobs[i].CreationTimestamp)
	})

	return cronJobs
}

// FindCronJobsFromChart
/**
 * FindCronJobsFromChart retrieves CronJob whose names contain the expected substring in the specified namespace.
 *
 * @param t *testing.T - The testing context.
 * @param namespace string - The namespace of the Kubernetes cluster.
 * @param expectedName string - The expected name substring of the CronJobs to find.
 *
 * @return []v1.CronJob - A slice of CronJobs whose names contain the expected substring.
 */
func FindCronJobsFromChart(t *testing.T, namespace string, expectedName string) []v1.CronJob {

	var jobs []v1.CronJob

	for _, job := range FindAllCronJobsInSchema(t, namespace) {
		if strings.Contains(job.Name, expectedName) {
			jobs = append(jobs, job)
		}
	}
	return jobs
}

func CreateJobFromCronJob(t *testing.T, namespace string, cronJob *v1.CronJob, newJobName string) *v1.Job {
	options := k8s.NewKubectlOptions("", "", namespace)
	clientset, err := k8s.GetKubernetesClientFromOptionsE(t, options)
	if err != nil {
		t.Fatalf("Failed to get Kubernetes client: %v", err)
	}

	// Create a Job object from the CronJob's JobTemplateSpec
	job := &v1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      newJobName,
			Namespace: namespace,
			Labels:    cronJob.Spec.JobTemplate.Labels,
		},
		Spec: *cronJob.Spec.JobTemplate.Spec.DeepCopy(),
	}

	createdJob, err := clientset.BatchV1().Jobs(namespace).Create(t.Context(), job, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Logf("Successfully created Job %s from CronJob %s", newJobName, cronJob.Name)
	return createdJob
}

func GetTerminatedPodLog(t *testing.T, namespace string, pod *corev1.Pod, fileNameSuffix string, podLogOptions *corev1.PodLogOptions) string {
	// Determine if we need previous logs
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == podLogOptions.Container {
			if status.State.Terminated != nil {
				t.Logf("Pod %s container %s terminated, retrieving logs.", pod.Name, podLogOptions.Container)
				// Get logs of terminated container (without Previous)
				podLogOptions.Previous = false
			} else if status.RestartCount > 0 {
				t.Logf("Pod %s container %s restarted, retrieving previous logs.", pod.Name, podLogOptions.Container)
				podLogOptions.Previous = true
			} else {
				t.Fatalf("Pod %s container %s is not terminated, current state unknown", pod.Name, podLogOptions.Container)
			}
			break
		}
	}

	dirPath := filepath.Join(RESULT_DIR, namespace)
	filePath := filepath.Join(dirPath, pod.Name+fileNameSuffix+".log")
	_ = os.MkdirAll(dirPath, 0700)
	f, err := os.Create(filePath)
	require.NoError(t, err)
	defer func() {
		_ = f.Close()
	}()

	options := k8s.NewKubectlOptions("", "", namespace)
	client, err := k8s.GetKubernetesClientFromOptionsE(t, options)
	require.NoError(t, err)

	reader, err := client.CoreV1().Pods(namespace).GetLogs(pod.Name, podLogOptions).Stream(context.TODO())
	require.NoError(t, err)
	defer func() {
		_ = reader.Close()
	}()

	_, err = io.Copy(f, reader)
	require.NoError(t, err)

	t.Logf("Finished reading log file %s", filePath)
	return filePath
}
