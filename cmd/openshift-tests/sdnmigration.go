package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/openshift/origin/pkg/synthetictests"
	"github.com/openshift/origin/pkg/test/ginkgo"
	"github.com/openshift/origin/test/e2e/sdnmigration"
	exutil "github.com/openshift/origin/test/extended/util"
	"github.com/pkg/errors"
	"github.com/spf13/pflag"
	"k8s.io/klog/v2"
	"k8s.io/kubectl/pkg/util/templates"
	"k8s.io/kubernetes/test/e2e/upgrades"
)

// sdnMigrationSuites are all known sdn migration test suites this binary should run
var sdnMigrationSuites = testSuites{
	{
		TestSuite: ginkgo.TestSuite{
			Name: "all",
			Description: templates.LongDesc(`
		Run all tests.
		`),
			Matches: func(name string) bool {
				if isStandardEarlyTest(name) {
					return true
				}
				return strings.Contains(name, "[Feature:ClusterSdnMigration]") && !strings.Contains(name, "[Suite:k8s]")
			},
			TestTimeout:         240 * time.Minute,
			SyntheticEventTests: ginkgo.JUnitForEventsFunc(synthetictests.SystemUpgradeEventInvariants),
		},
		PreSuite: sdnMigrationTestPreSuite,
	},
	{
		TestSuite: ginkgo.TestSuite{
			Name: "migration",
			Description: templates.LongDesc(`
		Run only the tests that verify the platform remains available.
		`),
			Matches: func(name string) bool {
				if isStandardEarlyTest(name) {
					return true
				}
				return strings.Contains(name, "[Feature:ClusterSdnMigration][Migration]") && !strings.Contains(name, "[Suite:k8s]")
			},
			TestTimeout:         240 * time.Minute,
			SyntheticEventTests: ginkgo.JUnitForEventsFunc(synthetictests.SystemUpgradeEventInvariants),
		},
		PreSuite: sdnMigrationTestPreSuite,
	},
	{
		TestSuite: ginkgo.TestSuite{
			Name: "rollback",
			Description: templates.LongDesc(`
		Run only the tests that verify the platform remains available.
		`),
			Matches: func(name string) bool {
				if isStandardEarlyTest(name) {
					return true
				}
				return strings.Contains(name, "[Feature:ClusterSdnMigration][Rollback]") && !strings.Contains(name, "[Suite:k8s]")
			},
			TestTimeout:         240 * time.Minute,
			SyntheticEventTests: ginkgo.JUnitForEventsFunc(synthetictests.SystemUpgradeEventInvariants),
		},
		PreSuite: sdnMigrationTestPreSuite,
	},
	{
		TestSuite: ginkgo.TestSuite{
			Name: "none",
			Description: templates.LongDesc(`
	Don't run disruption tests.
		`),
			Matches: func(name string) bool {
				if isStandardEarlyTest(name) {
					return true
				}
				return strings.Contains(name, "[Feature:ClusterSdnMigration]") && !strings.Contains(name, "[Suite:k8s]")
			},
			TestTimeout:         240 * time.Minute,
			// SyntheticEventTests: ginkgo.JUnitForEventsFunc(synthetictests.SystemSdnMigrationEventInvariants),
		},
		PreSuite: sdnMigrationTestPreSuite,
	},
}

// sdnMigrationTestPreSuite validates the test options and gathers data useful prior to launching the sdn migration and it's
// related tests.
func sdnMigrationTestPreSuite(opt *runOptions) error {
	if !opt.DryRun {
		testOpt := ginkgo.NewTestOptions(os.Stdout, os.Stderr)
		config, err := decodeProvider(os.Getenv("TEST_PROVIDER"), testOpt.DryRun, false, nil)
		if err != nil {
			return err
		}
		if err := initializeTestFramework(exutil.TestContext, config, testOpt.DryRun); err != nil {
			return err
		}
		klog.V(4).Infof("Loaded test configuration: %#v", exutil.TestContext)

		if err := sdnmigration.GatherPreSdnMigrationResourceCounts(); err != nil {
			return errors.Wrap(err, "error gathering preupgrade resource counts")
		}
	}

	// SdnMigration test output is important for debugging because it shows linear progress
	// and when the CVO hangs.
	opt.IncludeSuccessOutput = true
	return parseSdnMigrationOptions(opt.TestOptions)
}

// upgradeTestPreTest uses variables set at suite execution time to prepare the sdn migration
// test environment in process (setting constants in the sdn migration packages).
func sdnMigrationTestPreTest() error {
	value := os.Getenv("TEST_UPGRADE_OPTIONS")
	if len(value) == 0 {
		return nil
	}

	var opt SdnMigrationOptions
	if err := json.Unmarshal([]byte(value), &opt); err != nil {
		return err
	}
	parseSdnMigrationOptions(opt.TestOptions)
	switch opt.Suite {
	case "none":
		return filterSdnMigration(sdnmigration.NoTests(), func(string) bool { return true })
	case "platform":
		return filterSdnMigration(sdnmigration.AllTests(), func(name string) bool {
			return false
		})
	default:
		return filterSdnMigration(sdnmigration.AllTests(), func(string) bool { return true })
	}
}

func parseSdnMigrationOptions(options []string) error {
	for _, opt := range options {
		parts := strings.SplitN(opt, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("expected option of the form KEY=VALUE instead of %q", opt)
		}
		switch parts[0] {
		// case "abort-at":
		// 	if err := sdnmigration.SetSdnMigrationAbortAt(parts[1]); err != nil {
		// 		return err
		// 	}
		case "disrupt-reboot":
			if err := sdnmigration.SetSdnMigrationDisruptReboot(parts[1]); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unrecognized sdn migration option: %s", parts[0])
		}
	}
	return nil
}

type SdnMigrationOptions struct {
	Suite       string
	ToImage     string
	TestOptions []string
}

func (o *SdnMigrationOptions) ToEnv() string {
	out, err := json.Marshal(o)
	if err != nil {
		panic(err)
	}
	return string(out)
}

func filterSdnMigration(tests []upgrades.Test, match func(string) bool) error {
	var scope []upgrades.Test
	for _, test := range tests {
		if match(test.Name()) {
			scope = append(scope, test)
		}
	}
	sdnmigration.SetTests(scope)
	return nil
}

func bindSdnMigrationOptions(opt *runOptions, flags *pflag.FlagSet) {
	flags.StringSliceVar(&opt.TestOptions, "options", opt.TestOptions, "A set of KEY=VALUE options to control the test. See the help text.")
}
