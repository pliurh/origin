package networking

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"

	frrapi "github.com/metallb/frr-k8s/api/v1beta1"
	ratypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	"k8s.io/kubernetes/test/e2e/framework/skipper"
	admissionapi "k8s.io/pod-security-admission/api"

	configv1 "github.com/openshift/api/config/v1"

	exutil "github.com/openshift/origin/test/extended/util"
)

const (
	// for all tests
	bgpNamespacePrefix = "bgp"
	bgpProbePodName    = "bgp-prober-pod"
	targetProtocol     = "http"
	targetPort         = 8000
)

var _ = g.Describe("[sig-network][OCPFeatureGate:RouteAdvertisements][Feature:RouteAdvertisements][apigroup:operator.openshift.io]", func() {
	oc := exutil.NewCLIWithPodSecurityLevel(bgpNamespacePrefix, admissionapi.LevelPrivileged)
	f := oc.KubeFramework()
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	InOVNKubernetesContext(func() {
		var (
			networkPlugin string

			clientset kubernetes.Interface
			tmpDirBGP string

			workerNodesOrdered      []corev1.Node
			workerNodesOrderedNames []string
			advertisedPodsNodes      []string
			egressIPNodes           []string
			externalNodeName        string
			targetNamespace         string
			externalNamespace       string
			packetSnifferInterface  string
			cloudType               configv1.PlatformType
			deployName              string
			routeName               string
			ingressDomain           string
			packetSnifferDaemonSet  *v1.DaemonSet
			podList                 *corev1.PodList
			podIPSet                map[string]string
		)

		g.BeforeEach(func() {
			targetNamespace = f.Namespace.Name
			clientset = f.ClientSet
			networkPlugin = networkPluginName()

			g.By("Creating a temp directory")
			var err error
			tmpDirBGP, err = os.MkdirTemp("", "bgp-e2e")
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Determining the cloud infrastructure type")
			infra, err := oc.AdminConfigClient().ConfigV1().Infrastructures().Get(context.Background(), "cluster", metav1.GetOptions{})
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Verifying that the platform is baremetal")
			if infra.Spec.PlatformSpec.Type != configv1.BareMetalPlatformType {
				skipper.Skipf("This cloud platform (%s) is not supported for this test", cloudType)
			}

			// The RouteAdvertisements feature must be enabled by featuregate.
			// Otherwise, skip this test.
			g.By("Verifying that the RouteAdvertisements feature is enabled by featuregate")
			isBGPSupported := false
			featureGate, err := oc.AdminConfigClient().ConfigV1().FeatureGates().Get(context.Background(), "cluster", metav1.GetOptions{})
			o.Expect(err).NotTo(o.HaveOccurred())
			for _, feature := range featureGate.Status.FeatureGates[0].Enabled {
				if feature.Name == "RouteAdvertisements" {
					isBGPSupported = true
					break
				}
			}
			if !isBGPSupported {
				skipper.Skipf("The RouteAdvertisements feature is not enabled by featuregate")
			}

			g.By("Verifying that the RouteAdvertisements is enabled in the cluster")
			networkOperator, err := oc.AdminOperatorClient().OperatorV1().Networks().Get(context.Background(), "cluster", metav1.GetOptions{})
			o.Expect(err).NotTo(o.HaveOccurred())

			if networkOperator.Spec.AdditionalRoutingCapabilities == nil ||
				networkOperator.Spec.DefaultNetwork.OVNKubernetesConfig == nil ||
				networkOperator.Spec.DefaultNetwork.OVNKubernetesConfig.RouteAdvertisements != "Enabled" {
				skipper.Skipf("The RouteAdvertisements feature is not enabled in the network.operator CR")
			}

			g.By("Getting all worker nodes in alphabetical order")
			// Get all worker nodes, order them alphabetically with stable
			// sort order.
			workerNodesOrdered, err = getWorkerNodesOrdered(clientset)
			o.Expect(err).NotTo(o.HaveOccurred())
			for _, s := range workerNodesOrdered {
				workerNodesOrderedNames = append(workerNodesOrderedNames, s.Name)
			}
			if len(workerNodesOrdered) < 3 {
				skipper.Skipf("This test requires a minimum of 3 worker nodes. However, this environment has %d worker nodes.", len(workerNodesOrdered))
			}

			g.By("Creating a project for the prober pod")
			// Create a target project and assign source and target namespace
			// to variables for later use.
			externalNamespace = oc.SetupProject()
		})

		// Do not check for errors in g.AfterEach as the other cleanup steps will fail, otherwise.
		g.AfterEach(func() {
			g.By("Removing the temp directory")
			os.RemoveAll(tmpDirBGP)
		})

		g.Context("[PodNetwork] advertise the default network [apigroup:user.openshift.io][apigroup:security.openshift.io]", func() {
			g.JustBeforeEach(func() {
				// SCC privileged is needed to run tcpdump on the packet sniffer containers, and at the minimum host networked is needed for
				// host networked pods.
				g.By("Adding SCC privileged to the external namespace")
				_, err := runOcWithRetry(oc.AsAdmin(), "adm", "policy", "add-scc-to-user", "privileged", fmt.Sprintf("system:serviceaccount:%s:default", externalNamespace))
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Selecting a node to act as as an external host")
				o.Expect(len(workerNodesOrderedNames)).Should(o.BeNumerically(">", 1))
				externalNodeName = workerNodesOrderedNames[0]
				advertisedPodsNodes = workerNodesOrderedNames[1:]

				g.By("Determining the interface that will be used for packet sniffing")
				packetSnifferInterface, err = findPacketSnifferInterface(oc, networkPlugin, []string{externalNodeName})
				o.Expect(err).NotTo(o.HaveOccurred())
				framework.Logf("Using interface %s for packet captures", packetSnifferInterface)

				g.By("Turn on the BGP advertisement of PodNetwork")
				_, err = runOcWithRetry(oc.AsAdmin(), "patch", "ra", "default", "--type=merge", `-p={"spec":{"advertisements":["PodNetwork"]}}`)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Makes sure that the RouteAdvertisements is accepted")
				waitErr := wait.PollUntilContextTimeout(context.TODO(), 10*time.Second, 30*time.Second, true, func(context.Context) (bool, error) {
					ra, err := getRouteAdvertisements(oc, "default")
					if err != nil {
						return false, err
					}
					condition := meta.FindStatusCondition(ra.Status.Conditions, "Accepted")
					return condition.Status == metav1.ConditionTrue, nil
				})
				o.Expect(waitErr).NotTo(o.HaveOccurred())

				g.By("Makes sure the FRR configuration is generated for each node")
				for _, nodeName := range workerNodesOrderedNames {
					frr, err := getGeneratedFrrConfigurationForNode(oc, nodeName)
					o.Expect(err).NotTo(o.HaveOccurred())
					o.Expect(frr).NotTo(o.BeNil())
				}

				g.By("Spawning the packet sniffer pods on the workers")
				packetSnifferDaemonSet, err = createPacketSnifferDaemonSet(oc, externalNamespace, advertisedPodsNodes, targetProtocol, targetPort, packetSnifferInterface)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Creating the test target deployment with number of adervisedPodsNodes")
				ingressDomain, err = getIngressDomain(oc)
				o.Expect(err).NotTo(o.HaveOccurred())
				deployName, routeName, err = createAgnhostDeploymentAndIngressRoute(oc, targetNamespace, "", ingressDomain, len(advertisedPodsNodes), advertisedPodsNodes)
				o.Expect(err).NotTo(o.HaveOccurred())

				podList, err = clientset.CoreV1().Pods(targetNamespace).List(context.TODO(), metav1.ListOptions{LabelSelector: fmt.Sprintf("app=%s", deployName)})
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(len(podList.Items)).To(o.Equal(len(advertisedPodsNodes)))

				g.By("Retrieving the PodIPs of the test source deployment")
				podIPSet = make(map[string]string)
				for _, pod := range podList.Items {
					podIPSet[pod.Status.PodIP] = pod.Spec.NodeName
				}
			})

			g.It("nodes should learn BGP routes to the PodNetwork", func() {
				nodeSubnets, err := getNodeSubnets(oc, "default")
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Checking that BGP routes to the PodNetwork are learned by each node")
				for _, nodeName := range workerNodesOrderedNames {
					bgpV4Routes, bgpV6Routes, err := getLearnedBgpRoutesByNode(oc, nodeName)
					o.Expect(err).NotTo(o.HaveOccurred())

					for node, subnet := range nodeSubnets {
						if node == nodeName {
							continue
						}
						for _, subnet := range subnet {
							if subnet.IP.To4() != nil {
								_, ok := bgpV4Routes[subnet.String()]
								o.Expect(ok).To(o.BeTrue())
							} else {
								_, ok := bgpV6Routes[subnet.String()]
								o.Expect(ok).To(o.BeTrue())
							}
						}
					}
				}
			})

			g.It("pods should communicate with external host without being SNATed", func() {
				numberOfRequestsToSend := 10
				// The baremetal environment is built by dev-scripts, which runs a redfish http server at 192.168.111.1:8000. Use this as the external target host.
				targetHost := "192.168.111.1"

				g.By(fmt.Sprintf("Sending requests from prober and making sure that %d requests with search string and PodIP %v were seen", numberOfRequestsToSend, podIPSet))
				spawnProberSendEgressIPTrafficCheckLogs(oc, externalNamespace, probePodName, routeName, targetProtocol, targetHost, targetPort, numberOfRequestsToSend, numberOfRequestsToSend, packetSnifferDaemonSet, podIPSet)
			})

			g.It("External host should be able to quay route advertised pods by the pod IP", func() {
				g.By("Launching a hostnetworked prober pod")
				nodeSelection := e2epod.NodeSelection{}
				e2epod.SetAffinity(&nodeSelection, externalNodeName)
				proberPod := createProberPod(oc, externalNamespace, bgpProbePodName, func(p *corev1.Pod) {
					e2epod.SetNodeSelection(&p.Spec, nodeSelection)
					p.Spec.HostNetwork = true
				})

				pod, err := clientset.CoreV1().Pods(externalNamespace).Get(context.TODO(), proberPod.Name, metav1.GetOptions{})
				o.Expect(err).NotTo(o.HaveOccurred())
				clientIP := pod.Status.PodIP

				for targetIP := range podIPSet {
					url := fmt.Sprintf("%s:%d", targetIP, targetPort)
					g.By(fmt.Sprintf("Probing for the target pod at %s", url))
					request := fmt.Sprintf("http://%s/clientip", url)
					output, err := runOcWithRetry(oc.AsAdmin(), "exec", "-n", proberPod.Namespace, proberPod.Name, "--", "curl", "-s", request)
					o.Expect(err).NotTo(o.HaveOccurred())

					g.By("Making sure that probe pod IP presents in the response")
					framework.Logf("output: %s clientIP: %s", output, clientIP)
					clientIpPort := strings.Split(output, ":")
					o.Expect(clientIpPort).To(o.HaveLen(2))
					o.Expect(clientIpPort[0]).To(o.Equal(clientIP))

					g.By("Making sure that probe pod IP presents in the sniffer packet log")
					var found map[string]int
					err = wait.PollUntilContextTimeout(context.TODO(), 10*time.Second, 30*time.Second, true, func(context.Context) (bool, error) {
						found, _ = scanPacketSnifferDaemonSetPodLogs(oc, packetSnifferDaemonSet, targetProtocol, clientIP)
						if len(found) != 0 {
							framework.Logf("found: %v", found)
							return true, nil
						}
						return false, nil
					})
					o.Expect(err).NotTo(o.HaveOccurred())
					o.Expect(len(found)).To(o.BeNumerically(">", 0))
				}
				err = destroyProberPod(oc, proberPod)
				o.Expect(err).NotTo(o.HaveOccurred())
			})
		})

		g.Context("[PodNetwork] advertise a user define network to the default VRF [apigroup:user.openshift.io][apigroup:security.openshift.io]", func() {
			var (
				userDefinedNetworkIPv4Subnet = "203.203.0.0/16"
				userDefinedNetworkIPv6Subnet = "2014:100:200::0/60"
				nodeSubnets                  map[string]string
				udnRaName                    = "udn1"
			)

			g.BeforeEach(func() {
				g.By("Labeling the target namespace for using a user defined network as primary")
				var err error
				l := map[string]string{
					"e2e-framework": f.BaseName,
					RequiredUDNNamespaceLabel: "",
				}
			
				ns, err := f.CreateNamespace(context.TODO(), f.BaseName, l)
				o.Expect(err).NotTo(o.HaveOccurred())
				err = udnWaitForOpenShift(oc, ns.Name)
				o.Expect(err).NotTo(o.HaveOccurred())
				targetNamespace = ns.Name


				g.By("Creating a user defined network")
				nc := &networkAttachmentConfigParams{
					name:     "udn1",
					topology: "layer3",
					role:     "primary",
				}
				nc.cidr = correctCIDRFamily(oc, userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet)
				udnManifest := generateUserDefinedNetworkManifest(nc)
				err = applyManifest(targetNamespace, udnManifest)
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Eventually(userDefinedNetworkReadyFunc(oc.AdminDynamicClient(), targetNamespace, nc.name), 60*time.Second, time.Second).Should(o.Succeed())

				g.By("Labeling the UDN for advertisement")
				_, err = runOcWithRetry(oc.AsAdmin(), "label", "userdefinednetworks", "-n", targetNamespace, udnRaName, "advertise=true")
				o.Expect(err).NotTo(o.HaveOccurred())


				g.By("Create the route advertisement for UDN")
				raManifest := newRouteAdvertisementsManifest(udnRaName, false)
				err = applyManifest(targetNamespace, raManifest)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Makes sure that the RouteAdvertisements is accepted")
				waitErr := wait.PollUntilContextTimeout(context.TODO(), 10*time.Second, 30*time.Second, true, func(context.Context) (bool, error) {
					ra, err := getRouteAdvertisements(oc, udnRaName)
					if err != nil {
						return false, err
					}
					condition := meta.FindStatusCondition(ra.Status.Conditions, "Accepted")
					return condition.Status == metav1.ConditionTrue, nil
				})
				o.Expect(waitErr).NotTo(o.HaveOccurred())

				g.By("Makes sure the FRR configuration is generated for each node")
				for _, nodeName := range workerNodesOrderedNames {
					frr, err := getGeneratedFrrConfigurationForNode(oc, nodeName)
					o.Expect(err).NotTo(o.HaveOccurred())
					o.Expect(frr).NotTo(o.BeNil())
				}

				// SCC privileged is needed to run tcpdump on the packet sniffer containers, and at the minimum host networked is needed for
				// host networked pods.
				g.By("Adding SCC privileged to the external namespace")
				_, err = runOcWithRetry(oc.AsAdmin(), "adm", "policy", "add-scc-to-user", "privileged", fmt.Sprintf("system:serviceaccount:%s:default", externalNamespace))
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Selecting a node to act as as an external host")
				o.Expect(len(workerNodesOrderedNames)).Should(o.BeNumerically(">", 1))
				externalNodeName = workerNodesOrderedNames[0]
				advertisedPodsNodes = workerNodesOrderedNames[1:]

				g.By("Determining the interface that will be used for packet sniffing")
				packetSnifferInterface, err = findPacketSnifferInterface(oc, networkPlugin, []string{externalNodeName})
				o.Expect(err).NotTo(o.HaveOccurred())
				framework.Logf("Using interface %s for packet captures", packetSnifferInterface)

				g.By("Spawning the packet sniffer pods on the workers")
				packetSnifferDaemonSet, err = createPacketSnifferDaemonSet(oc, externalNamespace, advertisedPodsNodes, targetProtocol, targetPort, packetSnifferInterface)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Creating the test target deployment with number of advertisedPodsNodes")
				ingressDomain, err = getIngressDomain(oc)
				o.Expect(err).NotTo(o.HaveOccurred())
				deployName, routeName, err = createAgnhostDeploymentAndIngressRoute(oc, targetNamespace, "", ingressDomain, len(advertisedPodsNodes), advertisedPodsNodes)
				o.Expect(err).NotTo(o.HaveOccurred())

				podList, err = clientset.CoreV1().Pods(targetNamespace).List(context.TODO(), metav1.ListOptions{LabelSelector: fmt.Sprintf("app=%s", deployName)})
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(len(podList.Items)).To(o.Equal(len(advertisedPodsNodes)))

				g.By("Retrieving the Pod UDN IPs of the test deployment")
				podIPSet = make(map[string]string)

				var udnIP string
				nodeSubnets = make(map[string]string)
				for _, pod := range podList.Items {
					for i, cidr := range strings.Split(nc.cidr, ",") {
						if cidr != "" {
							g.By("asserting the pod has an UDN IP from the configured range")
							udnIP, err = podIPsForUserDefinedPrimaryNetwork(
								clientset,
								targetNamespace,
								pod.Name,
								namespacedName(targetNamespace, nc.name),
								i,
							)
							o.Expect(err).NotTo(o.HaveOccurred())
							const netPrefixLengthPerNode = 24
							g.By(fmt.Sprintf("asserting the pod UDN IP %v is from the configured range %v/%v", udnIP, cidr, netPrefixLengthPerNode))
							subnet, err := getNetCIDRSubnet(cidr)
							o.Expect(err).NotTo(o.HaveOccurred())
							o.Expect(inRange(subnet, udnIP)).To(o.Succeed())
							nodeSubnets[pod.Spec.NodeName] = subnet
							podIPSet[udnIP] = pod.Spec.NodeName
						}
					}
				}
			})

			g.JustAfterEach(func() {
			})

			g.It("nodes should learn BGP routes to the user defined network", func() {
				g.By("Checking that BGP routes to the PodNetwork are learned by other nodes")
				for _, nodeName := range workerNodesOrderedNames {
					bgpV4Routes, bgpV6Routes, err := getLearnedBgpRoutesByNode(oc, nodeName)
					o.Expect(err).NotTo(o.HaveOccurred())

					for node, subnet := range nodeSubnets {
						if node == nodeName {
							continue
						}
						_, v4 := bgpV4Routes[subnet]
						_, v6 := bgpV6Routes[subnet]
						o.Expect(v4 || v6).To(o.BeTrue())
					}
				}
			})

			g.It("pods should communicate with external host via UDN without being SNATed", func() {
				numberOfRequestsToSend := 10
				// The baremetal environment is built by dev-scripts, which runs a redfish http server at 192.168.111.1:8000. Use this as the external target host.
				externalHost := "192.168.111.1"

				g.By(fmt.Sprintf("Sending requests from prober and making sure that %d requests with search string and PodIP %v were seen", numberOfRequestsToSend, podIPSet))
				spawnProberSendEgressIPTrafficCheckLogs(oc, targetNamespace, probePodName, routeName, targetProtocol, externalHost, targetPort, numberOfRequestsToSend, numberOfRequestsToSend, packetSnifferDaemonSet, podIPSet)
			})

			g.It("External host should be able to quay route advertised pods by the pod UDN IP", func() {
				g.By("Launching a hostnetworked prober pod")
				nodeSelection := e2epod.NodeSelection{}
				e2epod.SetAffinity(&nodeSelection, externalNodeName)
				proberPod := createProberPod(oc, externalNamespace, bgpProbePodName, func(p *corev1.Pod) {
					e2epod.SetNodeSelection(&p.Spec, nodeSelection)
					p.Spec.HostNetwork = true
				})

				pod, err := clientset.CoreV1().Pods(externalNamespace).Get(context.TODO(), proberPod.Name, metav1.GetOptions{})
				o.Expect(err).NotTo(o.HaveOccurred())
				clientIP := pod.Status.PodIP

				for targetIP := range podIPSet {
					url := fmt.Sprintf("%s:%d", targetIP, targetPort)
					g.By(fmt.Sprintf("Probing for the target pod at %s", url))
					request := fmt.Sprintf("http://%s/clientip", url)
					output, err := runOcWithRetry(oc.AsAdmin(), "exec", "-n", proberPod.Namespace, proberPod.Name, "--", "curl", "-s", request)
 					o.Expect(err).NotTo(o.HaveOccurred())

					g.By("Making sure that probe pod IP presents in the response")
					clientIpPort := strings.Split(output, ":")
					o.Expect(clientIpPort).To(o.HaveLen(2))
					o.Expect(clientIpPort[0]).To(o.Equal(clientIP))

					g.By("Making sure that probe pod IP presents in the sniffer packet log")
					var found map[string]int
					err = wait.PollUntilContextTimeout(context.TODO(), 10*time.Second, 30*time.Second, true, func(context.Context) (bool, error) {
						found, _ = scanPacketSnifferDaemonSetPodLogs(oc, packetSnifferDaemonSet, targetProtocol, clientIP)
						if len(found) != 0 {
							framework.Logf("found: %v", found)
							return true, nil
						}
						return false, nil
					})
					o.Expect(err).NotTo(o.HaveOccurred())
					o.Expect(len(found)).To(o.BeNumerically(">", 0))
				}
				err = destroyProberPod(oc, proberPod)
				o.Expect(err).NotTo(o.HaveOccurred())
			})
		})

		g.Context("[EgressIP][apigroup:user.openshift.io][apigroup:security.openshift.io]", func() {
			g.JustBeforeEach(func() {
				// SCC privileged is needed to run tcpdump on the packet sniffer containers, and at the minimum host networked is needed for
				// host networked pods.
				g.By("Adding SCC privileged to the external namespace")
				_, err := runOcWithRetry(oc.AsAdmin(), "adm", "policy", "add-scc-to-user", "privileged", fmt.Sprintf("system:serviceaccount:%s:default", externalNamespace))
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Selecting a node to act as as an external host")
				o.Expect(len(workerNodesOrderedNames)).Should(o.BeNumerically(">", 1))
				externalNodeName = workerNodesOrderedNames[0]
				egressIPNodes = workerNodesOrderedNames[1:]

				g.By("Determining the interface that will be used for packet sniffing")
				packetSnifferInterface, err = findPacketSnifferInterface(oc, networkPlugin, []string{externalNodeName})
				o.Expect(err).NotTo(o.HaveOccurred())
				framework.Logf("Using interface %s for packet captures", packetSnifferInterface)

				g.By("Setting the EgressIP nodes as EgressIP assignable")
				for _, node := range egressIPNodes {
					_, err = runOcWithRetry(oc.AsAdmin(), "label", "node", node, "k8s.ovn.org/egress-assignable=")
					o.Expect(err).NotTo(o.HaveOccurred())
				}

				g.By("Turn on the BGP advertisement of EgressIPs")
				_, err = runOcWithRetry(oc.AsAdmin(), "patch", "ra", "default", "--type=merge", `-p={"spec":{"advertisements":["EgressIP","PodNetwork"]}}`)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Makes sure that the RouteAdvertisements is accepted")
				waitErr := wait.PollUntilContextTimeout(context.TODO(), 10*time.Second, 30*time.Second, true, func(context.Context) (bool, error) {
					ra, err := getRouteAdvertisements(oc, "default")
					if err != nil {
						return false, err
					}
					condition := meta.FindStatusCondition(ra.Status.Conditions, "Accepted")
					return condition.Status == metav1.ConditionTrue, nil
				})
				o.Expect(waitErr).NotTo(o.HaveOccurred())

				g.By("Makes sure the FRR configuration is generated for each node")
				for _, nodeName := range workerNodesOrderedNames {
					frr, err := getGeneratedFrrConfigurationForNode(oc, nodeName)
					o.Expect(err).NotTo(o.HaveOccurred())
					o.Expect(frr).NotTo(o.BeNil())
				}
				g.By("Spawning the packet sniffer pods on the workers")
				packetSnifferDaemonSet, err = createPacketSnifferDaemonSet(oc, externalNamespace, egressIPNodes, targetProtocol, targetPort, packetSnifferInterface)
				o.Expect(err).NotTo(o.HaveOccurred())
			})

			g.AfterEach(func() {
				g.By("Deleting the EgressIP object if it exists for OVN Kubernetes")
				egressIPYamlPath := tmpDirBGP + "/" + egressIPYaml
				if _, err := os.Stat(egressIPYamlPath); err == nil {
					_, _ = runOcWithRetry(oc.AsAdmin(), "delete", "-f", tmpDirBGP+"/"+egressIPYaml)
				}

				g.By("Removing the EgressIP assignable annotation for OVN Kubernetes")
				for _, nodeName := range egressIPNodes {
					_, _ = runOcWithRetry(oc.AsAdmin(), "label", "node", nodeName, "k8s.ovn.org/egress-assignable-")
				}

				g.By("Turn off the BGP advertisement of EgressIPs")
				_, err := runOcWithRetry(oc.AsAdmin(), "patch", "ra", "default", "--type=merge", `-p={"spec":{"advertisements":["PodNetwork"]}}`)
				o.Expect(err).NotTo(o.HaveOccurred())
			})

			g.It("pods should have the assigned EgressIPs and EgressIPs can be deleted and recreated [apigroup:route.openshift.io]", func() {
				g.By("Creating the EgressIP test source deployment with number of pods equals number of EgressIP nodes")
				ingressDomain, err := getIngressDomain(oc)
				o.Expect(err).NotTo(o.HaveOccurred())
				_, routeName, err := createAgnhostDeploymentAndIngressRoute(oc, targetNamespace, "", ingressDomain, len(egressIPNodes), egressIPNodes)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Choosing the EgressIPs to be assigned, one per node")
				egressIPSet := make(map[string]string)
				// With BGP, we can assign arbitrary EgressIPs to nodes.
				for i := 0; i < len(egressIPNodes); i++ {
					eip := fmt.Sprintf("192.168.111.%d", i+30)
					egressIPSet[eip] = egressIPNodes[i]
				}

				numberOfRequestsToSend := 10
				// The baremetal environment is built by dev-scripts, which runs a redfish http server at 192.168.111.1:8000. Use this as the external target host.
				targetHost := "192.168.111.1"
				egressIPYamlPath := tmpDirBGP + "/" + egressIPYaml
				egressIPObjectName := targetNamespace
				// Run this twice to make sure that repeated EgressIP creation and deletion works.
				for i := 0; i < 2; i++ {
					g.By("Creating the EgressIP object")
					ovnKubernetesCreateEgressIPObject(oc, egressIPYamlPath, egressIPObjectName, targetNamespace, "", egressIPSet)

					g.By("Applying the EgressIP object")
					applyEgressIPObject(oc, nil, egressIPYamlPath, targetNamespace, egressIPSet, egressUpdateTimeout)

					g.By("Makes sure the EgressIP is advertised by FRR")
					for eip, nodeName := range egressIPSet {
						o.Eventually(func() bool {
							frr, err := getGeneratedFrrConfigurationForNode(oc, nodeName)
							if err != nil {
								return false
							}
							for _, prefix := range frr.Spec.BGP.Routers[0].Prefixes {
								if prefix == fmt.Sprintf("%s/32", eip) {
									return true
								}
							}
							return false
						}).WithTimeout(30 * time.Second).WithPolling(5 * time.Second).Should(o.BeTrue())
					}

					g.By(fmt.Sprintf("Sending requests from prober and making sure that %d requests with search string and EgressIPs %v were seen", numberOfRequestsToSend, egressIPSet))
					spawnProberSendEgressIPTrafficCheckLogs(oc, externalNamespace, probePodName, routeName, targetProtocol, targetHost, targetPort, numberOfRequestsToSend, numberOfRequestsToSend, packetSnifferDaemonSet, egressIPSet)

					g.By("Deleting the EgressIP object")
					// Use cascading foreground deletion to make sure that the EgressIP object and its dependencies are gone.
					_, err = runOcWithRetry(oc.AsAdmin(), "delete", "egressip", egressIPObjectName, "--cascade=foreground")
					o.Expect(err).NotTo(o.HaveOccurred())

					g.By("Makes sure the EgressIP is not advertised by FRR")
					for eip, nodeName := range egressIPSet {
						o.Eventually(func() bool {
							frr, err := getGeneratedFrrConfigurationForNode(oc, nodeName)
							if err != nil {
								return true
							}
							for _, prefix := range frr.Spec.BGP.Routers[0].Prefixes {
								if prefix == fmt.Sprintf("%s/32", eip) {
									return true
								}
							}
							return false
						}).WithTimeout(30 * time.Second).WithPolling(5 * time.Second).Should(o.BeFalse())
					}

					g.By(fmt.Sprintf("Sending requests from prober and making sure that %d requests with search string and EgressIPs %v were seen", 0, egressIPSet))
					spawnProberSendEgressIPTrafficCheckLogs(oc, externalNamespace, probePodName, routeName, targetProtocol, targetHost, targetPort, numberOfRequestsToSend, 0, packetSnifferDaemonSet, egressIPSet)
				}
			})

			g.It("pods should keep the assigned EgressIPs when being rescheduled to another node", func() {
				g.By("Selecting a single EgressIP node, and a single start node for the pod")
				// requires a total of 3 worker nodes
				o.Expect(len(egressIPNodes)).Should(o.BeNumerically(">", 1))
				leftNode := egressIPNodes[0:1]
				rightNode := egressIPNodes[1:2]

				g.By(fmt.Sprintf("Creating the EgressIP test source deployment on node %s", rightNode[0]))
				ingressDomain, err := getIngressDomain(oc)
				o.Expect(err).NotTo(o.HaveOccurred())
				deploymentName, routeName, err := createAgnhostDeploymentAndIngressRoute(oc, targetNamespace, "", ingressDomain, len(rightNode), rightNode)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Choosing the EgressIPs to be assigned, one per node")
				egressIPSet := make(map[string]string)
				// With BGP, we can assign arbitrary EgressIPs to nodes.
				for i := 0; i < len(egressIPNodes); i++ {
					eip := fmt.Sprintf("192.168.111.%d", i+30)
					egressIPSet[eip] = egressIPNodes[i]
				}

				g.By("Creating the EgressIP object for OVN Kubernetes")
				egressIPYamlPath := tmpDirBGP + "/" + egressIPYaml
				egressIPObjectName := targetNamespace
				ovnKubernetesCreateEgressIPObject(oc, egressIPYamlPath, egressIPObjectName, targetNamespace, "", egressIPSet)

				g.By("Applying the EgressIP object for OVN Kubernetes")
				applyEgressIPObject(oc, nil, egressIPYamlPath, targetNamespace, egressIPSet, egressUpdateTimeout)

				g.By("Makes sure the EgressIP is advertised by FRR")
				for eip, nodeName := range egressIPSet {
					o.Eventually(func() bool {
						frr, err := getGeneratedFrrConfigurationForNode(oc, nodeName)
						if err != nil {
							return false
						}
						for _, prefix := range frr.Spec.BGP.Routers[0].Prefixes {
							if prefix == fmt.Sprintf("%s/32", eip) {
								return true
							}
						}
						return false
					}).WithTimeout(30 * time.Second).WithPolling(5 * time.Second).Should(o.BeTrue())
				}

				numberOfRequestsToSend := 10
				targetHost := "192.168.111.1"
				g.By(fmt.Sprintf("Sending requests from prober and making sure that %d requests with search string and EgressIPs %v were seen", numberOfRequestsToSend, egressIPSet))
				spawnProberSendEgressIPTrafficCheckLogs(oc, externalNamespace, probePodName, routeName, targetProtocol, targetHost, targetPort, numberOfRequestsToSend, numberOfRequestsToSend, packetSnifferDaemonSet, egressIPSet)

				g.By("Updating the source deployment's Affinity and moving it to the other source node")
				err = updateDeploymentAffinity(oc, targetNamespace, deploymentName, leftNode)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By(fmt.Sprintf("Sending requests from prober and making sure that %d requests with search string and EgressIPs %v were seen", numberOfRequestsToSend, egressIPSet))
				spawnProberSendEgressIPTrafficCheckLogs(oc, externalNamespace, probePodName, routeName, targetProtocol, targetHost, targetPort, numberOfRequestsToSend, numberOfRequestsToSend, packetSnifferDaemonSet, egressIPSet)
			})
		})
	})
})

