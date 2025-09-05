package healthcheckpkg

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"path/filepath"

	"github.com/openshift/origin/pkg/test"
	"github.com/openshift/origin/pkg/test/ginkgo/junitapi"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/objx"
	k8sv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	clientset "k8s.io/client-go/kubernetes"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
)

type Options struct {
	JUnitDir string
}

// Konwn dependency mapping
// key: operator name
// value: the depenent operators required by the operator
var operatorDependencies = map[string][]string{
	"etcd":                         {},
	"network":                      {"etcd"},
	"kube-apiserver":               {"etcd", "network"},
	"kube-controller-manager":      {"kube-apiserver"},
	"kube-scheduler":               {"kube-apiserver"},
	"service-ca":                   {"kube-apiserver"},
	"cloud-credential":             {"network"},
	"dns":                          {"kube-apiserver"},
	"openshift-apiserver":          {"kube-apiserver"},
	"openshift-controller-manager": {"openshift-apiserver", "kube-apiserver"},
	"cloud-controller-manager":     {"kube-apiserver", "cloud-credential"},
	"machine-api":                  {"cloud-controller-manager", "cloud-credential", "kube-apiserver"},
	"machine-config":               {"kube-apiserver", "openshift-apiserver", "machine-api"},
	"ingress":                      {"network", "machine-api", "cloud-credential", "dns"},
	"storage":                      {"cloud-credential", "machine-api"},
	"image-registry":               {"ingress", "cloud-credential"},
	"authentication":               {"ingress"},
	"console":                      {"authentication", "ingress"},
	"monitoring":                   {"storage"},
}

