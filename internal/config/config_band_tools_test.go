package config_test

import (
	"strings"
	"testing"

	"github.com/overspecific/blorb/internal/config"
)

func TestBandToolTypeRejectedInConfig(t *testing.T) {
	t.Run("band type in blorb.json is rejected", func(t *testing.T) {
		cfg := config.Config{
			Providers: []config.Provider{validProvider()},
			Models:    []config.Model{validModel()},
			Agents:    []config.Agent{validAgent()},
			Tools: []config.ToolEntry{{
				Type:        config.ToolTypeBand,
				Name:        "band_send_message",
				Description: "Sends a Band message.",
				Band:        "band_send_message",
			}},
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("Validate error = nil, want the band-wiring error")
		}
		if !strings.Contains(err.Error(), "band tools are wired by the band command") {
			t.Errorf("Validate error = %v, want it to name the band command", err)
		}
	})

	t.Run("band field on a command tool is rejected", func(t *testing.T) {
		cfg := config.Config{
			Providers: []config.Provider{validProvider()},
			Models:    []config.Model{validModel()},
			Agents:    []config.Agent{validAgent()},
			Tools: []config.ToolEntry{{
				Type:        config.ToolTypeCommand,
				Name:        "t",
				Description: "A command tool.",
				Command:     []string{"echo"},
				Band:        "band_send_message",
			}},
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("Validate error = nil, want a band-field rejection")
		}
		if !strings.Contains(err.Error(), "band is not valid") {
			t.Errorf("Validate error = %v, want a band-not-valid error", err)
		}
	})

	t.Run("band field inside a toolset is rejected", func(t *testing.T) {
		cfg := config.Config{
			Providers: []config.Provider{validProvider()},
			Models:    []config.Model{validModel()},
			Agents:    []config.Agent{validAgent()},
			Toolsets: []config.Toolset{{
				Name: "ts",
				Type: config.ToolsetTypeSimple,
				Tools: []config.ToolEntry{{
					Type:        config.ToolTypeCommand,
					Name:        "t",
					Description: "A command tool.",
					Command:     []string{"echo"},
					Band:        "band_send_message",
				}},
			}},
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("Validate error = nil, want a band-field error")
		}
		if !strings.Contains(err.Error(), "band is not valid") {
			t.Errorf("Validate error = %v, want a band-not-valid error", err)
		}
	})
}
