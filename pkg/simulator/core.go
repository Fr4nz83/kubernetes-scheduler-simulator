package simulator

import (
	"fmt"
	"math/rand"
	"os"

	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	batchv1beta1 "k8s.io/api/batch/v1beta1"
	corev1 "k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	storagev1 "k8s.io/api/storage/v1"

	"github.com/hkust-adsl/kubernetes-scheduler-simulator/pkg/api/v1alpha1"
	"github.com/hkust-adsl/kubernetes-scheduler-simulator/pkg/type"
	"github.com/hkust-adsl/kubernetes-scheduler-simulator/pkg/utils"
)

type ResourceTypes struct {
	Nodes                  []*corev1.Node
	Pods                   []*corev1.Pod
	DaemonSets             []*appsv1.DaemonSet
	StatefulSets           []*appsv1.StatefulSet
	Deployments            []*appsv1.Deployment
	ReplicationControllers []*corev1.ReplicationController
	ReplicaSets            []*appsv1.ReplicaSet
	Services               []*corev1.Service
	PersistentVolumeClaims []*corev1.PersistentVolumeClaim
	StorageClasss          []*storagev1.StorageClass
	PodDisruptionBudgets   []*policyv1beta1.PodDisruptionBudget
	Jobs                   []*batchv1.Job
	CronJobs               []*batchv1beta1.CronJob
}

type AppResource struct {
	Name     string
	Resource ResourceTypes
}

// The Interface type below specifies the function that a simulator must implement.
type Interface interface {
	RunCluster(cluster ResourceTypes) ([]simontype.UnscheduledPod, error)
	ScheduleApp(AppResource) ([]simontype.UnscheduledPod, error)
	SchedulePods(pods []*corev1.Pod) []simontype.UnscheduledPod

	ClusterAnalysis(tag string) (utils.FragAmount, []utils.ResourceSummary)
	ClusterGpuFragReport()
	GetClusterNodeStatus() []simontype.NodeStatus

	SetWorkloadPods(pods []*corev1.Pod)
	SetTypicalPods()
	SetSkylinePods()

	RecordPodTotalResourceReq(pods []*corev1.Pod) (int64, int64)
	RecordNodeTotalResource(nodes []*corev1.Node) (int64, int64)

	TunePodsByNodeTotalResource(pods []*corev1.Pod, config v1alpha1.WorkloadTuningConfig) []*corev1.Pod

	ExportPodSnapshotInYaml(unschedulePods []simontype.UnscheduledPod, filePath string)
	ExportNodeSnapshotInCSV(filePath string)
	ExportPodSnapshotInCSV(filePath string)

	SortClusterPods(pods []*corev1.Pod)

	RunWorkloadInflationEvaluation(tag string)

	GetCustomConfig() v1alpha1.CustomConfig

	DescheduleCluster() []simontype.UnscheduledPod

	Close()
}

