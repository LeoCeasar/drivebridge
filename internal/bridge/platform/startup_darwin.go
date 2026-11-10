package platform

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func launchAgent(s StartupSpec) []byte {
	var b strings.Builder
	escape := func(v string) string { var o strings.Builder; _ = xml.EscapeText(&o, []byte(v)); return o.String() }
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>Label</key><string>com.drivebridge.mount.` + s.ID + `</string><key>ProgramArguments</key><array>`)
	for _, arg := range append([]string{s.Executable}, s.Args...) {
		b.WriteString("<string>" + escape(arg) + "</string>")
	}
	b.WriteString(`</array><key>RunAtLoad</key><true/><key>KeepAlive</key><false/><key>ProcessType</key><string>Interactive</string></dict></plist>`)
	return []byte(b.String())
}
func SetStartup(ctx context.Context, s StartupSpec, enabled bool) error {
	if err := validateStartup(s); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		return os.ErrPermission
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err = EnsurePrivateDir(dir); err != nil {
		return err
	}
	file := filepath.Join(dir, "com.drivebridge.mount."+s.ID+".plist")
	label := "gui/" + strconv.Itoa(os.Getuid()) + "/com.drivebridge.mount." + s.ID
	if enabled {
		if err = CheckNoSymlinks(file); err != nil {
			return err
		}
		if err = os.WriteFile(file, launchAgent(s), 0600); err != nil {
			return err
		}
		if err = SecurePrivate(file, false); err != nil {
			return err
		}
		if err = runStartup(ctx, "launchctl", "enable", label); err != nil {
			return err
		}
		if err = runStartup(ctx, "launchctl", "bootstrap", "gui/"+strconv.Itoa(os.Getuid()), file); err != nil {
			if probe := runStartup(ctx, "launchctl", "print", label); probe != nil {
				return fmt.Errorf("launch agent registration failed")
			}
		}
		return nil
	}
	if err = runStartup(ctx, "launchctl", "disable", label); err != nil {
		return err
	}
	if err = os.Remove(file); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
