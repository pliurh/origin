package sdnmigration

import (
	// "bytes"
	"context"
	// "encoding/json"
	"fmt"

	// "math/rand"
	"os"
	// "strconv"
	// "strings"
	"time"

	g "github.com/onsi/ginkgo"
	// configv1 "github.com/openshift/api/config/v1"
	configv1client "github.com/openshift/client-go/config/clientset/versioned"
	// "github.com/openshift/origin/pkg/synthetictests/platformidentification"
	"github.com/openshift/origin/test/e2e/upgrade/adminack"
	"github.com/openshift/origin/test/e2e/upgrade/alert"
	"github.com/openshift/origin/test/e2e/upgrade/dns"
	"github.com/openshift/origin/test/e2e/upgrade/manifestdelete"
	"github.com/openshift/origin/test/e2e/upgrade/service"
	"github.com/openshift/origin/test/extended/prometheus"
	"github.com/openshift/origin/test/extended/util/disruption"
	"github.com/openshift/origin/test/extended/util/disruption/imageregistry"
	"github.com/openshift/origin/test/extended/util/operator"

	// "github.com/pborman/uuid"
	// v1 "k8s.io/api/core/v1"
	// eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	// "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	// "k8s.io/client-go/util/retry"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/upgrades"
	"k8s.io/kubernetes/test/e2e/upgrades/apps"
	"k8s.io/kubernetes/test/e2e/upgrades/node"
)

// NoTests is an empty list of tests
func NoTests() []upgrades.Test {
	return []upgrades.Test{}
}

// AllTests includes all tests (minimal + disruption)
func AllTests() []upgrades.Test {
	return []upgrades.Test{
		&adminack.UpgradeTest{},
		&manifestdelete.UpgradeTest{},
		&alert.UpgradeTest{},

		// These two tests require complex setup and thus are a poor fit for our current invariant/synthetic
		// disruption tests, so they remain separate. They output AdditionalEvents json files as artifacts which
		// are merged into our main e2e-events.
		service.NewServiceLoadBalancerWithNewConnectionsTest(),
		service.NewServiceLoadBalancerWithReusedConnectionsTest(),
		imageregistry.NewImageRegistryAvailableWithNewConnectionsTest(),
		imageregistry.NewImageRegistryAvailableWithReusedConnectionsTest(),

		&node.SecretUpgradeTest{},
		&apps.ReplicaSetUpgradeTest{},
		&apps.StatefulSetUpgradeTest{},
		&apps.DeploymentUpgradeTest{},
		&apps.JobUpgradeTest{},
		&node.ConfigMapUpgradeTest{},
		&apps.DaemonSetUpgradeTest{},
		&prometheus.ImagePullsAreFast{},
		&prometheus.MetricsAvailableAfterUpgradeTest{},
		&dns.UpgradeTest{},
	}
}

var (
	sdnMigrationTests          = []upgrades.Test{}
	upgradeAbortAt             int
	upgradeDisruptRebootPolicy string
)

const defaultCNOUpdateAckTimeout = 3 * time.Minute

// SetTests controls the list of tests to run during an upgrade. See AllTests for the supported
// suite.
func SetTests(tests []upgrades.Test) {
	sdnMigrationTests = tests
}

func SetSdnMigrationDisruptReboot(policy string) error {
	switch policy {
	case "graceful", "force":
		upgradeDisruptRebootPolicy = policy
		return nil
	default:
		upgradeDisruptRebootPolicy = ""
		return fmt.Errorf("disrupt-reboot must be empty, 'graceful', or 'force'")
	}
}


var _ = g.Describe("[sig-network][Feature:ClusterSdnMigration][Migration]", func() {
	f := framework.NewDefaultFramework("cluster-sdn-migration")
	f.SkipNamespaceCreation = true
	f.SkipPrivilegedPSPBinding = true

	g.It("Cluster should remain functional during sdn migration [Disruptive]", func() {
		config, err := framework.LoadConfig()
		framework.ExpectNoError(err)
		client := configv1client.NewForConfigOrDie(config)
		dynamicClient := dynamic.NewForConfigOrDie(config)

		disruption.Run(f, "Cluster sdn migration", "migration",
			disruption.TestData{},
			sdnMigrationTests,
			func() {
				framework.ExpectNoError(clusterSdnMigration(f, client, dynamicClient, config, "OVNKubernetes", true), "during sdn migration")
			},
		)
	})
})

