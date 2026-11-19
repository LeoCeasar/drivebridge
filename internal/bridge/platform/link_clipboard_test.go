package platform

import (
	"context"
	"errors"
	"testing"
)

// Models the external clipboard; the guard under test is the production code.
type memoryLinkClipboard struct {
	version        uint32
	text           string
	beforeLock     func()
	lockErr        error
	beforeSequence func()
}

func (c *memoryLinkClipboard) sequence() (uint32, error) {
	if c.beforeSequence != nil {
		c.beforeSequence()
	}
	return c.version, nil
}
func (c *memoryLinkClipboard) withLock(_ context.Context, fn func() error) error {
	if c.beforeLock != nil {
		c.beforeLock()
	}
	if c.lockErr != nil {
		return c.lockErr
	}
	return fn()
}
func (c *memoryLinkClipboard) write(_ context.Context, text string) error {
	c.text = text
	c.version++
	return nil
}

func TestLinkClipboardPreservesNewContent(t *testing.T) {
	for _, duringLock := range []bool{false, true} {
		c := &memoryLinkClipboard{version: 10, text: "original"}
		write, err := newLinkClipboardWriter(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
		change := func() { c.text = "new user content"; c.version++ }
		if duringLock {
			c.beforeLock = change
		} else {
			change()
		}
		if err := write("https://tenant.feishu.cn/docx/doc"); !errors.Is(err, ErrLinkClipboardChanged) {
			t.Fatal(err)
		}
		if c.text != "new user content" {
			t.Fatalf("overwrote clipboard: %q", c.text)
		}
	}
}

func TestLinkClipboardCopyCancelAndBusy(t *testing.T) {
	for _, scenario := range []string{"copy", "cancel", "busy", "cancel during lock"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := &memoryLinkClipboard{version: 10, text: "original"}
			write, err := newLinkClipboardWriter(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "cancel":
				cancel()
			case "busy":
				c.lockErr = errors.New("busy")
			case "cancel during lock":
				c.beforeLock = cancel
			}
			err = write("链接内容")
			if scenario == "copy" {
				if err != nil || c.text != "链接内容" {
					t.Fatal(c.text, err)
				}
			} else if err == nil || c.text != "original" {
				t.Fatal(c.text, err)
			}
		})
	}
}

func TestLinkClipboardCancellationDuringSequencePreservesContent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &memoryLinkClipboard{version: 10, text: "keep"}
	write, err := newLinkClipboardWriter(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	c.beforeSequence = cancel
	if err := write("late result"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if c.text != "keep" {
		t.Fatal(c.text)
	}
}
