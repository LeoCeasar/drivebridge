//go:build desktop && cgo && linux

package tray

/*
#cgo pkg-config: gtk+-3.0 gio-2.0
#include <gtk/gtk.h>
#include <gio/gio.h>

// Probe before upstream gtk_init(), which terminates the process without a display.
static int drivebridge_tray_preflight(void) {
	if (!gtk_init_check(NULL, NULL)) return 1;
	GError *error = NULL;
	GDBusConnection *connection = g_bus_get_sync(G_BUS_TYPE_SESSION, NULL, &error);
	if (connection == NULL) { if (error) g_error_free(error); return 2; }
	GVariant *reply = g_dbus_connection_call_sync(connection,
		"org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus",
		"NameHasOwner", g_variant_new("(s)", "org.kde.StatusNotifierWatcher"),
		G_VARIANT_TYPE("(b)"), G_DBUS_CALL_FLAGS_NONE, 1500, NULL, &error);
	gboolean present = FALSE;
	if (reply) { g_variant_get(reply, "(b)", &present); g_variant_unref(reply); }
	if (error) g_error_free(error);
	g_object_unref(connection);
	return present ? 0 : 2;
}
*/
import "C"

import (
	"fmt"
	"os"
)

func preflight() error {
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return ErrNoDisplay
	}
	switch C.drivebridge_tray_preflight() {
	case 0:
		return nil
	case 1:
		return ErrNoDisplay
	default:
		return fmt.Errorf("%w: Linux session has no StatusNotifierWatcher; enable a desktop AppIndicator extension", ErrUnavailable)
	}
}