func (opt *Options) Run() error {
	cfg, err := e2e.LoadConfig()
	if err != nil {
		logrus.WithError(err).Error("kubeconfig file NOT found:")
		return nil
	}

	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		logrus.WithError(err).Error("Fail to get generate dynamic client")
		return nil
	}
	cs, err := clientset.NewForConfig(cfg)
	if err != nil {
		logrus.WithError(err).Error("Fail to get generate client set")
		return nil
	}

	logrus.Infof("Check ClusterVersion Stability...")
	if err := checkClusterVersionStable(dc); err != nil {
		logrus.Warnf("Continue though cluster version stability check failed (%v)", err)
	}

	suite := &junitapi.JUnitTestSuite{
		Name:      "Cluster Health Check",
		TestCases: []*junitapi.JUnitTestCase{},
	}

	// ======================================================
	// ===== Consistency check between machine and node =====
	// ======================================================
	checkMachineNodeConsistency(cs, dc, suite)

	// ======================================================
	// =========== Check cluster operators health ===========
	// ======================================================
	logrus.Infof("Checking Cluster Operators...")

	coc := dc.Resource(schema.GroupVersionResource{
		Group:    "config.openshift.io",
		Resource: "clusteroperators",
		Version:  "v1",
	})
	clusterOperatorsObj, err := coc.List(context.Background(), metav1.ListOptions{})
	if err != nil {
		logrus.WithError(err).Error("Failed to list clusteroperators")
		return nil
	}

	// save all the operators items to futher querying
	operatorsMap := make(map[string]objx.Map)
	items := objects(objx.Map(clusterOperatorsObj.UnstructuredContent()).Get("items"))
	for _, item := range items {
		name := item.Get("metadata.name").String()
		operatorsMap[name] = item
	}

	// ===== stage 1: create a ordered operator list =====
	finalOperatorDependencies := expandDependencies(operatorDependencies)

	// get all core operators, all the keys in the operatorDependencies map
	var coreOperators []string
	for op := range finalOperatorDependencies {
		coreOperators = append(coreOperators, op)
	}
	sort.Strings(coreOperators)

	// sort core operators by topological order
	sortedCoreOperators, err := TopologicalSort(coreOperators, finalOperatorDependencies)
	if err != nil {
		logrus.WithError(err).Error("Failed to sort core operators")
		return nil
	}

	logrus.Infof("Core operators will be checked in order:\n%s", strings.Join(sortedCoreOperators, "\n"))

	// Per sorted core operators, consume each operator from operatorsMap
	// meantime, mark it if it is core operator
	var finalOperators []struct {
		Name string
		Op   objx.Map
	}

	for _, opName := range sortedCoreOperators {
		if op, exists := operatorsMap[opName]; exists {
			finalOperators = append(finalOperators, struct {
				Name string
				Op   objx.Map
			}{Name: opName, Op: op})
			// remove it from map, means it is consumed
			delete(operatorsMap, opName)
		} else {
			// if not exist, Op will be filled with nil
			finalOperators = append(finalOperators, struct {
				Name string
				Op   objx.Map
			}{Name: opName, Op: nil})
		}
	}

	// append all the left operators to finalOperators
	for opName, op := range operatorsMap {
		finalOperators = append(finalOperators, struct {
			Name string
			Op   objx.Map
		}{Name: opName, Op: op})
	}

	logrus.Infof("Final operator list has %d items (%d core + %d additional)", len(finalOperators), len(sortedCoreOperators), len(operatorsMap))

	// ===== stage 2: check each operator per the ordered list =====
	var failedOperators = make(map[string]bool)
	tcNamePrefix := "operator conditions"

	for _, item := range finalOperators {
		opName := item.Name
		op := item.Op
		tcName := fmt.Sprintf("%s %s", tcNamePrefix, opName)
		var skipMsg string
		var failureMsg string

		logrus.Infof("Checking %s.........", opName)

		// when the core operator does not exist in the cluster, skip it
		if op == nil {
			skipMsg = fmt.Sprintf("Operator %q not found in the cluster, skipping", opName)
			logrus.Infof("%s", skipMsg)
		} else {
			// when any dependent operator failed, skip the current operator
			for _, dep := range finalOperatorDependencies[opName] {
				if failedOperators[dep] {
					skipMsg = fmt.Sprintf("Precondition operator %q failed, skipping", dep)
					logrus.Infof("%s", skipMsg)
					break
				}
			}
		}
		// some cases used to valid the logic:
		// image-registry not installed, and it depends on ingress, while ingress failed
		// authentication depends on ingress, while ingress failed
		// C depends on A, B, while both A and B failed.
		if skipMsg != "" {
			tcAppend(suite, tcName, "", skipMsg)
			continue
		}

		// check operator status
		availableCond := condition(op, "Available")
		available := availableCond.Get("status").String()
		degradedCond := condition(op, "Degraded")
		degraded := degradedCond.Get("status").String()
		progressingCond := condition(op, "Progressing")
		progressing := progressingCond.Get("status").String()

		if available == "True" && degraded == "False" && progressing == "False" {
			logrus.Infof("%s PASSed", opName)
			tcAppend(suite, tcName, "", "")
		} else {
			failureMsg = fmt.Sprintf("Operator %q - Available=%s, Degraded=%s, Progressing=%s", opName, available, degraded, progressing)
			logrus.Infof("%s", failureMsg)
			tcAppend(suite, tcName, failureMsg, "")
			failedOperators[opName] = true
		}
	}

	suite.NumTests = uint(len(suite.TestCases))

	out, err := xml.MarshalIndent(suite, "", "    ")
	if err != nil {
		logrus.WithError(err).Error("Fail to deal with xml format:")
		return nil
	}
	fmt.Println(string(out))
	if opt.JUnitDir != "" {
		filePrefix := "cluster-health-check"
		start := time.Now()
		timeSuffix := fmt.Sprintf("_%s", start.UTC().Format("20060102-150405"))
		path := filepath.Join(opt.JUnitDir, fmt.Sprintf("%s_%s.xml", filePrefix, timeSuffix))
		fmt.Fprintf(os.Stderr, "Writing JUnit report to %s\n", path)
		os.WriteFile(path, test.StripANSI(out), 0640)
	}

	return nil
}

// Explore all the dependency to expand the dependencies map
// For Example:
//
//	map[string][]string{
//		"A":        {"B", "C"},
//	}
//
// will retrun:
//
//	map[string][]string{
//		"B":		{},
//		"C": 		{},
//		"A":        {"B", "C"},
//	}
//
// ------------------------------
//
//	map[string][]string{
//		"A":        {"B"},
//		"B":		{"C"},
//		}
//
// will retrun:
//
//	map[string][]string{
//		"C":		{},
//		"B": 		{"C"},
//		"A":        {"B", "C"},
//	}
func expandDependencies(manualDeps map[string][]string) map[string][]string {
	coreSet := make(map[string]bool)

	for op := range manualDeps {
		coreSet[op] = true
	}

	for _, deps := range manualDeps {
		for _, dep := range deps {
			coreSet[dep] = true
		}
	}

	finalDeps := make(map[string][]string)

	for op := range coreSet {
		var allDeps []string
		if _, exists := manualDeps[op]; exists {
			allDeps = getAllUpstreamDependencies(op, manualDeps)
		}
		finalDeps[op] = allDeps
	}

	//logrus.Infof("Final dependency graph: %v", finalDeps)

	return finalDeps
}

