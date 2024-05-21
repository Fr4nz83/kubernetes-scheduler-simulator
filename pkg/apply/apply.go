package apply

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"sort"
	"strconv"

	survey "github.com/AlecAivazis/survey/v2"
	"github.com/olekukonko/tablewriter"
	"github.com/pquerna/ffjson/ffjson"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	resourcehelper "k8s.io/kubectl/pkg/util/resource"
	"sigs.k8s.io/yaml"

	localcache "github.com/alibaba/open-local/pkg/scheduler/algorithm/cache"
	"github.com/hkust-adsl/kubernetes-scheduler-simulator/pkg/api/v1alpha1"
	"github.com/hkust-adsl/kubernetes-scheduler-simulator/pkg/chart"
	"github.com/hkust-adsl/kubernetes-scheduler-simulator/pkg/simulator"
	simontype "github.com/hkust-adsl/kubernetes-scheduler-simulator/pkg/type"
	gpushareutils "github.com/hkust-adsl/kubernetes-scheduler-simulator/pkg/type/open-gpu-share/utils"
	"github.com/hkust-adsl/kubernetes-scheduler-simulator/pkg/utils"
)

// ******************************************************************************
// PACKAGE PUBLIC RESOURCES, INTERFACES, STRUCTS, RECEIVER METHODS, and FUNCTIONS
// ******************************************************************************

type Options struct {
	SimonConfig                string
	DefaultSchedulerConfigFile string
	UseGreed                   bool
	Interactive                bool
	ExtendedResources          []string
}

type Applier struct {
	cluster           v1alpha1.Cluster
	appList           []v1alpha1.AppInfo
	schedulerConfig   string
	useGreed          bool
	interactive       bool
	extendedResources []string
	customConfig      v1alpha1.CustomConfig
}

type Interface interface {
	Run() error
}

// NewApplier returns a default applier that has passed the validity test
func NewApplier(opts Options) Interface {
	simonCR := &v1alpha1.Simon{}

	fmt.Printf("DEBUG FRA => executing function NewApplier!\n") // Content struct Options: %+v\n", opts)
	// fmt.Printf("DEBUG FRA => executing function NewApplier! Content struct Simon (1): %+v\n", simonCR)

	// Read Simon's YAML config file (the one passed via the -f flag).
	fmt.Printf("DEBUG FRA => trying reading SimonConfig file: %s\n", opts.SimonConfig)
	configFile, err := ioutil.ReadFile(opts.SimonConfig)
	if err != nil {
		log.Fatalf("failed to read config file(%s): %v", opts.SimonConfig, err)
	}
	// fmt.Printf("DEBUG FRA => SimonConfig YAML content: %s\n", configFile)

	// Unmarshal Simon's config file from YAML to JSON.
	configJSON, err := yaml.YAMLToJSON(configFile)
	if err != nil {
		log.Fatalf("failed to unmarshal config file(%s) to json: %v", opts.SimonConfig, err)
	}
	// fmt.Printf("DEBUG FRA => SimonConfig JSON content: %s\n", configJSON)

	// Unmarshal Simon's config file from JSON to object of type Simon (see also the package v1alpha1).
	// NOTE: at this point, the application, via the config file, can see how the nodes in the cluster are made, and the pods that must
	//       be executed in said nodes. The list of apps, however, is still empty, and will be populated later.
	if err := json.Unmarshal(configJSON, simonCR); err != nil {
		log.Fatalf("failed to unmarshal config json to object: %v", err)
	}
	// fmt.Printf("DEBUG FRA => executing function NewApplier! Content struct Simon (2): %+v\n", simonCR)

	applier := &Applier{
		cluster:           simonCR.Spec.Cluster,
		appList:           simonCR.Spec.AppList,
		customConfig:      simonCR.Spec.CustomConfig,
		schedulerConfig:   opts.DefaultSchedulerConfigFile,
		useGreed:          opts.UseGreed,
		interactive:       opts.Interactive,
		extendedResources: opts.ExtendedResources,
	}
	// fmt.Printf("DEBUG FRA => executing function NewApplier! Content struct Applier: %+v\n", applier)

	// Check (validate) if the information pertaining to the simulated cluster's configuration is correct.
	if err := validate(applier); err != nil {
		fmt.Printf("%v", err)
		os.Exit(1)
	}

	return applier
}

