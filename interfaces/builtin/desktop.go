// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2017 Canonical Ltd
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

package builtin

import (
	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/interfaces"
	"github.com/snapcore/snapd/interfaces/apparmor"
	"github.com/snapcore/snapd/interfaces/mount"
	"github.com/snapcore/snapd/osutil"
	"github.com/snapcore/snapd/release"
)

const desktopSummary = `allows access to basic graphical desktop resources`

const desktopBaseDeclarationSlots = `
  desktop:
    allow-installation:
      slot-snap-type:
        - core
`

const desktopConnectedPlugAppArmor = `
# Description: Can access basic graphical desktop resources. To be used with
# other interfaces (eg, wayland).

#include <abstractions/dbus-strict>
#include <abstractions/dbus-session-strict>

#include <abstractions/fonts>
/var/cache/fontconfig/   r,
/var/cache/fontconfig/** mr,

# subset of gnome abstraction
/etc/gtk-3.0/settings.ini r,
owner @{HOME}/.config/gtk-3.0/settings.ini r,
# Note: this leaks directory names that wouldn't otherwise be known to the snap
owner @{HOME}/.config/gtk-3.0/bookmarks r,

/usr/share/icons/                          r,
/usr/share/icons/**                        r,
/usr/share/icons/*/index.theme             rk,
/usr/share/pixmaps/                        r,
/usr/share/pixmaps/**                      r,
/usr/share/unity/icons/**                  r,
/usr/share/thumbnailer/icons/**            r,
/usr/share/themes/**                       r,

# The snapcraft desktop part may look for schema files in various locations, so
# allow reading system installed schemas.
/usr/share/glib*/schemas/{,*}              r,
/usr/share/gnome/glib*/schemas/{,*}        r,
/usr/share/ubuntu/glib*/schemas/{,*}       r,

# subset of freedesktop.org
owner @{HOME}/.local/share/mime/**   r,
owner @{HOME}/.config/user-dirs.dirs r,

# gmenu
dbus (send)
     bus=session
     interface=org.gtk.Actions
     member=Changed
     peer=(name=org.freedesktop.DBus, label=unconfined),

# notifications
dbus (send)
    bus=session
    path=/org/freedesktop/Notifications
    interface=org.freedesktop.Notifications
    member="{GetCapabilities,GetServerInformation,Notify}"
    peer=(label=unconfined),

dbus (receive)
    bus=session
    path=/org/freedesktop/Notifications
    interface=org.freedesktop.Notifications
    member=NotificationClosed
    peer=(label=unconfined),

# Allow requesting interest in receiving media key events. This tells Gnome
# settings that our application should be notified when key events we are
# interested in are pressed, and allows us to receive those events.
dbus (receive, send)
  bus=session
  interface=org.gnome.SettingsDaemon.MediaKeys
  path=/org/gnome/SettingsDaemon/MediaKeys
  peer=(label=unconfined),
dbus (send)
  bus=session
  interface=org.freedesktop.DBus.Properties
  path=/org/gnome/SettingsDaemon/MediaKeys
  member="Get{,All}"
  peer=(label=unconfined),

# Allow use of snapd's internal 'xdg-open'
/usr/bin/xdg-open ixr,
/usr/share/applications/{,*} r,
/usr/bin/dbus-send ixr,
dbus (send)
    bus=session
    path=/
    interface=com.canonical.SafeLauncher
    member=OpenURL
    peer=(label=unconfined),
# ... and this allows access to the new xdg-open service which
# is now part of snapd itself.
dbus (send)
    bus=session
    path=/io/snapcraft/Launcher
    interface=io.snapcraft.Launcher
    member=OpenURL
    peer=(label=unconfined),

# Lttng tracing is very noisy and should not be allowed by confined apps. Can
# safely deny. LP: #1260491
deny /{dev,run,var/run}/shm/lttng-ust-* rw,
`

type desktopInterface struct{}

func (iface *desktopInterface) Name() string {
	return "desktop"
}

func (iface *desktopInterface) StaticInfo() interfaces.StaticInfo {
	return interfaces.StaticInfo{
		Summary:              desktopSummary,
		ImplicitOnClassic:    true,
		BaseDeclarationSlots: desktopBaseDeclarationSlots,
	}
}

func (iface *desktopInterface) SanitizeSlot(slot *interfaces.Slot) error {
	return sanitizeSlotReservedForOS(iface, slot)
}

func (iface *desktopInterface) AutoConnect(*interfaces.Plug, *interfaces.Slot) bool {
	// allow what declarations allowed
	return true
}

func (iface *desktopInterface) AppArmorConnectedPlug(spec *apparmor.Specification, plug *interfaces.Plug, plugAttrs map[string]interface{}, slot *interfaces.Slot, slotAttrs map[string]interface{}) error {
	spec.AddSnippet(desktopConnectedPlugAppArmor)
	return nil
}

func (iface *desktopInterface) MountConnectedPlug(spec *mount.Specification, plug *interfaces.Plug, plugAttrs map[string]interface{}, slot *interfaces.Slot, slotAttrs map[string]interface{}) error {
	if !release.OnClassic {
		// There is nothing to expose on an all-snaps system
		return nil
	}

	fontconfigDirs := []string{
		dirs.SystemFontsDir,
		dirs.SystemLocalFontsDir,
		dirs.SystemFontconfigCacheDir,
	}

	for _, dir := range fontconfigDirs {
		if !osutil.IsDirectory(dir) {
			continue
		}
		spec.AddMountEntry(mount.Entry{
			Name:    "/var/lib/snapd/hostfs" + dir,
			Dir:     dirs.StripRootDir(dir),
			Options: []string{"bind", "ro"},
		})
	}
	return nil
}

func init() {
	registerIface(&desktopInterface{})
}