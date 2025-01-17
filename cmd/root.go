// Copyright 2020 Nokia
// Licensed under the BSD 3-Clause License.
// SPDX-License-Identifier: BSD-3-Clause

package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/srl-labs/containerlab/git"
	"github.com/srl-labs/containerlab/utils"
)

const CLAB_AUTHORISED_GROUP = "clab_admins"

var (
	debugCount int
	debug      bool
	timeout    time.Duration
	logLevel   string
)

// path to the topology file.
var topo string

var (
	varsFile string
	graph    bool
	rt       string
)

// lab name.
var name string

// rootCmd represents the base command when called without any subcommands.
var rootCmd = &cobra.Command{
	Use:               "containerlab",
	Short:             "deploy container based lab environments with a user-defined interconnections",
	PersistentPreRunE: preRunFn,
	Aliases:           []string{"clab"},
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1) // skipcq: RVV-A0003
	}
}

func init() {
	rootCmd.SilenceUsage = true
	rootCmd.PersistentFlags().CountVarP(&debugCount, "debug", "d", "enable debug mode")
	rootCmd.PersistentFlags().StringVarP(&topo, "topo", "t", "", "path to the topology file")
	rootCmd.PersistentFlags().StringVarP(&varsFile, "vars", "", "",
		"path to the topology template variables file")
	_ = rootCmd.MarkPersistentFlagFilename("topo", "*.yaml", "*.yml")
	rootCmd.PersistentFlags().StringVarP(&name, "name", "", "", "lab name")
	rootCmd.PersistentFlags().DurationVarP(&timeout, "timeout", "", 120*time.Second,
		"timeout for external API requests (e.g. container runtimes), e.g: 30s, 1m, 2m30s")
	rootCmd.PersistentFlags().StringVarP(&rt, "runtime", "r", "", "container runtime")
	rootCmd.PersistentFlags().StringVarP(&logLevel, "log-level", "", "info",
		"logging level; one of [trace, debug, info, warning, error, fatal]")
}

func checkAndGetRootPrivs(_ *cobra.Command, _ []string) error {
	_, euid, suid := unix.Getresuid()
	if euid != 0 && suid != 0 {
		return fmt.Errorf("this containerlab command requires root privileges or root via SUID to run, effective UID: %v SUID: %v", euid, suid)
	}

	if euid != 0 && suid == 0 {
		clabGroupExists := true
		clabGroup, err := user.LookupGroup(CLAB_AUTHORISED_GROUP)
		if err != nil {
			if _, ok := err.(user.UnknownGroupError); ok {
				log.Debug("Containerlab admin group does not exist, skipping group membership check")
				clabGroupExists = false
			} else {
				return fmt.Errorf("failed to lookup containerlab admin group: %v", err)
			}
		}

		if clabGroupExists {
			currentEffUser, err := user.Current()
			if err != nil {
				return err
			}

			effUserGroupIDs, err := currentEffUser.GroupIds()
			if err != nil {
				return err
			}

			if !slices.Contains(effUserGroupIDs, clabGroup.Gid) {
				return fmt.Errorf("user '%v' is not part of containerlab admin group 'clab_admins' (GID %v), which is required to execute this command.\nTo add yourself to this group, run the following command:\n\t$ sudo gpasswd -a %v clab_admins",
					currentEffUser.Username, clabGroup.Gid, currentEffUser.Username)
			}

			log.Debug("Group membership check passed")
		}

		err = obtainRootPrivs()
		if err != nil {
			return err
		}
	}

	return nil
}

func obtainRootPrivs() error {
	// Escalate to root privileges, changing saved UIDs to root/current group to be able to retain privilege escalation
	err := changePrivileges(0, os.Getgid(), 0, os.Getgid())
	if err != nil {
		return err
	}

	log.Debug("Obtained root privileges")

	return nil
}

func dropRootPrivs() error {
	// Drop privileges to the running user, retaining current saved IDs
	err := changePrivileges(os.Getuid(), os.Getgid(), -1, -1)
	if err != nil {
		return err
	}

	log.Debug("Dropped root privileges")

	return nil
}