// This function executes an applier.
func (applier *Applier) Run() (err error) {
	var (
		resourceMap  map[string]simulator.ResourceTypes
		resourceList []string
		content      []string
	)
	resourceMap = make(map[string]simulator.ResourceTypes) // Create a new empty map with string keys and values of type simulator.ResourceTypes

	// Convert the application files into the kubernetes objects and generate a ResourceTypes struct, then make a resource list
	// NOTE: the struct "simulator.ResourceTypes" comes from "./pkg/simulator/core.go".
	// NOTE 2: appList does not seem to be present in the YAML files used with the simulator. To be checked.
	fmt.Printf("DEBUG FRA, apply.go.Run() => number of applications in the appList: %d\n", len(applier.appList))
	var appResource simulator.ResourceTypes
	for _, app := range applier.appList {
		// process separately chart and other file
		if app.Chart {
			fmt.Printf("DEBUG FRA, apply.go.Run() => application files are located into a Helm chart. Converting...\n")

			// parse and render chart as a yaml array
			if content, err = chart.ProcessChart(app.Name, app.Path); err != nil {
				return err
			}
		} else {
			fmt.Printf("DEBUG FRA, apply.go.Run() => application files are located into a YAML. Loading and converting its content into Kubernetes objects...\n")

			if content, err = utils.GetYamlContentFromDirectory(app.Path); err != nil {
				return err
			}
		}
		// NOTE: the function "simulator.GetObjectFromYamlContent()" comes from "./pkg/simulator/utils.go"
		if appResource, err = simulator.GetObjectFromYamlContent(content); err != nil {
			return err
		}

		resourceMap[app.Name] = appResource
		resourceList = append(resourceList, app.Name)
	}

	// convert the cluster files into the kubernetes objects and generate a ResourceTypes struct
	// cluster resource generated by two types of cluster, custom cluster and real cluster.
	// NOTE: here is where the information concerning the cluster nodes and the pods to be executed are actually retrieved from the files and used
	//       to instantiate the various Kubernetes objects.
	var clusterResource simulator.ResourceTypes
	if applier.cluster.KubeConfig != "" {
		fmt.Printf("DEBUG FRA, apply.go.Run() Run() => KubeConfig found!\n")

		// generate kube-client
		kubeclient, err := utils.CreateKubeClient(applier.cluster.KubeConfig)
		if err != nil {
			return err
		}
		if clusterResource, err = simulator.CreateClusterResourceFromClient(kubeclient); err != nil {
			return err
		}
	} else {
		fmt.Printf("DEBUG FRA, apply.go.Run() Run() => Simulated cluster config found!\n")
		if clusterResource, err = simulator.CreateClusterResourceFromClusterConfig(applier.cluster.CustomCluster); err != nil {
			return err
		}
	}
	// fmt.Printf("DEBUG FRA, apply.go.Run() Run() => Simulated cluster config: %+v\n", clusterResource)

	// confirm the list of applications that need to be deployed in interactive mode
	// NOTE: this actually seems to be used just for debugging purposes.
	var selectedAppNameList []string
	var selectedResourceList []simulator.AppResource
	if applier.interactive {
		fmt.Printf("DEBUG FRA, apply.go.Run() Run() => interactive mode is enabled.\n")
		var multiQs = []*survey.Question{
			{
				Name: "APPs",
				Prompt: &survey.MultiSelect{
					Message: "Confirm your apps :",
					Options: resourceList,
				},
			},
		}
		err = survey.Ask(multiQs, &selectedAppNameList)
		if err != nil {
			log.Fatalf("%v", err)
		}
	} else {
		selectedAppNameList = resourceList
	}
	for _, name := range selectedAppNameList {
		selectedResourceList = append(selectedResourceList, simulator.AppResource{
			Name:     name,
			Resource: resourceMap[name],
		})
	}

	// fmt.Printf("DEBUG FRA: clusterResource content: %+v\n", clusterResource)
	// fmt.Printf("DEBUG FRA: clusterResource content: %+v\n", selectedResourceList)

	// *** Run the simulator *** //
	// NOTE: Simulate() represents the entry point to the simulator.
	// NOTE 2: Simulate() comes from ./pkg/simulator/core.go
	// NOTE 3: clusterResource, when running with the YAMLs from the example, actually contains all the information related to nodes and pods.
	//	   selectedResourceList is empty (maybe it is used during the simulation? To be checked).
	// NOTE 4: simulator.With* are wrapper functions that take some configuration contained in a string and return an object of type Option, i.e., a function.
	// 	   They get executed when called within Simulate(). See also ./pkg/simulator/simulator.go.
	success := false
	var result *simontype.SimulateResult
	result, err = simulator.Simulate(clusterResource,
		selectedResourceList,
		simulator.WithSchedulerConfig(applier.schedulerConfig), // Parse the configuration of the scheduler.
		simulator.WithKubeConfig(applier.cluster.KubeConfig),   // Ignored when using the simulator.
		simulator.WithCustomConfig(applier.customConfig))       // Parse the configuration of the simulated cluster and workload.

	// *** Check how the simulator ended its execution *** //
	if err != nil {
		return err
	}
	if len(result.UnscheduledPods) == 0 {
		if ok, reason, err := satisfyResourceSetting(result.NodeStatus); err != nil {
			return err
		} else if !ok {
			fmt.Printf(utils.ColorRed+"%s"+utils.ColorReset, reason)
		} else {
			success = true
		}
	} else {
		fmt.Printf(utils.ColorRed+"there are %d unscheduled pods\n"+utils.ColorReset, len(result.UnscheduledPods))
		log.Infof("there are %d unscheduled pods\n", len(result.UnscheduledPods))
		allDaemonSets := clusterResource.DaemonSets
		for _, app := range selectedResourceList {
			allDaemonSets = append(allDaemonSets, app.Resource.DaemonSets...)
		}
		for _, unScheduledPod := range result.UnscheduledPods {
			log.Debugf("failed to schedule pod %s/%s: %s", unScheduledPod.Pod.Namespace, unScheduledPod.Pod.Name, unScheduledPod.Reason)
		}
	}

	if success {
		fmt.Printf(utils.ColorGreen + "Success!\n" + utils.ColorReset)
	} else {
		fmt.Printf(utils.ColorRed + "Failed!\n" + utils.ColorReset)
	}

	return nil
}

