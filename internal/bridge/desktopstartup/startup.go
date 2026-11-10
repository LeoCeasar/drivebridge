// Package desktopstartup manages opt-in login startup for the current OS user.
// Registration never starts or stops mount workers. macOS/Linux preferences
// take effect at the next graphical login; the running tray is left alone.
package desktopstartup

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf16"
)

const label = "org.drivebridge.desktop"
const taskPrefix = `DriveBridge Desktop-`

func (b backend) taskName() string { return taskPrefix + b.uid }

type Spec struct{ Executable, ConfigFile, CLIExecutable string }
type runner func(context.Context, string, ...string) ([]byte, error)
type backend struct {
	goos, home, config, uid string
	run                     runner
}

func current() (backend, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return backend{}, err
	}
	u, err := user.Current()
	if err != nil {
		return backend{}, err
	}
	config := os.Getenv("XDG_CONFIG_HOME")
	if config == "" || !filepath.IsAbs(config) {
		config = filepath.Join(home, ".config")
	}
	return backend{runtime.GOOS, home, config, u.Uid, func(ctx context.Context, command string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, command, args...).CombinedOutput()
	}}, nil
}
func Set(ctx context.Context, s Spec, enabled bool) error {
	b, err := current()
	if err != nil {
		return err
	}
	return b.set(ctx, s, enabled)
}
func Enabled(ctx context.Context, s Spec) (bool, error) {
	b, err := current()
	if err != nil {
		return false, err
	}
	return b.enabled(ctx, s)
}
func (s Spec) argv() ([]string, error) {
	for _, p := range []string{s.Executable, s.ConfigFile} {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, "\x00\r\n") {
			return nil, errors.New("desktop startup requires absolute executable and configuration paths")
		}
	}
	if s.CLIExecutable != "" && (!filepath.IsAbs(s.CLIExecutable) || strings.ContainsAny(s.CLIExecutable, "\x00\r\n")) {
		return nil, errors.New("desktop startup requires an absolute lark CLI path")
	}
	a := []string{"desktop", "_run", "--bridge-config", s.ConfigFile}
	if s.CLIExecutable != "" {
		a = append(a, "--lark-cli", s.CLIExecutable)
	}
	return a, nil
}
func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
func (b backend) path() string {
	if b.goos == "darwin" {
		return filepath.Join(b.home, "Library", "LaunchAgents", label+".plist")
	}
	return filepath.Join(b.config, "autostart", label+".desktop")
}
func (b backend) render(s Spec) ([]byte, error) {
	args, err := s.argv()
	if err != nil {
		return nil, err
	}
	switch b.goos {
	case "darwin":
		var a strings.Builder
		for _, arg := range append([]string{s.Executable}, args...) {
			fmt.Fprintf(&a, "<string>%s</string>", xmlText(arg))
		}
		return []byte(`<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>Label</key><string>` + label + `</string><key>ProgramArguments</key><array>` + a.String() + `</array><key>LimitLoadToSessionType</key><string>Aqua</string><key>RunAtLoad</key><true/><key>KeepAlive</key><false/></dict></plist>` + "\n"), nil
	case "linux":
		a := []string{}
		for _, arg := range append([]string{s.Executable}, args...) {
			a = append(a, desktopQuote(arg))
		}
		return []byte("[Desktop Entry]\nType=Application\nName=DriveBridge\nComment=DriveBridge login startup\nExec=" + strings.Join(a, " ") + "\nTerminal=false\nX-GNOME-Autostart-enabled=true\n"), nil
	case "windows":
		a := []string{}
		for _, arg := range args {
			a = append(a, windowsQuote(arg))
		}
		return []byte(`<?xml version="1.0" encoding="UTF-8"?><Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task"><Triggers><LogonTrigger><Enabled>true</Enabled><UserId>` + xmlText(b.uid) + `</UserId></LogonTrigger></Triggers><Principals><Principal id="CurrentUser"><UserId>` + xmlText(b.uid) + `</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals><Settings><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy><DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries><StopIfGoingOnBatteries>false</StopIfGoingOnBatteries><ExecutionTimeLimit>PT0S</ExecutionTimeLimit><Enabled>true</Enabled><Hidden>true</Hidden></Settings><Actions Context="CurrentUser"><Exec><Command>` + xmlText(s.Executable) + `</Command><Arguments>` + xmlText(strings.Join(a, " ")) + `</Arguments></Exec></Actions></Task>`), nil
	default:
		return nil, fmt.Errorf("desktop startup unsupported on %s", b.goos)
	}
}

// Desktop Entry Exec has its own quoting; percent must not become a field code.
func desktopQuote(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`").Replace(s)
	return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"`
}