func newRouteAdvertisementsManifest(name string, egressip bool) string {
	advertisements := []string{"PodNetwork"}
	if egressip {
		advertisements = append(advertisements, "EgressIP")
	}
	return fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: RouteAdvertisements
metadata:
  name: %s
spec:
  advertisements: [%s]
  networkSelector:
    matchLabels:
      advertise: "true"
`, name, strings.Join(advertisements, ","))
}

// getRouteAdvertisements uses the dynamic admin client to return a pointer to
// an existing RouteAdvertisements, or error.
func getRouteAdvertisements(oc *exutil.CLI, name string) (*ratypes.RouteAdvertisements, error) {
	dynamic := oc.AdminDynamicClient()
	unstructured, err := dynamic.Resource(ratypes.SchemeGroupVersion.WithResource("routeadvertisements")).Namespace("").Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	ra := &ratypes.RouteAdvertisements{}
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(unstructured.UnstructuredContent(), ra)
	if err != nil {
		return nil, err
	}
	return ra, nil
}

// getGeneratedFrrConfigurationForNode returns the FRR configuration for the node
func getGeneratedFrrConfigurationForNode(oc *exutil.CLI, nodeName string) (*frrapi.FRRConfiguration, error) {
	dynamic := oc.AdminDynamicClient()
	unstructuredList, err := dynamic.Resource(frrapi.SchemeGroupVersion.WithResource("frrconfigurations")).Namespace("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	frrList := &frrapi.FRRConfigurationList{}
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredList.UnstructuredContent(), frrList)
	if err != nil {
		return nil, err
	}
	for _, frr := range frrList.Items {
		if frr.Spec.NodeSelector.MatchLabels["kubernetes.io/hostname"] == nodeName {
			return &frr, nil
		}
	}
	return nil, fmt.Errorf("FRR configuration for node %s not found", nodeName)
}

func getNodeSubnets(oc *exutil.CLI, network string) (map[string][]net.IPNet, error) {
	// Run the oc command to get node subnets
	out, err := runOcWithRetry(oc.AsAdmin(), "get", "nodes", "-o",
		`jsonpath={range .items[*]}{.metadata.name}{"\t"}{.metadata.annotations.k8s\.ovn\.org/node-subnets}{"\n"}{end}`)
	if err != nil {
		return nil, fmt.Errorf("failed to get node subnets: %v", err)
	}

	// Create map to store results
	nodeSubnets := make(map[string][]net.IPNet)

	// Parse each line
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		// Split line into node name and subnet JSON
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		nodeName := parts[0]
		subnetJSON := parts[1]

		// Parse the JSON subnet data
		var subnetMap map[string][]string
		if err := json.Unmarshal([]byte(subnetJSON), &subnetMap); err != nil {
			return nil, fmt.Errorf("failed to parse subnet JSON for node %s: %v", nodeName, err)
		}

		// Extract subnets for the specified network
		if subnets, ok := subnetMap[network]; ok {
			ipNets := make([]net.IPNet, 0, len(subnets))
			for _, subnet := range subnets {
				_, ipNet, err := net.ParseCIDR(subnet)
				if err != nil {
					return nil, fmt.Errorf("failed to parse CIDR %q for node %s: %v", subnet, nodeName, err)
				}
				ipNets = append(ipNets, *ipNet)
			}
			nodeSubnets[nodeName] = ipNets
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error scanning output: %v", err)
	}

	return nodeSubnets, nil
}

// getLearnedBgpRoutesByNode returns the BGP routes learned by the node
func getLearnedBgpRoutesByNode(oc *exutil.CLI, nodeName string) (map[string]string, map[string]string, error) {
	var podName string
	var out string
	var err error
	var v4bgpRoutes, v6bgpRoutes map[string]string

	out, err = runOcWithRetry(oc.AsAdmin(), "get",
		"pods",
		"-o", "name",
		"-n", "openshift-frr-k8s",
		"--field-selector", fmt.Sprintf("spec.nodeName=%s", nodeName),
		"-l", "app=frr-k8s")
	if err != nil {
		return nil, nil, err
	}
	outReader := bufio.NewScanner(strings.NewReader(out))
	re := regexp.MustCompile("^pod/(.*)")
	for outReader.Scan() {
		match := re.FindSubmatch([]byte(outReader.Text()))
		if len(match) != 2 {
			continue
		}
		podName = string(match[1])
		break
	}
	if podName == "" {
		return nil, nil, fmt.Errorf("could not find a valid frr pod on node '%s'", nodeName)
	}
	out, err = adminExecInPod(oc, "openshift-frr-k8s", podName, "frr", "ip route show proto bgp")
	if err != nil {
		return nil, nil, err
	}
	v4bgpRoutes = parseRoutes(out)

	out, err = adminExecInPod(oc, "openshift-frr-k8s", podName, "frr", "ip -6 route show proto bgp")
	if err != nil {
		return nil, nil, err
	}
	v6bgpRoutes = parseRoutes(out)

	return v4bgpRoutes, v6bgpRoutes, nil
}

func parseRoutes(routeOutput string) map[string]string {
	routes := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(routeOutput))

	for scanner.Scan() {
		line := scanner.Text()
		// Extract CIDR and via address using regex for both IPv4 and IPv6
		re := regexp.MustCompile(`([\d\.]+/\d+|[a-fA-F0-9:]+/\d+).*via\s+([a-fA-F0-9:.]+)`)
		matches := re.FindStringSubmatch(line)
		if len(matches) == 3 {
			cidr := matches[1] // e.g., "10.128.0.0/23" or "fd01:0:0:1::/64"
			via := matches[2]  // e.g., "192.168.111.22" or "fd2e:6f44:5dd8:c956::16"

			routes[cidr] = via
		}
	}
	return routes
}