// *************************
// PACKAGE PRIVATE FUNCTIONS
// *************************

func validate(applier *Applier) error {
	// fmt.Printf("DEBUG FRA, apply.go.validate() => cluster.KubeConfig = %s\n", applier.cluster.KubeConfig)
	// fmt.Printf("DEBUG FRA, apply.go.validate() => cluster.CustomCluster = %s\n", applier.cluster.CustomCluster)

	if len(applier.cluster.KubeConfig) == 0 && len(applier.cluster.CustomCluster) == 0 ||
		len(applier.cluster.KubeConfig) != 0 && len(applier.cluster.CustomCluster) != 0 {
		return fmt.Errorf("only one of values of both kubeConfig and customConfig must exist ")
	}

	if len(applier.cluster.KubeConfig) != 0 {
		if _, err := os.Stat(applier.cluster.KubeConfig); err != nil {
			return fmt.Errorf("invalid path of kubeConfig: %v ", err)
		}
	}

	if len(applier.cluster.CustomCluster) != 0 {
		if _, err := os.Stat(applier.cluster.CustomCluster); err != nil {
			return fmt.Errorf("invalid path of customConfig: %v ", err)
		}
	}

	if len(applier.schedulerConfig) != 0 {
		if _, err := os.Stat(applier.schedulerConfig); err != nil {
			return fmt.Errorf("invalid path of scheduler config: %v ", err)
		}
	}

	for _, app := range applier.appList {
		if _, err := os.Stat(app.Path); err != nil {
			return fmt.Errorf("invalid path of %s app: %v ", app.Name, err)
		}
	}

	return nil
}