func getAllUpstreamDependencies(op string, deps map[string][]string) []string {
	visited := make(map[string]bool)
	resultSet := make(map[string]bool)

	dfs(op, deps, visited, resultSet)

	var result []string
	for dep := range resultSet {
		result = append(result, dep)
	}
	sort.Strings(result)

	return result
}

func dfs(current string, deps map[string][]string, visited, resultSet map[string]bool) {
	if visited[current] {
		return
	}
	visited[current] = true

	if directDeps, exists := deps[current]; exists {
		for _, dep := range directDeps {
			resultSet[dep] = true
			dfs(dep, deps, visited, resultSet)
		}
	}
}

// Sort operators by topological order
func TopologicalSort(operators []string, dependencies map[string][]string) ([]string, error) {
	// initalize in-degree for each operator
	inDegree := make(map[string]int)
	for _, op := range operators {
		inDegree[op] = 0
	}

	// calcuate in-degree for each operator
	for _, op := range operators {
		for _, dep := range dependencies[op] {
			if stringInSlice(dep, operators) {
				inDegree[op]++
			}
		}
	}

	// add the in-degree=0 operator into queue
	var queue []string
	for _, op := range operators {
		if inDegree[op] == 0 {
			queue = append(queue, op)
		}
	}

	// main logic of the topological sort
	var sorted []string
	for len(queue) > 0 {
		// get the 1st item
		node := queue[0]
		queue = queue[1:]
		sorted = append(sorted, node)

		// reduce the in-degree for each dependency operator when the key operator is poped from the queue
		// e.g:
		// A B C depends on D, when D is poped from the queue, the in-degree minus 1
		for _, op := range operators {
			if stringInSlice(node, dependencies[op]) {
				inDegree[op]--
				if inDegree[op] == 0 {
					queue = append(queue, op)
				}
			}
		}
	}

	// checking if there is a dependency cycle
	if len(sorted) != len(operators) {
		return nil, fmt.Errorf("dependency graph has a cycle")
	}

	return sorted, nil
}

func stringInSlice(str string, slice []string) bool {
	for _, s := range slice {
		if s == str {
			return true
		}
	}
	return false
}

func checkClusterVersionStable(dc dynamic.Interface) error {
	cvc := dc.Resource(schema.GroupVersionResource{
		Group:    "config.openshift.io",
		Resource: "clusterversions",
		Version:  "v1",
	})

	obj, err := cvc.Get(context.Background(), "version", metav1.GetOptions{})
	if err != nil {
		logrus.WithError(err).Error("Fail to get cluster version:")
		return err
	}

	cv := objx.Map(obj.UnstructuredContent())

	if cond := condition(cv, "Available"); cond.Get("status").String() != "True" {
		err := fmt.Errorf("clusterversion not available")
		logrus.WithError(err).Errorf("ClusterVersion Available=%s", getInfoFromCondition(cond))
		return err
	}
	if cond := condition(cv, "Failing"); cond.Get("status").String() != "False" {
		err := fmt.Errorf("clusterversion is failing")
		logrus.WithError(err).Errorf("ClusterVersion Failing=%s", getInfoFromCondition(cond))
		return err
	}
	if cond := condition(cv, "Progressing"); cond.Get("status").String() != "False" {
		err := fmt.Errorf("clusterversion is progressing")
		logrus.WithError(err).Errorf("ClusterVersion Progressing=%s", getInfoFromCondition(cond))
		return err
	}

	return nil
}

