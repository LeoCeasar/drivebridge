package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/rclone/rclone/internal/bridge/markdown"
	"github.com/rclone/rclone/internal/bridge/platform"
)

var (
	ErrConversionRulesInvalid = errors.New("invalid conversion rules")
	ErrConversionRulesChanged = markdown.ErrConversionRulesChanged
)

// ConversionRulesView exposes persisted settings, separate from active rules.
type ConversionRulesView struct {
	Path            string          `json:"path"`
	Revision        string          `json:"revision"`
	Document        json.RawMessage `json:"document"`
	RestartRequired bool            `json:"restart_required"`
}

func conversionRulesView(path string, data []byte, rules *markdown.ConversionRules) ConversionRulesView {
	return ConversionRulesView{Path: path, Revision: markdown.ConversionRulesRevision(data), Document: rules.EffectiveDocumentJSON(), RestartRequired: rules.Fingerprint() != markdown.ActiveConversionRulesFingerprint()}
}

// ensureConversionRulesSettings never initializes the application configuration.
func (m *Manager) ensureConversionRulesSettings() (bool, error) {
	if err := platform.CheckNoSymlinks(m.options.ConfigFile); err != nil {
		return false, err
	}
	info, err := os.Lstat(m.options.ConfigFile)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%w: expected a regular configuration file", ErrConfig)
	}
	if err := markdown.EnsureConversionRulesFile(m.options.ConfigFile); err != nil {
		return false, err
	}
	return true, nil
}

// ConversionRules returns complete effective persisted rules without reconfiguring
// the immutable rules used by the currently running process.
func (m *Manager) ConversionRules(ctx context.Context) (ConversionRulesView, error) {
	if err := ctx.Err(); err != nil {
		return ConversionRulesView{}, err
	}
	_, err := m.ensureConversionRulesSettings()
	if err != nil {
		return ConversionRulesView{}, err
	}
	path := markdown.ConversionRulesPath(m.options.ConfigFile)
	data, err := markdown.ReadConversionRulesFile(path)
	if errors.Is(err, os.ErrNotExist) {
		rules, parseErr := markdown.PureParseConversionRules(markdown.DefaultConversionRulesJSON())
		if parseErr != nil {
			return ConversionRulesView{}, parseErr
		}
		return conversionRulesView(path, nil, rules), nil
	}
	if err != nil {
		return ConversionRulesView{}, err
	}
	rules, err := markdown.PureParseConversionRules(data)
	if err != nil {
		return ConversionRulesView{}, fmt.Errorf("%w: %s: %v", ErrConversionRulesInvalid, path, err)
	}
	return conversionRulesView(path, data, rules), nil
}

// SaveConversionRules persists validated settings with optimistic concurrency.
// Active conversion semantics remain unchanged until process restart.
func (m *Manager) SaveConversionRules(ctx context.Context, expectedRevision string, document json.RawMessage) (ConversionRulesView, error) {
	if err := ctx.Err(); err != nil {
		return ConversionRulesView{}, err
	}
	if _, err := markdown.PureParseConversionRules(document); err != nil {
		return ConversionRulesView{}, fmt.Errorf("%w: %v", ErrConversionRulesInvalid, err)
	}
	initialized, err := m.ensureConversionRulesSettings()
	if err != nil {
		return ConversionRulesView{}, err
	}
	if !initialized {
		return ConversionRulesView{}, fmt.Errorf("%w: initialize configuration before saving conversion rules", ErrConfig)
	}
	path := markdown.ConversionRulesPath(m.options.ConfigFile)
	rules, err := markdown.SaveConversionRulesFile(ctx, path, expectedRevision, document)
	if err != nil {
		return ConversionRulesView{}, err
	}
	return conversionRulesView(path, document, rules), nil
}
