package chat

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/logging"
)

// ResolveSink builds the wire-log sink for a session or run. File logging
// is on only when configPath is set and the config enables logging;
// otherwise a no-op sink is returned so downstream code always has a
// non-nil sink. When enabled, the session gets its own
// `<timestamp>-<uuid>` subdirectory of the log dir, so conversations
// never interleave.
func ResolveSink(configPath string, cfg config.Config) (logging.Sink, error) {
	if configPath == "" || !cfg.LoggingEnabled() {
		return logging.NewNop(), nil
	}

	dir := filepath.Join(filepath.Dir(configPath), cfg.LogDir())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	sink, _, err := logging.NewSession(dir)
	if err != nil {
		return nil, fmt.Errorf("open log dir: %w", err)
	}
	return sink, nil
}

// resolveSink builds the wire-log sink for a session, delegating to
// ResolveSink with the session's options.
func (o Options) resolveSink() (logging.Sink, error) {
	return ResolveSink(o.ConfigPath, o.Config)
}
