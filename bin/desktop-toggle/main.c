/*
 * Copyright (C) 2016 ~ 2018 Deepin Technology Co., Ltd.
 *
 * Author:     jouyouyun <jouyouwen717@gmail.com>
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

/**
 * This code was taken from this post (I did not write it):
 *
 * http://www.linuxquestions.org/questions/linux-software-2/how-to-show-desktop-in-xfce4-601161/
 *
 **/

#include <X11/Xatom.h>
#include <X11/Xlib.h>

#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include <stdbool.h>

#include <gio/gio.h>

#define DEEPIN_WM_SERVICE "com.deepin.wm"
#define DEEPIN_WM_PATH "/com/deepin/wm"
#define DEEPIN_WM_INTERFACE "com.deepin.wm"

// 判断当前是否处于 Wayland 会话 
static bool is_wayland_session(void)
{
    const char *wayland_display = getenv("WAYLAND_DISPLAY");
    if (wayland_display != NULL && wayland_display[0] != '\0') {
        return true;
    }
    const char *session_type = getenv("XDG_SESSION_TYPE");
    if (session_type != NULL && strcmp(session_type, "wayland") == 0) {
        return true;
    }
    return false;
}

// 原有 x11 实现
static int show_desktop_x11(void)
{
    Display *display = XOpenDisplay(NULL);
    if (display == NULL) {
        fprintf(stderr, "Cannot open display\n");
        return -1;
    }

    Window root = DefaultRootWindow(display);
    Atom showing_desktop = XInternAtom(display, "_NET_SHOWING_DESKTOP", False);
    Atom actual_type;
    int actual_format;
    unsigned long nitems, bytes_after;
    unsigned char *data = NULL;

    int result = XGetWindowProperty(display, root, showing_desktop, 0, 1, False,
                                    XA_CARDINAL, &actual_type, &actual_format,
                                    &nitems, &bytes_after, &data);

    int current = 0;
    if (result == Success && data != NULL && nitems > 0) {
        current = (int)(*(unsigned long *)data);
        XFree(data);
    } else {
        fprintf(stderr, "Failed to get _NET_SHOWING_DESKTOP property\n");
    }

    int target = current ? 0 : 1;

    XClientMessageEvent event;
    memset(&event, 0, sizeof(event));
    event.type = ClientMessage;
    event.window = root;
    event.message_type = showing_desktop;
    event.format = 32;
    event.data.l[0] = target;
    event.data.l[1] = 1;  /* source indication: 1 = application（KWin 要求非 0 才处理） */

    if (XSendEvent(display, root, False,
                   SubstructureNotifyMask | SubstructureRedirectMask,
                   (XEvent *)&event) == 0) {
        fprintf(stderr, "Failed to send _NET_SHOWING_DESKTOP ClientMessage\n");
        XCloseDisplay(display);
        return -1;
    }
    XSync(display, False);
    XCloseDisplay(display);

    printf("Current state: %d\n", current);
    return 0;
}

static int show_desktop_via_deepin_wm_dbus(void)
{
    GError *error = NULL;
    GDBusConnection *conn = g_bus_get_sync(G_BUS_TYPE_SESSION, NULL, &error);
    if (conn == NULL) {
        fprintf(stderr, "Failed to connect to session bus: %s\n",
                error ? error->message : "unknown");
        g_clear_error(&error);
        return -1;
    }

    // 读取当前状态
    GVariant *result = g_dbus_connection_call_sync(
        conn, DEEPIN_WM_SERVICE, DEEPIN_WM_PATH, DEEPIN_WM_INTERFACE,
        "GetIsShowDesktop", NULL, G_VARIANT_TYPE("(b)"),
        G_DBUS_CALL_FLAGS_NONE, -1, NULL, &error);
    if (result == NULL) {
        fprintf(stderr, "com.deepin.wm.GetIsShowDesktop unavailable: %s\n",
                error ? error->message : "unknown");
        g_clear_error(&error);
        g_object_unref(conn);
        return -1;
    }

    gboolean current = FALSE;
    g_variant_get(result, "(b)", &current);
    g_variant_unref(result);

    // 取反并写回 
    gboolean target = !current;
    result = g_dbus_connection_call_sync(
        conn, DEEPIN_WM_SERVICE, DEEPIN_WM_PATH, DEEPIN_WM_INTERFACE,
        "SetShowDesktop", g_variant_new("(b)", target), NULL,
        G_DBUS_CALL_FLAGS_NONE, -1, NULL, &error);
    if (result == NULL) {
        fprintf(stderr, "com.deepin.wm.SetShowDesktop failed: %s\n",
                error ? error->message : "unknown");
        g_clear_error(&error);
        g_object_unref(conn);
        return -1;
    }
    g_variant_unref(result);
    g_object_unref(conn);

    printf("Current state: %d\n", current ? 1 : 0);
    return 0;
}

int main(void)
{
    if (is_wayland_session()) {
        if (show_desktop_via_deepin_wm_dbus() == 0) {
            return 0;
        }
        fprintf(stderr, "Show desktop failed under Wayland\n");
        return -1;
    }

    return show_desktop_x11();
}
