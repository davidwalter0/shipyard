package cmd

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/DATA-DOG/godog"
	"github.com/DATA-DOG/godog/colors"
	"github.com/DATA-DOG/godog/gherkin"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/hashicorp/go-hclog"
	"github.com/shipyard-run/shipyard/pkg/clients"
	"github.com/shipyard-run/shipyard/pkg/shipyard"
	"github.com/shipyard-run/shipyard/pkg/utils"
	"github.com/spf13/cobra"
)

func newTestCmd(e shipyard.Engine, bp clients.Getter, hc clients.HTTP, bc clients.System, l hclog.Logger) *cobra.Command {
	var testFolder string
	var force bool
	var testCmd = &cobra.Command{
		Use:                   "test [blueprint]",
		Short:                 "Run functional tests for the blueprint",
		Long:                  `Run functional tests for the blueprint, this command will start the shipyard blueprint `,
		DisableFlagsInUseLine: true,
		Args:                  cobra.ArbitraryArgs,
		RunE:                  newTestCmdFunc(e, bp, hc, bc, testFolder, &force, l),
	}

	testCmd.Flags().StringVarP(&testFolder, "test-folder", "", "./functional_tests", "Specify the folder containing the functional tests.")
	testCmd.Flags().BoolVarP(&force, "force-update", "", false, "When set to true Shipyard will ignore cached images or files and will download all resources")

	return testCmd
}

func newTestCmdFunc(e shipyard.Engine, bp clients.Getter, hc clients.HTTP, bc clients.System, testFolder string, force *bool, l hclog.Logger) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		//

		tr := CucumberRunner{cmd, args, e, bp, hc, bc, testFolder, force, l}
		tr.start()

		return nil
	}
}

var opt = godog.Options{
	Output: colors.Colored(os.Stdout),
	Format: "progress", // can define default values
}

// CucumberRunner is a test runner for cucumber tests
type CucumberRunner struct {
	cmd        *cobra.Command
	args       []string
	e          shipyard.Engine
	bp         clients.Getter
	hc         clients.HTTP
	bc         clients.System
	testFolder string
	force      *bool
	l          hclog.Logger
}

// Initialize the functional tests
func (cr *CucumberRunner) start() {
	godog.BindFlags("godog.", flag.CommandLine, &opt)
	flag.Parse()

	format := "pretty"
	// the tests will be in the blueprint_folder/test
	testFolder := fmt.Sprintf("%s/test", cr.args[0])

	status := godog.RunWithOptions("godog", func(s *godog.Suite) {
		cr.featureContext(s)
	}, godog.Options{
		Format: format,
		Paths:  []string{testFolder},
	})

	os.Exit(status)
}

func (cr *CucumberRunner) featureContext(s *godog.Suite) {
	s.Step(`^I have a running blueprint$`, cr.iRunApply)
	s.Step(`^there should be a "([^"]*)" running called "([^"]*)"$`, cr.thereShouldBeAResourceRunningCalled)
	s.Step(`^a HTTP call to "([^"]*)" should result in status (\d+)$`, cr.aCallToShouldResultInStatus)
	s.Step(`^the following resources should be running$`, cr.theFollowingResourcesShouldBeRunning)

	s.BeforeScenario(func(interface{}) {
	})

	s.AfterScenario(func(i interface{}, err error) {
		fmt.Println("")
		cr.e.Destroy("", true)

		if err != nil {
			fmt.Println(writer.String())
		}

		// purge the cache
		//cmd = exec.Command("yard-dev", []string{"purge"}...)
		//cmd.Stdout = os.Stdout
		//cmd.Stderr = os.Stderr
		//cmd.Run()
	})
}

var writer = bytes.NewBufferString("")

func (cr *CucumberRunner) iRunApply() error {
	// start the blueprint
	noOpen := true

	opts := &hclog.LoggerOptions{}

	// set the log level
	opts.Level = hclog.Debug
	if lev := os.Getenv("LOG_LEVEL"); lev != "" {
		opts.Level = hclog.LevelFromString(lev)
	}

	// if the log level is debug print it to the output
	if os.Getenv("LOG_LEVEL") != "debug" {
		// capture output to a string
		opts.Output = writer

		cr.l = hclog.New(opts)
		engine, err := shipyard.New(cr.l)
		if err != nil {
			panic(err)
		}

		cr.e = engine

		// re-use the run command
		rc := newRunCmdFunc(
			engine,
			engine.GetClients().Getter,
			engine.GetClients().HTTP,
			engine.GetClients().Browser,
			&noOpen,
			cr.force,
			cr.l,
		)

		cr.cmd.SetOut(writer)

		err = rc(cr.cmd, cr.args)
		if err != nil {
			fmt.Println(writer.String())
		}
		return err
	}

	return nil
}

func (cr *CucumberRunner) theFollowingResourcesShouldBeRunning(arg1 *gherkin.DataTable) error {
	for i, r := range arg1.Rows {
		if i == 0 {
			if r.Cells[0].Value != "name" || r.Cells[1].Value != "type" {
				return fmt.Errorf("Tables should be formatted with a header row containing the columns 'name' and 'type'")
			}

			continue
		}

		if len(r.Cells) != 2 {
			return fmt.Errorf("Table rows should have two columns 'name' and 'type'")
		}

		if r.Cells[1].Value == "network" {
			err := cr.thereShouldBe1NetworkCalled(r.Cells[0].Value)
			if err != nil {
				return err
			}
		} else {

			err := cr.thereShouldBeAResourceRunningCalled(r.Cells[1].Value, r.Cells[0].Value)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (cr *CucumberRunner) thereShouldBeAResourceRunningCalled(resource string, name string) error {
	fqdn := utils.FQDN(name, resource)

	// a container can start immediately and then it can crash, this can cause a false positive for the test
	// wait a few seconds to ensure the state does not change
	time.Sleep(5 * time.Second)

	// we need to check this a number of times to make sure it is not just a slow starting container
	for i := 0; i < 100; i++ {
		args := filters.NewArgs()
		args.Add("name", fqdn)
		opts := types.ContainerListOptions{Filters: args, All: true}

		cl, err := cr.e.GetClients().Docker.ContainerList(context.Background(), opts)
		if err != nil {
			return err
		}

		if len(cl) == 1 {
			// check to see if the container has failed
			if cl[0].State == "exited" {
				return fmt.Errorf("container exited prematurely")
			}

			if cl[0].State == "running" {
				return nil
			}
		}

		// wait a few seconds before trying again
		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("Expected %d $s %s", 1, resource, name)
}

func (cr *CucumberRunner) thereShouldBe1NetworkCalled(arg1 string) error {
	args := filters.NewArgs()
	args.Add("name", arg1)
	n, err := cr.e.GetClients().Docker.NetworkList(context.Background(), types.NetworkListOptions{Filters: args})

	if err != nil {
		return err
	}

	if len(n) != 1 {
		return fmt.Errorf("Expected 1 network called %s to be created", arg1)
	}

	return nil
}

// test making a HTTP call, for testing Ingress
func (cr *CucumberRunner) aCallToShouldResultInStatus(arg1 string, arg2 int) error {
	// try 100 times
	var err error
	for i := 0; i < 200; i++ {
		var resp *http.Response
		resp, err = http.Get(arg1)

		if err == nil && resp.StatusCode == arg2 {
			return nil
		}

		if err == nil {
			err = fmt.Errorf("Expected status code %d, got %d", arg2, resp.StatusCode)
		}

		time.Sleep(2 * time.Second)
	}

	return err
}
