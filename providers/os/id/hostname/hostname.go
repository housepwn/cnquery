// Copyright (c) Mondoo, Inc.
// SPDX-License-Identifier: BUSL-1.1

package hostname

import (
	"io"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/spf13/afero"
	"go.mondoo.com/cnquery/v11/providers-sdk/v1/inventory"
	"go.mondoo.com/cnquery/v11/providers/os/connection/shared"
)

// Hostname returns the hostname of the system.

// On Linux systems we prefer `hostname -f` over `/etc/hostname` since systemd is not updating the value all the time.
// On Windows the `hostname` command (without the -f flag) works more reliable than `powershell -c "$env:computername"`
// since it will return a non-zero exit code.
func Hostname(conn shared.Connection, pf *inventory.Platform) (string, bool) {
	var hostname string

	if !pf.IsFamily(inventory.FAMILY_UNIX) && !pf.IsFamily(inventory.FAMILY_WINDOWS) {
		log.Warn().Msg("your platform is not supported for hostname detection")
		return hostname, false
	}

	// on unix systems we try to get the hostname via `hostname -f` first since it returns the fqdn
	if pf.IsFamily(inventory.FAMILY_UNIX) {
		cmd, err := conn.RunCommand("hostname -f")
		if err == nil && cmd.ExitStatus == 0 {
			data, err := io.ReadAll(cmd.Stdout)
			if err == nil {
				return strings.TrimSpace(string(data)), true
			}
		} else {
			log.Debug().Err(err).Msg("could not run `hostname -f` command")
		}
	}

	// This is the preferred way to get the hostname on windows, it is important to not use the -f flag here
	cmd, err := conn.RunCommand("hostname")
	if err == nil && cmd.ExitStatus == 0 {
		data, err := io.ReadAll(cmd.Stdout)
		if err == nil {
			return strings.TrimSpace(string(data)), true
		}
	} else {
		log.Debug().Err(err).Msg("could not run `hostname` command")
	}

	// Fallback to for unix systems to /etc/hostname, since hostname command is not available on all systems
	// This mechanism is also working for static analysis
	if pf.IsFamily(inventory.FAMILY_LINUX) {
		afs := &afero.Afero{Fs: conn.FileSystem()}
		ok, err := afs.Exists("/etc/hostname")
		if err == nil && ok {
			content, err := afs.ReadFile("/etc/hostname")
			if err == nil {
				return strings.TrimSpace(string(content)), true
			}
		} else {
			log.Debug().Err(err).Msg("could not read /etc/hostname file")
		}
	}

	return "", false
}