// report print out scheduling result of pods
func report(nodeStatuses []simontype.NodeStatus, extendedResources []string) {
	// Step 1: report pod info
	fmt.Println("Pod Info")
	podTable := tablewriter.NewWriter(os.Stdout)
	header := []string{
		"Node",
		"Pod",
		"CPU Requests",
		"Memory Requests",
	}
	if containLocalStorage(extendedResources) {
		header = append(header, "Volume Request")
	}
	if containGpu(extendedResources) {
		header = append(header, "GPU MILLI Requests")
	}
	header = append(header, "APP Name")
	podTable.SetHeader(header)

	for _, status := range nodeStatuses {
		node := status.Node
		allocatable := node.Status.Allocatable
		for _, pod := range status.Pods {
			if pod.Spec.NodeName != node.Name {
				continue
			}
			req, limit := resourcehelper.PodRequestsAndLimits(pod)
			cpuReq, _, memoryReq, _ := req[corev1.ResourceCPU], limit[corev1.ResourceCPU], req[corev1.ResourceMemory], limit[corev1.ResourceMemory]
			fractionCpuReq := float64(cpuReq.MilliValue()) / float64(allocatable.Cpu().MilliValue()) * 100
			fractionMemoryReq := float64(memoryReq.Value()) / float64(allocatable.Memory().Value()) * 100
			if allocatable.Memory().Value() == 0 {
				fractionMemoryReq = 0
			}

			// app name
			appname := ""
			if str, exist := pod.Labels[simontype.LabelAppName]; exist {
				appname = str
			}
			data := []string{
				node.Name,
				fmt.Sprintf("%s/%s", pod.Namespace, pod.Name),
				fmt.Sprintf("%s(%d%%)", cpuReq.String(), int64(fractionCpuReq)),
				fmt.Sprintf("%s(%d%%)", memoryReq.String(), int64(fractionMemoryReq)),
			}

			// Storage
			if containLocalStorage(extendedResources) {
				podVolumeStr := ""
				if volumes := utils.GetPodStorage(pod); volumes != nil {
					for i, volume := range volumes.Volumes {
						volumeQuantity := resource.NewQuantity(volume.Size, resource.BinarySI)
						volumeStr := fmt.Sprintf("<%d> %s: %s", i, volume.Kind, volumeQuantity.String())
						podVolumeStr = podVolumeStr + volumeStr
						if i+1 != len(volumes.Volumes) {
							podVolumeStr = fmt.Sprintf("%s\n", podVolumeStr)
						}
					}
				}
				data = append(data, podVolumeStr)
			}

			// GPU
			if containGpu(extendedResources) {
				gpuMilli := gpushareutils.GetGpuMilliFromPodAnnotation(pod) * int64(gpushareutils.GetGpuCountFromPodAnnotation(pod))
				gpuNumber := gpushareutils.GetGpuCountOfNode(node)
				var gpuMilliRatio int
				if gpuNumber != 0 {
					gpuMilliRatio = int(100 * float64(gpuMilli) / (float64(gpuNumber) * gpushareutils.MILLI))
				}
				data = append(data, fmt.Sprintf("%d(%d%%)", gpuMilli, gpuMilliRatio))
			}

			data = append(data, appname)
			podTable.Append(data)
		}
	}
	podTable.SetAutoMergeCellsByColumnIndex([]int{0})
	podTable.SetRowLine(true)
	podTable.SetAlignment(tablewriter.ALIGN_LEFT)
	podTable.Render() // Send output

	fmt.Println()

	// Step 2: report node info
	fmt.Println("Node Info")
	nodeTable := tablewriter.NewWriter(os.Stdout)
	nodeTableHeader := []string{
		"Node",
		"CPU",
		"CPU Requests",
		"Memory",
		"Memory Requests",
	}
	if containGpu(extendedResources) {
		nodeTableHeader = append(nodeTableHeader, []string{
			"GPU",
			"GPU MILLI Requests",
		}...)
	}
	nodeTableHeader = append(nodeTableHeader, []string{
		"Pod Count",
	}...)
	nodeTable.SetHeader(nodeTableHeader)

	allPods := utils.GetAllPodsPtrFromNodeStatus(nodeStatuses)
	for _, status := range nodeStatuses {
		node := status.Node
		allocatable := node.Status.Allocatable
		reqs, _ := utils.GetPodsTotalRequestsAndLimitsByNodeName(allPods, node.Name)
		nodeCpuReq, nodeMemoryReq := reqs[corev1.ResourceCPU], reqs[corev1.ResourceMemory]
		nodeCpuReqFraction := float64(nodeCpuReq.MilliValue()) / float64(allocatable.Cpu().MilliValue()) * 100
		var nodeMemoryReqFraction float64
		if allocatable.Memory().Value() != 0 {
			nodeMemoryReqFraction = float64(nodeMemoryReq.Value()) / float64(allocatable.Memory().Value()) * 100
		}

		data := []string{
			node.Name,
			allocatable.Cpu().String(),
			fmt.Sprintf("%s(%d%%)", nodeCpuReq.String(), int64(nodeCpuReqFraction)),
			allocatable.Memory().String(),
			fmt.Sprintf("%s(%d%%)", nodeMemoryReq.String(), int64(nodeMemoryReqFraction)),
		}
		if containGpu(extendedResources) {
			var nodeGpuMilliReq int64
			for _, pod := range allPods {
				if pod.Spec.NodeName == node.Name {
					gpuMilli := gpushareutils.GetGpuMilliFromPodAnnotation(pod) * int64(gpushareutils.GetGpuCountFromPodAnnotation(pod))
					nodeGpuMilliReq += gpuMilli
				}
			}

			nodeGpuCount := gpushareutils.GetGpuCountOfNode(node)
			nodeGpuMilliRatio := int(100 * float64(nodeGpuMilliReq) / (float64(nodeGpuCount) * gpushareutils.MILLI))
			data = append(data, []string{
				fmt.Sprintf("%d", nodeGpuCount),
				fmt.Sprintf("%d(%d%%)", nodeGpuMilliReq, nodeGpuMilliRatio),
			}...)
		}

		nodeTable.Append(data)
	}
	nodeTable.SetRowLine(true)
	nodeTable.SetAlignment(tablewriter.ALIGN_LEFT)
	nodeTable.Render() // Send output
	fmt.Println()

	// Step 3: report extended resource info (e.g., node storage, GPU)
	if len(extendedResources) != 0 {
		fmt.Println("Extended Resource Info")
		if containLocalStorage(extendedResources) {
			fmt.Println("Node Local Storage")
			nodeStorageTable := tablewriter.NewWriter(os.Stdout)
			nodeStorageTable.SetHeader([]string{
				"Node",
				"Storage Kind",
				"Storage Name",
				"Storage Allocatable",
				"Storage Requests",
			})
			for _, status := range nodeStatuses {
				node := status.Node
				if nodeStorageStr, exist := node.Annotations[simontype.AnnoNodeLocalStorage]; exist {
					var nodeStorage utils.NodeStorage
					if err := ffjson.Unmarshal([]byte(nodeStorageStr), &nodeStorage); err != nil {
						log.Fatalf("failed to unmarshal storage information of node(%s): %v", node.Name, err)
					}
					var storageData []string
					for _, vg := range nodeStorage.VGs {
						capacity := resource.NewQuantity(vg.Capacity, resource.BinarySI)
						request := resource.NewQuantity(vg.Requested, resource.BinarySI)
						storageData = []string{
							node.Name,
							"VG",
							vg.Name,
							capacity.String(),
							fmt.Sprintf("%s(%d%%)", request.String(), int64(float64(vg.Requested)/float64(vg.Capacity)*100)),
						}
						nodeStorageTable.Append(storageData)
					}

					for _, device := range nodeStorage.Devices {
						capacity := resource.NewQuantity(device.Capacity, resource.BinarySI)
						used := "unused"
						if device.IsAllocated {
							used = "used"
						}
						storageData = []string{
							node.Name,
							fmt.Sprintf("Device(%s)", device.MediaType),
							device.Device,
							capacity.String(),
							used,
						}
						nodeStorageTable.Append(storageData)
					}
				}
			}
			nodeStorageTable.SetAutoMergeCellsByColumnIndex([]int{0})
			nodeStorageTable.SetRowLine(true)
			nodeStorageTable.SetAlignment(tablewriter.ALIGN_LEFT)
			nodeStorageTable.Render() // Send output
		}
		if containGpu(extendedResources) {
			var podList []*corev1.Pod
			fmt.Println("GPU Node Resource")
			nodeGpuTable := tablewriter.NewWriter(os.Stdout)
			nodeGpuTable.SetHeader([]string{"Node", "GPU ID", "GPU MILLI Request/Capacity", "Pod List"})
			for _, status := range nodeStatuses {
				node := status.Node
				podList = append(podList, status.Pods...)

				if gn, err := utils.GetGpuNodeInfoFromAnnotation(node); err != nil || gn == nil {
					continue
				} else {
					nodeOutputLine := []string{
						fmt.Sprintf("%s (%s)", node.Name, gn.GpuModel),                         // "Node"
						fmt.Sprintf("%d GPUs", gn.GpuCount),                                    // "GPU ID"
						fmt.Sprintf("%d/%d", gn.GpuUsedMilli, gn.GpuCount*gpushareutils.MILLI), // "GPU Request/Capacity"
						fmt.Sprintf("%d Pods", gn.NumPods),                                     // "Pod List"
					}
					nodeGpuTable.Append(nodeOutputLine)

					for idx := 0; idx < len(gn.DevsBrief); idx += 1 {
						if dib, ok := gn.DevsBrief[idx]; ok {
							nodeOutputLineDev := []string{
								fmt.Sprintf("%s (%s)", node.Name, gn.GpuModel),              // "Node"
								fmt.Sprintf("%d", idx),                                      // "GPU ID"
								fmt.Sprintf("%d/%d", dib.GpuUsedMilli, gpushareutils.MILLI), // "GPU Request/Capacity"
								fmt.Sprintf("%s", dib.PodList),                              // "Pod List"
							}
							nodeGpuTable.Append(nodeOutputLineDev)
						}
					}
				}
			}
			nodeGpuTable.SetAutoMergeCellsByColumnIndex([]int{0})
			nodeGpuTable.SetRowLine(true)
			nodeGpuTable.SetAlignment(tablewriter.ALIGN_LEFT)
			nodeGpuTable.Render() // Send output

			fmt.Println("\nPod -> Node Map")
			podGpuTable := tablewriter.NewWriter(os.Stdout)
			podGpuTable.SetHeader([]string{"Pod", "CPU Req", "Mem Req", "GPU MILLI Req", "Host Node", "GPU IDX"})
			sort.Slice(podList, func(i, j int) bool { return podList[i].Name < podList[j].Name })
			for _, pod := range podList {
				req, limit := resourcehelper.PodRequestsAndLimits(pod)
				gpuMilli := gpushareutils.GetGpuMilliFromPodAnnotation(pod)
				cpuReq, _, memoryReq, _ := req[corev1.ResourceCPU], limit[corev1.ResourceCPU], req[corev1.ResourceMemory], limit[corev1.ResourceMemory]
				gpuIndex := gpushareutils.GetGpuIdFromAnnotation(pod)
				podOutputLine := []string{pod.Name, cpuReq.String(), memoryReq.String(), fmt.Sprintf("%d", gpuMilli), pod.Spec.NodeName, gpuIndex}
				podGpuTable.Append(podOutputLine)
			}
			podGpuTable.SetRowLine(true)
			podGpuTable.SetAlignment(tablewriter.ALIGN_LEFT)
			podGpuTable.Render() // Send output
		}
	}
}

