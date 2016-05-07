// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2014-2016 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package snapstate

import (
	"fmt"
	"strings"

	"github.com/ubuntu-core/snappy/arch"
	"github.com/ubuntu-core/snappy/snap"
	// XXX: what to do about flags
	"github.com/ubuntu-core/snappy/snappy"
)

// featureSet contains the flag values that can be listed in assumes entries
// that this ubuntu-core actually provides.
var featureSet = map[string]bool{
	// Support for common data directory across revisions of a snap.
	"common-data-dir": true,
}

func checkAssumes(s *snap.Info) error {
	missing := ([]string)(nil)
	for _, flag := range s.Assumes {
		if !featureSet[flag] {
			missing = append(missing, flag)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("snap %q assumes unsupported features: %s (try new ubuntu-core)", s.Name(), strings.Join(missing, ", "))
	}
	return nil
}

// openSnapFile opens a snap blob returning both a snap.Info completed
// with sideInfo (if not nil) and a corresponding snap.File.
func openSnapFileImpl(snapPath string, sideInfo *snap.SideInfo) (*snap.Info, snap.File, error) {
	snapf, err := snap.Open(snapPath)
	if err != nil {
		return nil, nil, err
	}

	info, err := snap.ReadInfoFromSnapFile(snapf, sideInfo)
	if err != nil {
		return nil, nil, err
	}

	return info, snapf, nil
}

var openSnapFile = openSnapFileImpl

// checkSnap ensures that the snap can be installed.
func checkSnap(snapFilePath string, curInfo *snap.Info, flags snappy.InstallFlags) error {
	//allowGadget := (flags & snappy.AllowGadget) != 0
	//allowUnauth := (flags & snappy.AllowUnauthenticated) != 0

	// XXX: actually verify snap before using content from it unless allowUnauth

	s, _, err := openSnapFile(snapFilePath, nil)
	if err != nil {
		return err
	}

	// verify we have a valid architecture
	if !arch.IsSupportedArchitecture(s.Architectures) {
		return fmt.Errorf("snap %q supported architectures (%s) are incompatible with this system (%s)", s.Name(), strings.Join(s.Architectures, ", "), arch.UbuntuArchitecture())
	}

	err = checkAssumes(s)
	if err != nil {
		return err
	}

	/* XXX: implement gadget install checks
	if s.Type == snap.TypeGadget {
		if !allowGadget {
			if currentGadget, err := getGadget(); err == nil {
				if currentGadget.Name() != s.Name() {
					return ErrGadgetPackageInstall
				}
			} else {
				// there should always be a gadget package now
				return ErrGadgetPackageInstall
			}
		}
	}
	*/

	return nil
}
