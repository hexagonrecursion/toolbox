/*
 * Copyright © 2019 – 2020 Red Hat Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/containers/toolbox/pkg/utils"
	"github.com/containers/toolbox/pkg/podman"
	"github.com/containers/toolbox/pkg/shell"
	"github.com/spf13/cobra"
	"github.com/sirupsen/logrus"
)

var (
	enterFlags struct {
		container string
		release   string
	}
)

var enterCmd = &cobra.Command{
	Use:   "enter",
	Short: "Enter a toolbox container for interactive use",
	RunE:  enter,
}

func init() {
	flags := enterCmd.Flags()

	flags.StringVarP(&enterFlags.container,
		"container",
		"c",
		"",
		"Enter a toolbox container with the given name.")

	flags.StringVarP(&enterFlags.release,
		"release",
		"r",
		"",
		"Enter a toolbox container for a different operating system release than the host.")

	enterCmd.SetHelpFunc(enterHelp)
	rootCmd.AddCommand(enterCmd)
}

func enter(cmd *cobra.Command, args []string) error {
	if utils.IsInsideContainer() {
		if !utils.IsInsideToolboxContainer() {
			return errors.New("this is not a toolbox container")
		}

		if _, err := utils.ForwardToHost(); err != nil {
			return err
		}

		return nil
	}

	var container string
	var containerArg string
	var nonDefaultContainer bool

	if len(args) != 0 {
		container = args[0]
		containerArg = "CONTAINER"
	} else if enterFlags.container != "" {
		container = enterFlags.container
		containerArg = "--container"
	}

	if container != "" {
		nonDefaultContainer = true

		if _, err := utils.IsContainerNameValid(container); err != nil {
			var builder strings.Builder
			fmt.Fprintf(&builder, "invalid argument for '%s'\n", containerArg)
			fmt.Fprintf(&builder, "Container names must match '%s'\n", utils.ContainerNameRegexp)
			fmt.Fprintf(&builder, "Run '%s --help' for usage.", executableBase)

			errMsg := builder.String()
			return errors.New(errMsg)
		}
	}

	var release string
	if enterFlags.release != "" {
		nonDefaultContainer = true

		var err error
		release, err = utils.ParseRelease(enterFlags.release)
		if err != nil {
			err := utils.CreateErrorInvalidRelease(executableBase)
			return err
		}
	}

	container, image, release, err := utils.ResolveContainerAndImageNames(container, "", release)
	if err != nil {
		return err
	}

	userShell := os.Getenv("SHELL")
	if userShell == "" {
		return errors.New("failed to get the current user's default shell")
	}

	command := []string{userShell, "-l"}

	hostID, err := utils.GetHostID()
	if err != nil {
		return errors.New("failed to get the host ID")
	}

	hostVariantID, err := utils.GetHostVariantID()
	if err != nil {
		return errors.New("failed to get the host VARIANT_ID")
	}

	var emitEscapeSequence bool

	if hostID == "fedora" && (hostVariantID == "silverblue" || hostVariantID == "workstation") {
		emitEscapeSequence = true
	}

	if err := enterContainer(container,
		!nonDefaultContainer,
		image,
		release,
		command,
		emitEscapeSequence,
		true,
		false); err != nil {
		return err
	}

	return nil
}

func enterContainer(container string,
	defaultContainer bool,
	image, release string,
	command []string,
	emitEscapeSequence, fallbackToBash, pedantic bool) error {

	if !pedantic {
		if image == "" {
			panic("image not specified")
		}

		if release == "" {
			panic("release not specified")
		}
	}

	logrus.Debugf("Checking if container %s exists", container)

	if _, err := podman.ContainerExists(container); err != nil {
		logrus.Debugf("Container %s not found", container)

		if pedantic {
			err := utils.CreateErrorContainerNotFound(container, executableBase)
			return err
		}

		containers, err := listContainers()
		if err != nil {
			err := utils.CreateErrorContainerNotFound(container, executableBase)
			return err
		}

		containersCount := len(containers)
		logrus.Debugf("Found %d containers", containersCount)

		if containersCount == 0 {
			var shouldCreateContainer bool
			promptForCreate := true

			if rootFlags.assumeYes {
				shouldCreateContainer = true
				promptForCreate = false
			}

			if promptForCreate {
				prompt := "No toolbox containers found. Create now? [y/N]"
				shouldCreateContainer = utils.AskForConfirmation(prompt)
			}

			if !shouldCreateContainer {
				fmt.Printf("A container can be created later with the 'create' command.\n")
				fmt.Printf("Run '%s --help' for usage.\n", executableBase)
				return nil
			}

			if err := createContainer(container, image, release, false); err != nil {
				return err
			}
		} else if containersCount == 1 && defaultContainer {
			fmt.Fprintf(os.Stderr, "Error: container %s not found\n", container)

			container = containers[0].Names[0]
			fmt.Fprintf(os.Stderr, "Entering container %s instead.\n", container)
			fmt.Fprintf(os.Stderr, "Use the 'create' command to create a different toolbox.\n")
			fmt.Fprintf(os.Stderr, "Run '%s --help' for usage.\n", executableBase)
		} else {
			var builder strings.Builder
			fmt.Fprintf(&builder, "container %s not found\n", container)
			fmt.Fprintf(&builder, "Use the '--container' option to select a toolbox.\n")
			fmt.Fprintf(&builder, "Run '%s --help' for usage.", executableBase)

			errMsg := builder.String()
			return errors.New(errMsg)
		}
	}

	if err := callFlatpakSessionHelper(container); err != nil {
		return err
	}

	logrus.Debugf("Starting container %s", container)
	if err := startContainer(container); err != nil {
		return err
	}

	entryPoint, entryPointPID, err := getEntryPointAndPID(container)
	if err != nil {
		return err
	}

	if entryPoint != "toolbox" {
		var builder strings.Builder
		fmt.Fprintf(&builder, "container %s is too old and no longer supported \n", container)
		fmt.Fprintf(&builder, "Recreate it with Toolbox version 0.0.17 or newer.\n")

		errMsg := builder.String()
		return errors.New(errMsg)
	}

	if entryPointPID <= 0 {
		return fmt.Errorf("invalid entry point PID of container %s", container)
	}

	logrus.Debugf("Waiting for container %s to finish initializing", container)

	toolboxRuntimeDirectory, err := utils.GetRuntimeDirectory(currentUser)
	if err != nil {
		return err
	}

	initializedStamp := fmt.Sprintf("%s/container-initialized-%d", toolboxRuntimeDirectory, entryPointPID)

	logrus.Debugf("Checking if initialization stamp %s exists", initializedStamp)

	initializedTimeout := 25 // seconds
	for i := 0; !utils.PathExists(initializedStamp); i++ {
		if i == initializedTimeout {
			return fmt.Errorf("failed to initialize container %s", container)
		}

		time.Sleep(time.Second)
	}

	logrus.Debugf("Container %s is initialized", container)

	if _, err := isCommandPresent(container, command[0]); err != nil {
		if fallbackToBash {
			fmt.Fprintf(os.Stderr,
				"Error: command %s not found in container %s\n",
				command[0],
				container)
			fmt.Fprintf(os.Stderr, "Using /bin/bash instead.\n")

			command = []string{"/bin/bash", "-l"}
		} else {
			return fmt.Errorf("command %s not found in container %s", command[0], container)
		}
	}

	logrus.Debug("Checking if 'podman exec' supports disabling the detach keys")

	var detachKeys []string

	if podman.CheckVersion("1.8.1") {
		logrus.Debug("'podman exec' supports disabling the detach keys")
		detachKeys = []string{"--detach-keys", ""}
	}

	envOptions := utils.GetEnvOptionsForPreservedVariables()
	logLevelString := podman.LogLevel.String()

	execArgs := []string{
		"--log-level", logLevelString,
		"exec",
	}

	execArgs = append(execArgs, detachKeys...)

	execArgs = append(execArgs, []string{
		"--interactive",
		"--tty",
		"--user", currentUser.Username,
		"--workdir", workingDirectory,
	}...)

	execArgs = append(execArgs, envOptions...)

	execArgs = append(execArgs, []string{
		container,
		"capsh", "--caps=", "--", "-c", "exec \"$@\"", "/bin/sh",
	}...)

	execArgs = append(execArgs, command...)

	if emitEscapeSequence {
		fmt.Printf("\033]777;container;push;%s;toolbox;%s\033\\", container, currentUser.Uid)
	}

	logrus.Debugf("Running in container %s:", container)
	logrus.Debug("podman")
	for _, arg := range execArgs {
		logrus.Debugf("%s", arg)
	}

	exitCode, err := shell.RunWithExitCode("podman", os.Stdin, os.Stdout, nil, execArgs...)

	if emitEscapeSequence {
		fmt.Printf("\033]777;container;pop;;;%s\033\\", currentUser.Uid)
	}

	switch exitCode {
	case 0:
		if err != nil {
			panic("unexpected error: 'podman exec' finished successfully")
		}
	case 125:
		err = fmt.Errorf("failed to invoke 'podman exec' in container %s", container)
	case 126:
		err = fmt.Errorf("failed to invoke command %s in container %s", command[0], container)
	case 127:
		if pathPresent, _ := isPathPresent(container, workingDirectory); !pathPresent {
			err = fmt.Errorf("directory %s not found in container %s", workingDirectory, container)
		} else {
			err = fmt.Errorf("command %s not found in container %s", command[0], container)
		}
	default:
		err = nil
	}

	if err != nil {
		return err
	}

	return nil
}

func enterHelp(cmd *cobra.Command, args []string) {
	if utils.IsInsideContainer() {
		if !utils.IsInsideToolboxContainer() {
			fmt.Fprintf(os.Stderr, "Error: this is not a toolbox container\n")
			return
		}

		if _, err := utils.ForwardToHost(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %s\n", err)
			return
		}

		return
	}

	if err := utils.ShowManual("toolbox-enter"); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		return
	}
}