func changePrivileges(new_uid, new_gid, saved_uid, saved_gid int) error {
	if err := unix.Setresuid(-1, new_uid, saved_uid); err != nil {
		return fmt.Errorf("failed to set UID: %v", err)
	}
	if err := unix.Setresgid(-1, new_gid, saved_gid); err != nil {
		return fmt.Errorf("failed to set GID: %v", err)
	}
	log.Debugf("Changed running UIDs to UID: %d GID: %d", new_uid, new_gid)
	return nil
}

func preRunFn(cmd *cobra.Command, _ []string) error {
	// setting log level
	switch {
	case debugCount > 0:
		log.SetLevel(log.DebugLevel)
	default:
		l, err := log.ParseLevel(logLevel)
		if err != nil {
			return err
		}

		log.SetLevel(l)
	}

	// setting output to stderr, so that json outputs can be parsed
	log.SetOutput(os.Stderr)

	err := dropRootPrivs()
	if err != nil {
		return err
	}

	return getTopoFilePath(cmd)
}

// getTopoFilePath finds *.clab.y*ml file in the current working directory
// if the file was not specified.
// If the topology file refers to a git repository, it will be cloned to the current directory.
// Errors if more than one file is found by the glob path.
func getTopoFilePath(cmd *cobra.Command) error {
	// set commands which may use topo file find functionality, the rest don't need it
	if !(cmd.Name() == "deploy" || cmd.Name() == "destroy" || cmd.Name() == "redeploy" || cmd.Name() == "inspect" ||
		cmd.Name() == "save" || cmd.Name() == "graph") {
		return nil
	}

	// inspect and destroy commands with --all flag don't use file find functionality
	if (cmd.Name() == "inspect" || cmd.Name() == "destroy") &&
		cmd.Flag("all").Value.String() == "true" {
		return nil
	}

	var err error
	// perform topology clone/fetch if the topo file is not available locally
	if !utils.FileOrDirExists(topo) {
		switch {
		case git.IsGitHubOrGitLabURL(topo) || git.IsGitHubShortURL(topo):
			topo, err = processGitTopoFile(topo)
			if err != nil {
				return err
			}
		case utils.IsHttpURL(topo, true):
			// canonize the passed topo as URL by adding https schema if it was missing
			if !strings.HasPrefix(topo, "http://") && !strings.HasPrefix(topo, "https://") {
				topo = "https://" + topo
			}
		}
	}

	// if topo or name flags have been provided, don't try to derive the topo file
	if topo != "" || name != "" {
		return nil
	}

	log.Debugf("trying to find topology files automatically")

	files, err := filepath.Glob("*.clab.y*ml")

	if len(files) == 0 {
		return errors.New("no topology files matching the pattern *.clab.yml or *.clab.yaml found")
	}

	if len(files) > 1 {
		return fmt.Errorf("more than one topology file matching the pattern *.clab.yml or *.clab.yaml found, can't pick one: %q", files)
	}

	topo = files[0]

	log.Debugf("topology file found: %s", files[0])

	return err
}

func processGitTopoFile(topo string) (string, error) {
	// for short github urls, prepend https://github.com
	// note that short notation only works for github links
	if git.IsGitHubShortURL(topo) {
		topo = "https://github.com/" + topo
	}

	repo, err := git.NewRepo(topo)
	if err != nil {
		return "", err
	}

	// Instantiate the git implementation to use.
	gitImpl := git.NewGoGit(repo)

	// clone the repo via the Git Implementation
	err = gitImpl.Clone()
	if err != nil {
		return "", err
	}

	// adjust permissions for the checked out repo
	// it would belong to root/root otherwise
	err = utils.SetUIDAndGID(repo.GetName())
	if err != nil {
		log.Errorf("error adjusting repository permissions %v. Continuing anyways", err)
	}

	// prepare the path with the repo based path
	path := filepath.Join(repo.GetPath()...)
	// prepend that path with the repo base directory
	path = filepath.Join(repo.GetName(), path)

	// change dir to the
	err = os.Chdir(path)
	if err != nil {
		return "", err
	}

	return repo.GetFilename(), err
}
