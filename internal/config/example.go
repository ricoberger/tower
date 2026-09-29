package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Example is the commented example configuration written by
// "tower config init". Home-relative paths use a literal $HOME reference that
// is interpolated when the file is loaded; it is never substituted here.
const Example = `# tower configuration.
#
# String settings may reference environment variables as $VAR or ${VAR}; use
# $$ for a literal dollar sign. A referenced variable that is not set is an
# error. A leading "~/" in path settings expands to $HOME. Relative paths are
# resolved against the directory containing this file, not the directory tower
# is started from.

# Where items, logs and the lock file are stored.
state_dir: $HOME/.local/state/tower

# Editor used to open reports and logs. Defaults to $EDITOR, then "vi".
# editor: nvim

sources:
  # Reads alerts from a JSON file in the GET /api/v2/alerts format on every
  # poll (development / tests). The path is relative to this file's directory,
  # i.e. testdata/alerts.json next to this configuration file.
  - name: dev-fixture                    # unique, [a-z0-9-]+, used in item ids
    type: file                           # alertmanager | file
    path: ./testdata/alerts.json

  # Grafana-managed Alertmanager, polled at
  # {url}/api/alertmanager/{grafana_alertmanager}/api/v2/alerts:
  # - name: prod
  #   type: alertmanager
  #   # Name of the Grafana instance in $GRAFANA_INSTANCES (the JSON object the
  #   # sre-grafana skill uses). It must exist when referenced. Its url and
  #   # auth.tokenCommand (run with sh -c at request time, sent as a bearer
  #   # token) are used for polling unless url / auth are set explicitly;
  #   # "auth: {type: none}" disables the derived auth.
  #   grafana_instance: prod
  #   grafana_alertmanager: grafana
  #   # Alertmanager matchers pushed down to the API as filter= params.
  #   filter:
  #     - team="core"
  #   # Optional receiver regex pushed down as receiver= param.
  #   receiver: ".*"

  # Plain Prometheus Alertmanager, polled at {url}/api/v2/alerts:
  # - name: legacy-am
  #   type: alertmanager
  #   url: https://alertmanager.example.com
  #   auth:
  #     type: basic                      # none | basic | bearer
  #     # exactly one of token / token_file / token_command for bearer;
  #     # username + one of password / password_file / password_command for basic
  #     username: tower
  #     # Files are read and commands run (sh -c) on every poll; their
  #     # output is trimmed. Commands are never interpolated.
  #     password_file: $HOME/.config/tower/legacy-am.password
  #   # Grafana instance for investigations (metadata only for plain sources;
  #   # its credentials are never used for polling).
  #   # Optional but recommended: without it the skill has no Grafana to query.
  #   grafana_instance: prod

alerts:
  # Poll interval for all alert sources (at least 10s).
  poll_interval: 1m
  # An alert must be firing for at least this long (measured from startsAt)
  # before it is prepared automatically.
  prepare_after: 5m
  # Resolved items stay visible this long before becoming done.
  resolved_linger: 4h
  # A re-fire of the same fingerprint within this window reopens the item.
  reopen_window: 24h
  # Queue priority by severity label value; unknown/missing values go last.
  severity_order: [critical, error, warning, info]

runs:
  # Maximum number of concurrently executing runs (1-10).
  concurrency: 2
  # Runs are killed after this duration and marked failed (at least 1m).
  timeout: 20m
  # Command and extra arguments for prep runs.
  command: copilot
  args: [--yolo]
  # Optional; empty = Copilot CLI default model.
  model: ""
  # Arguments used when resuming a session interactively.
  resume_args: []

notifications:
  enabled: true
  sound: default                         # "" disables sound

ghostty:
  # Script used to open new Ghostty surfaces.
  command: ghostty-new
  placement: split                       # split | tab | window
  direction: right                       # used for split placement

retention:
  # Done items older than this are deleted on startup and by "tower prune".
  done_after: 8760h                      # 1 year

prompts:
  # Optional override of the embedded prep prompt template (path).
  alert: ""
`

// ErrExists is returned by WriteExample when the target file already exists.
var ErrExists = errors.New("configuration file already exists")

// WriteExample writes Example to path, creating missing parent directories.
// It never overwrites an existing file.
func WriteExample(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create configuration directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the user selects the configuration file
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s (left unchanged)", ErrExists, path)
		}
		return fmt.Errorf("create configuration file: %w", err)
	}
	if _, err := f.WriteString(Example); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write configuration file: %w", err)
	}
	return f.Close()
}