// Simulate is used for simulating and deploying apps in a cluster. It takes in the cluster and apps generated by the user as parameters, and deploys the apps in order.
// Return Values
// 1. If error is not empty, the function execution failed.
// 2. If error is empty, the function executes successfully and the SimulateResult information can be used to get the cluster simulation information.
// The SimulateResult information includes:
// 1. UnscheduledPods - represents unscheduled Pods. If the value is empty, it means that the simulation scheduling was successful.
// 2. NodeStatus - will record the Pod situation on each Node in detail.
func Simulate(cluster ResourceTypes, apps []AppResource, opts ...Option) (*simontype.SimulateResult, error) {
	
	// init simulator
	fmt.Printf("DEBUG FRA, simulate.Simulate(): initialize the simulator's config options.\n")
	sim, err := New(opts...)
	if err != nil {
		return nil, err
	}
	
	
	// This line defers the execution of the Close method of the sim object until the surrounding function returns.
	defer sim.Close()


	// In Kubernetes, a DaemonSet ensures that a copy of a specific pod is running on all or a subset of nodes in a cluster. 
	// The DaemonSet controller automatically manages the lifecycle of these pods, ensuring that they are created as nodes 
	// are added to the cluster and terminated when nodes are removed.
	// When excluding DaemonSet pods from certain operations or queries, it's usually because these pods serve a different
	// purpose or have different management requirements compared to regular application pods. 
	cluster.Pods, err = GetValidPodExcludeDaemonSet(cluster)
	if err != nil {
		return nil, err
	}



	log.Infof("Number of original workload pods: %d", len(cluster.Pods))
	sim.SetWorkloadPods(cluster.Pods) // The method comes from ./pkg/simulator/simulator.go
	sim.SetTypicalPods() // The method comes from ./pkg/simulator/analysis.go
	sim.SetSkylinePods() // The method comes from ./pkg/simulator/analysis.go
	sim.ClusterGpuFragReport() // The method comes from ./pkg/simulator/analysis.go. Reports the Gpu Frag Amount of all nodes


	fmt.Printf("DEBUG FRA, simulate.Simulate(): workload tuning.\n")
	customConfig := sim.GetCustomConfig()
	rand.Seed(customConfig.WorkloadTuningConfig.Seed)
	log.Debugf("Random Seed: %d, Random Int: %d", customConfig.WorkloadTuningConfig.Seed, rand.Int())
	for _, item := range cluster.DaemonSets {
		validPods, err := utils.MakeValidPodsByDaemonset(item, cluster.Nodes)
		if err != nil {
			return nil, err
		}
		cluster.Pods = append(cluster.Pods, validPods...)
	}



	// run cluster
	sim.SortClusterPods(cluster.Pods)
	sim.RecordPodTotalResourceReq(cluster.Pods)
	sim.RecordNodeTotalResource(cluster.Nodes)

	if customConfig.WorkloadTuningConfig.Ratio > 0 {
		// <= 0 means no tuning, keeping the cluster.Pods == sim.workloadPods
		cluster.Pods = sim.TunePodsByNodeTotalResource(cluster.Pods, customConfig.WorkloadTuningConfig)
	}


	// IMPORTANT: RunCluster(), which comes from ./pkg/simulator/simulator.go, seems to be the most important method of the simulator, the one actually running the simulation.
	fmt.Printf("DEBUG FRA, simulate.Simulate(): executing RunCluster().\n")
	var failedPods []simontype.UnscheduledPod
	unscheduledPods, err := sim.RunCluster(cluster) // Existing pods in the cluster are scheduled here.
	if err != nil {
		return nil, err
	}
	failedPods = append(failedPods, unscheduledPods...)
	utils.ReportFailedPods(failedPods)
	sim.ClusterAnalysis(TagInitSchedule)



	// export a cluster snapshot after scheduling
	if customConfig.ExportConfig.PodSnapshotYamlFilePrefix != "" {
		fmt.Printf("DEBUG FRA, simulate.Simulate(): exporting cluster snapshot to a YAML file after scheduling.\n")
		// filePath: prefix/InitSchedule/pod-snapshot.yaml
		prefix := customConfig.ExportConfig.PodSnapshotYamlFilePrefix
		fileDir := fmt.Sprintf("%s/%s", prefix, TagInitSchedule)
		if e := os.MkdirAll(fileDir, os.FileMode(0777)); e != nil {
			log.Errorf("MkdirAll(%s, 0777) failed: %s", fileDir, e.Error())
		} else {
			filePath := fmt.Sprintf("%s/%s", fileDir, "pod-snapshot.yaml")
			sim.ExportPodSnapshotInYaml(unscheduledPods, filePath)
		}
	}
	if customConfig.ExportConfig.NodeSnapshotCSVFilePrefix != "" {
		fmt.Printf("DEBUG FRA, simulate.Simulate(): exporting cluster snapshot to a CSV file after scheduling.\n")
		// filePath: prefix/InitSchedule/node-snapshot.csv
		prefix := customConfig.ExportConfig.NodeSnapshotCSVFilePrefix
		fileDir := fmt.Sprintf("%s/%s", prefix, TagInitSchedule)
		if e := os.MkdirAll(fileDir, os.FileMode(0777)); e != nil {
			log.Errorf("MkdirAll(%s, 0777) failed: %s", fileDir, e.Error())
		} else {
			filePath := fmt.Sprintf("%s/%s", fileDir, "node-snapshot.csv")
			sim.ExportNodeSnapshotInCSV(filePath)
			podFilePath := fmt.Sprintf("%s/%s", fileDir, "pod-snapshot.csv")
			sim.ExportPodSnapshotInCSV(podFilePath)
		}
	}


	// NOTE: this does not seem to be executed in the example.
	if customConfig.WorkloadInflationConfig.Ratio > 1 {
		fmt.Printf("DEBUG FRA, simulate.Simulate(): executing RunWorkloadInflationEvaluation().\n")
		sim.RunWorkloadInflationEvaluation(TagScheduleInflation)
	}



	if customConfig.NewWorkloadConfig != "" {
		fmt.Printf("DEBUG FRA, simulate.Simulate(): customConfig.NewWorkloadConfig not empty.\n")
		resources, err := CreateClusterResourceFromClusterConfig(customConfig.NewWorkloadConfig)
		if err != nil {
			return nil, err
		}
		newWorkloadPods, err := GetValidPodExcludeDaemonSet(resources)
		if err != nil {
			return nil, err
		}
		log.Infof("Number of new workload pods: %d\n", len(newWorkloadPods))
		sim.SetWorkloadPods(newWorkloadPods)
		sim.SetTypicalPods()
		sim.ClusterGpuFragReport()
	}



	// evict some pods in the cluster and reschedule them
	if customConfig.DescheduleConfig.Policy != "" {
		fmt.Printf("DEBUG FRA, simulate.Simulate(): descheduling pods.\n")
		unscheduledPods = sim.DescheduleCluster()
		failedPods = append(failedPods, unscheduledPods...)
		sim.ClusterAnalysis(TagPostDeschedule)
		sim.ClusterGpuFragReport()

		if customConfig.ExportConfig.PodSnapshotYamlFilePrefix != "" {
			fmt.Printf("DEBUG FRA, simulate.Simulate(): exporting cluster snapshot to a YAML file after evict and reschedule.\n")
			// filePath: prefix/PostDeschedule/pod-snapshot.yaml
			prefix := customConfig.ExportConfig.PodSnapshotYamlFilePrefix
			fileDir := fmt.Sprintf("%s/%s", prefix, TagPostDeschedule)
			if e := os.MkdirAll(fileDir, os.FileMode(0777)); e != nil {
				log.Errorf("MkdirAll(%s, 0777) failed: %s", fileDir, e.Error())
			} else {
				filePath := fmt.Sprintf("%s/%s", fileDir, "pod-snapshot.yaml")
				sim.ExportPodSnapshotInYaml(unscheduledPods, filePath)
			}
		}
		if customConfig.ExportConfig.NodeSnapshotCSVFilePrefix != "" {
			fmt.Printf("DEBUG FRA, simulate.Simulate(): exporting cluster snapshot to a CSV file after evict and reschedule.\n")
			// filePath: prefix/PostDeschedule/node-snapshot.csv
			prefix := customConfig.ExportConfig.NodeSnapshotCSVFilePrefix
			fileDir := fmt.Sprintf("%s/%s", prefix, TagPostDeschedule)
			if e := os.MkdirAll(fileDir, os.FileMode(0777)); e != nil {
				log.Errorf("MkdirAll(%s, 0777) failed: %s", fileDir, e.Error())
			} else {
				filePath := fmt.Sprintf("%s/%s", fileDir, "node-snapshot.csv")
				sim.ExportNodeSnapshotInCSV(filePath)
				podFilePath := fmt.Sprintf("%s/%s", fileDir, "pod-snapshot.csv")
				sim.ExportNodeSnapshotInCSV(podFilePath)
			}
		}
	}
	if customConfig.NewWorkloadConfig != "" || customConfig.DescheduleConfig.Policy != "" {
		if customConfig.WorkloadInflationConfig.Ratio > 1 {
			sim.RunWorkloadInflationEvaluation(TagDescheduleInflation)
		}
	}



	// schedule (unscheduled? to be checked) pods
	for _, app := range apps {
		unscheduledPods, err = sim.ScheduleApp(app)
		if err != nil {
			return nil, err
		}
		failedPods = append(failedPods, unscheduledPods...)
	}


	// Return the final results.
	fmt.Printf("DEBUG FRA, simulate.Simulate(): returning the final results.\n")
	return &simontype.SimulateResult{
		UnscheduledPods: failedPods,
		NodeStatus:      sim.GetClusterNodeStatus(),
	}, nil
}