func checkMachineNodeConsistency(clientset clientset.Interface, dc dynamic.Interface, suite *junitapi.JUnitTestSuite) {
	logrus.Info("Starting Machine and Node consistency check")

	// ===== Case 1 =====
	tcName := "all machines should be in Running state"
	machineClient := dc.Resource(schema.GroupVersionResource{
		Group:    "machine.openshift.io",
		Version:  "v1beta1",
		Resource: "machines",
	})

	machineList, err := machineClient.Namespace("openshift-machine-api").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		message := fmt.Sprintf("Could not list machines: %v. The Machine API might not be available.", err)
		tcAppend(suite, tcName, message, "")
		// not retrun on purpose, so that continue to run other cases
	}

	var runningMachineCount int
	var notRunningMachines []string

	if machineList != nil && len(machineList.Items) > 0 {
		for _, machine := range machineList.Items {
			phase, _, _ := unstructured.NestedString(machine.Object, "status", "phase")
			if strings.ToLower(phase) == "running" {
				runningMachineCount++
			} else {
				name := machine.GetName()
				notRunningMachines = append(notRunningMachines, fmt.Sprintf("Machine %q is in %q state", name, phase))
			}
		}

		if len(notRunningMachines) == 0 {
			tcAppend(suite, tcName, "", "")
		} else {
			message := fmt.Sprintf("Found %d out of %d Machines not in Running state: ", len(notRunningMachines), len(machineList.Items))
			message += strings.Join(notRunningMachines, " ")
			tcAppend(suite, tcName, message, "")
		}
	} else {
		message := "No Machines found or could not retrieve list. Skipping Machine check."
		tcAppend(suite, tcName, "", message)
	}

	if len(notRunningMachines) > 0 {
		logrus.Warnf("Proceeding with node count check despite %d non-Running machines", len(notRunningMachines))
	}

	// ===== Case 2 =====
	tcName = "all nodes should be ready"
	nodeList, err := clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		message := fmt.Sprintf("Failed to list nodes: %v.", err)
		tcAppend(suite, tcName, message, "")
		return
	}

	notReadyNodes := getUnreadyOrUnschedulableNodeNames(nodeList)

	if len(notReadyNodes) == 0 {
		tcAppend(suite, tcName, "", "")
	} else {
		message := fmt.Sprintf("Found %d out of %d Nodes not Ready or unscheduable: ", len(notReadyNodes), len(nodeList.Items))
		message += strings.Join(notReadyNodes, " ")
		tcAppend(suite, tcName, message, "")
	}

	// ===== Case 3 =====
	tcName = "node count should match or exceed machine count"

	totalNodeCount := len(nodeList.Items)
	readyNodeCount := totalNodeCount - len(notReadyNodes)
	logrus.Infof("Found %d Ready and Scheduable Nodes out of %d total Nodes", readyNodeCount, totalNodeCount)

	if readyNodeCount >= runningMachineCount {
		logrus.Infof("Ready and Scheduable Nodes count (%d) >= Running Machine count (%d). Check passed.", readyNodeCount, runningMachineCount)
		tcAppend(suite, tcName, "", "")
	} else {
		message := fmt.Sprintf("Ready and Scheduable Nodes count (%d) is less than Running Machine count (%d): ", readyNodeCount, runningMachineCount)
		message += "Ready and Scheduable Nodes:"
		for _, node := range nodeList.Items {
			if e2enode.IsNodeSchedulable(&node) {
				message += fmt.Sprintf(" %s", node.Name)
			}
		}

		message += "; Running Machines:"
		for _, machine := range machineList.Items {
			phase, _, _ := unstructured.NestedString(machine.Object, "status", "phase")
			if strings.ToLower(phase) == "running" {
				message += fmt.Sprintf(" %s ", machine.GetName())
			}
		}
		tcAppend(suite, tcName, message, "")
	}
}

func tcAppend(suite *junitapi.JUnitTestSuite, tcName string, tcFailureMsg string, tcSkipMsg string) {
	//suite.NumTests++
	tc := &junitapi.JUnitTestCase{
		Name: tcName,
	}
	if tcFailureMsg != "" {
		tc.FailureOutput = &junitapi.FailureOutput{Message: tcFailureMsg}
		suite.NumFailed++
	} else if tcSkipMsg != "" {
		tc.SkipMessage = &junitapi.SkipMessage{Message: tcSkipMsg}
		suite.NumSkipped++
	}
	suite.TestCases = append(suite.TestCases, tc)
}

// GetUnreadyOrUnschedulableNodeNames returns a list of node names that are
// either not ready or marked as unschedulable.
func getUnreadyOrUnschedulableNodeNames(allNodes *k8sv1.NodeList) []string {
	var badNodeNames []string
	for _, node := range allNodes.Items {
		// IsNodeSchedulable checks if the node is ready AND schedulable.
		// If it returns false, the node is one we're interested in.
		if !e2enode.IsNodeSchedulable(&node) {
			badNodeNames = append(badNodeNames, node.Name)
		}
	}

	return badNodeNames
}

func objects(from *objx.Value) []objx.Map {
	var values []objx.Map
	switch {
	case from.IsObjxMapSlice():
		return from.ObjxMapSlice()
	case from.IsInterSlice():
		for _, i := range from.InterSlice() {
			if msi, ok := i.(map[string]interface{}); ok {
				values = append(values, objx.Map(msi))
			}
		}
	}
	return values
}

func getInfoFromCondition(cond objx.Map) string {
	infoString := fmt.Sprintf("%s | %s | %s",
		cond.Get("status").String(),
		cond.Get("reason").String(),
		cond.Get("message").String(),
	)
	return infoString
}

func condition(cv objx.Map, condition string) objx.Map {
	for _, obj := range objects(cv.Get("status.conditions")) {
		if obj.Get("type").String() == condition {
			return obj
		}
	}
	return objx.Map(nil)
}
