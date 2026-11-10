//go:build windows

package platform

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"golang.org/x/sys/windows"
	"os"
	"os/exec"
	"unicode/utf16"
)

func DesktopAvailable() bool {
	var session uint32
	if windows.ProcessIdToSessionId(uint32(os.Getpid()), &session) != nil || session == 0 {
		return false
	}
	window, _, _ := windows.NewLazySystemDLL("user32.dll").NewProc("GetShellWindow").Call()
	return window != 0
}
func openBrowser(ctx context.Context, u string) error {
	return exec.CommandContext(ctx, "rundll32.exe", "url.dll,FileProtocolHandler", u).Run()
}
func encodedCommand(s string) string {
	units := utf16.Encode([]rune(s))
	b := make([]byte, len(units)*2)
	for i, v := range units {
		binary.LittleEndian.PutUint16(b[i*2:], v)
	}
	return base64.StdEncoding.EncodeToString(b)
}
func notify(ctx context.Context, title, message string) error {
	// Only base64 data is interpolated into the fixed PowerShell program.
	t := base64.StdEncoding.EncodeToString([]byte(title))
	m := base64.StdEncoding.EncodeToString([]byte(message))
	script := `$ErrorActionPreference='Stop';[Windows.UI.Notifications.ToastNotificationManager,Windows.UI.Notifications,ContentType=WindowsRuntime]>$null;[Windows.Data.Xml.Dom.XmlDocument,Windows.Data.Xml.Dom.XmlDocument,ContentType=WindowsRuntime]>$null;$t=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('` + t + `'));$m=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('` + m + `'));$xml=New-Object Windows.Data.Xml.Dom.XmlDocument;$xml.LoadXml('<toast><visual><binding template="ToastGeneric"><text>'+[Security.SecurityElement]::Escape($t)+'</text><text>'+[Security.SecurityElement]::Escape($m)+'</text></binding></visual></toast>');$toast=[Windows.UI.Notifications.ToastNotification]::new($xml);[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('DriveBridge').Show($toast)`
	return exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", encodedCommand(script)).Run()
}