func satisfyResourceSetting(nodeStatuses []simontype.NodeStatus) (bool, string, error) {
	var err error
	var maxcpu int = 100
	var maxmem int = 100
	var maxvg int = 100
	if str := os.Getenv(simontype.EnvMaxCPU); str != "" {
		if maxcpu, err = strconv.Atoi(str); err != nil {
			return false, "", fmt.Errorf("failed to convert env %s to int: %s ", simontype.EnvMaxCPU, err.Error())
		}
		if maxcpu > 100 || maxcpu < 0 {
			maxcpu = 100
		}
	}

	if str := os.Getenv(simontype.EnvMaxMemory); str != "" {
		if maxmem, err = strconv.Atoi(str); err != nil {
			return false, "", fmt.Errorf("failed to convert env %s to int: %s ", simontype.EnvMaxMemory, err.Error())
		}
		if maxmem > 100 || maxmem < 0 {
			maxmem = 100
		}
	}

	if str := os.Getenv(simontype.EnvMaxVG); str != "" {
		if maxvg, err = strconv.Atoi(str); err != nil {
			return false, "", fmt.Errorf("failed to convert env %s to int: %s ", simontype.EnvMaxVG, err.Error())
		}
		if maxvg > 100 || maxvg < 0 {
			maxvg = 100
		}
	}

	totalAllocatableResource := map[corev1.ResourceName]*resource.Quantity{
		corev1.ResourceCPU:    resource.NewQuantity(0, resource.DecimalSI),
		corev1.ResourceMemory: resource.NewQuantity(0, resource.DecimalSI),
	}
	totalUsedResource := map[corev1.ResourceName]*resource.Quantity{
		corev1.ResourceCPU:    resource.NewQuantity(0, resource.DecimalSI),
		corev1.ResourceMemory: resource.NewQuantity(0, resource.DecimalSI),
	}
	totalVGResource := localcache.SharedResource{}
	allPods := utils.GetAllPodsPtrFromNodeStatus(nodeStatuses)

	for _, status := range nodeStatuses {
		node := status.Node
		totalAllocatableResource[corev1.ResourceCPU].Add(*node.Status.Allocatable.Cpu())
		totalAllocatableResource[corev1.ResourceMemory].Add(*node.Status.Allocatable.Memory())

		reqs, _ := utils.GetPodsTotalRequestsAndLimitsByNodeName(allPods, node.Name)
		totalUsedResource[corev1.ResourceCPU].Add(reqs[corev1.ResourceCPU])
		totalUsedResource[corev1.ResourceMemory].Add(reqs[corev1.ResourceMemory])

		if nodeStorageStr, exist := node.Annotations[simontype.AnnoNodeLocalStorage]; exist {
			var nodeStorage utils.NodeStorage
			if err := ffjson.Unmarshal([]byte(nodeStorageStr), &nodeStorage); err != nil {
				return false, "", fmt.Errorf("error when unmarshal json data, node is %s\n", node.Name)
			}
			for _, vg := range nodeStorage.VGs {
				totalVGResource.Requested += vg.Requested
				totalVGResource.Capacity += vg.Capacity
			}
		}
	}

	cpuOccupancyRate := int(float64(totalUsedResource[corev1.ResourceCPU].MilliValue()) / float64(totalAllocatableResource[corev1.ResourceCPU].MilliValue()) * 100)
	memoryOccupancyRate := int(float64(totalUsedResource[corev1.ResourceMemory].MilliValue()) / float64(totalAllocatableResource[corev1.ResourceMemory].MilliValue()) * 100)
	if cpuOccupancyRate > maxcpu {
		return false, fmt.Sprintf("the average occupancy rate(%d%%) of cpu goes beyond the env setting(%d%%)\n", cpuOccupancyRate, maxcpu), nil
	}
	if memoryOccupancyRate > maxmem {
		return false, fmt.Sprintf("the average occupancy rate(%d%%) of memory goes beyond the env setting(%d%%)\n", memoryOccupancyRate, maxmem), nil
	}

	if totalVGResource.Capacity != 0 {
		vgOccupancyRate := int(float64(totalVGResource.Requested) / float64(totalVGResource.Capacity) * 100)
		if vgOccupancyRate > maxvg {
			return false, fmt.Sprintf("the average occupancy rate(%d%%) of vg goes beyond the env setting(%d%%)\n", vgOccupancyRate, maxvg), nil
		}
	}

	return true, "", nil
}

func containLocalStorage(extendedResources []string) bool {
	for _, res := range extendedResources {
		if res == "open-local" {
			return true
		}
	}
	return false
}

func containGpu(extendedResources []string) bool {
	for _, res := range extendedResources {
		if res == "gpu" {
			return true
		}
	}
	return false
}
