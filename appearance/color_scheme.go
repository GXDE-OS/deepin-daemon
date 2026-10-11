/*
 * Copyright (C) 2026 CharOfString <root@charofstring.cc>
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package appearance

// Keep the GTK theme and org.gnome.desktop.interface color-scheme in sync.
//
// On Wayland the compositor tells DTK clients whether to be dark or light by
// following whichever of these two keys changed last, and at login an explicit
// color-scheme wins over gtk-theme. If they disagree, DTK apps follow
// color-scheme while the control center shows gtk-theme, and after the next
// login the theme appears to have been reset. So: setting a GTK theme also
// writes color-scheme, an explicit color-scheme change switches the GTK theme
// to its dark/light counterpart, and at startup gtk-theme is reconciled with
// an explicit color-scheme the same way the compositor resolves it.

import (
	"strings"

	"pkg.deepin.io/dde/daemon/appearance/subthemes"
	"pkg.deepin.io/lib/gsettings"
	"pkg.deepin.io/lib/strv"
	dutils "pkg.deepin.io/lib/utils"
)

const (
	gnomeInterfaceSchema = "org.gnome.desktop.interface"
	gsKeyColorScheme     = "color-scheme"

	colorSchemeDark  = "prefer-dark"
	colorSchemeLight = "prefer-light"

	defaultDarkGtkTheme = "deepin-dark"
)

// Same rule as the compositor and dtk2widget: a theme is dark when its name
// contains "dark".
func isDarkGtkTheme(name string) bool {
	return strings.Contains(strings.ToLower(name), "dark")
}

func (m *Manager) initColorSchemeSetting() {
	s, err := dutils.CheckAndNewGSettings(gnomeInterfaceSchema)
	if err != nil {
		logger.Debug(err)
		return
	}
	// color-scheme only exists since gsettings-desktop-schemas 42
	if !strv.Strv(s.ListKeys()).Contains(gsKeyColorScheme) {
		s.Unref()
		return
	}
	m.gnomeIfcSetting = s
}

// explicitColorScheme returns whether color-scheme states a preference and,
// if so, whether it is dark. "default" states no preference.
func (m *Manager) explicitColorScheme() (explicit, dark bool) {
	if m.gnomeIfcSetting == nil {
		return false, false
	}
	switch m.gnomeIfcSetting.GetString(gsKeyColorScheme) {
	case colorSchemeDark:
		return true, true
	case colorSchemeLight:
		return true, false
	}
	return false, false
}

func (m *Manager) syncColorScheme(gtkTheme string) {
	if m.gnomeIfcSetting == nil {
		return
	}
	value := colorSchemeLight
	if isDarkGtkTheme(gtkTheme) {
		value = colorSchemeDark
	}
	// Writing an unchanged value would still notify the compositor and our
	// own listener, so skip it.
	if m.gnomeIfcSetting.GetString(gsKeyColorScheme) == value {
		return
	}
	if !m.gnomeIfcSetting.SetString(gsKeyColorScheme, value) {
		logger.Warning("failed to set color-scheme to", value)
	}
}

// gtkThemeForScheme returns the counterpart of theme with the requested
// darkness: "foo" <-> "foo-dark" when installed, else the default
// deepin / deepin-dark theme.
func gtkThemeForScheme(theme string, dark bool) string {
	if isDarkGtkTheme(theme) == dark {
		return theme
	}

	var candidate string
	if dark {
		candidate = theme + "-dark"
	} else if strings.HasSuffix(strings.ToLower(theme), "-dark") {
		candidate = theme[:len(theme)-len("-dark")]
	}
	if candidate != "" && subthemes.ListGtkTheme().Get(candidate) != nil {
		return candidate
	}

	if dark {
		return defaultDarkGtkTheme
	}
	return defaultGtkTheme
}

func (m *Manager) listenColorSchemeChanged() {
	if m.gnomeIfcSetting == nil {
		return
	}
	gsettings.ConnectChanged(gnomeInterfaceSchema, gsKeyColorScheme, func(key string) {
		explicit, dark := m.explicitColorScheme()
		if !explicit {
			return
		}
		theme := gtkThemeForScheme(m.GtkTheme.Get(), dark)
		err := m.set(TypeGtkTheme, theme)
		if err != nil {
			logger.Warning("failed to follow color-scheme:", err)
		}
	})
}