var _ = g.Describe("[sig-network][Feature:ClusterSdnMigration][Rollback]", func() {
	f := framework.NewDefaultFramework("cluster-sdn-migration")
	f.SkipNamespaceCreation = true
	f.SkipPrivilegedPSPBinding = true

	g.It("Cluster should remain functional during sdn migration rollback [Disruptive]", func() {
		config, err := framework.LoadConfig()
		framework.ExpectNoError(err)
		client := configv1client.NewForConfigOrDie(config)
		dynamicClient := dynamic.NewForConfigOrDie(config)

		disruption.Run(f, "Cluster sdn migration", "rollback",
			disruption.TestData{},
			sdnMigrationTests,
			func() {
				framework.ExpectNoError(clusterSdnMigration(f, client, dynamicClient, config, "OpenShiftSDN", true), "during sdn migration")
			},
		)
	})
})


var errControlledAbort = fmt.Errorf("beginning abort")

// PreUpgradeResourceCounts stores a map of resource type to a count of the number of
// resources of that type in the entire cluster, gathered prior to launching the upgrade.
var PreUpgradeResourceCounts = map[string]int{}

func GatherPreSdnMigrationResourceCounts() error {
	config, err := framework.LoadConfig(true)
	if err != nil {
		return err
	}
	kubeClient := kubernetes.NewForConfigOrDie(config)
	// Store resource counts we're interested in monitoring from before upgrade to after.
	// Used to test for excessive resource growth during upgrade in the invariants.
	ctx := context.Background()
	secrets, err := kubeClient.CoreV1().Secrets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	PreUpgradeResourceCounts["secrets"] = len(secrets.Items)
	framework.Logf("found %d Secrets prior to upgrade at %s\n", len(secrets.Items),
		time.Now().UTC().Format(time.RFC3339))

	configMaps, err := kubeClient.CoreV1().ConfigMaps("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	PreUpgradeResourceCounts["configmaps"] = len(configMaps.Items)
	framework.Logf("found %d ConfigMaps prior to upgrade at %s\n", len(configMaps.Items),
		time.Now().UTC().Format(time.RFC3339))
	return nil
}

func clusterSdnMigration(f *framework.Framework, c configv1client.Interface, dc dynamic.Interface, config *rest.Config, target string, isLive bool) error {
	fmt.Fprintf(os.Stderr, "\n\n\n")
	defer func() { fmt.Fprintf(os.Stderr, "\n\n\n") }()

	// kubeClient := kubernetes.NewForConfigOrDie(config)

	// this is very long.  We should update the clusteroperator junit to give us a duration.
	// maximumDuration := 150 * time.Minute
	// baseDurationToSoftFailure := 75 * time.Minute
	// durationToSoftFailure := baseDurationToSoftFailure

	// infra, err := c.ConfigV1().Infrastructures().Get(context.Background(), "cluster", metav1.GetOptions{})
	// framework.ExpectNoError(err)
	// network, err := c.ConfigV1().Networks().Get(context.Background(), "cluster", metav1.GetOptions{})
	// framework.ExpectNoError(err)
	// platformType, err := platformidentification.GetJobType(context.TODO(), config)
	// framework.ExpectNoError(err)

	// switch {
	// case infra.Status.PlatformStatus.Type == configv1.AWSPlatformType:
	// 	// due to https://bugzilla.redhat.com/show_bug.cgi?id=1943804 upgrades take ~12 extra minutes on AWS
	// 	// and see commit d69db34a816f3ce8a9ab567621d145c5cd2d257f which notes that some AWS upgrades can
	// 	// take close to 105 minutes total (75 is base duration, so adding 30 more if it's AWS)
	// 	durationToSoftFailure = baseDurationToSoftFailure + (30 * time.Minute)

	// case network.Status.NetworkType == "OVNKubernetes":
	// 	// if the cluster is on AWS we've already bumped the timeout enough, but if not we need to check if
	// 	// the CNI is OVN and increase our timeout for that
	// 	// For now, deploying with OVN is expected to take longer. on average, ~15m longer
	// 	// some extra context to this increase which links to a jira showing which operators take longer:
	// 	// compared to OpenShiftSDN:
	// 	//   https://bugzilla.redhat.com/show_bug.cgi?id=1942164
	// 	durationToSoftFailure = baseDurationToSoftFailure + (15 * time.Minute)

	// case platformType.Architecture == platformidentification.ArchitectureS390:
	// 	// s390 appears to take nearly 100 minutes to upgrade. Not sure why, but let's keep it from getting worse and provide meaningful
	// 	// test results.
	// 	durationToSoftFailure = 100 * time.Minute

	// case platformType.Architecture == platformidentification.ArchitecturePPC64le:
	// 	// ppc appears to take just over 75 minutes. Not sure why, but let's keep it from getting worse and provide meaningful
	// 	// test results.
	// 	durationToSoftFailure = 80 * time.Minute
	// }

	// //used below in separate paths
	// clusterCompletesSdnMigrationTestName := "[sig-network] Cluster completes sdn migration"
	cnoAckTimeout := 1 * time.Minute

	// trigger the update and record verification as an independent step
	if err := disruption.RecordJUnit(
		f,
		"[sig-network] Cluster network operator prepare sdn migration",
		func() (error, bool) {
			netResource := dc.Resource(schema.GroupVersionResource{
				Group:    "operator.openshift.io",
				Version:  "v1",
				Resource: "networks",
			})
			net, err := netResource.Get(context.Background(), "cluster", metav1.GetOptions{})
			if err != nil {
				return err, false
			}
			original := net.Object["spec"].(map[string]interface{})["defaultNetwork"].(map[string]interface{})["type"]

			patch := []byte(fmt.Sprintf("{\"spec\":{\"migration\":{\"networkType\":\"%s\",\"isLive\":%v}}}", target, isLive))
			framework.Logf("patch: %v", string(patch))
			net, err = netResource.Patch(context.Background(), "cluster", types.MergePatchType, patch, metav1.PatchOptions{})
			if err != nil {
				return err, false
			}

			start := time.Now()
			// wait until the cluster acknowledges the update
			if err := wait.PollImmediate(5*time.Second, cnoAckTimeout, func() (bool, error) {
				n, err := c.ConfigV1().Networks().Get(context.Background(), "cluster", metav1.GetOptions{})
				if err != nil || n == nil {
					return false, err
				}
				if n.Status.Migration != nil {
					return n.Status.Migration.NetworkType == original, nil
				}
				return false, nil
			}); err != nil {
				return fmt.Errorf("timed out waiting for CNO to start SDN migration"), false
			}

			if err := operator.WaitForOperatorToSettle(context.TODO(), c, "network"); err != nil {
				return err, false
			}

			timeToAck := time.Now().Sub(start)
			if timeToAck > defaultCNOUpdateAckTimeout {
				return fmt.Errorf("CNO took %s to acknowledge migration (> %s), flaking test", timeToAck, defaultCNOUpdateAckTimeout), true
			}
			return nil, false
		},
	); err != nil {
		return err
	}

	var errMasterUpdating error
	if err := disruption.RecordJUnit(
		f,
		"[sig-mco] Machine config pools complete upgrade",
		func() (error, bool) {
			framework.Logf("CNO start SDN migration")

			patch := []byte(fmt.Sprintf("{\"spec\":{\"networkType\": \"%s\"}}", target))

			_, err := c.ConfigV1().Networks().Patch(context.Background(), "cluster", types.MergePatchType, patch, metav1.PatchOptions{})
			if err != nil {
				return err, false
			}

			if err := wait.PollImmediate(5*time.Second, cnoAckTimeout, func() (bool, error) {
				n, err := c.ConfigV1().Networks().Get(context.Background(), "cluster", metav1.GetOptions{})
				if err != nil || n == nil {
					return false, err
				}
				if n.Status.Migration != nil {
					return n.Status.Migration.NetworkType == target, nil
				}
				return false, nil
			}); err != nil {
				return fmt.Errorf("timed out waiting for CNO to start SDN migration"), false
			}

			framework.Logf("Waiting on pools update to be started")
			if err := wait.PollImmediate(10*time.Second, 30*time.Minute, func() (bool, error) {
				mcps := dc.Resource(schema.GroupVersionResource{
					Group:    "machineconfiguration.openshift.io",
					Version:  "v1",
					Resource: "machineconfigpools",
				})
				pools, err := mcps.List(context.Background(), metav1.ListOptions{})
				if err != nil {
					framework.Logf("error getting pools %v", err)
					return false, nil
				}
				allUpdating := true
				for _, p := range pools.Items {
					_, updating := IsPoolUpdated(mcps, p.GetName())
					allUpdating = allUpdating && updating

				}
				return allUpdating, nil
			}); err != nil {
				return fmt.Errorf("Pools did not complete upgrade: %v", err), false
			}

			framework.Logf("Waiting on pools to be upgraded")
			if err := wait.PollImmediate(10*time.Second, 30*time.Minute, func() (bool, error) {
				mcps := dc.Resource(schema.GroupVersionResource{
					Group:    "machineconfiguration.openshift.io",
					Version:  "v1",
					Resource: "machineconfigpools",
				})
				pools, err := mcps.List(context.Background(), metav1.ListOptions{})
				if err != nil {
					framework.Logf("error getting pools %v", err)
					return false, nil
				}
				allUpdated := true
				for _, p := range pools.Items {
					updated, requiresUpdate := IsPoolUpdated(mcps, p.GetName())
					allUpdated = allUpdated && updated

					// Invariant: when CVO reaches level, MCO is required to have rolled out control plane updates
					if p.GetName() == "master" && requiresUpdate && errMasterUpdating == nil {
						errMasterUpdating = fmt.Errorf("the %q pool should be updated before the CVO reports available at the new version", p.GetName())
						framework.Logf("Invariant violation detected: %s", errMasterUpdating)
					}
				}
				return allUpdated, nil
			}); err != nil {
				return fmt.Errorf("pools did not complete upgrade: %v", err), false
			}
			framework.Logf("All pools completed upgrade")
			return nil, false
		},
	); err != nil {
		return err
	}

	// trigger the cleanup
	if err := disruption.RecordJUnit(
		f,
		"[sig-network] Cluster network operator start sdn migration cleanup",
		func() (error, bool) {
			if err := operator.WaitForOperatorsToSettle(context.TODO(), c); err != nil {
				return err, false
			}

			netResource := dc.Resource(schema.GroupVersionResource{
				Group:    "operator.openshift.io",
				Version:  "v1",
				Resource: "networks",
			})

			patch := []byte(`{"spec":{"migration":null}}`)
			framework.Logf("patch: %v", string(patch))
			_, err := netResource.Patch(context.Background(), "cluster", types.MergePatchType, patch, metav1.PatchOptions{})
			if err != nil {
				return err, false
			}
			// wait until the CNO acknowledges the cleanup
			if err := wait.PollImmediate(5*time.Second, cnoAckTimeout, func() (bool, error) {
				n, err := c.ConfigV1().Networks().Get(context.Background(), "cluster", metav1.GetOptions{})
				if err != nil || n == nil {
					return false, err
				}
				return n.Status.Migration == nil, nil
			}); err != nil {
				return fmt.Errorf("timed out waiting for CNO to start SDN migration cleanup"), false
			}

			if err := operator.WaitForOperatorToSettle(context.TODO(), c, "network"); err != nil {
				return err, false
			}
			return nil, false
		}); err != nil {
		return err
	}


	if err := disruption.RecordJUnit(
		f,
		"[sig-network] ClusterOperators are available and not degraded after sdn migration",
		func() (error, bool) {
			if err := operator.WaitForOperatorsToSettle(context.TODO(), c); err != nil {
				return err, false
			}
			return nil, false
		},
	); err != nil {
		return err
	}

	return nil
}

// TODO(runcom): drop this when MCO types are in openshift/api and we can use the typed client directly
func IsPoolUpdated(dc dynamic.NamespaceableResourceInterface, name string) (poolUpToDate bool, poolIsUpdating bool) {
	pool, err := dc.Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		framework.Logf("error getting pool %s: %v", name, err)
		return false, false
	}

	paused, found, err := unstructured.NestedBool(pool.Object, "spec", "paused")
	if err != nil || !found {
		return false, false
	}

	conditions, found, err := unstructured.NestedFieldNoCopy(pool.Object, "status", "conditions")
	if err != nil || !found {
		return false, false
	}
	original, ok := conditions.([]interface{})
	if !ok {
		return false, false
	}
	var updated, updating, degraded bool
	for _, obj := range original {
		o, ok := obj.(map[string]interface{})
		if !ok {
			return false, false
		}
		t, found, err := unstructured.NestedString(o, "type")
		if err != nil || !found {
			return false, false
		}
		s, found, err := unstructured.NestedString(o, "status")
		if err != nil || !found {
			return false, false
		}
		if t == "Updated" && s == "True" {
			updated = true
		}
		if t == "Updating" && s == "True" {
			updating = true
		}
		if t == "Degraded" && s == "True" {
			degraded = true
		}
	}
	if paused {
		framework.Logf("Pool %s is paused, treating as up-to-date (Updated: %v, Updating: %v, Degraded: %v)", name, updated, updating, degraded)
		return true, updating
	}
	if updated && !updating && !degraded {
		return true, updating
	}
	framework.Logf("Pool %s is still reporting (Updated: %v, Updating: %v, Degraded: %v)", name, updated, updating, degraded)
	return false, updating
}