// CommandLineToArgvW backslash-before-quote rules, without a shell.
func windowsQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, c := range s {
		if c == '\\' {
			slashes++
			continue
		}
		if c == '"' {
			b.WriteString(strings.Repeat(`\`, slashes*2+1))
		} else {
			b.WriteString(strings.Repeat(`\`, slashes))
		}
		slashes = 0
		b.WriteRune(c)
	}
	b.WriteString(strings.Repeat(`\`, slashes*2))
	b.WriteByte('"')
	return b.String()
}
func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".drivebridge-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
func (b backend) enabled(ctx context.Context, s Spec) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	expected, err := b.render(s)
	if err != nil {
		return false, err
	}
	if b.goos == "windows" {
		listing, err := b.run(ctx, "schtasks.exe", "/Query", "/FO", "CSV", "/NH")
		if err != nil {
			return false, fmt.Errorf("list current-user login tasks: %w: %s", err, listing)
		}
		rows, err := csv.NewReader(strings.NewReader(string(listing))).ReadAll()
		if err != nil {
			return false, err
		}
		found := false
		for _, row := range rows {
			if len(row) > 0 && strings.TrimPrefix(row[0], `\`) == b.taskName() {
				found = true
			}
		}
		if !found {
			return false, nil
		}
		out, err := b.run(ctx, "schtasks.exe", "/Query", "/TN", b.taskName(), "/XML")
		if err != nil {
			return false, fmt.Errorf("query desktop login task: %w: %s", err, out)
		}
		return taskMatches(out, expected)
	}
	actual, err := os.ReadFile(b.path())
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !bytes.Equal(actual, expected) {
		return false, errors.New("desktop startup belongs to a different executable or configuration")
	}

	return true, nil
}
func (b backend) set(ctx context.Context, s Spec, on bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	content, err := b.render(s)
	if err != nil {
		return err
	}
	if b.goos == "windows" {
		if !on {
			enabled, err := b.enabled(ctx, s)
			if err != nil || !enabled {
				return err
			}
			out, err := b.run(ctx, "schtasks.exe", "/Delete", "/TN", b.taskName(), "/F")
			if err != nil {
				return fmt.Errorf("delete desktop login task: %w: %s", err, out)
			}
			return nil
		}
		// Query first: never overwrite a task for another configuration.
		if _, err := b.enabled(ctx, s); err != nil {
			return err
		}
		f, err := os.CreateTemp("", "drivebridge-task-*.xml")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		_, err = f.Write(content)
		ce := f.Close()
		if err != nil {
			return err
		}
		if ce != nil {
			return ce
		}
		out, err := b.run(ctx, "schtasks.exe", "/Create", "/TN", b.taskName(), "/XML", f.Name(), "/F")
		if err != nil {
			return fmt.Errorf("register desktop login task: %w: %s", err, out)
		}
		return nil
	}
	old, readErr := os.ReadFile(b.path())
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if readErr == nil && !bytes.Equal(old, content) {
		return errors.New("desktop startup belongs to a different executable or configuration")
	}

	if !on {
		err := os.Remove(b.path())
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := atomicWrite(b.path(), content); err != nil {
		return err
	}

	return nil
}
func taskMatches(actual, expected []byte) (bool, error) {
	type task struct {
		Triggers struct {
			Logon struct {
				User    string `xml:"UserId"`
				Enabled string
			} `xml:"LogonTrigger"`
		} `xml:"Triggers"`
		Principals struct {
			Principal struct {
				User                string `xml:"UserId"`
				LogonType, RunLevel string
			} `xml:"Principal"`
		} `xml:"Principals"`
		Actions struct {
			Exec struct{ Command, Arguments string } `xml:"Exec"`
		} `xml:"Actions"`
		Settings struct{ Enabled, Hidden, ExecutionTimeLimit, MultipleInstancesPolicy string } `xml:"Settings"`
	}
	// schtasks may return UTF-16; the command output is decoded explicitly.
	if len(actual) > 1 && (actual[0] == 0xff && actual[1] == 0xfe || actual[0] == '<' && actual[1] == 0) {
		start := 0
		if actual[0] == 0xff {
			start = 2
		}
		r := []uint16{}
		for i := start; i+1 < len(actual); i += 2 {
			r = append(r, uint16(actual[i])|uint16(actual[i+1])<<8)
		}
		actual = []byte(string(utf16.Decode(r)))
		actual = bytes.Replace(actual, []byte(`encoding="UTF-16"`), []byte(`encoding="UTF-8"`), 1)
		actual = bytes.Replace(actual, []byte(`encoding="utf-16"`), []byte(`encoding="UTF-8"`), 1)
	}
	var a, e task
	if err := xml.Unmarshal(actual, &a); err != nil {
		return false, err
	}
	if err := xml.Unmarshal(expected, &e); err != nil {
		return false, err
	}
	if a != e {
		return false, errors.New("desktop startup belongs to a different executable, user or configuration")
	}
	return true, nil
}
